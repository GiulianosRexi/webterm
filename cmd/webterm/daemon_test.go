package main

import (
	"flag"
	"io"
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
func TestEnsureDaemonLevantaUnoYReusaElQueYaEstaba(t *testing.T) {
	if testing.Short() {
		t.Skip("levanta un proceso de verdad")
	}
	bin := buildBinary(t)

	dir := shortTempDir(t)
	dbPath := filepath.Join(dir, "test.db")
	paths, err := daemon.PathsFor(dbPath)
	if err != nil {
		t.Fatalf("PathsFor falló: %v", err)
	}

	cl, err := ensureDaemonWith(bin, paths, dbPath, session.DefaultHistoryBytes)
	if err != nil {
		// El log del daemon va en el mensaje: el proceso no tiene terminal, así
		// que sin esto un fallo acá no dice absolutamente nada de por qué el
		// daemon no llegó a contestar.
		daemonLog, _ := os.ReadFile(paths.Log)
		t.Fatalf("ensureDaemon falló: %v\nlog del daemon:\n%s", err, daemonLog)
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

// shortTempDir es t.TempDir() sin el nombre del test adentro del path.
//
// t.TempDir() mete el nombre del test en la ruta, y sumado al TMPDIR de macOS
// —largo de por sí— un nombre descriptivo hacía que el .sock derivado pasara
// los 103 bytes de sockaddr_un.sun_path. Antes eso se pagaba acortando el
// nombre del test; se paga mejor acortando el path, que es lo que sobra.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "wt")
	if err != nil {
		t.Fatalf("no se pudo crear el temporal: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// El subcomando de `webterm daemon` tiene que reconocerse antes o después de
// los flags. Antes solo valía pegado a "daemon", así que
// `webterm daemon -db X status` arrancaba el daemon en foreground y te
// bloqueaba la terminal en vez de contestar un pid.
func TestParseDaemonArgsEncuentraElSubcomandoEnCualquierPosicion(t *testing.T) {
	casos := []struct {
		nombre string
		args   []string
		sub    string
		db     string
		fallo  bool
	}{
		{nombre: "sin nada", args: nil, sub: "", db: "d.db"},
		{nombre: "subcomando adelante", args: []string{"status", "-db", "x.db"}, sub: "status", db: "x.db"},
		{nombre: "subcomando atrás", args: []string{"-db", "x.db", "status"}, sub: "status", db: "x.db"},
		{nombre: "solo flags", args: []string{"-db", "x.db"}, sub: "", db: "x.db"},
		// "status" acá es el VALOR de -db, no un subcomando: distinguirlo es lo
		// que un escaneo a mano de os.Args no puede hacer.
		{nombre: "el valor de un flag no es subcomando", args: []string{"-db", "status"}, sub: "", db: "status"},
		{nombre: "argumentos de más", args: []string{"status", "sobra"}, fallo: true},
	}
	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			db := fs.String("db", "d.db", "")
			sub, err := parseDaemonArgs(fs, c.args)
			if c.fallo {
				if err == nil {
					t.Fatalf("no falló: sub=%q", sub)
				}
				return
			}
			if err != nil {
				t.Fatalf("falló: %v", err)
			}
			if sub != c.sub {
				t.Fatalf("subcomando = %q; quería %q", sub, c.sub)
			}
			if *db != c.db {
				t.Fatalf("-db = %q; quería %q", *db, c.db)
			}
		})
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
