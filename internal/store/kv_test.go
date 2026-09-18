package store

import (
	"errors"
	"testing"
)

func TestKVSetGetList(t *testing.T) {
	st := newTestStore(t)
	_ = st.CreateSession(sampleSession("s1"))

	if err := st.SetKV("s1", "claude_session_id", "abc-123"); err != nil {
		t.Fatalf("SetKV: %v", err)
	}
	// Sobrescribir una clave existente es un upsert, no un error.
	if err := st.SetKV("s1", "claude_session_id", "def-456"); err != nil {
		t.Fatalf("SetKV (upsert): %v", err)
	}
	if err := st.SetKV("s1", "work_status", "working"); err != nil {
		t.Fatalf("SetKV: %v", err)
	}

	v, err := st.GetKV("s1", "claude_session_id")
	if err != nil {
		t.Fatalf("GetKV: %v", err)
	}
	if v != "def-456" {
		t.Fatalf("GetKV = %q", v)
	}

	all, err := st.ListKV("s1")
	if err != nil {
		t.Fatalf("ListKV: %v", err)
	}
	if len(all) != 2 || all["work_status"] != "working" {
		t.Fatalf("ListKV = %v", all)
	}
}

func TestKVErrores(t *testing.T) {
	st := newTestStore(t)
	_ = st.CreateSession(sampleSession("s1"))

	if _, err := st.GetKV("s1", "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetKV de clave inexistente: %v", err)
	}
	if err := st.DeleteKV("s1", "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteKV de clave inexistente: %v", err)
	}
	// Sin la foreign key, esto guardaría KV colgado de una sesión fantasma.
	if err := st.SetKV("no-existe", "k", "v"); err == nil {
		t.Fatal("SetKV sobre una sesión inexistente tiene que fallar")
	}
}

// TestKVCascadaAlBorrar: borrar la sesión se lleva su KV.
func TestKVCascadaAlBorrar(t *testing.T) {
	st := newTestStore(t)
	_ = st.CreateSession(sampleSession("s1"))
	_ = st.SetKV("s1", "k", "v")

	if err := st.DeleteSession("s1"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM session_kv WHERE session_id = ?`, "s1").Scan(&n); err != nil {
		t.Fatalf("contando kv: %v", err)
	}
	if n != 0 {
		t.Fatalf("quedaron %d filas de kv huérfanas", n)
	}
}
