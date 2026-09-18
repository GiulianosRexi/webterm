package session

import (
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/giuliano/webterm/internal/ptyapi"
	"github.com/giuliano/webterm/internal/store"
	"github.com/giuliano/webterm/internal/terminal"
)

const (
	// Tamaño del chunk de lectura del pty.
	readBufSize = 32 * 1024
	// Cuántos chunks se le bufferean a un cliente antes de darlo por lento.
	subBuffer = 256
	// Cuánto se espera a que una sesión muerta termine de reconciliarse.
	reapTimeout = 5 * time.Second
)

// DefaultHistoryBytes es el cap de historial por sesión.
const DefaultHistoryBytes int64 = 1 << 20

// Config parametriza el manager. Es corta a propósito: todo lo que no sea el
// pty —shell por defecto, variables de entorno, recursos externos— lo resuelve
// el orquestador y llega resuelto en cada SpawnOpts.
type Config struct {
	HistoryBytes int64 // cap de historial por sesión
}

// Manager es el dueño de los ptys vivos. Es la implementación en proceso de
// ptyapi.Client: del otro lado del contrato puede estar esto o el cliente del
// daemon, y quien lo use no tiene que notar la diferencia.
//
// De la base toca solo lo que es estado del pty: el historial y las columnas
// pty_status, exit_*, cols y rows. La metadata —título, KV, recursos— es del
// orquestador y acá no se la conoce.
type Manager struct {
	st  *store.Store
	cfg Config

	// startedAt es cuándo nació este manager. Es inmutable después de
	// NewManager, así que leerlo desde cualquier goroutine es seguro sin
	// candado: no hay quién lo escriba.
	startedAt int64

	mu   sync.RWMutex
	live map[string]*liveSession

	// spawnMu serializa Spawn entero. Ver el comentario de Spawn.
	spawnMu sync.Mutex

	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// liveSession es una sesión con proceso corriendo.
type liveSession struct {
	id  string
	pty *terminal.Session

	// mu sincroniza ring y hub juntos: attach tiene que snapshotear el tail y
	// suscribirse en una sola operación atómica, o el cliente que entra se
	// pierde un chunk o lo ve dos veces.
	mu   sync.Mutex
	ring *ring
	hub  *hub

	writer *outputWriter
	killed atomic.Bool

	pumpDone chan struct{}
	reaped   chan struct{}
}

// emit manda un chunk al ring, a los clientes y al historial.
func (l *liveSession) emit(p []byte) {
	l.mu.Lock()
	l.ring.append(p)
	l.hub.broadcast(p)
	l.mu.Unlock()
	l.writer.write(p)
}

func (l *liveSession) attach(bufSize int) ([]byte, *subscriber) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.ring.snapshot(), l.hub.subscribe(bufSize)
}

func (l *liveSession) detach(s *subscriber) {
	l.mu.Lock()
	l.hub.unsubscribe(s)
	l.mu.Unlock()
}

// NewManager construye el manager. No toca la base: la reconciliación de lo
// que quedó de la ejecución anterior es del orquestador, que es el único que
// puede distinguir una fila huérfana de una sesión que todavía no arrancó.
func NewManager(st *store.Store, cfg Config) *Manager {
	if cfg.HistoryBytes <= 0 {
		cfg.HistoryBytes = DefaultHistoryBytes
	}
	return &Manager{
		st:        st,
		cfg:       cfg,
		startedAt: time.Now().UnixMilli(),
		live:      map[string]*liveSession{},
		stop:      make(chan struct{}),
	}
}

// Close apaga el manager: mata las sesiones vivas y espera a que todas
// terminen de reconciliarse.
//
// Toma spawnMu, así que Close y Spawn son mutuamente excluyentes. Sin eso, un
// Spawn que tomara m.mu después de la foto dejaría un pty que este Close no
// mata —un shell del usuario corriendo sin dueño— y sumaría al WaitGroup que
// este Wait ya está esperando, que es uso indebido y puede panickear.
func (m *Manager) Close() error {
	m.spawnMu.Lock()
	m.stopOnce.Do(func() { close(m.stop) })

	m.mu.RLock()
	live := make([]*liveSession, 0, len(m.live))
	for _, l := range m.live {
		live = append(live, l)
	}
	m.mu.RUnlock()

	for _, l := range live {
		l.killed.Store(true)
		_ = l.pty.Kill()
	}
	// El candado se suelta antes del Wait: ya con stop cerrado, cualquier Spawn
	// que estaba esperando se va a encontrar con ptyapi.ErrClosed, así que no
	// hace falta bloquearlo todo el tiempo que tarden los reaps.
	m.spawnMu.Unlock()

	m.wg.Wait()
	return nil
}

