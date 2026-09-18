package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/giuliano/webterm/internal/daemon"
	"github.com/giuliano/webterm/internal/session"
)

// El arranque on-demand tiene que ser capaz de levantar un daemon de cero y
// que el cliente le hable. Es el camino que corre cada vez que arrancás
// webterm con el daemon caído.
//
// Nombre corto a propósito: t.TempDir() arma el path del temporal con el
// nombre del test adentro, y sumado al TMPDIR de macOS (largo de por sí)
// un nombre más descriptivo hace que el .sock derivado supere los ~103 bytes
// de sockaddr_un.sun_path. Mismo motivo documentado en
// internal/daemon/server_test.go.
func TestEnsureDaemon(t *testing.T) {
	if testing.Short() {
		t.Skip("levanta un proceso de verdad")
	}
	bin := buildBinary(t)

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	paths, err := daemon.PathsFor(dbPath)
	if err != nil {
		t.Fatalf("PathsFor falló: %v", err)
	}

	cl, err := ensureDaemonWith(bin, paths, dbPath, session.DefaultHistoryBytes)
	if err != nil {
		t.Fatalf("ensureDaemon falló: %v", err)
	}
	t.Cleanup(func() {
		_ = cl.Close()
		stopDaemon(paths)
	})

	if err := cl.Check(); err != nil {
		t.Fatalf("Check falló contra el daemon recién levantado: %v", err)
	}

	// Una segunda llamada tiene que reusar el mismo daemon, no levantar otro.
	info1, _ := cl.Info()
	cl2, err := ensureDaemonWith(bin, paths, dbPath, session.DefaultHistoryBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer cl2.Close()
	info2, _ := cl2.Info()
	if info1.PID != info2.PID {
		t.Fatalf("levantó un segundo daemon: pid %d vs %d", info1.PID, info2.PID)
	}

	// Y el log tiene que existir: es donde va a parar todo lo que el daemon
	// diga, porque no tiene terminal.
	if _, err := os.Stat(paths.Log); err != nil {
		t.Fatalf("no se creó el log del daemon: %v", err)
	}
}

// buildBinary compila el binario en un temporal. No se puede usar os.Executable
// desde un test porque apunta al binario del test.
func buildBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "webterm")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("compilando: %v\n%s", err, out)
	}
	return bin
}
