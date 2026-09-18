// Package terminal envuelve un proceso corriendo bajo un pseudo-terminal.
//
// La Session no sabe nada de WebSockets ni de persistencia: su dueño es el
// session manager, que la mantiene viva entre conexiones.
package terminal

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"

	"github.com/creack/pty"
)

// Config describe cómo arrancar la sesión.
type Config struct {
	// Shell a ejecutar. Si está vacío se usa $SHELL y, en su defecto, /bin/zsh.
	Shell string
	// Cwd es el directorio de trabajo inicial. Si está vacío se usa $HOME.
	Cwd string
	// Env son variables extra que se suman al entorno del backend.
	Env []string
	// Rows y Cols son el tamaño inicial del pty.
	Rows, Cols uint16
}

// Session es un proceso corriendo bajo un pty.
type Session struct {
	ID  string
	cmd *exec.Cmd

	mu     sync.Mutex
	ptmx   *os.File
	closed bool

	// done se cierra cuando el proceso terminó; exitCode es válido a partir
	// de ahí (el cierre del canal ordena la escritura contra las lecturas).
	done     chan struct{}
	exitCode int
}

// New spawnea el shell bajo un pty nuevo.
func New(id string, cfg Config) (*Session, error) {
	shell := cfg.Shell
	if shell == "" {
		shell = os.Getenv("SHELL")
	}
	if shell == "" {
		shell = "/bin/zsh"
	}

	cwd := cfg.Cwd
	if cwd == "" {
		cwd, _ = os.UserHomeDir()
	}

	// -l para que el shell cargue el profile del usuario (PATH, aliases, etc).
	cmd := exec.Command(shell, "-l")
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(),
		"TERM=xterm-256color",
		"COLORTERM=truecolor",
		"WEBTERM_SESSION_ID="+id,
	)
	cmd.Env = append(cmd.Env, cfg.Env...)

	rows, cols := cfg.Rows, cfg.Cols
	if rows == 0 {
		rows = 24
	}
	if cols == 0 {
		cols = 80
	}

	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: rows, Cols: cols})
	if err != nil {
		return nil, fmt.Errorf("arrancando pty: %w", err)
	}

	s := &Session{ID: id, cmd: cmd, ptmx: ptmx, done: make(chan struct{})}
	go s.reap()
	return s, nil
}

// reap espera a que el proceso termine y cierra el pty.
//
// La señal autoritativa de muerte es cmd.Wait(), no el EOF del Read: si el
// shell muere pero un nieto heredó el esclavo del pty, el Read no da EOF
// nunca y la sesión quedaría marcada como viva para siempre. Cerrar el ptmx
// acá es además lo que destraba a un lector bloqueado.
func (s *Session) reap() {
	err := s.cmd.Wait()
	s.exitCode = exitCodeOf(err)

	s.mu.Lock()
	s.closed = true
	ptmx := s.ptmx
	s.mu.Unlock()

	_ = ptmx.Close()
	close(s.done)
}

// Done se cierra cuando el proceso terminó.
func (s *Session) Done() <-chan struct{} { return s.done }

// ExitCode devuelve el código de salida. Solo es válido después de que Done
// se haya cerrado; antes devuelve 0.
func (s *Session) ExitCode() int { return s.exitCode }

// Read devuelve output crudo del pty (secuencias ANSI incluidas).
func (s *Session) Read(p []byte) (int, error) { return s.ptmx.Read(p) }

// Write manda input crudo del usuario al pty.
func (s *Session) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, io.ErrClosedPipe
	}
	return s.ptmx.Write(p)
}

// Resize cambia el tamaño de la ventana del pty y le manda SIGWINCH al proceso.
func (s *Session) Resize(rows, cols uint16) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return io.ErrClosedPipe
	}
	return pty.Setsize(s.ptmx, &pty.Winsize{Rows: rows, Cols: cols})
}

// Kill le manda SIGKILL al proceso y vuelve enseguida. Es idempotente: el
// cierre real lo hace reap(). Para esperar a que termine, usar Close.
func (s *Session) Kill() error {
	select {
	case <-s.done:
		return nil
	default:
	}
	if s.cmd.Process == nil {
		return nil
	}
	if err := s.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

// Close mata el proceso y espera a que reap() termine de limpiar. Es idempotente.
func (s *Session) Close() error {
	if err := s.Kill(); err != nil {
		return err
	}
	<-s.done
	return nil
}

// exitCodeOf traduce el error de cmd.Wait a un código. -1 es "murió por una
// señal", que es lo que vemos cuando nosotros mismos lo matamos.
func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}
