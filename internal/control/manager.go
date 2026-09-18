// Package control es el manager del orquestador.
//
// Resuelve contra la base todo lo que no es un pty —metadata, KV, recursos
// externos— y le delega a un ptyapi.Client lo que sí. Esa división es el
// invariante de M10: una feature de contexto nueva se agrega acá y en store, y
// el daemon no se entera.
package control

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/giuliano/webterm/internal/ptyapi"
	"github.com/giuliano/webterm/internal/resources"
	"github.com/giuliano/webterm/internal/store"
)

// defaultSweepInterval es cada cuánto se verifica que la base y lo que el
// daemon reporta vivo coincidan.
const defaultSweepInterval = 30 * time.Second

// ErrAlreadyRunning lo devuelve Restart sobre una sesión que no murió.
var ErrAlreadyRunning = errors.New("la sesión ya está corriendo")

// ErrUnknownResource se re-exporta para que el servidor traduzca el error a un
// status sin importar el paquete resources.
var ErrUnknownResource = resources.ErrUnknownResource

// resumeBanner queda en el historial para que el replay muestre dónde se cortó
// la sesión anterior. Lo escribe el daemon —llega por SpawnOpts.Banner— porque
// es el único que puede ordenarlo contra el writer del historial.
const resumeBanner = "\r\n\x1b[90m— sesión reanudada —\x1b[0m\r\n"

// Config parametriza el orquestador.
type Config struct {
	Shell string // shell a spawnear; vacío = $SHELL
	// Resources resuelve el estado de los recursos externos linkeados. Si es
	// nil, linkear por URL deja de funcionar pero el resto anda igual.
	Resources *resources.Cache
	// ExtraEnv son variables que se suman al entorno de cada pty. El
	// entrypoint las arma; así el daemon no necesita saber que existe un token.
	ExtraEnv []string
	// SweepEvery es cada cuánto corre la verificación de invariante.
	SweepEvery time.Duration
}

