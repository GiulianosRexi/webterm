package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/giuliano/webterm/internal/store"
)

// attach abre un WebSocket contra una sesión existente.
func attach(t *testing.T, srv *httptest.Server, id string) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/terminal?session_id=" + id
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
	deadline := time.Now().Add(15 * time.Second)
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
	t.Fatalf("timeout esperando %q, acumulado: %q", want, sb.String())
	return ""
}

// awaitControl espera un mensaje de control con el type pedido.
func awaitControl(t *testing.T, conn *websocket.Conn, want string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		_ = conn.SetReadDeadline(deadline)
		typ, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("esperando %q: %v", want, err)
		}
		if typ != websocket.TextMessage {
			continue
		}
		var msg map[string]any
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		if msg["type"] == want {
			return msg
		}
	}
	t.Fatalf("timeout esperando el mensaje %q", want)
	return nil
}

// TestAttachMandaAttachedYReady: el handshake del protocolo, en orden.
func TestAttachMandaAttachedYReady(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := createSession(t, srv)
	conn := attach(t, srv, rec.ID)

	msg := awaitControl(t, conn, "attached")
	sess, ok := msg["session"].(map[string]any)
	if !ok || sess["id"] != rec.ID {
		t.Fatalf("attached sin la sesión correcta: %v", msg)
	}
	awaitControl(t, conn, "ready")
}

// TestTerminalEcho valida el pipeline completo: input del browser -> pty ->
// output de vuelta al browser.
func TestTerminalEcho(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := createSession(t, srv)
	conn := attach(t, srv, rec.ID)

	if err := conn.WriteMessage(websocket.BinaryMessage, []byte("echo hola-webterm-ok\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	awaitOutput(t, conn, "hola-webterm-ok")
}

// TestTerminalResize valida que el mensaje de control llegue al pty: el shell
// tiene que ver el tamaño nuevo.
func TestTerminalResize(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := createSession(t, srv)
	conn := attach(t, srv, rec.ID)

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
	srv, _ := newTestServer(t)
	rec := createSession(t, srv)
	conn := attach(t, srv, rec.ID)

	if err := conn.WriteMessage(websocket.BinaryMessage, []byte("exit 3\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	msg := awaitControl(t, conn, "exit")
	if msg["reason"] != string(store.ReasonNormal) {
		t.Fatalf("reason = %v", msg["reason"])
	}
	if code, _ := msg["code"].(float64); int(code) != 3 {
		t.Fatalf("code = %v", msg["code"])
	}
}

// TestSesionSobreviveAlCierreDelSocket es la premisa de M2 de punta a punta:
// cerrar la pestaña no mata el proceso y al volver está todo el historial.
func TestSesionSobreviveAlCierreDelSocket(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := createSession(t, srv)

	conn := attach(t, srv, rec.ID)
	if err := conn.WriteMessage(websocket.BinaryMessage, []byte("echo antes-de-cerrar\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	awaitOutput(t, conn, "antes-de-cerrar")

	// Se "cierra la pestaña".
	_ = conn.Close()
	time.Sleep(300 * time.Millisecond)

	// La sesión sigue viva en la API.
	status, body := do(t, srv, "GET", "/api/sessions/"+rec.ID, "")
	if status != http.StatusOK {
		t.Fatalf("GET = %d: %s", status, body)
	}
	if got := decodeSession(t, body); got.PtyStatus != store.StatusRunning {
		t.Fatalf("la sesión murió al cerrar el socket: %+v", got)
	}

	// Y al reattachear llega el replay con lo de antes.
	conn2 := attach(t, srv, rec.ID)
	awaitControl(t, conn2, "attached")
	awaitOutput(t, conn2, "antes-de-cerrar")

	// El proceso es el mismo y sigue respondiendo.
	if err := conn2.WriteMessage(websocket.BinaryMessage, []byte("echo despues-de-volver\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	awaitOutput(t, conn2, "despues-de-volver")
}

// TestAttachASesionMuertaEsSoloLectura: se puede ver el historial de una
// sesión terminada con el mismo código de la UI.
func TestAttachASesionMuertaEsSoloLectura(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := createSession(t, srv)

	conn := attach(t, srv, rec.ID)
	_ = conn.WriteMessage(websocket.BinaryMessage, []byte("echo quedo-en-la-historia\n"))
	awaitOutput(t, conn, "quedo-en-la-historia")
	_ = conn.Close()

	if status, body := do(t, srv, "POST", "/api/sessions/"+rec.ID+"/kill", ""); status != http.StatusOK {
		t.Fatalf("kill = %d: %s", status, body)
	}

	conn2 := attach(t, srv, rec.ID)
	awaitControl(t, conn2, "attached")
	awaitOutput(t, conn2, "quedo-en-la-historia")
	msg := awaitControl(t, conn2, "exit")
	if msg["reason"] != string(store.ReasonKilled) {
		t.Fatalf("reason = %v", msg["reason"])
	}
}

// TestWSSinSessionID: crear sesiones es tarea del endpoint REST; el upgrade no
// puede ser una puerta trasera para spawnear shells.
func TestWSSinSessionID(t *testing.T) {
	srv, _ := newTestServer(t)
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/terminal"
	_, res, err := websocket.DefaultDialer.Dial(url, nil)
	if err == nil {
		t.Fatal("se esperaba un error")
	}
	if res == nil || res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %v", res)
	}
}

func TestWSSessionInexistente(t *testing.T) {
	srv, _ := newTestServer(t)
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/terminal?session_id=no-existe"
	_, res, err := websocket.DefaultDialer.Dial(url, nil)
	if err == nil {
		t.Fatal("se esperaba un error")
	}
	if res == nil || res.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %v", res)
	}
}
