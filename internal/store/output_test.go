package store

import (
	"bytes"
	"testing"
)

func TestAppendYReadOutput(t *testing.T) {
	st := newTestStore(t)
	_ = st.CreateSession(sampleSession("s1"))

	for _, chunk := range []string{"hola ", "mundo", "!\n"} {
		if err := st.AppendOutput("s1", []byte(chunk)); err != nil {
			t.Fatalf("AppendOutput: %v", err)
		}
	}

	got, err := st.ReadOutput("s1")
	if err != nil {
		t.Fatalf("ReadOutput: %v", err)
	}
	if string(got) != "hola mundo!\n" {
		t.Fatalf("ReadOutput = %q", got)
	}

	n, err := st.OutputBytes("s1")
	if err != nil {
		t.Fatalf("OutputBytes: %v", err)
	}
	if n != int64(len("hola mundo!\n")) {
		t.Fatalf("OutputBytes = %d", n)
	}
}

func TestReadOutputVacio(t *testing.T) {
	st := newTestStore(t)
	_ = st.CreateSession(sampleSession("s1"))

	got, err := st.ReadOutput("s1")
	if err != nil {
		t.Fatalf("ReadOutput: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("una sesión sin output tiene que dar vacío, dio %q", got)
	}
}

// TestPruneOutputRespetaElCap: la poda deja el tail dentro del cap y conserva
// siempre lo más nuevo, que es lo que el cliente necesita para redibujar.
func TestPruneOutputRespetaElCap(t *testing.T) {
	st := newTestStore(t)
	_ = st.CreateSession(sampleSession("s1"))

	// 10 chunks de 100 bytes: 1000 en total.
	for i := 0; i < 10; i++ {
		chunk := bytes.Repeat([]byte{byte('a' + i)}, 100)
		if err := st.AppendOutput("s1", chunk); err != nil {
			t.Fatalf("AppendOutput %d: %v", i, err)
		}
	}

	if err := st.PruneOutput("s1", 250); err != nil {
		t.Fatalf("PruneOutput: %v", err)
	}

	got, err := st.ReadOutput("s1")
	if err != nil {
		t.Fatalf("ReadOutput: %v", err)
	}
	if len(got) > 250 {
		t.Fatalf("quedaron %d bytes, el cap es 250", len(got))
	}
	if len(got) < 100 {
		t.Fatalf("la poda dejó %d bytes: se comió el tail", len(got))
	}
	// Lo último escrito ('j') tiene que seguir ahí; lo primero ('a'), no.
	if !bytes.Contains(got, bytes.Repeat([]byte("j"), 100)) {
		t.Fatalf("se podó el chunk más nuevo: %q", got[:min(40, len(got))])
	}
	if bytes.Contains(got, bytes.Repeat([]byte("a"), 100)) {
		t.Fatal("quedó el chunk más viejo: no se podó nada")
	}
}

// TestPruneOutputNuncaBorraTodo: un solo chunk más grande que el cap se
// conserva igual. Borrarlo dejaría al cliente sin nada que redibujar.
func TestPruneOutputNuncaBorraTodo(t *testing.T) {
	st := newTestStore(t)
	_ = st.CreateSession(sampleSession("s1"))

	if err := st.AppendOutput("s1", bytes.Repeat([]byte("x"), 500)); err != nil {
		t.Fatalf("AppendOutput: %v", err)
	}
	if err := st.PruneOutput("s1", 100); err != nil {
		t.Fatalf("PruneOutput: %v", err)
	}
	got, _ := st.ReadOutput("s1")
	if len(got) != 500 {
		t.Fatalf("se perdió el único chunk: quedaron %d bytes", len(got))
	}
}

func TestPruneOutputNoTocaOtrasSesiones(t *testing.T) {
	st := newTestStore(t)
	_ = st.CreateSession(sampleSession("s1"))
	_ = st.CreateSession(sampleSession("s2"))

	for i := 0; i < 10; i++ {
		_ = st.AppendOutput("s1", bytes.Repeat([]byte("a"), 100))
		_ = st.AppendOutput("s2", bytes.Repeat([]byte("b"), 100))
	}
	if err := st.PruneOutput("s1", 150); err != nil {
		t.Fatalf("PruneOutput: %v", err)
	}

	otra, _ := st.ReadOutput("s2")
	if len(otra) != 1000 {
		t.Fatalf("la poda de s1 tocó a s2: quedaron %d bytes", len(otra))
	}
}

func TestOutputCascadaAlBorrar(t *testing.T) {
	st := newTestStore(t)
	_ = st.CreateSession(sampleSession("s1"))
	_ = st.AppendOutput("s1", []byte("algo"))

	if err := st.DeleteSession("s1"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	var n int
	_ = st.DB().QueryRow(`SELECT COUNT(*) FROM session_output WHERE session_id = ?`, "s1").Scan(&n)
	if n != 0 {
		t.Fatalf("quedaron %d chunks huérfanos", n)
	}
}

// TestAppendOutputIgnoraVacios evita ensuciar la tabla con filas de 0 bytes.
func TestAppendOutputIgnoraVacios(t *testing.T) {
	st := newTestStore(t)
	_ = st.CreateSession(sampleSession("s1"))

	if err := st.AppendOutput("s1", nil); err != nil {
		t.Fatalf("AppendOutput(nil): %v", err)
	}
	var n int
	_ = st.DB().QueryRow(`SELECT COUNT(*) FROM session_output`).Scan(&n)
	if n != 0 {
		t.Fatalf("se insertó una fila vacía (%d filas)", n)
	}
}