// Manager es el dueño del estado de las sesiones. Los ptys son de otro.
type Manager struct {
	st  *store.Store
	pty ptyapi.Client
	cfg Config

	// daemonStartedAt sirve para distinguir por qué murió una sesión huérfana:
	// si la fila es anterior al arranque del daemon, se la llevó el reinicio;
	// si es posterior, es deriva y el motivo honesto es "huérfana".
	daemonStartedAt int64

	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

func NewManager(st *store.Store, pty ptyapi.Client, cfg Config) *Manager {
	if cfg.SweepEvery <= 0 {
		cfg.SweepEvery = defaultSweepInterval
	}
	return &Manager{st: st, pty: pty, cfg: cfg, stop: make(chan struct{})}
}

// SetDaemonStartedAt lo llama el entrypoint con lo que contestó /info.
func (m *Manager) SetDaemonStartedAt(ms int64) { m.daemonStartedAt = ms }

// Start hace el primer sweep y arranca la verificación periódica.
//
// Tiene que correr ANTES de escuchar: si no, hay una ventana en la que la API
// reporta vivas sesiones que el daemon no tiene.
func (m *Manager) Start() error {
	if n := m.Sweep(); n > 0 {
		log.Printf("reconciliadas %d sesiones que el daemon ya no tiene", n)
	}
	m.wg.Add(1)
	go m.sweepLoop()
	return nil
}

// Close para la verificación periódica y suelta el cliente de ptys.
//
// NO mata las sesiones: que sobrevivan al apagado del orquestador es
// literalmente el objetivo de M10. Con el cliente remoto, pty.Close() solo
// suelta conexiones ociosas y los ptys siguen corriendo del otro lado del
// socket. Con el manager en proceso —el de los tests— sí se las lleva, porque
// ahí el dueño de los ptys se apaga junto con esto. La asimetría es del
// contrato, está documentada en ptyapi, y es justamente lo que el milestone
// vino a comprar.
func (m *Manager) Close() error {
	m.stopOnce.Do(func() { close(m.stop) })
	m.wg.Wait()
	return m.pty.Close()
}

// CreateOpts describe la sesión a crear.
type CreateOpts struct {
	Title       string
	Description string
	Cwd         string
	Cols, Rows  int
}

// Create inserta la fila y le pide al daemon que la spawnee.
//
// El orden importa: session_output tiene FK contra sessions, así que la fila
// tiene que existir antes de que el historial empiece a escribirse. La ventana
// entre las dos cosas es el estado starting.
func (m *Manager) Create(o CreateOpts) (*store.Session, error) {
	if o.Cols <= 0 {
		o.Cols = 80
	}
	if o.Rows <= 0 {
		o.Rows = 24
	}
	shell := m.cfg.Shell
	if shell == "" {
		shell = os.Getenv("SHELL")
	}
	if shell == "" {
		shell = "/bin/zsh"
	}
	cwd := o.Cwd
	if cwd == "" {
		cwd, _ = os.UserHomeDir()
	}

	rec := &store.Session{
		ID: store.NewID(), Title: o.Title, Description: o.Description,
		Cwd: cwd, Shell: shell, Cols: o.Cols, Rows: o.Rows,
		PtyStatus: store.StatusStarting,
	}
	if err := m.st.CreateSession(rec); err != nil {
		return nil, err
	}

	if err := m.spawn(rec, ""); err != nil {
		return rec, err
	}
	return m.st.GetSession(rec.ID)
}

// spawn le pide el pty al daemon y se asegura de que la fila no quede trabada
// en starting si algo sale mal.
//
// El daemon marca spawn_failed cuando el que falla es el pty, pero si el que
// falla es el transporte —el daemon se cayó entre medio— nadie más lo haría, y
// la UI mostraría una sesión arrancando para siempre.
//
// Shell y Cwd salen de la fila y nunca van vacíos: terminal.New cae al $SHELL
// y al $HOME del proceso que tiene los ptys, que con el daemon aparte es el
// entorno equivocado, y además dejaría la columna shell de la fila mintiendo
// sobre qué proceso corre realmente.
func (m *Manager) spawn(rec *store.Session, banner string) error {
	err := m.pty.Spawn(ptyapi.SpawnOpts{
		ID: rec.ID, Shell: rec.Shell, Cwd: rec.Cwd,
		Cols: rec.Cols, Rows: rec.Rows, Env: m.cfg.ExtraEnv, Banner: banner,
	})
	if err == nil {
		return nil
	}
	if got, gerr := m.st.GetSession(rec.ID); gerr == nil && got.PtyStatus == store.StatusStarting {
		code := -1
		if merr := m.st.MarkExited(rec.ID, store.ReasonSpawnFailed, &code); merr != nil {
			log.Printf("[%s] no se pudo registrar el spawn fallido: %v", rec.ID, merr)
		}
	}
	return err
}

// Restart spawnea un pty nuevo sobre la misma fila: conserva id, título, cwd,
// KV e historial, y sigue apendeando al mismo historial.
func (m *Manager) Restart(id string, cols, rows int) (*store.Session, error) {
	rec, err := m.st.GetSession(id)
	if err != nil {
		return nil, err
	}
	// La fuente de verdad de "está viva" es el daemon, no la fila: la fila
	// puede estar desactualizada por la ventana entre la muerte y el reap.
	vivas, err := m.pty.LiveIDs()
	if err != nil {
		return nil, err
	}
	for _, vid := range vivas {
		if vid == id {
			return nil, ErrAlreadyRunning
		}
	}

	if cols > 0 {
		rec.Cols = cols
	}
	if rows > 0 {
		rec.Rows = rows
	}
	if err := m.st.MarkStarting(id); err != nil {
		return nil, err
	}
	if err := m.spawn(rec, resumeBanner); err != nil {
		return nil, fmt.Errorf("reanudando %s: %w", id, err)
	}

	log.Printf("[%s] sesión reanudada (%dx%d)", id, rec.Cols, rec.Rows)
	return m.st.GetSession(id)
}

// Kill mata el proceso y conserva la fila y el historial.
//
// Es idempotente sobre una sesión ya muerta —el daemon contesta ErrNotLive y
// eso no es un error acá— pero distingue "ya estaba muerta" de "no existe",
// que es lo que el cliente necesita para saber si mostrar un 404.
func (m *Manager) Kill(id string) error {
	err := m.pty.Kill(id)
	if errors.Is(err, ptyapi.ErrNotLive) {
		_, gerr := m.st.GetSession(id)
		return gerr
	}
	return err
}

// Delete mata el proceso si vive y borra la fila con su KV, sus recursos y su
// historial. Es el acto destructivo explícito, separado de Kill a propósito.
func (m *Manager) Delete(id string) error {
	if err := m.Kill(id); err != nil && !errors.Is(err, store.ErrNotFound) {
		log.Printf("[%s] no se pudo matar antes de borrar; se borra igual: %v", id, err)
	}
	return m.st.DeleteSession(id)
}

// Sweep marca como muertas las filas que la base cree activas y el daemon no
// reporta vivas. Es una verificación de invariante, no el camino principal.
//
// Reemplaza al ReconcileBoot de M2: con un daemon recién arrancado, LiveIDs
// devuelve vacío y esto marca todo lo que había quedado activo. El
// comportamiento viejo, derivado en vez de hardcodeado.
//
// El orden de las dos lecturas es OBLIGATORIO y no se puede "ordenar mejor":
// ActiveIDs primero, LiveIDs después. No hay forma de que sean atómicas —con
// el daemon del otro lado, LiveIDs viaja por un socket— así que siempre hay
// una ventana entre las dos, y este orden es el que la hace inofensiva: una
// sesión spawneada en el medio no está en ActiveIDs pero sí en LiveIDs, y el
// peor caso es no barrer una fila que ya está bien. Al revés, esa misma sesión
// no aparecería en LiveIDs (leído antes de que naciera) pero sí en ActiveIDs
// (leído después), y el sweep marcaría muerta una sesión viva.
func (m *Manager) Sweep() int {
	activos, err := m.st.ActiveIDs()
	if err != nil {
		log.Printf("sweep: %v", err)
		return 0
	}
	if len(activos) == 0 {
		return 0
	}
	vivas, err := m.pty.LiveIDs()
	if err != nil {
		log.Printf("sweep: no se pudo consultar al daemon: %v", err)
		return 0
	}
	viva := make(map[string]bool, len(vivas))
	for _, id := range vivas {
		viva[id] = true
	}

	n := 0
	for _, id := range activos {
		if viva[id] {
			continue
		}
		// Releemos la fila antes de marcarla. Una sesión que murió entre las
		// dos lecturas ya tiene su exit_reason real —normal, killed— escrito
		// por el reap del daemon, y pisarlo con daemon_restart u orphaned es
		// corrupción de datos que el usuario ve en la UI. La ventana no se
		// cierra del todo sin un UPDATE condicional, pero pasa de "toda la
		// duración del sweep" a los microsegundos entre este SELECT y el
		// UPDATE de acá abajo.
		rec, err := m.st.GetSession(id)
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				log.Printf("sweep [%s]: %v", id, err)
			}
			continue
		}
		if rec.PtyStatus == store.StatusExited {
			continue
		}
		if err := m.st.MarkExited(id, m.reasonFor(rec), nil); err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				log.Printf("sweep [%s]: %v", id, err)
			}
			continue
		}
		log.Printf("[%s] la base la daba por activa pero el daemon no la tiene", id)
		n++
	}
	return n
}

