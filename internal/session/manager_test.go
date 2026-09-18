package session

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/giuliano/webterm/internal/store"
)

func newTestManager(t *testing.T) (*Manager, *store.Store) {
	t.Helper()
	st := newTestStore(t)
	m := NewManager(st, Config{Shell: "/bin/bash", HistoryBytes: 1 << 20, SweepEvery: time.Hour})
	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m, st
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

func TestCreatePersisteYCorre(t *testing.T) {
	m, st := newTestManager(t)

	rec, err := m.Create(CreateOpts{Title: "una sesión", Cwd: "/tmp", Cols: 80, Rows: 24})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if rec.ID == "" {
		t.Fatal("Create no asignó id")
	}

	got, err := st.GetSession(rec.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.PtyStatus != store.StatusRunning {
		t.Fatalf("pty_status = %q", got.PtyStatus)
	}
	if got.Title != "una sesión" || got.Cwd != "/tmp" {
		t.Fatalf("metadata mal guardada: %+v", got)
	}
}

// TestSobreviveAlDetach es la premisa entera de M2: cerrar el cliente no mata
// el proceso, y al volver se ve lo que pasó mientras tanto.
func TestSobreviveAlDetach(t *testing.T) {
	m, _ := newTestManager(t)
	rec, err := m.Create(CreateOpts{Cwd: "/tmp", Cols: 80, Rows: 24})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	att, err := m.Attach(rec.ID)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if !att.Live {
		t.Fatal("la sesión recién creada tendría que estar viva")
	}
	if err := m.Write(rec.ID, []byte("echo marca-uno\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	awaitChunk(t, att.Output, "marca-uno")
	att.Detach()

	// Con el cliente desconectado, el proceso sigue trabajando.
	if err := m.Write(rec.ID, []byte("echo marca-dos\n")); err != nil {
		t.Fatalf("Write con el cliente desconectado: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	att2, err := m.Attach(rec.ID)
	if err != nil {
		t.Fatalf("re-Attach: %v", err)
	}
	defer att2.Detach()
	if !att2.Live {
		t.Fatal("la sesión murió al desattachear")
	}
	if !bytes.Contains(att2.History, []byte("marca-dos")) {
		t.Fatalf("el replay no trae lo que pasó estando desconectado: %q", tail(att2.History, 200))
	}
	if !bytes.Contains(att2.History, []byte("marca-uno")) {
		t.Fatalf("el replay perdió lo de antes del detach: %q", tail(att2.History, 200))
	}
}

// TestExitDelShellSeReconcilia: el estado en la DB sigue al proceso real.
func TestExitDelShellSeReconcilia(t *testing.T) {
	m, st := newTestManager(t)
	rec, _ := m.Create(CreateOpts{Cwd: "/tmp", Cols: 80, Rows: 24})

	att, err := m.Attach(rec.ID)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if err := m.Write(rec.ID, []byte("exit 5\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// El canal se cierra cuando la sesión muere.
	deadline := time.After(15 * time.Second)
	for open := true; open; {
		select {
		case _, ok := <-att.Output:
			open = ok
		case <-deadline:
			t.Fatal("timeout esperando el cierre del canal")
		}
	}

	waitDead(t, m, rec.ID)
	got, err := st.GetSession(rec.ID)
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

// TestAttachASesionMuerta: se puede ver el historial de una sesión terminada
// sin una vista aparte, en modo lectura.
func TestAttachASesionMuerta(t *testing.T) {
	m, _ := newTestManager(t)
	rec, _ := m.Create(CreateOpts{Cwd: "/tmp", Cols: 80, Rows: 24})

	att, _ := m.Attach(rec.ID)
	_ = m.Write(rec.ID, []byte("echo antes-de-morir\n"))
	awaitChunk(t, att.Output, "antes-de-morir")
	_ = m.Write(rec.ID, []byte("exit\n"))
	att.Detach()

	waitDead(t, m, rec.ID)

	muerta, err := m.Attach(rec.ID)
	if err != nil {
		t.Fatalf("Attach a sesión muerta: %v", err)
	}
	defer muerta.Detach()
	if muerta.Live {
		t.Fatal("Live tendría que ser false")
	}
	if muerta.Output != nil {
		t.Fatal("una sesión muerta no tiene stream vivo")
	}
	if !bytes.Contains(muerta.History, []byte("antes-de-morir")) {
		t.Fatalf("el historial no sobrevivió: %q", tail(muerta.History, 200))
	}
	// El input a una sesión muerta no revive nada.
	if err := m.Write(rec.ID, []byte("echo tarde\n")); err == nil {
		t.Fatal("escribir a una sesión muerta tendría que fallar")
	}
}

func TestAttachInexistente(t *testing.T) {
	m, _ := newTestManager(t)
	if _, err := m.Attach("no-existe"); err == nil {
		t.Fatal("se esperaba un error")
	}
}

// TestFanOutADosClientes: dos pestañas abiertas sobre la misma sesión ven lo
// mismo.
func TestFanOutADosClientes(t *testing.T) {
	m, _ := newTestManager(t)
	rec, _ := m.Create(CreateOpts{Cwd: "/tmp", Cols: 80, Rows: 24})

	a, _ := m.Attach(rec.ID)
	defer a.Detach()
	b, _ := m.Attach(rec.ID)
	defer b.Detach()

	if err := m.Write(rec.ID, []byte("echo dos-clientes\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	awaitChunk(t, a.Output, "dos-clientes")
	awaitChunk(t, b.Output, "dos-clientes")
}

// TestResizeLlegaAlPtyYSePersiste: el shell ve el tamaño nuevo y la DB lo
// recuerda para cuando se reanude la sesión.
func TestResizeLlegaAlPtyYSePersiste(t *testing.T) {
	m, st := newTestManager(t)
	rec, _ := m.Create(CreateOpts{Cwd: "/tmp", Cols: 80, Rows: 24})

	att, _ := m.Attach(rec.ID)
	defer att.Detach()

	if err := m.Resize(rec.ID, 45, 123); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	_ = m.Write(rec.ID, []byte("stty size\n"))
	awaitChunk(t, att.Output, "45 123")

	got, _ := st.GetSession(rec.ID)
	if got.Cols != 123 || got.Rows != 45 {
		t.Fatalf("la DB guardó %dx%d", got.Cols, got.Rows)
	}
}

func TestUpdateMeta(t *testing.T) {
	m, _ := newTestManager(t)
	rec, _ := m.Create(CreateOpts{Title: "vieja", Cwd: "/tmp", Cols: 80, Rows: 24})

	nuevo := "nueva"
	got, err := m.UpdateMeta(rec.ID, store.MetaPatch{Title: &nuevo})
	if err != nil {
		t.Fatalf("UpdateMeta: %v", err)
	}
	if got.Title != "nueva" {
		t.Fatalf("title = %q", got.Title)
	}
}

// waitDead espera a que la sesión quede marcada como muerta en la DB.
func waitDead(t *testing.T, m *Manager, id string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		got, err := m.Get(id)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.PtyStatus == store.StatusExited {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("la sesión %s nunca quedó marcada como muerta", id)
}

func tail(p []byte, n int) string {
	if len(p) > n {
		p = p[len(p)-n:]
	}
	return string(p)
}
