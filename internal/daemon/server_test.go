package daemon

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giuliano/webterm/internal/ptyapi"
	"github.com/giuliano/webterm/internal/session"
	"github.com/giuliano/webterm/internal/store"
)

// newTestDaemon arma un daemon con un manager real sobre una base temporal.
// Sin mocks: el objetivo es probar el daemon contra ptys de verdad.
func newTestDaemon(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	m := session.NewManager(st, session.Config{HistoryBytes: 64 << 10})
	srv := httptest.NewServer(NewServer(m).Handler())
	t.Cleanup(func() {
		srv.Close()
		_ = m.Close()
		_ = st.Close()
	})
	return srv, st
}

// nuevaFila inserta la fila que el spawn necesita, como hace el orquestador.
func nuevaFila(t *testing.T, st *store.Store) *store.Session {
	t.Helper()
	rec := &store.Session{
		ID: store.NewID(), Cwd: t.TempDir(), Shell: "/bin/sh",
		Cols: 80, Rows: 24, PtyStatus: store.StatusStarting,
	}
	if err := st.CreateSession(rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

func postJSON(t *testing.T, url string, body any) *http.Response {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.Post(url, "application/json", strings.NewReader(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestInfoAnunciaLaVersionDeProtocolo(t *testing.T) {
	srv, _ := newTestDaemon(t)

	res, err := http.Get(srv.URL + "/info")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	var info Info
	if err := json.NewDecoder(res.Body).Decode(&info); err != nil {
		t.Fatal(err)
	}
	if info.ProtocolVersion != ProtocolVersion {
		t.Fatalf("protocol_version = %d; quería %d", info.ProtocolVersion, ProtocolVersion)
	}
	if info.PID != os.Getpid() {
		t.Fatalf("pid = %d; quería %d", info.PID, os.Getpid())
	}
	if info.StartedAt == 0 {
		t.Fatal("started_at vacío")
	}
}

func TestSpawnYListado(t *testing.T) {
	srv, st := newTestDaemon(t)
	rec := nuevaFila(t, st)

	res := postJSON(t, srv.URL+"/sessions", ptyapi.SpawnOpts{
		ID: rec.ID, Shell: rec.Shell, Cwd: rec.Cwd, Cols: 80, Rows: 24,
	})
	defer res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("POST /sessions dio %d; quería 204", res.StatusCode)
	}

	lres, err := http.Get(srv.URL + "/sessions")
	if err != nil {
		t.Fatal(err)
	}
	defer lres.Body.Close()
	var out struct {
		IDs []string `json:"ids"`
	}
	if err := json.NewDecoder(lres.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.IDs) != 1 || out.IDs[0] != rec.ID {
		t.Fatalf("ids = %v; quería [%s]", out.IDs, rec.ID)
	}
}

func TestSpawnSinFilaDa404(t *testing.T) {
	srv, _ := newTestDaemon(t)

	res := postJSON(t, srv.URL+"/sessions", ptyapi.SpawnOpts{
		ID: "no-existe", Shell: "/bin/sh", Cwd: t.TempDir(), Cols: 80, Rows: 24,
	})
	defer res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("dio %d; quería 404", res.StatusCode)
	}
}

func TestSpawnDuplicadoDa409(t *testing.T) {
	srv, st := newTestDaemon(t)
	rec := nuevaFila(t, st)
	opts := ptyapi.SpawnOpts{ID: rec.ID, Shell: rec.Shell, Cwd: rec.Cwd, Cols: 80, Rows: 24}

	postJSON(t, srv.URL+"/sessions", opts).Body.Close()
	res := postJSON(t, srv.URL+"/sessions", opts)
	defer res.Body.Close()
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("dio %d; quería 409", res.StatusCode)
	}
}

func TestKillDevuelve204YDespues410(t *testing.T) {
	srv, st := newTestDaemon(t)
	rec := nuevaFila(t, st)
	postJSON(t, srv.URL+"/sessions", ptyapi.SpawnOpts{
		ID: rec.ID, Shell: rec.Shell, Cwd: rec.Cwd, Cols: 80, Rows: 24,
	}).Body.Close()

	res := postJSON(t, srv.URL+"/sessions/"+rec.ID+"/kill", nil)
	defer res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("kill dio %d; quería 204", res.StatusCode)
	}

	// Kill es sincrónico: al volver la fila ya tiene que estar marcada.
	got, err := st.GetSession(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PtyStatus != store.StatusExited {
		t.Fatalf("después del kill la fila decía %s; quería exited", got.PtyStatus)
	}

	// Un segundo kill no tiene proceso que matar: 410, no 404 — la fila sigue
	// existiendo. La idempotencia la resuelve el orquestador, que es el que
	// sabe distinguir las dos cosas.
	res2 := postJSON(t, srv.URL+"/sessions/"+rec.ID+"/kill", nil)
	defer res2.Body.Close()
	if res2.StatusCode != http.StatusGone {
		t.Fatalf("segundo kill dio %d; quería 410", res2.StatusCode)
	}
}

// El daemon escucha en un socket Unix, no en un puerto: nadie de la red puede
// llegarle, y por eso no hay token en esta capa.
// El nombre es corto a propósito, no por estilo: t.TempDir() arma el path del
// temporal con el nombre del test adentro, y en macOS eso sumado al TMPDIR
// del sistema (largo de por sí, /var/folders/.../T/) se acerca peligrosamente
// a los ~104 bytes que el kernel admite en sockaddr_un.sun_path. Un nombre
// más descriptivo pero largo hace fallar el bind con "invalid argument" en
// esta misma máquina, no por flakiness sino de forma determinística.
func TestSocketUnix0600(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m := session.NewManager(st, session.Config{})
	defer m.Close()

	sock := filepath.Join(dir, "d.sock")
	srv := NewServer(m)
	go func() { _ = srv.Serve(sock) }()
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	waitForSocket(t, sock)

	fi, err := os.Stat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("permisos del socket %o; quería 600", perm)
	}
}

// Un .sock huérfano de un crash anterior no puede impedir el arranque.
// Nombre corto: mismo motivo que en TestSocketUnix0600, el límite de
// sockaddr_un.sun_path.
func TestSocketHuerfano(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m := session.NewManager(st, session.Config{})
	defer m.Close()

	sock := filepath.Join(dir, "d.sock")
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	srv := NewServer(m)
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(sock) }()
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	waitForSocket(t, sock)
	select {
	case err := <-errc:
		t.Fatalf("Serve falló con un socket huérfano: %v", err)
	default:
	}
}

// waitForSocket espera a que el socket acepte conexiones. Serve arranca en otra
// goroutine, así que sin esto el test corre antes de que exista.
func waitForSocket(t *testing.T, path string) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if c, err := net.Dial("unix", path); err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("el socket %s nunca aceptó conexiones", path)
}
