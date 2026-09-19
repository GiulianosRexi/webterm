package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// sseKeepalive es cada cuánto va un comentario por el stream. Sin esto, un
// intermediario puede dar por muerta una conexión que simplemente no tuvo
// novedades.
const sseKeepalive = 25 * time.Second

// handleEvents transmite los cambios por Server-Sent Events.
//
// Cada conexión arranca con un resync: el cliente se pone al día por REST y
// sigue desde ahí. Eso es lo que evita tener que guardar historial y atender
// Last-Event-ID, porque una reconexión y un arranque en frío son el mismo caso.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if s.events == nil {
		writeErrorMsg(w, http.StatusNotFound, "eventos deshabilitados")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErrorMsg(w, http.StatusInternalServerError, "el transporte no soporta streaming")
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// Un proxy que buffea se queda el stream entero y no llega nada hasta que
	// cierra: esto le pide explícitamente que no lo haga.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	ch, unsubscribe := s.events.Subscribe()
	defer unsubscribe()

	// Los errores de escritura de acá para abajo se ignoran a propósito: si
	// el cliente cortó la conexión, r.Context() se cancela enseguida y el
	// select de abajo devuelve el handler en la próxima vuelta. No hace falta
	// un segundo camino de error para lo mismo que ya cubre el contexto.
	fmt.Fprint(w, "event: resync\ndata: {}\n\n")
	flusher.Flush()

	tick := time.NewTicker(sseKeepalive)
	defer tick.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			// Event solo tiene campos serializables (uint64, string), así que
			// Marshal acá nunca puede fallar; el chequeo es defensivo.
			b, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.Seq, ev.Kind, b)
			flusher.Flush()
		case <-tick.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}
