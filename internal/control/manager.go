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

	"github.com/giuliano/webterm/internal/events"
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
	// Events recibe los avisos de cambio para que el server se los reparta a
	// los navegadores. Nil es válido y significa no publicar: es como lo
	// construyen los tests y cualquier uso sin UI.
	Events *events.Bus
	// ExtraEnv son variables que se suman al entorno de cada pty. El
	// entrypoint las arma; así el daemon no necesita saber que existe un token.
	ExtraEnv []string
	// SweepEvery es cada cuánto corre la verificación de invariante.
	SweepEvery time.Duration
	// EnsureDaemon levanta el dueño de los ptys si se cayó. El sweep lo llama
	// cuando LiveIDs falla por transporte.
	//
	// Es un callback y no un método de ptyapi.Client porque no es algo que el
	// dueño de los ptys pueda contestar de sí mismo: un daemon muerto no habla,
	// y spawnear un proceso desde os.Executable() es capacidad de la capa cmd.
	// El contrato queda limpio y la única dependencia rara vive acá, opcional y
	// nombrada.
	//
	// Nil es un valor válido: control tiene que andar sin esto —es como lo
	// construyen todos los tests— y ahí un daemon caído simplemente no se
	// levanta solo.
	EnsureDaemon func() error
}

// Manager es el dueño del estado de las sesiones. Los ptys son de otro.
type Manager struct {
	st  *store.Store
	pty ptyapi.Client
	cfg Config

	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup

	// hookMu serializa ApplyHook, que lee el estado y escribe el siguiente.
	hookMu sync.Mutex
}

func NewManager(st *store.Store, pty ptyapi.Client, cfg Config) *Manager {
	if cfg.SweepEvery <= 0 {
		cfg.SweepEvery = defaultSweepInterval
	}
	return &Manager{st: st, pty: pty, cfg: cfg, stop: make(chan struct{})}
}

// publish avisa un cambio, si hay a quién. Concentra el chequeo de nil para
// que los puntos de escritura sean una línea y no un if.
func (m *Manager) publish(kind events.Kind, sessionID string) {
	if m.cfg.Events == nil {
		return
	}
	m.cfg.Events.Publish(kind, sessionID)
}

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
		home, err := os.UserHomeDir()
		if err != nil {
			// El cwd resuelto tiene que quedar SIEMPRE en la fila y en el
			// SpawnOpts. Un cwd vacío no falla: cae al $HOME del proceso que
			// tiene los ptys (ver terminal.New), que con el daemon aparte es
			// el equivocado, y la columna cwd de la fila pasa a mentir sobre
			// dónde corre el proceso. Es el mismo modo de falla que Shell.
			//
			// Caemos a "/" en vez de propagar el error porque no saber el home
			// no es motivo para no poder abrir una terminal: "/" siempre
			// existe, es un cwd válido, y sobre todo es cierto. Un directorio
			// raro pero honesto es mejor que un dato corrupto.
			log.Printf("no se pudo resolver el home; la sesión arranca en /: %v", err)
			home = "/"
		}
		cwd = home
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
	got, err := m.st.GetSession(rec.ID)
	if err != nil {
		return nil, err
	}
	m.publish(events.SessionCreated, got.ID)
	return got, nil
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
		} else {
			// La fila cambió (starting → exited) aunque el llamador se vaya
			// con error: publicar acá, en el único lugar que escribe este
			// motivo de falla, cubre a Create y Restart de una sola vez.
			m.publish(events.SessionUpdated, rec.ID)
		}
	}
	return err
}

