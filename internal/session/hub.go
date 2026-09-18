package session

import "sync"

// subscriber es un cliente attacheado a una sesión.
type subscriber struct {
	ch chan []byte

	mu      sync.Mutex
	closed  bool
	dropped bool
}

// out es el canal por el que llega el output. Se cierra al desattachear o al
// morir la sesión.
func (s *subscriber) out() <-chan []byte { return s.ch }

// wasDropped dice si al cliente lo expulsamos por lento. El handler del
// WebSocket lo usa para elegir el motivo del close.
func (s *subscriber) wasDropped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dropped
}

func (s *subscriber) close(dropped bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	s.dropped = dropped
	close(s.ch)
}

// hub reparte el output de una sesión entre sus clientes.
//
// Como el ring, no tiene candado propio: lo sincroniza liveSession.
type hub struct {
	subs map[*subscriber]struct{}
}

func newHub() *hub {
	return &hub{subs: map[*subscriber]struct{}{}}
}

func (h *hub) subscribe(bufSize int) *subscriber {
	s := &subscriber{ch: make(chan []byte, bufSize)}
	h.subs[s] = struct{}{}
	return s
}

func (h *hub) unsubscribe(s *subscriber) {
	delete(h.subs, s)
	s.close(false)
}

// broadcast entrega p a todos los suscriptores sin bloquearse nunca. Al que
// tiene el buffer lleno lo expulsamos: frenar el pty porque un cliente no lee
// congelaría la sesión para todos los demás.
//
// p no se copia: los suscriptores comparten el slice y no deben modificarlo.
func (h *hub) broadcast(p []byte) {
	for s := range h.subs {
		select {
		case s.ch <- p:
		default:
			delete(h.subs, s)
			s.close(true)
		}
	}
}

// closeAll cierra a todos los clientes por un cierre normal de la sesión.
func (h *hub) closeAll() {
	for s := range h.subs {
		delete(h.subs, s)
		s.close(false)
	}
}

func (h *hub) count() int { return len(h.subs) }
