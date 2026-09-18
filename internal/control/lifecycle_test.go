package control

import (
	"bytes"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/giuliano/webterm/internal/ptyapi"
	"github.com/giuliano/webterm/internal/session"
	"github.com/giuliano/webterm/internal/store"
)

// El sweep se alimenta del daemon, no de un mapa en memoria. Una fila que la
// base cree activa y el cliente de ptys no reporta viva, está muerta.
func TestSweepUsaLoQueReportaElClienteDePtys(t *testing.T) {
	m, st := newTestManager(t)

	// Una fila "viva" que nunca se spawneó: exactamente lo que queda después
	// de que el daemon arranque de nuevo. El CreatedAt va explícito y anterior
	// al arranque del dueño de los ptys, que es lo que la vuelve una víctima
	// del reinicio y no una fila a la deriva.
	orphan := &store.Session{
		ID: store.NewID(), Cwd: t.TempDir(), Shell: "/bin/sh",
		Cols: 80, Rows: 24, PtyStatus: store.StatusRunning,
		CreatedAt: m.pty.StartedAt() - 1000,
	}
	if err := st.CreateSession(orphan); err != nil {
		t.Fatal(err)
	}
	// Y una de verdad, que el sweep no debe tocar.
	alive, err := m.Create(CreateOpts{Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	if n := m.Sweep(); n != 1 {
		t.Fatalf("el sweep corrigió %d filas; quería 1", n)
	}

	got, err := st.GetSession(orphan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PtyStatus != store.StatusExited {
		t.Fatalf("la huérfana quedó %s; quería exited", got.PtyStatus)
	}
	if got.ExitReason != string(store.ReasonDaemonRestart) {
		t.Fatalf("exit_reason = %s; quería daemon_restart (la fila es anterior al arranque del daemon)",
			got.ExitReason)
	}

	still, err := st.GetSession(alive.ID)
	if err != nil {
		t.Fatal(err)
	}
	if still.PtyStatus != store.StatusRunning {
		t.Fatalf("el sweep se llevó puesta una sesión viva: %s", still.PtyStatus)
	}

	// Idempotente: en la segunda pasada ya no hay nada que corregir.
	if n := m.Sweep(); n != 0 {
		t.Fatalf("el segundo sweep corrigió %d filas", n)
	}
}

// Una fila trabada en starting —el orquestador crasheó entre el insert y el
// spawn— también la levanta el sweep. Si no, queda así para siempre.
func TestSweepLevantaFilasTrabadasEnStarting(t *testing.T) {
	m, st := newTestManager(t)
	stuck := &store.Session{
		ID: store.NewID(), Cwd: t.TempDir(), Shell: "/bin/sh",
		Cols: 80, Rows: 24, PtyStatus: store.StatusStarting,
	}
	if err := st.CreateSession(stuck); err != nil {
		t.Fatal(err)
	}

	if n := m.Sweep(); n != 1 {
		t.Fatalf("el sweep corrigió %d filas; quería 1", n)
	}
	got, _ := st.GetSession(stuck.ID)
	if got.PtyStatus != store.StatusExited {
		t.Fatalf("quedó %s; quería exited", got.PtyStatus)
	}
}

// El sweep no puede pisar el motivo real de una sesión que murió entre sus dos
// lecturas: si la fila ya dice exited, su exit_reason lo escribió el reap del
// daemon y es el verdadero. Pisarlo con orphaned es corrupción de datos que el
// usuario ve en la UI.
func TestSweepNoPisaElMotivoDeUnaFilaYaMarcada(t *testing.T) {
	st := newTestStore(t)
	pty := session.NewManager(st, session.Config{HistoryBytes: 64 << 10})
	hook := &hookPty{Client: pty}
	m := NewManager(st, hook, Config{Shell: "/bin/sh"})
	t.Cleanup(func() {
		_ = m.Close()
		_ = pty.Close()
		_ = st.Close()
	})

	rec, err := m.Create(CreateOpts{Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	// La sesión muere justo en la ventana entre las dos lecturas del sweep:
	// entró en ActiveSessions viva y sale de LiveIDs muerta, con su motivo real ya
	// escrito por el reap. Reproducido con un hook y no con timing.
	hook.onLiveIDs = func() {
		hook.onLiveIDs = nil
		if err := m.Kill(rec.ID); err != nil {
			t.Errorf("kill en la ventana del sweep: %v", err)
		}
	}

	n := m.Sweep()

	// El motivo real primero: es la propiedad que este test nombra, y ante una
	// regresión es el mensaje que hay que ver. El contador después.
	got, err := st.GetSession(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PtyStatus != store.StatusExited {
		t.Fatalf("pty_status = %q; quería exited", got.PtyStatus)
	}
	if got.ExitReason != string(store.ReasonKilled) {
		t.Fatalf("exit_reason = %q; el sweep pisó el motivo real con el suyo", got.ExitReason)
	}
	// Y no cuenta como corregida una fila que no tocó.
	if n != 0 {
		t.Fatalf("el sweep corrigió %d filas; quería 0", n)
	}
}

// Con el arranque del daemon conocido, el sweep distingue las dos formas de
// quedar huérfana en vez de mentir con un motivo fijo: lo anterior al arranque
// se lo llevó el reinicio; lo posterior es deriva.
func TestSweepDistingueOrphanedDeDaemonRestart(t *testing.T) {
	st := newTestStore(t)
	pty := session.NewManager(st, session.Config{HistoryBytes: 64 << 10})
	// El arranque sale del propio dueño de los ptys: es lo que el sweep le
	// pregunta, así que el test fecha contra lo mismo que el código.
	startedAt := pty.StartedAt()
	m := NewManager(st, pty, Config{Shell: "/bin/sh"})
	t.Cleanup(func() {
		_ = m.Close()
		_ = pty.Close()
		_ = st.Close()
	})

	older := &store.Session{
		ID: store.NewID(), Cwd: t.TempDir(), Shell: "/bin/sh", Cols: 80, Rows: 24,
		PtyStatus: store.StatusRunning, CreatedAt: startedAt - 1000,
	}
	newer := &store.Session{
		ID: store.NewID(), Cwd: t.TempDir(), Shell: "/bin/sh", Cols: 80, Rows: 24,
		PtyStatus: store.StatusRunning, CreatedAt: startedAt + 1000,
	}
	for _, rec := range []*store.Session{older, newer} {
		if err := st.CreateSession(rec); err != nil {
			t.Fatal(err)
		}
	}

	if n := m.Sweep(); n != 2 {
		t.Fatalf("el sweep corrigió %d filas; quería 2", n)
	}

	got, _ := st.GetSession(older.ID)
	if got.ExitReason != string(store.ReasonDaemonRestart) {
		t.Fatalf("la fila anterior al arranque quedó %q; quería daemon_restart", got.ExitReason)
	}
	got, _ = st.GetSession(newer.ID)
	if got.ExitReason != string(store.ReasonOrphaned) {
		t.Fatalf("la fila posterior al arranque quedó %q; quería orphaned", got.ExitReason)
	}
}

// Un `webterm daemon restart` reemplaza al dueño de los ptys sin que el
// orquestador se entere: mismo proceso, mismo cliente, otro daemon del otro
// lado del socket. El sweep tiene que fechar contra el daemon de AHORA.
//
// Con el arranque cacheado en la Config, las sesiones de este caso quedaban
// marcadas "orphaned" —que está documentado como "deriva entre la base y las
// sesiones vivas", o sea un bug— cuando en realidad se las llevó puestas un
// reinicio que pediste vos.
func TestSweepFechaContraElDaemonDeAhoraYNoContraElDelArranque(t *testing.T) {
	st := newTestStore(t)
	now := time.Now().UnixMilli()

	// El dueño original arrancó hace rato, y la sesión se creó contra él.
	pty := &swappableOwner{inner: &fakeOwner{startedAt: now - 10_000}}
	m := NewManager(st, pty, Config{Shell: "/bin/sh"})
	t.Cleanup(func() {
		_ = m.Close()
		_ = st.Close()
	})

	rec := &store.Session{
		ID: store.NewID(), Cwd: t.TempDir(), Shell: "/bin/sh", Cols: 80, Rows: 24,
		PtyStatus: store.StatusRunning, CreatedAt: now - 5_000,
	}
	if err := st.CreateSession(rec); err != nil {
		t.Fatal(err)
	}

	// `webterm daemon restart`: el dueño de los ptys es otro, arrancado
	// después de que esta sesión naciera.
	pty.swap(&fakeOwner{startedAt: now - 1_000})

	if n := m.Sweep(); n != 1 {
		t.Fatalf("el sweep corrigió %d filas; quería 1", n)
	}
	got, err := st.GetSession(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ExitReason != string(store.ReasonDaemonRestart) {
		t.Fatalf("exit_reason = %q; se la llevó un daemon restart, no la deriva", got.ExitReason)
	}
}

// El sweep no puede matar en la base una sesión que está naciendo.
//
// La secuencia es la del I1 del review final. Create inserta la fila en
// starting y recién entonces le pide el pty al daemon, que es un viaje por el
// socket más un fork/exec: decenas de milisegundos. Si el sweep corre en esa
// ventana ve la fila en ActiveSessions —starting cuenta como activo— y NO la ve
// en LiveIDs, porque el pty todavía no se registró. Para cuando va a marcarla,
// el spawn ya terminó: la fila dice running y hay un pty vivo detrás, y el
// sweep la mata igual.
//
// El resultado no se autorrepara: la fila dice exited, así que el attach cae a
// solo lectura, pero Restart consulta LiveIDs, la ve viva y devuelve 409 sobre
// una fila terminada. La única salida es Delete.
//
// El comentario largo del Sweep razonaba sobre el hermano de este caso —una
// fila que todavía no se insertó, que no está en ActiveSessions pero sí en
// LiveIDs, y cuyo peor caso es no barrer una fila que ya está bien—. Ese
// razonamiento era correcto cuando se escribió; el estado starting, agregado
// después, rompió su premisa.
func TestSweepNoMataUnaSesionQueEstaNaciendo(t *testing.T) {
	st := newTestStore(t)
	pty := newSlowSpawnOwner(st)
	m := NewManager(st, pty, Config{Shell: "/bin/sh"})
	t.Cleanup(func() {
		_ = m.Close()
		_ = st.Close()
	})

	created := make(chan error, 1)
	go func() {
		_, err := m.Create(CreateOpts{Cwd: t.TempDir()})
		created <- err
	}()

	// La fila ya está insertada en starting y el "fork" está en curso: es
	// exactamente la ventana donde el sweep se equivoca.
	<-pty.spawning

	n := m.Sweep()

	if err := <-created; err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := st.GetSession(pty.spawnedID)
	if err != nil {
		t.Fatal(err)
	}
	// El estado primero: es la propiedad que este test nombra.
	if got.PtyStatus != store.StatusRunning {
		t.Fatalf("la fila quedó %s/%s con el pty vivo detrás: el sweep mató una sesión que estaba naciendo",
			got.PtyStatus, got.ExitReason)
	}
	if n != 0 {
		t.Fatalf("el sweep corrigió %d filas; quería 0", n)
	}
}

// slowSpawnOwner modela lo único que importa del daemon para este test: que
// spawnear tarda, y que el resultado de LiveIDs es una foto sacada ANTES de
// que la respuesta termine de viajar.
//
// No hay ptys de verdad adentro: la fila la marca running él mismo, igual que
// hace session.Manager.Spawn, y "vivo" es una entrada en un mapa.
type slowSpawnOwner struct {
	ptyapi.Client
	st *store.Store

	spawning  chan struct{} // se cierra cuando Spawn empezó y está por trabarse
	proceed   chan struct{} // libera al Spawn trabado
	spawned   chan struct{} // se cierra cuando el spawn terminó de verdad
	spawnedID string        // seguro de leer después de <-spawned

	releaseOnce sync.Once

	mu   sync.Mutex
	live map[string]bool
}

func newSlowSpawnOwner(st *store.Store) *slowSpawnOwner {
	return &slowSpawnOwner{
		st:       st,
		spawning: make(chan struct{}),
		proceed:  make(chan struct{}),
		spawned:  make(chan struct{}),
		live:     map[string]bool{},
	}
}

func (f *slowSpawnOwner) Spawn(o ptyapi.SpawnOpts) error {
	f.spawnedID = o.ID
	close(f.spawning)
	<-f.proceed
	// El fork/exec tarda. La espera acá no es para sincronizar nada —eso lo
	// hacen los canales— sino para que el MarkRunning caiga en un milisegundo
	// distinto al del insert, que es lo que le da algo que comparar a la
	// guarda del sweep. En producción el viaje por el socket la paga sola.
	time.Sleep(2 * time.Millisecond)
	if err := f.st.MarkRunning(o.ID, o.Cols, o.Rows); err != nil {
		return err
	}
	f.mu.Lock()
	f.live[o.ID] = true
	f.mu.Unlock()
	close(f.spawned)
	return nil
}

// LiveIDs saca la foto, deja terminar el spawn y recién entonces contesta. Es
// la latencia de la respuesta viajando por el socket: lo que el sweep recibe
// describe un instante ya pasado.
func (f *slowSpawnOwner) LiveIDs() ([]string, error) {
	f.mu.Lock()
	ids := make([]string, 0, len(f.live))
	for id := range f.live {
		ids = append(ids, id)
	}
	f.mu.Unlock()

	select {
	case <-f.spawning:
		f.releaseOnce.Do(func() { close(f.proceed) })
		<-f.spawned
	default:
	}
	return ids, nil
}

func (f *slowSpawnOwner) StartedAt() int64 { return 0 }
func (f *slowSpawnOwner) Close() error     { return nil }

// fakeOwner es un dueño de ptys sin ptys: contesta cuándo arrancó y que no
// tiene nada vivo, que es todo lo que el sweep consulta. Los demás métodos los
// pone el embebido en nil: llamarlos sería un bug del sweep y panickea, que es
// justo lo que queremos que pase.
type fakeOwner struct {
	ptyapi.Client
	startedAt int64
}

func (f *fakeOwner) LiveIDs() ([]string, error) { return nil, nil }
func (f *fakeOwner) StartedAt() int64           { return f.startedAt }
func (f *fakeOwner) Close() error               { return nil }

// swappableOwner es un cliente cuyo dueño de ptys puede cambiar bajo los pies,
// igual que le pasa al daemonclient cuando reiniciás el daemon aparte.
type swappableOwner struct {
	mu    sync.Mutex
	inner ptyapi.Client
}

func (s *swappableOwner) swap(c ptyapi.Client) {
	s.mu.Lock()
	s.inner = c
	s.mu.Unlock()
}

func (s *swappableOwner) current() ptyapi.Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inner
}

func (s *swappableOwner) Spawn(o ptyapi.SpawnOpts) error { return s.current().Spawn(o) }
func (s *swappableOwner) Attach(id string) (ptyapi.Attachment, error) {
	return s.current().Attach(id)
}
func (s *swappableOwner) Kill(id string) error       { return s.current().Kill(id) }
func (s *swappableOwner) LiveIDs() ([]string, error) { return s.current().LiveIDs() }
func (s *swappableOwner) StartedAt() int64           { return s.current().StartedAt() }
func (s *swappableOwner) Close() error               { return s.current().Close() }

// hookPty deja correr algo entre las dos lecturas del sweep, que es donde vive
// la carrera. Debajo hay un manager de ptys de verdad.
type hookPty struct {
	ptyapi.Client
	onLiveIDs func()
}

func (h *hookPty) LiveIDs() ([]string, error) {
	if h.onLiveIDs != nil {
		h.onLiveIDs()
	}
	return h.Client.LiveIDs()
}

// Attach a una sesión muerta no consulta al cliente de ptys: lee el historial
// de la base y devuelve una conexión de solo lectura.
func TestAttachASesionMuertaEsDeSoloLectura(t *testing.T) {
	m, _ := newTestManager(t)
	rec, err := m.Create(CreateOpts{Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	att, err := m.Attach(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := att.Write([]byte("echo MARCA-HISTORIAL\n")); err != nil {
		t.Fatal(err)
	}
	waitForOutput(t, att, "MARCA-HISTORIAL")
	att.Detach()

	if err := m.Kill(rec.ID); err != nil {
		t.Fatal(err)
	}

	dead, err := m.Attach(rec.ID)
	if err != nil {
		t.Fatalf("attach a sesión muerta tendría que funcionar: %v", err)
	}
	defer dead.Detach()
	if dead.Live {
		t.Fatal("Live = true en una sesión muerta")
	}
	if dead.Output() != nil {
		t.Fatal("una sesión muerta no tiene stream vivo")
	}
	if !bytes.Contains(dead.History, []byte("MARCA-HISTORIAL")) {
		t.Fatalf("el historial no sobrevivió al kill: %q", dead.History)
	}
}

// La versión con la sesión terminándose sola: el historial sobrevive igual y
// el input a una sesión muerta no revive nada.
func TestAttachASesionMuerta(t *testing.T) {
	m, _ := newTestManager(t)
	rec, err := m.Create(CreateOpts{Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	att, err := m.Attach(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := att.Write([]byte("echo antes-de-morir\n")); err != nil {
		t.Fatal(err)
	}
	waitForOutput(t, att, "antes-de-morir")
	if err := att.Write([]byte("exit\n")); err != nil {
		t.Fatal(err)
	}
	waitForClose(t, att)
	att.Detach()

	dead, err := m.Attach(rec.ID)
	if err != nil {
		t.Fatalf("Attach a sesión muerta: %v", err)
	}
	defer dead.Detach()
	if dead.Live {
		t.Fatal("Live tendría que ser false")
	}
	if dead.Output() != nil {
		t.Fatal("una sesión muerta no tiene stream vivo")
	}
	if !bytes.Contains(dead.History, []byte("antes-de-morir")) {
		t.Fatalf("el historial no sobrevivió: %q", tail(dead.History, 200))
	}
	// El input a una sesión muerta no revive nada.
	if err := dead.Write([]byte("echo tarde\n")); err == nil {
		t.Fatal("escribir a una sesión muerta tendría que fallar")
	}
}

func TestKillEsIdempotenteYDistingueInexistente(t *testing.T) {
	m, _ := newTestManager(t)
	rec, err := m.Create(CreateOpts{Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	if err := m.Kill(rec.ID); err != nil {
		t.Fatal(err)
	}
	// Segundo kill: la fila existe y ya está muerta, así que no es un error.
	if err := m.Kill(rec.ID); err != nil {
		t.Fatalf("segundo kill dio %v; quería nil", err)
	}
	// Pero una sesión que no existe sí lo es.
	if err := m.Kill("no-existe"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("kill inexistente dio %v; quería ErrNotFound", err)
	}
}

// El spawn fallido no puede dejar la fila trabada en starting: la UI mostraría
// una sesión arrancando para siempre.
func TestCreateConShellInvalidoNoDejaLaFilaEnStarting(t *testing.T) {
	m, st := newTestManager(t)
	m.cfg.Shell = "/no/existe/este/shell"

	rec, err := m.Create(CreateOpts{Cwd: t.TempDir()})
	if err == nil {
		t.Fatal("tendría que fallar")
	}
	// Create devuelve error pero la fila queda, marcada, para que el error se
	// vea en la UI. Buscamos la única fila que haya.
	rows, lerr := st.ListSessions()
	if lerr != nil || len(rows) != 1 {
		t.Fatalf("esperaba una fila; filas=%v err=%v", rows, lerr)
	}
	got := rows[0]
	if rec != nil && got.ID != rec.ID {
		t.Fatalf("la fila no es la de la sesión creada")
	}
	if got.PtyStatus == store.StatusStarting {
		t.Fatal("la fila quedó trabada en starting")
	}
	if got.ExitReason != string(store.ReasonSpawnFailed) {
		t.Fatalf("exit_reason = %q; quería spawn_failed", got.ExitReason)
	}
}

// TestRestartReusaLaFila: reanudar conserva id, título, KV e historial. Es lo
// que M6 va a necesitar para colgarle el `claude --resume`.
func TestRestartReusaLaFila(t *testing.T) {
	m, st := newTestManager(t)
	rec, err := m.Create(CreateOpts{Title: "con historia", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetKV(rec.ID, "claude_session_id", "abc-123"); err != nil {
		t.Fatal(err)
	}

	att, err := m.Attach(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := att.Write([]byte("echo antes-del-restart\n")); err != nil {
		t.Fatal(err)
	}
	waitForOutput(t, att, "antes-del-restart")
	att.Detach()
	if err := m.Kill(rec.ID); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	resumed, err := m.Restart(rec.ID, 100, 30)
	if err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if resumed.ID != rec.ID {
		t.Fatalf("Restart cambió el id: %s -> %s", rec.ID, resumed.ID)
	}
	if resumed.Title != "con historia" {
		t.Fatalf("se perdió el título: %q", resumed.Title)
	}
	if resumed.PtyStatus != store.StatusRunning {
		t.Fatalf("pty_status = %q", resumed.PtyStatus)
	}
	if resumed.ExitReason != "" || resumed.ExitCode != nil {
		t.Fatalf("quedaron rastros de la muerte anterior: %+v", resumed)
	}
	if resumed.Cols != 100 || resumed.Rows != 30 {
		t.Fatalf("el restart no tomó el tamaño nuevo: %dx%d", resumed.Cols, resumed.Rows)
	}

	kv, _ := st.ListKV(rec.ID)
	if kv["claude_session_id"] != "abc-123" {
		t.Fatalf("se perdió el KV: %v", kv)
	}

	att2, err := m.Attach(rec.ID)
	if err != nil {
		t.Fatalf("Attach después del restart: %v", err)
	}
	defer att2.Detach()
	if !att2.Live {
		t.Fatal("la sesión reanudada no está viva")
	}
	if !bytes.Contains(att2.History, []byte("antes-del-restart")) {
		t.Fatalf("el replay perdió lo anterior al restart: %q", tail(att2.History, 300))
	}
	if !bytes.Contains(att2.History, []byte("sesión reanudada")) {
		t.Fatalf("falta el marcador de reanudación: %q", tail(att2.History, 300))
	}

	// Y el proceso nuevo responde.
	if err := att2.Write([]byte("echo despues-del-restart\n")); err != nil {
		t.Fatal(err)
	}
	waitForOutput(t, att2, "despues-del-restart")
}

func TestRestartSobreSesionViva(t *testing.T) {
	m, _ := newTestManager(t)
	rec, err := m.Create(CreateOpts{Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := m.Restart(rec.ID, 80, 24); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("se esperaba ErrAlreadyRunning, vino %v", err)
	}
}

// Cuando el stream se corta porque la sesión terminó, la fila lo dice y trae el
// motivo real. Esta es la mitad fácil de la señal.
func TestFinDeStreamPorSesionTerminada(t *testing.T) {
	m, _ := newTestManager(t)
	rec, err := m.Create(CreateOpts{Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	att, err := m.Attach(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer att.Detach()

	if end := att.End(); end.Exited {
		t.Fatal("la sesión está viva y End() dice que terminó")
	}
	if err := att.Write([]byte("exit\n")); err != nil {
		t.Fatal(err)
	}
	waitForClose(t, att)

	end := att.End()
	if !end.Exited {
		t.Fatal("el stream cerró con la sesión terminada y End() no lo dice")
	}
	if end.Session == nil || end.Session.ExitReason != string(store.ReasonNormal) {
		t.Fatalf("End() no trajo el motivo real: %+v", end.Session)
	}
}

// Y la mitad que importa: el stream se corta pero la sesión sigue viva. Es lo
// que pasa cuando al cliente lo expulsan por lento o se cae el transporte, y no
// se puede decidir por el frame `dropped` —es best-effort y justo en ese caso
// puede no salir—. La fila es la que manda.
func TestFinDeStreamConLaSesionViva(t *testing.T) {
	m, _ := newTestManager(t)
	rec, err := m.Create(CreateOpts{Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	att, err := m.Attach(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Detach corta el stream de este cliente sin tocar la sesión: es la misma
	// forma que tiene un cliente expulsado.
	att.Detach()
	waitForClose(t, att)

	end := att.End()
	if end.Exited {
		t.Fatal("el cliente se fue pero la sesión sigue viva; End() dice que terminó")
	}
	if end.Session == nil || end.Session.PtyStatus != store.StatusRunning {
		t.Fatalf("la fila tendría que seguir en running: %+v", end.Session)
	}
}

// Con el daemon caído se tiene que poder leer el historial igual.
//
// Es el I3(a) del review final: control.Attach caía a solo lectura solo ante
// ptyapi.ErrNotLive, y un error de transporte no lo es, así que
// GET /ws/terminal sobre una sesión huérfana devolvía 500. Mirar qué pasó en
// una sesión es exactamente lo que uno quiere hacer cuando algo se cayó, y el
// historial vive en la base, que es del orquestador: que el daemon no esté no
// nos impide leerlo.
func TestAttachConElDaemonCaidoCaeASoloLectura(t *testing.T) {
	st := newTestStore(t)
	pty := session.NewManager(st, session.Config{HistoryBytes: 64 << 10})
	dead := &deadOwner{Client: pty}
	m := NewManager(st, dead, Config{Shell: "/bin/sh"})
	t.Cleanup(func() {
		_ = m.Close()
		_ = pty.Close()
		_ = st.Close()
	})

	rec, err := m.Create(CreateOpts{Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	att, err := m.Attach(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := att.Write([]byte("echo MARCA-PREVIA\n")); err != nil {
		t.Fatal(err)
	}
	waitForOutput(t, att, "MARCA-PREVIA")
	att.Detach()
	// El historial se baja a disco batcheado; sin esto la base puede no tener
	// todavía lo que el stream ya mostró.
	waitForHistory(t, st, rec.ID, "MARCA-PREVIA")

	// El daemon se cae. La fila sigue diciendo running, que es justamente el
	// estado en el que esto fallaba.
	dead.down.Store(true)

	off, err := m.Attach(rec.ID)
	if err != nil {
		t.Fatalf("con el daemon caído tendría que poder leerse el historial: %v", err)
	}
	defer off.Detach()
	if off.Live {
		t.Fatal("Live = true con el daemon caído")
	}
	if !bytes.Contains(off.History, []byte("MARCA-PREVIA")) {
		t.Fatalf("el historial no llegó: %q", off.History)
	}
}

// Un daemon caído no se arregla solo: el sweep tiene que pedir que lo levanten.
//
// Es el I3(b). Antes, ensureDaemon corría una sola vez al arrancar el
// orquestador y no había ningún camino de reconexión: con el daemon muerto, la
// UI mostraba sesiones running para siempre y POST /api/sessions daba 500 hasta
// que reiniciaras el orquestador a mano.
func TestSweepPideLevantarElDaemonCaido(t *testing.T) {
	st := newTestStore(t)
	dead := &deadOwner{Client: &fakeOwner{}}
	dead.down.Store(true)

	llamadas := 0
	m := NewManager(st, dead, Config{Shell: "/bin/sh", EnsureDaemon: func() error {
		llamadas++
		return nil
	}})
	t.Cleanup(func() {
		_ = m.Close()
		_ = st.Close()
	})

	rec := &store.Session{
		ID: store.NewID(), Cwd: t.TempDir(), Shell: "/bin/sh",
		Cols: 80, Rows: 24, PtyStatus: store.StatusRunning,
	}
	if err := st.CreateSession(rec); err != nil {
		t.Fatal(err)
	}

	if n := m.Sweep(); n != 0 {
		t.Fatalf("el sweep corrigió %d filas sin poder consultar al daemon; tiene que corregir 0", n)
	}
	if llamadas != 1 {
		t.Fatalf("EnsureDaemon se llamó %d veces; quería exactamente 1 por barrido", llamadas)
	}
	// Y no inventa: sin saber qué hay vivo, la fila queda como estaba.
	got, _ := st.GetSession(rec.ID)
	if got.PtyStatus != store.StatusRunning {
		t.Fatalf("la fila quedó %s; sin daemon no se puede afirmar nada", got.PtyStatus)
	}
}

// Y sin callback —que es como lo construyen todos los tests y como puede
// construirlo cualquiera— el sweep tiene que seguir andando igual.
func TestSweepConEnsureDaemonNilNoExplota(t *testing.T) {
	st := newTestStore(t)
	dead := &deadOwner{Client: &fakeOwner{}}
	dead.down.Store(true)
	m := NewManager(st, dead, Config{Shell: "/bin/sh"})
	t.Cleanup(func() {
		_ = m.Close()
		_ = st.Close()
	})
	if n := m.Sweep(); n != 0 {
		t.Fatalf("el sweep corrigió %d filas; quería 0", n)
	}
}

// LiveCount deja ver el error del transporte en vez de devolver 0.
//
// Es el I4: con el daemon caído, /api/health decía "sessions: 0" mientras
// /api/sessions listaba una running. Dos endpoints del mismo proceso
// contradiciéndose, justo cuando ese endpoint es la única señal de que algo
// pasó.
func TestLiveCountNoSeComeElErrorDelDaemon(t *testing.T) {
	st := newTestStore(t)
	dead := &deadOwner{Client: &fakeOwner{}}
	m := NewManager(st, dead, Config{Shell: "/bin/sh"})
	t.Cleanup(func() {
		_ = m.Close()
		_ = st.Close()
	})

	if n, err := m.LiveCount(); err != nil || n != 0 {
		t.Fatalf("con el daemon vivo: n=%d err=%v", n, err)
	}
	dead.down.Store(true)
	if _, err := m.LiveCount(); err == nil {
		t.Fatal("con el daemon caído, LiveCount devolvió un número como si supiera")
	}
}

// deadOwner es un dueño de ptys al que se le puede cortar el socket: con down
// prendido, todo falla con un error de transporte, que es lo que ve el
// orquestador cuando al daemon lo mataron con kill -9.
type deadOwner struct {
	ptyapi.Client
	down atomic.Bool
}

// errDaemonCaido imita lo que devuelve el daemonclient con el socket muerto: un
// error común y silvestre, que NO es ninguno de los del contrato. Ese es todo
// el punto: el orquestador no puede reconocerlo con un errors.Is.
var errDaemonCaido = errors.New("dial unix /tmp/webterm.sock: connect: connection refused")

func (d *deadOwner) Spawn(o ptyapi.SpawnOpts) error {
	if d.down.Load() {
		return errDaemonCaido
	}
	return d.Client.Spawn(o)
}

func (d *deadOwner) Attach(id string) (ptyapi.Attachment, error) {
	if d.down.Load() {
		return nil, errDaemonCaido
	}
	return d.Client.Attach(id)
}

func (d *deadOwner) Kill(id string) error {
	if d.down.Load() {
		return errDaemonCaido
	}
	return d.Client.Kill(id)
}

func (d *deadOwner) LiveIDs() ([]string, error) {
	if d.down.Load() {
		return nil, errDaemonCaido
	}
	return d.Client.LiveIDs()
}

func (d *deadOwner) StartedAt() int64 {
	if d.down.Load() {
		return 0
	}
	return d.Client.StartedAt()
}

func (d *deadOwner) Close() error { return d.Client.Close() }

// waitForHistory espera a que el historial batcheado llegue a la base.
func waitForHistory(t *testing.T, st *store.Store, id, mark string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		hist, err := st.ReadOutput(id)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(hist, []byte(mark)) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("el historial nunca llegó a la base con %q", mark)
}
