package store

import (
	"database/sql"
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

// TestMigracionSobreBaseV1: una base que quedó en v1 —la que ya tenías
// corriendo antes de M8— se migra sin perder nada. Es el caso que se rompe en
// silencio si una migración nueva está mal escrita.
func TestMigracionSobreBaseV1(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v1.db")

	// Armamos a mano una base en v1, como la dejaría la versión anterior.
	raw, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatalf("abriendo: %v", err)
	}
	if _, err := raw.Exec(`CREATE TABLE schema_version (version INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("schema_version: %v", err)
	}
	if _, err := raw.Exec(schemaV1); err != nil {
		t.Fatalf("schemaV1: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO schema_version (version) VALUES (1)`); err != nil {
		t.Fatalf("marcando v1: %v", err)
	}
	if _, err := raw.Exec(`
		INSERT INTO sessions (id, title, description, cwd, shell, cols, rows,
			pty_status, work_status, kanban_status, created_at, last_active_at)
		VALUES ('vieja', 'de antes', '', '/tmp', '/bin/bash', 80, 24,
			'exited', 'idle', 'todo', 1, 1)`); err != nil {
		t.Fatalf("sesión previa: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("cerrando: %v", err)
	}

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open sobre una base v1: %v", err)
	}
	defer st.Close()

	var v int
	if err := st.DB().QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&v); err != nil {
		t.Fatalf("leyendo versión: %v", err)
	}
	if v != len(migrations) {
		t.Fatalf("quedó en v%d, se esperaba v%d", v, len(migrations))
	}

	// Los datos de antes siguen ahí.
	got, err := st.GetSession("vieja")
	if err != nil {
		t.Fatalf("la sesión previa no sobrevivió: %v", err)
	}
	if got.Title != "de antes" {
		t.Fatalf("se corrompió la fila: %+v", got)
	}

	// Y la tabla nueva quedó usable.
	if err := st.AddResource(&Resource{
		SessionID: "vieja", System: "gh", Type: "pr",
		Ref:       "https://github.com/o/r/pull/1",
	}); err != nil {
		t.Fatalf("la tabla nueva no quedó usable: %v", err)
	}
}
