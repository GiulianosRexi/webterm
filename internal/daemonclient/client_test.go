package daemonclient

import (
	"bytes"
	"context"
	"errors"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/giuliano/webterm/internal/daemon"
	"github.com/giuliano/webterm/internal/ptyapi"
	"github.com/giuliano/webterm/internal/session"
	"github.com/giuliano/webterm/internal/store"
)

// newPair levanta un daemon de verdad sobre un socket temporal y devuelve un
// cliente apuntado a él. Sin mocks: lo que se está probando es exactamente que
// los dos lados hablen el mismo protocolo.
func newPair(t *testing.T) (*Client, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	m := session.NewManager(st, session.Config{HistoryBytes: 64 << 10})
	srv := daemon.NewServer(m)
	sock := filepath.Join(dir, "d.sock")
	go func() { _ = srv.Serve(sock) }()

	for i := 0; i < 200; i++ {
		if c, err := net.Dial("unix", sock); err == nil {
			_ = c.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	cl := New(sock)
	t.Cleanup(func() {
		_ = cl.Close()
		_ = srv.Shutdown(context.Background())
		_ = m.Close()
		_ = st.Close()
	})
	return cl, st
}

func filaNueva(t *testing.T, st *store.Store) *store.Session {
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

func TestCheckAceptaLaMismaVersion(t *testing.T) {
	cl, _ := newPair(t)
	if err := cl.Check(); err != nil {
		t.Fatalf("Check contra un daemon de la misma versión falló: %v", err)
	}
}

func TestClienteImplementaElContrato(t *testing.T) {
	var _ ptyapi.Client = (*Client)(nil)
}

func TestSpawnAttachEscribirYLeer(t *testing.T) {
	cl, st := newPair(t)
	rec := filaNueva(t, st)

	if err := cl.Spawn(ptyapi.SpawnOpts{
		ID: rec.ID, Shell: rec.Shell, Cwd: rec.Cwd, Cols: 80, Rows: 24,
	}); err != nil {
		t.Fatal(err)
	}

	att, err := cl.Attach(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer att.Detach()

	if err := att.Write([]byte("echo MARCA-REMOTA\n")); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	deadline := time.After(5 * time.Second)
	for !bytes.Contains(buf.Bytes(), []byte("MARCA-REMOTA")) {
		select {
		case chunk, ok := <-att.Output():
			if !ok {
				t.Fatalf("el stream cerró antes de la marca: %q", buf.String())
			}
			buf.Write(chunk)
		case <-deadline:
			t.Fatalf("timeout; junté %q", buf.String())
		}
	}
}

// El historial llega ANTES del primer chunk vivo y como History(), no como
// output: si no, el orquestador no sabría dónde termina el replay.
func TestAttachTraeElHistorialAparte(t *testing.T) {
	cl, st := newPair(t)
	rec := filaNueva(t, st)
	if err := cl.Spawn(ptyapi.SpawnOpts{
		ID: rec.ID, Shell: rec.Shell, Cwd: rec.Cwd, Cols: 80, Rows: 24,
		Banner: "MARCA-BANNER",
	}); err != nil {
		t.Fatal(err)
	}

	att, err := cl.Attach(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer att.Detach()

	if !bytes.Contains(att.History(), []byte("MARCA-BANNER")) {
		t.Fatalf("History() = %q; quería el banner adentro", att.History())
	}
}

func TestLiveIDsYKill(t *testing.T) {
	cl, st := newPair(t)
	rec := filaNueva(t, st)
	if err := cl.Spawn(ptyapi.SpawnOpts{
		ID: rec.ID, Shell: rec.Shell, Cwd: rec.Cwd, Cols: 80, Rows: 24,
	}); err != nil {
		t.Fatal(err)
	}

	ids, err := cl.LiveIDs()
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != rec.ID {
		t.Fatalf("LiveIDs = %v; quería [%s]", ids, rec.ID)
	}

	if err := cl.Kill(rec.ID); err != nil {
		t.Fatal(err)
	}
	ids, _ = cl.LiveIDs()
	if len(ids) != 0 {
		t.Fatalf("después del kill LiveIDs = %v; quería vacío", ids)
	}
}

// Tres de los cuatro errores del contrato tienen que sobrevivir el viaje por
// el socket: el orquestador reacciona distinto a cada uno. El cuarto,
// ErrClosed, lo prueba aparte TestErrClosedViaja porque necesita un manager
// cerrado ANTES de levantar el servidor, y no encaja en newPair.
func TestLosErroresDelContratoViajan(t *testing.T) {
	cl, st := newPair(t)

	// Fila inexistente -> ErrNotFound.
	err := cl.Spawn(ptyapi.SpawnOpts{ID: "no-existe", Shell: "/bin/sh", Cwd: t.TempDir(), Cols: 80, Rows: 24})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("spawn sin fila dio %v; quería ErrNotFound", err)
	}

	// Fila sin proceso -> ErrNotLive.
	rec := filaNueva(t, st)
	if _, err := cl.Attach(rec.ID); !errors.Is(err, ptyapi.ErrNotLive) {
		t.Fatalf("attach a fila sin proceso dio %v; quería ErrNotLive", err)
	}
	if err := cl.Kill(rec.ID); !errors.Is(err, ptyapi.ErrNotLive) {
		t.Fatalf("kill a fila sin proceso dio %v; quería ErrNotLive", err)
	}

	// Ya corriendo -> ErrAlreadyLive.
	opts := ptyapi.SpawnOpts{ID: rec.ID, Shell: rec.Shell, Cwd: rec.Cwd, Cols: 80, Rows: 24}
	if err := cl.Spawn(opts); err != nil {
		t.Fatal(err)
	}
	if err := cl.Spawn(opts); !errors.Is(err, ptyapi.ErrAlreadyLive) {
		t.Fatalf("spawn duplicado dio %v; quería ErrAlreadyLive", err)
	}
}

// Cuando la sesión muere, el canal de output se cierra. Es la señal sobre la
// que el orquestador arma el frame exit que ve el browser.
func TestOutputSeCierraAlMorir(t *testing.T) {
	cl, st := newPair(t)
	rec := filaNueva(t, st)
	if err := cl.Spawn(ptyapi.SpawnOpts{
		ID: rec.ID, Shell: rec.Shell, Cwd: rec.Cwd, Cols: 80, Rows: 24,
	}); err != nil {
		t.Fatal(err)
	}
	att, err := cl.Attach(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer att.Detach()

	if err := att.Write([]byte("exit\n")); err != nil {
		t.Fatal(err)
	}

	cerrado := make(chan struct{})
	go func() {
		for range att.Output() {
		}
		close(cerrado)
	}()
	select {
	case <-cerrado:
	case <-time.After(5 * time.Second):
		t.Fatal("el canal de output no se cerró al morir la sesión")
	}
}

// El cuarto error del contrato: ErrClosed viaja como 503 cuando el dueño de
// los ptys se está apagando. Se arma el daemon a mano en vez de usar newPair
// porque hace falta cerrar el manager ANTES de levantar el server (newPair
// deja el Close para el t.Cleanup, que corre al final del test).
func TestErrClosedViaja(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rec := filaNueva(t, st)

	m := session.NewManager(st, session.Config{})
	// Close deja al manager en el estado "apagándose": Spawn lo detecta y
	// devuelve ErrClosed antes de tocar nada más.
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}

	srv := daemon.NewServer(m)
	sock := filepath.Join(dir, "d2.sock")
	go func() { _ = srv.Serve(sock) }()
	defer func() { _ = srv.Shutdown(context.Background()) }()

	for i := 0; i < 200; i++ {
		if c, err := net.Dial("unix", sock); err == nil {
			_ = c.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	cl := New(sock)
	defer cl.Close()

	err = cl.Spawn(ptyapi.SpawnOpts{ID: rec.ID, Shell: rec.Shell, Cwd: rec.Cwd, Cols: 80, Rows: 24})
	if !errors.Is(err, ptyapi.ErrClosed) {
		t.Fatalf("spawn con el dueño de los ptys apagándose dio %v; quería ErrClosed", err)
	}
}

func TestSinDaemonFallaClaro(t *testing.T) {
	cl := New(filepath.Join(t.TempDir(), "no-existe.sock"))
	defer cl.Close()

	if err := cl.Check(); err == nil {
		t.Fatal("sin daemon del otro lado Check tendría que fallar")
	}
}
