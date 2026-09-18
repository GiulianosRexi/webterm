package control

import (
	"bytes"
	"errors"
	"testing"

	"github.com/giuliano/webterm/internal/ptyapi"
	"github.com/giuliano/webterm/internal/session"
	"github.com/giuliano/webterm/internal/store"
)

// El sweep se alimenta del daemon, no de un mapa en memoria. Una fila que la
// base cree activa y el cliente de ptys no reporta viva, está muerta.
func TestSweepUsaLoQueReportaElClienteDePtys(t *testing.T) {
	m, st := newTestManager(t)

	// Una fila "viva" que nunca se spawneó: exactamente lo que queda después
	// de que el daemon arranque de nuevo.
	huerfana := &store.Session{
		ID: store.NewID(), Cwd: t.TempDir(), Shell: "/bin/sh",
		Cols: 80, Rows: 24, PtyStatus: store.StatusRunning,
	}
	if err := st.CreateSession(huerfana); err != nil {
		t.Fatal(err)
	}
	// Y una de verdad, que el sweep no debe tocar.
	viva, err := m.Create(CreateOpts{Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	if n := m.Sweep(); n != 1 {
		t.Fatalf("el sweep corrigió %d filas; quería 1", n)
	}

	got, err := st.GetSession(huerfana.ID)
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

	sigue, err := st.GetSession(viva.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sigue.PtyStatus != store.StatusRunning {
		t.Fatalf("el sweep se llevó puesta una sesión viva: %s", sigue.PtyStatus)
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
	trabada := &store.Session{
		ID: store.NewID(), Cwd: t.TempDir(), Shell: "/bin/sh",
		Cols: 80, Rows: 24, PtyStatus: store.StatusStarting,
	}
	if err := st.CreateSession(trabada); err != nil {
		t.Fatal(err)
	}

	if n := m.Sweep(); n != 1 {
		t.Fatalf("el sweep corrigió %d filas; quería 1", n)
	}
	got, _ := st.GetSession(trabada.ID)
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
	// entró en ActiveIDs viva y sale de LiveIDs muerta, con su motivo real ya
	// escrito por el reap. Reproducido con un hook y no con timing.
	hook.onLiveIDs = func() {
		hook.onLiveIDs = nil
		if err := m.Kill(rec.ID); err != nil {
			t.Errorf("kill en la ventana del sweep: %v", err)
		}
	}

	m.Sweep()

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
}

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
	esperarOutput(t, att, "MARCA-HISTORIAL")
	att.Detach()

	if err := m.Kill(rec.ID); err != nil {
		t.Fatal(err)
	}

	muerta, err := m.Attach(rec.ID)
	if err != nil {
		t.Fatalf("attach a sesión muerta tendría que funcionar: %v", err)
	}
	defer muerta.Detach()
	if muerta.Live {
		t.Fatal("Live = true en una sesión muerta")
	}
	if muerta.Output() != nil {
		t.Fatal("una sesión muerta no tiene stream vivo")
	}
	if !bytes.Contains(muerta.History, []byte("MARCA-HISTORIAL")) {
		t.Fatalf("el historial no sobrevivió al kill: %q", muerta.History)
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
	esperarOutput(t, att, "antes-de-morir")
	if err := att.Write([]byte("exit\n")); err != nil {
		t.Fatal(err)
	}
	esperarCierre(t, att)
	att.Detach()

	muerta, err := m.Attach(rec.ID)
	if err != nil {
		t.Fatalf("Attach a sesión muerta: %v", err)
	}
	defer muerta.Detach()
	if muerta.Live {
		t.Fatal("Live tendría que ser false")
	}
	if muerta.Output() != nil {
		t.Fatal("una sesión muerta no tiene stream vivo")
	}
	if !bytes.Contains(muerta.History, []byte("antes-de-morir")) {
		t.Fatalf("el historial no sobrevivió: %q", tail(muerta.History, 200))
	}
	// El input a una sesión muerta no revive nada.
	if err := muerta.Write([]byte("echo tarde\n")); err == nil {
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
	list, lerr := st.ListSessions()
	if lerr != nil || len(list) != 1 {
		t.Fatalf("esperaba una fila; list=%v err=%v", list, lerr)
	}
	got := list[0]
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
	esperarOutput(t, att, "antes-del-restart")
	att.Detach()
	if err := m.Kill(rec.ID); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	vuelto, err := m.Restart(rec.ID, 100, 30)
	if err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if vuelto.ID != rec.ID {
		t.Fatalf("Restart cambió el id: %s -> %s", rec.ID, vuelto.ID)
	}
	if vuelto.Title != "con historia" {
		t.Fatalf("se perdió el título: %q", vuelto.Title)
	}
	if vuelto.PtyStatus != store.StatusRunning {
		t.Fatalf("pty_status = %q", vuelto.PtyStatus)
	}
	if vuelto.ExitReason != "" || vuelto.ExitCode != nil {
		t.Fatalf("quedaron rastros de la muerte anterior: %+v", vuelto)
	}
	if vuelto.Cols != 100 || vuelto.Rows != 30 {
		t.Fatalf("el restart no tomó el tamaño nuevo: %dx%d", vuelto.Cols, vuelto.Rows)
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
	esperarOutput(t, att2, "despues-del-restart")
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
	esperarCierre(t, att)

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
	esperarCierre(t, att)

	end := att.End()
	if end.Exited {
		t.Fatal("el cliente se fue pero la sesión sigue viva; End() dice que terminó")
	}
	if end.Session == nil || end.Session.PtyStatus != store.StatusRunning {
		t.Fatalf("la fila tendría que seguir en running: %+v", end.Session)
	}
}
