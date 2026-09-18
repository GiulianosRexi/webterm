package daemonclient

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/giuliano/webterm/internal/ptyapi"
)

// outBuffer es cuántos chunks se bufferean entre el socket del daemon y quien
// lee acá. El daemon ya expulsa a los clientes lentos por su cuenta; esto es
// el segundo nivel de backpressure, el que protege este proceso.
const outBuffer = 256

// writeTimeout acota una escritura al socket interno.
//
// Es 5x el del daemon (2s, ver internal/daemon/attach.go) y no tiene el mismo
// argumento de "cortar rápido para no acumular memoria en un hub": acá no hay
// hub ni fan-out, la escritura es de este proceso directo al daemon en la
// misma máquina, así que un poco más de margen no cuesta nada. Sigue siendo
// corto a propósito: el peer es local, y una escritura que tarda segundos ahí
// es el daemon trabado, no una red lenta.
const writeTimeout = 10 * time.Second

// handshakeTimeout acota cuánto se espera el "ready" antes de rendirse.
//
// Check() valida la versión del protocolo una vez, al arrancar, pero no es un
// invariante por conexión: un daemon que quedó mudo a mitad de un upgrade (de
// otra versión, o simplemente trabado) dejaría este ReadMessage esperando para
// siempre, y como ptyapi.Client.Attach no toma contexto, no hay forma de
// cancelarlo desde quien nos llama. Sin este deadline, un Attach colgado
// cuelga para siempre el handler HTTP del orquestador que lo invocó.
//
// Es var y no const solo para que el test del peer mudo lo pueda bajar: ese
// test espera el timeout entero y a 10 s era él solo el que hacía de
// daemonclient el paquete más lento de la suite. Nadie lo escribe en
// producción.
var handshakeTimeout = 10 * time.Second

// Attach abre el WebSocket y consume el handshake antes de devolver: al volver,
// History() ya está completo y Output() es solo stream vivo.
func (c *Client) Attach(id string) (ptyapi.Attachment, error) {
	return c.attach(id, outBuffer)
}

// attach es Attach con el tamaño del buffer de salida parametrizado.
//
// Existe para que los tests puedan forzar el buffer lleno con dos chunks en
// vez de tener que generar 256 frames reales para ejercer la rama de
// expulsión local de pump.
func (c *Client) attach(id string, bufSize int) (ptyapi.Attachment, error) {
	conn, res, err := c.dialer.Dial("ws://daemon/sessions/"+url.PathEscape(id)+"/attach", nil)
	if err != nil {
		if res != nil {
			defer res.Body.Close()
			if serr := statusError(res); serr != nil {
				return nil, serr
			}
		}
		return nil, fmt.Errorf("attacheando a %s: %w", id, err)
	}

	a := &attachment{conn: conn, out: make(chan []byte, bufSize), done: make(chan struct{})}
	if err := a.readHandshake(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	go a.pump()
	return a, nil
}

// attachment es una ptyapi.Attachment del otro lado del socket.
type attachment struct {
	conn    *websocket.Conn
	history []byte
	out     chan []byte

	wmu sync.Mutex // gorilla no admite writers concurrentes

	mu      sync.Mutex
	dropped bool

	done     chan struct{}
	doneOnce sync.Once
}

// readHandshake junta los frames binarios hasta el "ready".
//
// Consumirlo acá y no dejárselo al que lee es lo que hace que History() y
// Output() sean cosas distintas: si no, quien consume tendría que saber dónde
// termina el replay, que es exactamente lo que el protocolo ya le dice.
//
// Corre bajo handshakeTimeout: un peer que sube el WebSocket y no manda
// "ready" (daemon de otra versión, o simplemente trabado) no puede colgar
// esto para siempre. El deadline se limpia antes de volver porque el bombeo
// de acá en más sí puede quedarse sin datos por rato largo (una sesión
// inactiva) y no queremos que eso se confunda con un peer mudo.
func (a *attachment) readHandshake() error {
	if err := a.conn.SetReadDeadline(time.Now().Add(handshakeTimeout)); err != nil {
		return fmt.Errorf("fijando el deadline del handshake: %w", err)
	}
	defer func() { _ = a.conn.SetReadDeadline(time.Time{}) }()

	for {
		typ, data, err := a.conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("leyendo el handshake: %w", err)
		}
		switch typ {
		case websocket.BinaryMessage:
			a.history = append(a.history, data...)
		case websocket.TextMessage:
			var msg struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(data, &msg); err == nil && msg.Type == "ready" {
				return nil
			}
		}
	}
}

