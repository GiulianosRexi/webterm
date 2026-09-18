package session

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/giuliano/webterm/internal/resources"
	"github.com/giuliano/webterm/internal/store"
	"github.com/giuliano/webterm/internal/terminal"
)

const (
	// Tamaño del chunk de lectura del pty.
	readBufSize = 32 * 1024
	// Cuántos chunks se le bufferean a un cliente antes de darlo por lento.
	subBuffer = 256
	// Cada cuánto se verifica que la DB y el mapa de sesiones vivas coincidan.
	defaultSweepInterval = 30 * time.Second
	// Cuánto se espera a que una sesión muerta termine de reconciliarse.
	reapTimeout = 5 * time.Second
)

// DefaultHistoryBytes es el cap de historial por sesión.
const DefaultHistoryBytes int64 = 1 << 20

var (
	// ErrNotLive lo devuelven las operaciones que necesitan un proceso vivo.
	ErrNotLive = errors.New("la sesión no está corriendo")
	// ErrAlreadyRunning lo devuelve Restart sobre una sesión que no murió.
	ErrAlreadyRunning = errors.New("la sesión ya está corriendo")
)

// resumeBanner queda en el historial para que el replay muestre dónde se
// cortó la sesión anterior.
var resumeBanner = []byte("\r\n\x1b[90m— sesión reanudada —\x1b[0m\r\n")

// Config parametriza el manager.
type Config struct {
	Shell        string        // shell a spawnear; vacío = $SHELL
	HistoryBytes int64         // cap de historial por sesión
	SweepEvery   time.Duration // cada cuánto corre la verificación de invariante
	// Resources resuelve el estado de los recursos externos linkeados. Si es
	// nil, linkear por URL deja de funcionar pero el resto del manager anda
	// igual: la integración es opcional y no puede tumbar las sesiones.
	Resources *resources.Cache
}

// Manager es el dueño de los ptys vivos. Es la única capa que compone
// terminal con store; el servidor HTTP habla solo con él.
type Manager struct {
	st  *store.Store
	cfg Config

	mu   sync.RWMutex
	live map[string]*liveSession

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

// NewManager construye el manager. No toca la base hasta Start.
func NewManager(st *store.Store, cfg Config) *Manager {
	if cfg.HistoryBytes <= 0 {
		cfg.HistoryBytes = DefaultHistoryBytes
	}
	if cfg.SweepEvery <= 0 {
		cfg.SweepEvery = defaultSweepInterval
	}
	return &Manager{
		st:   st,
		cfg:  cfg,
		live: map[string]*liveSession{},
		stop: make(chan struct{}),
	}
}

// Start reconcilia lo que quedó de la ejecución anterior y arranca la
// verificación periódica. Tiene que correr antes de aceptar requests: si no,
// hay una ventana en la que la API reporta vivas sesiones que no lo están.
func (m *Manager) Start() error {
	n, err := m.st.ReconcileBoot()
	if err != nil {
		return err
	}
	if n > 0 {
		log.Printf("reconciliadas %d sesiones que el reinicio del backend se llevó puestas", n)
	}
	m.wg.Add(1)
	go m.sweepLoop()
	return nil
}

// Close apaga el manager: mata las sesiones vivas y espera a que todas
// terminen de reconciliarse.
func (m *Manager) Close() error {
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
	m.wg.Wait()
	return nil
}

// CreateOpts describe la sesión a crear.
type CreateOpts struct {
	Title       string
	Description string
	Cwd         string
	Cols, Rows  int
}

// Create persiste la sesión y spawnea su pty.
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
		PtyStatus: store.StatusRunning,
	}

	pt, err := terminal.New(rec.ID, terminal.Config{
		Shell: shell, Cwd: cwd, Rows: uint16(o.Rows), Cols: uint16(o.Cols),
	})
	if err != nil {
		// Dejamos la fila igual, marcada como fallida: así el error aparece
		// en la UI en vez de perderse en un log del servidor.
		now := time.Now().UnixMilli()
		rec.PtyStatus = store.StatusExited
		rec.ExitReason = string(store.ReasonSpawnFailed)
		rec.ExitedAt = &now
		if cerr := m.st.CreateSession(rec); cerr != nil {
			log.Printf("[%s] no se pudo registrar el spawn fallido: %v", rec.ID, cerr)
		}
		return nil, fmt.Errorf("spawneando la sesión: %w", err)
	}

	// El insert va bajo el mismo candado que el registro en el mapa: el sweep
	// toma RLock, así que nunca puede ver una fila viva sin sesión asociada y
	// declararla huérfana por error.
	m.mu.Lock()
	if err := m.st.CreateSession(rec); err != nil {
		m.mu.Unlock()
		_ = pt.Close()
		return nil, err
	}
	m.startLive(rec, pt, nil)
	m.mu.Unlock()

	log.Printf("[%s] sesión creada (%dx%d) en %s", rec.ID, o.Cols, o.Rows, cwd)
	return rec, nil
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

