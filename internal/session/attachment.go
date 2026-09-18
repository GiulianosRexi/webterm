package session

import (
	"github.com/giuliano/webterm/internal/ptyapi"
)

// Attachment es la conexión de un cliente a un pty vivo.
//
// Write y Resize cuelgan de acá y no del Manager porque la implementación
// remota los manda por el mismo socket que el output: hacerlos operaciones
// sueltas del Client obligaría a una segunda conexión para lo mismo.
type Attachment struct {
	m       *Manager
	live    *liveSession
	history []byte
	sub     *subscriber
}

func (a *Attachment) History() []byte       { return a.history }
func (a *Attachment) Output() <-chan []byte { return a.sub.out() }
func (a *Attachment) Dropped() bool         { return a.sub.wasDropped() }
func (a *Attachment) Detach()               { a.live.detach(a.sub) }

// Write manda input crudo al pty.
func (a *Attachment) Write(p []byte) error {
	_, err := a.live.pty.Write(p)
	return err
}

// Resize cambia el tamaño del pty y lo persiste: cols y rows son estado del
// pty, así que su dueño es el daemon, y persistirlos es lo que hace que una
// sesión reanudada vuelva con las dimensiones que tenía.
func (a *Attachment) Resize(rows, cols uint16) error {
	if err := a.live.pty.Resize(rows, cols); err != nil {
		return err
	}
	return a.m.st.UpdateSize(a.live.id, int(cols), int(rows))
}

var _ ptyapi.Attachment = (*Attachment)(nil)