// reasonFor distingue las dos formas de quedar huérfana. Una sesión creada
// antes de que el daemon arrancara se la llevó el reinicio; una posterior es
// deriva, y decir "daemon_restart" ahí sería mentir.
func (m *Manager) reasonFor(rec *store.Session) store.ExitReason {
	if m.daemonStartedAt == 0 || rec.CreatedAt < m.daemonStartedAt {
		return store.ReasonDaemonRestart
	}
	return store.ReasonOrphaned
}

func (m *Manager) sweepLoop() {
	defer m.wg.Done()
	ticker := time.NewTicker(m.cfg.SweepEvery)
	defer ticker.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-ticker.C:
			m.Sweep()
		}
	}
}

// LiveCount es cuántas sesiones tienen proceso corriendo ahora mismo.
func (m *Manager) LiveCount() int {
	ids, err := m.pty.LiveIDs()
	if err != nil {
		return 0
	}
	return len(ids)
}

// A partir de acá, todo se resuelve contra la base sin tocar al daemon. Es la
// parte que puede crecer sin costo: agregar una feature de contexto es sumar
// métodos acá.

func (m *Manager) List() ([]*store.Session, error)             { return m.st.ListSessions() }
func (m *Manager) Get(id string) (*store.Session, error)       { return m.st.GetSession(id) }
func (m *Manager) ListKV(id string) (map[string]string, error) { return m.st.ListKV(id) }
func (m *Manager) DeleteKV(id, key string) error               { return m.st.DeleteKV(id, key) }

