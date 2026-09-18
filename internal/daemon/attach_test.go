package daemon

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/giuliano/webterm/internal/ptyapi"
	"github.com/giuliano/webterm/internal/store"
)

// dialAttach abre el WebSocket de attach contra el httptest.Server.
func dialAttach(t *testing.T, srv *httptest.Server, id string) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/sessions/" + id + "/attach"
	conn, res, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		if res != nil {
			t.Fatalf("attach falló con status %d: %v", res.StatusCode, err)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// spawnado inserta la fila, spawnea por HTTP y devuelve el id.
func spawnado(t *testing.T, srv *httptest.Server, st *store.Store) string {
	t.Helper()
	rec := nuevaFila(t, st)
	res := postJSON(t, srv.URL+"/sessions", ptyapi.SpawnOpts{
		ID: rec.ID, Shell: rec.Shell, Cwd: rec.Cwd, Cols: 80, Rows: 24,
	})
	defer res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("spawn dio %d", res.StatusCode)
	}
	return rec.ID
}

// leerHasta junta frames binarios hasta encontrar la marca o agotar el plazo.
func leerHasta(t *testing.T, conn *websocket.Conn, marca string, plazo time.Duration) []byte {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(plazo))
	var buf bytes.Buffer
	for {
		typ, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("esperando %q, junté %q: %v", marca, buf.String(), err)
		}
		if typ == websocket.BinaryMessage {
			buf.Write(data)
			if bytes.Contains(buf.Bytes(), []byte(marca)) {
				return buf.Bytes()
			}
		}
	}
}

func TestAttachMandaReadyDespuesDelReplay(t *testing.T) {
	srv, st := newTestDaemon(t)
	id := spawnado(t, srv, st)

	conn := dialAttach(t, srv, id)
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		typ, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		if typ == websocket.TextMessage {
			if !strings.Contains(string(data), `"ready"`) {
				t.Fatalf("primer frame de texto = %q; quería ready", data)
			}
			return
		}
		// Los binarios de antes de ready son el replay: está bien que haya.
	}
}

func TestAttachEcoDeInput(t *testing.T) {
	srv, st := newTestDaemon(t)
	id := spawnado(t, srv, st)
	conn := dialAttach(t, srv, id)

	if err := conn.WriteMessage(websocket.BinaryMessage, []byte("echo MARCA-ECO\n")); err != nil {
		t.Fatal(err)
	}
	// El eco del shell trae la marca dos veces (el tipeo y la salida); con
	// encontrarla alcanza para saber que el input llegó al pty.
	leerHasta(t, conn, "MARCA-ECO", 5*time.Second)
}

func TestAttachResizeLlegaAlPty(t *testing.T) {
	srv, st := newTestDaemon(t)
	id := spawnado(t, srv, st)
	conn := dialAttach(t, srv, id)

	if err := conn.WriteJSON(map[string]any{"type": "resize", "rows": 30, "cols": 100}); err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteMessage(websocket.BinaryMessage, []byte("stty size\n")); err != nil {
		t.Fatal(err)
	}
	leerHasta(t, conn, "30 100", 5*time.Second)

	// El resize también se persiste: es lo que hace que una sesión reanudada
	// vuelva con las dimensiones que tenía.
	esperar(t, 2*time.Second, "cols persistidas", func() bool {
		rec, err := st.GetSession(id)
		return err == nil && rec.Cols == 100 && rec.Rows == 30
	})
}

// Cuando el pty muere, el stream se corta. Para ese momento la fila ya tiene
// que decir exited: es la garantía sobre la que el orquestador arma el frame
// exit que ve el browser.
func TestAttachCierraAlMorirLaSesionYLaFilaYaEstaMarcada(t *testing.T) {
	srv, st := newTestDaemon(t)
	id := spawnado(t, srv, st)
	conn := dialAttach(t, srv, id)

	if err := conn.WriteMessage(websocket.BinaryMessage, []byte("exit\n")); err != nil {
		t.Fatal(err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			break
		}
	}

	rec, err := st.GetSession(id)
	if err != nil {
		t.Fatal(err)
	}
	if rec.PtyStatus != store.StatusExited {
		t.Fatalf("al cerrarse el stream la fila decía %s; quería exited", rec.PtyStatus)
	}
}

func TestAttachASesionNoVivaDa410(t *testing.T) {
	srv, st := newTestDaemon(t)
	rec := nuevaFila(t, st) // fila sin spawnear: existe pero no tiene proceso

	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/sessions/" + rec.ID + "/attach"
	_, res, err := websocket.DefaultDialer.Dial(url, nil)
	if err == nil {
		t.Fatal("tendría que haber fallado")
	}
	if res == nil || res.StatusCode != http.StatusGone {
		t.Fatalf("status = %v; quería 410", res)
	}
}

func TestDosClientesVenElMismoOutput(t *testing.T) {
	srv, st := newTestDaemon(t)
	id := spawnado(t, srv, st)

	a := dialAttach(t, srv, id)
	b := dialAttach(t, srv, id)

	if err := a.WriteMessage(websocket.BinaryMessage, []byte("echo MARCA-FANOUT\n")); err != nil {
		t.Fatal(err)
	}
	leerHasta(t, a, "MARCA-FANOUT", 5*time.Second)
	leerHasta(t, b, "MARCA-FANOUT", 5*time.Second)
}
