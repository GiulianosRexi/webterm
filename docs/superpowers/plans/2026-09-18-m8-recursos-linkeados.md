# M8 — Recursos linkeados: plan de implementación

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** poder linkearle PRs de GitHub a una sesión y ver su estado —review, checks, comments sin resolver— refrescado desde la propia UI.

**Architecture:** un paquete `internal/resources` que no conoce ni la DB ni las sesiones: dada una URL devuelve un `Ref`, y dado un `Ref` devuelve un `Snapshot`. El link se persiste en una tabla nueva; el estado vive en un caché con TTL en memoria. El provider de GitHub shellea a `gh api graphql` a través de un `runner` inyectable, lo que deja todo testeable sin red.

**Tech Stack:** Go 1.25, SQLite (`modernc.org/sqlite`), el `gh` CLI para credenciales, React + TypeScript.

**Spec:** `docs/superpowers/specs/2026-09-18-m8-recursos-linkeados-design.md`

## Global Constraints

- **Comentarios y mensajes de commit en español**, explicando el *porqué*. Identificadores en inglés.
- **El estado externo nunca se persiste en SQLite.** Solo el link.
- **`resources` no importa `store` ni `session`.** Si aparece esa necesidad, el diseño está mal.
- **Ningún test toca la red**, salvo el único de integración marcado con `t.Skip` si falta `gh`.
- **TTL del caché: 30 s** para respuestas buenas, **10 s** para errores.
- **Timeout del subproceso `gh`: 10 s**, vía `context.WithTimeout`.
- **`go test ./... -race` verde** es la condición de cierre de cada tarea.

## Estructura de archivos

| Archivo | Responsabilidad |
|---|---|
| `internal/store/schema.go` | (modificar) migración v2 con `session_resources` |
| `internal/store/resources.go` | CRUD de `session_resources` |
| `internal/resources/resources.go` | tipos `Ref`/`Snapshot`/`PRState`, `Provider`, `Registry` |
| `internal/resources/github.go` | matching de URL y provider de PRs |
| `internal/resources/parse.go` | respuesta de GraphQL → `PRState` |
| `internal/resources/cache.go` | caché con TTL y single-flight |
| `internal/session/manager.go` | (modificar) ABM de recursos que consume el server |
| `internal/server/resources.go` | endpoints REST |
| `web/src/ResourcePanel.tsx` | panel desplegable y card de PR |
| `web/src/api.ts` | (modificar) cliente de los endpoints nuevos |

## Hallazgos de la verificación previa

Confirmados contra la API real antes de escribir esto; el plan depende de ellos:

1. **Una sola query trae todo.** `state`, `isDraft`, `mergeable`, `reviewDecision`, `statusCheckRollup` y `reviewThreads` en una llamada.
2. **`statusCheckRollup` puede venir `null`** — pasa en PRs viejos cuyos checks expiraron. Hay que mapearlo, no romper.
3. **Ante un error de GraphQL, `gh` escribe el JSON con la clave `errors` en stdout** y además un mensaje humano en stderr. Parsear stdout y mirar `errors` es más confiable que mirar solo el exit code.
4. **Los contextos de checks son de dos tipos**: `CheckRun` (con `name`/`conclusion`/`status`) y `StatusContext` (con `context`/`state`). Hay que contemplar los dos.

---

### Task 1: Store — tabla `session_resources`

**Files:**
- Modify: `internal/store/schema.go`
- Create: `internal/store/resources.go`
- Create: `internal/store/resources_test.go`

**Interfaces:**
- Consumes: `(*Store).db`, `(*Store).mu`, `ErrNotFound`, `execAffecting` (M2).
- Produces:
  - `type Resource struct { ID int64; SessionID, System, Type, Ref string; CreatedAt int64 }`
  - `(*Store).AddResource(r *Resource) error` — `ErrDuplicate` si ya está
  - `(*Store).ListResources(sessionID string) ([]*Resource, error)`
  - `(*Store).DeleteResource(sessionID string, id int64) error`
  - `(*Store).SessionsForRef(ref string) ([]string, error)`
  - `var ErrDuplicate = errors.New("ya existe")`

- [ ] **Step 1: Escribir los tests que fallan**

`internal/store/resources_test.go`:

```go
package store

import (
	"errors"
	"testing"
)

func sampleResource(sessionID, ref string) *Resource {
	return &Resource{SessionID: sessionID, System: "gh", Type: "pr", Ref: ref}
}

func TestAddYListResources(t *testing.T) {
	st := newTestStore(t)
	_ = st.CreateSession(sampleSession("s1"))

	r := sampleResource("s1", "https://github.com/o/r/pull/1")
	if err := st.AddResource(r); err != nil {
		t.Fatalf("AddResource: %v", err)
	}
	if r.ID == 0 || r.CreatedAt == 0 {
		t.Fatalf("AddResource tiene que completar id y timestamp: %+v", r)
	}

	list, err := st.ListResources("s1")
	if err != nil {
		t.Fatalf("ListResources: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("vinieron %d recursos", len(list))
	}
	if list[0].System != "gh" || list[0].Type != "pr" || list[0].Ref != r.Ref {
		t.Fatalf("se guardó mal: %+v", list[0])
	}
}

func TestListResourcesVacio(t *testing.T) {
	st := newTestStore(t)
	_ = st.CreateSession(sampleSession("s1"))

	list, err := st.ListResources("s1")
	if err != nil {
		t.Fatalf("ListResources: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("una sesión sin recursos tiene que dar lista vacía, dio %d", len(list))
	}
}

// TestAddResourceDuplicado: linkear dos veces el mismo PR a la misma sesión es
// un error del usuario, no una fila repetida en la UI.
func TestAddResourceDuplicado(t *testing.T) {
	st := newTestStore(t)
	_ = st.CreateSession(sampleSession("s1"))
	ref := "https://github.com/o/r/pull/1"

	if err := st.AddResource(sampleResource("s1", ref)); err != nil {
		t.Fatalf("AddResource 1: %v", err)
	}
	if err := st.AddResource(sampleResource("s1", ref)); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("se esperaba ErrDuplicate, vino %v", err)
	}
}

// TestMismoRefEnDosSesiones: el mismo PR sí puede estar en varias sesiones.
func TestMismoRefEnDosSesiones(t *testing.T) {
	st := newTestStore(t)
	_ = st.CreateSession(sampleSession("s1"))
	_ = st.CreateSession(sampleSession("s2"))
	ref := "https://github.com/o/r/pull/1"

	if err := st.AddResource(sampleResource("s1", ref)); err != nil {
		t.Fatalf("AddResource s1: %v", err)
	}
	if err := st.AddResource(sampleResource("s2", ref)); err != nil {
		t.Fatalf("el mismo ref en otra sesión tiene que andar: %v", err)
	}
}

func TestDeleteResource(t *testing.T) {
	st := newTestStore(t)
	_ = st.CreateSession(sampleSession("s1"))
	r := sampleResource("s1", "https://github.com/o/r/pull/1")
	_ = st.AddResource(r)

	if err := st.DeleteResource("s1", r.ID); err != nil {
		t.Fatalf("DeleteResource: %v", err)
	}
	if err := st.DeleteResource("s1", r.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("borrar dos veces tiene que dar ErrNotFound, vino %v", err)
	}
}

// TestDeleteResourceDeOtraSesion: el id es global, así que el borrado tiene
// que filtrar por sesión o una sesión podría borrar recursos de otra.
func TestDeleteResourceDeOtraSesion(t *testing.T) {
	st := newTestStore(t)
	_ = st.CreateSession(sampleSession("s1"))
	_ = st.CreateSession(sampleSession("s2"))
	r := sampleResource("s1", "https://github.com/o/r/pull/1")
	_ = st.AddResource(r)

	if err := st.DeleteResource("s2", r.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("se esperaba ErrNotFound, vino %v", err)
	}
	list, _ := st.ListResources("s1")
	if len(list) != 1 {
		t.Fatal("el recurso se borró desde otra sesión")
	}
}

// TestSessionsForRef es la búsqueda inversa que motiva que esto sea una tabla
// y no un blob JSON: dado un PR, qué sesiones se prenden.
func TestSessionsForRef(t *testing.T) {
	st := newTestStore(t)
	_ = st.CreateSession(sampleSession("s1"))
	_ = st.CreateSession(sampleSession("s2"))
	_ = st.CreateSession(sampleSession("s3"))
	ref := "https://github.com/o/r/pull/1"
	_ = st.AddResource(sampleResource("s1", ref))
	_ = st.AddResource(sampleResource("s3", ref))
	_ = st.AddResource(sampleResource("s2", "https://github.com/o/r/pull/2"))

	ids, err := st.SessionsForRef(ref)
	if err != nil {
		t.Fatalf("SessionsForRef: %v", err)
	}
	if len(ids) != 2 || ids[0] != "s1" || ids[1] != "s3" {
		t.Fatalf("SessionsForRef = %v", ids)
	}
}

func TestResourcesCascadaAlBorrarSesion(t *testing.T) {
	st := newTestStore(t)
	_ = st.CreateSession(sampleSession("s1"))
	_ = st.AddResource(sampleResource("s1", "https://github.com/o/r/pull/1"))

	if err := st.DeleteSession("s1"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	var n int
	_ = st.DB().QueryRow(`SELECT COUNT(*) FROM session_resources`).Scan(&n)
	if n != 0 {
		t.Fatalf("quedaron %d recursos huérfanos", n)
	}
}
```

- [ ] **Step 2: Correr los tests y verificar que fallan**

Run: `go test ./internal/store/ -run Resource`
Expected: FAIL, no compila (`undefined: Resource`, `st.AddResource undefined`).

- [ ] **Step 3: Agregar la migración v2**

En `internal/store/schema.go`, cambiar la lista y agregar la constante:

```go
var migrations = []string{schemaV1, schemaV2}
```

```go
// schemaV2 agrega los recursos externos linkeados a una sesión (M8).
//
// Es tabla y no un campo JSON en sessions por la búsqueda inversa: dado un PR
// hay que resolver qué sesiones se prenden, y sobre un blob eso sería un scan
// completo de sessions más parsear cada fila.
const schemaV2 = `
CREATE TABLE session_resources (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id TEXT    NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  system     TEXT    NOT NULL,
  type       TEXT    NOT NULL,
  ref        TEXT    NOT NULL,
  created_at INTEGER NOT NULL,
  UNIQUE (session_id, ref)
);