// Spawn arranca el pty de una sesión cuya fila ya existe.
//
// No inserta nada: la fila la crea el orquestador antes de llamar acá, porque
// session_output tiene FK contra sessions y el historial empieza a escribirse
// apenas arranca el pump.
//
// Está serializado entero bajo spawnMu: spawnear es raro y barato de
// serializar, y sin eso dos Spawn concurrentes del mismo id podrían pasar los
// dos el chequeo de "no está vivo" y dejar un pty huérfano en el mapa. El
// mismo candado lo toma Close, que es lo que garantiza que un apagado no deje
// atrás un pty recién arrancado.
//
// Shell y Cwd vacíos no caen a lo que diga la fila: caen al entorno del dueño
// del pty ($SHELL y $HOME del proceso que corre esto, ver terminal.New). Con
// el daemon como proceso aparte ese es el entorno equivocado, así que el
// orquestador manda los dos siempre resueltos.
func (m *Manager) Spawn(o ptyapi.SpawnOpts) error {
	m.spawnMu.Lock()
	defer m.spawnMu.Unlock()

	// El error es del contrato, no del paquete: quien pidió la sesión tiene que
	// enterarse de que no va a existir —la fila ya quedó insertada del otro
	// lado— y tiene que poder hacerlo igual esté el daemon en este proceso o
	// del otro lado de un socket.
	select {
	case <-m.stop:
		return ptyapi.ErrClosed
	default:
	}

	rec, err := m.st.GetSession(o.ID)
	if err != nil {
		return err
	}
	if m.lookup(o.ID) != nil {
		return ptyapi.ErrAlreadyLive
	}
	if o.Cols <= 0 {
		o.Cols = rec.Cols
	}
	if o.Rows <= 0 {
		o.Rows = rec.Rows
	}

	pt, err := terminal.New(o.ID, terminal.Config{
		Shell: o.Shell, Cwd: o.Cwd,
		Rows: uint16(o.Rows), Cols: uint16(o.Cols), Env: o.Env,
	})
	if err != nil {
		// El error queda en la fila, no en un log: así aparece en la UI en vez
		// de perderse.
		code := -1
		if merr := m.st.MarkExited(o.ID, store.ReasonSpawnFailed, &code); merr != nil {
			log.Printf("[%s] no se pudo registrar el spawn fallido: %v", o.ID, merr)
		}
		return fmt.Errorf("spawneando la sesión %s: %w", o.ID, err)
	}

	// El MarkRunning y el registro en el mapa van bajo el mismo candado, que es
	// lo único que el candado garantiza: que no se intercalen entre sí. Nadie
	// que mire la base y el mapa por separado obtiene de acá una foto atómica
	// de los dos.
	m.mu.Lock()
	if err := m.st.MarkRunning(o.ID, o.Cols, o.Rows); err != nil {
		m.mu.Unlock()
		_ = pt.Close()
		return err
	}
	m.startLive(rec, pt, []byte(o.Banner))
	m.mu.Unlock()

	log.Printf("[%s] pty arrancado (%dx%d) en %s", o.ID, o.Cols, o.Rows, o.Cwd)
	return nil
}

// startLive arma la sesión viva y lanza sus goroutines. Hay que llamarla con
// m.mu tomado.
func (m *Manager) startLive(rec *store.Session, pt *terminal.Session, banner []byte) *liveSession {
	l := &liveSession{
		id:       rec.ID,
		pty:      pt,
		ring:     newRing(int(m.cfg.HistoryBytes)),
		hub:      newHub(),
		writer:   newOutputWriter(m.st, rec.ID, m.cfg.HistoryBytes),
		pumpDone: make(chan struct{}),
		reaped:   make(chan struct{}),
	}
	// Sembramos el ring con lo que ya hay en la base para que una sesión
	// reanudada replaye también lo anterior al corte.
	if hist, err := m.st.ReadOutput(rec.ID); err != nil {
		log.Printf("[%s] no se pudo leer el historial: %v", rec.ID, err)
	} else {
		l.ring.preload(hist)
	}
	if len(banner) > 0 {
		// Antes de arrancar el pump, así el marcador queda antes del prompt.
		l.ring.append(banner)
		l.writer.write(banner)
	}

	m.live[rec.ID] = l
	m.wg.Add(2)
	go m.pump(l)
	go m.reap(l)
	return l
}

