package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giuliano/webterm/internal/resources"
	"github.com/giuliano/webterm/internal/session"
	"github.com/giuliano/webterm/internal/store"
)

// newTestServer levanta el stack completo: store en un tmpdir, manager y
// servidor HTTP. Sin mocks.
func newTestServer(t *testing.T) (*httptest.Server, *session.Manager) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "webterm.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	mgr := session.NewManager(st, session.Config{
		Shell: "/bin/bash", HistoryBytes: 1 << 20, SweepEvery: time.Hour,
		Resources: resources.NewCache(resources.NewRegistry(proveedorFalso{})),
	})
	if err := mgr.Start(); err != nil {
		t.Fatalf("manager.Start: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })

	srv := httptest.NewServer(New(Config{}, mgr).Handler())
	t.Cleanup(srv.Close)
	return srv, mgr
}

// do manda un request y devuelve status y body.
func do(t *testing.T, srv *httptest.Server, method, path, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("armando request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(res.Body)
	return res.StatusCode, buf.Bytes()
}

func decodeSession(t *testing.T, body []byte) *store.Session {
	t.Helper()
	var s store.Session
	if err := json.Unmarshal(body, &s); err != nil {
		t.Fatalf("decodificando sesión (%s): %v", body, err)
	}
	return &s
}

func createSession(t *testing.T, srv *httptest.Server) *store.Session {
	t.Helper()
	status, body := do(t, srv, "POST", "/api/sessions", `{"title":"test","cwd":"/tmp","cols":80,"rows":24}`)
	if status != http.StatusCreated {
		t.Fatalf("POST /api/sessions = %d: %s", status, body)
	}
	return decodeSession(t, body)
}

func TestCrearYListarSesiones(t *testing.T) {
	srv, _ := newTestServer(t)

	status, body := do(t, srv, "GET", "/api/sessions", "")
	if status != http.StatusOK {
		t.Fatalf("GET vacío = %d: %s", status, body)
	}
	if strings.TrimSpace(string(body)) != "[]" {
		t.Fatalf("sin sesiones tiene que dar [], dio %s", body)
	}

	rec := createSession(t, srv)
	if rec.ID == "" || rec.Title != "test" {
		t.Fatalf("sesión mal creada: %+v", rec)
	}
	if rec.PtyStatus != store.StatusRunning {
		t.Fatalf("pty_status = %q", rec.PtyStatus)
	}

	_, body = do(t, srv, "GET", "/api/sessions", "")
	var list []*store.Session
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("decodificando lista: %v", err)
	}
	if len(list) != 1 || list[0].ID != rec.ID {
		t.Fatalf("lista = %s", body)
	}
}

func TestGetSessionYNotFound(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := createSession(t, srv)

	status, body := do(t, srv, "GET", "/api/sessions/"+rec.ID, "")
	if status != http.StatusOK {
		t.Fatalf("GET = %d: %s", status, body)
	}

	status, _ = do(t, srv, "GET", "/api/sessions/no-existe", "")
	if status != http.StatusNotFound {
		t.Fatalf("GET inexistente = %d", status)
	}
}

func TestPatchSession(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := createSession(t, srv)

	status, body := do(t, srv, "PATCH", "/api/sessions/"+rec.ID, `{"title":"renombrada","kanban_status":"in_progress"}`)
	if status != http.StatusOK {
		t.Fatalf("PATCH = %d: %s", status, body)
	}
	got := decodeSession(t, body)
	if got.Title != "renombrada" || got.KanbanStatus != "in_progress" {
		t.Fatalf("no se aplicó el patch: %+v", got)
	}

	// Un estado fuera del enum tiene que rebotar: M7 va a construir el board
	// sobre este campo.
	status, _ = do(t, srv, "PATCH", "/api/sessions/"+rec.ID, `{"kanban_status":"inventado"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("kanban_status inválido = %d", status)
	}
	status, _ = do(t, srv, "PATCH", "/api/sessions/"+rec.ID, `{no es json`)
	if status != http.StatusBadRequest {
		t.Fatalf("body inválido = %d", status)
	}
}

// TestKillYRestart: matar conserva la fila; reanudar la reusa.
func TestKillYRestart(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := createSession(t, srv)

	status, body := do(t, srv, "POST", "/api/sessions/"+rec.ID+"/kill", "")
	if status != http.StatusOK {
		t.Fatalf("kill = %d: %s", status, body)
	}
	if got := decodeSession(t, body); got.PtyStatus != store.StatusExited {
		t.Fatalf("después del kill: %+v", got)
	}

	status, body = do(t, srv, "POST", "/api/sessions/"+rec.ID+"/restart", `{"cols":100,"rows":30}`)
	if status != http.StatusOK {
		t.Fatalf("restart = %d: %s", status, body)
	}
	vuelto := decodeSession(t, body)
	if vuelto.ID != rec.ID || vuelto.PtyStatus != store.StatusRunning {
		t.Fatalf("restart devolvió %+v", vuelto)
	}

	// Reanudar una sesión viva es un conflicto, no un error interno.
	status, _ = do(t, srv, "POST", "/api/sessions/"+rec.ID+"/restart", "")
	if status != http.StatusConflict {
		t.Fatalf("restart sobre sesión viva = %d", status)
	}
}

func TestDeleteSessionHTTP(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := createSession(t, srv)

	status, body := do(t, srv, "DELETE", "/api/sessions/"+rec.ID, "")
	if status != http.StatusNoContent {
		t.Fatalf("DELETE = %d: %s", status, body)
	}
	status, _ = do(t, srv, "GET", "/api/sessions/"+rec.ID, "")
	if status != http.StatusNotFound {
		t.Fatalf("la sesión sigue viva después del DELETE: %d", status)
	}
	status, _ = do(t, srv, "DELETE", "/api/sessions/"+rec.ID, "")
	if status != http.StatusNotFound {
		t.Fatalf("DELETE repetido = %d", status)
	}
}

// TestKVHTTP prueba la superficie que va a consumir el CLI webterm de M5.
func TestKVHTTP(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := createSession(t, srv)
	base := "/api/sessions/" + rec.ID + "/kv"

	status, body := do(t, srv, "GET", base, "")
	if status != http.StatusOK || strings.TrimSpace(string(body)) != "{}" {
		t.Fatalf("KV vacío = %d %s", status, body)
	}

	if status, body := do(t, srv, "PUT", base+"/claude_session_id", "abc-123"); status != http.StatusNoContent {
		t.Fatalf("PUT = %d: %s", status, body)
	}
	_, body = do(t, srv, "GET", base, "")
	var kv map[string]string
	if err := json.Unmarshal(body, &kv); err != nil {
		t.Fatalf("decodificando kv: %v", err)
	}
	if kv["claude_session_id"] != "abc-123" {
		t.Fatalf("kv = %s", body)
	}

	if status, _ := do(t, srv, "DELETE", base+"/claude_session_id", ""); status != http.StatusNoContent {
		t.Fatalf("DELETE kv = %d", status)
	}
	if status, _ := do(t, srv, "DELETE", base+"/claude_session_id", ""); status != http.StatusNotFound {
		t.Fatalf("DELETE kv repetido = %d", status)
	}
}

// TestHealthCuentaSesiones: el health sirve para chequear de un vistazo que el
// backend ve las sesiones que dice tener.
func TestHealthCuentaSesiones(t *testing.T) {
	srv, _ := newTestServer(t)

	status, body := do(t, srv, "GET", "/api/health", "")
	if status != http.StatusOK {
		t.Fatalf("health = %d: %s", status, body)
	}
	var h struct {
		Status   string `json:"status"`
		Auth     bool   `json:"auth"`
		Sessions int    `json:"sessions"`
	}
	if err := json.Unmarshal(body, &h); err != nil {
		t.Fatalf("decodificando health: %v", err)
	}
	if h.Status != "ok" || h.Sessions != 0 {
		t.Fatalf("health = %s", body)
	}

	createSession(t, srv)
	_, body = do(t, srv, "GET", "/api/health", "")
	_ = json.Unmarshal(body, &h)
	if h.Sessions != 1 {
		t.Fatalf("sessions = %d: %s", h.Sessions, body)
	}
}
