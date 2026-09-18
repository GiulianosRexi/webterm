package control

import (
	"bytes"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/giuliano/webterm/internal/ptyapi"
	"github.com/giuliano/webterm/internal/session"
	"github.com/giuliano/webterm/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// newTestManager arma el orquestador con un manager de ptys EN PROCESO.
//
// Que los tests no levanten un daemon no es un atajo: es la propiedad que da
// ptyapi. El daemon de verdad ya está probado en internal/daemonclient contra
// un socket real; acá lo que se prueba es la lógica del orquestador.
func newTestManager(t *testing.T) (*Manager, *store.Store) {
	t.Helper()
	st := newTestStore(t)
	pty := session.NewManager(st, session.Config{HistoryBytes: 64 << 10})
	m := NewManager(st, pty, Config{Shell: "/bin/sh"})
	t.Cleanup(func() {
		_ = m.Close()
		_ = pty.Close()
		_ = st.Close()
	})
	return m, st
}

// waitForOutput drena el stream hasta encontrar la marca.
func waitForOutput(t *testing.T, att *Attachment, mark string) {
	t.Helper()
	var buf bytes.Buffer
	deadline := time.After(15 * time.Second)
	for {
		select {
		case chunk, ok := <-att.Output():
			if !ok {
				t.Fatalf("el stream cerró antes de %q; junté %q", mark, buf.String())
			}
			buf.Write(chunk)
			if bytes.Contains(buf.Bytes(), []byte(mark)) {
				return
			}
		case <-deadline:
			t.Fatalf("timeout esperando %q; junté %q", mark, buf.String())
		}
	}
}

// waitForClose drena hasta que el stream cierre, que es la señal de que la
// sesión terminó. No hace falta esperar a nada más: quien tiene los ptys marca
// la fila ANTES de cerrarle el canal a los clientes, así que cuando esto
// vuelve, la base ya dice exited.
func waitForClose(t *testing.T, att *Attachment) {
	t.Helper()
	deadline := time.After(15 * time.Second)
	for {
		select {
		case _, ok := <-att.Output():
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("timeout esperando que el stream cierre")
		}
	}
}

func tail(p []byte, n int) string {
	if len(p) > n {
		p = p[len(p)-n:]
	}
	return string(p)
}

func TestCreatePersisteYCorre(t *testing.T) {
	m, st := newTestManager(t)
	dir := t.TempDir()

	rec, err := m.Create(CreateOpts{Title: "una sesión", Cwd: dir, Cols: 80, Rows: 24})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if rec.ID == "" {
		t.Fatal("Create no asignó id")
	}

	got, err := st.GetSession(rec.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.PtyStatus != store.StatusRunning {
		t.Fatalf("pty_status = %q", got.PtyStatus)
	}
	if got.Title != "una sesión" || got.Cwd != dir {
		t.Fatalf("metadata mal guardada: %+v", got)
	}
}

func TestCreateDejaLaFilaEnRunning(t *testing.T) {
	m, st := newTestManager(t)
	rec, err := m.Create(CreateOpts{Title: "t", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.GetSession(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PtyStatus != store.StatusRunning {
		t.Fatalf("pty_status = %s; quería running", got.PtyStatus)
	}
	if got.Title != "t" {
		t.Fatalf("title = %q; quería t", got.Title)
	}
}

func TestAttachInexistente(t *testing.T) {
	m, _ := newTestManager(t)
	if _, err := m.Attach("no-existe"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Attach a inexistente dio %v; quería ErrNotFound", err)
	}
}

func TestUpdateMeta(t *testing.T) {
	m, _ := newTestManager(t)
	rec, err := m.Create(CreateOpts{Title: "vieja", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	updated := "nueva"
	got, err := m.UpdateMeta(rec.ID, store.MetaPatch{Title: &updated})
	if err != nil {
		t.Fatalf("UpdateMeta: %v", err)
	}
	if got.Title != "nueva" {
		t.Fatalf("title = %q", got.Title)
	}
}

// El KV es del orquestador y no pasa por el daemon, pero igual se cuelga de una
// sesión: sin fila no hay dónde colgarlo, y eso tiene que ser un 404 y no un
// error de foreign key.
func TestSetKVSobreSesionInexistente(t *testing.T) {
	m, _ := newTestManager(t)
	if err := m.SetKV("no-existe", "k", "v"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("SetKV sin fila dio %v; quería ErrNotFound", err)
	}
}

func TestDeleteBorraTodo(t *testing.T) {
	m, st := newTestManager(t)
	rec, err := m.Create(CreateOpts{Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetKV(rec.ID, "k", "v"); err != nil {
		t.Fatal(err)
	}

	att, err := m.Attach(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := att.Write([]byte("echo hola\n")); err != nil {
		t.Fatal(err)
	}
	waitForOutput(t, att, "hola")
	att.Detach()

	if err := m.Delete(rec.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := st.GetSession(rec.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("la fila sigue ahí: %v", err)
	}
	hist, _ := st.ReadOutput(rec.ID)
	if len(hist) != 0 {
		t.Fatalf("quedó historial huérfano: %d bytes", len(hist))
	}
	kv, _ := st.ListKV(rec.ID)
	if len(kv) != 0 {
		t.Fatalf("quedó KV huérfano: %v", kv)
	}
}

// spyPty anota el último SpawnOpts y delega en un manager de ptys de verdad.
// No reemplaza al pty: lo envuelve, porque lo que se quiere verificar es qué
// manda el orquestador, no qué hace el pty con eso.
type spyPty struct {
	ptyapi.Client
	mu   sync.Mutex
	last ptyapi.SpawnOpts
}

func (s *spyPty) Spawn(o ptyapi.SpawnOpts) error {
	s.mu.Lock()
	s.last = o
	s.mu.Unlock()
	return s.Client.Spawn(o)
}

func (s *spyPty) lastOpts() ptyapi.SpawnOpts {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

// El orquestador tiene que mandar siempre Shell y Cwd resueltos desde la fila.
// Si van vacíos, terminal.New cae al $SHELL y al $HOME del proceso que tiene
// los ptys —con el daemon aparte, el entorno equivocado— y la columna shell de
// la fila pasa a mentir sobre el proceso que realmente está corriendo.
func TestSpawnSiempreMandaShellYCwdDeLaFila(t *testing.T) {
	st := newTestStore(t)
	pty := session.NewManager(st, session.Config{HistoryBytes: 64 << 10})
	spy := &spyPty{Client: pty}
	m := NewManager(st, spy, Config{Shell: "/bin/sh"})
	t.Cleanup(func() {
		_ = m.Close()
		_ = pty.Close()
		_ = st.Close()
	})

	dir := t.TempDir()
	rec, err := m.Create(CreateOpts{Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}
	if o := spy.lastOpts(); o.Shell != "/bin/sh" || o.Cwd != dir {
		t.Fatalf("Create spawneó con shell=%q cwd=%q; quería /bin/sh y %q", o.Shell, o.Cwd, dir)
	}

	if err := m.Kill(rec.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Restart(rec.ID, 0, 0); err != nil {
		t.Fatal(err)
	}
	if o := spy.lastOpts(); o.Shell != rec.Shell || o.Cwd != rec.Cwd {
		t.Fatalf("Restart spawneó con shell=%q cwd=%q; quería los de la fila (%q, %q)",
			o.Shell, o.Cwd, rec.Shell, rec.Cwd)
	}
}

// Config.ExtraEnv es el mecanismo con el que se justifica el invariante de M10:
// sumarle una variable al entorno de cada pty —el token que el MCP necesita, por
// ejemplo— sin tocar el daemon, que no sabe ni tiene que saber qué es un token.
//
// El test va de punta a punta a propósito. Que SpawnOpts.Env llegue al entorno
// del pty ya está probado en internal/session; lo que no estaba probado es la
// unión, que es justamente el cable que se puede borrar sin que nada se ponga
// rojo: el agente que corre adentro del pty se quedaría sin token en silencio.
func TestExtraEnvLlegaAlEntornoDelPty(t *testing.T) {
	st := newTestStore(t)
	pty := session.NewManager(st, session.Config{HistoryBytes: 64 << 10})
	spy := &spyPty{Client: pty}
	m := NewManager(st, spy, Config{
		Shell:    "/bin/sh",
		ExtraEnv: []string{"WEBTERM_TEST_TOKEN=valor-del-token"},
	})
	t.Cleanup(func() {
		_ = m.Close()
		_ = pty.Close()
		_ = st.Close()
	})

	rec, err := m.Create(CreateOpts{Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if env := spy.lastOpts().Env; len(env) != 1 || env[0] != "WEBTERM_TEST_TOKEN=valor-del-token" {
		t.Fatalf("el SpawnOpts salió con Env=%v; quería el ExtraEnv de la Config", env)
	}

	att, err := m.Attach(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer att.Detach()
	// El corchete evita confundir el eco del propio comando con su salida: lo
	// que se tipea lleva el nombre de la variable, no su valor.
	if err := att.Write([]byte("echo \"TOKEN=[$WEBTERM_TEST_TOKEN]\"\n")); err != nil {
		t.Fatal(err)
	}
	waitForOutput(t, att, "TOKEN=[valor-del-token]")
}

// El cwd resuelto tiene que quedar siempre en la fila y en el SpawnOpts. Si se
// va vacío, quien tiene los ptys cae a su propio $HOME —con el daemon aparte,
// el equivocado— y la columna cwd pasa a mentir sobre dónde corre el proceso.
func TestCreateSinHomeNoDejaElCwdVacio(t *testing.T) {
	// Sin HOME, os.UserHomeDir falla: es la única forma de provocar el caso.
	t.Setenv("HOME", "")

	m, st := newTestManager(t)
	rec, err := m.Create(CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.GetSession(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Cwd != "/" {
		t.Fatalf("cwd = %q; quería / (el fallback honesto)", got.Cwd)
	}
}
