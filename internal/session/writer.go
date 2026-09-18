package session

import (
	"log"
	"sync"
	"time"

	"github.com/giuliano/webterm/internal/store"
)

const (
	// Cada cuánto se baja a disco lo acumulado.
	flushInterval = 250 * time.Millisecond
	// Cuánto se puede acumular antes de forzar un flush.
	flushThreshold = 64 * 1024
)

// outputWriter baja a SQLite el output de una sesión, batcheado.
//
// Sin el batch, un `cat` de un archivo grande dispararía miles de INSERT por
// segundo. El buffer vive en memoria y acumula como máximo lo que entre en un
// intervalo de flush, así que write() nunca bloquea al lector del pty.
type outputWriter struct {
	st        *store.Store
	sessionID string
	maxBytes  int64

	mu      sync.Mutex
	pending []byte
	closed  bool

	kick      chan struct{}
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

func newOutputWriter(st *store.Store, sessionID string, maxBytes int64) *outputWriter {
	w := &outputWriter{
		st:        st,
		sessionID: sessionID,
		maxBytes:  maxBytes,
		kick:      make(chan struct{}, 1),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
	go w.loop()
	return w
}

// write encola un chunk. Copia p porque el lector del pty reusa su buffer.
func (w *outputWriter) write(p []byte) {
	if len(p) == 0 {
		return
	}
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.pending = append(w.pending, p...)
	full := len(w.pending) >= flushThreshold
	w.mu.Unlock()

	if full {
		select {
		case w.kick <- struct{}{}:
		default: // ya hay un flush pedido
		}
	}
}

// close hace el último flush y espera a que la goroutine termine. El reaper lo
// llama antes de marcar la sesión como muerta, para que el historial guardado
// llegue hasta el final.
func (w *outputWriter) close() {
	w.closeOnce.Do(func() {
		close(w.stop)
		<-w.done
		w.mu.Lock()
		w.closed = true
		w.mu.Unlock()
	})
}

// flushNow baja lo pendiente de forma sincrónica. Es para los tests, donde
// esperar el ticker de 250 ms haría todo más lento y más frágil.
func (w *outputWriter) flushNow() { w.flush() }

func (w *outputWriter) loop() {
	defer close(w.done)
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			w.flush()
		case <-w.kick:
			w.flush()
		case <-w.stop:
			w.flush()
			return
		}
	}
}

func (w *outputWriter) flush() {
	w.mu.Lock()
	batch := w.pending
	w.pending = nil
	w.mu.Unlock()

	if len(batch) == 0 {
		return
	}
	if err := w.st.AppendOutput(w.sessionID, batch); err != nil {
		// Perder historial es feo pero no justifica matar la sesión: el
		// stream vivo y el ring buffer siguen funcionando.
		log.Printf("[%s] no se pudo guardar el historial: %v", w.sessionID, err)
		return
	}
	if err := w.st.PruneOutput(w.sessionID, w.maxBytes); err != nil {
		log.Printf("[%s] no se pudo podar el historial: %v", w.sessionID, err)
	}
}
