package store

import (
	"errors"
	"testing"
)

func sampleSession(id string) *Session {
	return &Session{
		ID:        id,
		Title:     "pruebas",
		Cwd:       "/tmp",
		Shell:     "/bin/bash",
		Cols:      80,
		Rows:      24,
		PtyStatus: StatusRunning,
	}
}

func TestCreateYGetSession(t *testing.T) {
	st := newTestStore(t)
	in := sampleSession("s1")
	if err := st.CreateSession(in); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if in.CreatedAt == 0 || in.LastActiveAt == 0 {
		t.Fatal("CreateSession tiene que completar los timestamps")
	}

	got, err := st.GetSession("s1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.Title != "pruebas" || got.Cwd != "/tmp" || got.Cols != 80 {
		t.Fatalf("se guardó mal: %+v", got)
	}
	if got.PtyStatus != StatusRunning {
		t.Fatalf("pty_status = %q", got.PtyStatus)
	}
	// Defaults del schema, que M6/M7 van a usar.
	if got.WorkStatus != "idle" || got.KanbanStatus != "todo" {
		t.Fatalf("defaults mal: work=%q kanban=%q", got.WorkStatus, got.KanbanStatus)
	}
}

func TestGetSessionInexistente(t *testing.T) {
	st := newTestStore(t)
	if _, err := st.GetSession("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("se esperaba ErrNotFound, vino %v", err)
	}
}

// TestListSessionsOrdenada: la más nueva primero, que es como la pinta la UI.
func TestListSessionsOrdenada(t *testing.T) {
	st := newTestStore(t)
	for _, id := range []string{"a", "b", "c"} {
		s := sampleSession(id)
		if err := st.CreateSession(s); err != nil {
			t.Fatalf("CreateSession %s: %v", id, err)
		}
	}
	list, err := st.ListSessions()
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("vinieron %d sesiones", len(list))
	}
	if list[0].ID != "c" || list[2].ID != "a" {
		t.Fatalf("orden inesperado: %s, %s, %s", list[0].ID, list[1].ID, list[2].ID)
	}
}

func TestUpdateMetaParcial(t *testing.T) {
	st := newTestStore(t)
	_ = st.CreateSession(sampleSession("s1"))

	titulo := "renombrada"
	if err := st.UpdateMeta("s1", MetaPatch{Title: &titulo}); err != nil {
		t.Fatalf("UpdateMeta: %v", err)
	}
	got, _ := st.GetSession("s1")
	if got.Title != "renombrada" {
		t.Fatalf("title = %q", got.Title)
	}
	if got.Cwd != "/tmp" {
		t.Fatal("UpdateMeta pisó un campo que no le pasaron")
	}

	if err := st.UpdateMeta("nope", MetaPatch{Title: &titulo}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("se esperaba ErrNotFound, vino %v", err)
	}
}

func TestMarkExitedYMarkRunning(t *testing.T) {
	st := newTestStore(t)
	_ = st.CreateSession(sampleSession("s1"))

	code := 3
	if err := st.MarkExited("s1", ReasonNormal, &code); err != nil {
		t.Fatalf("MarkExited: %v", err)
	}
	got, _ := st.GetSession("s1")
	if got.PtyStatus != StatusExited {
		t.Fatalf("pty_status = %q", got.PtyStatus)
	}
	if got.ExitReason != string(ReasonNormal) {
		t.Fatalf("exit_reason = %q", got.ExitReason)
	}
	if got.ExitCode == nil || *got.ExitCode != 3 {
		t.Fatalf("exit_code = %v", got.ExitCode)
	}
	if got.ExitedAt == nil {
		t.Fatal("exited_at quedó en NULL")
	}

	// Reanudar limpia los rastros de la muerte anterior: si no, la UI muestra
	// una sesión corriendo con un exit code al lado.
	if err := st.MarkRunning("s1", 120, 40); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}
	got, _ = st.GetSession("s1")
	if got.PtyStatus != StatusRunning || got.ExitReason != "" || got.ExitCode != nil || got.ExitedAt != nil {
		t.Fatalf("MarkRunning no limpió el estado de salida: %+v", got)
	}
	if got.Cols != 120 || got.Rows != 40 {
		t.Fatalf("MarkRunning no guardó el tamaño: %dx%d", got.Cols, got.Rows)
	}
}

func TestDeleteSession(t *testing.T) {
	st := newTestStore(t)
	_ = st.CreateSession(sampleSession("s1"))

	if err := st.DeleteSession("s1"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, err := st.GetSession("s1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("se esperaba ErrNotFound, vino %v", err)
	}
	if err := st.DeleteSession("s1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("borrar dos veces tiene que dar ErrNotFound, vino %v", err)
	}
}

// TestReconcileBoot cubre el caso central de M2: el pty es hijo del backend,
// así que toda sesión que la DB diga "running" al arrancar es mentira.
func TestReconcileBoot(t *testing.T) {
	st := newTestStore(t)
	_ = st.CreateSession(sampleSession("viva"))
	_ = st.CreateSession(sampleSession("muerta"))
	code := 0
	_ = st.MarkExited("muerta", ReasonNormal, &code)

	n, err := st.ReconcileBoot()
	if err != nil {
		t.Fatalf("ReconcileBoot: %v", err)
	}
	if n != 1 {
		t.Fatalf("se reconciliaron %d sesiones, se esperaba 1", n)
	}

	got, _ := st.GetSession("viva")
	if got.PtyStatus != StatusExited || got.ExitReason != string(ReasonBackendRestart) {
		t.Fatalf("no se reconcilió: %+v", got)
	}
	// La que ya estaba muerta no se toca: conserva su razón real.
	otra, _ := st.GetSession("muerta")
	if otra.ExitReason != string(ReasonNormal) {
		t.Fatalf("se pisó una sesión ya muerta: %+v", otra)
	}
}

func TestRunningIDs(t *testing.T) {
	st := newTestStore(t)
	_ = st.CreateSession(sampleSession("a"))
	_ = st.CreateSession(sampleSession("b"))
	_ = st.MarkExited("b", ReasonKilled, nil)

	ids, err := st.RunningIDs()
	if err != nil {
		t.Fatalf("RunningIDs: %v", err)
	}
	if len(ids) != 1 || ids[0] != "a" {
		t.Fatalf("RunningIDs = %v", ids)
	}
}

// TestNewIDOrdenable: los ids se ordenan por tiempo de creación, así la lista
// no depende de un ORDER BY created_at con empates al milisegundo.
func TestNewIDOrdenable(t *testing.T) {
	seen := map[string]bool{}
	prev := ""
	for i := 0; i < 500; i++ {
		id := NewID()
		if seen[id] {
			t.Fatalf("id repetido: %s", id)
		}
		seen[id] = true
		if id < prev {
			t.Fatalf("id %s salió antes que %s", id, prev)
		}
		prev = id
	}
}

func TestUpdateSize(t *testing.T) {
	st := newTestStore(t)
	_ = st.CreateSession(sampleSession("s1"))

	if err := st.UpdateSize("s1", 140, 50); err != nil {
		t.Fatalf("UpdateSize: %v", err)
	}
	got, _ := st.GetSession("s1")
	if got.Cols != 140 || got.Rows != 50 {
		t.Fatalf("tamaño = %dx%d", got.Cols, got.Rows)
	}
}
