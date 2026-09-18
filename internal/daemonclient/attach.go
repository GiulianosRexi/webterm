package daemonclient

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/giuliano/webterm/internal/ptyapi"
)

// outBuffer es cuántos chunks se bufferean entre el socket del daemon y quien
// lee acá. El daemon ya expulsa a los clientes lentos por su cuenta; esto es
// el segundo nivel de backpressure, el que protege este proceso.
const outBuffer = 256

const writeTimeout = 10 * time.Second

// Attach abre el WebSocket y consume el handshake antes de devolver: al volver,
// History() ya está completo y Output() es solo stream vivo.
func (c *Client) Attach(id string) (ptyapi.Attachment, error) {
	conn, res, err := c.dialer.Dial("ws://daemon/sessions/"+id+"/attach", nil)
	if err != nil {
		if res != nil {
			defer res.Body.Close()
			if serr := statusError(res); serr != nil {
				return nil, serr
			}
		}
		return nil, fmt.Errorf("attacheando a %s: %w", id, err)
	}

	a := &attachment{conn: conn, out: make(chan []byte, outBuffer), done: make(chan struct{})}
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
func (a *attachment) readHandshake() error {
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
			}
		case websocket.TextMessage:
			var msg struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(data, &msg); err == nil && msg.Type == "dropped" {
				a.mu.Lock()
				a.dropped = true
				a.mu.Unlock()
			}
		}
	}
}

func (a *attachment) History() []byte       { return a.history }
func (a *attachment) Output() <-chan []byte { return a.out }

// Dropped dice si el daemon avisó que nos expulsó por lentos. Es best-effort,
// no una garantía: el frame "dropped" que lo dispara puede no llegar nunca.
//
// El daemon (ver internal/daemon/attach.go) manda ese frame después de que el
// hub del pty ya decidió expulsarnos, pero si lo que provocó la expulsión fue
// que dejamos de leer del todo (pestaña colgada, proceso trabado), la propia
// escritura del chunk que la disparó ya rompió la conexión: gorilla deja ese
// error pegajoso (prepWrite lo repite en toda escritura posterior), así que el
// writeJSON del frame "dropped" también falla, en silencio. En ese caso vemos
// un close sin más, indistinguible de la sesión terminando o del transporte
// cayéndose.
//
// Por eso esto solo sirve como señal positiva ("sí, nos expulsaron") y nunca
// como negativa: false después de que Output() se cerró no dice "no nos
// expulsaron", dice "no lo sabemos por acá". Quien necesite certeza tiene que
// mirar la fila de la sesión: PtyStatus == exited es que terminó; running con
// el stream cortado es que nos expulsaron o se cayó el transporte.
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
