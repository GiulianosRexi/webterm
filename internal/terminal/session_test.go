package terminal

import (
	"io"
	"testing"
	"time"
)

func newTestSession(t *testing.T) *Session {
	t.Helper()
	s, err := New("t1", Config{Shell: "/bin/bash", Rows: 24, Cols: 80})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// drain lee en background para que el pty no se llene y frene al shell.
func drain(s *Session) {
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := s.Read(buf); err != nil {
				return
			}
		}
	}()
}

// TestDoneYExitCode: el exit code del shell llega hasta el backend. Es lo que
// el session manager escribe en la DB al reconciliar.
func TestDoneYExitCode(t *testing.T) {
	s := newTestSession(t)
	drain(s)

	if _, err := s.Write([]byte("exit 7\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	select {
	case <-s.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("timeout esperando Done")
	}
	if got := s.ExitCode(); got != 7 {
		t.Fatalf("ExitCode = %d, se esperaba 7", got)
	}
}

// TestDoneConNietoQueRetieneElPty es el caso que motiva usar cmd.Wait() en
// lugar del EOF del Read: el shell muere pero un nieto heredó el esclavo del
// pty, así que el Read no da EOF nunca. Sin esta señal la sesión quedaría
// marcada como viva para siempre.
func TestDoneConNietoQueRetieneElPty(t *testing.T) {
	s := newTestSession(t)
	drain(s)

	// El sleep queda corriendo con el pty abierto después de que el shell sale.
	if _, err := s.Write([]byte("sleep 30 & exit 0\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	select {
	case <-s.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("Done no llegó: se está esperando el EOF del Read en vez de cmd.Wait()")
	}
	if got := s.ExitCode(); got != 0 {
		t.Fatalf("ExitCode = %d", got)
	}
}

// TestKillDestrabaAlLector: cerrar el pty al morir el proceso es lo que saca
// al lector de un Read bloqueado.
func TestKillDestrabaAlLector(t *testing.T) {
	s := newTestSession(t)

	errc := make(chan error, 1)
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := s.Read(buf); err != nil {
				errc <- err
				return
			}
		}
	}()

	if err := s.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	select {
	case <-errc:
	case <-time.After(10 * time.Second):
		t.Fatal("el lector quedó bloqueado después del Kill")
	}
}

func TestCloseEsIdempotente(t *testing.T) {
	s := newTestSession(t)
	if err := s.Close(); err != nil {
		t.Fatalf("Close 1: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close 2: %v", err)
	}
	if _, err := s.Write([]byte("x")); err == nil {
		t.Fatal("escribir a una sesión cerrada tiene que fallar")
	} else if err != io.ErrClosedPipe {
		t.Fatalf("se esperaba io.ErrClosedPipe, vino %v", err)
	}
}
