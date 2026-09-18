// Package server expone la UI estática, la API REST de sesiones y el endpoint
// WebSocket que attachea el browser a un pty del backend.
package server

import (
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/gorilla/websocket"

	"github.com/giuliano/webterm/internal/session"
)

// Config parametriza el servidor.
type Config struct {
	Addr      string // dirección de escucha, ej. "127.0.0.1:7788"
	StaticDir string // carpeta con el build del frontend (web/dist)
	Token     string // token requerido en cada request; vacío = sin auth
	// MCP es el handler del servidor MCP. Si es nil, /mcp no se monta.
	MCP http.Handler
}

// Server sirve la UI y las sesiones de terminal.
type Server struct {
	cfg      Config
	mgr      *session.Manager
	upgrader websocket.Upgrader
}

// New construye el servidor sobre un manager de sesiones ya arrancado.
func New(cfg Config, mgr *session.Manager) *Server {
	return &Server{
		cfg: cfg,
		mgr: mgr,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  4 * 1024,
			WriteBufferSize: 32 * 1024,
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

	mux.HandleFunc("GET /api/sessions", s.handleListSessions)
	mux.HandleFunc("POST /api/sessions", s.handleCreateSession)
	mux.HandleFunc("GET /api/sessions/{id}", s.handleGetSession)
	mux.HandleFunc("PATCH /api/sessions/{id}", s.handlePatchSession)
	mux.HandleFunc("DELETE /api/sessions/{id}", s.handleDeleteSession)
	mux.HandleFunc("POST /api/sessions/{id}/kill", s.handleKillSession)
	mux.HandleFunc("POST /api/sessions/{id}/restart", s.handleRestartSession)
	mux.HandleFunc("GET /api/sessions/{id}/kv", s.handleListKV)
	mux.HandleFunc("PUT /api/sessions/{id}/kv/{key}", s.handleSetKV)
	mux.HandleFunc("DELETE /api/sessions/{id}/kv/{key}", s.handleDeleteKV)

	mux.HandleFunc("GET /api/sessions/{id}/resources", s.handleListResources)
	mux.HandleFunc("POST /api/sessions/{id}/resources", s.handleLinkResource)
	mux.HandleFunc("DELETE /api/sessions/{id}/resources/{rid}", s.handleUnlinkResource)

	mux.HandleFunc("/ws/terminal", s.handleTerminal)

	// El servidor MCP se monta adentro de withAuth como todo lo demás: sin
	// eso, al levantar con run-lan cualquiera en la red podría leer y escribir
	// el contexto de las sesiones sin token.
	if s.cfg.MCP != nil {
		mux.Handle("/mcp", s.cfg.MCP)
		mux.Handle("/mcp/", s.cfg.MCP)
	}

	// Catch-all de /api/ y /ws/: sin esto, una ruta de API que no existe cae
	// en el fallback de la SPA y devuelve index.html con un 200. El cliente
	// falla entonces parseando HTML como JSON, y el error que se ve no tiene
	// nada que ver con la causa real. Los patrones más específicos de arriba
	// tienen precedencia, así que esto solo agarra lo que no matcheó nada.
	mux.HandleFunc("/api/", s.handleUnknownAPI)
	mux.HandleFunc("/ws/", s.handleUnknownAPI)

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
	live := 0
	if s.mgr != nil {
		live = s.mgr.LiveCount()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":   "ok",
		"auth":     s.cfg.Token != "",
		"sessions": live,
	})
}

// handleUnknownAPI responde 404 en JSON para rutas de API inexistentes.
func (s *Server) handleUnknownAPI(w http.ResponseWriter, r *http.Request) {
	writeErrorMsg(w, http.StatusNotFound, "ruta desconocida: "+r.URL.Path)
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
