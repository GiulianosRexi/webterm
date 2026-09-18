package session

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/giuliano/webterm/internal/store"
)

// TestKillConservaElHistorial: matar no es borrar. La distinción es explícita
// justamente para que el historial no se vaya sin que nadie lo pida.
func TestKillConservaElHistorial(t *testing.T) {
	m, st := newTestManager(t)
	rec, _ := m.Create(CreateOpts{Cwd: "/tmp", Cols: 80, Rows: 24})

	att, _ := m.Attach(rec.ID)
	_ = m.Write(rec.ID, []byte("echo sobrevive-al-kill\n"))
	awaitChunk(t, att.Output, "sobrevive-al-kill")
	att.Detach()

	if err := m.Kill(rec.ID); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	// Kill es sincrónico: al volver, la DB ya tiene que estar reconciliada.
	got, err := st.GetSession(rec.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.PtyStatus != store.StatusExited {
		t.Fatalf("pty_status = %q", got.PtyStatus)
	}
	if got.ExitReason != string(store.ReasonKilled) {
		t.Fatalf("exit_reason = %q, se esperaba killed", got.ExitReason)
	}

	hist, _ := st.ReadOutput(rec.ID)
	if !bytes.Contains(hist, []byte("sobrevive-al-kill")) {
		t.Fatalf("el kill se llevó el historial: %q", tail(hist, 200))
	}
}

func TestKillEsIdempotente(t *testing.T) {
	m, _ := newTestManager(t)
	rec, _ := m.Create(CreateOpts{Cwd: "/tmp", Cols: 80, Rows: 24})

	if err := m.Kill(rec.ID); err != nil {
		t.Fatalf("Kill 1: %v", err)
	}
	if err := m.Kill(rec.ID); err != nil {
		t.Fatalf("Kill 2: %v", err)
	}
	if err := m.Kill("no-existe"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Kill de inexistente: %v", err)
	}
}

// TestRestartReusaLaFila: reanudar conserva id, título, KV e historial. Es lo
// que M6 va a necesitar para colgarle el `claude --resume`.
func TestRestartReusaLaFila(t *testing.T) {
	m, st := newTestManager(t)
	rec, _ := m.Create(CreateOpts{Title: "con historia", Cwd: "/tmp", Cols: 80, Rows: 24})
	_ = st.SetKV(rec.ID, "claude_session_id", "abc-123")

	att, _ := m.Attach(rec.ID)
	_ = m.Write(rec.ID, []byte("echo antes-del-restart\n"))
	awaitChunk(t, att.Output, "antes-del-restart")
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
	_ = m.Write(rec.ID, []byte("echo despues-del-restart\n"))
	awaitChunk(t, att2.Output, "despues-del-restart")
}

func TestRestartSobreSesionViva(t *testing.T) {
	m, _ := newTestManager(t)
	rec, _ := m.Create(CreateOpts{Cwd: "/tmp", Cols: 80, Rows: 24})

	if _, err := m.Restart(rec.ID, 80, 24); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("se esperaba ErrAlreadyRunning, vino %v", err)
	}
}

