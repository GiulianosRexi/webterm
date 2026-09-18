package session

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/giuliano/webterm/internal/ptyapi"
	"github.com/giuliano/webterm/internal/store"
)

// TestKillConservaElHistorial: matar no es borrar. La distinción es explícita
// justamente para que el historial no se vaya sin que nadie lo pida.
func TestKillConservaElHistorial(t *testing.T) {
	m, st := newTestManager(t)
	id := spawnTest(t, m, st, nil)

	att, err := m.Attach(id)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if err := att.Write([]byte("echo sobrevive-al-kill\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	awaitChunk(t, att.Output(), "sobrevive-al-kill")
	att.Detach()

	if err := m.Kill(id); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	// Kill es sincrónico: al volver, la DB ya tiene que estar reconciliada.
	got, err := st.GetSession(id)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.PtyStatus != store.StatusExited {
		t.Fatalf("pty_status = %q", got.PtyStatus)
	}
	if got.ExitReason != string(store.ReasonKilled) {
		t.Fatalf("exit_reason = %q, se esperaba killed", got.ExitReason)
	}

	hist, _ := st.ReadOutput(id)
	if !bytes.Contains(hist, []byte("sobrevive-al-kill")) {
		t.Fatalf("el kill se llevó el historial: %q", tail(hist, 200))
	}
}

func TestKillSesionNoVivaDaErrNotLive(t *testing.T) {
	m, st := newTestManager(t)
	id := spawnTest(t, m, st, nil)
	if err := m.Kill(id); err != nil {
		t.Fatal(err)
	}
	// La idempotencia es del orquestador, que sabe si la fila existe. Acá el
	// contrato es literal: no hay proceso que matar.
	if err := m.Kill(id); !errors.Is(err, ptyapi.ErrNotLive) {
		t.Fatalf("segundo Kill dio %v; quería ErrNotLive", err)
	}
}

// Garantía load-bearing: cuando al cliente se le cierra el canal de output, la
// fila YA dice exited. Todo el manejo de fin de sesión —el del daemon y el del
// orquestador— depende de este orden.
func TestLaFilaYaEstaMarcadaCuandoSeCierraElOutput(t *testing.T) {
	m, st := newTestManager(t)
	id := spawnTest(t, m, st, nil)

	att, err := m.Attach(id)
	if err != nil {
		t.Fatal(err)
	}
	defer att.Detach()
	if err := att.Write([]byte("exit\n")); err != nil {
		t.Fatal(err)
	}

	for range att.Output() {
		// drenar hasta que cierre
	}

	rec, err := st.GetSession(id)
	if err != nil {
		t.Fatal(err)
	}
	if rec.PtyStatus != store.StatusExited {
		t.Fatalf("al cerrarse el output la fila decía %s; quería exited", rec.PtyStatus)
	}
}

// TestCloseMataTodo: al apagar el daemon no quedan procesos sueltos ni filas
// mintiendo.
func TestCloseMataTodo(t *testing.T) {
	m, st := newTestManager(t)
	id := spawnTest(t, m, st, nil)

	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	got, err := st.GetSession(id)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.PtyStatus != store.StatusExited {
		t.Fatalf("pty_status = %q después de Close", got.PtyStatus)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close dos veces: %v", err)
	}
}

// TestExtraEnvLlegaAlPty: el cliente MCP que corre adentro de la sesión saca el
// token de su propio entorno, así que tiene que estar ahí. Las variables las
// arma el orquestador y llegan resueltas en SpawnOpts: el dueño del pty no sabe
// que existe un token.
func TestExtraEnvLlegaAlPty(t *testing.T) {
	m, st := newTestManager(t)
	id := spawnTest(t, m, st, []string{"WEBTERM_TOKEN=un-token-de-prueba"})

	att, err := m.Attach(id)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer att.Detach()

	// El id de sesión ya viajaba desde M1; el token es lo que suma M9.
	if err := att.Write([]byte("echo T=$WEBTERM_TOKEN S=$WEBTERM_SESSION_ID\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	out := awaitChunk(t, att.Output(), "T=un-token-de-prueba")
	if !strings.Contains(out, "S="+id) {
		t.Fatalf("falta el id de sesión en el entorno: %q", tail([]byte(out), 200))
	}
}

// TestSinExtraEnvNoHayToken: sin token en el SpawnOpts no se filtra una
// variable vacía al entorno.
func TestSinExtraEnvNoHayToken(t *testing.T) {
	m, st := newTestManager(t)
	id := spawnTest(t, m, st, nil)

	att, err := m.Attach(id)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer att.Detach()

	if err := att.Write([]byte("echo TOKEN=[${WEBTERM_TOKEN:-vacio}]\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	awaitChunk(t, att.Output(), "TOKEN=[vacio]")
}
