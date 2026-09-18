package session

import (
	"bytes"
	"errors"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/giuliano/webterm/internal/ptyapi"
	"github.com/giuliano/webterm/internal/store"
)

// newTestManager arma un manager sobre una base temporal. A diferencia de M2,
// el manager ya no inserta filas: la fila la crea quien lo llama, igual que
// hace el orquestador en producción.
func newTestManager(t *testing.T) (*Manager, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(st, Config{HistoryBytes: 64 << 10})
	t.Cleanup(func() {
		_ = m.Close()
		_ = st.Close()
	})
	return m, st
}

// spawnTest inserta la fila y arranca el pty, que es la secuencia que hace el
// orquestador. Devuelve el id.
func spawnTest(t *testing.T, m *Manager, st *store.Store, env []string) string {
	t.Helper()
	rec := &store.Session{
		ID: store.NewID(), Cwd: t.TempDir(), Shell: "/bin/sh",
		Cols: 80, Rows: 24, PtyStatus: store.StatusStarting,
	}
	if err := st.CreateSession(rec); err != nil {
		t.Fatal(err)
	}
	if err := m.Spawn(ptyapi.SpawnOpts{
		ID: rec.ID, Shell: rec.Shell, Cwd: rec.Cwd, Cols: 80, Rows: 24, Env: env,
	}); err != nil {
		t.Fatal(err)
	}
	return rec.ID
}

// waitFor espera hasta que cond sea verdadera. Los ptys son asincrónicos y un
// sleep fijo es la receta de un test que falla una vez cada veinte.
func waitFor(t *testing.T, plazo time.Duration, motivo string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(plazo)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout esperando: %s", motivo)
}

// awaitChunk acumula output del canal hasta encontrar want.
func awaitChunk(t *testing.T, ch <-chan []byte, want string) string {
	t.Helper()
	var sb strings.Builder
	deadline := time.After(15 * time.Second)
	for {
		select {
		case chunk, ok := <-ch:
			if !ok {
				t.Fatalf("el canal se cerró esperando %q (acumulado %q)", want, sb.String())
			}
			sb.Write(chunk)
			if strings.Contains(sb.String(), want) {
				return sb.String()
			}
		case <-deadline:
			t.Fatalf("timeout esperando %q (acumulado %q)", want, sb.String())
		}
	}
}

func tail(p []byte, n int) string {
	if len(p) > n {
		p = p[len(p)-n:]
	}
	return string(p)
}

func TestSpawnNecesitaLaFila(t *testing.T) {
	m, _ := newTestManager(t)
	// Sin fila no hay spawn: session_output tiene FK contra sessions, así que
	// un pty sin fila dejaría el historial sin dónde escribirse.
	err := m.Spawn(ptyapi.SpawnOpts{ID: "no-existe", Shell: "/bin/sh", Cwd: t.TempDir(), Cols: 80, Rows: 24})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Spawn sin fila dio %v; quería ErrNotFound", err)
	}
}

func TestSpawnSobreSesionVivaEsConflicto(t *testing.T) {
	m, st := newTestManager(t)
	id := spawnTest(t, m, st, nil)

	err := m.Spawn(ptyapi.SpawnOpts{ID: id, Shell: "/bin/sh", Cwd: t.TempDir(), Cols: 80, Rows: 24})
	if !errors.Is(err, ptyapi.ErrAlreadyLive) {
		t.Fatalf("Spawn duplicado dio %v; quería ErrAlreadyLive", err)
	}
}

func TestSpawnMarcaRunning(t *testing.T) {
	m, st := newTestManager(t)
	id := spawnTest(t, m, st, nil)

	rec, err := st.GetSession(id)
	if err != nil {
		t.Fatal(err)
	}
	if rec.PtyStatus != store.StatusRunning {
		t.Fatalf("pty_status = %s; quería running", rec.PtyStatus)
	}
}