CREATE INDEX idx_resources_ref ON session_resources(ref);
CREATE INDEX idx_resources_session ON session_resources(session_id);
`
```

- [ ] **Step 4: Implementar el CRUD**

`internal/store/resources.go`:

```go
package store

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrDuplicate lo devuelve AddResource cuando el recurso ya está linkeado a
// esa sesión.
var ErrDuplicate = errors.New("ya existe")

// Resource es un recurso externo linkeado a una sesión. Solo guarda el link:
// el estado traído del sistema externo es caché en memoria y no se persiste.
type Resource struct {
	ID        int64  `json:"id"`
	SessionID string `json:"session_id"`
	System    string `json:"system"`
	Type      string `json:"type"`
	Ref       string `json:"ref"`
	CreatedAt int64  `json:"created_at"`
}

// AddResource linkea el recurso y completa ID y CreatedAt.
func (s *Store) AddResource(r *Resource) error {
	if r.CreatedAt == 0 {
		r.CreatedAt = time.Now().UnixMilli()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`
		INSERT INTO session_resources (session_id, system, type, ref, created_at)
		VALUES (?, ?, ?, ?, ?)`,
		r.SessionID, r.System, r.Type, r.Ref, r.CreatedAt)
	if err != nil {
		// modernc/sqlite no expone un código tipado para la violación de
		// UNIQUE, así que se reconoce por el texto del error.
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return ErrDuplicate
		}
		return fmt.Errorf("linkeando %s a %s: %w", r.Ref, r.SessionID, err)
	}
	r.ID, err = res.LastInsertId()
	return err
}

// ListResources devuelve los recursos de una sesión, el más viejo primero:
// el orden en que los fuiste linkeando es el que tiene sentido para leerlos.
func (s *Store) ListResources(sessionID string) ([]*Resource, error) {
	rows, err := s.db.Query(`
		SELECT id, session_id, system, type, ref, created_at
		FROM session_resources WHERE session_id = ? ORDER BY id`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("listando recursos de %s: %w", sessionID, err)
	}
	defer rows.Close()

	out := []*Resource{}
	for rows.Next() {
		var r Resource
		if err := rows.Scan(&r.ID, &r.SessionID, &r.System, &r.Type, &r.Ref, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}

// DeleteResource borra el link. Filtra por sesión además de por id: el id es
// global, y sin ese filtro una sesión podría borrar recursos de otra.
func (s *Store) DeleteResource(sessionID string, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.execAffecting(`DELETE FROM session_resources WHERE session_id = ? AND id = ?`,
		sessionID, id)
}

// SessionsForRef devuelve las sesiones que tienen linkeado ese recurso. Es la
// búsqueda inversa que va a necesitar cualquier notificación entrante.
func (s *Store) SessionsForRef(ref string) ([]string, error) {
	rows, err := s.db.Query(`
		SELECT session_id FROM session_resources WHERE ref = ? ORDER BY session_id`, ref)
	if err != nil {
		return nil, fmt.Errorf("buscando sesiones de %s: %w", ref, err)
	}
	defer rows.Close()

	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
```

- [ ] **Step 5: Correr los tests y verificar que pasan**

Run: `go test ./internal/store/ -race -count=1`
Expected: PASS, incluidos los de M2 (la migración v2 se aplica sobre bases que ya tenían v1).

- [ ] **Step 6: Commit**

```bash
git add internal/store/
git commit -m "feat(store): tabla de recursos externos linkeados a una sesión

Tabla y no blob JSON por la búsqueda inversa: dado un PR hay que
resolver qué sesiones se prenden, y sobre JSON sería un scan completo."
```

---

### Task 2: Resources — tipos, registry y matching de URL

**Files:**
- Create: `internal/resources/resources.go`
- Create: `internal/resources/github.go`
- Create: `internal/resources/match_test.go`

**Interfaces:**
- Consumes: nada.
- Produces:
  - `type Ref struct { System, Type, URL string }`
  - `type PRState struct { … }` (campos abajo)
  - `type Snapshot struct { FetchedAt int64; Error string; PR *PRState }`
  - `type Provider interface { Match(rawURL string) (Ref, bool); Fetch(ctx context.Context, ref Ref) (*Snapshot, error) }`
  - `type Registry struct{ … }`, `NewRegistry(ps ...Provider) *Registry`
  - `(*Registry).Resolve(rawURL string) (Ref, Provider, bool)`
  - `type GitHub struct { … }`, `NewGitHub() *GitHub`
  - `(*GitHub).Match(rawURL string) (Ref, bool)`
  - `var ErrUnknownResource = errors.New("no se reconoce la URL")`

- [ ] **Step 1: Escribir los tests que fallan**

`internal/resources/match_test.go`:

```go
package resources

import "testing"

// TestMatchPRNormaliza: se acepta cualquier forma en que se copie la URL de un
// PR desde el browser, y se guarda siempre la canónica.
func TestMatchPRNormaliza(t *testing.T) {
	gh := NewGitHub()
	canonica := "https://github.com/cli/cli/pull/14456"

	entradas := []string{
		"https://github.com/cli/cli/pull/14456",
		"http://github.com/cli/cli/pull/14456",
		"github.com/cli/cli/pull/14456",
		"https://github.com/cli/cli/pull/14456/files",
		"https://github.com/cli/cli/pull/14456/commits/abc123",
		"https://github.com/cli/cli/pull/14456#discussion_r123456",
		"https://github.com/cli/cli/pull/14456?w=1",
		"  https://github.com/cli/cli/pull/14456  ",
		"https://www.github.com/cli/cli/pull/14456",
	}
	for _, in := range entradas {
		ref, ok := gh.Match(in)
		if !ok {
			t.Errorf("no reconoció %q", in)
			continue
		}
		if ref.URL != canonica {
			t.Errorf("%q se normalizó a %q, se esperaba %q", in, ref.URL, canonica)
		}
		if ref.System != "gh" || ref.Type != "pr" {
			t.Errorf("%q dio system=%q type=%q", in, ref.System, ref.Type)
		}
	}
}

// TestMatchRechaza: mejor rechazar claro que adivinar.
func TestMatchRechaza(t *testing.T) {
	gh := NewGitHub()
	entradas := []string{
		"",
		"no es una url",
		"https://github.com/cli/cli",
		"https://github.com/cli/cli/issues/14456",
		"https://github.com/cli/cli/pull/",
		"https://github.com/cli/cli/pull/abc",
		"https://gitlab.com/cli/cli/pull/1",
		"https://github.com.evil.example/cli/cli/pull/1",
		"https://github.com/cli/cli/pull/-1",
	}
	for _, in := range entradas {
		if ref, ok := gh.Match(in); ok {
			t.Errorf("aceptó %q como %+v", in, ref)
		}
	}
}

func TestParseRefDevuelveOwnerRepoNumero(t *testing.T) {
	gh := NewGitHub()
	ref, ok := gh.Match("https://github.com/cli/cli/pull/14456")
	if !ok {
		t.Fatal("no matcheó")
	}
	owner, repo, number, err := parsePRRef(ref)
	if err != nil {
		t.Fatalf("parsePRRef: %v", err)
	}
	if owner != "cli" || repo != "cli" || number != 14456 {
		t.Fatalf("parsePRRef = %s/%s#%d", owner, repo, number)
	}
}

func TestRegistryResolve(t *testing.T) {
	reg := NewRegistry(NewGitHub())

	ref, p, ok := reg.Resolve("https://github.com/cli/cli/pull/14456")
	if !ok {
		t.Fatal("el registry no resolvió un PR de GitHub")
	}
	if p == nil {
		t.Fatal("resolvió sin provider")
	}
	if ref.System != "gh" || ref.Type != "pr" {
		t.Fatalf("ref = %+v", ref)
	}

	if _, _, ok := reg.Resolve("https://ejemplo.invalido/algo"); ok {
		t.Fatal("el registry aceptó una URL que ningún provider reconoce")
	}
}
```

- [ ] **Step 2: Correr los tests y verificar que fallan**

Run: `go test ./internal/resources/`
Expected: FAIL, no compila (`undefined: NewGitHub`, `undefined: NewRegistry`).

- [ ] **Step 3: Escribir los tipos y el registry**

`internal/resources/resources.go`:

```go
// Package resources traduce URLs de sistemas externos a referencias
// tipadas, y esas referencias al estado actual del recurso.
//
// No conoce ni la base ni las sesiones a propósito: dada una URL devuelve un
// Ref, y dado un Ref devuelve un Snapshot. Eso lo hace testeable sin DB y sin
// red, y deja que el único lugar que sabe de sesiones sea el manager.
package resources

import (
	"context"
	"errors"
)

// ErrUnknownResource lo devuelve el registry cuando ningún provider reconoce
// la URL.
var ErrUnknownResource = errors.New("no se reconoce la URL")

// Ref identifica un recurso externo.
type Ref struct {
	System string `json:"system"` // gh
	Type   string `json:"type"`   // pr
	URL    string `json:"url"`    // URL canónica, ya normalizada
}

// PRState es el estado de un pull request de GitHub.
type PRState struct {
	Number         int    `json:"number"`
	Title          string `json:"title"`
	Author         string `json:"author"`
	State          string `json:"state"`           // OPEN | CLOSED | MERGED
	IsDraft        bool   `json:"is_draft"`
	Mergeable      string `json:"mergeable"`       // MERGEABLE | CONFLICTING | UNKNOWN
	ReviewDecision string `json:"review_decision"` // APPROVED | CHANGES_REQUESTED | REVIEW_REQUIRED | ""

	UnresolvedCount  int  `json:"unresolved_count"`
	ThreadsTruncated bool `json:"threads_truncated"`

	// ChecksState queda vacío cuando el PR no tiene checks o cuando ya
	// expiraron, que es lo que devuelve la API en PRs viejos.
	ChecksState   string `json:"checks_state"` // SUCCESS | FAILURE | PENDING | ""
	ChecksTotal   int    `json:"checks_total"`
	ChecksFailing int    `json:"checks_failing"`
}

// Snapshot es el estado de un recurso en un momento dado. Error y PR son
// excluyentes: si hubo error, no hay estado que mostrar.
type Snapshot struct {
	FetchedAt int64    `json:"fetched_at"`
	Error     string   `json:"error,omitempty"`
	PR        *PRState `json:"pr,omitempty"`
}

// Provider traduce URLs a refs y refs a estado.
type Provider interface {
	// Match reconoce la URL y la normaliza. El segundo valor dice si este
	// provider se hace cargo.
	Match(rawURL string) (Ref, bool)
	// Fetch consulta el estado actual del recurso.
	Fetch(ctx context.Context, ref Ref) (*Snapshot, error)
}

// Registry es el punto de extensión: sumar Linear o Slack es sumar un provider
// acá, sin tocar el contrato de la API ni el modelo de datos.
type Registry struct {
	providers []Provider
}

func NewRegistry(ps ...Provider) *Registry {
	return &Registry{providers: ps}
}

// Resolve encuentra el provider que reconoce la URL.
func (r *Registry) Resolve(rawURL string) (Ref, Provider, bool) {
	for _, p := range r.providers {
		if ref, ok := p.Match(rawURL); ok {
			return ref, p, true
		}
	}
	return Ref{}, nil, false
}

// ProviderFor devuelve el provider que maneja un ref ya guardado.
func (r *Registry) ProviderFor(ref Ref) (Provider, bool) {
	for _, p := range r.providers {
		if got, ok := p.Match(ref.URL); ok && got.System == ref.System {
			return p, true
		}
	}
	return nil, false
}
```

