package store

import (
	"errors"
	"testing"
)

func TestCreateFolderCompletaIDYFecha(t *testing.T) {
	st := newTestStore(t)

	f, err := st.CreateFolder("Iceberg")
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	if f.ID == "" {
		t.Fatal("el folder quedó sin id")
	}
	if f.CreatedAt == 0 {
		t.Fatal("el folder quedó sin created_at")
	}
	if f.Name != "Iceberg" {
		t.Fatalf("Name = %q", f.Name)
	}
}

// El nombre es lo que el usuario ve y lo que Claude usa para elegir folder por
// MCP, así que un nombre vacío no puede existir.
func TestCreateFolderRechazaNombreVacio(t *testing.T) {
	st := newTestStore(t)

	for _, name := range []string{"", "   ", "\t\n"} {
		if _, err := st.CreateFolder(name); !errors.Is(err, ErrNombreVacio) {
			t.Fatalf("CreateFolder(%q) = %v, esperaba ErrNombreVacio", name, err)
		}
	}
}

func TestCreateFolderRecortaEspacios(t *testing.T) {
	st := newTestStore(t)

	f, err := st.CreateFolder("  Iceberg  ")
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	if f.Name != "Iceberg" {
		t.Fatalf("Name = %q, esperaba sin espacios alrededor", f.Name)
	}
}

// Sin esto, crear por MCP y crear por la UI terminan produciendo dos folders
// que en pantalla se ven iguales.
func TestCreateFolderRechazaDuplicadoIgnorandoMayusculas(t *testing.T) {
	st := newTestStore(t)

	if _, err := st.CreateFolder("Iceberg"); err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	for _, name := range []string{"Iceberg", "iceberg", "ICEBERG", " iceberg "} {
		if _, err := st.CreateFolder(name); !errors.Is(err, ErrDuplicate) {
			t.Fatalf("CreateFolder(%q) = %v, esperaba ErrDuplicate", name, err)
		}
	}
}

// Alfabético y no por fecha: el orden tiene que ser estable, que es lo mismo
// que se busca en la sidebar al no ordenarla por actividad.
func TestListFoldersOrdenaAlfabeticamente(t *testing.T) {
	st := newTestStore(t)

	for _, name := range []string{"webterm", "Iceberg", "análisis"} {
		if _, err := st.CreateFolder(name); err != nil {
			t.Fatalf("CreateFolder(%q): %v", name, err)
		}
	}

	got, err := st.ListFolders()
	if err != nil {
		t.Fatalf("ListFolders: %v", err)
	}
	want := []string{"análisis", "Iceberg", "webterm"}
	if len(got) != len(want) {
		t.Fatalf("devolvió %d folders, esperaba %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Name != want[i] {
			t.Fatalf("posición %d = %q, esperaba %q", i, got[i].Name, want[i])
		}
	}
}

func TestRenameFolder(t *testing.T) {
	st := newTestStore(t)

	f, err := st.CreateFolder("Iceberg")
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	if err := st.RenameFolder(f.ID, "  Iceberg v2 "); err != nil {
		t.Fatalf("RenameFolder: %v", err)
	}

	folders, err := st.ListFolders()
	if err != nil {
		t.Fatalf("ListFolders: %v", err)
	}
	if folders[0].Name != "Iceberg v2" {
		t.Fatalf("Name = %q", folders[0].Name)
	}
}

func TestRenameFolderInexistente(t *testing.T) {
	st := newTestStore(t)

	if err := st.RenameFolder("no-existe", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("RenameFolder = %v, esperaba ErrNotFound", err)
	}
}

func TestRenameFolderRechazaDuplicado(t *testing.T) {
	st := newTestStore(t)

	if _, err := st.CreateFolder("Iceberg"); err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	otro, err := st.CreateFolder("webterm")
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	if err := st.RenameFolder(otro.ID, "ICEBERG"); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("RenameFolder = %v, esperaba ErrDuplicate", err)
	}
}

func TestSetSessionFolder(t *testing.T) {
	st := newTestStore(t)
	if err := st.CreateSession(sampleSession("s1")); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	f, err := st.CreateFolder("Iceberg")
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}

	if err := st.SetSessionFolder("s1", &f.ID); err != nil {
		t.Fatalf("SetSessionFolder: %v", err)
	}
	sess, err := st.GetSession("s1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if sess.FolderID == nil || *sess.FolderID != f.ID {
		t.Fatalf("FolderID = %v, esperaba %q", sess.FolderID, f.ID)
	}

	// nil la saca del folder sin borrar nada más.
	if err := st.SetSessionFolder("s1", nil); err != nil {
		t.Fatalf("SetSessionFolder(nil): %v", err)
	}
	sess, err = st.GetSession("s1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if sess.FolderID != nil {
		t.Fatalf("FolderID = %v, esperaba nil", *sess.FolderID)
	}
}

func TestSetSessionFolderRechazaFolderInexistente(t *testing.T) {
	st := newTestStore(t)
	if err := st.CreateSession(sampleSession("s1")); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	fantasma := "no-existe"
	if err := st.SetSessionFolder("s1", &fantasma); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetSessionFolder = %v, esperaba ErrNotFound", err)
	}
}

// Borrar un folder no borra sesiones: las saca del folder. Es la única razón
// por la que la falta de foreign key no se nota, así que tiene test propio.
func TestDeleteFolderSacaLasSesionesSinBorrarlas(t *testing.T) {
	st := newTestStore(t)
	for _, id := range []string{"s1", "s2"} {
		if err := st.CreateSession(sampleSession(id)); err != nil {
			t.Fatalf("CreateSession(%s): %v", id, err)
		}
	}
	f, err := st.CreateFolder("Iceberg")
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	for _, id := range []string{"s1", "s2"} {
		if err := st.SetSessionFolder(id, &f.ID); err != nil {
			t.Fatalf("SetSessionFolder(%s): %v", id, err)
		}
	}

	if err := st.DeleteFolder(f.ID); err != nil {
		t.Fatalf("DeleteFolder: %v", err)
	}

	folders, err := st.ListFolders()
	if err != nil {
		t.Fatalf("ListFolders: %v", err)
	}
	if len(folders) != 0 {
		t.Fatalf("quedaron %d folders", len(folders))
	}

	for _, id := range []string{"s1", "s2"} {
		sess, err := st.GetSession(id)
		if err != nil {
			t.Fatalf("la sesión %s se borró con el folder: %v", id, err)
		}
		if sess.FolderID != nil {
			t.Fatalf("la sesión %s quedó apuntando a un folder borrado", id)
		}
	}
}

func TestDeleteFolderInexistente(t *testing.T) {
	st := newTestStore(t)

	if err := st.DeleteFolder("no-existe"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteFolder = %v, esperaba ErrNotFound", err)
	}
}
