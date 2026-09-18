package daemon

import "testing"

func TestPathsFor(t *testing.T) {
	casos := []struct {
		db                     string
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
		got := PathsFor(c.db)
		if got.Socket != c.socket || got.Lock != c.lock || got.Log != c.logPath {
			t.Errorf("PathsFor(%q) = %+v; quería %s / %s / %s",
				c.db, got, c.socket, c.lock, c.logPath)
		}
	}
}

// Dos bases distintas nunca comparten socket: es lo que aísla una instancia de
// desarrollo del daemon de producción.
func TestPathsForAislaBasesDistintas(t *testing.T) {
	prod := PathsFor("/home/g/.webterm/webterm.db")
	dev := PathsFor("/home/g/.webterm/dev.db")
	if prod.Socket == dev.Socket || prod.Lock == dev.Lock {
		t.Fatalf("dev y prod comparten rutas: %+v vs %+v", prod, dev)
	}
}