- [ ] **Step 4: Escribir el matching de GitHub**

`internal/resources/github.go`:

```go
package resources

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// prPath captura owner, repo y número de un path de PR. Lo que venga después
// del número (/files, /commits/..., el fragmento) se descarta al normalizar.
//
// El owner y el repo se restringen al alfabeto que GitHub permite, para que
// una URL rara no se cuele como si fuera un repo.
var prPath = regexp.MustCompile(`^/([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)/pull/([0-9]+)(?:/|$)`)

// GitHub es el provider de pull requests.
type GitHub struct {
	run runner
}

// Match reconoce una URL de PR y devuelve su forma canónica.
//
// Acepta lo que uno copia del browser: con o sin esquema, con www, con
// sufijos como /files o un fragmento de comment. Rechaza todo lo demás en vez
// de intentar adivinar.
func (g *GitHub) Match(rawURL string) (Ref, bool) {
	s := strings.TrimSpace(rawURL)
	if s == "" {
		return Ref{}, false
	}
	// Sin esquema, url.Parse mete todo en Path; se lo agregamos.
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := neturlParse(s)
	if err != nil {
		return Ref{}, false
	}
	host := strings.ToLower(u.Hostname())
	if host != "github.com" && host != "www.github.com" {
		return Ref{}, false
	}
	m := prPath.FindStringSubmatch(u.Path)
	if m == nil {
		return Ref{}, false
	}
	return Ref{
		System: "gh",
		Type:   "pr",
		URL:    fmt.Sprintf("https://github.com/%s/%s/pull/%s", m[1], m[2], m[3]),
	}, true
}

// parsePRRef vuelve de la URL canónica a sus partes, para armar la query.
func parsePRRef(ref Ref) (owner, repo string, number int, err error) {
	u, perr := neturlParse(ref.URL)
	if perr != nil {
		return "", "", 0, fmt.Errorf("ref inválido %q: %w", ref.URL, perr)
	}
	m := prPath.FindStringSubmatch(u.Path)
	if m == nil {
		return "", "", 0, fmt.Errorf("ref inválido %q", ref.URL)
	}
	n, perr := strconv.Atoi(m[3])
	if perr != nil {
		return "", "", 0, fmt.Errorf("número de PR inválido en %q", ref.URL)
	}
	return m[1], m[2], n, nil
}
```

Y arriba del archivo, el alias del parser (evita un import con nombre largo en
cada uso):

```go
import neturl "net/url"

var neturlParse = neturl.Parse
```

**Nota:** `runner` se define en la tarea 4. Para que esta tarea compile, agregar
por ahora en `github.go`:

```go
// runner ejecuta el gh CLI. La tarea 4 le da su implementación real; se
// inyecta para que los tests no dependan de la red.
type runner func(ctx context.Context, args ...string) ([]byte, error)

// NewGitHub construye el provider con el runner real.
func NewGitHub() *GitHub { return &GitHub{run: ghRunner} }
```

y un `ghRunner` provisorio que la tarea 4 reemplaza:

```go
func ghRunner(ctx context.Context, args ...string) ([]byte, error) {
	return nil, fmt.Errorf("el runner de gh se implementa en la tarea 4")
}
```

Agregar el import de `context` a `github.go`.

- [ ] **Step 5: Correr los tests y verificar que pasan**

Run: `go test ./internal/resources/ -v`
Expected: PASS en los cuatro tests.

- [ ] **Step 6: Commit**

```bash
git add internal/resources/
git commit -m "feat(resources): registry de providers y matching de URLs de PR

El registry es el punto de extensión real: sumar Linear es sumar un
provider, sin tocar el contrato de la API ni el modelo de datos."
```

---

### Task 3: Resources — parseo de la respuesta de GraphQL

**Files:**
- Create: `internal/resources/parse.go`
- Create: `internal/resources/parse_test.go`
- Create: `internal/resources/testdata/pr_open.json`
- Create: `internal/resources/testdata/pr_merged_sin_checks.json`
- Create: `internal/resources/testdata/pr_no_encontrado.json`

**Interfaces:**
- Consumes: `PRState` (tarea 2).
- Produces:
  - `parsePRResponse(body []byte) (*PRState, error)`
  - `var ErrRemote = errors.New("el sistema externo devolvió un error")`

- [ ] **Step 1: Crear las fixtures**

Son respuestas reales de la API, recortadas a lo que la query pide.

`internal/resources/testdata/pr_open.json` — PR abierto con checks:

```json
{"data":{"repository":{"pullRequest":{
  "number":14456,
  "title":"Refreshable tokens (7/7): guided end-to-end verification scripts",
  "url":"https://github.com/cli/cli/pull/14456",
  "state":"OPEN","isDraft":false,"mergeable":"MERGEABLE",
  "reviewDecision":"REVIEW_REQUIRED",
  "author":{"login":"babakks"},
  "reviewThreads":{"totalCount":0,"nodes":[]},
  "commits":{"nodes":[{"commit":{"statusCheckRollup":{
    "state":"FAILURE",
    "contexts":{"totalCount":4,"nodes":[
      {"__typename":"CheckRun","name":"label-external","conclusion":"SKIPPED","status":"COMPLETED"},
      {"__typename":"CheckRun","name":"build","conclusion":"SUCCESS","status":"COMPLETED"},
      {"__typename":"CheckRun","name":"test","conclusion":"FAILURE","status":"COMPLETED"},
      {"__typename":"StatusContext","context":"ci/legacy","state":"ERROR"}
    ]}
  }}}]}
}}}}
```

`internal/resources/testdata/pr_merged_sin_checks.json` — PR viejo cuyos checks
expiraron (`statusCheckRollup: null`) y con threads mixtos:

```json
{"data":{"repository":{"pullRequest":{
  "number":9000,
  "title":"proof of concept for flag-level disable auth check",
  "url":"https://github.com/cli/cli/pull/9000",
  "state":"MERGED","isDraft":false,"mergeable":"UNKNOWN",
  "reviewDecision":"APPROVED",
  "author":{"login":"andyfeller"},
  "reviewThreads":{"totalCount":2,"nodes":[{"isResolved":true},{"isResolved":false}]},
  "commits":{"nodes":[{"commit":{"statusCheckRollup":null}}]}
}}}}
```

`internal/resources/testdata/pr_no_encontrado.json` — lo que devuelve `gh` por
stdout cuando el PR no existe:

```json
{"data":{"repository":{"pullRequest":null}},"errors":[{"type":"NOT_FOUND","path":["repository","pullRequest"],"message":"Could not resolve to a PullRequest with the number of 99999999."}]}
```

- [ ] **Step 2: Escribir los tests que fallan**

`internal/resources/parse_test.go`:

```go
package resources

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("leyendo fixture %s: %v", name, err)
	}
	return b
}

func TestParsePROpenConChecks(t *testing.T) {
	pr, err := parsePRResponse(fixture(t, "pr_open.json"))
	if err != nil {
		t.Fatalf("parsePRResponse: %v", err)
	}
	if pr.Number != 14456 || pr.Author != "babakks" {
		t.Fatalf("cabecera mal: %+v", pr)
	}
	if pr.State != "OPEN" || pr.IsDraft || pr.Mergeable != "MERGEABLE" {
		t.Fatalf("estado mal: %+v", pr)
	}
	if pr.ReviewDecision != "REVIEW_REQUIRED" {
		t.Fatalf("review_decision = %q", pr.ReviewDecision)
	}
	if pr.ChecksState != "FAILURE" || pr.ChecksTotal != 4 {
		t.Fatalf("checks = %s %d", pr.ChecksState, pr.ChecksTotal)
	}
	// FAILURE del CheckRun y ERROR del StatusContext cuentan; SKIPPED no.
	if pr.ChecksFailing != 2 {
		t.Fatalf("checks_failing = %d, se esperaban 2", pr.ChecksFailing)
	}
	if pr.UnresolvedCount != 0 {
		t.Fatalf("unresolved = %d", pr.UnresolvedCount)
	}
}

// TestParsePRSinChecks: statusCheckRollup null es un caso real —PRs viejos con
// los checks expirados— y no puede romper el parseo.
func TestParsePRSinChecks(t *testing.T) {
	pr, err := parsePRResponse(fixture(t, "pr_merged_sin_checks.json"))
	if err != nil {
		t.Fatalf("parsePRResponse: %v", err)
	}
	if pr.State != "MERGED" || pr.ReviewDecision != "APPROVED" {
		t.Fatalf("estado mal: %+v", pr)
	}
	if pr.ChecksState != "" || pr.ChecksTotal != 0 || pr.ChecksFailing != 0 {
		t.Fatalf("sin checks tendría que quedar todo en cero: %+v", pr)
	}
	// De dos threads, uno sin resolver.
	if pr.UnresolvedCount != 1 {
		t.Fatalf("unresolved = %d, se esperaba 1", pr.UnresolvedCount)
	}
	if pr.ThreadsTruncated {
		t.Fatal("2 threads no se truncan")
	}
}

// TestParsePRErrorDeGraphQL: gh escribe el JSON con la clave errors en stdout
// aunque falle, así que el parseo tiene que mirar ahí y no solo el exit code.
func TestParsePRErrorDeGraphQL(t *testing.T) {
	_, err := parsePRResponse(fixture(t, "pr_no_encontrado.json"))
	if err == nil {
		t.Fatal("se esperaba un error")
	}
	if !errors.Is(err, ErrRemote) {
		t.Fatalf("se esperaba ErrRemote, vino %v", err)
	}
	if !strings.Contains(err.Error(), "99999999") {
		t.Fatalf("el mensaje de GitHub tiene que sobrevivir: %v", err)
	}
}

func TestParsePRBasura(t *testing.T) {
	if _, err := parsePRResponse([]byte("esto no es json")); err == nil {
		t.Fatal("se esperaba un error")
	}
}

// TestParsePRThreadsTruncados: con más de 100 threads la query trunca, y la
// card tiene que poder decir "100+" en vez de mentir con un número bajo.
func TestParsePRThreadsTruncados(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(`{"data":{"repository":{"pullRequest":{"number":1,"title":"t","url":"u",`)
	sb.WriteString(`"state":"OPEN","isDraft":false,"mergeable":"MERGEABLE","reviewDecision":"",`)
	sb.WriteString(`"author":{"login":"a"},"reviewThreads":{"totalCount":150,"nodes":[`)
	for i := 0; i < 100; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`{"isResolved":false}`)
	}
	sb.WriteString(`]},"commits":{"nodes":[{"commit":{"statusCheckRollup":null}}]}}}}}`)

	pr, err := parsePRResponse([]byte(sb.String()))
	if err != nil {
		t.Fatalf("parsePRResponse: %v", err)
	}
	if pr.UnresolvedCount != 100 || !pr.ThreadsTruncated {
		t.Fatalf("unresolved=%d truncated=%v", pr.UnresolvedCount, pr.ThreadsTruncated)
	}
}
```

- [ ] **Step 3: Correr los tests y verificar que fallan**

Run: `go test ./internal/resources/ -run TestParse`
Expected: FAIL, no compila (`undefined: parsePRResponse`).

- [ ] **Step 4: Implementar el parseo**

`internal/resources/parse.go`:

```go
package resources

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ErrRemote envuelve los errores que devuelve el sistema externo, para
// distinguirlos de los nuestros.
var ErrRemote = errors.New("el sistema externo devolvió un error")

// grahpQL response tipada contra la query de github.go.
type prResponse struct {
	Data struct {
		Repository *struct {
			PullRequest *struct {
				Number         int    `json:"number"`
				Title          string `json:"title"`
				URL            string `json:"url"`
				State          string `json:"state"`
				IsDraft        bool   `json:"isDraft"`
				Mergeable      string `json:"mergeable"`
				ReviewDecision string `json:"reviewDecision"`
				Author         *struct {
					Login string `json:"login"`
				} `json:"author"`
				ReviewThreads struct {
					TotalCount int `json:"totalCount"`
					Nodes      []struct {
						IsResolved bool `json:"isResolved"`
					} `json:"nodes"`
				} `json:"reviewThreads"`
				Commits struct {
					Nodes []struct {
						Commit struct {
							// Puede venir null: los checks de un PR viejo expiran.
							StatusCheckRollup *struct {
								State    string `json:"state"`
								Contexts struct {
									TotalCount int `json:"totalCount"`
									Nodes      []struct {
										TypeName   string `json:"__typename"`
										Conclusion string `json:"conclusion"`
										State      string `json:"state"`
									} `json:"nodes"`
								} `json:"contexts"`
							} `json:"statusCheckRollup"`
						} `json:"commit"`
					} `json:"nodes"`
				} `json:"commits"`
			} `json:"pullRequest"`
		} `json:"repository"`
	} `json:"data"`
	Errors []struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"errors"`
}

