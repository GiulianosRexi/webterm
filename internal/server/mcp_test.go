package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	webmcp "github.com/giuliano/webterm/internal/mcp"
)

// headerRT agrega headers fijos a cada request, que es como Claude Code manda
// el id de sesión y el token.
type headerRT struct {
	base    http.RoundTripper
	headers map[string]string
}

func (h headerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, v := range h.headers {
		r.Header.Set(k, v)
	}
	return h.base.RoundTrip(r)
}

// conectarMCP abre una sesión MCP contra el servidor, con los headers dados.
func conectarMCP(t *testing.T, srv *httptest.Server, headers map[string]string) *sdk.ClientSession {
	t.Helper()
	cli := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	sess, err := cli.Connect(ctx, &sdk.StreamableClientTransport{
		Endpoint:   srv.URL + "/mcp",
		HTTPClient: &http.Client{Transport: headerRT{http.DefaultTransport, headers}},
		// En modo Stateless el GET da 405, así que no hay stream standalone.
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatalf("conectando al MCP: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

func llamarTool(t *testing.T, sess *sdk.ClientSession, name string, args map[string]any) *sdk.CallToolResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	res, err := sess.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return res
}

func textoDe(t *testing.T, res *sdk.CallToolResult) string {
	t.Helper()
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}

// TestMCPEndToEnd recorre el protocolo entero contra el servidor real: el
// cliente del SDK se conecta por HTTP, lista las tools, las llama, y después se
// verifica por la API REST que el estado quedó escrito de verdad.
func TestMCPEndToEnd(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := createSession(t, srv)
	sess := conectarMCP(t, srv, map[string]string{webmcp.SessionHeader: rec.ID})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Las cinco tools tienen que estar anunciadas.
	lista, err := sess.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	nombres := map[string]bool{}
	for _, tool := range lista.Tools {
		nombres[tool.Name] = true
		if tool.Description == "" {
			t.Errorf("la tool %s no tiene descripción", tool.Name)
		}
	}
	for _, quiero := range []string{"set_context", "get_context", "set_title", "link_pr", "list_links"} {
		if !nombres[quiero] {
			t.Errorf("falta la tool %s (hay: %v)", quiero, nombres)
		}
	}

	// set_title llega hasta la base.
	llamarTool(t, sess, "set_title", map[string]any{
		"title": "arreglar el token", "description": "viene de un hook",
	})
	_, body := do(t, srv, "GET", "/api/sessions/"+rec.ID, "")
	got := decodeSession(t, body)
	if got.Title != "arreglar el token" || got.Description != "viene de un hook" {
		t.Fatalf("set_title no se persistió: %+v", got)
	}

	// set_context y get_context, ida y vuelta.
	llamarTool(t, sess, "set_context", map[string]any{"key": "ticket", "value": "ABC-2425"})
	_, body = do(t, srv, "GET", "/api/sessions/"+rec.ID+"/kv", "")
	var kv map[string]string
	_ = json.Unmarshal(body, &kv)
	if kv["ticket"] != "ABC-2425" {
		t.Fatalf("set_context no se persistió: %s", body)
	}
	if txt := textoDe(t, llamarTool(t, sess, "get_context", map[string]any{"key": "ticket"})); txt != "ABC-2425" {
		t.Fatalf("get_context = %q", txt)
	}

	// link_pr usa el proveedor falso del test, así que no toca la red.
	llamarTool(t, sess, "link_pr", map[string]any{"url": "https://github.com/o/r/pull/1"})
	_, body = do(t, srv, "GET", "/api/sessions/"+rec.ID+"/resources", "")
	if !strings.Contains(string(body), "github.com/o/r/pull/1") {
		t.Fatalf("link_pr no se persistió: %s", body)
	}

	// Y list_links lo ve, con el estado que trae el proveedor.
	if txt := textoDe(t, llamarTool(t, sess, "list_links", map[string]any{})); !strings.Contains(txt, "un PR") {
		t.Fatalf("list_links = %q", txt)
	}
}

// TestMCPSinHeaderDeSesion: el error tiene que explicar cómo configurarlo, y
// llegar como error de tool para que el modelo pueda leerlo.
func TestMCPSinHeaderDeSesion(t *testing.T) {
	srv, _ := newTestServer(t)
	sess := conectarMCP(t, srv, map[string]string{})

	res := llamarTool(t, sess, "get_context", map[string]any{})
	if !res.IsError {
		t.Fatal("se esperaba un error de tool")
	}
	if txt := textoDe(t, res); !strings.Contains(txt, webmcp.SessionHeader) {
		t.Fatalf("el error tendría que nombrar el header: %q", txt)
	}
}

// TestMCPConLiteralSinExpandir cubre a Claude corriendo fuera de WebTerm.
func TestMCPConLiteralSinExpandir(t *testing.T) {
	srv, _ := newTestServer(t)
	sess := conectarMCP(t, srv, map[string]string{
		webmcp.SessionHeader: "${WEBTERM_SESSION_ID}",
	})

	res := llamarTool(t, sess, "get_context", map[string]any{})
	if !res.IsError {
		t.Fatal("se esperaba un error de tool")
	}
	if txt := textoDe(t, res); !strings.Contains(txt, "adentro de una sesión de WebTerm") {
		t.Fatalf("mensaje poco útil: %q", txt)
	}
}
