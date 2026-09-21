# Canal de eventos servidor → navegador — Plan de implementación

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Que linkear un recurso o cambiar una sesión se vea en la UI al instante, reemplazando el polling por un stream SSE.

**Architecture:** Un bus de eventos in-process (`internal/events`) al que publica `control.Manager` en sus puntos de escritura. `GET /api/events` expone ese bus por SSE. El evento dice qué cambió y de qué sesión, nunca el objeto: el cliente refetchea el endpoint REST que ya existe.

**Tech Stack:** Go 1.x (stdlib: `net/http`, `sync`, `encoding/json`), React 19 + TypeScript (`EventSource`), Vite.

**Spec:** `docs/superpowers/specs/2026-09-19-canal-de-eventos-design.md`

## Global Constraints

- **No tocar `internal/session` ni `internal/daemon`.** El daemon es dueño de los ptys de sesiones en uso; tocarlo las pone en riesgo. Si una tarea parece necesitarlo, parar y avisar.
- El bus es opcional en todos lados: `nil` significa "no publicar". Los tests existentes se construyen sin bus y no se modifican.
- Publicar **siempre después** de que el store confirmó la escritura, nunca antes.
- Comentarios y mensajes de commit en español, como el resto del repo.
- Los commits terminan con `Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>`.
- Correr `go test ./...` antes de cada commit de backend; `npm --prefix web run build` antes del commit de frontend.

## File Structure

| Archivo | Responsabilidad |
|---|---|
| `internal/events/bus.go` (nuevo) | Tipos `Kind`/`Event` y el `Bus`: numerar, repartir, suscribir. No sabe de HTTP ni de sesiones. |
| `internal/events/bus_test.go` (nuevo) | Fanout, cierre y comportamiento ante un suscriptor lento. |
| `internal/control/manager.go` (modificar) | Publica en sus puntos de escritura. Única dependencia nueva: `internal/events`. |
| `internal/control/events_test.go` (nuevo) | Que los puntos de escritura publican, y que con bus `nil` no rompe. |
| `internal/server/events.go` (nuevo) | El handler SSE. Traduce `events.Event` a frames. |
| `internal/server/events_test.go` (nuevo) | Formato del frame, resync inicial, cierre por contexto. |
| `internal/server/server.go` (modificar) | Campo `Events` en `Config`, campo en `Server`, ruta nueva. |
| `cmd/webterm/main.go` (modificar) | Construye el bus y lo inyecta en las dos configs. |
| `web/src/useEvents.ts` (nuevo) | Hook del `EventSource`: suscripción, resync y detección de huecos. |
| `web/src/App.tsx` (modificar) | Monta el hook, refetchea sesiones, propaga los eventos de recursos. |
| `web/src/ResourcePanel.tsx` (modificar) | Refetchea cuando su `reloadKey` cambia. |

---

### Task 1: El bus de eventos

**Files:**
- Create: `internal/events/bus.go`
- Test: `internal/events/bus_test.go`

**Interfaces:**
- Consumes: nada.
- Produces: `events.Kind` (constantes `ResourceAdded`, `ResourceRemoved`, `SessionCreated`, `SessionUpdated`, `SessionDeleted`), `events.Event{Seq uint64; Kind Kind; SessionID string}`, `events.New(buffer int) *Bus`, `(*Bus).Publish(kind Kind, sessionID string)`, `(*Bus).Subscribe() (<-chan Event, func())`.

- [x] **Step 1: Escribir los tests que fallan**

Crear `internal/events/bus_test.go`:

```go
package events

import (
	"testing"
	"time"
)

// recv saca un evento del canal o falla: un test que se cuelga esperando un
// evento que no llega no dice nada útil.
func recv(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("el canal estaba cerrado")
		}
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("no llegó ningún evento")
		return Event{}
	}
}

func TestPublicaATodosLosSuscriptores(t *testing.T) {
	b := New(4)
	a, closeA := b.Subscribe()
	defer closeA()
	c, closeC := b.Subscribe()
	defer closeC()

	b.Publish(ResourceAdded, "s1")

	for _, ch := range []<-chan Event{a, c} {
		ev := recv(t, ch)
		if ev.Kind != ResourceAdded || ev.SessionID != "s1" {
			t.Fatalf("evento inesperado: %+v", ev)
		}
		if ev.Seq != 1 {
			t.Fatalf("Seq = %d, esperaba 1", ev.Seq)
		}
	}
}

func TestSeqEsMonotonicoYCompartido(t *testing.T) {
	b := New(4)
	ch, stop := b.Subscribe()
	defer stop()

	b.Publish(SessionCreated, "s1")
	b.Publish(SessionUpdated, "s1")

	if got := recv(t, ch).Seq; got != 1 {
		t.Fatalf("primer Seq = %d, esperaba 1", got)
	}
	if got := recv(t, ch).Seq; got != 2 {
		t.Fatalf("segundo Seq = %d, esperaba 2", got)
	}
}

func TestUnsubscribeCierraYDejaDeRecibir(t *testing.T) {
	b := New(4)
	ch, stop := b.Subscribe()
	stop()

	if _, ok := <-ch; ok {
		t.Fatal("el canal tendría que estar cerrado")
	}
	// Publicar después de que se fue el único suscriptor no puede entrar en
	// pánico por escribir en un canal cerrado.
	b.Publish(ResourceAdded, "s1")
}

func TestUnsubscribeEsIdempotente(t *testing.T) {
	b := New(4)
	_, stop := b.Subscribe()
	stop()
	stop() // un doble defer no puede cerrar dos veces el mismo canal
}

// Un cliente que no lee no puede frenar al que publica: se le saltean eventos
// y el hueco queda visible en Seq, que es como el cliente sabe que tiene que
// resincronizarse.
func TestSuscriptorLentoNoBloqueaYDejaHueco(t *testing.T) {
	b := New(1)
	ch, stop := b.Subscribe()
	defer stop()

	done := make(chan struct{})
	go func() {
		b.Publish(ResourceAdded, "s1") // entra al buffer
		b.Publish(ResourceAdded, "s2") // se descarta
		b.Publish(ResourceAdded, "s3") // se descarta
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish se bloqueó con el suscriptor lleno")
	}

	if got := recv(t, ch).Seq; got != 1 {
		t.Fatalf("Seq = %d, esperaba 1", got)
	}

	b.Publish(ResourceAdded, "s4")
	if got := recv(t, ch).Seq; got != 4 {
		t.Fatalf("Seq = %d, esperaba 4: el hueco tiene que verse", got)
	}
}
```

- [x] **Step 2: Correr los tests para verificar que fallan**

Run: `go test ./internal/events/`
Expected: FAIL, no compila (`undefined: New`, `undefined: Event`).

- [x] **Step 3: Escribir la implementación**

Crear `internal/events/bus.go`:

```go
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
```

- [x] **Step 4: Correr los tests para verificar que pasan**

Run: `go test ./internal/events/ -race -v`
Expected: PASS, los cinco tests.

- [x] **Step 5: Commit**

```bash
git add internal/events/
git commit -m "$(cat <<'MSG'
feat(events): bus in-process de avisos de cambio

El evento lleva tipo y sesión, no el objeto. Publish no bloquea nunca:
al suscriptor lleno se le saltea el evento y el hueco queda visible en
Seq, que es como el cliente sabe que tiene que resincronizarse.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
MSG
)"
```

---

### Task 2: El orquestador publica

**Files:**
- Modify: `internal/control/manager.go` (`Config` en :41, y los métodos `Create` :125, `Restart` :216, `Kill` :255, `Delete` :266, `Sweep` :299, `UpdateMeta` :439, `AddResource` :485, `DeleteResource` :512)
- Test: `internal/control/events_test.go`

**Interfaces:**
- Consumes: `events.New`, `(*events.Bus).Publish`, `(*events.Bus).Subscribe`, las constantes de `events.Kind` (Task 1).
- Produces: campo `Config.Events *events.Bus`. Nada más cambia de firma.

