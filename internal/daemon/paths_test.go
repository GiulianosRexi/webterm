package daemon

import (
	"strings"
	"testing"
)

func TestPathsFor(t *testing.T) {
	casos := []struct {
		db                    string
		socket, lock, logPath string
	}{
		{
			db:      "/home/g/.webterm/webterm.db",
			socket:  "/home/g/.webterm/webterm.sock",
			lock:    "/home/g/.webterm/webterm.lock",
			logPath: "/home/g/.webterm/webterm.log",
		},
		{
			db:      "/home/g/.webterm/dev.db",
			socket:  "/home/g/.webterm/dev.sock",
			lock:    "/home/g/.webterm/dev.lock",
			logPath: "/home/g/.webterm/dev.log",
		},
		// Sin extensión: se le pegan los sufijos igual, no se le come nada.
		{
			db:      "/tmp/webterm",
			socket:  "/tmp/webterm.sock",
			lock:    "/tmp/webterm.lock",
			logPath: "/tmp/webterm.log",
		},
		// Relativo: se conserva relativo. Quien lo use resuelve.
		{
			db:      "webterm.db",
			socket:  "webterm.sock",
			lock:    "webterm.lock",
			logPath: "webterm.log",
		},
	}

	for _, c := range casos {
		got, err := PathsFor(c.db)
		if err != nil {
			t.Fatalf("PathsFor(%q) devolvió error inesperado: %v", c.db, err)
		}
		if got.Socket != c.socket || got.Lock != c.lock || got.Log != c.logPath {
			t.Errorf("PathsFor(%q) = %+v; quería %s / %s / %s",
				c.db, got, c.socket, c.lock, c.logPath)
		}
	}
}

// Dos bases distintas nunca comparten socket: es lo que aísla una instancia de
// desarrollo del daemon de producción.
func TestPathsForAislaBasesDistintas(t *testing.T) {
	prod, err := PathsFor("/home/g/.webterm/webterm.db")
	if err != nil {
		t.Fatal(err)
	}
	dev, err := PathsFor("/home/g/.webterm/dev.db")
	if err != nil {
		t.Fatal(err)
	}
	if prod.Socket == dev.Socket || prod.Lock == dev.Lock {
		t.Fatalf("dev y prod comparten rutas: %+v vs %+v", prod, dev)
	}
}

// Un -db cuyo socket derivado no entra en un sockaddr_un tiene que fallar acá,
// con un mensaje que diga el largo y el límite, y no más adelante como un
// "bind: invalid argument" desnudo que obliga a contar caracteres.
func TestPathsForRechazaSocketDemasiadoLargo(t *testing.T) {
	db := "/tmp/" + strings.Repeat("a", 120) + "/webterm.db"
	_, err := PathsFor(db)
	if err == nil {
		t.Fatal("se esperaba error por socket demasiado largo")
	}
}
