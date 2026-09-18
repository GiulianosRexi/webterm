package resources

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// contador es un Provider de prueba que cuenta cuántas veces lo consultaron.
type contador struct {
	n       atomic.Int64
	fallar  bool
	demorar time.Duration
}

func (c *contador) Match(rawURL string) (Ref, bool) {
	return Ref{System: "test", Type: "x", URL: rawURL}, true
}

func (c *contador) Fetch(ctx context.Context, ref Ref) (*Snapshot, error) {
	c.n.Add(1)
	if c.demorar > 0 {
		select {
		case <-time.After(c.demorar):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if c.fallar {
		return &Snapshot{Error: "se rompió"}, nil
	}
	return &Snapshot{PR: &PRState{Number: 1}}, nil
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeClock) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}

func newTestCache(p Provider) (*Cache, *fakeClock) {
	clk := &fakeClock{t: time.Unix(1700000000, 0)}
	c := NewCache(NewRegistry(p))
	c.now = clk.Now
	return c, clk
}

// TestCacheSirveDelCacheDentroDelTTL: el polling del frontend no puede
// traducirse uno a uno en llamadas a GitHub.
func TestCacheSirveDelCacheDentroDelTTL(t *testing.T) {
	p := &contador{}
	c, clk := newTestCache(p)
	ref := Ref{System: "test", Type: "x", URL: "u"}

	for i := 0; i < 5; i++ {
		if snap := c.Get(context.Background(), ref); snap.PR == nil {
			t.Fatalf("llamada %d sin estado", i)
		}
		clk.advance(5 * time.Second)
	}
	if got := p.n.Load(); got != 1 {
		t.Fatalf("se consultó %d veces en 25 s con TTL de 30 s, se esperaba 1", got)
	}
}

func TestCacheRefrescaAlVencer(t *testing.T) {
	p := &contador{}
	c, clk := newTestCache(p)
	ref := Ref{System: "test", Type: "x", URL: "u"}

	c.Get(context.Background(), ref)
	clk.advance(okTTL + time.Second)
	c.Get(context.Background(), ref)

	if got := p.n.Load(); got != 2 {
		t.Fatalf("se consultó %d veces, se esperaban 2", got)
	}
}

// TestCacheErrorTieneTTLCorto: si GitHub está caído no conviene reintentar en
// cada request, pero tampoco quedarse pegado al error medio minuto.
func TestCacheErrorTieneTTLCorto(t *testing.T) {
	p := &contador{fallar: true}
	c, clk := newTestCache(p)
	ref := Ref{System: "test", Type: "x", URL: "u"}

	c.Get(context.Background(), ref)
	clk.advance(errTTL - time.Second)
	c.Get(context.Background(), ref)
	if got := p.n.Load(); got != 1 {
		t.Fatalf("dentro del TTL de error se consultó %d veces", got)
	}

	clk.advance(2 * time.Second)
	c.Get(context.Background(), ref)
	if got := p.n.Load(); got != 2 {
		t.Fatalf("pasado el TTL de error se consultó %d veces, se esperaban 2", got)
	}
}

// TestCacheSingleFlight: N clientes mirando el mismo PR cuestan una sola
// llamada a GitHub.
func TestCacheSingleFlight(t *testing.T) {
	p := &contador{demorar: 100 * time.Millisecond}
	c, _ := newTestCache(p)
	ref := Ref{System: "test", Type: "x", URL: "u"}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if snap := c.Get(context.Background(), ref); snap.PR == nil {
				t.Error("snapshot sin estado")
			}
		}()
	}
	wg.Wait()

	if got := p.n.Load(); got != 1 {
		t.Fatalf("20 clientes simultáneos dispararon %d llamadas", got)
	}
}

func TestCacheRefsDistintos(t *testing.T) {
	p := &contador{}
	c, _ := newTestCache(p)

	c.Get(context.Background(), Ref{System: "test", Type: "x", URL: "a"})
	c.Get(context.Background(), Ref{System: "test", Type: "x", URL: "b"})

	if got := p.n.Load(); got != 2 {
		t.Fatalf("dos refs distintos dieron %d llamadas", got)
	}
}

// TestCacheSinProvider: un ref guardado cuyo provider ya no existe no puede
// tumbar la lista entera.
func TestCacheSinProvider(t *testing.T) {
	c := NewCache(NewRegistry())
	snap := c.Get(context.Background(), Ref{System: "fantasma", Type: "x", URL: "u"})
	if snap.Error == "" {
		t.Fatal("se esperaba un error en el snapshot")
	}
}

func TestCacheResolve(t *testing.T) {
	c := NewCache(NewRegistry(NewGitHub()))
	ref, ok := c.Resolve("github.com/cli/cli/pull/1")
	if !ok || ref.System != "gh" || ref.URL != "https://github.com/cli/cli/pull/1" {
		t.Fatalf("Resolve = %+v %v", ref, ok)
	}
	if _, ok := c.Resolve("https://ejemplo.invalido/x"); ok {
		t.Fatal("resolvió una URL desconocida")
	}
}