// conclusiones de CheckRun y estados de StatusContext que cuentan como falla.
// SKIPPED, NEUTRAL y CANCELLED no son fallas: un check salteado no rompe nada.
var checkFailures = map[string]bool{
	"FAILURE": true, "TIMED_OUT": true, "STARTUP_FAILURE": true,
	"ACTION_REQUIRED": true, "ERROR": true,
}

// parsePRResponse mapea la respuesta de GraphQL a PRState.
//
// Mira la clave `errors` aunque haya datos: gh escribe el JSON de error en
// stdout con exit code propio, así que confiar solo en el exit code deja
// pasar errores con un body que parsea bien.
func parsePRResponse(body []byte) (*PRState, error) {
	var res prResponse
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, fmt.Errorf("respuesta ilegible de GitHub: %w", err)
	}
	if len(res.Errors) > 0 {
		return nil, fmt.Errorf("%w: %s", ErrRemote, res.Errors[0].Message)
	}
	if res.Data.Repository == nil || res.Data.Repository.PullRequest == nil {
		return nil, fmt.Errorf("%w: no se encontró el pull request", ErrRemote)
	}
	pr := res.Data.Repository.PullRequest

	out := &PRState{
		Number:         pr.Number,
		Title:          pr.Title,
		State:          pr.State,
		IsDraft:        pr.IsDraft,
		Mergeable:      pr.Mergeable,
		ReviewDecision: pr.ReviewDecision,
	}
	if pr.Author != nil {
		out.Author = pr.Author.Login
	}

	for _, t := range pr.ReviewThreads.Nodes {
		if !t.IsResolved {
			out.UnresolvedCount++
		}
	}
	// La query pide first:100; si hay más, la cuenta es un piso, no el total.
	out.ThreadsTruncated = pr.ReviewThreads.TotalCount > len(pr.ReviewThreads.Nodes)

	if len(pr.Commits.Nodes) > 0 {
		if rollup := pr.Commits.Nodes[0].Commit.StatusCheckRollup; rollup != nil {
			out.ChecksState = rollup.State
			out.ChecksTotal = rollup.Contexts.TotalCount
			for _, c := range rollup.Contexts.Nodes {
				// CheckRun trae conclusion; StatusContext trae state.
				if checkFailures[c.Conclusion] || checkFailures[c.State] {
					out.ChecksFailing++
				}
			}
		}
	}
	return out, nil
}
```

- [ ] **Step 5: Correr los tests y verificar que pasan**

Run: `go test ./internal/resources/ -race -count=1`
Expected: PASS en los cinco tests de parseo y en los de matching.

- [ ] **Step 6: Commit**

```bash
git add internal/resources/
git commit -m "feat(resources): parseo de la respuesta de GraphQL a PRState

Las fixtures salen de respuestas reales de la API. statusCheckRollup
null es un caso real —PRs viejos con los checks expirados— y el error
se reconoce por la clave errors, que gh escribe en stdout."
```

---

### Task 4: Resources — el runner de `gh` y el `Fetch` completo

**Files:**
- Modify: `internal/resources/github.go`
- Create: `internal/resources/github_test.go`

**Interfaces:**
- Consumes: `parsePRResponse`, `ErrRemote` (tarea 3), `parsePRRef`, `Ref`, `Snapshot` (tarea 2).
- Produces:
  - `(*GitHub).Fetch(ctx context.Context, ref Ref) (*Snapshot, error)` — completa la interfaz `Provider`
  - `newGitHubWith(run runner) *GitHub` — constructor para tests
  - `var ErrToolMissing = errors.New("el gh CLI no está instalado")`
  - `var ErrNotAuthenticated = errors.New("el gh CLI no está autenticado")`
  - constante `prQuery` con la query de GraphQL

- [ ] **Step 1: Escribir los tests que fallan**

`internal/resources/github_test.go`:

```go
package resources

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// fakeRunner devuelve siempre lo mismo y registra con qué lo llamaron.
func fakeRunner(out []byte, err error, seen *[]string) runner {
	return func(_ context.Context, args ...string) ([]byte, error) {
		if seen != nil {
			*seen = append(*seen, args...)
		}
		return out, err
	}
}

func testRef(t *testing.T) Ref {
	t.Helper()
	ref, ok := NewGitHub().Match("https://github.com/cli/cli/pull/14456")
	if !ok {
		t.Fatal("no matcheó la URL de prueba")
	}
	return ref
}

func TestFetchArmaLaQuery(t *testing.T) {
	var seen []string
	g := newGitHubWith(fakeRunner(fixture(t, "pr_open.json"), nil, &seen))

	snap, err := g.Fetch(context.Background(), testRef(t))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if snap.PR == nil || snap.PR.Number != 14456 {
		t.Fatalf("snapshot = %+v", snap)
	}
	if snap.FetchedAt == 0 {
		t.Fatal("el snapshot tiene que traer fetched_at")
	}
	if snap.Error != "" {
		t.Fatalf("no tendría que haber error: %q", snap.Error)
	}

	args := strings.Join(seen, " ")
	// Owner, repo y número tienen que viajar como variables de GraphQL y no
	// interpolados en la query: así una URL rara no puede inyectar nada.
	for _, want := range []string{"api", "graphql", "owner=cli", "name=cli", "number=14456"} {
		if !strings.Contains(args, want) {
			t.Errorf("falta %q en los argumentos: %v", want, seen)
		}
	}
}

// TestFetchErrorDeGitHub: el mensaje del sistema externo va al snapshot, no
// se pierde. gh escribe el JSON de error en stdout, así que hay que parsearlo
// aunque el comando haya devuelto un exit code distinto de cero.
func TestFetchErrorDeGitHub(t *testing.T) {
	g := newGitHubWith(fakeRunner(
		fixture(t, "pr_no_encontrado.json"),
		&exec.ExitError{},
		nil,
	))

	snap, err := g.Fetch(context.Background(), testRef(t))
	if err != nil {
		t.Fatalf("un error remoto va en el snapshot, no como error de Fetch: %v", err)
	}
	if snap.PR != nil {
		t.Fatal("con error no tendría que haber estado")
	}
	if !strings.Contains(snap.Error, "99999999") {
		t.Fatalf("se perdió el mensaje de GitHub: %q", snap.Error)
	}
}

// TestFetchSinGh: sin el CLI instalado, linkear sigue andando y el estado
// explica qué hacer.
func TestFetchSinGh(t *testing.T) {
	g := newGitHubWith(fakeRunner(nil, exec.ErrNotFound, nil))

	snap, err := g.Fetch(context.Background(), testRef(t))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !strings.Contains(snap.Error, "gh") {
		t.Fatalf("el error tendría que mencionar gh: %q", snap.Error)
	}
}

// TestFetchSinAutenticar: gh avisa por stderr; el mensaje tiene que llegar al
// usuario en vez de un "error desconocido".
func TestFetchSinAutenticar(t *testing.T) {
	g := newGitHubWith(fakeRunner(nil, errors.New("gh: To get started with GitHub CLI, please run: gh auth login"), nil))

	snap, err := g.Fetch(context.Background(), testRef(t))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !strings.Contains(snap.Error, "gh auth login") {
		t.Fatalf("no se propagó la instrucción: %q", snap.Error)
	}
}

