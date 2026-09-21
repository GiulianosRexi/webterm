// Package events reparte avisos de cambio entre los clientes conectados.
//
// Un evento dice qué cambió y de qué sesión, nunca qué quedó: el que escucha
// va a buscar el dato por REST. Así no hay una segunda serialización que
// mantener ni un estado que pueda divergir del store.
package events

import "sync"

// Kind es el tipo de cambio. Viaja tal cual en el campo event: del frame SSE.
type Kind string

const (
	ResourceAdded   Kind = "resource.added"
	ResourceRemoved Kind = "resource.removed"
	SessionCreated  Kind = "session.created"
	SessionUpdated  Kind = "session.updated"
	SessionDeleted  Kind = "session.deleted"
	// Los de folder no llevan sesión: el cliente vuelve a pedir la lista de
	// folders entera, que es corta.
	FolderCreated Kind = "folder.created"
	FolderUpdated Kind = "folder.updated"
	FolderDeleted Kind = "folder.deleted"
)

// Event es lo que se reparte. Seq lo asigna el bus.
type Event struct {
	Seq       uint64 `json:"seq"`
	Kind      Kind   `json:"kind"`
	SessionID string `json:"session_id"`
}

// defaultBuffer es lo que se usa si New recibe un buffer sin sentido. Alcanza
// para una ráfaga corta sin que un cliente que parpadea pierda eventos.
const defaultBuffer = 16

// Bus reparte eventos entre suscriptores. Es seguro para uso concurrente.
type Bus struct {
	mu     sync.Mutex
	seq    uint64
	buffer int
	subs   map[chan Event]struct{}
}

// New construye un bus. buffer es el tamaño del canal de CADA suscriptor, no
// un buffer compartido.
func New(buffer int) *Bus {
	if buffer <= 0 {
		buffer = defaultBuffer
	}
	return &Bus{buffer: buffer, subs: map[chan Event]struct{}{}}
}

// Publish numera el evento y se lo reparte a todos.
//
// Nunca bloquea. Al suscriptor que tiene el buffer lleno se le saltea este
// evento y listo: ni se lo espera —frenar al que publica por un cliente lento
// congelaría al resto— ni se lo expulsa, porque el hueco en Seq ya le alcanza
// para darse cuenta y resincronizarse.
func (b *Bus) Publish(kind Kind, sessionID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seq++
	ev := Event{Seq: b.seq, Kind: kind, SessionID: sessionID}
	for ch := range b.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// Subscribe devuelve el canal del suscriptor y la función que lo da de baja.
// Esa función es idempotente y hay que llamarla siempre, o el suscriptor queda
// en el mapa recibiendo eventos que nadie lee.
func (b *Bus) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, b.buffer)

	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			// El close va adentro del candado junto con el delete: así no
			// puede pasar que Publish esté adentro del select con este canal
			// justo cuando lo cerramos.
			b.mu.Lock()
			defer b.mu.Unlock()
			delete(b.subs, ch)
			close(ch)
		})
	}
}
