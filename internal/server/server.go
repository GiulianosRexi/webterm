// Package server expone la UI estática y el endpoint WebSocket que conecta
// el browser con un pty del backend.
package server

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/giuliano/webterm/internal/terminal"
)

const (
	// Tamaño del chunk de lectura del pty.
	readBufSize = 32 * 1024
	// Cada cuánto mandamos ping para detectar clientes muertos.
	pingInterval = 30 * time.Second
	// Cuánto esperamos un pong antes de dar la conexión por perdida.
	pongTimeout = 60 * time.Second
	// Timeout de escritura sobre el socket.
	writeTimeout = 10 * time.Second
)

// Config parametriza el servidor.
type Config struct {
	Addr      string // dirección de escucha, ej. "127.0.0.1:7788"
	StaticDir string // carpeta con el build del frontend (web/dist)
	Shell     string // shell a spawnear; vacío = $SHELL
	Token     string // token requerido en cada request; vacío = sin auth
}

// Server sirve la UI y las sesiones de terminal.
type Server struct {
	cfg      Config
	upgrader websocket.Upgrader
	nextID   atomic.Uint64
}

// New construye el servidor.
func New(cfg Config) *Server {
	return &Server{
		cfg: cfg,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  4 * 1024,
			WriteBufferSize: readBufSize,
			CheckOrigin:     sameOrigin,
		},
	}
}

// Handler arma el router HTTP.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", s.handleHealth)
	mux.HandleFunc(loginPath, s.handleLogin)
	mux.HandleFunc("/api/logout", s.handleLogout)
	mux.HandleFunc("/ws/terminal", s.handleTerminal)
	mux.Handle("/", s.staticHandler())
	return s.withAuth(mux)
}

// ListenAndServe arranca el servidor HTTP.
func (s *Server) ListenAndServe() error {
	srv := &http.Server{
		Addr:              s.cfg.Addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	s.logURLs()
	return srv.ListenAndServe()
}

// logURLs imprime las direcciones por las que se llega al server, con el
// token incluido para poder copiar y pegar.
func (s *Server) logURLs() {
	suffix := ""
	if s.cfg.Token != "" {
		suffix = "/?token=" + s.cfg.Token
	}

	host, port, err := net.SplitHostPort(s.cfg.Addr)
	if err != nil {
		log.Printf("webterm escuchando en http://%s%s", s.cfg.Addr, suffix)
		return
	}

	log.Printf("webterm escuchando en %s", s.cfg.Addr)
	if host != "" && host != "0.0.0.0" && host != "::" {
		log.Printf("  → http://%s:%s%s", host, port, suffix)
		return
	}

	log.Printf("  → http://127.0.0.1:%s%s", port, suffix)
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() || ipnet.IP.To4() == nil {
			continue
		}
		log.Printf("  → http://%s:%s%s", ipnet.IP, port, suffix)
	}
	if s.cfg.Token == "" {
		log.Printf("  ⚠️  sin token: cualquiera en la red puede abrir una shell acá")
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	auth := "false"
	if s.cfg.Token != "" {
		auth = "true"
	}
	_, _ = io.WriteString(w, `{"status":"ok","auth":`+auth+`}`)
}

// staticHandler sirve el build de Vite, con fallback a index.html para que
// cualquier ruta del cliente resuelva a la SPA.
func (s *Server) staticHandler() http.Handler {
	dir := s.cfg.StaticDir
	files := http.FileServer(http.Dir(dir))
	index := filepath.Join(dir, "index.html")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := os.Stat(index); err != nil {
			http.Error(w, "frontend no buildeado: corré `npm --prefix web run build`", http.StatusServiceUnavailable)
			return
		}
		path := filepath.Join(dir, filepath.Clean("/"+r.URL.Path))
		if info, err := os.Stat(path); err != nil || info.IsDir() {
			http.ServeFile(w, r, index)
			return
		}
		files.ServeHTTP(w, r)
	})
}

// clientMsg es un mensaje de control del browser (los de texto; el input
// crudo del usuario viaja como mensaje binario).
type clientMsg struct {
	Type string `json:"type"`
	Rows uint16 `json:"rows"`
	Cols uint16 `json:"cols"`
}

// handleTerminal hace el upgrade a WebSocket y le conecta un pty nuevo.
func (s *Server) handleTerminal(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("upgrade falló: %v", err)
		return
	}

	rows := uint16(queryInt(r, "rows", 24))
	cols := uint16(queryInt(r, "cols", 80))

	id := "s" + strconv.FormatUint(s.nextID.Add(1), 10)
	sess, err := terminal.New(id, terminal.Config{
		Shell: s.cfg.Shell,
		Cwd:   r.URL.Query().Get("cwd"),
		Rows:  rows,
		Cols:  cols,
	})
	if err != nil {
		log.Printf("[%s] no se pudo spawnear el pty: %v", id, err)
		_ = conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseInternalServerErr, err.Error()),
			time.Now().Add(writeTimeout))
		_ = conn.Close()
		return
	}
	log.Printf("[%s] sesión abierta (%dx%d)", id, cols, rows)

	c := &wsClient{conn: conn}
	done := make(chan struct{})

	// pty -> browser
	go func() {
		defer close(done)
		buf := make([]byte, readBufSize)
		for {
			n, err := sess.Read(buf)
			if n > 0 {
				if werr := c.write(websocket.BinaryMessage, buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				// El shell terminó (EOF del pty) o se cerró la sesión.
				_ = c.writeJSON(map[string]string{"type": "exit"})
				return
			}
		}
	}()

	go c.keepalive(done)

	// browser -> pty
	conn.SetReadDeadline(time.Now().Add(pongTimeout))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(pongTimeout))
	})

	for {
		typ, data, err := conn.ReadMessage()
		if err != nil {
			break
		}
		switch typ {
		case websocket.BinaryMessage, websocket.TextMessage:
			if typ == websocket.TextMessage && len(data) > 0 && data[0] == '{' {
				var msg clientMsg
				if err := json.Unmarshal(data, &msg); err == nil && msg.Type == "resize" {
					if err := sess.Resize(msg.Rows, msg.Cols); err != nil && !errors.Is(err, io.ErrClosedPipe) {
						log.Printf("[%s] resize falló: %v", id, err)
					}
					continue
				}
			}
			if _, err := sess.Write(data); err != nil {
				log.Printf("[%s] write al pty falló: %v", id, err)
			}
		}
	}

	_ = sess.Close()
	<-done
	_ = conn.Close()
	log.Printf("[%s] sesión cerrada", id)
}

// wsClient serializa las escrituras al socket: gorilla no admite writers
// concurrentes y acá escriben el lector del pty y el keepalive.
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
			c.mu.Lock()
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
			err := c.conn.WriteMessage(websocket.PingMessage, nil)
			c.mu.Unlock()
			if err != nil {
				return
			}
		}
	}
}

func queryInt(r *http.Request, key string, def int) int {
	v, err := strconv.Atoi(r.URL.Query().Get(key))
	if err != nil || v <= 0 {
		return def
	}
	return v
}
