// Package ptyapi es el contrato entre quien tiene los ptys y quien los usa.
//
// Existe para que el orquestador no sepa si del otro lado hay un daemon en
// otro proceso o un manager en el mismo: lo implementan las dos cosas, y los
// tests usan el segundo sin levantar nada.
//
// No importa nada del proyecto a propósito. Es el contrato, no una capa.
package ptyapi

import "errors"

// ErrNotLive lo devuelve todo lo que necesita un proceso vivo del otro lado.
// El orquestador lo traduce al camino de solo lectura, que no es un error
// sino cómo se mira una sesión terminada.
var ErrNotLive = errors.New("la sesión no está corriendo")

// ErrAlreadyLive lo devuelve Spawn sobre una sesión que ya tiene proceso.
//
// Es un error distinto de ErrNotLive y no su negación: viajan por el socket
// como status HTTP distintos (410 y 409) justamente para que el cliente pueda
// reconstruir cuál era sin parsear el texto.
var ErrAlreadyLive = errors.New("la sesión ya está corriendo")

// SpawnOpts describe el pty a arrancar.
//
// La fila en la base ya existe cuando esto llega: el que spawnea no inserta.
// Ese orden no es arbitrario, session_output tiene FK contra sessions.
type SpawnOpts struct {
	ID    string `json:"id"`
	Shell string `json:"shell"`
	Cwd   string `json:"cwd"`
	Cols  int    `json:"cols"`
	Rows  int    `json:"rows"`
	// Env son variables que se suman al entorno del pty. Las arma el
	// orquestador: quien spawnea no sabe qué es un token ni le importa. Es lo
	// que permite sumar una variable nueva sin tocar el daemon.
	Env []string `json:"env,omitempty"`
	// Banner queda en el historial antes del primer prompt. Lo escribe quien
	// posee el pty porque es el único que puede ordenarlo contra el writer.
	Banner string `json:"banner,omitempty"`
}

// Attachment es la conexión de un cliente a una sesión viva.
//
// Write y Resize cuelgan de acá y no del Client porque en la implementación
// remota viajan por el mismo socket que el output: son parte de la conexión,
// no operaciones sueltas.
type Attachment interface {
	// History es el replay que hay que mandar antes del stream vivo.
	History() []byte
	// Output se cierra cuando la sesión termina o se desattachea.
	Output() <-chan []byte
	// Write manda input crudo al pty.
	Write(p []byte) error
	// Resize cambia el tamaño de la ventana del pty.
	Resize(rows, cols uint16) error
	// Detach desconecta al cliente sin tocar la sesión.
	Detach()
	// Dropped dice si al cliente lo expulsaron por no leer a tiempo. Solo es
	// significativo después de que Output se haya cerrado.
	Dropped() bool
}

// Client es quien tiene los ptys.
type Client interface {
	// Spawn arranca el pty de una sesión cuya fila ya existe.
	Spawn(o SpawnOpts) error
	// Attach devuelve ErrNotLive si la sesión no tiene proceso corriendo.
	Attach(id string) (Attachment, error)
	// Kill es sincrónico: al volver, la fila ya refleja la muerte.
	Kill(id string) error
	// LiveIDs son las sesiones con proceso corriendo. Es la fuente de verdad
	// del sweep del orquestador.
	LiveIDs() ([]string, error)
	// Close suelta los recursos del cliente. No mata las sesiones remotas.
	Close() error
}
