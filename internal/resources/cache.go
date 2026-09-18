package resources

import (
	"context"
	"sync"
	"time"
)

const (
	// Cuánto vale un estado bueno antes de volver a consultar.
	okTTL = 30 * time.Second
	// Los errores se cachean menos: si GitHub está caído no conviene
	// reintentar en cada request, pero tampoco quedarse pegado al error.
	errTTL = 10 * time.Second
)

// entry es lo cacheado para un ref. Mientras done sea no-nil hay una consulta
// en curso y los demás lectores esperan en ese canal en vez de disparar otra.
type entry struct {
	snap *Snapshot
	done chan struct{}
	at   time.Time
}

// Cache guarda el último estado conocido de cada recurso.
//
// No se persiste a propósito: un check en verde de hace veinte minutos
// mostrado como actual es peor que no mostrar nada, y al reiniciar el backend
// conviene volver a preguntar.
type Cache struct {
	reg *Registry
	// now se puede reemplazar en los tests para no depender del reloj real.
	now func() time.Time

	mu      sync.Mutex
	entries map[string]*entry
}

func NewCache(reg *Registry) *Cache {
	return &Cache{
		reg:     reg,
		now:     time.Now,
		entries: map[string]*entry{},
	}
}

// Resolve normaliza una URL a su Ref canónico, sin consultar el estado. Lo usa
// el manager al linkear, que necesita system y type pero no el estado.
func (c *Cache) Resolve(rawURL string) (Ref, bool) {
	ref, _, ok := c.reg.Resolve(rawURL)
	return ref, ok
}

// Get devuelve el estado del recurso, consultando solo si lo cacheado venció.
//
// Nunca devuelve nil: los problemas viajan dentro del Snapshot, porque son
// estado que la UI tiene que mostrar, no fallas de esta capa.
func (c *Cache) Get(ctx context.Context, ref Ref) *Snapshot {
	for {
		c.mu.Lock()
		e, ok := c.entries[ref.URL]

		// Hay una consulta en curso: esperamos su resultado en vez de disparar
		// otra. Es lo que hace que N clientes mirando el mismo PR cuesten una
		// sola llamada a GitHub.
		if ok && e.done != nil {
			done := e.done
			c.mu.Unlock()
			select {
			case <-done:
				continue // ya hay resultado fresco: volvemos a mirar el mapa
			case <-ctx.Done():
				return &Snapshot{FetchedAt: c.now().UnixMilli(), Error: "consulta cancelada"}
			}
		}

		if ok && !c.expired(e) {
			snap := e.snap
			c.mu.Unlock()
			return snap
		}

		// Nos quedamos nosotros con la consulta.
		pending := &entry{done: make(chan struct{}), at: c.now()}
		if ok {
			pending.snap = e.snap // conservamos lo viejo por si la consulta falla
		}
		c.entries[ref.URL] = pending
		c.mu.Unlock()

		snap := c.fetch(ctx, ref)

		c.mu.Lock()
		done := pending.done
		pending.snap = snap
		pending.at = c.now()
		pending.done = nil
		c.mu.Unlock()
		close(done)

		return snap
	}
}

// expired dice si hay que volver a consultar. Los errores vencen antes.
func (c *Cache) expired(e *entry) bool {
	ttl := okTTL
	if e.snap == nil || e.snap.Error != "" {
		ttl = errTTL
	}
	return c.now().Sub(e.at) >= ttl
}

func (c *Cache) fetch(ctx context.Context, ref Ref) *Snapshot {
	p, ok := c.reg.ProviderFor(ref)
	if !ok {
		return &Snapshot{
			FetchedAt: c.now().UnixMilli(),
			Error:     "no hay integración para " + ref.System,
		}
	}
	snap, err := p.Fetch(ctx, ref)
	if err != nil {
		return &Snapshot{FetchedAt: c.now().UnixMilli(), Error: err.Error()}
	}
	if snap.FetchedAt == 0 {
		snap.FetchedAt = c.now().UnixMilli()
	}
	return snap
}
