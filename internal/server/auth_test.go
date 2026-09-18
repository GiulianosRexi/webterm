package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

const testToken = "token-de-prueba"

func authServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(New(Config{Shell: "/bin/bash", Token: testToken}).Handler())
	t.Cleanup(srv.Close)
	return srv
}

// TestAuthRechazaSinToken: sin token no se llega ni a la UI ni al WebSocket.
func TestAuthRechazaSinToken(t *testing.T) {
	srv := authServer(t)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	res, err := client.Get(srv.URL + "/api/health")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("esperaba 401, obtuve %d", res.StatusCode)
	}

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/terminal"
	if _, _, err := websocket.DefaultDialer.Dial(wsURL, nil); err == nil {
		t.Fatal("el WebSocket aceptó una conexión sin token")
	}
}

// TestAuthTokenEnQueryDejaCookie: el primer acceso con ?token= redirige y deja
// la cookie, así el resto de la navegación no necesita el token en la URL.
func TestAuthTokenEnQueryDejaCookie(t *testing.T) {
	srv := authServer(t)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	res, err := client.Get(srv.URL + "/api/health?token=" + testToken)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusFound {
		t.Fatalf("esperaba redirect 302, obtuve %d", res.StatusCode)
	}
	if loc := res.Header.Get("Location"); strings.Contains(loc, "token") {
		t.Fatalf("el redirect todavía lleva el token: %q", loc)
	}

	var cookie *http.Cookie
	for _, c := range res.Cookies() {
		if c.Name == cookieName {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no se seteó la cookie con el token")
	}

	// Con la cookie ya alcanza.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/health", nil)
	req.AddCookie(cookie)
	res2, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res2.Body.Close()
	if res2.StatusCode != http.StatusOK {
		t.Fatalf("con cookie esperaba 200, obtuve %d", res2.StatusCode)
	}
}

// TestAuthTokenInvalido: un token que no es el nuestro no entra.
func TestAuthTokenInvalido(t *testing.T) {
	srv := authServer(t)
	res, err := http.Get(srv.URL + "/api/health?token=otra-cosa")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("esperaba 401, obtuve %d", res.StatusCode)
	}
}

// TestWebSocketRechazaOtroOrigin: aunque el browser mande la cookie, un
// handshake originado en otra página no debe abrir una shell.
func TestWebSocketRechazaOtroOrigin(t *testing.T) {
	srv := httptest.NewServer(New(Config{Shell: "/bin/bash"}).Handler())
	t.Cleanup(srv.Close)

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/terminal"
	_, res, err := websocket.DefaultDialer.Dial(wsURL, http.Header{
		"Origin": []string{"http://sitio-malicioso.example"},
	})
	if err == nil {
		t.Fatal("se aceptó un handshake cross-origin")
	}
	if res != nil && res.StatusCode != http.StatusForbidden {
		t.Fatalf("esperaba 403, obtuve %d", res.StatusCode)
	}
}

func TestIsLoopback(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:7788":     true,
		"localhost:7788":     true,
		"[::1]:7788":         true,
		"0.0.0.0:7788":       false,
		":7788":              false,
		"192.168.0.250:7788": false,
	}
	for addr, want := range cases {
		if got := IsLoopback(addr); got != want {
			t.Errorf("IsLoopback(%q) = %v, esperaba %v", addr, got, want)
		}
	}
}