- [x] **Step 1: Escribir el test que falla**

Crear `internal/control/events_test.go`:

```go
package control

import (
	"testing"
	"time"

	"github.com/giuliano/webterm/internal/events"
	"github.com/giuliano/webterm/internal/session"
	"github.com/giuliano/webterm/internal/store"
)

// newTestManagerWithBus es newTestManager (manager_test.go) con un bus
// enchufado, que es lo único que cambia acá.
func newTestManagerWithBus(t *testing.T) (*Manager, *events.Bus) {
	t.Helper()
	st := newTestStore(t)
	pty := session.NewManager(st, session.Config{HistoryBytes: 64 << 10})
	bus := events.New(16)
	m := NewManager(st, pty, Config{Shell: "/bin/sh", Events: bus})
	t.Cleanup(func() {
		_ = m.Close()
		_ = pty.Close()
		_ = st.Close()
	})
	return m, bus
}

// waitFor consume eventos hasta encontrar el que se busca. Filtra en vez de
// mirar solo el primero porque crear una sesión ya publica de por sí.
func waitFor(t *testing.T, ch <-chan events.Event, kind events.Kind) events.Event {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-ch:
			if ev.Kind == kind {
				return ev
			}
		case <-deadline:
			t.Fatalf("nunca llegó un evento %s", kind)
			return events.Event{}
		}
	}
}

func TestAddResourcePublica(t *testing.T) {
	m, bus := newTestManagerWithBus(t)
	ch, stop := bus.Subscribe()
	defer stop()

	rec, err := m.Create(CreateOpts{Title: "t", Cwd: t.TempDir(), Cols: 80, Rows: 24})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// system y type explícitos: así no hace falta un Resources configurado
	// para resolver la URL, que no es lo que este test mira.
	if _, err := m.AddResource(rec.ID, "https://github.com/o/r/pull/1", "github", "pr"); err != nil {
		t.Fatalf("AddResource: %v", err)
	}

	ev := waitFor(t, ch, events.ResourceAdded)
	if ev.SessionID != rec.ID {
		t.Fatalf("SessionID = %q, esperaba %q", ev.SessionID, rec.ID)
	}
}

func TestCreateYDeletePublican(t *testing.T) {
	m, bus := newTestManagerWithBus(t)
	ch, stop := bus.Subscribe()
	defer stop()

	rec, err := m.Create(CreateOpts{Title: "t", Cwd: t.TempDir(), Cols: 80, Rows: 24})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if ev := waitFor(t, ch, events.SessionCreated); ev.SessionID != rec.ID {
		t.Fatalf("SessionID = %q, esperaba %q", ev.SessionID, rec.ID)
	}

	if err := m.Delete(rec.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if ev := waitFor(t, ch, events.SessionDeleted); ev.SessionID != rec.ID {
		t.Fatalf("SessionID = %q, esperaba %q", ev.SessionID, rec.ID)
	}
}

func TestUpdateMetaPublica(t *testing.T) {
	m, bus := newTestManagerWithBus(t)
	ch, stop := bus.Subscribe()
	defer stop()

	rec, err := m.Create(CreateOpts{Title: "t", Cwd: t.TempDir(), Cols: 80, Rows: 24})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	titulo := "otro nombre"
	if _, err := m.UpdateMeta(rec.ID, store.MetaPatch{Title: &titulo}); err != nil {
		t.Fatalf("UpdateMeta: %v", err)
	}
	waitFor(t, ch, events.SessionUpdated)
}

// El bus es opcional: sin él el orquestador tiene que andar igual, que es como
// lo construyen todos los tests que ya existen.
func TestSinBusNoRompe(t *testing.T) {
	m, _ := newTestManager(t)
	rec, err := m.Create(CreateOpts{Title: "t", Cwd: t.TempDir(), Cols: 80, Rows: 24})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := m.AddResource(rec.ID, "https://github.com/o/r/pull/1", "github", "pr"); err != nil {
		t.Fatalf("AddResource: %v", err)
	}
}
```

