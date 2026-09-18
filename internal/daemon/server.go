package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"github.com/giuliano/webterm/internal/ptyapi"
	"github.com/giuliano/webterm/internal/store"
)

// ProtocolVersion se bumpea SOLO cuando cambia el protocolo del socket, nunca
// por un build. Existe porque recompilar el binario no reemplaza al daemon que
// ya está corriendo: sin este número, un daemon viejo falla con un error raro
// a mitad de un attach en vez de decir que hay que reiniciarlo.
const ProtocolVersion = 1

// maxSpawnBody acota el body de un spawn. Nadie manda un cwd de más de unos KB.
const maxSpawnBody = 64 << 10

// Info es lo que contesta GET /info.
type Info struct {
	ProtocolVersion int   `json:"protocol_version"`
	PID             int   `json:"pid"`
	StartedAt       int64 `json:"started_at"`
}

// Server expone el manager de ptys sobre un socket Unix.
//
// No tiene auth: escucha en un socket con permisos 0600, así que quien puede
// abrirlo ya es el dueño de la máquina y ya tiene shell. Sumar un token acá
// sería ceremonia sin propiedad nueva.
type Server struct {
	pty       ptyapi.Client
	startedAt int64

	// http es un atomic.Pointer y no un campo simple porque Serve lo escribe
	// desde la goroutine que sirve mientras Shutdown lo lee desde quien apaga:
	// sin esto, los tests que llaman Shutdown apenas el socket existe (antes de
	// que Serve termine de armar el *http.Server) disparan una data race.
	http atomic.Pointer[http.Server]
}

func NewServer(pty ptyapi.Client) *Server {
	return &Server{pty: pty, startedAt: time.Now().UnixMilli()}
}

// Handler arma el router. Son cinco rutas y ese número no debería crecer: si
// hace falta un verbo nuevo para algo que no es un pty, el diseño está mal.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /info", s.handleInfo)
	mux.HandleFunc("GET /sessions", s.handleList)
	mux.HandleFunc("POST /sessions", s.handleSpawn)
	mux.HandleFunc("POST /sessions/{id}/kill", s.handleKill)
	mux.HandleFunc("GET /sessions/{id}/attach", s.handleAttach)
	return mux
}

// Serve escucha en el socket y bloquea.
func (s *Server) Serve(socketPath string) error {
	// Un .sock que quedó de un crash anterior haría fallar el bind. Borrarlo es
	// seguro porque el flock del arranque ya garantizó que no hay otro daemon
	// para esta base.
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("limpiando %s: %w", socketPath, err)
	}
	// El *http.Server se arma y se guarda ANTES de escuchar: así, para cuando
	// alguien logra dialear el socket (la señal que usan los tests para saber
	// que ya pueden actuar), Shutdown ya tiene de dónde leer y nunca se
	// encuentra con un puntero todavía en nil.
	httpSrv := &http.Server{Handler: s.Handler()}
	s.http.Store(httpSrv)

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("escuchando en %s: %w", socketPath, err)
	}
	// El bind respeta el umask, así que los permisos se fijan después.
	if err := os.Chmod(socketPath, 0o600); err != nil {
		_ = ln.Close()
		return fmt.Errorf("ajustando permisos de %s: %w", socketPath, err)
	}

	log.Printf("daemon escuchando en %s (protocolo %d, pid %d)", socketPath, ProtocolVersion, os.Getpid())
	if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Shutdown corta el servidor HTTP. No mata las sesiones: eso lo decide quien
// apaga el proceso.
func (s *Server) Shutdown(ctx context.Context) error {
	httpSrv := s.http.Load()
	if httpSrv == nil {
		return nil
	}
	return httpSrv.Shutdown(ctx)
}

func (s *Server) handleInfo(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, Info{
		ProtocolVersion: ProtocolVersion, PID: os.Getpid(), StartedAt: s.startedAt,
	})
}

func (s *Server) handleList(w http.ResponseWriter, _ *http.Request) {
	ids, err := s.pty.LiveIDs()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string][]string{"ids": ids})
}

func (s *Server) handleSpawn(w http.ResponseWriter, r *http.Request) {
	var o ptyapi.SpawnOpts
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSpawnBody)).Decode(&o); err != nil {
		http.Error(w, "body inválido", http.StatusBadRequest)
		return
	}
	if o.ID == "" {
		http.Error(w, "falta id", http.StatusBadRequest)
		return
	}
	if err := s.pty.Spawn(o); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleKill(w http.ResponseWriter, r *http.Request) {
	if err := s.pty.Kill(r.PathValue("id")); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleAttach todavía no existe: la tarea 7 lo reemplaza por el upgrade al
// streaming de verdad. El stub devuelve 501 para que el router compile y las
// otras cuatro rutas queden probadas ya.
func (s *Server) handleAttach(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, "no implementado", http.StatusNotImplemented)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("daemon: escribiendo respuesta: %v", err)
	}
}

// writeError traduce los errores del manager a un status que el cliente sabe
// volver a convertir en el error original.
//
// Los cuatro estados tienen status distintos a propósito. "No existe la fila",
// "la fila existe pero no hay proceso", "ya hay un proceso" y "el dueño de los
// ptys está apagando" son situaciones distintas que ameritan reacciones
// distintas —404 es un error de verdad, 410 es el camino de solo lectura, 409
// es un conflicto del propio pedido, 503 invita a reintentar—, así que si
// compartieran status el cliente remoto tendría que adivinar parseando el
// texto en vez de reconstruir el error original desde el status. El mapeo
// completo vive documentado en ptyapi.go, que es la fuente: acá solo se aplica.
func writeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		http.Error(w, err.Error(), http.StatusNotFound) // no existe la fila
	case errors.Is(err, ptyapi.ErrNotLive):
		http.Error(w, err.Error(), http.StatusGone) // existe, pero sin proceso
	case errors.Is(err, ptyapi.ErrAlreadyLive):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, ptyapi.ErrClosed):
		http.Error(w, err.Error(), http.StatusServiceUnavailable) // el dueño de los ptys se apaga
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