// pump traduce los frames del socket al canal de output.
//
// Si quien consume Output() abandona sin llamar Detach() —no lee más, no
// cierra nada—, el buffer se llena y el select de abajo tiene que decidir qué
// hacer. La respuesta tiene que ser la misma que la del manager en proceso
// ante un suscriptor lento (ver hub.broadcast en internal/session/hub.go): no
// bloquearse esperando a que vuelva a leer. Bloquearse acá frenaría el
// bombeo para siempre —nadie más va a leer del socket del daemon— y dejaría
// esta goroutine viva mientras nadie llame Detach(), que nunca va a pasar si
// quien la tendría que llamar ya abandonó. El manager expulsa a su lento; este
// cliente tiene que expulsarse a sí mismo del mismo modo, o el contrato deja
// de ser simétrico justo donde más importa.
func (a *attachment) pump() {
	defer close(a.out)
	for {
		typ, data, err := a.conn.ReadMessage()
		if err != nil {
			return
		}
		switch typ {
		case websocket.BinaryMessage:
			select {
			case a.out <- data:
			case <-a.done:
				return
			default:
				// El consumidor local no está leyendo: nos expulsamos nosotros
				// mismos, igual que el hub expulsa a un suscriptor lento.
				// Cerrar la conexión destraba cualquier lectura pendiente del
				// otro lado y hace que el próximo ReadMessage de este mismo
				// loop (si llegara a haber uno) vuelva con error; wsWriter no
				// entra en juego porque esta es la única goroutina que escribe
				// leyendo de acá.
				a.markDropped()
				_ = a.conn.Close()
				return
			}
		case websocket.TextMessage:
			var msg struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(data, &msg); err == nil && msg.Type == "dropped" {
				a.markDropped()
			}
		}
	}
}

func (a *attachment) markDropped() {
	a.mu.Lock()
	a.dropped = true
	a.mu.Unlock()
}

func (a *attachment) History() []byte       { return a.history }
func (a *attachment) Output() <-chan []byte { return a.out }

// Dropped dice si nos expulsaron por lentos, ya sea el daemon (frame
// "dropped") o nosotros mismos al ver el buffer local lleno (ver pump). Solo
// es significativo después de que Output() se haya cerrado.
//
// El caso remoto es best-effort, no una garantía: el frame "dropped" que lo
// dispara puede no llegar nunca. El daemon (ver internal/daemon/attach.go)
// lo manda después de que su hub ya decidió expulsarnos, pero si lo que
// provocó la expulsión fue que dejamos de leer del todo del lado del
// transporte (pestaña colgada, proceso trabado), la propia escritura del
// chunk que la disparó ya rompió la conexión: gorilla deja ese error pegajoso
// (prepWrite lo repite en toda escritura posterior), así que el writeJSON del
// frame "dropped" también falla, en silencio. En ese caso vemos un close sin
// más, indistinguible de la sesión terminando o del transporte cayéndose.
//
// El caso local (buffer de este proceso lleno) no tiene esa ambigüedad: lo
// marcamos nosotros mismos de forma sincrónica antes de cerrar, así que
// siempre se ve.
//
// Aun así, esto solo sirve como señal positiva ("sí, nos expulsaron") y nunca
// como negativa: false después de que Output() se cerró no dice "no nos
// expulsaron", dice "no lo sabemos por acá" (puede ser el caso remoto
// silencioso de arriba). Quien necesite certeza tiene que mirar la fila de la
// sesión: PtyStatus == exited es que terminó; running con el stream cortado es
// que nos expulsaron (local o remoto) o se cayó el transporte.
func (a *attachment) Dropped() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.dropped
}

func (a *attachment) Write(p []byte) error {
	return a.send(websocket.BinaryMessage, p)
}

func (a *attachment) Resize(rows, cols uint16) error {
	data, err := json.Marshal(map[string]any{"type": "resize", "rows": rows, "cols": cols})
	if err != nil {
		return err
	}
	return a.send(websocket.TextMessage, data)
}

// send es fire-and-forget del lado del pty: el daemon loguea un error de
// escritura al pty (ver el "write al pty falló" en internal/daemon/attach.go)
// pero no lo devuelve por el socket, así que un error de aplicación ahí es
// invisible para nosotros. El error que sí propagamos acá es el de la
// escritura al WebSocket en sí (conexión caída, deadline vencido): eso es
// nuestro, no del pty.
func (a *attachment) send(msgType int, data []byte) error {
	a.wmu.Lock()
	defer a.wmu.Unlock()
	_ = a.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	return a.conn.WriteMessage(msgType, data)
}

// Detach cierra el socket. El daemon ve el cierre y desuscribe al cliente; la
// sesión no se entera.
func (a *attachment) Detach() {
	a.doneOnce.Do(func() { close(a.done) })
	_ = a.conn.Close()
}

var _ ptyapi.Attachment = (*attachment)(nil)