func TestFetchRespetaElContexto(t *testing.T) {
	lento := func(ctx context.Context, _ ...string) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	g := newGitHubWith(lento)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	snap, err := g.Fetch(ctx, testRef(t))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if snap.Error == "" {
		t.Fatal("el timeout tendría que quedar registrado en el snapshot")
	}
}

// TestFetchIntegracion es el único test que toca la red. Se saltea si no hay
// gh o no está autenticado, para que la suite corra en cualquier máquina.
func TestFetchIntegracion(t *testing.T) {
	if testing.Short() {
		t.Skip("modo -short")
	}
	if _, err := exec.LookPath("gh"); err != nil {
		t.Skip("gh no está instalado")
	}
	if err := exec.Command("gh", "auth", "status").Run(); err != nil {
		t.Skip("gh no está autenticado")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ref, _ := NewGitHub().Match("https://github.com/cli/cli/pull/9000")
	snap, err := NewGitHub().Fetch(ctx, ref)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if snap.Error != "" {
		t.Fatalf("error consultando un PR público real: %s", snap.Error)
	}
	if snap.PR == nil || snap.PR.Number != 9000 {
		t.Fatalf("snapshot = %+v", snap)
	}
	if snap.PR.State != "MERGED" {
		t.Fatalf("el PR 9000 de cli/cli está mergeado, vino %q", snap.PR.State)
	}
}
```

- [ ] **Step 2: Correr los tests y verificar que fallan**

Run: `go test ./internal/resources/ -run TestFetch`
Expected: FAIL, no compila (`undefined: newGitHubWith`, `g.Fetch undefined`).

- [ ] **Step 3: Implementar el runner y el Fetch**

En `internal/resources/github.go`, reemplazar el `ghRunner` provisorio de la
tarea 2 y agregar:

```go
// prQuery trae en una sola llamada todo lo que muestra la card.
//
// Es GraphQL y no REST por una razón concreta: la cuenta de comments sin
// resolver no existe en REST. Los review threads con isResolved solo están
// acá; por REST habría que paginar comments e inferirlo, y saldría mal.
const prQuery = `
query($owner:String!, $name:String!, $number:Int!) {
  repository(owner:$owner, name:$name) {
    pullRequest(number:$number) {
      number title url state isDraft mergeable reviewDecision
      author { login }
      reviewThreads(first:100) { totalCount nodes { isResolved } }
      commits(last:1) { nodes { commit { statusCheckRollup {
        state
        contexts(first:100) { totalCount nodes {
          __typename
          ... on CheckRun { name conclusion status }
          ... on StatusContext { context state }
        } }
      } } } }
    }
  }
}`

// ghTimeout acota lo que puede tardar el subproceso.
const ghTimeout = 10 * time.Second

// NewGitHub construye el provider con el gh real.
func NewGitHub() *GitHub { return &GitHub{run: ghRunner} }

// newGitHubWith inyecta un runner. Es para los tests: así el paquete entero
// se prueba sin red.
func newGitHubWith(run runner) *GitHub { return &GitHub{run: run} }

// ghRunner ejecuta el gh CLI.
//
// Se shellea a gh en vez de manejar un token propio: ya está autenticado en
// la máquina y maneja el keyring y el refresh. Los argumentos van como argv
// separado, así que no hay inyección posible desde la URL.
func ghRunner(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil && stdout.Len() == 0 {
		// Sin nada en stdout, el único diagnóstico es lo que gh dijo por
		// stderr: que no está autenticado, que no encuentra el repo, etc.
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("%s", msg)
		}
		return nil, err
	}
	// Con salida en stdout devolvemos el body aunque el exit code sea != 0:
	// ante un error de GraphQL, gh escribe ahí el JSON con la clave `errors`,
	// que tiene mejor mensaje que el de stderr.
	return stdout.Bytes(), nil
}

// Fetch consulta el estado actual del PR.
//
// Los problemas del sistema externo —PR inexistente, sin permisos, gh sin
// autenticar— vuelven dentro del Snapshot, no como error de Fetch: son estado
// que la card tiene que mostrar, no fallas de esta capa.
func (g *GitHub) Fetch(ctx context.Context, ref Ref) (*Snapshot, error) {
	owner, repo, number, err := parsePRRef(ref)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, ghTimeout)
	defer cancel()

	out, runErr := g.run(ctx,
		"api", "graphql",
		"-F", "owner="+owner,
		"-F", "name="+repo,
		"-F", "number="+strconv.Itoa(number),
		"-f", "query="+prQuery,
	)

	snap := &Snapshot{FetchedAt: time.Now().UnixMilli()}

	if len(out) == 0 {
		snap.Error = describeRunError(runErr)
		return snap, nil
	}
	pr, perr := parsePRResponse(out)
	if perr != nil {
		snap.Error = perr.Error()
		return snap, nil
	}
	snap.PR = pr
	return snap, nil
}

// describeRunError traduce la falla del subproceso a algo accionable.
func describeRunError(err error) string {
	switch {
	case err == nil:
		return "GitHub no devolvió respuesta"
	case errors.Is(err, exec.ErrNotFound):
		return "el gh CLI no está instalado: instalalo para ver el estado del PR"
	case errors.Is(err, context.DeadlineExceeded):
		return "GitHub no respondió a tiempo"
	case errors.Is(err, context.Canceled):
		return "consulta cancelada"
	default:
		// gh ya explica bien lo suyo (por ejemplo "please run: gh auth login").
		return err.Error()
	}
}
```

Los `var ErrToolMissing` / `ErrNotAuthenticated` que anunciaba el bloque de
interfaces **no se agregan**: `describeRunError` cubre esos casos con un
mensaje accionable, y exportar centinelas que nadie compara sería código
muerto. Si más adelante la UI necesita distinguirlos por código, se agregan
entonces.

Ajustar los imports de `github.go` a: `bytes`, `context`, `errors`, `fmt`,
`os/exec`, `regexp`, `strconv`, `strings`, `time`, y `neturl "net/url"`.

- [ ] **Step 4: Correr los tests y verificar que pasan**

Run: `go test ./internal/resources/ -race -count=1 -v`
Expected: PASS en todo. `TestFetchIntegracion` corre de verdad si hay `gh`
autenticado; si no, aparece como SKIP.

- [ ] **Step 5: Verificar que el skip funciona**

Run: `go test ./internal/resources/ -run TestFetchIntegracion -short -v`
Expected: `--- SKIP: TestFetchIntegracion`.

- [ ] **Step 6: Commit**

```bash
git add internal/resources/
git commit -m "feat(resources): consulta del estado de un PR vía el gh CLI

Se shellea a gh en vez de manejar un token propio: ya está autenticado
y maneja el keyring. Ante un error de GraphQL gh escribe el JSON en
stdout, así que se parsea eso aunque el exit code sea distinto de cero."
```

---

### Task 5: Resources — caché con TTL y single-flight

**Files:**
- Create: `internal/resources/cache.go`
- Create: `internal/resources/cache_test.go`

**Interfaces:**
- Consumes: `Ref`, `Snapshot`, `Provider`, `Registry` (tareas 2-4).
- Produces:
  - `type Cache struct { … }`
  - `NewCache(reg *Registry) *Cache`
  - `(*Cache).Get(ctx context.Context, ref Ref) *Snapshot`
  - constantes `okTTL = 30 * time.Second`, `errTTL = 10 * time.Second`
  - campo `now func() time.Time` en `Cache`, para que los tests controlen el reloj

- [ ] **Step 1: Escribir los tests que fallan**

`internal/resources/cache_test.go`:

```go
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

