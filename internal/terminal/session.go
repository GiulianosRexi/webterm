// Package terminal envuelve un proceso corriendo bajo un pseudo-terminal.
//
// En M1 la sesión vive mientras dure la conexión WebSocket. La API está
// pensada para que en M2 el session manager pueda quedarse dueño de la
// Session sin que cambie nada de acá.
package terminal

import (
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

	mu   sync.Mutex
	ptmx *os.File
	done bool
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

	return &Session{ID: id, cmd: cmd, ptmx: ptmx}, nil
}

// Read devuelve output crudo del pty (secuencias ANSI incluidas).
func (s *Session) Read(p []byte) (int, error) { return s.ptmx.Read(p) }

// Write manda input crudo del usuario al pty.
func (s *Session) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return 0, io.ErrClosedPipe
	}
	return s.ptmx.Write(p)
}

// Resize cambia el tamaño de la ventana del pty y le manda SIGWINCH al proceso.
func (s *Session) Resize(rows, cols uint16) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return io.ErrClosedPipe
	}
	return pty.Setsize(s.ptmx, &pty.Winsize{Rows: rows, Cols: cols})
}

// Close mata el proceso y cierra el pty. Es idempotente.
func (s *Session) Close() error {
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		return nil
	}
	s.done = true
	ptmx := s.ptmx
	s.mu.Unlock()

	err := ptmx.Close()
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	_ = s.cmd.Wait()
	return err
}