func (m *Manager) UpdateMeta(id string, p store.MetaPatch) (*store.Session, error) {
	if err := m.st.UpdateMeta(id, p); err != nil {
		return nil, err
	}
	return m.st.GetSession(id)
}

func (m *Manager) SetKV(id, key, value string) error {
	if _, err := m.st.GetSession(id); err != nil {
		return err
	}
	return m.st.SetKV(id, key, value)
}

// LinkedResource es un recurso linkeado junto con su estado actual.
type LinkedResource struct {
	*store.Resource
	Snapshot *resources.Snapshot `json:"snapshot,omitempty"`
}

// ListResources devuelve los recursos de la sesión con su estado. El estado
// sale del caché, así que el polling del frontend no se traduce uno a uno en
// llamadas al sistema externo.
func (m *Manager) ListResources(ctx context.Context, sessionID string) ([]*LinkedResource, error) {
	if _, err := m.st.GetSession(sessionID); err != nil {
		return nil, err
	}
	rows, err := m.st.ListResources(sessionID)
	if err != nil {
		return nil, err
	}
	out := make([]*LinkedResource, 0, len(rows))
	for _, r := range rows {
		lr := &LinkedResource{Resource: r}
		if m.cfg.Resources != nil {
			lr.Snapshot = m.cfg.Resources.Get(ctx, resources.Ref{
				System: r.System, Type: r.Type, URL: r.Ref,
			})
		}
		out = append(out, lr)
	}
	return out, nil
}

// AddResource linkea un recurso a la sesión. system y type se infieren del
// propio link; se aceptan explícitos como escape hatch.
func (m *Manager) AddResource(sessionID, rawURL, system, typ string) (*store.Resource, error) {
	if _, err := m.st.GetSession(sessionID); err != nil {
		return nil, err
	}
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, ErrUnknownResource
	}
	ref := resources.Ref{System: system, Type: typ, URL: rawURL}
	if system == "" || typ == "" {
		if m.cfg.Resources == nil {
			return nil, ErrUnknownResource
		}
		resolved, ok := m.cfg.Resources.Resolve(rawURL)
		if !ok {
			return nil, ErrUnknownResource
		}
		ref = resolved
	}
	r := &store.Resource{SessionID: sessionID, System: ref.System, Type: ref.Type, Ref: ref.URL}
	if err := m.st.AddResource(r); err != nil {
		return nil, err
	}
	log.Printf("[%s] recurso linkeado: %s", sessionID, r.Ref)
	return r, nil
}

func (m *Manager) DeleteResource(sessionID string, id int64) error {
	if _, err := m.st.GetSession(sessionID); err != nil {
		return err
	}
	return m.st.DeleteResource(sessionID, id)
}