// pump lee el pty y reparte cada chunk al ring, a los clientes y al historial.
func (m *Manager) pump(l *liveSession) {
	defer m.wg.Done()
	defer close(l.pumpDone)

	buf := make([]byte, readBufSize)
	for {
		n, err := l.pty.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			l.emit(chunk)
		}
		if err != nil {
			return
		}
	}
}

// reap espera la muerte del proceso y deja la DB y los clientes consistentes.
//
// El orden importa y es load-bearing: primero se marca la fila y recién
// después se cierran los clientes. Así, cuando a un cliente se le cierra el
// canal de output, la fila ya dice exited con su código, y todo el manejo de
// fin de sesión puede leerla sin esperar nada.
func (m *Manager) reap(l *liveSession) {
	defer m.wg.Done()

	<-l.pty.Done()
	// El ptmx ya está cerrado, así que el pump sale enseguida. Lo esperamos
	// para que no quede escribiendo después del flush final.
	<-l.pumpDone
	l.writer.close()

	reason := store.ReasonNormal
	if l.killed.Load() {
		reason = store.ReasonKilled
	}
	code := l.pty.ExitCode()

	m.mu.Lock()
	if m.live[l.id] == l {
		delete(m.live, l.id)
	}
	m.mu.Unlock()

	if err := m.st.MarkExited(l.id, reason, &code); err != nil && !errors.Is(err, store.ErrNotFound) {
		log.Printf("[%s] no se pudo marcar la salida: %v", l.id, err)
	}

	l.mu.Lock()
	l.hub.closeAll()
	l.mu.Unlock()

	close(l.reaped)
	log.Printf("[%s] sesión terminada (%s, código %d)", l.id, reason, code)
}

// Attach conecta un cliente al pty. Una sesión sin proceso da ErrNotLive: el
// camino de solo lectura sobre el historial es del orquestador, que lo resuelve
// contra la base sin consultar acá.
func (m *Manager) Attach(id string) (ptyapi.Attachment, error) {
	l := m.lookup(id)
	if l == nil {
		return nil, ptyapi.ErrNotLive
	}
	hist, sub := l.attach(subBuffer)
	if sub.isClosed() {
		// Carrera con reap: pasamos el lookup antes de que borrara del mapa,
		// pero llegamos al hub después de que cerrara a todos. La sesión está
		// muerta, y devolver un attachment cuyo Output ya nadie va a cerrar
		// rompería el contrato justo donde el consumidor hace `for range`.
		return nil, ptyapi.ErrNotLive
	}
	return &Attachment{m: m, live: l, history: ptyapi.SanitizeReplay(hist), sub: sub}, nil
}

// Kill mata el proceso y conserva la fila y el historial. Es sincrónico: al
// volver, la DB ya refleja la muerte, así que un GET inmediato no miente.
func (m *Manager) Kill(id string) error {
	l := m.lookup(id)
	if l == nil {
		return ptyapi.ErrNotLive
	}
	l.killed.Store(true)
	if err := l.pty.Kill(); err != nil {
		return err
	}
	select {
	case <-l.reaped:
		return nil
	case <-time.After(reapTimeout):
		return fmt.Errorf("la sesión %s no terminó a tiempo", id)
	}
}

// LiveIDs son las sesiones con proceso corriendo. Es la fuente de verdad del
// sweep del orquestador: lo que no está acá y la base cree activo, está muerto.
func (m *Manager) LiveIDs() ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := make([]string, 0, len(m.live))
	for id := range m.live {
		ids = append(ids, id)
	}
	return ids, nil
}

// StartedAt es cuándo arrancó este dueño de ptys. Lo usa el sweep del
// orquestador para distinguir una sesión que se llevó puesta el reinicio del
// daemon de una fila que quedó a la deriva.
func (m *Manager) StartedAt() int64 { return m.startedAt }

func (m *Manager) lookup(id string) *liveSession {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.live[id]
}

// El manager en proceso es una implementación de ptyapi.Client igual que el
// cliente del daemon. Esta línea es lo que hace que romper el contrato falle
// al compilar y no en runtime.
var _ ptyapi.Client = (*Manager)(nil)
