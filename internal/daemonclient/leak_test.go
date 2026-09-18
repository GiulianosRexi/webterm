package daemonclient

import (
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/giuliano/webterm/internal/ptyapi"
)

// waitNoLeak sondea runtime.NumGoroutine() hasta que vuelve a estar cerca de
// como estaba antes de la operación bajo prueba, o se cumple el timeout.
//
// El margen chico (no cero) es porque el runtime y el transport HTTP tienen
// goroutines de fondo (GC, keepalive) que suben y bajan solas y no dependen de
// lo que se está probando; pedir el número exacto haría el test frágil sin
// hacerlo más estricto donde importa: que no crezca sin límite.
func waitNoLeak(t *testing.T, before int) {
	t.Helper()
	const margin = 3
	deadline := time.Now().Add(3 * time.Second)
	for {
		runtime.GC()
		after := runtime.NumGoroutine()
		if after <= before+margin {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutines no volvieron a bajar: antes=%d, después=%d", before, after)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Estos cinco tests son la contraparte permanente del ad-hoc que se corrió a
// mano para verificar la tarea 8 (100 ciclos spawn/attach/detach/kill, sin
// dejar rastro en el repo). El ad-hoc cubría cuatro caminos y no el quinto
// —el consumidor que abandona sin Detach()— que era justo el que tenía el
// leak real (I2 del review de ronda 1). Quedan los cinco acá para que
// cualquier cambio futuro al bombeo los vuelva a ejercer.

// Camino 1: el daemon nunca contestó. pump ni arranca —Attach falla antes—,
// así que no debería haber nada que limpiar.
func TestPumpSaleSiElDaemonNoEstaAhi(t *testing.T) {
	runtime.GC()
	before := runtime.NumGoroutine()

	cl := New(filepath.Join(t.TempDir(), "no-existe.sock"))
	defer cl.Close()
	if _, err := cl.Attach("cualquiera"); err == nil {
		t.Fatal("attach sin daemon tendría que fallar")
	}

	waitNoLeak(t, before)
}

// Camino 2: el consumidor se va prolijo, llamando Detach().
func TestPumpSaleAlDetach(t *testing.T) {
	cl, st := newPair(t)
	rec := filaNueva(t, st)
	if err := cl.Spawn(ptyapi.SpawnOpts{ID: rec.ID, Shell: rec.Shell, Cwd: rec.Cwd, Cols: 80, Rows: 24}); err != nil {
		t.Fatal(err)
	}

	runtime.GC()
	before := runtime.NumGoroutine()

	att, err := cl.Attach(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	att.Detach()

	waitNoLeak(t, before)
}

// Camino 3: el shell sale prolijo (exit normal, código 0).
func TestPumpSaleAlMorir(t *testing.T) {
	cl, st := newPair(t)
	rec := filaNueva(t, st)
	if err := cl.Spawn(ptyapi.SpawnOpts{ID: rec.ID, Shell: rec.Shell, Cwd: rec.Cwd, Cols: 80, Rows: 24}); err != nil {
		t.Fatal(err)
	}

	runtime.GC()
	before := runtime.NumGoroutine()

	att, err := cl.Attach(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer att.Detach()
	if err := att.Write([]byte("exit\n")); err != nil {
		t.Fatal(err)
	}
	for range att.Output() {
	}

	waitNoLeak(t, before)
}

// Camino 4: a alguien (otro cliente, o el sweep del orquestador) lo mata con
// Kill mientras este cliente sigue attacheado. Es un camino de reconciliación
// distinto del 3 del lado del manager (Kill() manda la señal y espera el
// reap; el shell saliendo solo es la reconciliación pasiva del lector del
// pty), aunque de este lado del socket ambos terminan viéndose igual: un
// error de lectura cuando el daemon cierra la conexión.
func TestPumpSaleSiMatanElProceso(t *testing.T) {
	cl, st := newPair(t)
	rec := filaNueva(t, st)
	if err := cl.Spawn(ptyapi.SpawnOpts{ID: rec.ID, Shell: rec.Shell, Cwd: rec.Cwd, Cols: 80, Rows: 24}); err != nil {
		t.Fatal(err)
	}

	runtime.GC()
	before := runtime.NumGoroutine()

	att, err := cl.Attach(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer att.Detach()

	if err := cl.Kill(rec.ID); err != nil {
		t.Fatal(err)
	}
	for range att.Output() {
	}

	waitNoLeak(t, before)
}

// Camino 5: el consumidor abandona el attachment sin llamar Detach() y sin
// seguir leyendo Output(). Es el que I2 dejaba colgado para siempre: con el
// buffer de salida lleno, pump se bloqueaba en el send porque su único otro
// caso de salida era Detach(), que nadie iba a llamar.
//
// Se usa el hook interno attach() con buffer de 1 en vez de Attach(): con el
// buffer de producción (256) haría falta generar cientos de frames reales
// para tocar la misma rama, y el punto de este test es la rama, no el volumen.
func TestPumpSaleSiAbandonan(t *testing.T) {
	cl, st := newPair(t)
	rec := filaNueva(t, st)
	if err := cl.Spawn(ptyapi.SpawnOpts{ID: rec.ID, Shell: rec.Shell, Cwd: rec.Cwd, Cols: 80, Rows: 24}); err != nil {
		t.Fatal(err)
	}

	runtime.GC()
	before := runtime.NumGoroutine()

	att, err := cl.attach(rec.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := att.Write([]byte("yes\n")); err != nil {
		t.Fatal(err)
	}

	// Nunca leemos Output() a propósito: el consumidor "abandona". Si pump
	// siguiera bloqueado en el send (I2 sin arreglar), este receive nunca
	// termina y el test se cae por timeout, no por un assert.
	select {
	case <-time.After(5 * time.Second):
		t.Fatal("pump se quedó bloqueado: el canal de output nunca se cerró")
	case <-waitClosed(att.Output()):
	}

	if !att.Dropped() {
		t.Fatal("Dropped() tendría que reportar la expulsión local, igual que el manager en proceso")
	}

	// La sesión remota sigue viva (el "yes" sigue corriendo); la limpiamos
	// para no dejarla huérfana hasta el Close() del t.Cleanup.
	_ = cl.Kill(rec.ID)

	waitNoLeak(t, before)
}

// waitClosed drena un canal hasta que se cierra y devuelve una señal de ese
// momento. Deliberadamente no expone lo que lee: acá lo único que importa es
// el cierre, no los datos.
func waitClosed(ch <-chan []byte) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		for range ch {
		}
		close(done)
	}()
	return done
}