func TestDeleteBorraTodo(t *testing.T) {
	m, st := newTestManager(t)
	rec, _ := m.Create(CreateOpts{Cwd: "/tmp", Cols: 80, Rows: 24})
	_ = st.SetKV(rec.ID, "k", "v")

	att, _ := m.Attach(rec.ID)
	_ = m.Write(rec.ID, []byte("echo hola\n"))
	awaitChunk(t, att.Output, "hola")
	att.Detach()

	if err := m.Delete(rec.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := st.GetSession(rec.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("la fila sigue ahí: %v", err)
	}
	hist, _ := st.ReadOutput(rec.ID)
	if len(hist) != 0 {
		t.Fatalf("quedó historial huérfano: %d bytes", len(hist))
	}
	kv, _ := st.ListKV(rec.ID)
	if len(kv) != 0 {
		t.Fatalf("quedó KV huérfano: %v", kv)
	}
}

// TestSweepMarcaHuerfanas cubre el caso de desincronización: la DB dice que la
// sesión está viva pero no hay proceso detrás.
func TestSweepMarcaHuerfanas(t *testing.T) {
	m, st := newTestManager(t)

	// Fila viva escrita a mano, sin pty: simula la desincronización.
	err := st.CreateSession(&store.Session{
		ID: "fantasma", Cwd: "/tmp", Shell: "/bin/bash", Cols: 80, Rows: 24,
		PtyStatus: store.StatusRunning,
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// Y una de verdad, que el sweep no tiene que tocar.
	viva, _ := m.Create(CreateOpts{Cwd: "/tmp", Cols: 80, Rows: 24})

	if n := m.Sweep(); n != 1 {
		t.Fatalf("el sweep corrigió %d filas, se esperaba 1", n)
	}

	got, _ := st.GetSession("fantasma")
	if got.PtyStatus != store.StatusExited || got.ExitReason != string(store.ReasonOrphaned) {
		t.Fatalf("la huérfana no se reconcilió: %+v", got)
	}
	sigue, _ := st.GetSession(viva.ID)
	if sigue.PtyStatus != store.StatusRunning {
		t.Fatalf("el sweep mató una sesión viva: %+v", sigue)
	}
	// Idempotente: en la segunda pasada ya no hay nada que corregir.
	if n := m.Sweep(); n != 0 {
		t.Fatalf("el segundo sweep corrigió %d filas", n)
	}
}

// TestCloseMataTodo: al apagar el backend no quedan procesos sueltos ni filas
// mintiendo.
func TestCloseMataTodo(t *testing.T) {
	st := newTestStore(t)
	m := NewManager(st, Config{Shell: "/bin/bash", HistoryBytes: 1 << 20, SweepEvery: time.Hour})
	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	rec, _ := m.Create(CreateOpts{Cwd: "/tmp", Cols: 80, Rows: 24})

	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	got, _ := st.GetSession(rec.ID)
	if got.PtyStatus != store.StatusExited {
		t.Fatalf("pty_status = %q después de Close", got.PtyStatus)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close dos veces: %v", err)
	}
}

// TestExtraEnvLlegaAlPty: el cliente MCP que corre adentro de la sesión saca el
// token de su propio entorno, así que tiene que estar ahí.
func TestExtraEnvLlegaAlPty(t *testing.T) {
	st := newTestStore(t)
	m := NewManager(st, Config{
		Shell: "/bin/bash", HistoryBytes: 1 << 20, SweepEvery: time.Hour,
		ExtraEnv: []string{"WEBTERM_TOKEN=un-token-de-prueba"},
	})
	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })

	rec, err := m.Create(CreateOpts{Cwd: "/tmp", Cols: 80, Rows: 24})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	att, _ := m.Attach(rec.ID)
	defer att.Detach()

	// El id de sesión ya viajaba desde M1; el token es lo que suma M9.
	_ = m.Write(rec.ID, []byte("echo T=$WEBTERM_TOKEN S=$WEBTERM_SESSION_ID\n"))
	out := awaitChunk(t, att.Output, "T=un-token-de-prueba")
	if !strings.Contains(out, "S="+rec.ID) {
		t.Fatalf("falta el id de sesión en el entorno: %q", tail([]byte(out), 200))
	}
}

// TestSinExtraEnvNoHayToken: sin token configurado no se filtra una variable
// vacía al entorno.
func TestSinExtraEnvNoHayToken(t *testing.T) {
	m, _ := newTestManager(t)
	rec, _ := m.Create(CreateOpts{Cwd: "/tmp", Cols: 80, Rows: 24})
	att, _ := m.Attach(rec.ID)
	defer att.Detach()

	_ = m.Write(rec.ID, []byte("echo TOKEN=[${WEBTERM_TOKEN:-vacio}]\n"))
	awaitChunk(t, att.Output, "TOKEN=[vacio]")
}
