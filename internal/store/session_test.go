package store

import (
	"errors"
	"reflect"
	"sort"
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

func TestActiveSessionsIncluyeStartingYRunning(t *testing.T) {
	st := newTestStore(t)

	corriendo := &Session{ID: "a", Cwd: "/tmp", Shell: "/bin/sh", Cols: 80, Rows: 24, PtyStatus: StatusRunning}
	arrancando := &Session{ID: "b", Cwd: "/tmp", Shell: "/bin/sh", Cols: 80, Rows: 24, PtyStatus: StatusStarting}
	muerta := &Session{ID: "c", Cwd: "/tmp", Shell: "/bin/sh", Cols: 80, Rows: 24, PtyStatus: StatusExited}
	for _, s := range []*Session{corriendo, arrancando, muerta} {
		if err := st.CreateSession(s); err != nil {
			t.Fatal(err)
		}
	}

	active, err := st.ActiveSessions()
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, a := range active {
		ids = append(ids, a.ID)
		// El estado y la marca de actividad son la entrada del CAS del sweep:
		// si vinieran en cero, MarkExitedIfUnchanged no matchearía nunca y el
		// sweep dejaría de barrer sin que nada fallara ruidosamente.
		if a.PtyStatus == "" || a.LastActiveAt == 0 {
			t.Fatalf("la fila %s vino sin estado ni last_active_at: %+v", a.ID, a)
		}
	}
	sort.Strings(ids)
	if !reflect.DeepEqual(ids, []string{"a", "b"}) {
		t.Fatalf("ActiveSessions = %v; quería [a b]", ids)
	}
}

// Una fila que quedó en starting porque el orquestador crasheó entre el
// insert y el spawn tiene que poder marcarse muerta como cualquier otra.
func TestMarkExitedSobreStarting(t *testing.T) {
	st := newTestStore(t)
	sess := &Session{ID: "x", Cwd: "/tmp", Shell: "/bin/sh", Cols: 80, Rows: 24, PtyStatus: StatusStarting}
	if err := st.CreateSession(sess); err != nil {
		t.Fatal(err)
	}

	if err := st.MarkExited("x", ReasonOrphaned, nil); err != nil {
		t.Fatal(err)
	}

	got, err := st.GetSession("x")
	if err != nil {
		t.Fatal(err)
	}
	if got.PtyStatus != StatusExited || got.ExitReason != string(ReasonOrphaned) {
		t.Fatalf("quedó %s/%s; quería exited/orphaned", got.PtyStatus, got.ExitReason)
	}
}

func TestMarkStarting(t *testing.T) {
	st := newTestStore(t)
	sess := &Session{ID: "x", Cwd: "/tmp", Shell: "/bin/sh", Cols: 80, Rows: 24, PtyStatus: StatusExited}
	sess.ExitReason = string(ReasonNormal)
	if err := st.CreateSession(sess); err != nil {
		t.Fatal(err)
	}

	if err := st.MarkStarting("x"); err != nil {
		t.Fatal(err)
	}

	got, err := st.GetSession("x")
	if err != nil {
		t.Fatal(err)
	}
	if got.PtyStatus != StatusStarting {
		t.Fatalf("pty_status = %s; quería starting", got.PtyStatus)
	}
	// Los rastros de la salida anterior se borran: si no, la UI muestra un
	// exit code al lado de una sesión que está arrancando.
	if got.ExitReason != "" || got.ExitCode != nil || got.ExitedAt != nil {
		t.Fatalf("quedaron rastros de la salida anterior: %+v", got)
	}
	if err := st.MarkStarting("no-existe"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("MarkStarting sobre una sesión inexistente dio %v; quería ErrNotFound", err)
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

// El CAS de MarkExitedIfUnchanged es lo que le permite al sweep escribir sin
// riesgo: una fila que ya murió conserva su motivo real.
func TestMarkExitedIfUnchanged(t *testing.T) {
	st := newTestStore(t)
	if err := st.CreateSession(sampleSession("s1")); err != nil {
		t.Fatal(err)
	}
	prev := soloActiva(t, st, "s1")

	// La fila sigue como la leímos: le toca a este llamador marcarla.
	marcada, err := st.MarkExitedIfUnchanged(prev, ReasonOrphaned, nil)
	if err != nil {
		t.Fatalf("MarkExitedIfUnchanged: %v", err)
	}
	if !marcada {
		t.Fatal("la fila estaba como la leímos y no la marcó")
	}
	got, _ := st.GetSession("s1")
	if got.PtyStatus != StatusExited || got.ExitReason != string(ReasonOrphaned) {
		t.Fatalf("quedó %s/%s; quería exited/orphaned", got.PtyStatus, got.ExitReason)
	}

	// Ya muerta: no se toca, y el motivo real sobrevive.
	marcada, err = st.MarkExitedIfUnchanged(prev, ReasonDaemonRestart, nil)
	if err != nil {
		t.Fatalf("MarkExitedIfUnchanged sobre una fila muerta: %v", err)
	}
	if marcada {
		t.Fatal("pisó una fila que ya estaba exited")
	}
	got, _ = st.GetSession("s1")
	if got.ExitReason != string(ReasonOrphaned) {
		t.Fatalf("exit_reason = %q; se perdió el motivo original", got.ExitReason)
	}

	// Una fila que no existe no es un error: es el mismo "no me tocó a mí".
	marcada, err = st.MarkExitedIfUnchanged(
		ActiveSession{ID: "no-existe", PtyStatus: StatusRunning, LastActiveAt: 1}, ReasonOrphaned, nil)
	if err != nil || marcada {
		t.Fatalf("fila inexistente dio marcada=%v err=%v; quería false y nil", marcada, err)
	}
}

// Y la contracara: sobre una fila en starting sí escribe, porque starting es un
// estado activo. Es lo que levanta las filas que quedaron trabadas entre el
// insert y el spawn.
func TestMarkExitedIfUnchangedSobreStarting(t *testing.T) {
	st := newTestStore(t)
	sess := &Session{ID: "x", Cwd: "/tmp", Shell: "/bin/sh", Cols: 80, Rows: 24, PtyStatus: StatusStarting}
	if err := st.CreateSession(sess); err != nil {
		t.Fatal(err)
	}
	marcada, err := st.MarkExitedIfUnchanged(soloActiva(t, st, "x"), ReasonDaemonRestart, nil)
	if err != nil || !marcada {
		t.Fatalf("starting tendría que marcarse: marcada=%v err=%v", marcada, err)
	}
}

// Una fila que pasó de starting a running entre la lectura y el UPDATE es una
// sesión que terminó de nacer: es el I1 del review final, visto desde el store.
func TestMarkExitedIfUnchangedNoTocaUnaFilaQueCambio(t *testing.T) {
	st := newTestStore(t)
	sess := &Session{ID: "x", Cwd: "/tmp", Shell: "/bin/sh", Cols: 80, Rows: 24, PtyStatus: StatusStarting}
	if err := st.CreateSession(sess); err != nil {
		t.Fatal(err)
	}
	prev := soloActiva(t, st, "x")

	// El spawn termina: la fila pasa a running con un pty vivo detrás.
	if err := st.MarkRunning("x", 80, 24); err != nil {
		t.Fatal(err)
	}

	marcada, err := st.MarkExitedIfUnchanged(prev, ReasonDaemonRestart, nil)
	if err != nil {
		t.Fatal(err)
	}
	if marcada {
		t.Fatal("marcó muerta una fila que cambió después de leerla")
	}
	got, _ := st.GetSession("x")
	if got.PtyStatus != StatusRunning {
		t.Fatalf("quedó %s; quería running", got.PtyStatus)
	}
}

// soloActiva devuelve la fila activa con ese id tal como la ve el sweep.
func soloActiva(t *testing.T, st *Store, id string) ActiveSession {
	t.Helper()
	active, err := st.ActiveSessions()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range active {
		if a.ID == id {
			return a
		}
	}
	t.Fatalf("la fila %s no está entre las activas: %+v", id, active)
	return ActiveSession{}
}
