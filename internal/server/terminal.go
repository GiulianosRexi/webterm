package server

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/giuliano/webterm/internal/session"
	"github.com/giuliano/webterm/internal/store"
)

const (
	// Cada cuánto mandamos ping para detectar clientes muertos.
	pingInterval = 30 * time.Second
	// Cuánto esperamos un pong antes de dar la conexión por perdida.
	pongTimeout = 60 * time.Second
	// Timeout de escritura sobre el socket.
	writeTimeout = 10 * time.Second
)

// clientMsg es un mensaje de control del browser (los de texto; el input
// crudo del usuario viaja como mensaje binario).
type clientMsg struct {
	Type string `json:"type"`
	Rows uint16 `json:"rows"`
	Cols uint16 `json:"cols"`
}

type attachedMsg struct {
	Type    string         `json:"type"`
	Session *store.Session `json:"session"`
}

type exitMsg struct {
	Type   string `json:"type"`
	Code   int    `json:"code"`
	Reason string `json:"reason"`
}

// handleTerminal attachea el WebSocket a una sesión ya existente.
//
// Crear sesiones es tarea del endpoint REST: si el upgrade pudiera spawnear
// una, el socket sería una puerta trasera para abrir shells sin pasar por el
// ABM.
func (s *Server) handleTerminal(w http.ResponseWriter, r *http.Request) {
	// El chequeo de origin va antes que todo lo demás: un handshake originado
	// en otra página no tiene por qué llegar a mirar una sesión. El upgrader
	// lo repite, pero ahí ya sería tarde para responder un 403 claro.
	if !sameOrigin(r) {
		http.Error(w, "origen no permitido", http.StatusForbidden)
		return
	}

	id := r.URL.Query().Get("session_id")
	if id == "" {
		http.Error(w, "falta session_id", http.StatusBadRequest)
		return
	}

	// Attacheamos antes del upgrade para poder responder con un status HTTP
	// claro en vez de abrir el socket y cerrarlo enseguida.
	att, err := s.mgr.Attach(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "la sesión no existe", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		att.Detach()
		log.Printf("[%s] upgrade falló: %v", id, err)
		return
	}
	defer func() {
		att.Detach()
		_ = conn.Close()
	}()

	c := &wsClient{conn: conn}
	done := make(chan struct{})
	defer close(done)
	go c.keepalive(done)

	// Handshake: metadata, replay, y recién ahí el stream vivo.
	if err := c.writeJSON(attachedMsg{Type: "attached", Session: att.Session}); err != nil {
		return
	}
	if len(att.History) > 0 {
		if err := c.write(websocket.BinaryMessage, att.History); err != nil {
			return
		}
	}
	if err := c.writeJSON(map[string]string{"type": "ready"}); err != nil {
		return
	}

	if !att.Live {
		// Sesión muerta: se ve el historial y nada más. El input se descarta.
		_ = c.writeJSON(exitMsg{
			Type: "exit", Code: derefInt(att.Session.ExitCode), Reason: att.Session.ExitReason,
		})
		drainUntilClose(conn)
		return
	}

	// pty -> browser
	go func() {
		for chunk := range att.Output {
			if err := c.write(websocket.BinaryMessage, chunk); err != nil {
				break
			}
		}
		s.closeAfterStream(c, att, id)
		// Cerrar el socket destraba el ReadMessage del loop de abajo.
		_ = conn.Close()
	}()

	// browser -> pty
	_ = conn.SetReadDeadline(time.Now().Add(pongTimeout))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(pongTimeout))
	})

	for {
		typ, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if typ == websocket.TextMessage && len(data) > 0 && data[0] == '{' {
			var msg clientMsg
			if err := json.Unmarshal(data, &msg); err == nil && msg.Type == "resize" {
				if err := s.mgr.Resize(id, msg.Rows, msg.Cols); err != nil {
					log.Printf("[%s] resize falló: %v", id, err)
				}
				continue
			}
		}
		if err := s.mgr.Write(id, data); err != nil {
			log.Printf("[%s] write al pty falló: %v", id, err)
		}
	}
}

// closeAfterStream explica por qué se cortó el stream: o al cliente lo
// expulsamos por lento, o la sesión terminó.
func (s *Server) closeAfterStream(c *wsClient, att *session.Attachment, id string) {
	if att.Dropped() {
		_ = c.writeJSON(map[string]string{
			"type": "error", "error": "cliente demasiado lento; volvé a conectar",
		})
		return
	}
	rec, err := s.mgr.Get(id)
	if err != nil {
		return
	}
	if rec.PtyStatus == store.StatusExited {
		_ = c.writeJSON(exitMsg{Type: "exit", Code: derefInt(rec.ExitCode), Reason: rec.ExitReason})
	}
}

// drainUntilClose descarta lo que mande el cliente hasta que corte. Lo usamos
// en el modo de solo lectura, donde igual hay que leer del socket para que
// lleguen los pongs y se detecte el cierre.
func drainUntilClose(conn *websocket.Conn) {
	_ = conn.SetReadDeadline(time.Now().Add(pongTimeout))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(pongTimeout))
	})
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
	}
}

func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// wsClient serializa las escrituras al socket: gorilla no admite writers
// concurrentes y acá escriben el lector de la sesión y el keepalive.
type wsClient struct {
	mu   sync.Mutex
	conn *websocket.Conn
}

func (c *wsClient) write(msgType int, data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	return c.conn.WriteMessage(msgType, data)
}

func (c *wsClient) writeJSON(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.write(websocket.TextMessage, data)
}

func (c *wsClient) keepalive(done <-chan struct{}) {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			if err := c.write(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}