// Restart spawnea un pty nuevo sobre la misma fila: conserva id, título, cwd,
// KV e historial, y sigue apendeando al mismo historial.
//
// OJO: la seguridad de esto ante dos Restart concurrentes NO vive acá. El
// chequeo de LiveIDs de más abajo es una cortesía —dice 409 sin ir al daemon en
// el caso normal— pero no es atómico: dos requests pueden pasarlo los dos.
// Quien realmente impide el pty duplicado es el spawnMu de internal/session,
// que serializa Spawn entero y hace que el segundo se encuentre con la sesión
// ya en el mapa y devuelva ptyapi.ErrAlreadyLive (que el server traduce a 409).
//
// Es el único lugar del orquestador donde la corrección depende de un candado
// de otro paquete —y encima de otro proceso, con el daemon aparte— así que
// queda escrito: si alguna vez se toca el spawnMu de session, este camino es el
// que se rompe, y se rompe sin que ningún test de control falle.
func (m *Manager) Restart(id string, cols, rows int) (*store.Session, error) {
	rec, err := m.st.GetSession(id)
	if err != nil {
		return nil, err
	}
	// La fuente de verdad de "está viva" es el daemon, no la fila: la fila
	// puede estar desactualizada por la ventana entre la muerte y el reap.
	liveIDs, err := m.pty.LiveIDs()
	if err != nil {
		return nil, err
	}
	for _, vid := range liveIDs {
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
	got, err := m.st.GetSession(id)
	if err != nil {
		return nil, err
	}
	m.publish(events.SessionUpdated, id)
	return got, nil
}

// Kill mata el proceso y conserva la fila y el historial.
//
// Es idempotente sobre una sesión ya muerta —el daemon contesta ErrNotLive y
// eso no es un error acá— pero distingue "ya estaba muerta" de "no existe",
// que es lo que el cliente necesita para saber si mostrar un 404.
func (m *Manager) Kill(id string) error {
	err := m.pty.Kill(id)
	if errors.Is(err, ptyapi.ErrNotLive) {
		// Ya estaba muerta: este llamado no escribió nada, así que no hay
		// cambio de estado que avisar.
		_, gerr := m.st.GetSession(id)
		return gerr
	}
	if err != nil {
		return err
	}
	m.publish(events.SessionUpdated, id)
	return nil
}

// Delete mata el proceso si vive y borra la fila con su KV, sus recursos y su
// historial. Es el acto destructivo explícito, separado de Kill a propósito.
func (m *Manager) Delete(id string) error {
	if err := m.Kill(id); err != nil && !errors.Is(err, store.ErrNotFound) {
		log.Printf("[%s] no se pudo matar antes de borrar; se borra igual: %v", id, err)
	}
	if err := m.st.DeleteSession(id); err != nil {
		return err
	}
	m.publish(events.SessionDeleted, id)
	return nil
}

// Sweep marca como muertas las filas que la base cree activas y el daemon no
// reporta vivas. Es una verificación de invariante, no el camino principal.
//
// Reemplaza al ReconcileBoot de M2: con un daemon recién arrancado, LiveIDs
// devuelve vacío y esto marca todo lo que había quedado activo. El
// comportamiento viejo, derivado en vez de hardcodeado.
//
// Hay una ventana entre las dos lecturas y no se puede cerrar: con el daemon
// del otro lado, LiveIDs viaja por un socket, así que no hay forma de que sean
// atómicas. Una sesión que nace en el medio cae de uno de dos lados, y los dos
// hay que atenderlos:
//
//   - la fila todavía no se insertó: no está en ActiveSessions pero sí llega a
//     estar en LiveIDs. Inofensivo: el peor caso es no barrer una fila que ya
//     está bien. De esto se ocupa el orden ActiveSessions → LiveIDs, que por eso
//     es OBLIGATORIO y no se puede "ordenar mejor": al revés, la misma sesión no
//     aparecería en LiveIDs (leído antes de que naciera) pero sí en
//     ActiveSessions (leído después).
//   - la fila YA se insertó, en starting, y el pty todavía no se registró: está
//     en ActiveSessions y no está en LiveIDs, o sea que este barrido la da por
//     muerta. El orden no la salva —es el caso hermano, el que el estado
//     starting agregó después de que se escribiera esta regla— y matarla no se
//     autorrepara: la fila diría exited con el pty corriendo, el attach caería a
//     solo lectura y Restart devolvería 409. De esto se ocupa el CAS de
//     MarkExitedIfUnchanged, que compara la fila contra cómo la leímos: un
//     spawn que termine en el medio la cambia y el UPDATE no matchea.
func (m *Manager) Sweep() int {
	active, err := m.st.ActiveSessions()
	if err != nil {
		log.Printf("sweep: %v", err)
		return 0
	}
	if len(active) == 0 {
		return 0
	}
	liveIDs, err := m.pty.LiveIDs()
	if err != nil {
		// Con el daemon caído no sabemos nada: no marcamos nada —marcar sería
		// inventar— pero sí intentamos levantarlo, porque si no el orquestador
		// se queda para siempre mostrando sesiones running que ya no existen y
		// sin poder crear ninguna nueva. Ver ensureDaemon.
		log.Printf("sweep: no se pudo consultar al daemon: %v", err)
		m.reviveDaemon()
		return 0
	}
	live := make(map[string]bool, len(liveIDs))
	for _, id := range liveIDs {
		live[id] = true
	}
	// El arranque del dueño de los ptys se pregunta acá, una vez por barrido, y
	// no se guarda en la Config: `webterm daemon restart` reemplaza al daemon
	// sin reiniciar al orquestador, y un valor tomado al arrancar seguiría
	// fechando contra un proceso que ya murió. El resultado sería etiquetar
	// "orphaned" —o sea, "hay un bug"— a sesiones que se llevó puestas un
	// reinicio que pediste vos. Una vez por barrido y no por fila porque es un
	// viaje por el socket y todas las filas de este barrido merecen la misma
	// referencia temporal.
	startedAt := m.pty.StartedAt()

	n := 0
	for _, prev := range active {
		if live[prev.ID] {
			continue
		}
		// El CAS del UPDATE es lo que hace inofensiva la ventana entre las dos
		// lecturas, en sus dos direcciones: una sesión que murió mientras tanto
		// ya tiene su exit_reason real —normal, killed— escrito por el reap, y
		// una que nació mientras tanto ya tiene pty. En los dos casos la fila
		// cambió respecto de cómo la leímos y este UPDATE no la toca. Sin el
		// CAS, el sweep pisaría el motivo real en el primer caso y mataría una
		// sesión viva en el segundo.
		marked, err := m.st.MarkExitedIfUnchanged(prev, m.reasonFor(prev.ID, startedAt), nil)
		if err != nil {
			log.Printf("sweep [%s]: %v", prev.ID, err)
			continue
		}
		if !marked {
			// La fila cambió debajo nuestro. Es el caso normal de la carrera,
			// no un error, y si quedó algo por barrer lo agarra el barrido que
			// viene.
			continue
		}
		log.Printf("[%s] la base la daba por activa pero el daemon no la tiene", prev.ID)
		m.publish(events.SessionUpdated, prev.ID)
		n++
	}
	return n
}

// reviveDaemon intenta levantar de nuevo al dueño de los ptys.
//
// El callback es opcional y viene de afuera porque spawnear un proceso desde
// os.Executable() es capacidad de la capa cmd, no algo que quepa en ptyapi: el
// contrato es "el dueño de los ptys se describe a sí mismo", y un daemon caído
// no puede describirse. Con EnsureDaemon en nil —como lo construyen todos los
// tests— esto no hace nada y el sweep se comporta como antes.
//
// Un solo intento por barrido: si el daemon no arranca, el próximo sweep vuelve
// a probar 30 s después. Reintentar en loop acá no arreglaría nada que el
// intervalo no arregle y llenaría el log.
func (m *Manager) reviveDaemon() {
	if m.cfg.EnsureDaemon == nil {
		return
	}
	if err := m.cfg.EnsureDaemon(); err != nil {
		log.Printf("sweep: no se pudo levantar el daemon de nuevo: %v", err)
		return
	}
	log.Print("sweep: el daemon volvió; el próximo barrido reconcilia las filas")
}

// reasonFor distingue las dos formas de quedar huérfana. Una sesión creada
// antes de que el daemon arrancara se la llevó el reinicio; una posterior es
// deriva, y decir "daemon_restart" ahí sería mentir.
//
// startedAt llega por parámetro y no se lee de un campo: es el arranque del
// daemon que está vivo AHORA, tal como lo contestó este barrido.
func (m *Manager) reasonFor(id string, startedAt int64) store.ExitReason {
	if startedAt == 0 {
		return store.ReasonDaemonRestart
	}
	rec, err := m.st.GetSession(id)
	if err != nil || rec.CreatedAt < startedAt {
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
//
// Devuelve el error en vez de comérselo: antes, un daemon que no contestaba se
// traducía en 0, y /api/health decía "sessions: 0" mientras /api/sessions
// listaba una running. Dos endpoints del mismo proceso contradiciéndose, y
// justo en el momento en que ese endpoint es la única señal de que algo pasó.
// Con error != nil el número no significa nada y quien llama tiene que decirlo
// así.
func (m *Manager) LiveCount() (int, error) {
	ids, err := m.pty.LiveIDs()
	if err != nil {
		return 0, err
	}
	return len(ids), nil
}

// A partir de acá, todo se resuelve contra la base sin tocar al daemon. Es la
// parte que puede crecer sin costo: agregar una feature de contexto es sumar
// métodos acá.

func (m *Manager) List() ([]*store.Session, error)             { return m.st.ListSessions() }
func (m *Manager) Get(id string) (*store.Session, error)       { return m.st.GetSession(id) }
func (m *Manager) ListKV(id string) (map[string]string, error) { return m.st.ListKV(id) }

func (m *Manager) DeleteKV(id, key string) error {
	if err := m.st.DeleteKV(id, key); err != nil {
		// Borrar una clave que no estaba no cambió nada: no vale un evento.
		return err
	}
	m.publish(events.ContextRemoved, id)
	return nil
}

func (m *Manager) UpdateMeta(id string, p store.MetaPatch) (*store.Session, error) {
	if err := m.st.UpdateMeta(id, p); err != nil {
		return nil, err
	}
	got, err := m.st.GetSession(id)
	if err != nil {
		return nil, err
	}
	m.publish(events.SessionUpdated, id)
	return got, nil
}

func (m *Manager) SetKV(id, key, value string) error {
	if _, err := m.st.GetSession(id); err != nil {
		return err
	}
	if err := m.st.SetKV(id, key, value); err != nil {
		return err
	}
	m.publish(events.ContextUpdated, id)
	return nil
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
// ListFolders devuelve los folders ordenados por nombre.
func (m *Manager) ListFolders() ([]*store.Folder, error) { return m.st.ListFolders() }

// CreateFolder crea un folder vacío.
func (m *Manager) CreateFolder(name string) (*store.Folder, error) {
	f, err := m.st.CreateFolder(name)
	if err != nil {
		return nil, err
	}
	m.publish(events.FolderCreated, "")
	return f, nil
}

// RenameFolder le cambia el nombre.
func (m *Manager) RenameFolder(id, name string) error {
	if err := m.st.RenameFolder(id, name); err != nil {
		return err
	}
	m.publish(events.FolderUpdated, "")
	return nil
}

// DeleteFolder borra el folder; sus sesiones quedan sin folder.
//
// El evento que sale es folder.deleted y no uno por cada sesión que quedó
// suelta: el cliente refetchea la lista entera igual, y emitir N eventos por
// una acción sola llenaría el buffer de los suscriptores lentos al pedo.
func (m *Manager) DeleteFolder(id string) error {
	if err := m.st.DeleteFolder(id); err != nil {
		return err
	}
	m.publish(events.FolderDeleted, "")
	return nil
}

// SetSessionFolder mueve una sesión a un folder, o la saca con nil.
func (m *Manager) SetSessionFolder(sessionID string, folderID *string) error {
	if err := m.st.SetSessionFolder(sessionID, folderID); err != nil {
		return err
	}
	// Lo que cambió es la sesión, no el folder: es la fila que la UI tiene que
	// volver a dibujar, y encima en otro lugar de la lista.
	m.publish(events.SessionUpdated, sessionID)
	return nil
}

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
	m.publish(events.ResourceAdded, sessionID)
	return r, nil
}

func (m *Manager) DeleteResource(sessionID string, id int64) error {
	if _, err := m.st.GetSession(sessionID); err != nil {
		return err
	}
	if err := m.st.DeleteResource(sessionID, id); err != nil {
		return err
	}
	m.publish(events.ResourceRemoved, sessionID)
	return nil
}

// ListTags devuelve los tags en uso con cuántas sesiones lleva cada uno.
func (m *Manager) ListTags() ([]store.TagCount, error) { return m.st.ListTags() }

// SetSessionTags reemplaza los tags de la sesión. Como con los folders, lo que
// cambió es la sesión: el evento es session.updated, y de ahí el cliente
// recalcula también la lista de tags existentes.
func (m *Manager) SetSessionTags(sessionID string, tags []string) error {
	if err := m.st.SetSessionTags(sessionID, tags); err != nil {
		return err
	}
	m.publish(events.SessionUpdated, sessionID)
	return nil
}

// AddSessionTags suma tags sin tocar los que ya tenía.
func (m *Manager) AddSessionTags(sessionID string, tags []string) error {
	if err := m.st.AddSessionTags(sessionID, tags); err != nil {
		return err
	}
	m.publish(events.SessionUpdated, sessionID)
	return nil
}

// RemoveSessionTags saca esos tags de la sesión.
func (m *Manager) RemoveSessionTags(sessionID string, tags []string) error {
	if err := m.st.RemoveSessionTags(sessionID, tags); err != nil {
		return err
	}
	m.publish(events.SessionUpdated, sessionID)
	return nil
}
