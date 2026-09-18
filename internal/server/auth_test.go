package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

const testToken = "token-de-prueba"

func authServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(New(Config{Token: testToken}, nil).Handler())
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
	srv := httptest.NewServer(New(Config{}, nil).Handler())
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

// TestLoginPageEnNavegacion: una navegación del browser sin cookie tiene que
// ver el formulario, no un 401 pelado.
func TestLoginPageEnNavegacion(t *testing.T) {
	srv := authServer(t)
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	body := new(strings.Builder)
	if _, err := io.Copy(body, res.Body); err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("esperaba 401, obtuve %d", res.StatusCode)
	}
	if !strings.Contains(body.String(), `name="token"`) {
		t.Fatalf("no se renderizó el formulario de login: %q", body.String())
	}
}

// TestLoginFormCorrecto: el POST del formulario deja la cookie y manda a la UI.
func TestLoginFormCorrecto(t *testing.T) {
	srv := authServer(t)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	res, err := client.PostForm(srv.URL+loginPath, url.Values{"token": {testToken}})
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusFound {
		t.Fatalf("esperaba 302, obtuve %d", res.StatusCode)
	}
	if loc := res.Header.Get("Location"); loc != "/" {
		t.Fatalf("esperaba redirect a /, obtuve %q", loc)
	}

	var cookie *http.Cookie
	for _, c := range res.Cookies() {
		if c.Name == cookieName {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("el login no dejó la cookie")
	}

	// La cookie tiene que alcanzar para pasar la auth del WebSocket. El
	// handshake igual falla, pero con 400 (falta session_id, que sale del ABM)
	// y no con 401: eso es justo lo que distingue "autenticado" de "rechazado".
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/terminal"
	_, res, err = websocket.DefaultDialer.Dial(wsURL, http.Header{
		"Cookie": {cookie.Name + "=" + cookie.Value},
	})
	if err == nil {
		t.Fatal("sin session_id el handshake no tendría que prosperar")
	}
	if res == nil || res.StatusCode != http.StatusBadRequest {
		t.Fatalf("con la cookie del login se esperaba 400, obtuve %v", res)
	}
}

// TestLoginFormIncorrecto: token equivocado vuelve al formulario con el error.
func TestLoginFormIncorrecto(t *testing.T) {
	srv := authServer(t)
	res, err := http.PostForm(srv.URL+loginPath, url.Values{"token": {"no-es"}})
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	body := new(strings.Builder)
	_, _ = io.Copy(body, res.Body)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("esperaba 401, obtuve %d", res.StatusCode)
	}
	if !strings.Contains(body.String(), "Token incorrecto") {
		t.Fatal("no se mostró el mensaje de error")
	}
	for _, c := range res.Cookies() {
		if c.Name == cookieName {
			t.Fatal("se dejó cookie con un token incorrecto")
		}
	}
}

// TestLogout: salir invalida la cookie del browser.
func TestLogout(t *testing.T) {
	srv := authServer(t)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/logout", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: testToken})
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	for _, c := range res.Cookies() {
		if c.Name == cookieName && c.MaxAge >= 0 {
			t.Fatalf("la cookie no se invalidó: MaxAge=%d", c.MaxAge)
		}
	}
}
