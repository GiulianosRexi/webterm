package session

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/giuliano/webterm/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "webterm.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func seedSession(t *testing.T, st *store.Store, id string) {
	t.Helper()
	err := st.CreateSession(&store.Session{
		ID: id, Cwd: "/tmp", Shell: "/bin/bash", Cols: 80, Rows: 24,
		PtyStatus: store.StatusRunning,
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
}

// TestWriterPersisteAlCerrar: close() tiene que dejar el historial completo.
// El reaper lo llama antes de marcar la sesión como muerta, así que si el
// flush final se pierde, se pierden los últimos segundos de la sesión.
func TestWriterPersisteAlCerrar(t *testing.T) {
	st := newTestStore(t)
	seedSession(t, st, "s1")

	w := newOutputWriter(st, "s1", 1<<20)
	w.write([]byte("hola "))
	w.write([]byte("mundo"))
	w.close()

	got, err := st.ReadOutput("s1")
	if err != nil {
		t.Fatalf("ReadOutput: %v", err)
	}
	if string(got) != "hola mundo" {
		t.Fatalf("historial = %q", got)
	}
}

// TestWriterPodaAlCap: el historial en la base no crece sin límite.
func TestWriterPodaAlCap(t *testing.T) {
	st := newTestStore(t)
	seedSession(t, st, "s1")

	w := newOutputWriter(st, "s1", 200)
	for i := 0; i < 10; i++ {
		w.write(bytes.Repeat([]byte{byte('a' + i)}, 100))
		w.flushNow()
	}
	w.close()

	got, _ := st.ReadOutput("s1")
	if len(got) > 200 {
		t.Fatalf("quedaron %d bytes con un cap de 200", len(got))
	}
	if !bytes.Contains(got, bytes.Repeat([]byte("j"), 100)) {
		t.Fatal("se podó el chunk más nuevo")
	}
}

// TestWriterCopiaElChunk: igual que el ring, el writer no puede quedarse con
// el buffer que reusa el lector del pty.
func TestWriterCopiaElChunk(t *testing.T) {
	st := newTestStore(t)
	seedSession(t, st, "s1")

	w := newOutputWriter(st, "s1", 1<<20)
	buf := []byte("hola")
	w.write(buf)
	copy(buf, "chau")
	w.close()

	got, _ := st.ReadOutput("s1")
	if string(got) != "hola" {
		t.Fatalf("historial = %q", got)
	}
}

func TestWriterCloseEsIdempotente(t *testing.T) {
	st := newTestStore(t)
	seedSession(t, st, "s1")

	w := newOutputWriter(st, "s1", 1<<20)
	w.write([]byte("x"))
	w.close()
	w.close()
	// Escribir después de cerrar no tiene que panickear ni persistir nada.
	w.write([]byte("tarde"))

	got, _ := st.ReadOutput("s1")
	if string(got) != "x" {
		t.Fatalf("historial = %q", got)
	}
}