func TestSpawnFallidoMarcaSpawnFailed(t *testing.T) {
	m, st := newTestManager(t)
	rec := &store.Session{
		ID: store.NewID(), Cwd: t.TempDir(), Shell: "/no/existe/este/shell",
		Cols: 80, Rows: 24, PtyStatus: store.StatusStarting,
	}
	if err := st.CreateSession(rec); err != nil {
		t.Fatal(err)
	}

	if err := m.Spawn(ptyapi.SpawnOpts{
		ID: rec.ID, Shell: rec.Shell, Cwd: rec.Cwd, Cols: 80, Rows: 24,
	}); err == nil {
		t.Fatal("un shell inexistente tendría que fallar")
	}

	// El error tiene que quedar en la fila, no perderse en un log: es lo que
	// hace que aparezca en la UI.
	got, err := st.GetSession(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PtyStatus != store.StatusExited || got.ExitReason != string(store.ReasonSpawnFailed) {
		t.Fatalf("quedó %s/%s; quería exited/spawn_failed", got.PtyStatus, got.ExitReason)
	}
}

func TestSpawnConBannerLoDejaEnElHistorial(t *testing.T) {
	m, st := newTestManager(t)
	rec := &store.Session{
		ID: store.NewID(), Cwd: t.TempDir(), Shell: "/bin/sh",
		Cols: 80, Rows: 24, PtyStatus: store.StatusStarting,
	}
	if err := st.CreateSession(rec); err != nil {
		t.Fatal(err)
	}
	if err := m.Spawn(ptyapi.SpawnOpts{
		ID: rec.ID, Shell: rec.Shell, Cwd: rec.Cwd, Cols: 80, Rows: 24,
		Banner: "MARCADOR-DE-REANUDACION",
	}); err != nil {
		t.Fatal(err)
	}

	att, err := m.Attach(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer att.Detach()
	if !bytes.Contains(att.History(), []byte("MARCADOR-DE-REANUDACION")) {
		t.Fatalf("el banner no está en el replay: %q", att.History())
	}
}

func TestAttachASesionNoVivaDaErrNotLive(t *testing.T) {
	m, st := newTestManager(t)
	id := spawnTest(t, m, st, nil)
	if err := m.Kill(id); err != nil {
		t.Fatal(err)
	}

	// El daemon no sabe leer historiales de sesiones muertas: ese camino es
	// del orquestador, que lo resuelve contra la base sin consultarlo.
	if _, err := m.Attach(id); !errors.Is(err, ptyapi.ErrNotLive) {
		t.Fatalf("Attach a sesión muerta dio %v; quería ErrNotLive", err)
	}
}

func TestLiveIDs(t *testing.T) {
	m, st := newTestManager(t)
	a := spawnTest(t, m, st, nil)
	b := spawnTest(t, m, st, nil)

	ids, err := m.LiveIDs()
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(ids)
	quiero := []string{a, b}
	sort.Strings(quiero)
	if !reflect.DeepEqual(ids, quiero) {
		t.Fatalf("LiveIDs = %v; quería %v", ids, quiero)
	}

	if err := m.Kill(a); err != nil {
		t.Fatal(err)
	}
	ids, _ = m.LiveIDs()
	if !reflect.DeepEqual(ids, []string{b}) {
		t.Fatalf("después del kill LiveIDs = %v; quería [%s]", ids, b)
	}
}

// TestSobreviveAlDetach es la premisa entera de M2: cerrar el cliente no mata
// el proceso, y al volver se ve lo que pasó mientras tanto.
func TestSobreviveAlDetach(t *testing.T) {
	m, st := newTestManager(t)
	id := spawnTest(t, m, st, nil)

	att, err := m.Attach(id)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if err := att.Write([]byte("echo marca-uno\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	awaitChunk(t, att.Output(), "marca-uno")
	att.Detach()

	// Con el cliente desconectado, el proceso sigue trabajando: mandamos el
	// comando y nos vamos enseguida, así el output se produce sin nadie
	// escuchando, que es lo que el test tiene que probar.
	emisor, err := m.Attach(id)
	if err != nil {
		t.Fatalf("Attach para escribir: %v", err)
	}
	if err := emisor.Write([]byte("echo marca-dos\n")); err != nil {
		t.Fatalf("Write con el cliente desconectado: %v", err)
	}
	emisor.Detach()

	// Volver a attachear es el camino real del cliente que vuelve: lo que pasó
	// mientras tanto tiene que estar en el replay.
	var replay []byte
	waitFor(t, 15*time.Second, "que el replay traiga lo que pasó estando desconectado", func() bool {
		vuelta, err := m.Attach(id)
		if err != nil {
			t.Fatalf("re-Attach: %v", err)
		}
		replay = vuelta.History()
		vuelta.Detach()
		return bytes.Contains(replay, []byte("marca-dos"))
	})
	if !bytes.Contains(replay, []byte("marca-uno")) {
		t.Fatalf("el replay perdió lo de antes del detach: %q", tail(replay, 200))
	}
}

// TestExitDelShellSeReconcilia: el estado en la DB sigue al proceso real.
func TestExitDelShellSeReconcilia(t *testing.T) {
	m, st := newTestManager(t)
	id := spawnTest(t, m, st, nil)

	att, err := m.Attach(id)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer att.Detach()
	if err := att.Write([]byte("exit 5\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// El canal se cierra cuando la sesión muere, y para entonces la fila ya
	// está marcada.
	deadline := time.After(15 * time.Second)
	for open := true; open; {
		select {
		case _, ok := <-att.Output():
			open = ok
		case <-deadline:
			t.Fatal("timeout esperando el cierre del canal")
		}
	}

	got, err := st.GetSession(id)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.ExitReason != string(store.ReasonNormal) {
		t.Fatalf("exit_reason = %q", got.ExitReason)
	}
	if got.ExitCode == nil || *got.ExitCode != 5 {
		t.Fatalf("exit_code = %v", got.ExitCode)
	}
}

// TestFanOutADosClientes: dos pestañas abiertas sobre la misma sesión ven lo
// mismo.
func TestFanOutADosClientes(t *testing.T) {
	m, st := newTestManager(t)
	id := spawnTest(t, m, st, nil)

	a, err := m.Attach(id)
	if err != nil {
		t.Fatalf("Attach a: %v", err)
	}
	defer a.Detach()
	b, err := m.Attach(id)
	if err != nil {
		t.Fatalf("Attach b: %v", err)
	}
	defer b.Detach()

	if err := a.Write([]byte("echo dos-clientes\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	awaitChunk(t, a.Output(), "dos-clientes")
	awaitChunk(t, b.Output(), "dos-clientes")
}

// TestResizeLlegaAlPtyYSePersiste: el shell ve el tamaño nuevo y la DB lo
// recuerda para cuando se reanude la sesión.
func TestResizeLlegaAlPtyYSePersiste(t *testing.T) {
	m, st := newTestManager(t)
	id := spawnTest(t, m, st, nil)

	att, err := m.Attach(id)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer att.Detach()

	if err := att.Resize(45, 123); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if err := att.Write([]byte("stty size\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	awaitChunk(t, att.Output(), "45 123")

	got, err := st.GetSession(id)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.Cols != 123 || got.Rows != 45 {
		t.Fatalf("la DB guardó %dx%d", got.Cols, got.Rows)
	}
}

// TestAttachContraLaMuerteNoDejaElOutputColgado: entre que reap borra la sesión
// del mapa y que cierra a los clientes hay una ventana en la que un Attach
// puede pasar el lookup y llegar al hub tarde. Ese attachment quedaría con un
// Output que nadie va a cerrar nunca, y el consumidor lo lee con `for range`:
// del otro lado del socket es una goroutine filtrada por sesión.
//
// La ventana son unas pocas instrucciones, así que pegarle por timing es una
// lotería —probado: no cae ni en cientos de miles de intentos—. En vez de eso
// se fuerza el estado que la ventana produce: sesión todavía en el mapa, hub ya
// cerrado.
func TestAttachContraLaMuerteNoDejaElOutputColgado(t *testing.T) {
	m, st := newTestManager(t)
	id := spawnTest(t, m, st, nil)

	l := m.lookup(id)
	if l == nil {
		t.Fatal("la sesión recién spawneada tendría que estar en el mapa")
	}
	l.mu.Lock()
	l.hub.closeAll()
	l.mu.Unlock()

	if _, err := m.Attach(id); !errors.Is(err, ptyapi.ErrNotLive) {
		t.Fatalf("Attach en la ventana de la muerte dio %v; quería ErrNotLive", err)
	}
}