// Attachment es la conexión de un cliente a una sesión.
type Attachment struct {
	Session *store.Session
	// History es el replay que hay que mandar antes del stream vivo.
	History []byte
	// Live dice si hay proceso corriendo. Si es false, Output es nil y la
	// conexión queda de solo lectura.
	Live   bool
	Output <-chan []byte

	sub  *subscriber
	live *liveSession
}

// Detach desconecta al cliente sin tocar la sesión.
func (a *Attachment) Detach() {
	if a.live != nil && a.sub != nil {
		a.live.detach(a.sub)
	}
}

// Dropped dice si al cliente lo expulsamos por no leer a tiempo.
func (a *Attachment) Dropped() bool {
	return a.sub != nil && a.sub.wasDropped()
}

// Attach conecta un cliente. Una sesión muerta se attachea igual, en modo
// lectura: así ver su historial no necesita una vista aparte.
func (m *Manager) Attach(id string) (*Attachment, error) {
	rec, err := m.st.GetSession(id)
	if err != nil {
		return nil, err
	}

	l := m.lookup(id)
	if l == nil {
		hist, err := m.st.ReadOutput(id)
		if err != nil {
			return nil, err
		}
		return &Attachment{Session: rec, History: sanitizeReplay(hist)}, nil
	}

	hist, sub := l.attach(subBuffer)
	_ = m.st.TouchActive(id)
	return &Attachment{
		Session: rec, History: sanitizeReplay(hist), Live: true,
		Output: sub.out(), sub: sub, live: l,
	}, nil
}

// sanitizeReplay prepara el tail para un cliente nuevo. El historial está
// cortado en el cap, así que puede empezar en medio de un carácter UTF-8 y
// arrastrar atributos de color abiertos antes del corte.
func sanitizeReplay(p []byte) []byte {
	for len(p) > 0 && p[0]&0xC0 == 0x80 {
		p = p[1:]
	}
	if len(p) == 0 {
		return nil
	}
	return append([]byte("\x1b[0m"), p...)
}

// Write manda input al pty.
func (m *Manager) Write(id string, p []byte) error {
	l := m.lookup(id)
	if l == nil {
		return ErrNotLive
	}
	if _, err := l.pty.Write(p); err != nil {
		return err
	}
	return nil
}

// Resize cambia el tamaño del pty y lo persiste, para que al reanudar la
// sesión vuelva con las dimensiones que tenía.
func (m *Manager) Resize(id string, rows, cols uint16) error {
	l := m.lookup(id)
	if l == nil {
		return ErrNotLive
	}
	if err := l.pty.Resize(rows, cols); err != nil {
		return err
	}
	return m.st.UpdateSize(id, int(cols), int(rows))
}

// List devuelve todas las sesiones, la más nueva primero.
func (m *Manager) List() ([]*store.Session, error) { return m.st.ListSessions() }

// Get devuelve una sesión por id.
func (m *Manager) Get(id string) (*store.Session, error) { return m.st.GetSession(id) }

// UpdateMeta aplica un update parcial y devuelve la sesión ya actualizada.
func (m *Manager) UpdateMeta(id string, p store.MetaPatch) (*store.Session, error) {
	if err := m.st.UpdateMeta(id, p); err != nil {
		return nil, err
	}
	return m.st.GetSession(id)
}

// LiveCount es cuántas sesiones tienen proceso corriendo ahora mismo.
func (m *Manager) LiveCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.live)
}

// ListKV, SetKV y DeleteKV son el contexto persistido de la sesión. El
// servidor no habla con el store directamente: todo pasa por acá.
func (m *Manager) ListKV(id string) (map[string]string, error) { return m.st.ListKV(id) }

func (m *Manager) SetKV(id, key, value string) error {
	if _, err := m.st.GetSession(id); err != nil {
		return err
	}
	return m.st.SetKV(id, key, value)
}

func (m *Manager) DeleteKV(id, key string) error { return m.st.DeleteKV(id, key) }

