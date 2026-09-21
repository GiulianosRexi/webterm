package control

import (
	"testing"

	"github.com/giuliano/webterm/internal/events"
)

func TestCreateFolderPublica(t *testing.T) {
	m, bus := newTestManagerWithBus(t)
	ch, stop := bus.Subscribe()
	defer stop()

	f, err := m.CreateFolder("Iceberg")
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	if f.Name != "Iceberg" {
		t.Fatalf("Name = %q", f.Name)
	}
	waitFor(t, ch, events.FolderCreated)
}

func TestRenameYDeleteFolderPublican(t *testing.T) {
	m, bus := newTestManagerWithBus(t)
	ch, stop := bus.Subscribe()
	defer stop()

	f, err := m.CreateFolder("Iceberg")
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	if err := m.RenameFolder(f.ID, "Iceberg v2"); err != nil {
		t.Fatalf("RenameFolder: %v", err)
	}
	waitFor(t, ch, events.FolderUpdated)

	if err := m.DeleteFolder(f.ID); err != nil {
		t.Fatalf("DeleteFolder: %v", err)
	}
	waitFor(t, ch, events.FolderDeleted)
}

// Mover una sesión cambia la sesión, no el folder: el evento que le importa a
// la UI es el de la sesión, que es la que se dibuja en otro lado de la lista.
func TestSetSessionFolderPublicaSessionUpdated(t *testing.T) {
	m, bus := newTestManagerWithBus(t)
	ch, stop := bus.Subscribe()
	defer stop()

	rec, err := m.Create(CreateOpts{Title: "t", Cwd: t.TempDir(), Cols: 80, Rows: 24})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	f, err := m.CreateFolder("Iceberg")
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	if err := m.SetSessionFolder(rec.ID, &f.ID); err != nil {
		t.Fatalf("SetSessionFolder: %v", err)
	}

	ev := waitFor(t, ch, events.SessionUpdated)
	if ev.SessionID != rec.ID {
		t.Fatalf("SessionID = %q, esperaba %q", ev.SessionID, rec.ID)
	}
}

func TestFoldersSinBusNoRompe(t *testing.T) {
	m, _ := newTestManager(t)
	f, err := m.CreateFolder("Iceberg")
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	if err := m.DeleteFolder(f.ID); err != nil {
		t.Fatalf("DeleteFolder: %v", err)
	}
}
