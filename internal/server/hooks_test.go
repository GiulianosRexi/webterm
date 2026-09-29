package server

import (
	"net/http"
	"strings"
	"testing"

	webmcp "github.com/giuliano/webterm/internal/mcp"
)

func TestHookMovesWorkStatus(t *testing.T) {
	srv, mgr := newTestServer(t)
	rec := createSession(t, srv)

	post := func(session, body string) int {
		t.Helper()
		req, err := http.NewRequest("POST", srv.URL+"/api/hooks", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if session != "" {
			req.Header.Set(webmcp.SessionHeader, session)
		}
		res, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}

	body := `{"session_id":"x","hook_event_name":"PreToolUse","tool_name":"AskUserQuestion","tool_input":{"questions":[]}}`
	if got := post(rec.ID, body); got != http.StatusNoContent {
		t.Fatalf("status = %d", got)
	}
	s, err := mgr.Get(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if s.WorkStatus != "waiting_input" {
		t.Fatalf("work_status = %q", s.WorkStatus)
	}

	// Claude fuera de WebTerm: header vacío, no es un error.
	if got := post("", body); got != http.StatusNoContent {
		t.Fatalf("sin sesión = %d", got)
	}
	if got := post("nope", body); got != http.StatusNotFound {
		t.Fatalf("sesión inexistente = %d", got)
	}
	if got := post(rec.ID, "{"); got != http.StatusBadRequest {
		t.Fatalf("body inválido = %d", got)
	}
}
