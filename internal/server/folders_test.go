package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/giuliano/webterm/internal/store"
)

func decodeFolder(t *testing.T, body []byte) *store.Folder {
	t.Helper()
	var f store.Folder
	if err := json.Unmarshal(body, &f); err != nil {
		t.Fatalf("decodificando folder (%s): %v", body, err)
	}
	return &f
}

func TestFoldersABM(t *testing.T) {
	srv, _ := newTestServer(t)

	code, body := do(t, srv, http.MethodPost, "/api/folders", `{"name":"Iceberg"}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /api/folders = %d (%s)", code, body)
	}
	f := decodeFolder(t, body)
	if f.ID == "" || f.Name != "Iceberg" {
		t.Fatalf("folder devuelto: %+v", f)
	}

	code, body = do(t, srv, http.MethodGet, "/api/folders", "")
	if code != http.StatusOK {
		t.Fatalf("GET /api/folders = %d (%s)", code, body)
	}
	var list []*store.Folder
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("decodificando lista: %v", err)
	}
	if len(list) != 1 || list[0].Name != "Iceberg" {
		t.Fatalf("lista = %s", body)
	}

	code, body = do(t, srv, http.MethodPatch, "/api/folders/"+f.ID, `{"name":"Iceberg v2"}`)
	if code != http.StatusNoContent {
		t.Fatalf("PATCH = %d (%s)", code, body)
	}

	code, body = do(t, srv, http.MethodDelete, "/api/folders/"+f.ID, "")
	if code != http.StatusNoContent {
		t.Fatalf("DELETE = %d (%s)", code, body)
	}

	_, body = do(t, srv, http.MethodGet, "/api/folders", "")
	if string(body) != "[]\n" && string(body) != "[]" {
		t.Fatalf("después de borrar la lista quedó: %s", body)
	}
}

func TestCrearFolderDuplicadoDa409(t *testing.T) {
	srv, _ := newTestServer(t)

	if code, body := do(t, srv, http.MethodPost, "/api/folders", `{"name":"Iceberg"}`); code != http.StatusCreated {
		t.Fatalf("primer POST = %d (%s)", code, body)
	}
	// Distinta capitalización: tiene que chocar igual, si no la UI y el MCP
	// terminan creando dos folders que se ven iguales.
	code, body := do(t, srv, http.MethodPost, "/api/folders", `{"name":"iceberg"}`)
	if code != http.StatusConflict {
		t.Fatalf("segundo POST = %d (%s), esperaba 409", code, body)
	}
}

func TestCrearFolderSinNombreDa400(t *testing.T) {
	srv, _ := newTestServer(t)

	code, body := do(t, srv, http.MethodPost, "/api/folders", `{"name":"   "}`)
	if code != http.StatusBadRequest {
		t.Fatalf("POST = %d (%s), esperaba 400", code, body)
	}
}

func TestMoverSesionAUnFolderYSacarla(t *testing.T) {
	srv, _ := newTestServer(t)

	_, body := do(t, srv, http.MethodPost, "/api/sessions", `{"cols":80,"rows":24}`)
	sess := decodeSession(t, body)
	_, body = do(t, srv, http.MethodPost, "/api/folders", `{"name":"Iceberg"}`)
	f := decodeFolder(t, body)

	code, body := do(t, srv, http.MethodPut, "/api/sessions/"+sess.ID+"/folder",
		`{"folder_id":"`+f.ID+`"}`)
	if code != http.StatusNoContent {
		t.Fatalf("PUT folder = %d (%s)", code, body)
	}
	_, body = do(t, srv, http.MethodGet, "/api/sessions/"+sess.ID, "")
	if got := decodeSession(t, body); got.FolderID == nil || *got.FolderID != f.ID {
		t.Fatalf("la sesión no quedó en el folder: %s", body)
	}

	// null es "sacala del folder", y tiene que distinguirse de no mandar nada.
	code, body = do(t, srv, http.MethodPut, "/api/sessions/"+sess.ID+"/folder", `{"folder_id":null}`)
	if code != http.StatusNoContent {
		t.Fatalf("PUT null = %d (%s)", code, body)
	}
	_, body = do(t, srv, http.MethodGet, "/api/sessions/"+sess.ID, "")
	if got := decodeSession(t, body); got.FolderID != nil {
		t.Fatalf("la sesión siguió en el folder: %s", body)
	}
}

func TestMoverSesionAFolderInexistenteDa404(t *testing.T) {
	srv, _ := newTestServer(t)

	_, body := do(t, srv, http.MethodPost, "/api/sessions", `{"cols":80,"rows":24}`)
	sess := decodeSession(t, body)

	code, body := do(t, srv, http.MethodPut, "/api/sessions/"+sess.ID+"/folder",
		`{"folder_id":"no-existe"}`)
	if code != http.StatusNotFound {
		t.Fatalf("PUT = %d (%s), esperaba 404", code, body)
	}
}

// Borrar un folder con sesiones adentro las deja sueltas, no las borra.
func TestBorrarFolderNoBorraSusSesiones(t *testing.T) {
	srv, _ := newTestServer(t)

	_, body := do(t, srv, http.MethodPost, "/api/sessions", `{"cols":80,"rows":24}`)
	sess := decodeSession(t, body)
	_, body = do(t, srv, http.MethodPost, "/api/folders", `{"name":"Iceberg"}`)
	f := decodeFolder(t, body)
	if code, _ := do(t, srv, http.MethodPut, "/api/sessions/"+sess.ID+"/folder",
		`{"folder_id":"`+f.ID+`"}`); code != http.StatusNoContent {
		t.Fatalf("no se pudo mover la sesión")
	}

	if code, body := do(t, srv, http.MethodDelete, "/api/folders/"+f.ID, ""); code != http.StatusNoContent {
		t.Fatalf("DELETE = %d (%s)", code, body)
	}

	code, body := do(t, srv, http.MethodGet, "/api/sessions/"+sess.ID, "")
	if code != http.StatusOK {
		t.Fatalf("la sesión desapareció con el folder: %d (%s)", code, body)
	}
	if got := decodeSession(t, body); got.FolderID != nil {
		t.Fatalf("la sesión quedó apuntando a un folder borrado: %s", body)
	}
}