func newTestCache(p Provider) (*Cache, *fakeClock) {
	clk := &fakeClock{t: time.Unix(1700000000, 0)}
	c := NewCache(NewRegistry(p))
	c.now = clk.Now
	return c, clk
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

// TestCacheRefsDistintosNoSePisan.
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
```

- [ ] **Step 2: Correr los tests y verificar que fallan**

Run: `go test ./internal/resources/ -run TestCache`
Expected: FAIL, no compila (`undefined: NewCache`).

- [ ] **Step 3: Implementar el caché**

`internal/resources/cache.go`:

```go
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

// Get devuelve el estado del recurso, consultando solo si lo cacheado venció.
//
// Nunca devuelve nil: los problemas viajan dentro del Snapshot, porque son
// estado que la UI tiene que mostrar, no fallas de esta capa.
func (c *Cache) Get(ctx context.Context, ref Ref) *Snapshot {
	for {
		c.mu.Lock()
		e, ok := c.entries[ref.URL]

		// Hay una consulta en curso: esperamos su resultado en vez de
		// disparar otra. Es lo que hace que N clientes mirando el mismo PR
		// cuesten una sola llamada a GitHub.
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
```

- [ ] **Step 4: Correr los tests y verificar que pasan**

Run: `go test ./internal/resources/ -race -count=1`
Expected: PASS en los seis tests de caché y en los anteriores. El `-race` es
importante acá: `TestCacheSingleFlight` lanza 20 goroutines contra el mismo
ref.

- [ ] **Step 5: Commit**

```bash
git add internal/resources/
git commit -m "feat(resources): caché con TTL y single-flight

N clientes mirando el mismo PR cuestan una sola llamada a GitHub. El
estado no se persiste: al reiniciar conviene volver a preguntar antes
que mostrar un check en verde de hace veinte minutos como si fuera de
ahora."
```

---

### Task 6: Session + Server — ABM de recursos por REST

**Files:**
- Modify: `internal/session/manager.go`
- Create: `internal/server/resources.go`
- Create: `internal/server/resources_test.go`
- Modify: `internal/server/server.go` (rutas nuevas)
- Modify: `internal/server/sessions_test.go` (inyectar el caché de prueba)
- Modify: `cmd/webterm/main.go` (armar el registry y el caché)

**Interfaces:**
- Consumes: `store.Resource`, `store.ErrDuplicate` (tarea 1); `resources.Registry`, `resources.Cache`, `resources.Ref`, `resources.Snapshot` (tareas 2-5).
- Produces:
  - En `session.Config`: campo `Resources *resources.Cache`
  - `type LinkedResource struct { *store.Resource; Snapshot *resources.Snapshot }`
  - `(*Manager).ListResources(ctx context.Context, sessionID string) ([]*LinkedResource, error)`
  - `(*Manager).AddResource(sessionID, rawURL, system, typ string) (*store.Resource, error)`
  - `(*Manager).DeleteResource(sessionID string, id int64) error`
  - `var ErrUnknownResource = resources.ErrUnknownResource` (re-export, para que el server no importe `resources`)
  - `(*Cache).Resolve(rawURL string) (Ref, bool)`
  - Endpoints: `GET|POST /api/sessions/{id}/resources`, `DELETE /api/sessions/{id}/resources/{rid}`

- [ ] **Step 1: Escribir los tests que fallan**

`internal/server/resources_test.go`:

```go
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/giuliano/webterm/internal/resources"
)

// proveedorFalso responde sin tocar la red, para que los tests del servidor no
// dependan de GitHub.
type proveedorFalso struct{}

func (proveedorFalso) Match(rawURL string) (resources.Ref, bool) {
	if rawURL != "https://github.com/o/r/pull/1" {
		return resources.Ref{}, false
	}
	return resources.Ref{System: "gh", Type: "pr", URL: rawURL}, true
}

func (proveedorFalso) Fetch(context.Context, resources.Ref) (*resources.Snapshot, error) {
	return &resources.Snapshot{PR: &resources.PRState{
		Number: 1, Title: "un PR", State: "OPEN", ReviewDecision: "APPROVED",
		UnresolvedCount: 3, ChecksState: "SUCCESS", ChecksTotal: 5,
	}}, nil
}

type resourceJSON struct {
	ID       int64  `json:"id"`
	System   string `json:"system"`
	Type     string `json:"type"`
	Ref      string `json:"ref"`
	Snapshot *struct {
		FetchedAt int64  `json:"fetched_at"`
		Error     string `json:"error"`
		PR        *struct {
			Number          int    `json:"number"`
			State           string `json:"state"`
			UnresolvedCount int    `json:"unresolved_count"`
		} `json:"pr"`
	} `json:"snapshot"`
}

func TestLinkearYListarRecursos(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := createSession(t, srv)
	base := "/api/sessions/" + rec.ID + "/resources"

	status, body := do(t, srv, "GET", base, "")
	if status != http.StatusOK || string(bytes.TrimSpace(body)) != "[]" {
		t.Fatalf("sin recursos = %d %s", status, body)
	}

	status, body = do(t, srv, "POST", base, `{"ref":"https://github.com/o/r/pull/1"}`)
	if status != http.StatusCreated {
		t.Fatalf("POST = %d: %s", status, body)
	}
	var creado resourceJSON
	if err := json.Unmarshal(body, &creado); err != nil {
		t.Fatalf("decodificando: %v", err)
	}
	// system y type los infiere el backend de la URL.
	if creado.System != "gh" || creado.Type != "pr" {
		t.Fatalf("no se infirió system/type: %+v", creado)
	}

	_, body = do(t, srv, "GET", base, "")
	var lista []resourceJSON
	if err := json.Unmarshal(body, &lista); err != nil {
		t.Fatalf("decodificando lista: %v", err)
	}
	if len(lista) != 1 {
		t.Fatalf("vinieron %d recursos", len(lista))
	}
	if lista[0].Snapshot == nil || lista[0].Snapshot.PR == nil {
		t.Fatalf("la lista tiene que traer el estado: %s", body)
	}
	if lista[0].Snapshot.PR.UnresolvedCount != 3 {
		t.Fatalf("estado mal: %+v", lista[0].Snapshot.PR)
	}
	if lista[0].Snapshot.FetchedAt == 0 {
		t.Fatal("falta fetched_at: la UI necesita saber qué tan viejo es el dato")
	}
}

func TestLinkearURLDesconocida(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := createSession(t, srv)
	base := "/api/sessions/" + rec.ID + "/resources"

	if status, _ := do(t, srv, "POST", base, `{"ref":"https://ejemplo.invalido/x"}`); status != http.StatusBadRequest {
		t.Fatalf("URL desconocida = %d", status)
	}
	if status, _ := do(t, srv, "POST", base, `{"ref":""}`); status != http.StatusBadRequest {
		t.Fatalf("ref vacío = %d", status)
	}
}

func TestLinkearDuplicado(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := createSession(t, srv)
	base := "/api/sessions/" + rec.ID + "/resources"
	body := `{"ref":"https://github.com/o/r/pull/1"}`

	if status, _ := do(t, srv, "POST", base, body); status != http.StatusCreated {
		t.Fatal("el primer POST tendría que andar")
	}
	if status, _ := do(t, srv, "POST", base, body); status != http.StatusConflict {
		t.Fatalf("el duplicado = %d, se esperaba 409", status)
	}
}

func TestLinkearEnSesionInexistente(t *testing.T) {
	srv, _ := newTestServer(t)
	if status, _ := do(t, srv, "POST", "/api/sessions/no-existe/resources",
		`{"ref":"https://github.com/o/r/pull/1"}`); status != http.StatusNotFound {
		t.Fatalf("sesión inexistente = %d", status)
	}
	if status, _ := do(t, srv, "GET", "/api/sessions/no-existe/resources", ""); status != http.StatusNotFound {
		t.Fatalf("GET de sesión inexistente = %d", status)
	}
}

func TestDeslinkear(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := createSession(t, srv)
	base := "/api/sessions/" + rec.ID + "/resources"

	_, body := do(t, srv, "POST", base, `{"ref":"https://github.com/o/r/pull/1"}`)
	var creado resourceJSON
	_ = json.Unmarshal(body, &creado)
	id := strconv.FormatInt(creado.ID, 10)

	if status, _ := do(t, srv, "DELETE", base+"/"+id, ""); status != http.StatusNoContent {
		t.Fatalf("DELETE = %d", status)
	}
	if status, _ := do(t, srv, "DELETE", base+"/"+id, ""); status != http.StatusNotFound {
		t.Fatalf("DELETE repetido = %d", status)
	}
	if status, _ := do(t, srv, "DELETE", base+"/abc", ""); status != http.StatusBadRequest {
		t.Fatalf("id no numérico = %d", status)
	}
}

// TestRecursosSeBorranConLaSesion verifica la cascada a través de la API.
func TestRecursosSeBorranConLaSesion(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := createSession(t, srv)
	base := "/api/sessions/" + rec.ID + "/resources"
	_, _ = do(t, srv, "POST", base, `{"ref":"https://github.com/o/r/pull/1"}`)

	if status, _ := do(t, srv, "DELETE", "/api/sessions/"+rec.ID, ""); status != http.StatusNoContent {
		t.Fatal("no se pudo borrar la sesión")
	}
	if status, _ := do(t, srv, "GET", base, ""); status != http.StatusNotFound {
		t.Fatal("los recursos sobrevivieron a la sesión")
	}
}
```

- [ ] **Step 2: Inyectar el proveedor falso en el servidor de prueba**

En `internal/server/sessions_test.go`, dentro de `newTestServer`, cambiar la
construcción del manager por:

```go
	mgr := session.NewManager(st, session.Config{
		Shell: "/bin/bash", HistoryBytes: 1 << 20, SweepEvery: time.Hour,
		Resources: resources.NewCache(resources.NewRegistry(proveedorFalso{})),
	})
```

y agregar el import `"github.com/giuliano/webterm/internal/resources"`.

- [ ] **Step 3: Correr los tests y verificar que fallan**

Run: `go test ./internal/server/ -run 'Linkear|Deslinkear|Recursos'`
Expected: FAIL, no compila (`unknown field Resources`, handlers inexistentes).

- [ ] **Step 4: Agregar `Resolve` al caché**

En `internal/resources/cache.go`:

```go
// Resolve normaliza una URL a su Ref canónico, sin consultar el estado. Lo usa
// el manager al linkear, que necesita saber system y type pero no el estado.
func (c *Cache) Resolve(rawURL string) (Ref, bool) {
	ref, _, ok := c.reg.Resolve(rawURL)
	return ref, ok
}
```

- [ ] **Step 5: Agregar el ABM al manager**

En `internal/session/manager.go`, sumar el campo a `Config`:

```go
	// Resources resuelve el estado de los recursos externos linkeados. Si es
	// nil, linkear por URL deja de funcionar pero el resto del manager anda
	// igual: la integración es opcional y no puede tumbar las sesiones.
	Resources *resources.Cache
```

y los métodos, al final del archivo:

```go
// ErrUnknownResource se re-exporta para que el servidor traduzca el error a un
// status sin tener que importar el paquete resources.
var ErrUnknownResource = resources.ErrUnknownResource

// LinkedResource es un recurso linkeado junto con su estado actual.
type LinkedResource struct {
	*store.Resource
	Snapshot *resources.Snapshot `json:"snapshot,omitempty"`
}

// ListResources devuelve los recursos de la sesión con su estado. El estado
// sale del caché, así que el polling del frontend no se traduce uno a uno en
// llamadas al sistema externo.
func (m *Manager) ListResources(ctx context.Context, sessionID string) ([]*LinkedResource, error) {
	if _, err := m.st.GetSession(sessionID); err != nil {
		return nil, err
	}
	rows, err := m.st.ListResources(sessionID)
	if err != nil {
		return nil, err
	}

	out := make([]*LinkedResource, 0, len(rows))
	for _, r := range rows {
		lr := &LinkedResource{Resource: r}
		if m.cfg.Resources != nil {
			lr.Snapshot = m.cfg.Resources.Get(ctx, resources.Ref{
				System: r.System, Type: r.Type, URL: r.Ref,
			})
		}
		out = append(out, lr)
	}
	return out, nil
}

// AddResource linkea un recurso a la sesión.
//
// system y type se infieren del propio link: es mejor UX —pegás la URL y
// listo— y es el seam que generaliza, porque sumar otro sistema es sumar un
// provider al registry sin tocar este contrato. Se aceptan explícitos como
// escape hatch para un formato que el registry todavía no conozca.
func (m *Manager) AddResource(sessionID, rawURL, system, typ string) (*store.Resource, error) {
	if _, err := m.st.GetSession(sessionID); err != nil {
		return nil, err
	}
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, ErrUnknownResource
	}

	ref := resources.Ref{System: system, Type: typ, URL: rawURL}
	if system == "" || typ == "" {
		if m.cfg.Resources == nil {
			return nil, ErrUnknownResource
		}
		resolved, ok := m.cfg.Resources.Resolve(rawURL)
		if !ok {
			return nil, ErrUnknownResource
		}
		ref = resolved
	}

	r := &store.Resource{
		SessionID: sessionID, System: ref.System, Type: ref.Type, Ref: ref.URL,
	}
	if err := m.st.AddResource(r); err != nil {
		return nil, err
	}
	log.Printf("[%s] recurso linkeado: %s", sessionID, r.Ref)
	return r, nil
}

// DeleteResource desvincula el recurso de la sesión.
func (m *Manager) DeleteResource(sessionID string, id int64) error {
	if _, err := m.st.GetSession(sessionID); err != nil {
		return err
	}
	return m.st.DeleteResource(sessionID, id)
}
```

Agregar a los imports de `manager.go`: `context`, `strings` y
`github.com/giuliano/webterm/internal/resources`.

- [ ] **Step 6: Escribir los handlers**

`internal/server/resources.go`:

```go
package server

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/giuliano/webterm/internal/session"
	"github.com/giuliano/webterm/internal/store"
)

