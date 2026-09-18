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

// waitForReady drena el handshake (replay + ready) para dejar al conn
// posicionado justo al principio del stream vivo.
func waitForReady(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		typ, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("esperando ready: %v", err)
		}
		if typ == websocket.TextMessage && strings.Contains(string(data), `"ready"`) {
			return
		}
	}
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

// TestAttachClienteLentoQueSigueLeyendoRecibeDropped cubre el caso en el que
// dropped SÍ llega: un cliente que se queda atrás pero nunca deja de leer del
// todo. Acá el drop lo dispara el buffer del hub (se le acumulan 256 chunks
// sin consumir) y no una escritura trabada, así que la conexión sigue sana y
// el frame de texto sale.
func TestAttachClienteLentoQueSigueLeyendoRecibeDropped(t *testing.T) {
	srv, st := newTestDaemon(t)
	id := spawnado(t, srv, st)
	conn := dialAttach(t, srv, id)
	waitForReady(t, conn)

	// yes con líneas largas alcanza para desbordar el buffer del hub durante
	// la pausa de abajo: no hace falta tocar los buffers del socket.
	if err := conn.WriteMessage(websocket.BinaryMessage, []byte("yes "+strings.Repeat("A", 200)+"\n")); err != nil {
		t.Fatal(err)
	}

	// Pausa deliberada, sin leer nada: el productor sigue mientras este
	// cliente se queda atrás a propósito. Es lo que desborda el buffer del
	// hub y dispara el drop.
	time.Sleep(500 * time.Millisecond)

	// A partir de acá el cliente vuelve a leer: lento (nunca dejó de estarlo
	// del todo) pero sigue drenando, así que cualquier escritura que haya
	// quedado a mitad de camino termina destrabándose sola.
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	sawDropped := false
	for {
		typ, data, err := conn.ReadMessage()
		if err != nil {
			break
		}
		if typ == websocket.TextMessage && strings.Contains(string(data), `"dropped"`) {
			sawDropped = true
			break
		}
	}
	if !sawDropped {
		t.Fatal("un cliente que se queda atrás pero sigue leyendo tendría que recibir dropped")
	}
}

// TestAttachClienteQueDejaDeLeerNoRecibeDropped documenta la limitación que
// describe el comentario de attach.go: un cliente que deja de leer del todo
// (pestaña congelada, proxy trabado) NO recibe dropped. La escritura del
// chunk que provoca el drop es la misma que se traba contra el socket y
// vence writeTimeout; gorilla deja ese error pegajoso en la conexión
// (prepWrite lo repite en toda escritura posterior sin tocar el cable), así
// que el writeJSON del frame dropped falla en silencio igual que cualquier
// otra escritura después de esa. La ausencia del frame acá es el
// comportamiento esperado, no un bug pendiente: la señal autoritativa para
// este caso es la fila del orquestador, no este socket.
func TestAttachClienteQueDejaDeLeerNoRecibeDropped(t *testing.T) {
	srv, st := newTestDaemon(t)
	id := spawnado(t, srv, st)
	conn := dialAttach(t, srv, id)
	waitForReady(t, conn)

	if err := conn.WriteMessage(websocket.BinaryMessage, []byte("yes "+strings.Repeat("A", 200)+"\n")); err != nil {
		t.Fatal(err)
	}

	// Este cliente deja de leer del todo, y por más tiempo que writeTimeout:
	// alcanza para que la escritura del pty quede bloqueada contra el socket
	// y venza su propio deadline, envenenando la conexión.
	time.Sleep(writeTimeout + 500*time.Millisecond)

	// Recién ahora se lee. Si dropped fuera a llegar, tendría que aparecer acá;
	// lo único que hay es el cierre (EOF o 1006), sin el frame antes.
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		typ, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if typ == websocket.TextMessage && strings.Contains(string(data), `"dropped"`) {
			t.Fatal("un cliente que dejó de leer del todo no debería recibir dropped (ver el comentario de attach.go)")
		}
	}
}
