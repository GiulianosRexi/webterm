package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/giuliano/webterm/internal/resources"
)

// proveedorFalso responde sin tocar la red, para que los tests del servidor no
// dependan de GitHub.
type proveedorFalso struct{}

func (proveedorFalso) Match(rawURL string) (resources.Ref, bool) {
	if rawURL != "https://github.com/o/r/pull/1" {
		return resources.Ref{}, false
	}
	return resources.Ref{System: "gh", Type: "pr", URL: rawURL}, true
}

func (proveedorFalso) Fetch(context.Context, resources.Ref) (*resources.Snapshot, error) {
	return &resources.Snapshot{PR: &resources.PRState{
		Number: 1, Title: "un PR", State: "OPEN", ReviewDecision: "APPROVED",
		UnresolvedCount: 3, ChecksState: "SUCCESS", ChecksTotal: 5,
	}}, nil
}

type resourceJSON struct {
	ID       int64  `json:"id"`
	System   string `json:"system"`
	Type     string `json:"type"`
	Ref      string `json:"ref"`
	Snapshot *struct {
		FetchedAt int64  `json:"fetched_at"`
		Error     string `json:"error"`
		PR        *struct {
			Number          int    `json:"number"`
			State           string `json:"state"`
			UnresolvedCount int    `json:"unresolved_count"`
		} `json:"pr"`
	} `json:"snapshot"`
}

func TestLinkearYListarRecursos(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := createSession(t, srv)
	base := "/api/sessions/" + rec.ID + "/resources"

	status, body := do(t, srv, "GET", base, "")
	if status != http.StatusOK || string(bytes.TrimSpace(body)) != "[]" {
		t.Fatalf("sin recursos = %d %s", status, body)
	}

	status, body = do(t, srv, "POST", base, `{"ref":"https://github.com/o/r/pull/1"}`)
	if status != http.StatusCreated {
		t.Fatalf("POST = %d: %s", status, body)
	}
	var creado resourceJSON
	if err := json.Unmarshal(body, &creado); err != nil {
		t.Fatalf("decodificando: %v", err)
	}
	// system y type los infiere el backend de la URL.
	if creado.System != "gh" || creado.Type != "pr" {
		t.Fatalf("no se infirió system/type: %+v", creado)
	}

	_, body = do(t, srv, "GET", base, "")
	var lista []resourceJSON
	if err := json.Unmarshal(body, &lista); err != nil {
		t.Fatalf("decodificando lista: %v", err)
	}
	if len(lista) != 1 {
		t.Fatalf("vinieron %d recursos", len(lista))
	}
	if lista[0].Snapshot == nil || lista[0].Snapshot.PR == nil {
		t.Fatalf("la lista tiene que traer el estado: %s", body)
	}
	if lista[0].Snapshot.PR.UnresolvedCount != 3 {
		t.Fatalf("estado mal: %+v", lista[0].Snapshot.PR)
	}
	if lista[0].Snapshot.FetchedAt == 0 {
		t.Fatal("falta fetched_at: la UI necesita saber qué tan viejo es el dato")
	}
}

func TestLinkearURLDesconocida(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := createSession(t, srv)
	base := "/api/sessions/" + rec.ID + "/resources"

	if status, _ := do(t, srv, "POST", base, `{"ref":"https://ejemplo.invalido/x"}`); status != http.StatusBadRequest {
		t.Fatalf("URL desconocida = %d", status)
	}
	if status, _ := do(t, srv, "POST", base, `{"ref":""}`); status != http.StatusBadRequest {
		t.Fatalf("ref vacío = %d", status)
	}
}

func TestLinkearDuplicado(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := createSession(t, srv)
	base := "/api/sessions/" + rec.ID + "/resources"
	body := `{"ref":"https://github.com/o/r/pull/1"}`

	if status, _ := do(t, srv, "POST", base, body); status != http.StatusCreated {
		t.Fatal("el primer POST tendría que andar")
	}
	if status, _ := do(t, srv, "POST", base, body); status != http.StatusConflict {
		t.Fatalf("el duplicado = %d, se esperaba 409", status)
	}
}

func TestLinkearEnSesionInexistente(t *testing.T) {
	srv, _ := newTestServer(t)
	if status, _ := do(t, srv, "POST", "/api/sessions/no-existe/resources",
		`{"ref":"https://github.com/o/r/pull/1"}`); status != http.StatusNotFound {
		t.Fatalf("sesión inexistente = %d", status)
	}
	if status, _ := do(t, srv, "GET", "/api/sessions/no-existe/resources", ""); status != http.StatusNotFound {
		t.Fatalf("GET de sesión inexistente = %d", status)
	}
}

func TestDeslinkear(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := createSession(t, srv)
	base := "/api/sessions/" + rec.ID + "/resources"

	_, body := do(t, srv, "POST", base, `{"ref":"https://github.com/o/r/pull/1"}`)
	var creado resourceJSON
	_ = json.Unmarshal(body, &creado)
	id := strconv.FormatInt(creado.ID, 10)

	if status, _ := do(t, srv, "DELETE", base+"/"+id, ""); status != http.StatusNoContent {
		t.Fatalf("DELETE = %d", status)
	}
	if status, _ := do(t, srv, "DELETE", base+"/"+id, ""); status != http.StatusNotFound {
		t.Fatalf("DELETE repetido = %d", status)
	}
	if status, _ := do(t, srv, "DELETE", base+"/abc", ""); status != http.StatusBadRequest {
		t.Fatalf("id no numérico = %d", status)
	}
}

// TestRecursosSeBorranConLaSesion verifica la cascada a través de la API.
func TestRecursosSeBorranConLaSesion(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := createSession(t, srv)
	base := "/api/sessions/" + rec.ID + "/resources"
	_, _ = do(t, srv, "POST", base, `{"ref":"https://github.com/o/r/pull/1"}`)

	if status, _ := do(t, srv, "DELETE", "/api/sessions/"+rec.ID, ""); status != http.StatusNoContent {
		t.Fatal("no se pudo borrar la sesión")
	}
	if status, _ := do(t, srv, "GET", base, ""); status != http.StatusNotFound {
		t.Fatal("los recursos sobrevivieron a la sesión")
	}
}