type linkResourceReq struct {
	Ref string `json:"ref"`
	// system y type son opcionales: por defecto se infieren de la URL. Están
	// para no cerrarle la puerta a un formato que el registry no conozca.
	System string `json:"system"`
	Type   string `json:"type"`
}

func (s *Server) handleListResources(w http.ResponseWriter, r *http.Request) {
	list, err := s.mgr.ListResources(r.Context(), r.PathValue("id"))
	if err != nil {
		writeResourceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleLinkResource(w http.ResponseWriter, r *http.Request) {
	var req linkResourceReq
	if err := decodeBody(r, &req); err != nil {
		writeErrorMsg(w, http.StatusBadRequest, "body inválido")
		return
	}
	rec, err := s.mgr.AddResource(r.PathValue("id"), req.Ref, req.System, req.Type)
	if err != nil {
		writeResourceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, rec)
}

func (s *Server) handleUnlinkResource(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("rid"), 10, 64)
	if err != nil {
		writeErrorMsg(w, http.StatusBadRequest, "id de recurso inválido")
		return
	}
	if err := s.mgr.DeleteResource(r.PathValue("id"), id); err != nil {
		writeResourceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeResourceError suma a la traducción general los errores propios del ABM
// de recursos.
func writeResourceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, session.ErrUnknownResource):
		writeErrorMsg(w, http.StatusBadRequest, "no se reconoce esa URL; por ahora solo PRs de GitHub")
	case errors.Is(err, store.ErrDuplicate):
		writeErrorMsg(w, http.StatusConflict, "ese recurso ya está linkeado a la sesión")
	default:
		writeError(w, err)
	}
}
```

Registrar las rutas en `Handler()` de `internal/server/server.go`, junto a las
de KV:

```go
	mux.HandleFunc("GET /api/sessions/{id}/resources", s.handleListResources)
	mux.HandleFunc("POST /api/sessions/{id}/resources", s.handleLinkResource)
	mux.HandleFunc("DELETE /api/sessions/{id}/resources/{rid}", s.handleUnlinkResource)
```

- [ ] **Step 7: Armar el registry en el entrypoint**

En `cmd/webterm/main.go`, antes de construir el manager:

```go
	// El registry es el punto de extensión: sumar Linear o Slack es sumar un
	// provider acá.
	sess.Resources = resources.NewCache(resources.NewRegistry(resources.NewGitHub()))
