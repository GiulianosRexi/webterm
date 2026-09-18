package daemon

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// writeTimeout acota cuánto esperamos por una escritura al socket interno.
//
// Corto a propósito: el peer de este socket es el orquestador, en la misma
// máquina. Una escritura que tarda segundos ahí no es una red lenta —es que
// el orquestador dejó de leer (pestaña colgada del otro lado, proxy
// trabado)—, así que no hay nada que ganar esperando de más. Cuanto antes
// vence el deadline, antes se libera el subscriber del hub y antes se nota
// el atasco en vez de acumular memoria en su buffer.
const writeTimeout = 2 * time.Second

// maxClientMessage acota lo que aceptamos leer del cliente en un solo frame.
// Sin esto, gorilla bufferiza sin límite: un frame gigante (malicioso o por
// un bug del otro lado) se comería RAM antes de llegar a att.Write. El peer
// es confiable —socket 0600, sin red de por medio— pero acotar no cuesta
// nada.
const maxClientMessage = 1 << 20 // 1 MiB

// upgrader del socket interno.
//
// CheckOrigin siempre true porque del otro lado no hay un browser: es un
// socket Unix con permisos 0600, así que no existe el ataque que el chequeo de
// origin previene (una página cualquiera usando la cookie de la víctima).
var upgrader = websocket.Upgrader{
	ReadBufferSize:  4 * 1024,
	WriteBufferSize: 32 * 1024,
	CheckOrigin:     func(*http.Request) bool { return true },
}

// resizeMsg es el único mensaje de control que manda el cliente.
type resizeMsg struct {
	Type string `json:"type"`
	Rows uint16 `json:"rows"`
	Cols uint16 `json:"cols"`
}

// handleAttach conecta un cliente al pty.
//
// El framing es el mismo que el orquestador le habla al browser, a propósito:
// así reenviar es copiar frames y no traducir.
func (s *Server) handleAttach(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	// Attacheamos ANTES del upgrade para poder contestar un status HTTP claro
	// (410 si no está viva) en vez de abrir el socket y cerrarlo enseguida.
	att, err := s.pty.Attach(id)
	if err != nil {
		writeError(w, err)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		att.Detach()
		log.Printf("daemon [%s]: upgrade falló: %v", id, err)
		return
	}
	defer func() {
		att.Detach()
		_ = conn.Close()
	}()
	conn.SetReadLimit(maxClientMessage)

	c := &wsWriter{conn: conn}

	// Handshake: primero el replay, después ready, y recién ahí el stream vivo.
	if hist := att.History(); len(hist) > 0 {
		if err := c.write(websocket.BinaryMessage, hist); err != nil {
			return
		}
	}
	if err := c.writeJSON(map[string]string{"type": "ready"}); err != nil {
		return
	}

	// pty -> cliente
	go func() {
		for chunk := range att.Output() {
			if err := c.write(websocket.BinaryMessage, chunk); err != nil {
				break
			}
		}
		// dropped es best-effort, no una garantía. Una vez que una escritura
		// al socket falla, gorilla deja ese error pegajoso en la conexión
		// (prepWrite lo repite en toda escritura posterior sin tocar el
		// cable), así que este writeJSON también va a fallar, en silencio.
		//
		// Eso pasa justo en el caso más probable en producción: un cliente
		// que dejó de leer del todo (pestaña congelada, proxy trabado). Ahí
		// la propia escritura del chunk que disparó el drop es la que ya
		// envenenó la conexión, y el cliente termina viendo un close 1006
		// indistinguible del fin de la sesión —la ambigüedad que este frame
		// existe para resolver, sin resolverla en este caso.
		//
		// Sí llega cuando el cliente es lento pero sigue leyendo: ahí el
		// drop lo dispara el buffer del hub (se le acumularon 256 chunks sin
		// consumir) y no una escritura trabada, así que la conexión sigue
		// sana y el frame sale.
		//
		// Por eso la señal autoritativa no puede vivir en este socket: es la
		// fila del orquestador. Si el stream se corta y la fila dice
		// exited, la sesión terminó; si dice running, al cliente lo
		// expulsaron o se cayó el transporte.
		if att.Dropped() {
			_ = c.writeJSON(map[string]string{"type": "dropped"})
		}
		// Cerrar el socket destraba el ReadMessage del loop de abajo.
		_ = conn.Close()
	}()

	// cliente -> pty. Sin deadline de lectura: no hay keepalive en el socket
	// interno porque si el proceso del otro lado muere, el socket se cierra y
	// esto devuelve error enseguida.
	for {
		typ, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if typ == websocket.TextMessage && len(data) > 0 && data[0] == '{' {
			var msg resizeMsg
			if err := json.Unmarshal(data, &msg); err == nil && msg.Type == "resize" {
				if err := att.Resize(msg.Rows, msg.Cols); err != nil {
					log.Printf("daemon [%s]: resize falló: %v", id, err)
				}
				continue
			}
		}
		if err := att.Write(data); err != nil {
			log.Printf("daemon [%s]: write al pty falló: %v", id, err)
		}
	}
}

// wsWriter serializa las escrituras: gorilla no admite writers concurrentes y
// acá escriben el bombeo del pty y el handshake.
type wsWriter struct {
	mu   sync.Mutex
	conn *websocket.Conn
}

func (c *wsWriter) write(msgType int, data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	return c.conn.WriteMessage(msgType, data)
}

func (c *wsWriter) writeJSON(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.write(websocket.TextMessage, data)
}
