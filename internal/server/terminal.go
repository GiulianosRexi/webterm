package server

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/giuliano/webterm/internal/control"
	"github.com/giuliano/webterm/internal/store"
)

const (
	// Cada cuánto mandamos ping para detectar clientes muertos.
	pingInterval = 30 * time.Second
	// Cuánto esperamos un pong antes de dar la conexión por perdida.
	pongTimeout = 60 * time.Second
	// Timeout de escritura sobre el socket.
	writeTimeout = 10 * time.Second
	// maxClientMessage acota lo que aceptamos leer del browser en un solo
	// frame. Mismo número y mismo motivo que internal/daemon/attach.go: sin
	// límite, gorilla bufferiza sin tope. Pero acá hay una razón extra para
	// que sea EXACTAMENTE el mismo número: att.Write reenvía el frame tal
	// cual al daemon por el socket interno, que tiene su propio límite de 1
	// MiB. Sin este límite acá, un paste grande del browser como un solo
	// frame binario pasaría el upgrade de este socket para después reventar
	// contra el límite del daemon, tumbando el attachment con un error
	// confuso del lado equivocado. Cortando acá con el mismo tope, el browser
	// ve un close inmediato y explicable en vez de una desconexión que
	// parece un bug del daemon.
	maxClientMessage = 1 << 20
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
	conn.SetReadLimit(maxClientMessage)

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
		// Sin proceso corriendo. Ojo: esto NO es sinónimo de "terminó". Una
		// fila en starting —el orquestador se cayó entre el insert y el
		// spawn— también llega acá con Live=false pero sin haber salido
		// nunca, así que ramificamos por att.End().Exited y no por !att.Live
		// para decidir si corresponde un exit. Mandar exit{code:0} sobre una
		// sesión que nunca corrió sería mentirle a la UI.
		if end := att.End(); end.Exited {
			_ = c.writeJSON(exitMsg{
				Type: "exit", Code: derefInt(end.Session.ExitCode), Reason: end.Session.ExitReason,
			})
		}
		// Se ve el historial y nada más. El input se descarta.
		drainUntilClose(conn)
		return
	}

	// pty -> browser
	go func() {
		for chunk := range att.Output() {
			if err := c.write(websocket.BinaryMessage, chunk); err != nil {
				break
			}
		}
		s.closeAfterStream(c, att)
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
				// Write y Resize pasan por el attachment: en el camino remoto
				// viajan por el mismo socket que el output, así que son parte
				// de la conexión y no operaciones sueltas del manager.
				if err := att.Resize(msg.Rows, msg.Cols); err != nil {
					log.Printf("[%s] resize falló: %v", id, err)
				}
				continue
			}
		}
		if err := att.Write(data); err != nil {
			log.Printf("[%s] write al pty falló: %v", id, err)
		}
	}
}

// closeAfterStream explica por qué se cortó el stream vivo.
//
// No puede confiar en Dropped(): el frame `dropped` que manda el daemon es
// best-effort, y si el cliente dejó de leer del todo la escritura del daemon
// se traba, gorilla envenena la conexión y ese frame nunca sale —justo en el
// caso que más nos importaría reportar—. Ver el comentario largo en
// internal/daemon/attach.go y en control.Attachment.End().
//
// La señal autoritativa es la fila, que End() ya resolvió en tres ramas
// excluyentes: no la reinterpretamos acá, solo elegimos qué decirle al
// browser en cada una. Exited tiene prioridad sobre Dropped a propósito: si
// la sesión terminó, eso es lo que le importa al usuario, haya habido o no
// expulsión de por medio.
func (s *Server) closeAfterStream(c *wsClient, att *control.Attachment) {
	end := att.End()
	switch {
	case end.Err != nil:
		// No se pudo leer la fila: no sabemos qué pasó, así que no inventamos
		// ni una expulsión ni una salida. El cliente ve nada más que el cierre
		// del socket.
	case end.Session == nil:
		// La fila se borró mientras el cliente miraba. No hay nada que
		// explicar contra un id que ya no existe.
	case end.Exited:
		_ = c.writeJSON(exitMsg{Type: "exit", Code: derefInt(end.Session.ExitCode), Reason: end.Session.ExitReason})
	case end.Dropped:
		_ = c.writeJSON(map[string]string{
			"type": "error", "error": "cliente demasiado lento; volvé a conectar",
		})
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