```

con el import `"github.com/giuliano/webterm/internal/resources"`.

- [ ] **Step 8: Correr los tests y verificar que pasan**

Run: `go vet ./... && go test ./... -race -count=1`
Expected: PASS en todo, incluidos los tests de M2 sin cambios.

- [ ] **Step 9: Probarlo a mano contra un PR real**

```bash
go build -o bin/webterm ./cmd/webterm
./bin/webterm -db /tmp/m8.db -addr 127.0.0.1:7799 -static /nonexistent &
sleep 2
ID=$(curl -sX POST localhost:7799/api/sessions -d '{"cols":80,"rows":24}' \
     | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
curl -sX POST localhost:7799/api/sessions/$ID/resources \
     -d '{"ref":"https://github.com/cli/cli/pull/9000"}'
curl -s localhost:7799/api/sessions/$ID/resources | python3 -m json.tool
kill %1
rm -f /tmp/m8.db /tmp/m8.db-wal /tmp/m8.db-shm
```

Expected: el `POST` devuelve `"system": "gh"`, `"type": "pr"`; el `GET` trae el
snapshot con `"state": "MERGED"` y `"review_decision": "APPROVED"`.

- [ ] **Step 10: Commit**

```bash
git add internal/ cmd/
git commit -m "feat(server): ABM de recursos linkeados por REST

El POST manda solo la URL: system y type los infiere el backend, que es
el seam que generaliza —sumar Linear es sumar un provider, no cambiar
el contrato de la API."
```

---

### Task 7: Frontend — panel desplegable y card de PR

**Files:**
- Create: `web/src/ResourcePanel.tsx`
- Modify: `web/src/api.ts`
- Modify: `web/src/App.tsx`
- Modify: `web/src/index.css`

**Interfaces:**
- Consumes: los tres endpoints de la tarea 6.
- Produces: `<ResourcePanel sessionId>`; en `api.ts`, los tipos `PRState`/`Snapshot`/`LinkedResource` y `api.resources.{list,link,unlink}`.

- [ ] **Step 1: Extender el cliente de la API**

Agregar los tipos al final de `web/src/api.ts`:

```ts
export interface PRState {
  number: number
  title: string
  author: string
  state: 'OPEN' | 'CLOSED' | 'MERGED'
  is_draft: boolean
  mergeable: string
  review_decision: string
  unresolved_count: number
  threads_truncated: boolean
  checks_state: string
  checks_total: number
  checks_failing: number
}

export interface Snapshot {
  fetched_at: number
  error?: string
  pr?: PRState
}

export interface LinkedResource {
  id: number
  session_id: string
  system: string
  type: string
  ref: string
  created_at: number
  snapshot?: Snapshot
}
```

y dentro del objeto `api`, después de `health`:

```ts
  resources: {
    list: (sessionId: string) =>
      req<LinkedResource[]>(`/api/sessions/${sessionId}/resources`),
    link: (sessionId: string, ref: string) =>
      req<LinkedResource>(`/api/sessions/${sessionId}/resources`, {
        method: 'POST',
        body: JSON.stringify({ ref }),
      }),
    unlink: (sessionId: string, id: number) =>
      req<void>(`/api/sessions/${sessionId}/resources/${id}`, { method: 'DELETE' }),
  },
```

- [ ] **Step 2: Escribir el panel**

`web/src/ResourcePanel.tsx`:

```tsx
import { useCallback, useEffect, useState } from 'react'
import { api, type LinkedResource, type PRState } from './api'

// Cada cuánto se refresca mientras el panel está abierto. El backend cachea
// 30 s, así que este polling no se traduce uno a uno en llamadas a GitHub.
const POLL_MS = 15000

export function ResourcePanel({ sessionId }: { sessionId: string }) {
  const [open, setOpen] = useState(false)
  const [items, setItems] = useState<LinkedResource[]>([])
  const [draft, setDraft] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)

  const refresh = useCallback(async () => {
    try {
      setItems(await api.resources.list(sessionId))
    } catch (err) {
      setError(String(err))
    }
  }, [sessionId])

  // La lista se pide una vez al montar para poder mostrar el contador; el
  // refresco periódico corre solo con el panel abierto, que es lo que evita
  // consultar GitHub por sesiones que nadie está mirando.
  useEffect(() => {
    void refresh()
  }, [refresh])

  useEffect(() => {
    if (!open) return
    const t = setInterval(() => void refresh(), POLL_MS)
    return () => clearInterval(t)
  }, [open, refresh])

  const link = async () => {
    const ref = draft.trim()
    if (!ref) return
    setBusy(true)
    try {
      await api.resources.link(sessionId, ref)
      setDraft('')
      setError(null)
      await refresh()
    } catch (err) {
      setError(String(err))
    } finally {
      setBusy(false)
    }
  }

  const unlink = async (id: number) => {
    setBusy(true)
    try {
      await api.resources.unlink(sessionId, id)
      setError(null)
      await refresh()
    } catch (err) {
      setError(String(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <section className="resources">
      <button className="resources-head" onClick={() => setOpen((o) => !o)}>
        <span className="caret">{open ? '▾' : '▸'}</span>
        Linkeado
        {items.length > 0 && <span className="count">{items.length}</span>}
      </button>

      {open && (
        <div className="resources-body">
          <div className="link-form">
            <input
              value={draft}
              placeholder="URL de un PR de GitHub"
              onChange={(e) => setDraft(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === 'Enter') void link()
              }}
            />
            <button onClick={() => void link()} disabled={busy || !draft.trim()}>
              Linkear
            </button>
          </div>

          {error && <p className="resource-error">{error}</p>}
          {items.length === 0 && <p className="empty">nada linkeado todavía</p>}

          {items.map((it) => (
            <ResourceCard key={it.id} item={it} onUnlink={() => void unlink(it.id)} busy={busy} />
          ))}
        </div>
      )}
    </section>
  )
}

function ResourceCard({
  item,
  onUnlink,
  busy,
}: {
  item: LinkedResource
  onUnlink: () => void
  busy: boolean
}) {
  const pr = item.snapshot?.pr
  return (
    <article className="resource-card">
      <header>
        <a href={item.ref} target="_blank" rel="noreferrer">
          {pr ? `#${pr.number} ${pr.title}` : item.ref}
        </a>
        <button className="danger" onClick={onUnlink} disabled={busy} title="Deslinkear">
          ✕
        </button>
      </header>

      {item.snapshot?.error && <p className="resource-error">{item.snapshot.error}</p>}
      {pr && <PRBadges pr={pr} />}
      {item.snapshot && <Freshness at={item.snapshot.fetched_at} />}
    </article>
  )
}

function PRBadges({ pr }: { pr: PRState }) {
  return (
    <p className="badges">
      <span className="badge" data-value={prState(pr)}>
        {prStateLabel(pr)}
      </span>
      <span className="badge" data-value={pr.review_decision || 'NONE'}>
        {reviewLabel(pr.review_decision)}
      </span>
      {pr.checks_state && (
        <span className="badge" data-value={checksValue(pr)}>
          {checksLabel(pr)}
        </span>
      )}
      <span className="badge" data-value={pr.unresolved_count > 0 ? 'threads' : 'clean'}>
        {pr.unresolved_count === 0
          ? 'sin comments pendientes'
          : `${pr.unresolved_count}${pr.threads_truncated ? '+' : ''} sin resolver`}
      </span>
    </p>
  )
}

function prState(pr: PRState): string {
  return pr.is_draft && pr.state === 'OPEN' ? 'DRAFT' : pr.state
}

function prStateLabel(pr: PRState): string {
  switch (prState(pr)) {
    case 'DRAFT':
      return 'borrador'
    case 'OPEN':
      return pr.mergeable === 'CONFLICTING' ? 'abierto · conflictos' : 'abierto'
    case 'MERGED':
      return 'mergeado'
    default:
      return 'cerrado'
  }
}

function reviewLabel(decision: string): string {
  switch (decision) {
    case 'APPROVED':
      return 'aprobado'
    case 'CHANGES_REQUESTED':
      return 'cambios pedidos'
    case 'REVIEW_REQUIRED':
      return 'falta review'
    default:
      return 'sin review'
  }
}

// checksValue separa "hay checks fallando" de lo que diga el rollup: un
// rollup en SUCCESS con checks fallando no se da, pero un FAILURE con cero
// fallas contadas sí (por ejemplo si falló algo que no contamos), y en ese
// caso conviene pintarlo por lo que realmente sabemos.
function checksValue(pr: PRState): string {
  if (pr.checks_failing > 0) return 'FAILURE'
  if (pr.checks_state === 'PENDING') return 'PENDING'
  return pr.checks_state
}

function checksLabel(pr: PRState): string {
  if (pr.checks_failing > 0) return `${pr.checks_failing} de ${pr.checks_total} fallando`
  if (pr.checks_state === 'PENDING') return 'checks corriendo'
  return `${pr.checks_total} checks ok`
}

// Freshness marca la edad del dato. Un check en verde de hace veinte minutos
// mostrado como actual es peor que no mostrar nada.
function Freshness({ at }: { at: number }) {
  const secs = Math.max(0, Math.round((Date.now() - at) / 1000))
  if (secs < 60) return null
  const txt = secs < 3600 ? `hace ${Math.floor(secs / 60)} min` : `hace ${Math.floor(secs / 3600)} h`
  return <p className="stale">{txt}</p>
}
```

- [ ] **Step 3: Montarlo en el layout**

En `web/src/App.tsx`, agregar el import:

```tsx
import { ResourcePanel } from './ResourcePanel'
```

y reemplazar el contenido de `<main className="main">` por:

```tsx
        <main className="main">
          {selected ? (
            <>
              <ResourcePanel key={selected} sessionId={selected} />
              {/* key fuerza un remount al cambiar de sesión: cada una tiene su
                  propio xterm y su propio socket. */}
              <TerminalView key={selected} sessionId={selected} onState={setState} />
            </>
          ) : (
            <div className="placeholder">
              No hay ninguna sesión abierta. Creá una con <b>+ Nueva</b>.
            </div>
          )}
        </main>
```

- [ ] **Step 4: Estilos**

Agregar al final de `web/src/index.css`:

```css
.resources {
  flex: 0 0 auto;
  border-bottom: 1px solid var(--border);
  background: var(--bg-chrome);
}

.resources-head {
  display: flex;
  align-items: center;
  gap: 6px;
  width: 100%;
  border: 0;
  border-radius: 0;
  padding: 6px 12px;
  color: var(--fg-dim);
  font-size: 12px;
  text-align: left;
}

.resources-head:hover { color: var(--fg); }
.resources-head .caret { width: 10px; }

.resources-head .count {
  background: var(--border);
  border-radius: 8px;
  padding: 0 6px;
  font-size: 11px;
}

.resources-body {
  padding: 4px 12px 10px;
  display: flex;
  flex-direction: column;
  gap: 8px;
  max-height: 40vh;
  overflow-y: auto;
}

.link-form { display: flex; gap: 6px; }

.link-form input {
  flex: 1 1 auto;
  min-width: 0;
  font: inherit;
  font-size: 12px;
  color: var(--fg);
  background: var(--bg);
  border: 1px solid var(--border);
  border-radius: 6px;
  padding: 4px 8px;
}

.resource-card {
  border: 1px solid var(--border);
  border-radius: 6px;
  padding: 8px 10px;
  font-size: 12px;
}

.resource-card header {
  display: flex;
  align-items: flex-start;
  gap: 8px;
}

.resource-card header a {
  flex: 1 1 auto;
  color: var(--fg);
  text-decoration: none;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.resource-card header a:hover { text-decoration: underline; }

.badges {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
  margin: 8px 0 0;
}

.badge {
  border: 1px solid var(--border);
  border-radius: 10px;
  padding: 1px 8px;
  font-size: 11px;
  color: var(--fg-dim);
}

.badge[data-value="OPEN"],
.badge[data-value="MERGED"],
.badge[data-value="APPROVED"],
.badge[data-value="SUCCESS"],
.badge[data-value="clean"] { border-color: var(--accent); color: var(--accent); }

.badge[data-value="CHANGES_REQUESTED"],
.badge[data-value="FAILURE"],
.badge[data-value="threads"] { border-color: var(--danger); color: var(--danger); }

.badge[data-value="PENDING"],
.badge[data-value="DRAFT"],
.badge[data-value="REVIEW_REQUIRED"] { border-color: var(--warn); color: var(--warn); }

.resource-error {
  margin: 6px 0 0;
  color: var(--danger);
  font-size: 11px;
}

.stale {
  margin: 6px 0 0;
  color: var(--fg-dim);
  font-size: 11px;
}

.resources .empty {
  margin: 4px 0;
  color: var(--fg-dim);
  font-size: 12px;
}
```

- [ ] **Step 5: Verificar tipos y build**

Run: `npm --prefix web run build`
Expected: compila sin errores de TypeScript.

- [ ] **Step 6: Probarlo a ojo**

```bash
make run
```

Con la app abierta: crear una sesión, abrir **Linkeado**, pegar
`https://github.com/cli/cli/pull/9000` y verificar que aparece la card con
"mergeado", "aprobado" y la cuenta de comments sin resolver. Después pegar una
URL basura y ver que el error se muestra sin romper el panel. Por último,
deslinkear con la **✕**.

- [ ] **Step 7: Commit**

```bash
git add web/src/
git commit -m "feat(web): panel de recursos linkeados con card de PR

Una sola card hardcodeada, sin sistema de plugins: diseñar la
abstracción contra una muestra de uno es errarle. El refresco periódico
corre solo con el panel abierto."
```

---

### Task 8: Documentación y verificación final

**Files:**
- Modify: `README.md`
- Modify: `webterm-diseno.md`

**Interfaces:** ninguna nueva.

- [ ] **Step 1: Actualizar el estado en el README**

Cambiar el encabezado a `## Estado: M8` y la lista de milestones a:

```markdown
- [x] **M1** — terminal web básica: un pty por conexión WebSocket, input/output,
      resize, true color, mouse.
- [x] **M2** — persistencia de sesiones (SQLite + session manager) y ABM.
- [x] **M8** — recursos externos linkeados a una sesión (PRs de GitHub).
- [ ] **M3** — UI multi-terminal (tabs). ← próximo
- [ ] **M4** — folders.
- [ ] **M5** — CLI local `webterm`.
- [ ] **M6** — integración con Claude Code (hooks).
- [ ] **M7** — estado administrativo y dashboard.
```

- [ ] **Step 2: Documentar la API nueva**

Agregar a la tabla de la sección **API** del README:

```markdown
| `GET` | `/api/sessions/{id}/resources` | recursos linkeados, con su estado |
| `POST` | `/api/sessions/{id}/resources` | linkea: `{"ref":"https://github.com/o/r/pull/1"}` |
| `DELETE` | `/api/sessions/{id}/resources/{rid}` | deslinkea |
```

Y una sección nueva justo después de la API:

```markdown
## Recursos linkeados

Una sesión puede tener colgados recursos externos. Por ahora, PRs de GitHub: se
pega la URL en el panel **Linkeado** y la card muestra estado, review, checks y
comments sin resolver, refrescados mientras el panel está abierto.

El estado sale del `gh` CLI, así que hay que tenerlo instalado y autenticado
(`gh auth login`). Sin él, linkear sigue funcionando y la card explica qué
falta.

Se consulta por GraphQL y no por REST por un motivo concreto: **la cuenta de
comments sin resolver no existe en la API REST**. Una sola query trae estado,
review, checks y threads.

El estado nunca se persiste: es un caché en memoria con TTL de 30 s. N clientes
mirando el mismo PR cuestan una sola llamada a GitHub, y los datos con más de
un minuto se muestran con su edad.

Sumar otro sistema —Linear, Slack— es sumar un provider en
`internal/resources`, sin tocar el modelo de datos ni el contrato de la API.
```

- [ ] **Step 3: Actualizar la estructura**

En la sección **Estructura** del README, agregar la línea:

```
internal/resources/   providers de sistemas externos (GitHub), caché con TTL
```

- [ ] **Step 4: Cerrar el milestone en el diseño**

En `webterm-diseno.md`, dentro de `### M8`, reemplazar:

```markdown
**Próximo a implementar** (antes que M3: los números son ids estables, no
orden de ejecución).
```

por:

```markdown
**Hecho.** El diseño detallado está en
`docs/superpowers/specs/2026-09-18-m8-recursos-linkeados-design.md`.
```

- [ ] **Step 5: Verificación final completa**

```bash
go vet ./...
gofmt -l cmd internal
go test ./... -race -count=1
npm --prefix web run build
```

Expected: `vet` limpio, `gofmt` sin archivos listados, todos los tests verdes,
frontend buildeando.

- [ ] **Step 6: Commit**

```bash
git add README.md webterm-diseno.md
git commit -m "docs: documentar los recursos linkeados de M8"
```

---

## Verificación de que el plan cubre la spec

| Sección de la spec | Tareas |
|---|---|
| Tabla `session_resources` y búsqueda inversa | 1 |
| El estado no se persiste | 5 (caché en memoria) |
| `Ref`/`Snapshot`/`PRState`, `Provider`, `Registry` | 2 |
| Matching y normalización de URLs de PR | 2 |
| Query de GraphQL y su justificación | 4 |
| `statusCheckRollup` null | 3 |
| Truncado de threads a 100 | 3 |
| Credenciales vía `gh`, runner inyectable | 4 |
| Timeout de 10 s del subproceso | 4 |
| Caché con TTL 30 s / 10 s y single-flight | 5 |
| API REST con inferencia de `system`/`type` | 6 |
| Errores 400 / 404 / 409 | 6 |
| Panel desplegable y card de PR | 7 |
| Estados de error en la UI | 7 |
| Edad del dato visible | 7 |
| Polling solo con el panel abierto | 7 |
| Documentación | 8 |