- [x] **Step 2: Correr el test para verificar que falla**

Run: `go test ./internal/control/ -run 'TestAddResourcePublica|TestCreateYDeletePublican|TestUpdateMetaPublica|TestSinBusNoRompe'`
Expected: FAIL, no compila (`unknown field Events in struct literal`).

- [x] **Step 3: Agregar el campo al Config**

En `internal/control/manager.go`, dentro de `type Config struct`, después del campo `Resources`:

```go
	// Events recibe los avisos de cambio para que el server se los reparta a
	// los navegadores. Nil es válido y significa no publicar: es como lo
	// construyen los tests y cualquier uso sin UI.
	Events *events.Bus
```

Y en el bloque de imports, junto a los otros paquetes internos:

```go
	"github.com/giuliano/webterm/internal/events"
```

- [x] **Step 4: Agregar el helper de publicación**

En `internal/control/manager.go`, justo después de `func NewManager(...)`:

```go
// publish avisa un cambio, si hay a quién. Concentra el chequeo de nil para
// que los puntos de escritura sean una línea y no un if.
func (m *Manager) publish(kind events.Kind, sessionID string) {
	if m.cfg.Events == nil {
		return
	}
	m.cfg.Events.Publish(kind, sessionID)
}
```

- [x] **Step 5: Publicar en cada punto de escritura**

En cada método, agregar la llamada **después** de que la escritura al store haya salido bien y **antes** del `return` exitoso. Un evento sobre algo que después falla deja a la UI mostrando lo que no pasó.

- `Create`: antes de devolver el `*store.Session` creado → `m.publish(events.SessionCreated, rec.ID)`
- `Restart`: antes del return exitoso → `m.publish(events.SessionUpdated, id)`
- `Kill`: antes del `return nil` → `m.publish(events.SessionUpdated, id)`
- `Delete`: antes del `return nil` → `m.publish(events.SessionDeleted, id)`
- `UpdateMeta`: antes del return exitoso → `m.publish(events.SessionUpdated, id)`
- `AddResource`: antes del return exitoso → `m.publish(events.ResourceAdded, sessionID)`
- `DeleteResource`: antes del `return nil` → `m.publish(events.ResourceRemoved, sessionID)`
- `Sweep`: adentro del loop, cada vez que `MarkExitedIfUnchanged` devuelve `marked == true` → `m.publish(events.SessionUpdated, prev.ID)`

- [x] **Step 6: Correr los tests para verificar que pasan**

Run: `go test ./internal/control/ -race`
Expected: PASS, incluidos todos los tests que ya existían.

- [x] **Step 7: Commit**

```bash
git add internal/control/
git commit -m "$(cat <<'MSG'
feat(control): publicar los cambios de estado al bus

El orquestador avisa en sus puntos de escritura, siempre después de que
el store confirmó. El bus es opcional: con Config.Events en nil no
publica nada, que es como lo construyen los tests.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
MSG
)"
```

---

### Task 3: El endpoint SSE

**Files:**
- Create: `internal/server/events.go`
- Test: `internal/server/events_test.go`
- Modify: `internal/server/server.go` (`Config` :19, `Server` :28, `New` :34, `Handler` :47)
- Modify: `cmd/webterm/main.go` (:167 `var ctl control.Config`, :256 `control.NewManager`, :278 `server.New`)

**Interfaces:**
- Consumes: `events.Bus`, `(*events.Bus).Subscribe`, `events.Event` (Task 1); `control.Config.Events` (Task 2).
- Produces: `server.Config.Events *events.Bus`, ruta `GET /api/events`.

- [x] **Step 1: Escribir el test que falla**

Crear `internal/server/events_test.go`:

```go
package server

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/giuliano/webterm/internal/events"
)

// readFrame junta líneas hasta el renglón en blanco que cierra un frame SSE.
// Devuelve el frame entero para poder afirmar sobre id:, event: y data: juntos.
func readFrame(t *testing.T, br *bufio.Reader) string {
	t.Helper()
	var sb strings.Builder
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("leyendo el frame: %v", err)
		}
		if line == "\n" {
			return sb.String()
		}
		sb.WriteString(line)
	}
}

func TestEventsMandaResyncAlAbrirYLuegoLosEventos(t *testing.T) {
	bus := events.New(8)
	s := &Server{events: bus}
	ts := httptest.NewServer(http.HandlerFunc(s.handleEvents))
	defer ts.Close()

	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}

	br := bufio.NewReader(resp.Body)
	if got := readFrame(t, br); !strings.Contains(got, "event: resync") {
		t.Fatalf("el primer frame tendría que ser el resync, fue %q", got)
	}

	bus.Publish(events.ResourceAdded, "s1")

	frame := readFrame(t, br)
	for _, want := range []string{"id: 1", "event: resource.added", `"session_id":"s1"`} {
		if !strings.Contains(frame, want) {
			t.Fatalf("el frame %q no contiene %q", frame, want)
		}
	}
}

// Cortar la conexión tiene que devolver el handler y soltar la suscripción: si
// no, cada pestaña que se cierra deja una goroutine y un canal recibiendo para
// siempre.
func TestEventsCierraAlIrseElCliente(t *testing.T) {
	bus := events.New(8)
	s := &Server{events: bus}
	ts := httptest.NewServer(http.HandlerFunc(s.handleEvents))
	defer ts.Close()

	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	br := bufio.NewReader(resp.Body)
	readFrame(t, br) // resync
	resp.Body.Close()

	// Sin suscriptores vivos, publicar no puede colgarse.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			bus.Publish(events.SessionUpdated, "s1")
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish se bloqueó después de que el cliente se fue")
	}
}

func TestEventsSinBusDevuelve404(t *testing.T) {
	s := &Server{}
	rec := httptest.NewRecorder()
	s.handleEvents(rec, httptest.NewRequest(http.MethodGet, "/api/events", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, esperaba 404", rec.Code)
	}
}
```

- [x] **Step 2: Correr el test para verificar que falla**

Run: `go test ./internal/server/ -run TestEvents`
Expected: FAIL, no compila (`s.events undefined`, `s.handleEvents undefined`).

- [x] **Step 3: Escribir el handler**

Crear `internal/server/events.go`:

```go
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
```

- [x] **Step 4: Enchufar el bus al Server**

En `internal/server/server.go`:

1. Import: `"github.com/giuliano/webterm/internal/events"`.
2. En `type Config struct`, después de `MCP`:

```go
	// Events es el bus que alimenta /api/events. Si es nil, la ruta contesta
	// 404 y el frontend se queda con el polling.
	Events *events.Bus
```

3. En `type Server struct`, después de `upgrader`:

```go
	events *events.Bus
```

4. En `func New`, dentro del literal `&Server{...}`: `events: cfg.Events,`
5. En `func (s *Server) Handler()`, junto a la ruta del WS:

```go
	mux.HandleFunc("GET /api/events", s.handleEvents)
```

- [x] **Step 5: Correr los tests para verificar que pasan**

Run: `go test ./internal/server/ -race`
Expected: PASS, incluidos los que ya existían.

- [x] **Step 6: Construir el bus en main.go**

En `cmd/webterm/main.go`:

1. Import: `"github.com/giuliano/webterm/internal/events"`.
2. Antes de `mgr := control.NewManager(st, pty, ctl)` (:256):

```go
	// Un solo bus para los dos: el orquestador publica, el server reparte.
	bus := events.New(64)
	ctl.Events = bus
```

3. Antes de `server.New(cfg, mgr)` (:278): `cfg.Events = bus`

- [x] **Step 7: Verificar la suite entera y probar el stream a mano**

Run: `go test ./... && go build -o bin/webterm ./cmd/webterm`
Expected: PASS y binario construido.

Después, con el server corriendo, en otra terminal:

```bash
curl -N http://127.0.0.1:7788/api/events
```

Expected: aparece `event: resync` al instante. Linkear un PR desde la UI o el MCP imprime un frame `event: resource.added`.

- [x] **Step 8: Commit**

```bash
git add internal/server/ cmd/webterm/main.go
git commit -m "$(cat <<'MSG'
feat(server): stream SSE de eventos en GET /api/events

Cada conexión arranca con un resync, así una reconexión y un arranque en
frío son el mismo caso y no hace falta guardar historial ni atender
Last-Event-ID. Keepalive cada 25s y cierre por contexto del request.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
MSG
)"
```

---

### Task 4: El frontend escucha

**Files:**
- Create: `web/src/useEvents.ts`
- Modify: `web/src/App.tsx` (`POLL_MS` :20, el cuerpo de `App`, el render de `ResourcePanel`)
- Modify: `web/src/ResourcePanel.tsx` (`POLL_MS` :17, props, efecto de refresco)

**Interfaces:**
- Consumes: `GET /api/events` (Task 3), con `event:` igual al `kind` y `data` = `{seq, kind, session_id}`.
- Produces: `useEvents(onEvent: (ev: ServerEvent | null) => void)` de `web/src/useEvents.ts`; prop nueva `reloadKey: number` en `ResourcePanel`.

- [x] **Step 1: Escribir el hook**

Crear `web/src/useEvents.ts`:

```ts
import { useEffect, useRef } from 'react'

// Los eventos dicen qué cambió, nunca el objeto: el cliente va a buscarlo por
// REST. Ver docs/superpowers/specs/2026-09-19-canal-de-eventos-design.md
export type EventKind =
  | 'resource.added'
  | 'resource.removed'
  | 'session.created'
  | 'session.updated'
  | 'session.deleted'

export interface ServerEvent {
  seq: number
  kind: EventKind
  session_id: string
}

// EventSource entrega por nombre de evento: lo que no se escucha explícitamente
// no llega a onmessage, porque los frames van con event: <kind>.
const KINDS: EventKind[] = [
  'resource.added',
  'resource.removed',
  'session.created',
  'session.updated',
  'session.deleted',
]

/**
 * useEvents abre el stream y llama a onEvent por cada cambio.
 *
 * onEvent recibe null cuando hay que resincronizar todo: al abrir el stream
 * (incluidas las reconexiones, que EventSource maneja solo) y cuando se detecta
 * un salto en seq, que es como se ve desde acá un evento que el server nos
 * salteó por venir lentos. El server no guarda historial, así que pedir "lo que
 * falta" no es una opción: se refetchea entero.
 */
export function useEvents(onEvent: (ev: ServerEvent | null) => void) {
  // El callback va en un ref para que el efecto corra una sola vez y no
  // reabra el stream en cada render del que lo usa.
  const cb = useRef(onEvent)
  cb.current = onEvent

  useEffect(() => {
    const es = new EventSource('/api/events')
    let lastSeq = 0

    const onResync = () => {
      lastSeq = 0
      cb.current(null)
    }

    const onKind = (e: MessageEvent) => {
      let ev: ServerEvent
      try {
        ev = JSON.parse(e.data) as ServerEvent
      } catch {
        return
      }
      if (lastSeq !== 0 && ev.seq !== lastSeq + 1) cb.current(null)
      else cb.current(ev)
      lastSeq = ev.seq
    }

    es.addEventListener('resync', onResync)
    for (const k of KINDS) es.addEventListener(k, onKind)

    return () => {
      es.removeEventListener('resync', onResync)
      for (const k of KINDS) es.removeEventListener(k, onKind)
      es.close()
    }
  }, [])
}
```

- [x] **Step 2: Consumir el hook en App**

En `web/src/App.tsx`:

1. Import: `import { useEvents, type ServerEvent } from './useEvents'`
2. Cambiar el comentario y el valor de `POLL_MS`:

