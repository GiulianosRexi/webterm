package server

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadOrCreateTokenGeneraYPersiste(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")

	primero, err := LoadOrCreateToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if primero == "" {
		t.Fatal("no generó token")
	}

	// La segunda llamada tiene que devolver el mismo: si no, cada reinicio
	// invalidaría el WEBTERM_TOKEN de los ptys que siguen vivos.
	segundo, err := LoadOrCreateToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if segundo != primero {
		t.Fatalf("el token cambió entre llamadas: %q vs %q", primero, segundo)
	}
}

func TestLoadOrCreateTokenUsaPermisosRestrictivos(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if _, err := LoadOrCreateToken(path); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("permisos %o; quería 600 (el token da shell)", perm)
	}
}

func TestLoadOrCreateTokenIgnoraEspaciosYArchivoVacio(t *testing.T) {
	dir := t.TempDir()

	conEspacios := filepath.Join(dir, "a")
	if err := os.WriteFile(conEspacios, []byte("  secreto\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadOrCreateToken(conEspacios)
	if err != nil {
		t.Fatal(err)
	}
	if got != "secreto" {
		t.Fatalf("token = %q; quería %q", got, "secreto")
	}

	// Un archivo vacío es basura, no un token: hay que regenerarlo en vez de
	// arrancar con auth efectivamente desactivada.
	vacio := filepath.Join(dir, "b")
	if err := os.WriteFile(vacio, []byte("\n  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = LoadOrCreateToken(vacio)
	if err != nil {
		t.Fatal(err)
	}
	if got == "" {
		t.Fatal("con el archivo vacío tendría que haber generado uno nuevo")
	}
}

func TestLoadOrCreateTokenCreaElDirectorio(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "dir", "token")
	if _, err := LoadOrCreateToken(path); err != nil {
		t.Fatalf("tendría que crear el directorio: %v", err)
	}
}
