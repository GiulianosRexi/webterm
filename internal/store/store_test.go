package store

import (
	"path/filepath"
	"testing"
)

// newTestStore abre un store sobre una DB nueva en un directorio temporal.
// Sin mocks: los tests corren contra SQLite de verdad.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "sub", "webterm.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestOpenCreaSchema(t *testing.T) {
	st := newTestStore(t)

	var version int
	if err := st.DB().QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&version); err != nil {
		t.Fatalf("leyendo schema_version: %v", err)
	}
	if version != len(migrations) {
		t.Fatalf("schema_version = %d, se esperaba %d", version, len(migrations))
	}

	for _, table := range []string{"sessions", "session_kv", "session_output"} {
		var name string
		err := st.DB().QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name)
		if err != nil {
			t.Fatalf("falta la tabla %s: %v", table, err)
		}
	}
}

// TestOpenEsIdempotente: reabrir una DB ya migrada no vuelve a aplicar nada.
func TestOpenEsIdempotente(t *testing.T) {
	path := filepath.Join(t.TempDir(), "webterm.db")

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open 1: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close 1: %v", err)
	}

	st2, err := Open(path)
	if err != nil {
		t.Fatalf("Open 2: %v", err)
	}
	defer st2.Close()

	var rows int
	if err := st2.DB().QueryRow(`SELECT COUNT(*) FROM schema_version`).Scan(&rows); err != nil {
		t.Fatalf("contando migraciones: %v", err)
	}
	if rows != len(migrations) {
		t.Fatalf("se aplicaron %d migraciones, se esperaban %d", rows, len(migrations))
	}
}

// TestForeignKeysActivas: la cascada del DELETE depende del pragma, que en
// SQLite es por conexión. Si el pool abre una conexión sin él, los borrados
// dejan huérfanos en silencio.
func TestForeignKeysActivas(t *testing.T) {
	st := newTestStore(t)
	var on int
	if err := st.DB().QueryRow(`PRAGMA foreign_keys`).Scan(&on); err != nil {
		t.Fatalf("PRAGMA foreign_keys: %v", err)
	}
	if on != 1 {
		t.Fatal("foreign_keys está apagado")
	}
}
