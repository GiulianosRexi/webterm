package server

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// dial levanta el servidor y abre una sesión de terminal por WebSocket.
func dial(t *testing.T, query string) *websocket.Conn {
	t.Helper()
	srv := httptest.NewServer(New(Config{Shell: "/bin/bash"}).Handler())
	t.Cleanup(srv.Close)

	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/terminal" + query
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// awaitOutput acumula output del pty hasta encontrar want.
func awaitOutput(t *testing.T, conn *websocket.Conn, want string) string {
	t.Helper()
	var sb strings.Builder
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_ = conn.SetReadDeadline(deadline)
		typ, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("leyendo output (acumulado %q): %v", sb.String(), err)
		}
		if typ != websocket.BinaryMessage {
			continue
		}
		sb.Write(data)
		if strings.Contains(sb.String(), want) {
			return sb.String()
		}
	}
	t.Fatalf("timeout esperando %q, output acumulado: %q", want, sb.String())
	return ""
}

// TestTerminalEcho valida el pipeline completo: input del browser -> pty ->
// output de vuelta al browser.
func TestTerminalEcho(t *testing.T) {
	conn := dial(t, "?cols=80&rows=24")

	if err := conn.WriteMessage(websocket.BinaryMessage, []byte("echo hola-webterm-ok\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	// El primer match es el eco del propio tipeo; el segundo, la salida real.
	out := awaitOutput(t, conn, "hola-webterm-ok")
	if strings.Count(out, "hola-webterm-ok") == 0 {
		t.Fatalf("no se encontró la salida del comando: %q", out)
	}
}

// TestTerminalResize valida que el mensaje de control llegue al pty: el shell
// tiene que ver el tamaño nuevo.
func TestTerminalResize(t *testing.T) {
	conn := dial(t, "?cols=80&rows=24")

	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"resize","cols":123,"rows":45}`)); err != nil {
		t.Fatalf("resize: %v", err)
	}
	if err := conn.WriteMessage(websocket.BinaryMessage, []byte("stty size\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	awaitOutput(t, conn, "45 123")
}

// TestTerminalExit valida que al terminar el shell el backend avise al browser.
func TestTerminalExit(t *testing.T) {
	conn := dial(t, "")

	if err := conn.WriteMessage(websocket.BinaryMessage, []byte("exit\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_ = conn.SetReadDeadline(deadline)
		typ, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("esperando exit: %v", err)
		}
		if typ == websocket.TextMessage && strings.Contains(string(data), `"exit"`) {
			return
		}
	}
	t.Fatal("timeout esperando el mensaje de exit")
}
