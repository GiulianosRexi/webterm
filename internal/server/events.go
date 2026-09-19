package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// sseWriteTimeout acota cuánto puede tardar un Flush. Sin esto, un cliente
// que se quedó sin leer —la laptop se durmió, el wifi se cortó en silencio,
// cualquier corte que no cierre el socket prolijo— llena el buffer del SO y
// Flush se bloquea para siempre: el goroutine nunca llega al select de abajo,
// así que la cancelación de r.Context() no lo salva.
const sseWriteTimeout = 10 * time.Second

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
	if s.cfg.Events == nil {
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

	ch, unsubscribe := s.cfg.Events.Subscribe()
	defer unsubscribe()

	rc := http.NewResponseController(w)

	// Los errores de escritura de acá para abajo se ignoran a propósito: lo
	// que hace que este handler no se quede colgado para siempre no es
	// chequear el error, es el deadline de más abajo. Un cliente que cortó
	// prolijo cancela r.Context() y el select lo agarra en la próxima vuelta;
	// uno que se quedó mudo a mitad de un envío —la laptop se durmió, el wifi
	// se cortó sin avisar— nunca llega a ese select porque Flush está
	// bloqueado esperando que el SO vacíe el buffer, y ahí el único que corta
	// eso es el deadline.
	_ = rc.SetWriteDeadline(time.Now().Add(sseWriteTimeout))
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
			_ = rc.SetWriteDeadline(time.Now().Add(sseWriteTimeout))
			fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.Seq, ev.Kind, b)
			flusher.Flush()
		case <-tick.C:
			_ = rc.SetWriteDeadline(time.Now().Add(sseWriteTimeout))
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}