// Kill mata el proceso y conserva la fila y el historial. Es sincrónico: al
// volver, la DB ya refleja la muerte, así que un GET inmediato no miente.
// Es idempotente sobre una sesión ya muerta, pero devuelve ErrNotFound si no
// existe.
func (m *Manager) Kill(id string) error {
	l := m.lookup(id)
	if l == nil {
		_, err := m.st.GetSession(id)
		return err
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

// Restart spawnea un pty nuevo sobre la misma fila: conserva id, título, cwd,
// KV e historial, y sigue apendeando al mismo historial. Reusar la fila es lo
// que va a permitir en M6 reanudar con `claude --resume` usando el KV de la
// propia sesión.
func (m *Manager) Restart(id string, cols, rows int) (*store.Session, error) {
	rec, err := m.st.GetSession(id)
	if err != nil {
		return nil, err
	}
	if m.lookup(id) != nil {
		return nil, ErrAlreadyRunning
	}
	if cols <= 0 {
		cols = rec.Cols
	}
	if rows <= 0 {
		rows = rec.Rows
	}

	pt, err := terminal.New(id, terminal.Config{
		Shell: rec.Shell, Cwd: rec.Cwd, Rows: uint16(rows), Cols: uint16(cols),
	})
	if err != nil {
		code := -1
		_ = m.st.MarkExited(id, store.ReasonSpawnFailed, &code)
		return nil, fmt.Errorf("reanudando %s: %w", id, err)
	}

	m.mu.Lock()
	if err := m.st.MarkRunning(id, cols, rows); err != nil {
		m.mu.Unlock()
		_ = pt.Close()
		return nil, err
	}
	m.startLive(rec, pt, resumeBanner)
	m.mu.Unlock()

	rec.PtyStatus = store.StatusRunning
	rec.Cols, rec.Rows = cols, rows
	rec.ExitReason, rec.ExitCode, rec.ExitedAt = "", nil, nil

	log.Printf("[%s] sesión reanudada (%dx%d)", id, cols, rows)
	return rec, nil
}

// Delete mata el proceso si vive y borra la fila con su KV y su historial.
// Es el acto destructivo explícito, separado de Kill a propósito.
func (m *Manager) Delete(id string) error {
	if l := m.lookup(id); l != nil {
		l.killed.Store(true)
		_ = l.pty.Kill()
		select {
		case <-l.reaped:
		case <-time.After(reapTimeout):
			log.Printf("[%s] no terminó a tiempo; se borra igual", id)
		}
	}
	return m.st.DeleteSession(id)
}

// Sweep marca como muertas las filas que la DB cree vivas pero que no tienen
// sesión asociada. Es una verificación de invariante, no el camino principal:
// en condiciones normales reap() siempre llega primero. Existe para que un bug
// del camino principal se autocorrija en vez de dejar la UI mintiendo.
// Devuelve cuántas filas corrigió.
func (m *Manager) Sweep() int {
	ids, err := m.st.RunningIDs()
	if err != nil {
		log.Printf("sweep: %v", err)
		return 0
	}
	n := 0
	for _, id := range ids {
		if m.lookup(id) != nil {
			continue
		}
		if err := m.st.MarkExited(id, store.ReasonOrphaned, nil); err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				log.Printf("sweep [%s]: %v", id, err)
			}
			continue
		}
		log.Printf("[%s] huérfana: la DB la daba por viva pero no hay proceso detrás", id)
		n++
	}
	return n
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

func (m *Manager) lookup(id string) *liveSession {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.live[id]
}

// ErrUnknownResource se re-exporta para que el servidor traduzca el error a un
// status sin tener que importar el paquete resources.
var ErrUnknownResource = resources.ErrUnknownResource

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

// AddResource linkea un recurso a la sesión.
//
// system y type se infieren del propio link: es mejor UX —pegás la URL y
// listo— y es el seam que generaliza, porque sumar otro sistema es sumar un
// provider al registry sin tocar este contrato. Se aceptan explícitos como
// escape hatch para un formato que el registry todavía no conozca.
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

	r := &store.Resource{
		SessionID: sessionID, System: ref.System, Type: ref.Type, Ref: ref.URL,
	}
	if err := m.st.AddResource(r); err != nil {
		return nil, err
	}
	log.Printf("[%s] recurso linkeado: %s", sessionID, r.Ref)
	return r, nil
}

// DeleteResource desvincula el recurso de la sesión.
func (m *Manager) DeleteResource(sessionID string, id int64) error {
	if _, err := m.st.GetSession(sessionID); err != nil {
		return err
	}
	return m.st.DeleteResource(sessionID, id)
}
