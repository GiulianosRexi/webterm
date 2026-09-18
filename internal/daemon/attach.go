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
const writeTimeout = 10 * time.Second

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
		// Si al cliente lo expulsamos por lento hay que decírselo: el cierre
		// del stream solo, sin esto, se confunde con el fin de la sesión.
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
