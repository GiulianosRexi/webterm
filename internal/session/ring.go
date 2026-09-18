// Package session es el dueño de los ptys vivos: los compone con el store,
// reparte su output a N clientes y reconcilia el estado cuando mueren.
package session

// ring guarda los últimos maxBytes de output como chunks enteros.
//
// Nunca corta un chunk al medio: replayar media secuencia ANSI o medio
// carácter UTF-8 le ensucia la pantalla al cliente que se attachea. Por eso el
// tail puede quedar un poco por encima del cap.
//
// No tiene candado propio: lo sincroniza liveSession, que necesita
// snapshotear y suscribir en una sola operación atómica.
type ring struct {
	chunks   [][]byte
	bytes    int
	maxBytes int
}

func newRing(maxBytes int) *ring {
	if maxBytes <= 0 {
		maxBytes = 1
	}
	return &ring{maxBytes: maxBytes}
}

// append agrega una copia de p. La copia es obligatoria: el lector del pty
// reusa su buffer entre lecturas.
func (r *ring) append(p []byte) {
	if len(p) == 0 {
		return
	}
	chunk := make([]byte, len(p))
	copy(chunk, p)
	r.chunks = append(r.chunks, chunk)
	r.bytes += len(chunk)
	r.trim()
}

// preload siembra el ring con el historial que ya está en la base, para que
// una sesión reanudada replaye también lo de antes del reinicio.
func (r *ring) preload(p []byte) {
	if len(p) == 0 {
		return
	}
	r.chunks = [][]byte{p}
	r.bytes = len(p)
	r.trim()
}

// trim descarta los chunks más viejos, conservando siempre al menos uno:
// dejar el ring vacío le sacaría al cliente lo único con lo que redibuja.
func (r *ring) trim() {
	for r.bytes > r.maxBytes && len(r.chunks) > 1 {
		r.bytes -= len(r.chunks[0])
		r.chunks = r.chunks[1:]
	}
}

// snapshot devuelve el tail completo, listo para mandarle al cliente.
func (r *ring) snapshot() []byte {
	out := make([]byte, 0, r.bytes)
	for _, c := range r.chunks {
		out = append(out, c...)
	}
	return out
}
