package control

import (
	"testing"
	"time"

	"github.com/giuliano/webterm/internal/events"
	"github.com/giuliano/webterm/internal/session"
	"github.com/giuliano/webterm/internal/store"
)

// newTestManagerWithBus es newTestManager (manager_test.go) con un bus
// enchufado, que es lo único que cambia acá.
func newTestManagerWithBus(t *testing.T) (*Manager, *events.Bus) {
	t.Helper()
	st := newTestStore(t)
	pty := session.NewManager(st, session.Config{HistoryBytes: 64 << 10})
	bus := events.New(16)
	m := NewManager(st, pty, Config{Shell: "/bin/sh", Events: bus})
	t.Cleanup(func() {
		_ = m.Close()
		_ = pty.Close()
		_ = st.Close()
	})
	return m, bus
}

// waitFor consume eventos hasta encontrar el que se busca. Filtra en vez de
// mirar solo el primero porque crear una sesión ya publica de por sí.
func waitFor(t *testing.T, ch <-chan events.Event, kind events.Kind) events.Event {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-ch:
			if ev.Kind == kind {
				return ev
			}
		case <-deadline:
			t.Fatalf("nunca llegó un evento %s", kind)
			return events.Event{}
		}
	}
}

func TestAddResourcePublica(t *testing.T) {
	m, bus := newTestManagerWithBus(t)
	ch, stop := bus.Subscribe()
	defer stop()

	rec, err := m.Create(CreateOpts{Title: "t", Cwd: t.TempDir(), Cols: 80, Rows: 24})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// system y type explícitos: así no hace falta un Resources configurado
	// para resolver la URL, que no es lo que este test mira.
	if _, err := m.AddResource(rec.ID, "https://github.com/o/r/pull/1", "github", "pr"); err != nil {
		t.Fatalf("AddResource: %v", err)
	}

	ev := waitFor(t, ch, events.ResourceAdded)
	if ev.SessionID != rec.ID {
		t.Fatalf("SessionID = %q, esperaba %q", ev.SessionID, rec.ID)
	}
}

func TestCreateYDeletePublican(t *testing.T) {
	m, bus := newTestManagerWithBus(t)
	ch, stop := bus.Subscribe()
	defer stop()

	rec, err := m.Create(CreateOpts{Title: "t", Cwd: t.TempDir(), Cols: 80, Rows: 24})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if ev := waitFor(t, ch, events.SessionCreated); ev.SessionID != rec.ID {
		t.Fatalf("SessionID = %q, esperaba %q", ev.SessionID, rec.ID)
	}

	if err := m.Delete(rec.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if ev := waitFor(t, ch, events.SessionDeleted); ev.SessionID != rec.ID {
		t.Fatalf("SessionID = %q, esperaba %q", ev.SessionID, rec.ID)
	}
}

func TestUpdateMetaPublica(t *testing.T) {
	m, bus := newTestManagerWithBus(t)
	ch, stop := bus.Subscribe()
	defer stop()

	rec, err := m.Create(CreateOpts{Title: "t", Cwd: t.TempDir(), Cols: 80, Rows: 24})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	titulo := "otro nombre"
	if _, err := m.UpdateMeta(rec.ID, store.MetaPatch{Title: &titulo}); err != nil {
		t.Fatalf("UpdateMeta: %v", err)
	}
	waitFor(t, ch, events.SessionUpdated)
}

// El caso de Kill tiene dos ramas y solo una escribe: matar una sesión viva
// mueve la fila (y por lo tanto publica), pero matar una que ya está muerta es
// idempotente y no toca el store, así que no tiene que publicar de nuevo. Es
// la rama que el propio Kill señala en su comentario y que no tenía cobertura.
func TestKillPublicaUnaVezYNoDeNuevoSiYaEstabaMuerta(t *testing.T) {
	m, bus := newTestManagerWithBus(t)
	ch, stop := bus.Subscribe()
	defer stop()

	rec, err := m.Create(CreateOpts{Title: "t", Cwd: t.TempDir(), Cols: 80, Rows: 24})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	waitFor(t, ch, events.SessionCreated)

	if err := m.Kill(rec.ID); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if ev := waitFor(t, ch, events.SessionUpdated); ev.SessionID != rec.ID {
		t.Fatalf("SessionID = %q, esperaba %q", ev.SessionID, rec.ID)
	}

	// La sesión ya está muerta acá: este segundo Kill es el camino
	// ErrNotLive, no escribe nada, y por lo tanto no debería publicar.
	if err := m.Kill(rec.ID); err != nil {
		t.Fatalf("segundo Kill (idempotente): %v", err)
	}
	select {
	case ev := <-ch:
		t.Fatalf("evento inesperado tras el Kill idempotente: %+v", ev)
	case <-time.After(500 * time.Millisecond):
	}
}

func TestDeleteResourcePublica(t *testing.T) {
	m, bus := newTestManagerWithBus(t)
	ch, stop := bus.Subscribe()
	defer stop()

	rec, err := m.Create(CreateOpts{Title: "t", Cwd: t.TempDir(), Cols: 80, Rows: 24})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	res, err := m.AddResource(rec.ID, "https://github.com/o/r/pull/1", "github", "pr")
	if err != nil {
		t.Fatalf("AddResource: %v", err)
	}
	waitFor(t, ch, events.ResourceAdded)

	if err := m.DeleteResource(rec.ID, res.ID); err != nil {
		t.Fatalf("DeleteResource: %v", err)
	}
	if ev := waitFor(t, ch, events.ResourceRemoved); ev.SessionID != rec.ID {
		t.Fatalf("SessionID = %q, esperaba %q", ev.SessionID, rec.ID)
	}
}

// El bus es opcional: sin él el orquestador tiene que andar igual, que es como
// lo construyen todos los tests que ya existen.
func TestSinBusNoRompe(t *testing.T) {
	m, _ := newTestManager(t)
	rec, err := m.Create(CreateOpts{Title: "t", Cwd: t.TempDir(), Cols: 80, Rows: 24})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := m.AddResource(rec.ID, "https://github.com/o/r/pull/1", "github", "pr"); err != nil {
		t.Fatalf("AddResource: %v", err)
	}
}