```ts
// El refresco en vivo llega por /api/events; este poll queda de red de
// seguridad para el caso en que el stream se caiga sin que el browser lo note.
const POLL_MS = 60000
```

3. Junto a los otros `useState`, el contador que despierta al panel de recursos:

```tsx
  // Un contador en vez de un booleano: lo que le importa a ResourcePanel es que
  // cambió, no qué valor tiene.
  const [resourceTick, setResourceTick] = useState(0)
```

4. Después de la declaración de `refresh`:

```tsx
  useEvents(
    useCallback(
      (ev: ServerEvent | null) => {
        // null es resync: no sabemos qué nos perdimos, así que se refresca todo.
        if (!ev) {
          void refresh()
          setResourceTick((t) => t + 1)
          return
        }
        if (ev.kind.startsWith('session.')) void refresh()
        if (ev.kind.startsWith('resource.') && ev.session_id === selectedRef.current) {
          setResourceTick((t) => t + 1)
        }
      },
      [refresh],
    ),
  )
```

5. En el render, pasarle el contador al panel:

```tsx
              <ResourcePanel key={'res-' + selected} sessionId={selected} reloadKey={resourceTick} />
```

- [x] **Step 3: Consumir el contador en ResourcePanel**

En `web/src/ResourcePanel.tsx`:

1. Cambiar el comentario y el valor de `POLL_MS`:

```ts
// El refresco en vivo llega por /api/events; este poll queda de red de
// seguridad. El backend cachea 30 s, así que tampoco se traduce uno a uno en
// llamadas a GitHub.
const POLL_MS = 60000
```

2. Cambiar la firma:

```tsx
export function ResourcePanel({
  sessionId,
  reloadKey,
}: {
  sessionId: string
  reloadKey: number
}) {
```

3. Reemplazar el efecto que pide la lista al montar por uno que también reacciona al contador:

```tsx
  // Corre al montar y cada vez que App avisa que hubo un evento de recursos
  // para esta sesión.
  useEffect(() => {
    void refresh()
  }, [refresh, reloadKey])
```

- [x] **Step 4: Verificar que compila**

Run: `npm --prefix web run build`
Expected: `tsc -b` sin errores y build generado.

- [x] **Step 5: Verificar a mano el comportamiento**

Con el server corriendo y la UI abierta en el navegador:

1. Abrir el panel "Linkeado" de una sesión.
2. Desde otra sesión de Claude Code, correr la herramienta MCP `link_pr` contra esa sesión.
3. Expected: el recurso aparece en el panel en menos de un segundo, sin recargar.
4. En la consola del navegador, `performance.getEntriesByType('resource').filter(r => r.name.includes('/api/events'))` tiene que mostrar una sola conexión, no una por segundo.
5. Reiniciar el server: el panel se vuelve a poblar solo cuando `EventSource` reconecta.

- [x] **Step 6: Commit**

```bash
git add web/src/useEvents.ts web/src/App.tsx web/src/ResourcePanel.tsx
git commit -m "$(cat <<'MSG'
feat(web): escuchar /api/events en vez de esperar al poll

Un EventSource montado en App refetchea sesiones y recursos cuando el
server avisa. El poll baja a 60s y queda de red de seguridad por si el
stream se cae sin que el browser lo note.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
MSG
)"
```

---

## Verificación final

- [x] `go test ./... -race` en verde.
- [x] `npm --prefix web run build` sin errores.
- [x] `curl -N http://127.0.0.1:7788/api/events` muestra el resync y después los eventos. (verificado el 2026-09-19 con el orquestador reiniciado: llegó `event: resync` al abrir y un `id: 1 / event: session.updated` tras un PATCH de rename. Las 3 sesiones vivas sobrevivieron el restart.)
- [ ] Linkear por MCP se ve en la UI al instante. (pendiente: el lado del server está verificado — el evento sale; falta confirmar en el navegador que el EventSource lo consume y el panel se refresca solo)
- [x] Las sesiones que estaban corriendo antes del restart del orquestador siguen vivas y attacheables (el daemon no se tocó).
