# M2 — Persistencia de sesiones: plan de implementación

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** desatar el pty del WebSocket — que viva en un session manager del backend, con su estado, su KV y su historial persistidos en SQLite y reconciliados cuando el proceso muere.

**Architecture:** tres capas nuevas con dependencias en una sola dirección. `internal/store` es SQLite puro (no conoce ptys). `internal/session` compone `terminal` + `store`: es dueño del mapa de sesiones vivas, hace fan-out del output a N clientes y corre la reconciliación. `internal/server` solo habla con `session.Manager`. El historial vive en SQLite (fuente de verdad) con un ring buffer en memoria como caché del tail.

**Tech Stack:** Go 1.25, `modernc.org/sqlite` (puro Go, sin cgo), `creack/pty`, `gorilla/websocket`, React + `xterm.js`.

**Spec:** `docs/superpowers/specs/2026-09-17-m2-persistencia-sesiones-design.md`

## Global Constraints

- **Comentarios y mensajes de commit en español**, siguiendo el estilo del repo: explican el *porqué*, no el *qué*. Los identificadores de código en inglés.
- **Sin cgo.** El driver de SQLite es `modernc.org/sqlite`, importado como `_ "modernc.org/sqlite"`, con nombre de driver `"sqlite"`.
- **Cap de historial por sesión: 1 MB** (`1048576` bytes), configurable vía `Config.HistoryBytes`.
- **Timestamps: unix millis** (`time.Now().UnixMilli()`), `int64`.
- **Tests sin mocks.** El store se prueba contra una DB real en `t.TempDir()`; el manager, contra ptys reales con `/bin/bash`.
- **Estados válidos:** `pty_status` ∈ {`running`, `exited`}; `exit_reason` ∈ {`normal`, `killed`, `backend_restart`, `orphaned`, `spawn_failed`}.
- **Un solo `go test ./...` verde** es la condición de cierre de cada tarea.

## Estructura de archivos

| Archivo | Responsabilidad |
|---|---|
| `internal/store/store.go` | `Open`, pragmas, migraciones, `Close`, mutex de escritura |
| `internal/store/schema.go` | DDL de la v1 y la lista de migraciones |
| `internal/store/session.go` | tipos `Session`/`MetaPatch` y su CRUD, `NewID`, reconciliación |
| `internal/store/kv.go` | CRUD del KV por sesión |
| `internal/store/output.go` | append, poda y lectura del historial |
| `internal/terminal/session.go` | (modificar) `Done()`, `ExitCode()`, `Kill()` separado de `Close()` |
| `internal/session/ring.go` | ring buffer de chunks enteros |
| `internal/session/hub.go` | fan-out no bloqueante a N suscriptores |
| `internal/session/writer.go` | batcheo de output hacia SQLite + poda |
| `internal/session/manager.go` | ABM de sesiones, mapa de vivas, pump/reap, sweep |
| `internal/server/sessions.go` | handlers REST de `/api/sessions` |
| `internal/server/terminal.go` | (mover desde `server.go`) handler del WebSocket |
| `web/src/api.ts` | cliente HTTP tipado |
| `web/src/SessionList.tsx` | sidebar con la lista y el ABM |
| `web/src/TerminalView.tsx` | (modificar) recibe `sessionId` |
| `web/src/App.tsx` | (modificar) layout sidebar + terminal |

---

### Task 1: Store — apertura, pragmas y migraciones

**Files:**
- Create: `internal/store/store.go`
- Create: `internal/store/schema.go`
- Create: `internal/store/store_test.go`
- Modify: `go.mod`, `go.sum`

**Interfaces:**
- Consumes: nada.
- Produces: `store.Open(path string) (*Store, error)`, `(*Store).Close() error`, `(*Store).DB() *sql.DB` (solo para tests), campo no exportado `mu sync.Mutex` usado por las tareas 2-4 para serializar escrituras.

- [ ] **Step 1: Agregar la dependencia**

```bash
go get modernc.org/sqlite@latest
```

- [ ] **Step 2: Escribir el test que falla**

`internal/store/store_test.go`:

```go
package store

import (
	"path/filepath"
	"testing"
)

// newTestStore abre un store sobre una DB nueva en un directorio temporal.
// Sin mocks: los tests corren contra SQLite de verdad.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "sub", "webterm.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestOpenCreaSchema(t *testing.T) {
	st := newTestStore(t)

	var version int
	if err := st.DB().QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&version); err != nil {
		t.Fatalf("leyendo schema_version: %v", err)
	}
	if version != len(migrations) {
		t.Fatalf("schema_version = %d, se esperaba %d", version, len(migrations))
	}

	for _, table := range []string{"sessions", "session_kv", "session_output"} {
		var name string
		err := st.DB().QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name)
		if err != nil {
			t.Fatalf("falta la tabla %s: %v", table, err)
		}
	}
}

// TestOpenEsIdempotente: reabrir una DB ya migrada no vuelve a aplicar nada.
func TestOpenEsIdempotente(t *testing.T) {
	path := filepath.Join(t.TempDir(), "webterm.db")

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open 1: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close 1: %v", err)
	}

	st2, err := Open(path)
	if err != nil {
		t.Fatalf("Open 2: %v", err)
	}
	defer st2.Close()

	var rows int
	if err := st2.DB().QueryRow(`SELECT COUNT(*) FROM schema_version`).Scan(&rows); err != nil {
		t.Fatalf("contando migraciones: %v", err)
	}
	if rows != len(migrations) {
		t.Fatalf("se aplicaron %d migraciones, se esperaban %d", rows, len(migrations))
	}
}

// TestForeignKeysActivas: la cascada del DELETE depende del pragma, que en
// SQLite es por conexión. Si el pool abre una conexión sin él, los borrados
// dejan huérfanos en silencio.
func TestForeignKeysActivas(t *testing.T) {
	st := newTestStore(t)
	var on int
	if err := st.DB().QueryRow(`PRAGMA foreign_keys`).Scan(&on); err != nil {
		t.Fatalf("PRAGMA foreign_keys: %v", err)
	}
	if on != 1 {
		t.Fatal("foreign_keys está apagado")
	}
}
```

- [ ] **Step 3: Correr el test y verificar que falla**

Run: `go test ./internal/store/ -run TestOpen -v`
Expected: FAIL, no compila (`undefined: Open`, `undefined: Store`, `undefined: migrations`).

- [ ] **Step 4: Escribir el schema**

`internal/store/schema.go`:

```go
package store

// migrations se aplican en orden; el índice + 1 es el número de versión.
// Nunca se edita una migración ya publicada: se agrega otra al final.
var migrations = []string{schemaV1}

// schemaV1 define el modelo completo de sesión del diseño, incluidos los
// campos que la UI de M2 todavía no usa (folder_id es M4, work_status y
// kanban_status son M6/M7). Dejarlos afuera obligaría a migrar después sin
// ganar nada ahora.
const schemaV1 = `
CREATE TABLE sessions (
  id             TEXT PRIMARY KEY,
  title          TEXT    NOT NULL DEFAULT '',
  description    TEXT    NOT NULL DEFAULT '',
  folder_id      TEXT,
  cwd            TEXT    NOT NULL,
  shell          TEXT    NOT NULL,
  cols           INTEGER NOT NULL,
  rows           INTEGER NOT NULL,
  pty_status     TEXT    NOT NULL,
  exit_reason    TEXT,
  exit_code      INTEGER,
  work_status    TEXT    NOT NULL DEFAULT 'idle',
  kanban_status  TEXT    NOT NULL DEFAULT 'todo',
  created_at     INTEGER NOT NULL,
  last_active_at INTEGER NOT NULL,
  exited_at      INTEGER
);

CREATE TABLE session_kv (
  session_id TEXT    NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  key        TEXT    NOT NULL,
  value      TEXT    NOT NULL,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (session_id, key)
);

CREATE TABLE session_output (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id TEXT    NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  data       BLOB    NOT NULL,
  created_at INTEGER NOT NULL
);

CREATE INDEX idx_output_session ON session_output(session_id, id);
CREATE INDEX idx_sessions_status ON sessions(pty_status);
`
```

- [ ] **Step 5: Escribir `Open`, `Close` y `migrate`**

`internal/store/store.go`:

```go
// Package store persiste el estado de las sesiones en SQLite: metadata, KV
// por sesión e historial de output.
//
// No conoce ptys ni WebSockets a propósito: es una capa de datos pura, para
// poder probarla contra una DB de verdad sin levantar procesos.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	_ "modernc.org/sqlite"
)

// ErrNotFound lo devuelven los getters cuando la fila no existe.
var ErrNotFound = errors.New("no encontrado")

// Store es el acceso a la base.
type Store struct {
	db *sql.DB
	// mu serializa las escrituras. Con WAL los lectores nunca se bloquean,
	// pero SQLite admite un solo escritor: es más barato esperar acá que
	// comerse un SQLITE_BUSY y reintentar.
	mu sync.Mutex
}

// Open abre (y crea si hace falta) la base en path, aplica las migraciones
// pendientes y reconcilia las sesiones que quedaron marcadas como vivas en
// una ejecución anterior.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("creando %s: %w", dir, err)
		}
	}

	// Los pragmas van en el DSN porque en SQLite son por conexión: el pool de
	// database/sql abre varias y todas tienen que arrancar igual.
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("abriendo %s: %w", path, err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("conectando a %s: %w", path, err)
	}

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close cierra la base.
func (s *Store) Close() error { return s.db.Close() }

// DB expone la conexión cruda. Es para los tests y para nada más.
func (s *Store) DB() *sql.DB { return s.db }

// migrate aplica en orden las migraciones que falten, cada una junto con su
// marca de versión dentro de la misma transacción.
func (s *Store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER PRIMARY KEY)`); err != nil {
		return fmt.Errorf("creando schema_version: %w", err)
	}

	var current int
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&current); err != nil {
		return fmt.Errorf("leyendo schema_version: %w", err)
	}

	for i, stmt := range migrations {
		version := i + 1
		if version <= current {
			continue
		}
		tx, err := s.db.Begin()
		if err != nil {
			return fmt.Errorf("migración %d: %w", version, err)
		}
		if _, err := tx.Exec(stmt); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migración %d: %w", version, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_version (version) VALUES (?)`, version); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migración %d: marcando versión: %w", version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("migración %d: commit: %w", version, err)
		}
	}
	return nil
}
```

- [ ] **Step 6: Correr los tests y verificar que pasan**

Run: `go test ./internal/store/ -v`
Expected: PASS en `TestOpenCreaSchema`, `TestOpenEsIdempotente`, `TestForeignKeysActivas`.

- [ ] **Step 7: Commit**

```bash
git add go.mod go.sum internal/store/
git commit -m "feat(store): base SQLite con migraciones versionadas

Los pragmas van en el DSN porque en SQLite son por conexión y el pool
de database/sql abre varias."
```

---

### Task 2: Store — CRUD de sesiones y reconciliación de boot

**Files:**
- Create: `internal/store/session.go`
- Create: `internal/store/session_test.go`

**Interfaces:**
- Consumes: `store.Open`, `(*Store).db`, `(*Store).mu`, `ErrNotFound` (tarea 1).
- Produces:
  - `type PtyStatus string` con `StatusRunning`, `StatusExited`
  - `type ExitReason string` con `ReasonNormal`, `ReasonKilled`, `ReasonBackendRestart`, `ReasonOrphaned`, `ReasonSpawnFailed`
  - `type Session struct` (campos abajo) y `type MetaPatch struct`
  - `NewID() string`
  - `(*Store).CreateSession(*Session) error`
  - `(*Store).GetSession(id string) (*Session, error)`
  - `(*Store).ListSessions() ([]*Session, error)`
  - `(*Store).UpdateMeta(id string, p MetaPatch) error`
  - `(*Store).MarkExited(id string, reason ExitReason, code *int) error`
  - `(*Store).MarkRunning(id string, cols, rows int) error`
  - `(*Store).TouchActive(id string) error`
  - `(*Store).DeleteSession(id string) error`
  - `(*Store).RunningIDs() ([]string, error)`
  - `(*Store).ReconcileBoot() (int, error)`

- [ ] **Step 1: Escribir los tests que fallan**

`internal/store/session_test.go`:

```go
package store

import (
	"errors"
	"testing"
)

func sampleSession(id string) *Session {
	return &Session{
		ID:        id,
		Title:     "pruebas",
		Cwd:       "/tmp",
		Shell:     "/bin/bash",
		Cols:      80,
		Rows:      24,
		PtyStatus: StatusRunning,
	}
}

func TestCreateYGetSession(t *testing.T) {
	st := newTestStore(t)
	in := sampleSession("s1")
	if err := st.CreateSession(in); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if in.CreatedAt == 0 || in.LastActiveAt == 0 {
		t.Fatal("CreateSession tiene que completar los timestamps")
	}

	got, err := st.GetSession("s1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.Title != "pruebas" || got.Cwd != "/tmp" || got.Cols != 80 {
		t.Fatalf("se guardó mal: %+v", got)
	}
	if got.PtyStatus != StatusRunning {
		t.Fatalf("pty_status = %q", got.PtyStatus)
	}
	// Defaults del schema, que M6/M7 van a usar.
	if got.WorkStatus != "idle" || got.KanbanStatus != "todo" {
		t.Fatalf("defaults mal: work=%q kanban=%q", got.WorkStatus, got.KanbanStatus)
	}
}

func TestGetSessionInexistente(t *testing.T) {
	st := newTestStore(t)
	if _, err := st.GetSession("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("se esperaba ErrNotFound, vino %v", err)
	}
}

// TestListSessionsOrdenada: la más nueva primero, que es como la pinta la UI.
func TestListSessionsOrdenada(t *testing.T) {
	st := newTestStore(t)
	for _, id := range []string{"a", "b", "c"} {
		s := sampleSession(id)
		if err := st.CreateSession(s); err != nil {
			t.Fatalf("CreateSession %s: %v", id, err)
		}
	}
	list, err := st.ListSessions()
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("vinieron %d sesiones", len(list))
	}
	if list[0].ID != "c" || list[2].ID != "a" {
		t.Fatalf("orden inesperado: %s, %s, %s", list[0].ID, list[1].ID, list[2].ID)
	}
}

func TestUpdateMetaParcial(t *testing.T) {
	st := newTestStore(t)
	_ = st.CreateSession(sampleSession("s1"))

	titulo := "renombrada"
	if err := st.UpdateMeta("s1", MetaPatch{Title: &titulo}); err != nil {
		t.Fatalf("UpdateMeta: %v", err)
	}
	got, _ := st.GetSession("s1")
	if got.Title != "renombrada" {
		t.Fatalf("title = %q", got.Title)
	}
	if got.Cwd != "/tmp" {
		t.Fatal("UpdateMeta pisó un campo que no le pasaron")
	}

	if err := st.UpdateMeta("nope", MetaPatch{Title: &titulo}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("se esperaba ErrNotFound, vino %v", err)
	}
}

func TestMarkExitedYMarkRunning(t *testing.T) {
	st := newTestStore(t)
	_ = st.CreateSession(sampleSession("s1"))

	code := 3
	if err := st.MarkExited("s1", ReasonNormal, &code); err != nil {
		t.Fatalf("MarkExited: %v", err)
	}
	got, _ := st.GetSession("s1")
	if got.PtyStatus != StatusExited {
		t.Fatalf("pty_status = %q", got.PtyStatus)
	}
	if got.ExitReason != string(ReasonNormal) {
		t.Fatalf("exit_reason = %q", got.ExitReason)
	}
	if got.ExitCode == nil || *got.ExitCode != 3 {
		t.Fatalf("exit_code = %v", got.ExitCode)
	}
	if got.ExitedAt == nil {
		t.Fatal("exited_at quedó en NULL")
	}

	// Reanudar limpia los rastros de la muerte anterior: si no, la UI muestra
	// una sesión corriendo con un exit code al lado.
	if err := st.MarkRunning("s1", 120, 40); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}
	got, _ = st.GetSession("s1")
	if got.PtyStatus != StatusRunning || got.ExitReason != "" || got.ExitCode != nil || got.ExitedAt != nil {
		t.Fatalf("MarkRunning no limpió el estado de salida: %+v", got)
	}
	if got.Cols != 120 || got.Rows != 40 {
		t.Fatalf("MarkRunning no guardó el tamaño: %dx%d", got.Cols, got.Rows)
	}
}

func TestDeleteSession(t *testing.T) {
	st := newTestStore(t)
	_ = st.CreateSession(sampleSession("s1"))

	if err := st.DeleteSession("s1"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, err := st.GetSession("s1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("se esperaba ErrNotFound, vino %v", err)
	}
	if err := st.DeleteSession("s1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("borrar dos veces tiene que dar ErrNotFound, vino %v", err)
	}
}

// TestReconcileBoot cubre el caso central de M2: el pty es hijo del backend,
// así que toda sesión que la DB diga "running" al arrancar es mentira.
func TestReconcileBoot(t *testing.T) {
	st := newTestStore(t)
	_ = st.CreateSession(sampleSession("viva"))
	_ = st.CreateSession(sampleSession("muerta"))
	code := 0
	_ = st.MarkExited("muerta", ReasonNormal, &code)

	n, err := st.ReconcileBoot()
	if err != nil {
		t.Fatalf("ReconcileBoot: %v", err)
	}
	if n != 1 {
		t.Fatalf("se reconciliaron %d sesiones, se esperaba 1", n)
	}

	got, _ := st.GetSession("viva")
	if got.PtyStatus != StatusExited || got.ExitReason != string(ReasonBackendRestart) {
		t.Fatalf("no se reconcilió: %+v", got)
	}
	// La que ya estaba muerta no se toca: conserva su razón real.
	otra, _ := st.GetSession("muerta")
	if otra.ExitReason != string(ReasonNormal) {
		t.Fatalf("se pisó una sesión ya muerta: %+v", otra)
	}
}

func TestRunningIDs(t *testing.T) {
	st := newTestStore(t)
	_ = st.CreateSession(sampleSession("a"))
	_ = st.CreateSession(sampleSession("b"))
	_ = st.MarkExited("b", ReasonKilled, nil)

	ids, err := st.RunningIDs()
	if err != nil {
		t.Fatalf("RunningIDs: %v", err)
	}
	if len(ids) != 1 || ids[0] != "a" {
		t.Fatalf("RunningIDs = %v", ids)
	}
}

// TestNewIDOrdenable: los ids se ordenan por tiempo de creación, así la lista
// no depende de un ORDER BY created_at con empates al milisegundo.
func TestNewIDOrdenable(t *testing.T) {
	seen := map[string]bool{}
	prev := ""
	for i := 0; i < 500; i++ {
		id := NewID()
		if seen[id] {
			t.Fatalf("id repetido: %s", id)
		}
		seen[id] = true
		if id < prev {
			t.Fatalf("id %s salió antes que %s", id, prev)
		}
		prev = id
	}
}
```

- [ ] **Step 2: Correr los tests y verificar que fallan**

Run: `go test ./internal/store/ -run 'Session|Reconcile|Running|NewID|Meta|Mark' -v`
Expected: FAIL, no compila (`undefined: Session`, `undefined: NewID`, …).

- [ ] **Step 3: Implementar**

`internal/store/session.go`:

```go
package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// PtyStatus dice si el proceso de la sesión está vivo.
type PtyStatus string

const (
	StatusRunning PtyStatus = "running"
	StatusExited  PtyStatus = "exited"
)

// ExitReason explica por qué murió una sesión. Importa distinguirlas: un
// `exit` del usuario y un reinicio del backend se ven igual en la UI si no
// se guarda el motivo.
type ExitReason string

const (
	ReasonNormal         ExitReason = "normal"
	ReasonKilled         ExitReason = "killed"
	ReasonBackendRestart ExitReason = "backend_restart"
	ReasonOrphaned       ExitReason = "orphaned"
	ReasonSpawnFailed    ExitReason = "spawn_failed"
)

// Session es la fila de una sesión. work_status, kanban_status y folder_id se
// persisten desde M2 aunque los usen M4/M6/M7.
type Session struct {
	ID           string    `json:"id"`
	Title        string    `json:"title"`
	Description  string    `json:"description"`
	FolderID     *string   `json:"folder_id"`
	Cwd          string    `json:"cwd"`
	Shell        string    `json:"shell"`
	Cols         int       `json:"cols"`
	Rows         int       `json:"rows"`
	PtyStatus    PtyStatus `json:"pty_status"`
	ExitReason   string    `json:"exit_reason,omitempty"`
	ExitCode     *int      `json:"exit_code,omitempty"`
	WorkStatus   string    `json:"work_status"`
	KanbanStatus string    `json:"kanban_status"`
	CreatedAt    int64     `json:"created_at"`
	LastActiveAt int64     `json:"last_active_at"`
	ExitedAt     *int64    `json:"exited_at,omitempty"`
}

// MetaPatch es un update parcial: los campos en nil no se tocan.
type MetaPatch struct {
	Title        *string
	Description  *string
	WorkStatus   *string
	KanbanStatus *string
}

// NewID devuelve un id ordenable por tiempo de creación: millis en base36 con
// padding fijo, más 4 bytes de aleatoriedad para que dos sesiones creadas en
// el mismo milisegundo no colisionen ni empaten al ordenar.
func NewID() string {
	ms := strconv.FormatInt(time.Now().UnixMilli(), 36)
	var b [4]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%09s%s", ms, hex.EncodeToString(b[:]))
}

const sessionColumns = `id, title, description, folder_id, cwd, shell, cols, rows,
	pty_status, exit_reason, exit_code, work_status, kanban_status,
	created_at, last_active_at, exited_at`

func scanSession(row interface{ Scan(...any) error }) (*Session, error) {
	var s Session
	var reason sql.NullString
	err := row.Scan(&s.ID, &s.Title, &s.Description, &s.FolderID, &s.Cwd, &s.Shell,
		&s.Cols, &s.Rows, &s.PtyStatus, &reason, &s.ExitCode,
		&s.WorkStatus, &s.KanbanStatus, &s.CreatedAt, &s.LastActiveAt, &s.ExitedAt)
	if err != nil {
		return nil, err
	}
	s.ExitReason = reason.String
	return &s, nil
}

// CreateSession inserta la fila y completa los timestamps que falten.
func (s *Store) CreateSession(sess *Session) error {
	now := time.Now().UnixMilli()
	if sess.CreatedAt == 0 {
		sess.CreatedAt = now
	}
	if sess.LastActiveAt == 0 {
		sess.LastActiveAt = now
	}
	if sess.PtyStatus == "" {
		sess.PtyStatus = StatusRunning
	}
	if sess.WorkStatus == "" {
		sess.WorkStatus = "idle"
	}
	if sess.KanbanStatus == "" {
		sess.KanbanStatus = "todo"
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`
		INSERT INTO sessions (`+sessionColumns+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		sess.ID, sess.Title, sess.Description, sess.FolderID, sess.Cwd, sess.Shell,
		sess.Cols, sess.Rows, sess.PtyStatus, nullString(sess.ExitReason), sess.ExitCode,
		sess.WorkStatus, sess.KanbanStatus, sess.CreatedAt, sess.LastActiveAt, sess.ExitedAt)
	if err != nil {
		return fmt.Errorf("insertando sesión %s: %w", sess.ID, err)
	}
	return nil
}

// GetSession devuelve una sesión por id, o ErrNotFound.
func (s *Store) GetSession(id string) (*Session, error) {
	row := s.db.QueryRow(`SELECT `+sessionColumns+` FROM sessions WHERE id = ?`, id)
	sess, err := scanSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("leyendo sesión %s: %w", id, err)
	}
	return sess, nil
}

// ListSessions devuelve todas las sesiones, la más nueva primero.
func (s *Store) ListSessions() ([]*Session, error) {
	rows, err := s.db.Query(`SELECT ` + sessionColumns + ` FROM sessions ORDER BY id DESC`)
	if err != nil {
		return nil, fmt.Errorf("listando sesiones: %w", err)
	}
	defer rows.Close()

	out := []*Session{}
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, fmt.Errorf("listando sesiones: %w", err)
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

// UpdateMeta aplica un update parcial sobre los campos administrativos.
func (s *Store) UpdateMeta(id string, p MetaPatch) error {
	sets := []string{}
	args := []any{}
	if p.Title != nil {
		sets, args = append(sets, "title = ?"), append(args, *p.Title)
	}
	if p.Description != nil {
		sets, args = append(sets, "description = ?"), append(args, *p.Description)
	}
	if p.WorkStatus != nil {
		sets, args = append(sets, "work_status = ?"), append(args, *p.WorkStatus)
	}
	if p.KanbanStatus != nil {
		sets, args = append(sets, "kanban_status = ?"), append(args, *p.KanbanStatus)
	}
	if len(sets) == 0 {
		// Un PATCH vacío igual tiene que distinguir "no existe" de "nada que hacer".
		_, err := s.GetSession(id)
		return err
	}
	args = append(args, id)

	s.mu.Lock()
	defer s.mu.Unlock()
	return s.execAffecting(`UPDATE sessions SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)
}

// MarkExited registra la muerte del proceso: estado, motivo, código y momento.
func (s *Store) MarkExited(id string, reason ExitReason, code *int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.execAffecting(`
		UPDATE sessions
		SET pty_status = ?, exit_reason = ?, exit_code = ?, exited_at = ?, last_active_at = ?
		WHERE id = ?`,
		StatusExited, string(reason), code, time.Now().UnixMilli(), time.Now().UnixMilli(), id)
}

// MarkRunning vuelve a marcar la sesión como viva y borra los rastros de la
// salida anterior, para que la UI no muestre un exit code junto a una sesión
// que está corriendo.
func (s *Store) MarkRunning(id string, cols, rows int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.execAffecting(`
		UPDATE sessions
		SET pty_status = ?, exit_reason = NULL, exit_code = NULL, exited_at = NULL,
		    cols = ?, rows = ?, last_active_at = ?
		WHERE id = ?`,
		StatusRunning, cols, rows, time.Now().UnixMilli(), id)
}

// TouchActive actualiza la marca de última actividad.
func (s *Store) TouchActive(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.execAffecting(`UPDATE sessions SET last_active_at = ? WHERE id = ?`,
		time.Now().UnixMilli(), id)
}

// DeleteSession borra la sesión, su KV y su historial (por la cascada).
func (s *Store) DeleteSession(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.execAffecting(`DELETE FROM sessions WHERE id = ?`, id)
}

// RunningIDs devuelve los ids que la DB cree vivos. El manager lo usa para
// detectar filas que quedaron desincronizadas de su mapa de sesiones vivas.
func (s *Store) RunningIDs() ([]string, error) {
	rows, err := s.db.Query(`SELECT id FROM sessions WHERE pty_status = ?`, StatusRunning)
	if err != nil {
		return nil, fmt.Errorf("listando sesiones vivas: %w", err)
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

// ReconcileBoot marca como muertas todas las sesiones que la DB dejó vivas en
// la ejecución anterior. El pty es hijo del backend: si el backend arrancó de
// nuevo, ninguna sobrevivió. Devuelve cuántas filas corrigió.
func (s *Store) ReconcileBoot() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UnixMilli()
	res, err := s.db.Exec(`
		UPDATE sessions
		SET pty_status = ?, exit_reason = ?, exited_at = ?
		WHERE pty_status = ?`,
		StatusExited, string(ReasonBackendRestart), now, StatusRunning)
	if err != nil {
		return 0, fmt.Errorf("reconciliando el arranque: %w", err)
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// execAffecting corre un statement que tiene que tocar exactamente una fila y
// traduce "ninguna fila" a ErrNotFound.
func (s *Store) execAffecting(query string, args ...any) error {
	res, err := s.db.Exec(query, args...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func nullString(v string) any {
	if v == "" {
		return nil
	}
	return v
}
```

- [ ] **Step 4: Correr los tests y verificar que pasan**

Run: `go test ./internal/store/ -v`
Expected: PASS en todos.

- [ ] **Step 5: Commit**

```bash
git add internal/store/
git commit -m "feat(store): CRUD de sesiones y reconciliación de arranque

Toda fila que la DB diga running al abrir la base es mentira: el pty es
hijo del backend y no sobrevive a su reinicio."
```

---

### Task 3: Store — KV por sesión

**Files:**
- Create: `internal/store/kv.go`
- Create: `internal/store/kv_test.go`

**Interfaces:**
- Consumes: `(*Store).db`, `(*Store).mu`, `ErrNotFound`, `execAffecting` (tareas 1-2).
- Produces:
  - `(*Store).SetKV(sessionID, key, value string) error`
  - `(*Store).GetKV(sessionID, key string) (string, error)`
  - `(*Store).ListKV(sessionID string) (map[string]string, error)`
  - `(*Store).DeleteKV(sessionID, key string) error`

- [ ] **Step 1: Escribir los tests que fallan**

`internal/store/kv_test.go`:

```go
package store

import (
	"errors"
	"testing"
)

func TestKVSetGetList(t *testing.T) {
	st := newTestStore(t)
	_ = st.CreateSession(sampleSession("s1"))

	if err := st.SetKV("s1", "claude_session_id", "abc-123"); err != nil {
		t.Fatalf("SetKV: %v", err)
	}
	// Sobrescribir una clave existente es un upsert, no un error.
	if err := st.SetKV("s1", "claude_session_id", "def-456"); err != nil {
		t.Fatalf("SetKV (upsert): %v", err)
	}
	if err := st.SetKV("s1", "work_status", "working"); err != nil {
		t.Fatalf("SetKV: %v", err)
	}

	v, err := st.GetKV("s1", "claude_session_id")
	if err != nil {
		t.Fatalf("GetKV: %v", err)
	}
	if v != "def-456" {
		t.Fatalf("GetKV = %q", v)
	}

	all, err := st.ListKV("s1")
	if err != nil {
		t.Fatalf("ListKV: %v", err)
	}
	if len(all) != 2 || all["work_status"] != "working" {
		t.Fatalf("ListKV = %v", all)
	}
}

func TestKVErrores(t *testing.T) {
	st := newTestStore(t)
	_ = st.CreateSession(sampleSession("s1"))

	if _, err := st.GetKV("s1", "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetKV de clave inexistente: %v", err)
	}
	if err := st.DeleteKV("s1", "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteKV de clave inexistente: %v", err)
	}
	// Sin la foreign key, esto guardaría KV colgado de una sesión fantasma.
	if err := st.SetKV("no-existe", "k", "v"); err == nil {
		t.Fatal("SetKV sobre una sesión inexistente tiene que fallar")
	}
}

// TestKVCascadaAlBorrar: borrar la sesión se lleva su KV.
func TestKVCascadaAlBorrar(t *testing.T) {
	st := newTestStore(t)
	_ = st.CreateSession(sampleSession("s1"))
	_ = st.SetKV("s1", "k", "v")

	if err := st.DeleteSession("s1"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM session_kv WHERE session_id = ?`, "s1").Scan(&n); err != nil {
		t.Fatalf("contando kv: %v", err)
	}
	if n != 0 {
		t.Fatalf("quedaron %d filas de kv huérfanas", n)
	}
}
```

- [ ] **Step 2: Correr los tests y verificar que fallan**

Run: `go test ./internal/store/ -run TestKV -v`
Expected: FAIL, no compila (`undefined: SetKV`).

- [ ] **Step 3: Implementar**

`internal/store/kv.go`:

```go
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SetKV guarda (o pisa) una clave del contexto persistido de la sesión. Es la
// superficie que va a consumir `webterm set state` en M5.
func (s *Store) SetKV(sessionID, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`
		INSERT INTO session_kv (session_id, key, value, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(session_id, key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		sessionID, key, value, time.Now().UnixMilli())
	if err != nil {
		return fmt.Errorf("guardando %s[%s]: %w", sessionID, key, err)
	}
	return nil
}

// GetKV devuelve una clave, o ErrNotFound.
func (s *Store) GetKV(sessionID, key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM session_kv WHERE session_id = ? AND key = ?`,
		sessionID, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return v, err
}

// ListKV devuelve todo el contexto de una sesión.
func (s *Store) ListKV(sessionID string) (map[string]string, error) {
	rows, err := s.db.Query(`SELECT key, value FROM session_kv WHERE session_id = ? ORDER BY key`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("listando kv de %s: %w", sessionID, err)
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// DeleteKV borra una clave, o devuelve ErrNotFound si no estaba.
func (s *Store) DeleteKV(sessionID, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.execAffecting(`DELETE FROM session_kv WHERE session_id = ? AND key = ?`, sessionID, key)
}
```

- [ ] **Step 4: Correr los tests y verificar que pasan**

Run: `go test ./internal/store/ -v`
Expected: PASS en todos.

- [ ] **Step 5: Commit**

```bash
git add internal/store/
git commit -m "feat(store): KV persistido por sesión

Es la misma superficie que va a consumir el CLI webterm de M5; dejarla
probada ahora convierte ese milestone en un cliente HTTP y nada más."
```

---

### Task 4: Store — historial de output con cap por bytes

**Files:**
- Create: `internal/store/output.go`
- Create: `internal/store/output_test.go`

**Interfaces:**
- Consumes: `(*Store).db`, `(*Store).mu` (tarea 1), `sampleSession` (tarea 2, en tests).
- Produces:
  - `(*Store).AppendOutput(sessionID string, data []byte) error`
  - `(*Store).PruneOutput(sessionID string, maxBytes int64) error`
  - `(*Store).ReadOutput(sessionID string) ([]byte, error)`
  - `(*Store).OutputBytes(sessionID string) (int64, error)`

- [ ] **Step 1: Escribir los tests que fallan**

`internal/store/output_test.go`:

```go
package store

import (
	"bytes"
	"testing"
)

func TestAppendYReadOutput(t *testing.T) {
	st := newTestStore(t)
	_ = st.CreateSession(sampleSession("s1"))

	for _, chunk := range []string{"hola ", "mundo", "!\n"} {
		if err := st.AppendOutput("s1", []byte(chunk)); err != nil {
			t.Fatalf("AppendOutput: %v", err)
		}
	}

	got, err := st.ReadOutput("s1")
	if err != nil {
		t.Fatalf("ReadOutput: %v", err)
	}
	if string(got) != "hola mundo!\n" {
		t.Fatalf("ReadOutput = %q", got)
	}

	n, err := st.OutputBytes("s1")
	if err != nil {
		t.Fatalf("OutputBytes: %v", err)
	}
	if n != int64(len("hola mundo!\n")) {
		t.Fatalf("OutputBytes = %d", n)
	}
}

func TestReadOutputVacio(t *testing.T) {
	st := newTestStore(t)
	_ = st.CreateSession(sampleSession("s1"))

	got, err := st.ReadOutput("s1")
	if err != nil {
		t.Fatalf("ReadOutput: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("una sesión sin output tiene que dar vacío, dio %q", got)
	}
}

// TestPruneOutputRespetaElCap: la poda deja el tail dentro del cap y conserva
// siempre lo más nuevo, que es lo que el cliente necesita para redibujar.
func TestPruneOutputRespetaElCap(t *testing.T) {
	st := newTestStore(t)
	_ = st.CreateSession(sampleSession("s1"))

	// 10 chunks de 100 bytes: 1000 en total.
	for i := 0; i < 10; i++ {
		chunk := bytes.Repeat([]byte{byte('a' + i)}, 100)
		if err := st.AppendOutput("s1", chunk); err != nil {
			t.Fatalf("AppendOutput %d: %v", i, err)
		}
	}

	if err := st.PruneOutput("s1", 250); err != nil {
		t.Fatalf("PruneOutput: %v", err)
	}

	got, err := st.ReadOutput("s1")
	if err != nil {
		t.Fatalf("ReadOutput: %v", err)
	}
	if len(got) > 250 {
		t.Fatalf("quedaron %d bytes, el cap es 250", len(got))
	}
	if len(got) < 100 {
		t.Fatalf("la poda dejó %d bytes: se comió el tail", len(got))
	}
	// Lo último escrito ('j') tiene que seguir ahí; lo primero ('a'), no.
	if !bytes.Contains(got, bytes.Repeat([]byte("j"), 100)) {
		t.Fatalf("se podó el chunk más nuevo: %q", got[:min(40, len(got))])
	}
	if bytes.Contains(got, bytes.Repeat([]byte("a"), 100)) {
		t.Fatal("quedó el chunk más viejo: no se podó nada")
	}
}

// TestPruneOutputNuncaBorraTodo: un solo chunk más grande que el cap se
// conserva igual. Borrarlo dejaría al cliente sin nada que redibujar.
func TestPruneOutputNuncaBorraTodo(t *testing.T) {
	st := newTestStore(t)
	_ = st.CreateSession(sampleSession("s1"))

	if err := st.AppendOutput("s1", bytes.Repeat([]byte("x"), 500)); err != nil {
		t.Fatalf("AppendOutput: %v", err)
	}
	if err := st.PruneOutput("s1", 100); err != nil {
		t.Fatalf("PruneOutput: %v", err)
	}
	got, _ := st.ReadOutput("s1")
	if len(got) != 500 {
		t.Fatalf("se perdió el único chunk: quedaron %d bytes", len(got))
	}
}

func TestPruneOutputNoTocaOtrasSesiones(t *testing.T) {
	st := newTestStore(t)
	_ = st.CreateSession(sampleSession("s1"))
	_ = st.CreateSession(sampleSession("s2"))

	for i := 0; i < 10; i++ {
		_ = st.AppendOutput("s1", bytes.Repeat([]byte("a"), 100))
		_ = st.AppendOutput("s2", bytes.Repeat([]byte("b"), 100))
	}
	if err := st.PruneOutput("s1", 150); err != nil {
		t.Fatalf("PruneOutput: %v", err)
	}

	otra, _ := st.ReadOutput("s2")
	if len(otra) != 1000 {
		t.Fatalf("la poda de s1 tocó a s2: quedaron %d bytes", len(otra))
	}
}

func TestOutputCascadaAlBorrar(t *testing.T) {
	st := newTestStore(t)
	_ = st.CreateSession(sampleSession("s1"))
	_ = st.AppendOutput("s1", []byte("algo"))

	if err := st.DeleteSession("s1"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	var n int
	_ = st.DB().QueryRow(`SELECT COUNT(*) FROM session_output WHERE session_id = ?`, "s1").Scan(&n)
	if n != 0 {
		t.Fatalf("quedaron %d chunks huérfanos", n)
	}
}

// TestAppendOutputIgnoraVacios evita ensuciar la tabla con filas de 0 bytes.
func TestAppendOutputIgnoraVacios(t *testing.T) {
	st := newTestStore(t)
	_ = st.CreateSession(sampleSession("s1"))

	if err := st.AppendOutput("s1", nil); err != nil {
		t.Fatalf("AppendOutput(nil): %v", err)
	}
	var n int
	_ = st.DB().QueryRow(`SELECT COUNT(*) FROM session_output`).Scan(&n)
	if n != 0 {
		t.Fatalf("se insertó una fila vacía (%d filas)", n)
	}
}
```

- [ ] **Step 2: Correr los tests y verificar que fallan**

Run: `go test ./internal/store/ -run Output -v`
Expected: FAIL, no compila (`undefined: AppendOutput`).

- [ ] **Step 3: Implementar**

`internal/store/output.go`:

```go
package store

import (
	"fmt"
	"time"
)

// AppendOutput guarda un chunk de output crudo del pty. Los chunks se guardan
// enteros a propósito: al replayar nunca hay que cortar uno al medio, y cortar
// ANSI por el medio ensucia la pantalla del cliente.
func (s *Store) AppendOutput(sessionID string, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT INTO session_output (session_id, data, created_at) VALUES (?, ?, ?)`,
		sessionID, data, time.Now().UnixMilli())
	if err != nil {
		return fmt.Errorf("guardando output de %s: %w", sessionID, err)
	}
	return nil
}

// OutputBytes devuelve cuánto historial hay guardado para la sesión.
func (s *Store) OutputBytes(sessionID string) (int64, error) {
	var n int64
	err := s.db.QueryRow(`SELECT COALESCE(SUM(LENGTH(data)), 0) FROM session_output WHERE session_id = ?`,
		sessionID).Scan(&n)
	return n, err
}

// PruneOutput borra los chunks más viejos hasta que el historial entre en
// maxBytes. Recorre de lo más nuevo a lo más viejo sumando tamaños: con un cap
// de 1 MB y batches de 64 KB son unas pocas decenas de filas, así que sale
// más barato que una agregación con window functions.
//
// Siempre conserva al menos el chunk más nuevo, aunque él solo supere el cap:
// dejar el historial en cero le sacaría al cliente lo único con lo que puede
// redibujar la pantalla.
func (s *Store) PruneOutput(sessionID string, maxBytes int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	rows, err := s.db.Query(`SELECT id, LENGTH(data) FROM session_output WHERE session_id = ? ORDER BY id DESC`,
		sessionID)
	if err != nil {
		return fmt.Errorf("podando el historial de %s: %w", sessionID, err)
	}

	var kept int64
	var cutoff int64 // primer id (de los viejos) que hay que borrar, junto con todo lo anterior
	first := true
	for rows.Next() {
		var id, size int64
		if err := rows.Scan(&id, &size); err != nil {
			_ = rows.Close()
			return err
		}
		if !first && kept+size > maxBytes {
			cutoff = id
			break
		}
		kept += size
		first = false
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()

	if cutoff == 0 {
		return nil
	}
	if _, err := s.db.Exec(`DELETE FROM session_output WHERE session_id = ? AND id <= ?`, sessionID, cutoff); err != nil {
		return fmt.Errorf("podando el historial de %s: %w", sessionID, err)
	}
	return nil
}

// ReadOutput devuelve el historial completo de la sesión, en orden.
func (s *Store) ReadOutput(sessionID string) ([]byte, error) {
	rows, err := s.db.Query(`SELECT data FROM session_output WHERE session_id = ? ORDER BY id`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("leyendo el historial de %s: %w", sessionID, err)
	}
	defer rows.Close()

	var out []byte
	for rows.Next() {
		var chunk []byte
		if err := rows.Scan(&chunk); err != nil {
			return nil, err
		}
		out = append(out, chunk...)
	}
	return out, rows.Err()
}
```

- [ ] **Step 4: Correr los tests y verificar que pasan**

Run: `go test ./internal/store/ -v`
Expected: PASS en todos.

- [ ] **Step 5: Commit**

```bash
git add internal/store/
git commit -m "feat(store): historial de output con cap por bytes

Guarda chunks enteros y poda de lo más viejo: replayar un chunk cortado
al medio parte secuencias ANSI y ensucia la pantalla del cliente."
```

---

### Task 5: Terminal — `cmd.Wait()` como señal autoritativa de muerte

**Files:**
- Modify: `internal/terminal/session.go`
- Create: `internal/terminal/session_test.go`

**Interfaces:**
- Consumes: nada nuevo.
- Produces (sobre `*terminal.Session`):
  - `Done() <-chan struct{}` — se cierra cuando el proceso terminó
  - `ExitCode() int` — válido solo después de que `Done()` se cerró
  - `Kill() error` — manda SIGKILL, no espera, idempotente
  - `Close() error` — `Kill()` + esperar
  - `Read`, `Write`, `Resize` siguen con la misma firma

- [ ] **Step 1: Escribir los tests que fallan**

`internal/terminal/session_test.go`:

```go
package terminal

import (
	"io"
	"testing"
	"time"
)

func newTestSession(t *testing.T) *Session {
	t.Helper()
	s, err := New("t1", Config{Shell: "/bin/bash", Rows: 24, Cols: 80})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// drain lee en background para que el pty no se llene y frene al shell.
func drain(s *Session) {
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := s.Read(buf); err != nil {
				return
			}
		}
	}()
}

// TestDoneYExitCode: el exit code del shell llega hasta el backend. Es lo que
// el session manager escribe en la DB al reconciliar.
func TestDoneYExitCode(t *testing.T) {
	s := newTestSession(t)
	drain(s)

	if _, err := s.Write([]byte("exit 7\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	select {
	case <-s.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("timeout esperando Done")
	}
	if got := s.ExitCode(); got != 7 {
		t.Fatalf("ExitCode = %d, se esperaba 7", got)
	}
}

// TestDoneConNietoQueRetieneElPty es el caso que motiva usar cmd.Wait() en
// lugar del EOF del Read: el shell muere pero un nieto heredó el esclavo del
// pty, así que el Read no da EOF nunca. Sin esta señal la sesión quedaría
// marcada como viva para siempre.
func TestDoneConNietoQueRetieneElPty(t *testing.T) {
	s := newTestSession(t)
	drain(s)

	// El sleep queda corriendo con el pty abierto después de que el shell sale.
	if _, err := s.Write([]byte("sleep 30 & exit 0\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	select {
	case <-s.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("Done no llegó: se está esperando el EOF del Read en vez de cmd.Wait()")
	}
	if got := s.ExitCode(); got != 0 {
		t.Fatalf("ExitCode = %d", got)
	}
}

// TestKillDestrabaAlLector: cerrar el pty al morir el proceso es lo que saca
// al lector de un Read bloqueado.
func TestKillDestrabaAlLector(t *testing.T) {
	s := newTestSession(t)

	errc := make(chan error, 1)
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := s.Read(buf); err != nil {
				errc <- err
				return
			}
		}
	}()

	if err := s.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	select {
	case <-errc:
	case <-time.After(10 * time.Second):
		t.Fatal("el lector quedó bloqueado después del Kill")
	}
}

func TestCloseEsIdempotente(t *testing.T) {
	s := newTestSession(t)
	if err := s.Close(); err != nil {
		t.Fatalf("Close 1: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close 2: %v", err)
	}
	if _, err := s.Write([]byte("x")); err == nil {
		t.Fatal("escribir a una sesión cerrada tiene que fallar")
	} else if err != io.ErrClosedPipe {
		t.Fatalf("se esperaba io.ErrClosedPipe, vino %v", err)
	}
}
```

- [ ] **Step 2: Correr los tests y verificar que fallan**

Run: `go test ./internal/terminal/ -v`
Expected: FAIL, no compila (`s.Done undefined`, `s.ExitCode undefined`, `s.Kill undefined`).

- [ ] **Step 3: Reescribir el ciclo de vida de `Session`**

Reemplazar en `internal/terminal/session.go` el struct `Session` y todo lo que
va de `New` hasta el final del archivo por:

```go
// Session es un proceso corriendo bajo un pty.
type Session struct {
	ID  string
	cmd *exec.Cmd

	mu     sync.Mutex
	ptmx   *os.File
	closed bool

	// done se cierra cuando el proceso terminó; exitCode es válido a partir
	// de ahí (el cierre del canal ordena la escritura contra las lecturas).
	done     chan struct{}
	exitCode int
}

// New spawnea el shell bajo un pty nuevo.
func New(id string, cfg Config) (*Session, error) {
	shell := cfg.Shell
	if shell == "" {
		shell = os.Getenv("SHELL")
	}
	if shell == "" {
		shell = "/bin/zsh"
	}

	cwd := cfg.Cwd
	if cwd == "" {
		cwd, _ = os.UserHomeDir()
	}

	// -l para que el shell cargue el profile del usuario (PATH, aliases, etc).
	cmd := exec.Command(shell, "-l")
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(),
		"TERM=xterm-256color",
		"COLORTERM=truecolor",
		"WEBTERM_SESSION_ID="+id,
	)
	cmd.Env = append(cmd.Env, cfg.Env...)

	rows, cols := cfg.Rows, cfg.Cols
	if rows == 0 {
		rows = 24
	}
	if cols == 0 {
		cols = 80
	}

	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: rows, Cols: cols})
	if err != nil {
		return nil, fmt.Errorf("arrancando pty: %w", err)
	}

	s := &Session{ID: id, cmd: cmd, ptmx: ptmx, done: make(chan struct{})}
	go s.reap()
	return s, nil
}

// reap espera a que el proceso termine y cierra el pty.
//
// La señal autoritativa de muerte es cmd.Wait(), no el EOF del Read: si el
// shell muere pero un nieto heredó el esclavo del pty, el Read no da EOF
// nunca y la sesión quedaría marcada como viva para siempre. Cerrar el ptmx
// acá es además lo que destraba a un lector bloqueado.
func (s *Session) reap() {
	err := s.cmd.Wait()
	s.exitCode = exitCodeOf(err)

	s.mu.Lock()
	s.closed = true
	ptmx := s.ptmx
	s.mu.Unlock()

	_ = ptmx.Close()
	close(s.done)
}

// Done se cierra cuando el proceso terminó.
func (s *Session) Done() <-chan struct{} { return s.done }

// ExitCode devuelve el código de salida. Solo es válido después de que Done
// se haya cerrado; antes devuelve 0.
func (s *Session) ExitCode() int { return s.exitCode }

// Read devuelve output crudo del pty (secuencias ANSI incluidas).
func (s *Session) Read(p []byte) (int, error) { return s.ptmx.Read(p) }

// Write manda input crudo del usuario al pty.
func (s *Session) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, io.ErrClosedPipe
	}
	return s.ptmx.Write(p)
}

// Resize cambia el tamaño de la ventana del pty y le manda SIGWINCH al proceso.
func (s *Session) Resize(rows, cols uint16) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return io.ErrClosedPipe
	}
	return pty.Setsize(s.ptmx, &pty.Winsize{Rows: rows, Cols: cols})
}

// Kill le manda SIGKILL al proceso y vuelve enseguida. Es idempotente: el
// cierre real lo hace reap(). Para esperar a que termine, usar Close.
func (s *Session) Kill() error {
	select {
	case <-s.done:
		return nil
	default:
	}
	if s.cmd.Process == nil {
		return nil
	}
	if err := s.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

// Close mata el proceso y espera a que reap() termine de limpiar. Es idempotente.
func (s *Session) Close() error {
	if err := s.Kill(); err != nil {
		return err
	}
	<-s.done
	return nil
}

// exitCodeOf traduce el error de cmd.Wait a un código. -1 es "murió por una
// señal", que es lo que vemos cuando nosotros mismos lo matamos.
func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}
```

Ajustar el bloque de imports a: `errors`, `fmt`, `io`, `os`, `os/exec`, `sync`, `github.com/creack/pty`.

Actualizar también el comentario de paquete, que todavía dice que la sesión
vive mientras dure el WebSocket:

```go
// Package terminal envuelve un proceso corriendo bajo un pseudo-terminal.
//
// La Session no sabe nada de WebSockets ni de persistencia: su dueño es el
// session manager, que la mantiene viva entre conexiones.
package terminal
```

- [ ] **Step 4: Correr los tests y verificar que pasan**

Run: `go test ./internal/terminal/ -v`
Expected: PASS en los cuatro tests.

- [ ] **Step 5: Verificar que no rompimos nada y commitear**

Run: `go build ./... && go test ./internal/terminal/ ./internal/store/`
Expected: PASS. (`internal/server` todavía compila: usa `Read`/`Write`/`Resize`/`Close`, que conservan su firma.)

```bash
git add internal/terminal/
git commit -m "feat(terminal): exponer Done/ExitCode y separar Kill de Close

La señal autoritativa de muerte pasa a ser cmd.Wait(): si el shell muere
pero un nieto heredó el esclavo del pty, el Read no da EOF nunca."
```

---

### Task 6: Session — ring buffer y fan-out

**Files:**
- Create: `internal/session/ring.go`
- Create: `internal/session/hub.go`
- Create: `internal/session/fanout_test.go`

**Interfaces:**
- Consumes: nada.
- Produces (todo no exportado, dentro del paquete `session`):
  - `newRing(maxBytes int) *ring`, `(*ring).append(p []byte)`, `(*ring).snapshot() []byte`, `(*ring).preload(p []byte)`
  - `newHub() *hub`, `(*hub).subscribe(bufSize int) *subscriber`, `(*hub).unsubscribe(s *subscriber)`, `(*hub).broadcast(p []byte)`, `(*hub).closeAll()`, `(*hub).count() int`
  - `type subscriber struct { ch chan []byte; dropped bool; closed bool }` con `(*subscriber).out() <-chan []byte` y `(*subscriber).wasDropped() bool`

**Nota de diseño:** ni `ring` ni `hub` tienen mutex propio. El candado lo pone
`liveSession` (tarea 8), que necesita snapshotear el ring y suscribir al hub
como una sola operación atómica: si no, un cliente que se attachea puede
perderse un chunk o verlo dos veces.

- [ ] **Step 1: Escribir los tests que fallan**

`internal/session/fanout_test.go`:

```go
package session

import (
	"bytes"
	"testing"
)

func TestRingGuardaElTail(t *testing.T) {
	r := newRing(250)
	for i := 0; i < 10; i++ {
		r.append(bytes.Repeat([]byte{byte('a' + i)}, 100))
	}
	got := r.snapshot()
	if len(got) > 250 {
		t.Fatalf("el ring guardó %d bytes con un cap de 250", len(got))
	}
	if !bytes.Contains(got, bytes.Repeat([]byte("j"), 100)) {
		t.Fatal("se descartó el chunk más nuevo")
	}
	if bytes.Contains(got, bytes.Repeat([]byte("a"), 100)) {
		t.Fatal("quedó el chunk más viejo")
	}
}

// TestRingCopiaElChunk: el lector del pty reusa su buffer, así que el ring
// tiene que quedarse con una copia o termina mostrando basura.
func TestRingCopiaElChunk(t *testing.T) {
	r := newRing(100)
	buf := []byte("hola")
	r.append(buf)
	copy(buf, "chau")

	if string(r.snapshot()) != "hola" {
		t.Fatalf("el ring se quedó con el buffer del llamador: %q", r.snapshot())
	}
}

// TestRingNuncaQuedaVacio: un chunk más grande que el cap se conserva igual.
func TestRingNuncaQuedaVacio(t *testing.T) {
	r := newRing(10)
	r.append(bytes.Repeat([]byte("x"), 500))
	if len(r.snapshot()) != 500 {
		t.Fatalf("se descartó el único chunk: quedaron %d bytes", len(r.snapshot()))
	}
}

func TestRingPreload(t *testing.T) {
	r := newRing(250)
	r.preload(bytes.Repeat([]byte("h"), 300))
	r.append([]byte("nuevo"))

	got := r.snapshot()
	if !bytes.HasSuffix(got, []byte("nuevo")) {
		t.Fatalf("el append no quedó al final: %q", got[max(0, len(got)-10):])
	}
	if len(got) > 250 {
		t.Fatalf("el preload ignoró el cap: %d bytes", len(got))
	}
}

func TestHubBroadcastADosSuscriptores(t *testing.T) {
	h := newHub()
	a := h.subscribe(4)
	b := h.subscribe(4)

	h.broadcast([]byte("hola"))

	for name, s := range map[string]*subscriber{"a": a, "b": b} {
		select {
		case got := <-s.out():
			if string(got) != "hola" {
				t.Fatalf("%s recibió %q", name, got)
			}
		default:
			t.Fatalf("%s no recibió nada", name)
		}
	}
}

// TestHubExpulsaAlLento: un cliente que no lee no puede frenar al pty. Se lo
// desconecta a él en vez de bloquear a todos.
func TestHubExpulsaAlLento(t *testing.T) {
	h := newHub()
	lento := h.subscribe(2)
	rapido := h.subscribe(64)

	for i := 0; i < 10; i++ {
		h.broadcast([]byte("x"))
	}

	// El canal del lento quedó cerrado y marcado.
	drenado := 0
	for range lento.out() {
		drenado++
	}
	if !lento.wasDropped() {
		t.Fatal("el suscriptor lento no quedó marcado como expulsado")
	}
	if drenado > 2 {
		t.Fatalf("el lento recibió %d chunks con un buffer de 2", drenado)
	}
	if h.count() != 1 {
		t.Fatalf("quedaron %d suscriptores, se esperaba 1", h.count())
	}

	// El rápido siguió recibiendo todo.
	if len(rapido.out()) != 10 {
		t.Fatalf("el rápido recibió %d chunks, se esperaban 10", len(rapido.out()))
	}
}

func TestHubUnsubscribeEsIdempotente(t *testing.T) {
	h := newHub()
	s := h.subscribe(4)

	h.unsubscribe(s)
	h.unsubscribe(s) // no tiene que panickear por cerrar dos veces el canal

	if h.count() != 0 {
		t.Fatalf("quedaron %d suscriptores", h.count())
	}
	if _, ok := <-s.out(); ok {
		t.Fatal("el canal tenía que quedar cerrado")
	}
}

func TestHubCloseAll(t *testing.T) {
	h := newHub()
	a := h.subscribe(4)
	b := h.subscribe(4)

	h.closeAll()

	if _, ok := <-a.out(); ok {
		t.Fatal("a quedó abierto")
	}
	if _, ok := <-b.out(); ok {
		t.Fatal("b quedó abierto")
	}
	if a.wasDropped() || b.wasDropped() {
		t.Fatal("closeAll no es una expulsión: el cliente tiene que ver un cierre normal")
	}
	if h.count() != 0 {
		t.Fatalf("quedaron %d suscriptores", h.count())
	}
}
```

- [ ] **Step 2: Correr los tests y verificar que fallan**

Run: `go test ./internal/session/ -v`
Expected: FAIL, no compila (`undefined: newRing`, `undefined: newHub`).

- [ ] **Step 3: Implementar el ring**

`internal/session/ring.go`:

```go
// Package session es el dueño de los ptys vivos: los compone con el store,
// reparte su output a N clientes y reconcilia el estado cuando mueren.
package session

// ring guarda los últimos maxBytes de output como chunks enteros.
//
// Nunca corta un chunk al medio: replayar media secuencia ANSI o medio
// carácter UTF-8 le ensucia la pantalla al cliente que se attachea. Por eso el
// tail puede quedar un poco por encima o por debajo del cap.
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
```

- [ ] **Step 4: Implementar el hub**

`internal/session/hub.go`:

```go
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
```

- [ ] **Step 5: Correr los tests y verificar que pasan**

Run: `go test ./internal/session/ -v -race`
Expected: PASS en los siete tests.

- [ ] **Step 6: Commit**

```bash
git add internal/session/
git commit -m "feat(session): ring buffer de chunks y fan-out no bloqueante

Al cliente que no lee lo expulsamos en vez de frenar el pty: un solo
cliente lento congelaría la sesión para todos los demás."
```

---

### Task 7: Session — writer batcheado hacia SQLite

**Files:**
- Create: `internal/session/writer.go`
- Create: `internal/session/writer_test.go`

**Interfaces:**
- Consumes: `store.Store` con `AppendOutput` y `PruneOutput` (tarea 4).
- Produces:
  - `newOutputWriter(st *store.Store, sessionID string, maxBytes int64) *outputWriter`
  - `(*outputWriter).write(p []byte)` — no bloqueante, nunca frena al pty
  - `(*outputWriter).close()` — hace el último flush y espera a que termine
  - constantes `flushInterval = 250 * time.Millisecond`, `flushThreshold = 64 * 1024`

- [ ] **Step 1: Escribir los tests que fallan**

`internal/session/writer_test.go`:

```go
package session

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/giuliano/webterm/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "webterm.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func seedSession(t *testing.T, st *store.Store, id string) {
	t.Helper()
	err := st.CreateSession(&store.Session{
		ID: id, Cwd: "/tmp", Shell: "/bin/bash", Cols: 80, Rows: 24,
		PtyStatus: store.StatusRunning,
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
}

// TestWriterPersisteAlCerrar: close() tiene que dejar el historial completo.
// El reaper lo llama antes de marcar la sesión como muerta, así que si el
// flush final se pierde, se pierden los últimos segundos de la sesión.
func TestWriterPersisteAlCerrar(t *testing.T) {
	st := newTestStore(t)
	seedSession(t, st, "s1")

	w := newOutputWriter(st, "s1", 1<<20)
	w.write([]byte("hola "))
	w.write([]byte("mundo"))
	w.close()

	got, err := st.ReadOutput("s1")
	if err != nil {
		t.Fatalf("ReadOutput: %v", err)
	}
	if string(got) != "hola mundo" {
		t.Fatalf("historial = %q", got)
	}
}

// TestWriterPodaAlCap: el historial en la base no crece sin límite.
func TestWriterPodaAlCap(t *testing.T) {
	st := newTestStore(t)
	seedSession(t, st, "s1")

	w := newOutputWriter(st, "s1", 200)
	for i := 0; i < 10; i++ {
		w.write(bytes.Repeat([]byte{byte('a' + i)}, 100))
		w.flushNow()
	}
	w.close()

	got, _ := st.ReadOutput("s1")
	if len(got) > 200 {
		t.Fatalf("quedaron %d bytes con un cap de 200", len(got))
	}
	if !bytes.Contains(got, bytes.Repeat([]byte("j"), 100)) {
		t.Fatal("se podó el chunk más nuevo")
	}
}

// TestWriterCopiaElChunk: igual que el ring, el writer no puede quedarse con
// el buffer que reusa el lector del pty.
func TestWriterCopiaElChunk(t *testing.T) {
	st := newTestStore(t)
	seedSession(t, st, "s1")

	w := newOutputWriter(st, "s1", 1<<20)
	buf := []byte("hola")
	w.write(buf)
	copy(buf, "chau")
	w.close()

	got, _ := st.ReadOutput("s1")
	if string(got) != "hola" {
		t.Fatalf("historial = %q", got)
	}
}

func TestWriterCloseEsIdempotente(t *testing.T) {
	st := newTestStore(t)
	seedSession(t, st, "s1")

	w := newOutputWriter(st, "s1", 1<<20)
	w.write([]byte("x"))
	w.close()
	w.close()
	// Escribir después de cerrar no tiene que panickear ni persistir nada.
	w.write([]byte("tarde"))

	got, _ := st.ReadOutput("s1")
	if string(got) != "x" {
		t.Fatalf("historial = %q", got)
	}
}
```

- [ ] **Step 2: Correr los tests y verificar que fallan**

Run: `go test ./internal/session/ -run TestWriter -v`
Expected: FAIL, no compila (`undefined: newOutputWriter`).

- [ ] **Step 3: Implementar**

`internal/session/writer.go`:

```go
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
// segundo. El buffer vive en memoria y se acumula como máximo lo que entre en
// un intervalo de flush, así que write() nunca bloquea al lector del pty.
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
```

- [ ] **Step 4: Correr los tests y verificar que pasan**

Run: `go test ./internal/session/ -v -race`
Expected: PASS en todos.

- [ ] **Step 5: Commit**

```bash
git add internal/session/
git commit -m "feat(session): writer batcheado del historial hacia SQLite

Un cat de un archivo grande dispararía miles de INSERT por segundo sin
el batch; el buffer intermedio deja que write() nunca frene al pty."
```

---

### Task 8: Session — Manager: crear, attachear y bombear el pty

**Files:**
- Create: `internal/session/manager.go`
- Create: `internal/session/manager_test.go`
- Modify: `internal/store/session.go` (agregar `UpdateSize`)
- Modify: `internal/store/session_test.go` (test de `UpdateSize`)

**Interfaces:**
- Consumes: `store` (tareas 1-4), `terminal.Session` con `Done`/`ExitCode`/`Kill` (tarea 5), `ring`/`hub`/`subscriber` (tarea 6), `outputWriter` (tarea 7).
- Produces:
  - `(*store.Store).UpdateSize(id string, cols, rows int) error`
  - `type Config struct { Shell string; HistoryBytes int64; SweepEvery time.Duration }`
  - `DefaultHistoryBytes int64 = 1 << 20`
  - `NewManager(st *store.Store, cfg Config) *Manager`
  - `(*Manager).Start() error`
  - `type CreateOpts struct { Title, Description, Cwd string; Cols, Rows int }`
  - `(*Manager).Create(o CreateOpts) (*store.Session, error)`
  - `(*Manager).List() ([]*store.Session, error)`, `(*Manager).Get(id string) (*store.Session, error)`
  - `(*Manager).UpdateMeta(id string, p store.MetaPatch) (*store.Session, error)`
  - `(*Manager).Attach(id string) (*Attachment, error)`
  - `type Attachment struct { Session *store.Session; History []byte; Live bool; Output <-chan []byte }` con `(*Attachment).Detach()` y `(*Attachment).Dropped() bool`
  - `(*Manager).Write(id string, p []byte) error`, `(*Manager).Resize(id string, rows, cols uint16) error`
  - `var ErrNotLive = errors.New("la sesión no está corriendo")`

- [ ] **Step 1: Agregar `UpdateSize` al store con su test**

En `internal/store/session_test.go`:

```go
func TestUpdateSize(t *testing.T) {
	st := newTestStore(t)
	_ = st.CreateSession(sampleSession("s1"))

	if err := st.UpdateSize("s1", 140, 50); err != nil {
		t.Fatalf("UpdateSize: %v", err)
	}
	got, _ := st.GetSession("s1")
	if got.Cols != 140 || got.Rows != 50 {
		t.Fatalf("tamaño = %dx%d", got.Cols, got.Rows)
	}
}
```

En `internal/store/session.go`:

```go
// UpdateSize persiste el último tamaño conocido de la terminal, para que una
// sesión reanudada vuelva con las dimensiones que tenía.
func (s *Store) UpdateSize(id string, cols, rows int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.execAffecting(`UPDATE sessions SET cols = ?, rows = ?, last_active_at = ? WHERE id = ?`,
		cols, rows, time.Now().UnixMilli(), id)
}
```

Run: `go test ./internal/store/ -run TestUpdateSize -v` → PASS.

- [ ] **Step 2: Escribir los tests del manager (fallan)**

`internal/session/manager_test.go`:

```go
package session

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/giuliano/webterm/internal/store"
)

func newTestManager(t *testing.T) (*Manager, *store.Store) {
	t.Helper()
	st := newTestStore(t)
	m := NewManager(st, Config{Shell: "/bin/bash", HistoryBytes: 1 << 20, SweepEvery: time.Hour})
	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m, st
}

// awaitChunk acumula output del canal hasta encontrar want.
func awaitChunk(t *testing.T, ch <-chan []byte, want string) string {
	t.Helper()
	var sb strings.Builder
	deadline := time.After(15 * time.Second)
	for {
		select {
		case chunk, ok := <-ch:
			if !ok {
				t.Fatalf("el canal se cerró esperando %q (acumulado %q)", want, sb.String())
			}
			sb.Write(chunk)
			if strings.Contains(sb.String(), want) {
				return sb.String()
			}
		case <-deadline:
			t.Fatalf("timeout esperando %q (acumulado %q)", want, sb.String())
		}
	}
}

func TestCreatePersisteYCorre(t *testing.T) {
	m, st := newTestManager(t)

	rec, err := m.Create(CreateOpts{Title: "una sesión", Cwd: "/tmp", Cols: 80, Rows: 24})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if rec.ID == "" {
		t.Fatal("Create no asignó id")
	}

	got, err := st.GetSession(rec.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.PtyStatus != store.StatusRunning {
		t.Fatalf("pty_status = %q", got.PtyStatus)
	}
	if got.Title != "una sesión" || got.Cwd != "/tmp" {
		t.Fatalf("metadata mal guardada: %+v", got)
	}
}

// TestSobreviveAlDetach es la premisa entera de M2: cerrar el cliente no mata
// el proceso, y al volver se ve lo que pasó mientras tanto.
func TestSobreviveAlDetach(t *testing.T) {
	m, _ := newTestManager(t)
	rec, err := m.Create(CreateOpts{Cwd: "/tmp", Cols: 80, Rows: 24})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	att, err := m.Attach(rec.ID)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if !att.Live {
		t.Fatal("la sesión recién creada tendría que estar viva")
	}
	if err := m.Write(rec.ID, []byte("echo marca-uno\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	awaitChunk(t, att.Output, "marca-uno")
	att.Detach()

	// Con el cliente desconectado, el proceso sigue trabajando.
	if err := m.Write(rec.ID, []byte("echo marca-dos\n")); err != nil {
		t.Fatalf("Write con el cliente desconectado: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	att2, err := m.Attach(rec.ID)
	if err != nil {
		t.Fatalf("re-Attach: %v", err)
	}
	defer att2.Detach()
	if !att2.Live {
		t.Fatal("la sesión murió al desattachear")
	}
	if !bytes.Contains(att2.History, []byte("marca-dos")) {
		t.Fatalf("el replay no trae lo que pasó estando desconectado: %q", tail(att2.History, 200))
	}
	if !bytes.Contains(att2.History, []byte("marca-uno")) {
		t.Fatalf("el replay perdió lo de antes del detach: %q", tail(att2.History, 200))
	}
}

// TestExitDelShellSeReconcilia: el estado en la DB sigue al proceso real.
func TestExitDelShellSeReconcilia(t *testing.T) {
	m, st := newTestManager(t)
	rec, _ := m.Create(CreateOpts{Cwd: "/tmp", Cols: 80, Rows: 24})

	att, err := m.Attach(rec.ID)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if err := m.Write(rec.ID, []byte("exit 5\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// El canal se cierra cuando la sesión muere.
	deadline := time.After(15 * time.Second)
	for open := true; open; {
		select {
		case _, ok := <-att.Output:
			open = ok
		case <-deadline:
			t.Fatal("timeout esperando el cierre del canal")
		}
	}

	got, err := st.GetSession(rec.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.PtyStatus != store.StatusExited {
		t.Fatalf("pty_status = %q", got.PtyStatus)
	}
	if got.ExitReason != string(store.ReasonNormal) {
		t.Fatalf("exit_reason = %q", got.ExitReason)
	}
	if got.ExitCode == nil || *got.ExitCode != 5 {
		t.Fatalf("exit_code = %v", got.ExitCode)
	}
}

// TestAttachASesionMuerta: se puede ver el historial de una sesión terminada
// sin una vista aparte, en modo lectura.
func TestAttachASesionMuerta(t *testing.T) {
	m, _ := newTestManager(t)
	rec, _ := m.Create(CreateOpts{Cwd: "/tmp", Cols: 80, Rows: 24})

	att, _ := m.Attach(rec.ID)
	_ = m.Write(rec.ID, []byte("echo antes-de-morir\n"))
	awaitChunk(t, att.Output, "antes-de-morir")
	_ = m.Write(rec.ID, []byte("exit\n"))
	att.Detach()

	waitDead(t, m, rec.ID)

	muerta, err := m.Attach(rec.ID)
	if err != nil {
		t.Fatalf("Attach a sesión muerta: %v", err)
	}
	defer muerta.Detach()
	if muerta.Live {
		t.Fatal("Live tendría que ser false")
	}
	if muerta.Output != nil {
		t.Fatal("una sesión muerta no tiene stream vivo")
	}
	if !bytes.Contains(muerta.History, []byte("antes-de-morir")) {
		t.Fatalf("el historial no sobrevivió: %q", tail(muerta.History, 200))
	}
	// El input a una sesión muerta no revive nada.
	if err := m.Write(rec.ID, []byte("echo tarde\n")); err == nil {
		t.Fatal("escribir a una sesión muerta tendría que fallar")
	}
}

func TestAttachInexistente(t *testing.T) {
	m, _ := newTestManager(t)
	if _, err := m.Attach("no-existe"); err == nil {
		t.Fatal("se esperaba un error")
	}
}

// TestFanOutADosClientes: dos pestañas abiertas sobre la misma sesión ven lo
// mismo.
func TestFanOutADosClientes(t *testing.T) {
	m, _ := newTestManager(t)
	rec, _ := m.Create(CreateOpts{Cwd: "/tmp", Cols: 80, Rows: 24})

	a, _ := m.Attach(rec.ID)
	defer a.Detach()
	b, _ := m.Attach(rec.ID)
	defer b.Detach()

	if err := m.Write(rec.ID, []byte("echo dos-clientes\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	awaitChunk(t, a.Output, "dos-clientes")
	awaitChunk(t, b.Output, "dos-clientes")
}

// TestResizeLlegaAlPtyYSePersiste: el shell ve el tamaño nuevo y la DB lo
// recuerda para cuando se reanude la sesión.
func TestResizeLlegaAlPtyYSePersiste(t *testing.T) {
	m, st := newTestManager(t)
	rec, _ := m.Create(CreateOpts{Cwd: "/tmp", Cols: 80, Rows: 24})

	att, _ := m.Attach(rec.ID)
	defer att.Detach()

	if err := m.Resize(rec.ID, 45, 123); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	_ = m.Write(rec.ID, []byte("stty size\n"))
	awaitChunk(t, att.Output, "45 123")

	got, _ := st.GetSession(rec.ID)
	if got.Cols != 123 || got.Rows != 45 {
		t.Fatalf("la DB guardó %dx%d", got.Cols, got.Rows)
	}
}

func TestUpdateMeta(t *testing.T) {
	m, _ := newTestManager(t)
	rec, _ := m.Create(CreateOpts{Title: "vieja", Cwd: "/tmp", Cols: 80, Rows: 24})

	nuevo := "nueva"
	got, err := m.UpdateMeta(rec.ID, store.MetaPatch{Title: &nuevo})
	if err != nil {
		t.Fatalf("UpdateMeta: %v", err)
	}
	if got.Title != "nueva" {
		t.Fatalf("title = %q", got.Title)
	}
}

// waitDead espera a que la sesión quede marcada como muerta en la DB.
func waitDead(t *testing.T, m *Manager, id string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		got, err := m.Get(id)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.PtyStatus == store.StatusExited {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("la sesión %s nunca quedó marcada como muerta", id)
}

func tail(p []byte, n int) string {
	if len(p) > n {
		p = p[len(p)-n:]
	}
	return string(p)
}
```

- [ ] **Step 3: Correr los tests y verificar que fallan**

Run: `go test ./internal/session/ -run 'TestCreate|TestSobrevive|TestExitDel|TestAttach|TestFanOut|TestResize|TestUpdateMeta' -v`
Expected: FAIL, no compila (`undefined: NewManager`).

- [ ] **Step 4: Implementar el manager**

`internal/session/manager.go`:

```go
package session

import (
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/giuliano/webterm/internal/store"
	"github.com/giuliano/webterm/internal/terminal"
)

const (
	// Tamaño del chunk de lectura del pty.
	readBufSize = 32 * 1024
	// Cuántos chunks se le bufferean a un cliente antes de darlo por lento.
	subBuffer = 256
	// Cada cuánto se verifica que la DB y el mapa de sesiones vivas coincidan.
	defaultSweepInterval = 30 * time.Second
	// Cuánto se espera a que una sesión muerta termine de reconciliarse.
	reapTimeout = 5 * time.Second
)

// DefaultHistoryBytes es el cap de historial por sesión.
const DefaultHistoryBytes int64 = 1 << 20

// ErrNotLive lo devuelven las operaciones que necesitan un proceso vivo.
var ErrNotLive = errors.New("la sesión no está corriendo")

// Config parametriza el manager.
type Config struct {
	Shell        string        // shell a spawnear; vacío = $SHELL
	HistoryBytes int64         // cap de historial por sesión
	SweepEvery   time.Duration // cada cuánto corre la verificación de invariante
}

// Manager es el dueño de los ptys vivos. Es la única capa que compone
// terminal con store; el servidor HTTP habla solo con él.
type Manager struct {
	st  *store.Store
	cfg Config

	mu   sync.RWMutex
	live map[string]*liveSession

	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// liveSession es una sesión con proceso corriendo.
type liveSession struct {
	id  string
	pty *terminal.Session

	// mu sincroniza ring y hub juntos: attach tiene que snapshotear el tail y
	// suscribirse en una sola operación atómica, o el cliente que entra se
	// pierde un chunk o lo ve dos veces.
	mu   sync.Mutex
	ring *ring
	hub  *hub

	writer *outputWriter
	killed atomic.Bool

	pumpDone chan struct{}
	reaped   chan struct{}
}

// emit manda un chunk al ring, a los clientes y al historial.
func (l *liveSession) emit(p []byte) {
	l.mu.Lock()
	l.ring.append(p)
	l.hub.broadcast(p)
	l.mu.Unlock()
	l.writer.write(p)
}

func (l *liveSession) attach(bufSize int) ([]byte, *subscriber) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.ring.snapshot(), l.hub.subscribe(bufSize)
}

func (l *liveSession) detach(s *subscriber) {
	l.mu.Lock()
	l.hub.unsubscribe(s)
	l.mu.Unlock()
}

// NewManager construye el manager. No toca la base hasta Start.
func NewManager(st *store.Store, cfg Config) *Manager {
	if cfg.HistoryBytes <= 0 {
		cfg.HistoryBytes = DefaultHistoryBytes
	}
	if cfg.SweepEvery <= 0 {
		cfg.SweepEvery = defaultSweepInterval
	}
	return &Manager{
		st:   st,
		cfg:  cfg,
		live: map[string]*liveSession{},
		stop: make(chan struct{}),
	}
}

// Start reconcilia lo que quedó de la ejecución anterior y arranca la
// verificación periódica. Tiene que correr antes de aceptar requests: si no,
// hay una ventana en la que la API reporta vivas sesiones que no lo están.
func (m *Manager) Start() error {
	n, err := m.st.ReconcileBoot()
	if err != nil {
		return err
	}
	if n > 0 {
		log.Printf("reconciliadas %d sesiones que el reinicio del backend se llevó puestas", n)
	}
	m.wg.Add(1)
	go m.sweepLoop()
	return nil
}

// CreateOpts describe la sesión a crear.
type CreateOpts struct {
	Title       string
	Description string
	Cwd         string
	Cols, Rows  int
}

// Create persiste la sesión y spawnea su pty.
func (m *Manager) Create(o CreateOpts) (*store.Session, error) {
	if o.Cols <= 0 {
		o.Cols = 80
	}
	if o.Rows <= 0 {
		o.Rows = 24
	}
	shell := m.cfg.Shell
	if shell == "" {
		shell = os.Getenv("SHELL")
	}
	if shell == "" {
		shell = "/bin/zsh"
	}
	cwd := o.Cwd
	if cwd == "" {
		cwd, _ = os.UserHomeDir()
	}

	rec := &store.Session{
		ID: store.NewID(), Title: o.Title, Description: o.Description,
		Cwd: cwd, Shell: shell, Cols: o.Cols, Rows: o.Rows,
		PtyStatus: store.StatusRunning,
	}

	pt, err := terminal.New(rec.ID, terminal.Config{
		Shell: shell, Cwd: cwd, Rows: uint16(o.Rows), Cols: uint16(o.Cols),
	})
	if err != nil {
		// Dejamos la fila igual, marcada como fallida: así el error aparece
		// en la UI en vez de perderse en un log del servidor.
		now := time.Now().UnixMilli()
		rec.PtyStatus = store.StatusExited
		rec.ExitReason = string(store.ReasonSpawnFailed)
		rec.ExitedAt = &now
		if cerr := m.st.CreateSession(rec); cerr != nil {
			log.Printf("[%s] no se pudo registrar el spawn fallido: %v", rec.ID, cerr)
		}
		return nil, fmt.Errorf("spawneando la sesión: %w", err)
	}

	// El insert va bajo el mismo candado que el registro en el mapa: el sweep
	// toma RLock, así que nunca puede ver una fila viva sin sesión asociada y
	// declararla huérfana por error.
	m.mu.Lock()
	if err := m.st.CreateSession(rec); err != nil {
		m.mu.Unlock()
		_ = pt.Close()
		return nil, err
	}
	m.startLive(rec, pt, nil)
	m.mu.Unlock()

	log.Printf("[%s] sesión creada (%dx%d) en %s", rec.ID, o.Cols, o.Rows, cwd)
	return rec, nil
}

// startLive arma la sesión viva y lanza sus goroutines. Hay que llamarla con
// m.mu tomado.
func (m *Manager) startLive(rec *store.Session, pt *terminal.Session, banner []byte) *liveSession {
	l := &liveSession{
		id:       rec.ID,
		pty:      pt,
		ring:     newRing(int(m.cfg.HistoryBytes)),
		hub:      newHub(),
		writer:   newOutputWriter(m.st, rec.ID, m.cfg.HistoryBytes),
		pumpDone: make(chan struct{}),
		reaped:   make(chan struct{}),
	}
	// Sembramos el ring con lo que ya hay en la base para que una sesión
	// reanudada replaye también lo anterior al corte.
	if hist, err := m.st.ReadOutput(rec.ID); err != nil {
		log.Printf("[%s] no se pudo leer el historial: %v", rec.ID, err)
	} else {
		l.ring.preload(hist)
	}
	if len(banner) > 0 {
		// Antes de arrancar el pump, así el marcador queda antes del prompt.
		l.ring.append(banner)
		l.writer.write(banner)
	}

	m.live[rec.ID] = l
	m.wg.Add(2)
	go m.pump(l)
	go m.reap(l)
	return l
}

// pump lee el pty y reparte cada chunk al ring, a los clientes y al historial.
func (m *Manager) pump(l *liveSession) {
	defer m.wg.Done()
	defer close(l.pumpDone)

	buf := make([]byte, readBufSize)
	for {
		n, err := l.pty.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			l.emit(chunk)
		}
		if err != nil {
			return
		}
	}
}

// reap espera la muerte del proceso y deja la DB y los clientes consistentes.
func (m *Manager) reap(l *liveSession) {
	defer m.wg.Done()

	<-l.pty.Done()
	// El ptmx ya está cerrado, así que el pump sale enseguida. Lo esperamos
	// para que no quede escribiendo después del flush final.
	<-l.pumpDone
	l.writer.close()

	reason := store.ReasonNormal
	if l.killed.Load() {
		reason = store.ReasonKilled
	}
	code := l.pty.ExitCode()

	m.mu.Lock()
	if m.live[l.id] == l {
		delete(m.live, l.id)
	}
	m.mu.Unlock()

	if err := m.st.MarkExited(l.id, reason, &code); err != nil && !errors.Is(err, store.ErrNotFound) {
		log.Printf("[%s] no se pudo marcar la salida: %v", l.id, err)
	}

	l.mu.Lock()
	l.hub.closeAll()
	l.mu.Unlock()

	close(l.reaped)
	log.Printf("[%s] sesión terminada (%s, código %d)", l.id, reason, code)
}

// Attachment es la conexión de un cliente a una sesión.
type Attachment struct {
	Session *store.Session
	// History es el replay que hay que mandar antes del stream vivo.
	History []byte
	// Live dice si hay proceso corriendo. Si es false, Output es nil y la
	// conexión queda de solo lectura.
	Live   bool
	Output <-chan []byte

	sub  *subscriber
	live *liveSession
}

// Detach desconecta al cliente sin tocar la sesión.
func (a *Attachment) Detach() {
	if a.live != nil && a.sub != nil {
		a.live.detach(a.sub)
	}
}

// Dropped dice si al cliente lo expulsamos por no leer a tiempo.
func (a *Attachment) Dropped() bool {
	return a.sub != nil && a.sub.wasDropped()
}

// Attach conecta un cliente. Una sesión muerta se attachea igual, en modo
// lectura: así ver su historial no necesita una vista aparte.
func (m *Manager) Attach(id string) (*Attachment, error) {
	rec, err := m.st.GetSession(id)
	if err != nil {
		return nil, err
	}

	l := m.lookup(id)
	if l == nil {
		hist, err := m.st.ReadOutput(id)
		if err != nil {
			return nil, err
		}
		return &Attachment{Session: rec, History: sanitizeReplay(hist)}, nil
	}

	hist, sub := l.attach(subBuffer)
	_ = m.st.TouchActive(id)
	return &Attachment{
		Session: rec, History: sanitizeReplay(hist), Live: true,
		Output: sub.out(), sub: sub, live: l,
	}, nil
}

// sanitizeReplay prepara el tail para un cliente nuevo. El historial está
// cortado en el cap, así que puede empezar en medio de un carácter UTF-8 y
// arrastrar atributos de color abiertos antes del corte.
func sanitizeReplay(p []byte) []byte {
	for len(p) > 0 && p[0]&0xC0 == 0x80 {
		p = p[1:]
	}
	if len(p) == 0 {
		return nil
	}
	return append([]byte("\x1b[0m"), p...)
}

// Write manda input al pty.
func (m *Manager) Write(id string, p []byte) error {
	l := m.lookup(id)
	if l == nil {
		return ErrNotLive
	}
	if _, err := l.pty.Write(p); err != nil {
		return err
	}
	return nil
}

// Resize cambia el tamaño del pty y lo persiste, para que al reanudar la
// sesión vuelva con las dimensiones que tenía.
func (m *Manager) Resize(id string, rows, cols uint16) error {
	l := m.lookup(id)
	if l == nil {
		return ErrNotLive
	}
	if err := l.pty.Resize(rows, cols); err != nil {
		return err
	}
	return m.st.UpdateSize(id, int(cols), int(rows))
}

// List devuelve todas las sesiones, la más nueva primero.
func (m *Manager) List() ([]*store.Session, error) { return m.st.ListSessions() }

// Get devuelve una sesión por id.
func (m *Manager) Get(id string) (*store.Session, error) { return m.st.GetSession(id) }

// UpdateMeta aplica un update parcial y devuelve la sesión ya actualizada.
func (m *Manager) UpdateMeta(id string, p store.MetaPatch) (*store.Session, error) {
	if err := m.st.UpdateMeta(id, p); err != nil {
		return nil, err
	}
	return m.st.GetSession(id)
}

func (m *Manager) lookup(id string) *liveSession {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.live[id]
}
```

**Nota:** `sweepLoop` y `Close` los agrega la tarea 9. Para que esta tarea
compile y sus tests pasen, agregar por ahora al final del archivo:

```go
func (m *Manager) sweepLoop() { defer m.wg.Done(); <-m.stop }

// Close apaga el manager. La tarea 9 lo completa con el sweep.
func (m *Manager) Close() error {
	m.stopOnce.Do(func() { close(m.stop) })
	m.mu.RLock()
	live := make([]*liveSession, 0, len(m.live))
	for _, l := range m.live {
		live = append(live, l)
	}
	m.mu.RUnlock()
	for _, l := range live {
		l.killed.Store(true)
		_ = l.pty.Kill()
	}
	m.wg.Wait()
	return nil
}
```

- [ ] **Step 5: Correr los tests y verificar que pasan**

Run: `go test ./internal/session/ -v -race`
Expected: PASS en todos, incluido `TestSobreviveAlDetach`.

- [ ] **Step 6: Commit**

```bash
git add internal/session/ internal/store/
git commit -m "feat(session): manager de sesiones vivas con attach y replay

El pty deja de estar atado al cliente: desattachear solo saca al
suscriptor, y al volver se replaya el tail que quedó en el ring."
```

---

### Task 9: Session — matar, reanudar, borrar y sweep de huérfanas

**Files:**
- Modify: `internal/session/manager.go`
- Create: `internal/session/lifecycle_test.go`

**Interfaces:**
- Consumes: todo lo de la tarea 8.
- Produces:
  - `(*Manager).Kill(id string) error` — sincrónico: al volver, la DB ya dice `exited`
  - `(*Manager).Restart(id string, cols, rows int) (*store.Session, error)`
  - `(*Manager).Delete(id string) error`
  - `(*Manager).Sweep() int`
  - `var ErrAlreadyRunning = errors.New("la sesión ya está corriendo")`
  - `sweepLoop` y `Close` completos (reemplazan los provisorios de la tarea 8)

- [ ] **Step 1: Escribir los tests que fallan**

`internal/session/lifecycle_test.go`:

```go
package session

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/giuliano/webterm/internal/store"
)

// TestKillConservaElHistorial: matar no es borrar. La distinción es explícita
// justamente para que el historial no se vaya sin que nadie lo pida.
func TestKillConservaElHistorial(t *testing.T) {
	m, st := newTestManager(t)
	rec, _ := m.Create(CreateOpts{Cwd: "/tmp", Cols: 80, Rows: 24})

	att, _ := m.Attach(rec.ID)
	_ = m.Write(rec.ID, []byte("echo sobrevive-al-kill\n"))
	awaitChunk(t, att.Output, "sobrevive-al-kill")
	att.Detach()

	if err := m.Kill(rec.ID); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	// Kill es sincrónico: al volver, la DB ya tiene que estar reconciliada.
	got, err := st.GetSession(rec.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.PtyStatus != store.StatusExited {
		t.Fatalf("pty_status = %q", got.PtyStatus)
	}
	if got.ExitReason != string(store.ReasonKilled) {
		t.Fatalf("exit_reason = %q, se esperaba killed", got.ExitReason)
	}

	hist, _ := st.ReadOutput(rec.ID)
	if !bytes.Contains(hist, []byte("sobrevive-al-kill")) {
		t.Fatalf("el kill se llevó el historial: %q", tail(hist, 200))
	}
}

func TestKillEsIdempotente(t *testing.T) {
	m, _ := newTestManager(t)
	rec, _ := m.Create(CreateOpts{Cwd: "/tmp", Cols: 80, Rows: 24})

	if err := m.Kill(rec.ID); err != nil {
		t.Fatalf("Kill 1: %v", err)
	}
	if err := m.Kill(rec.ID); err != nil {
		t.Fatalf("Kill 2: %v", err)
	}
	if err := m.Kill("no-existe"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Kill de inexistente: %v", err)
	}
}

// TestRestartReusaLaFila: reanudar conserva id, título, KV e historial. Es lo
// que M6 va a necesitar para colgarle el `claude --resume`.
func TestRestartReusaLaFila(t *testing.T) {
	m, st := newTestManager(t)
	rec, _ := m.Create(CreateOpts{Title: "con historia", Cwd: "/tmp", Cols: 80, Rows: 24})
	_ = st.SetKV(rec.ID, "claude_session_id", "abc-123")

	att, _ := m.Attach(rec.ID)
	_ = m.Write(rec.ID, []byte("echo antes-del-restart\n"))
	awaitChunk(t, att.Output, "antes-del-restart")
	att.Detach()
	if err := m.Kill(rec.ID); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	vuelto, err := m.Restart(rec.ID, 100, 30)
	if err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if vuelto.ID != rec.ID {
		t.Fatalf("Restart cambió el id: %s -> %s", rec.ID, vuelto.ID)
	}
	if vuelto.Title != "con historia" {
		t.Fatalf("se perdió el título: %q", vuelto.Title)
	}
	if vuelto.PtyStatus != store.StatusRunning {
		t.Fatalf("pty_status = %q", vuelto.PtyStatus)
	}
	if vuelto.ExitReason != "" || vuelto.ExitCode != nil {
		t.Fatalf("quedaron rastros de la muerte anterior: %+v", vuelto)
	}

	kv, _ := st.ListKV(rec.ID)
	if kv["claude_session_id"] != "abc-123" {
		t.Fatalf("se perdió el KV: %v", kv)
	}

	att2, err := m.Attach(rec.ID)
	if err != nil {
		t.Fatalf("Attach después del restart: %v", err)
	}
	defer att2.Detach()
	if !att2.Live {
		t.Fatal("la sesión reanudada no está viva")
	}
	if !bytes.Contains(att2.History, []byte("antes-del-restart")) {
		t.Fatalf("el replay perdió lo anterior al restart: %q", tail(att2.History, 300))
	}
	if !bytes.Contains(att2.History, []byte("sesión reanudada")) {
		t.Fatalf("falta el marcador de reanudación: %q", tail(att2.History, 300))
	}

	// Y el proceso nuevo responde.
	_ = m.Write(rec.ID, []byte("echo despues-del-restart\n"))
	awaitChunk(t, att2.Output, "despues-del-restart")
}

func TestRestartSobreSesionViva(t *testing.T) {
	m, _ := newTestManager(t)
	rec, _ := m.Create(CreateOpts{Cwd: "/tmp", Cols: 80, Rows: 24})

	if _, err := m.Restart(rec.ID, 80, 24); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("se esperaba ErrAlreadyRunning, vino %v", err)
	}
}

func TestDeleteBorraTodo(t *testing.T) {
	m, st := newTestManager(t)
	rec, _ := m.Create(CreateOpts{Cwd: "/tmp", Cols: 80, Rows: 24})
	_ = st.SetKV(rec.ID, "k", "v")

	att, _ := m.Attach(rec.ID)
	_ = m.Write(rec.ID, []byte("echo hola\n"))
	awaitChunk(t, att.Output, "hola")
	att.Detach()

	if err := m.Delete(rec.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := st.GetSession(rec.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("la fila sigue ahí: %v", err)
	}
	hist, _ := st.ReadOutput(rec.ID)
	if len(hist) != 0 {
		t.Fatalf("quedó historial huérfano: %d bytes", len(hist))
	}
	kv, _ := st.ListKV(rec.ID)
	if len(kv) != 0 {
		t.Fatalf("quedó KV huérfano: %v", kv)
	}
}

// TestSweepMarcaHuerfanas cubre el caso que pediste explícitamente: la DB dice
// que la sesión está viva pero no hay proceso detrás.
func TestSweepMarcaHuerfanas(t *testing.T) {
	m, st := newTestManager(t)

	// Fila viva escrita a mano, sin pty: simula la desincronización.
	err := st.CreateSession(&store.Session{
		ID: "fantasma", Cwd: "/tmp", Shell: "/bin/bash", Cols: 80, Rows: 24,
		PtyStatus: store.StatusRunning,
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// Y una de verdad, que el sweep no tiene que tocar.
	viva, _ := m.Create(CreateOpts{Cwd: "/tmp", Cols: 80, Rows: 24})

	if n := m.Sweep(); n != 1 {
		t.Fatalf("el sweep corrigió %d filas, se esperaba 1", n)
	}

	got, _ := st.GetSession("fantasma")
	if got.PtyStatus != store.StatusExited || got.ExitReason != string(store.ReasonOrphaned) {
		t.Fatalf("la huérfana no se reconcilió: %+v", got)
	}
	sigue, _ := st.GetSession(viva.ID)
	if sigue.PtyStatus != store.StatusRunning {
		t.Fatalf("el sweep mató una sesión viva: %+v", sigue)
	}
	// Idempotente: en la segunda pasada ya no hay nada que corregir.
	if n := m.Sweep(); n != 0 {
		t.Fatalf("el segundo sweep corrigió %d filas", n)
	}
}

// TestCloseMataTodo: al apagar el backend no quedan procesos sueltos ni filas
// mintiendo. Lo que quede vivo lo levanta ReconcileBoot en el próximo arranque.
func TestCloseMataTodo(t *testing.T) {
	st := newTestStore(t)
	m := NewManager(st, Config{Shell: "/bin/bash", HistoryBytes: 1 << 20, SweepEvery: time.Hour})
	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	rec, _ := m.Create(CreateOpts{Cwd: "/tmp", Cols: 80, Rows: 24})

	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	got, _ := st.GetSession(rec.ID)
	if got.PtyStatus != store.StatusExited {
		t.Fatalf("pty_status = %q después de Close", got.PtyStatus)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close dos veces: %v", err)
	}
}
```

- [ ] **Step 2: Correr los tests y verificar que fallan**

Run: `go test ./internal/session/ -run 'TestKill|TestRestart|TestDelete|TestSweep|TestClose' -v`
Expected: FAIL, no compila (`undefined: ErrAlreadyRunning`, `m.Kill undefined`, …).

- [ ] **Step 3: Implementar el ABM y el sweep**

Agregar junto a `ErrNotLive` en `internal/session/manager.go`:

```go
// ErrAlreadyRunning lo devuelve Restart sobre una sesión que no murió.
var ErrAlreadyRunning = errors.New("la sesión ya está corriendo")
```

Agregar los métodos:

```go
// Kill mata el proceso y conserva la fila y el historial. Es sincrónico: al
// volver, la DB ya refleja la muerte, así que un GET inmediato no miente.
// Es idempotente sobre una sesión ya muerta, pero devuelve ErrNotFound si no
// existe.
func (m *Manager) Kill(id string) error {
	l := m.lookup(id)
	if l == nil {
		_, err := m.st.GetSession(id)
		return err
	}
	l.killed.Store(true)
	if err := l.pty.Kill(); err != nil {
		return err
	}
	select {
	case <-l.reaped:
		return nil
	case <-time.After(reapTimeout):
		return fmt.Errorf("la sesión %s no terminó a tiempo", id)
	}
}

// Restart spawnea un pty nuevo sobre la misma fila: conserva id, título, cwd,
// KV e historial, y sigue apendeando al mismo historial. Reusar la fila es lo
// que va a permitir en M6 reanudar con `claude --resume` usando el KV de la
// propia sesión.
func (m *Manager) Restart(id string, cols, rows int) (*store.Session, error) {
	rec, err := m.st.GetSession(id)
	if err != nil {
		return nil, err
	}
	if m.lookup(id) != nil {
		return nil, ErrAlreadyRunning
	}
	if cols <= 0 {
		cols = rec.Cols
	}
	if rows <= 0 {
		rows = rec.Rows
	}

	pt, err := terminal.New(id, terminal.Config{
		Shell: rec.Shell, Cwd: rec.Cwd, Rows: uint16(rows), Cols: uint16(cols),
	})
	if err != nil {
		code := -1
		_ = m.st.MarkExited(id, store.ReasonSpawnFailed, &code)
		return nil, fmt.Errorf("reanudando %s: %w", id, err)
	}

	banner := []byte("\r\n\x1b[90m— sesión reanudada —\x1b[0m\r\n")

	m.mu.Lock()
	if err := m.st.MarkRunning(id, cols, rows); err != nil {
		m.mu.Unlock()
		_ = pt.Close()
		return nil, err
	}
	m.startLive(rec, pt, banner)
	m.mu.Unlock()

	rec.PtyStatus = store.StatusRunning
	rec.Cols, rec.Rows = cols, rows
	rec.ExitReason, rec.ExitCode, rec.ExitedAt = "", nil, nil

	log.Printf("[%s] sesión reanudada (%dx%d)", id, cols, rows)
	return rec, nil
}

// Delete mata el proceso si vive y borra la fila con su KV y su historial.
// Es el acto destructivo explícito, separado de Kill a propósito.
func (m *Manager) Delete(id string) error {
	if l := m.lookup(id); l != nil {
		l.killed.Store(true)
		_ = l.pty.Kill()
		select {
		case <-l.reaped:
		case <-time.After(reapTimeout):
			log.Printf("[%s] no terminó a tiempo; se borra igual", id)
		}
	}
	return m.st.DeleteSession(id)
}

// Sweep marca como muertas las filas que la DB cree vivas pero que no tienen
// sesión asociada. Es una verificación de invariante, no el camino principal:
// en condiciones normales reap() siempre llega primero. Existe para que un bug
// del camino principal se autocorrija en vez de dejar la UI mintiendo.
// Devuelve cuántas filas corrigió.
func (m *Manager) Sweep() int {
	ids, err := m.st.RunningIDs()
	if err != nil {
		log.Printf("sweep: %v", err)
		return 0
	}
	n := 0
	for _, id := range ids {
		if m.lookup(id) != nil {
			continue
		}
		if err := m.st.MarkExited(id, store.ReasonOrphaned, nil); err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				log.Printf("sweep [%s]: %v", id, err)
			}
			continue
		}
		log.Printf("[%s] huérfana: la DB la daba por viva pero no hay proceso detrás", id)
		n++
	}
	return n
}
```

Reemplazar el `sweepLoop` provisorio de la tarea 8 por:

```go
func (m *Manager) sweepLoop() {
	defer m.wg.Done()
	ticker := time.NewTicker(m.cfg.SweepEvery)
	defer ticker.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-ticker.C:
			m.Sweep()
		}
	}
}
```

`Close` queda como está (la tarea 8 ya lo dejó completo).

- [ ] **Step 4: Correr los tests y verificar que pasan**

Run: `go test ./internal/session/ -v -race`
Expected: PASS en todos.

- [ ] **Step 5: Verificar el paquete entero y commitear**

Run: `go test ./internal/... -race`
Expected: PASS en `store`, `terminal` y `session`. (`server` todavía no cambió y sigue en verde.)

```bash
git add internal/session/
git commit -m "feat(session): kill, restart, delete y sweep de huérfanas

Kill y Delete quedan separados: matar conserva el historial, borrar es el
acto destructivo explícito. El sweep corrige filas que digan running sin
proceso detrás."
```

---

### Task 10: Server — API REST de sesiones

**Files:**
- Create: `internal/server/sessions.go`
- Create: `internal/server/sessions_test.go`
- Modify: `internal/server/server.go` (el `Server` recibe el manager; `Shell` sale de `Config`)
- Modify: `internal/server/auth_test.go` (adaptar las llamadas a `New`)

**Interfaces:**
- Consumes: `session.Manager` completo (tareas 8-9), `store.Session`, `store.MetaPatch`, `store.ErrNotFound`.
- Produces:
  - `New(cfg Config, mgr *session.Manager) *Server` — **cambia la firma**
  - `Config` sin el campo `Shell` (pasa a `session.Config`)
  - los handlers REST de la tabla de la spec

- [ ] **Step 1: Escribir los tests que fallan**

`internal/server/sessions_test.go`:

```go
package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giuliano/webterm/internal/session"
	"github.com/giuliano/webterm/internal/store"
)

// newTestServer levanta el stack completo: store en un tmpdir, manager y
// servidor HTTP. Sin mocks.
func newTestServer(t *testing.T) (*httptest.Server, *session.Manager) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "webterm.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	mgr := session.NewManager(st, session.Config{
		Shell: "/bin/bash", HistoryBytes: 1 << 20, SweepEvery: time.Hour,
	})
	if err := mgr.Start(); err != nil {
		t.Fatalf("manager.Start: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })

	srv := httptest.NewServer(New(Config{}, mgr).Handler())
	t.Cleanup(srv.Close)
	return srv, mgr
}

// do manda un request y devuelve status y body.
func do(t *testing.T, srv *httptest.Server, method, path, body string) (int, []byte) {
	t.Helper()
	var rdr *strings.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	} else {
		rdr = strings.NewReader("")
	}
	req, err := http.NewRequest(method, srv.URL+path, rdr)
	if err != nil {
		t.Fatalf("armando request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(res.Body)
	return res.StatusCode, buf.Bytes()
}

func decodeSession(t *testing.T, body []byte) *store.Session {
	t.Helper()
	var s store.Session
	if err := json.Unmarshal(body, &s); err != nil {
		t.Fatalf("decodificando sesión (%s): %v", body, err)
	}
	return &s
}

func createSession(t *testing.T, srv *httptest.Server) *store.Session {
	t.Helper()
	status, body := do(t, srv, "POST", "/api/sessions", `{"title":"test","cwd":"/tmp","cols":80,"rows":24}`)
	if status != http.StatusCreated {
		t.Fatalf("POST /api/sessions = %d: %s", status, body)
	}
	return decodeSession(t, body)
}

func TestCrearYListarSesiones(t *testing.T) {
	srv, _ := newTestServer(t)

	status, body := do(t, srv, "GET", "/api/sessions", "")
	if status != http.StatusOK {
		t.Fatalf("GET vacío = %d: %s", status, body)
	}
	if strings.TrimSpace(string(body)) != "[]" {
		t.Fatalf("sin sesiones tiene que dar [], dio %s", body)
	}

	rec := createSession(t, srv)
	if rec.ID == "" || rec.Title != "test" {
		t.Fatalf("sesión mal creada: %+v", rec)
	}
	if rec.PtyStatus != store.StatusRunning {
		t.Fatalf("pty_status = %q", rec.PtyStatus)
	}

	status, body = do(t, srv, "GET", "/api/sessions", "")
	var list []*store.Session
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("decodificando lista: %v", err)
	}
	if len(list) != 1 || list[0].ID != rec.ID {
		t.Fatalf("lista = %s", body)
	}
}

func TestGetSessionYNotFound(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := createSession(t, srv)

	status, body := do(t, srv, "GET", "/api/sessions/"+rec.ID, "")
	if status != http.StatusOK {
		t.Fatalf("GET = %d: %s", status, body)
	}

	status, _ = do(t, srv, "GET", "/api/sessions/no-existe", "")
	if status != http.StatusNotFound {
		t.Fatalf("GET inexistente = %d", status)
	}
}

func TestPatchSession(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := createSession(t, srv)

	status, body := do(t, srv, "PATCH", "/api/sessions/"+rec.ID, `{"title":"renombrada","kanban_status":"in_progress"}`)
	if status != http.StatusOK {
		t.Fatalf("PATCH = %d: %s", status, body)
	}
	got := decodeSession(t, body)
	if got.Title != "renombrada" || got.KanbanStatus != "in_progress" {
		t.Fatalf("no se aplicó el patch: %+v", got)
	}

	// Un estado fuera del enum tiene que rebotar: M7 va a construir el board
	// sobre este campo.
	status, _ = do(t, srv, "PATCH", "/api/sessions/"+rec.ID, `{"kanban_status":"inventado"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("kanban_status inválido = %d", status)
	}
	status, _ = do(t, srv, "PATCH", "/api/sessions/"+rec.ID, `{no es json`)
	if status != http.StatusBadRequest {
		t.Fatalf("body inválido = %d", status)
	}
}

// TestKillYRestart: matar conserva la fila; reanudar la reusa.
func TestKillYRestart(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := createSession(t, srv)

	status, body := do(t, srv, "POST", "/api/sessions/"+rec.ID+"/kill", "")
	if status != http.StatusOK {
		t.Fatalf("kill = %d: %s", status, body)
	}
	if got := decodeSession(t, body); got.PtyStatus != store.StatusExited {
		t.Fatalf("después del kill: %+v", got)
	}

	status, body = do(t, srv, "POST", "/api/sessions/"+rec.ID+"/restart", `{"cols":100,"rows":30}`)
	if status != http.StatusOK {
		t.Fatalf("restart = %d: %s", status, body)
	}
	vuelto := decodeSession(t, body)
	if vuelto.ID != rec.ID || vuelto.PtyStatus != store.StatusRunning {
		t.Fatalf("restart devolvió %+v", vuelto)
	}

	// Reanudar una sesión viva es un conflicto, no un error interno.
	status, _ = do(t, srv, "POST", "/api/sessions/"+rec.ID+"/restart", "")
	if status != http.StatusConflict {
		t.Fatalf("restart sobre sesión viva = %d", status)
	}
}

func TestDeleteSessionHTTP(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := createSession(t, srv)

	status, body := do(t, srv, "DELETE", "/api/sessions/"+rec.ID, "")
	if status != http.StatusNoContent {
		t.Fatalf("DELETE = %d: %s", status, body)
	}
	status, _ = do(t, srv, "GET", "/api/sessions/"+rec.ID, "")
	if status != http.StatusNotFound {
		t.Fatalf("la sesión sigue viva después del DELETE: %d", status)
	}
	status, _ = do(t, srv, "DELETE", "/api/sessions/"+rec.ID, "")
	if status != http.StatusNotFound {
		t.Fatalf("DELETE repetido = %d", status)
	}
}

// TestKVHTTP prueba la superficie que va a consumir el CLI webterm de M5.
func TestKVHTTP(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := createSession(t, srv)
	base := "/api/sessions/" + rec.ID + "/kv"

	status, body := do(t, srv, "GET", base, "")
	if status != http.StatusOK || strings.TrimSpace(string(body)) != "{}" {
		t.Fatalf("KV vacío = %d %s", status, body)
	}

	if status, body := do(t, srv, "PUT", base+"/claude_session_id", "abc-123"); status != http.StatusNoContent {
		t.Fatalf("PUT = %d: %s", status, body)
	}
	status, body = do(t, srv, "GET", base, "")
	var kv map[string]string
	if err := json.Unmarshal(body, &kv); err != nil {
		t.Fatalf("decodificando kv: %v", err)
	}
	if kv["claude_session_id"] != "abc-123" {
		t.Fatalf("kv = %s", body)
	}

	if status, _ := do(t, srv, "DELETE", base+"/claude_session_id", ""); status != http.StatusNoContent {
		t.Fatalf("DELETE kv = %d", status)
	}
	if status, _ := do(t, srv, "DELETE", base+"/claude_session_id", ""); status != http.StatusNotFound {
		t.Fatalf("DELETE kv repetido = %d", status)
	}
}
```

- [ ] **Step 2: Correr los tests y verificar que fallan**

Run: `go test ./internal/server/ -run 'TestCrear|TestGetSession|TestPatch|TestKill|TestDeleteSession|TestKV' -v`
Expected: FAIL, no compila (`New` recibe un argumento de más).

- [ ] **Step 3: Cambiar el `Server` para que reciba el manager**

En `internal/server/server.go`:

- Sacar el campo `Shell` de `Config` (ahora vive en `session.Config`) y el
  campo `nextID` del `Server` (los ids los genera el store).
- Agregar el campo `mgr *session.Manager` al `Server` y cambiar la firma:

```go
// New construye el servidor sobre un manager de sesiones ya arrancado.
func New(cfg Config, mgr *session.Manager) *Server {
	return &Server{
		cfg: cfg,
		mgr: mgr,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  4 * 1024,
			WriteBufferSize: 32 * 1024,
			CheckOrigin:     sameOrigin,
		},
	}
}
```

- Registrar las rutas nuevas en `Handler()` (Go 1.22+ admite método y
  wildcards en los patrones del `ServeMux`):

```go
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", s.handleHealth)
	mux.HandleFunc(loginPath, s.handleLogin)
	mux.HandleFunc("/api/logout", s.handleLogout)

	mux.HandleFunc("GET /api/sessions", s.handleListSessions)
	mux.HandleFunc("POST /api/sessions", s.handleCreateSession)
	mux.HandleFunc("GET /api/sessions/{id}", s.handleGetSession)
	mux.HandleFunc("PATCH /api/sessions/{id}", s.handlePatchSession)
	mux.HandleFunc("DELETE /api/sessions/{id}", s.handleDeleteSession)
	mux.HandleFunc("POST /api/sessions/{id}/kill", s.handleKillSession)
	mux.HandleFunc("POST /api/sessions/{id}/restart", s.handleRestartSession)
	mux.HandleFunc("GET /api/sessions/{id}/kv", s.handleListKV)
	mux.HandleFunc("PUT /api/sessions/{id}/kv/{key}", s.handleSetKV)
	mux.HandleFunc("DELETE /api/sessions/{id}/kv/{key}", s.handleDeleteKV)

	mux.HandleFunc("/ws/terminal", s.handleTerminal)
	mux.Handle("/", s.staticHandler())
	return s.withAuth(mux)
}
```

- En `auth_test.go`, reemplazar cada `New(Config{...})` por
  `New(Config{...}, nil)`: esos tests solo tocan `/api/health`, `/login` y
  rutas estáticas, que no usan el manager.

- [ ] **Step 4: Escribir los handlers**

`internal/server/sessions.go`:

```go
package server

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"

	"github.com/giuliano/webterm/internal/session"
	"github.com/giuliano/webterm/internal/store"
)

// maxBodyBytes acota lo que leemos de un request: nadie manda un título de
// más de unos KB, y sin el límite un cliente podría inflarnos la memoria.
const maxBodyBytes = 64 << 10

// Enums que la API valida. M7 construye el dashboard sobre estos campos, así
// que conviene que la base no acumule valores inventados.
var (
	workStatuses   = map[string]bool{"idle": true, "working": true, "waiting_input": true, "error": true}
	kanbanStatuses = map[string]bool{"todo": true, "in_progress": true, "done": true}
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("escribiendo respuesta: %v", err)
	}
}

func writeErrorMsg(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// writeError traduce los errores del manager al status que corresponde.
func writeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErrorMsg(w, http.StatusNotFound, "no existe")
	case errors.Is(err, session.ErrAlreadyRunning):
		writeErrorMsg(w, http.StatusConflict, "la sesión ya está corriendo")
	case errors.Is(err, session.ErrNotLive):
		writeErrorMsg(w, http.StatusConflict, "la sesión no está corriendo")
	default:
		writeErrorMsg(w, http.StatusInternalServerError, err.Error())
	}
}

// decodeBody lee un JSON opcional: un body vacío deja el struct con sus ceros.
func decodeBody(r *http.Request, v any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		return err
	}
	if len(body) == 0 {
		return nil
	}
	return json.Unmarshal(body, v)
}

type sizeReq struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

type createSessionReq struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Cwd         string `json:"cwd"`
	Cols        int    `json:"cols"`
	Rows        int    `json:"rows"`
}

func (s *Server) handleListSessions(w http.ResponseWriter, _ *http.Request) {
	list, err := s.mgr.List()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req createSessionReq
	if err := decodeBody(r, &req); err != nil {
		writeErrorMsg(w, http.StatusBadRequest, "body inválido")
		return
	}
	rec, err := s.mgr.Create(session.CreateOpts{
		Title: req.Title, Description: req.Description, Cwd: req.Cwd,
		Cols: req.Cols, Rows: req.Rows,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, rec)
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	rec, err := s.mgr.Get(r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func (s *Server) handlePatchSession(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Title        *string `json:"title"`
		Description  *string `json:"description"`
		WorkStatus   *string `json:"work_status"`
		KanbanStatus *string `json:"kanban_status"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeErrorMsg(w, http.StatusBadRequest, "body inválido")
		return
	}
	if req.WorkStatus != nil && !workStatuses[*req.WorkStatus] {
		writeErrorMsg(w, http.StatusBadRequest, "work_status inválido")
		return
	}
	if req.KanbanStatus != nil && !kanbanStatuses[*req.KanbanStatus] {
		writeErrorMsg(w, http.StatusBadRequest, "kanban_status inválido")
		return
	}
	rec, err := s.mgr.UpdateMeta(r.PathValue("id"), store.MetaPatch{
		Title: req.Title, Description: req.Description,
		WorkStatus: req.WorkStatus, KanbanStatus: req.KanbanStatus,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

// handleKillSession mata el proceso y conserva el historial.
func (s *Server) handleKillSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.mgr.Kill(id); err != nil {
		writeError(w, err)
		return
	}
	rec, err := s.mgr.Get(id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func (s *Server) handleRestartSession(w http.ResponseWriter, r *http.Request) {
	var req sizeReq
	if err := decodeBody(r, &req); err != nil {
		writeErrorMsg(w, http.StatusBadRequest, "body inválido")
		return
	}
	rec, err := s.mgr.Restart(r.PathValue("id"), req.Cols, req.Rows)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

// handleDeleteSession borra la fila con su KV y su historial. Es el acto
// destructivo, separado del kill a propósito.
func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	if err := s.mgr.Delete(r.PathValue("id")); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListKV(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.mgr.Get(id); err != nil {
		writeError(w, err)
		return
	}
	kv, err := s.mgr.ListKV(id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, kv)
}

// handleSetKV toma el valor crudo del body: es más cómodo para el CLI de M5
// que envolverlo en un JSON.
func (s *Server) handleSetKV(w http.ResponseWriter, r *http.Request) {
	value, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		writeErrorMsg(w, http.StatusBadRequest, "no se pudo leer el valor")
		return
	}
	if err := s.mgr.SetKV(r.PathValue("id"), r.PathValue("key"), string(value)); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDeleteKV(w http.ResponseWriter, r *http.Request) {
	if err := s.mgr.DeleteKV(r.PathValue("id"), r.PathValue("key")); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
```

- [ ] **Step 5: Exponer el KV en el manager**

El servidor no toca el store directo, así que agregar en
`internal/session/manager.go`:

```go
// ListKV, SetKV y DeleteKV son el contexto persistido de la sesión. El
// servidor no habla con el store directamente: todo pasa por acá.
func (m *Manager) ListKV(id string) (map[string]string, error) { return m.st.ListKV(id) }

func (m *Manager) SetKV(id, key, value string) error {
	if _, err := m.st.GetSession(id); err != nil {
		return err
	}
	return m.st.SetKV(id, key, value)
}

func (m *Manager) DeleteKV(id, key string) error { return m.st.DeleteKV(id, key) }
```

- [ ] **Step 6: Correr los tests y verificar que pasan**

Run: `go test ./internal/server/ -run 'TestCrear|TestGetSession|TestPatch|TestKill|TestDeleteSession|TestKV|TestAuth|TestLogin' -v`
Expected: PASS en los nuevos y en los de auth. Los tres tests viejos de
`server_test.go` todavía fallan: los adapta la tarea 11.

- [ ] **Step 7: Commit**

```bash
git add internal/server/ internal/session/
git commit -m "feat(server): API REST de ABM de sesiones

Kill y DELETE quedan como endpoints distintos: matar el proceso no tiene
por qué llevarse el historial."
```

---

### Task 11: Server — WebSocket attacheado a una sesión

**Files:**
- Create: `internal/server/terminal.go` (se lleva `handleTerminal` y `wsClient` desde `server.go`)
- Modify: `internal/server/server.go` (sacar lo que se movió y `queryInt`)
- Modify: `internal/server/server_test.go` (adaptar los tres tests de M1 y agregar el de persistencia)

**Interfaces:**
- Consumes: `(*Manager).Attach/Write/Resize/Get`, `Attachment` (tarea 8).
- Produces: el protocolo WebSocket de la spec sobre `/ws/terminal?session_id=…`.

- [ ] **Step 1: Reescribir los tests de `server_test.go`**

Reemplazar el contenido completo de `internal/server/server_test.go` por:

```go
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/giuliano/webterm/internal/store"
)

// attach abre un WebSocket contra una sesión existente.
func attach(t *testing.T, srv *httptest.Server, id string) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/terminal?session_id=" + id
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// awaitOutput acumula output del pty hasta encontrar want.
func awaitOutput(t *testing.T, conn *websocket.Conn, want string) string {
	t.Helper()
	var sb strings.Builder
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		_ = conn.SetReadDeadline(deadline)
		typ, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("leyendo output (acumulado %q): %v", sb.String(), err)
		}
		if typ != websocket.BinaryMessage {
			continue
		}
		sb.Write(data)
		if strings.Contains(sb.String(), want) {
			return sb.String()
		}
	}
	t.Fatalf("timeout esperando %q, acumulado: %q", want, sb.String())
	return ""
}

// awaitControl espera un mensaje de control con el type pedido.
func awaitControl(t *testing.T, conn *websocket.Conn, want string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		_ = conn.SetReadDeadline(deadline)
		typ, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("esperando %q: %v", want, err)
		}
		if typ != websocket.TextMessage {
			continue
		}
		var msg map[string]any
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		if msg["type"] == want {
			return msg
		}
	}
	t.Fatalf("timeout esperando el mensaje %q", want)
	return nil
}

// TestAttachMandaAttachedYReady: el handshake del protocolo, en orden.
func TestAttachMandaAttachedYReady(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := createSession(t, srv)
	conn := attach(t, srv, rec.ID)

	msg := awaitControl(t, conn, "attached")
	sess, ok := msg["session"].(map[string]any)
	if !ok || sess["id"] != rec.ID {
		t.Fatalf("attached sin la sesión correcta: %v", msg)
	}
	awaitControl(t, conn, "ready")
}

// TestTerminalEcho valida el pipeline completo: input del browser -> pty ->
// output de vuelta al browser.
func TestTerminalEcho(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := createSession(t, srv)
	conn := attach(t, srv, rec.ID)

	if err := conn.WriteMessage(websocket.BinaryMessage, []byte("echo hola-webterm-ok\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	awaitOutput(t, conn, "hola-webterm-ok")
}

// TestTerminalResize valida que el mensaje de control llegue al pty: el shell
// tiene que ver el tamaño nuevo.
func TestTerminalResize(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := createSession(t, srv)
	conn := attach(t, srv, rec.ID)

	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"resize","cols":123,"rows":45}`)); err != nil {
		t.Fatalf("resize: %v", err)
	}
	if err := conn.WriteMessage(websocket.BinaryMessage, []byte("stty size\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	awaitOutput(t, conn, "45 123")
}

// TestTerminalExit valida que al terminar el shell el backend avise al browser.
func TestTerminalExit(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := createSession(t, srv)
	conn := attach(t, srv, rec.ID)

	if err := conn.WriteMessage(websocket.BinaryMessage, []byte("exit 3\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	msg := awaitControl(t, conn, "exit")
	if msg["reason"] != string(store.ReasonNormal) {
		t.Fatalf("reason = %v", msg["reason"])
	}
	if code, _ := msg["code"].(float64); int(code) != 3 {
		t.Fatalf("code = %v", msg["code"])
	}
}

// TestSesionSobreviveAlCierreDelSocket es la premisa de M2 de punta a punta:
// cerrar la pestaña no mata el proceso y al volver está todo el historial.
func TestSesionSobreviveAlCierreDelSocket(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := createSession(t, srv)

	conn := attach(t, srv, rec.ID)
	if err := conn.WriteMessage(websocket.BinaryMessage, []byte("echo antes-de-cerrar\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	awaitOutput(t, conn, "antes-de-cerrar")

	// Se "cierra la pestaña".
	_ = conn.Close()
	time.Sleep(300 * time.Millisecond)

	// La sesión sigue viva en la API.
	status, body := do(t, srv, "GET", "/api/sessions/"+rec.ID, "")
	if status != http.StatusOK {
		t.Fatalf("GET = %d: %s", status, body)
	}
	if got := decodeSession(t, body); got.PtyStatus != store.StatusRunning {
		t.Fatalf("la sesión murió al cerrar el socket: %+v", got)
	}

	// Y al reattachear llega el replay con lo de antes.
	conn2 := attach(t, srv, rec.ID)
	awaitControl(t, conn2, "attached")
	awaitOutput(t, conn2, "antes-de-cerrar")

	// El proceso es el mismo y sigue respondiendo.
	if err := conn2.WriteMessage(websocket.BinaryMessage, []byte("echo despues-de-volver\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	awaitOutput(t, conn2, "despues-de-volver")
}

// TestAttachASesionMuertaEsSoloLectura: se puede ver el historial de una
// sesión terminada con el mismo código de la UI.
func TestAttachASesionMuertaEsSoloLectura(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := createSession(t, srv)

	conn := attach(t, srv, rec.ID)
	_ = conn.WriteMessage(websocket.BinaryMessage, []byte("echo quedo-en-la-historia\n"))
	awaitOutput(t, conn, "quedo-en-la-historia")
	_ = conn.Close()

	if status, body := do(t, srv, "POST", "/api/sessions/"+rec.ID+"/kill", ""); status != http.StatusOK {
		t.Fatalf("kill = %d: %s", status, body)
	}

	conn2 := attach(t, srv, rec.ID)
	awaitControl(t, conn2, "attached")
	awaitOutput(t, conn2, "quedo-en-la-historia")
	msg := awaitControl(t, conn2, "exit")
	if msg["reason"] != string(store.ReasonKilled) {
		t.Fatalf("reason = %v", msg["reason"])
	}
}

// TestWSSinSessionID: crear sesiones es tarea del endpoint REST; el upgrade no
// puede ser una puerta trasera para spawnear shells.
func TestWSSinSessionID(t *testing.T) {
	srv, _ := newTestServer(t)
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/terminal"
	_, res, err := websocket.DefaultDialer.Dial(url, nil)
	if err == nil {
		t.Fatal("se esperaba un error")
	}
	if res == nil || res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %v", res)
	}
}

func TestWSSessionInexistente(t *testing.T) {
	srv, _ := newTestServer(t)
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/terminal?session_id=no-existe"
	_, res, err := websocket.DefaultDialer.Dial(url, nil)
	if err == nil {
		t.Fatal("se esperaba un error")
	}
	if res == nil || res.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %v", res)
	}
}
```

- [ ] **Step 2: Correr los tests y verificar que fallan**

Run: `go test ./internal/server/ -run 'TestAttach|TestTerminal|TestSesion|TestWS' -v`
Expected: FAIL — el handler viejo spawnea un pty por conexión e ignora
`session_id`, así que no manda `attached` ni `ready` y no devuelve 400/404.

- [ ] **Step 3: Mover el WebSocket a su archivo y reescribirlo**

Borrar de `internal/server/server.go`: `clientMsg`, `handleTerminal`,
`wsClient` con sus métodos, `queryInt`, el bloque de constantes
(`readBufSize`, `pingInterval`, `pongTimeout`, `writeTimeout` — se van a
`terminal.go`) y los imports que queden sin uso (`internal/terminal`,
`encoding/json`, `errors`, `sync`, `strconv`).

`internal/server/terminal.go`:

```go
package server

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/giuliano/webterm/internal/store"
)

const (
	// Cada cuánto mandamos ping para detectar clientes muertos.
	pingInterval = 30 * time.Second
	// Cuánto esperamos un pong antes de dar la conexión por perdida.
	pongTimeout = 60 * time.Second
	// Timeout de escritura sobre el socket.
	writeTimeout = 10 * time.Second
)

// clientMsg es un mensaje de control del browser (los de texto; el input
// crudo del usuario viaja como mensaje binario).
type clientMsg struct {
	Type string `json:"type"`
	Rows uint16 `json:"rows"`
	Cols uint16 `json:"cols"`
}

type attachedMsg struct {
	Type    string         `json:"type"`
	Session *store.Session `json:"session"`
}

type exitMsg struct {
	Type   string `json:"type"`
	Code   int    `json:"code"`
	Reason string `json:"reason"`
}

// handleTerminal attachea el WebSocket a una sesión ya existente.
//
// Crear sesiones es tarea del endpoint REST: si el upgrade pudiera spawnear
// una, el socket sería una puerta trasera para abrir shells sin pasar por el
// ABM.
func (s *Server) handleTerminal(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("session_id")
	if id == "" {
		http.Error(w, "falta session_id", http.StatusBadRequest)
		return
	}

	// Attacheamos antes del upgrade para poder responder con un status HTTP
	// claro en vez de abrir el socket y cerrarlo enseguida.
	att, err := s.mgr.Attach(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "la sesión no existe", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		att.Detach()
		log.Printf("[%s] upgrade falló: %v", id, err)
		return
	}
	defer func() {
		att.Detach()
		_ = conn.Close()
	}()

	c := &wsClient{conn: conn}
	done := make(chan struct{})
	defer close(done)
	go c.keepalive(done)

	// Handshake: metadata, replay, y recién ahí el stream vivo.
	if err := c.writeJSON(attachedMsg{Type: "attached", Session: att.Session}); err != nil {
		return
	}
	if len(att.History) > 0 {
		if err := c.write(websocket.BinaryMessage, att.History); err != nil {
			return
		}
	}
	if err := c.writeJSON(map[string]string{"type": "ready"}); err != nil {
		return
	}

	if !att.Live {
		// Sesión muerta: se ve el historial y nada más. El input se descarta.
		_ = c.writeJSON(exitMsg{
			Type: "exit", Code: derefInt(att.Session.ExitCode), Reason: att.Session.ExitReason,
		})
		drainUntilClose(conn)
		return
	}

	// pty -> browser
	go func() {
		for chunk := range att.Output {
			if err := c.write(websocket.BinaryMessage, chunk); err != nil {
				break
			}
		}
		s.closeAfterStream(c, att, id)
		// Cerrar el socket destraba el ReadMessage del loop de abajo.
		_ = conn.Close()
	}()

	// browser -> pty
	_ = conn.SetReadDeadline(time.Now().Add(pongTimeout))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(pongTimeout))
	})

	for {
		typ, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if typ == websocket.TextMessage && len(data) > 0 && data[0] == '{' {
			var msg clientMsg
			if err := json.Unmarshal(data, &msg); err == nil && msg.Type == "resize" {
				if err := s.mgr.Resize(id, msg.Rows, msg.Cols); err != nil {
					log.Printf("[%s] resize falló: %v", id, err)
				}
				continue
			}
		}
		if err := s.mgr.Write(id, data); err != nil {
			log.Printf("[%s] write al pty falló: %v", id, err)
		}
	}
}

// closeAfterStream explica por qué se cortó el stream: o al cliente lo
// expulsamos por lento, o la sesión terminó.
func (s *Server) closeAfterStream(c *wsClient, att *sessionAttachment, id string) {
	if att.Dropped() {
		_ = c.writeJSON(map[string]string{
			"type": "error", "error": "cliente demasiado lento; volvé a conectar",
		})
		return
	}
	rec, err := s.mgr.Get(id)
	if err != nil {
		return
	}
	if rec.PtyStatus == store.StatusExited {
		_ = c.writeJSON(exitMsg{Type: "exit", Code: derefInt(rec.ExitCode), Reason: rec.ExitReason})
	}
}

// drainUntilClose descarta lo que mande el cliente hasta que corte. Lo usamos
// en el modo de solo lectura, donde igual hay que leer del socket para que
// lleguen los pongs y se detecte el cierre.
func drainUntilClose(conn *websocket.Conn) {
	_ = conn.SetReadDeadline(time.Now().Add(pongTimeout))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(pongTimeout))
	})
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
	}
}

func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// wsClient serializa las escrituras al socket: gorilla no admite writers
// concurrentes y acá escriben el lector de la sesión y el keepalive.
type wsClient struct {
	mu   sync.Mutex
	conn *websocket.Conn
}

func (c *wsClient) write(msgType int, data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	return c.conn.WriteMessage(msgType, data)
}

func (c *wsClient) writeJSON(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.write(websocket.TextMessage, data)
}

func (c *wsClient) keepalive(done <-chan struct{}) {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			if err := c.write(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}
```

**Nota sobre el tipo:** `sessionAttachment` es el alias local de
`*session.Attachment`. Agregar arriba del archivo, junto a los imports:

```go
// sessionAttachment es el attachment del manager. El alias evita arrastrar el
// nombre del paquete por todo el handler.
type sessionAttachment = session.Attachment
```

y el import `"github.com/giuliano/webterm/internal/session"`.

- [ ] **Step 4: Correr los tests y verificar que pasan**

Run: `go test ./internal/server/ -v -race`
Expected: PASS en todo, incluido `TestSesionSobreviveAlCierreDelSocket`.

- [ ] **Step 5: Verificar el backend completo y commitear**

Run: `go vet ./... && go test ./... -race`
Expected: PASS. `cmd/webterm` todavía no compila contra la firma nueva de
`New` — lo arregla la tarea 12, así que si `go build ./...` falla ahí, seguir.

```bash
git add internal/server/
git commit -m "feat(server): el WebSocket se attachea a una sesión existente

Crear sesiones queda solo en el endpoint REST: si el upgrade pudiera
spawnear una, sería una puerta trasera para abrir shells sin ABM."
```

---

### Task 12: Entrypoint — flags, wiring y apagado ordenado

**Files:**
- Modify: `cmd/webterm/main.go`
- Modify: `internal/server/server.go` (el log de arranque ya no depende de `Shell`)
- Create: `internal/server/health_test.go`

**Interfaces:**
- Consumes: `store.Open`, `session.NewManager`/`Start`/`Close`, `server.New` (tareas 1-11).
- Produces: flags `-db` y `-history-bytes`; `/api/health` devuelve también la cantidad de sesiones vivas.

- [ ] **Step 1: Escribir el test que falla**

`internal/server/health_test.go`:

```go
package server

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestHealthCuentaSesiones: el health sirve para chequear de un vistazo que el
// backend ve las sesiones que dice tener.
func TestHealthCuentaSesiones(t *testing.T) {
	srv, _ := newTestServer(t)

	status, body := do(t, srv, "GET", "/api/health", "")
	if status != http.StatusOK {
		t.Fatalf("health = %d: %s", status, body)
	}
	var h struct {
		Status   string `json:"status"`
		Auth     bool   `json:"auth"`
		Sessions int    `json:"sessions"`
	}
	if err := json.Unmarshal(body, &h); err != nil {
		t.Fatalf("decodificando health: %v", err)
	}
	if h.Status != "ok" || h.Sessions != 0 {
		t.Fatalf("health = %s", body)
	}

	createSession(t, srv)
	_, body = do(t, srv, "GET", "/api/health", "")
	_ = json.Unmarshal(body, &h)
	if h.Sessions != 1 {
		t.Fatalf("sessions = %d: %s", h.Sessions, body)
	}
}
```

- [ ] **Step 2: Correr el test y verificar que falla**

Run: `go test ./internal/server/ -run TestHealthCuenta -v`
Expected: FAIL — `sessions` no está en la respuesta (`h.Sessions != 1`).

- [ ] **Step 3: Actualizar el health y el manager**

En `internal/session/manager.go`:

```go
// LiveCount es cuántas sesiones tienen proceso corriendo ahora mismo.
func (m *Manager) LiveCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.live)
}
```

En `internal/server/server.go`, reemplazar `handleHealth` por:

```go
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	live := 0
	if s.mgr != nil {
		live = s.mgr.LiveCount()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":   "ok",
		"auth":     s.cfg.Token != "",
		"sessions": live,
	})
}
```

(Se puede borrar el import de `io` de `server.go` si ya no lo usa nada.)

- [ ] **Step 4: Reescribir `cmd/webterm/main.go`**

```go
// Command webterm levanta el backend de WebTerm: sirve la UI estática,
// persiste las sesiones en SQLite y las expone por HTTP y WebSocket.
package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/giuliano/webterm/internal/server"
	"github.com/giuliano/webterm/internal/session"
	"github.com/giuliano/webterm/internal/store"
)

func main() {
	var cfg server.Config
	var sess session.Config
	var dbPath string
	var historyBytes int64
	var noAuth bool

	flag.StringVar(&cfg.Addr, "addr", "127.0.0.1:7788", "dirección de escucha (0.0.0.0:7788 para exponerlo a la red local)")
	flag.StringVar(&cfg.StaticDir, "static", "web/dist", "carpeta con el build del frontend")
	flag.StringVar(&sess.Shell, "shell", "", "shell a spawnear (default: $SHELL)")
	flag.StringVar(&cfg.Token, "token", "", "token de acceso (default: se genera uno si no escucha solo en loopback)")
	flag.StringVar(&dbPath, "db", defaultDBPath(), "base SQLite con el estado de las sesiones")
	flag.Int64Var(&historyBytes, "history-bytes", session.DefaultHistoryBytes, "cuánto output se guarda por sesión")
	flag.BoolVar(&noAuth, "no-auth", false, "no pedir token aunque escuche en la red (peligroso)")
	flag.Parse()

	// El env var evita que el token quede en el historial del shell y en ps.
	if cfg.Token == "" {
		cfg.Token = os.Getenv("WEBTERM_TOKEN")
	}
	// Escuchar fuera de loopback expone una shell a toda la red, así que si no
	// nos dieron token generamos uno en vez de abrirlo pelado.
	if cfg.Token == "" && !noAuth && !server.IsLoopback(cfg.Addr) {
		cfg.Token = server.NewToken()
	}
	if noAuth {
		cfg.Token = ""
	}

	st, err := store.Open(dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()
	log.Printf("estado en %s", dbPath)

	sess.HistoryBytes = historyBytes
	mgr := session.NewManager(st, sess)
	// Start reconcilia lo que quedó vivo de la ejecución anterior. Va antes de
	// escuchar: si no, hay una ventana en la que la API miente.
	if err := mgr.Start(); err != nil {
		log.Fatal(err)
	}

	// Al apagar, matamos los ptys y esperamos el último flush del historial.
	// Lo que llegue a quedar marcado como vivo lo corrige ReconcileBoot en el
	// próximo arranque.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigs
		log.Print("apagando: cerrando las sesiones vivas")
		_ = mgr.Close()
		_ = st.Close()
		os.Exit(0)
	}()

	if err := server.New(cfg, mgr).ListenAndServe(); err != nil {
		_ = mgr.Close()
		log.Fatal(err)
	}
}

// defaultDBPath deja la base en ~/.webterm/webterm.db, fuera del repo.
func defaultDBPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "webterm.db"
	}
	return filepath.Join(home, ".webterm", "webterm.db")
}
```

- [ ] **Step 5: Verificar todo el backend**

Run: `go vet ./... && go build ./... && go test ./... -race`
Expected: PASS y binario que compila.

- [ ] **Step 6: Probar a mano el ciclo completo**

```bash
go run ./cmd/webterm -db /tmp/webterm-prueba.db -addr 127.0.0.1:7799 &
sleep 2
ID=$(curl -sX POST localhost:7799/api/sessions -d '{"title":"prueba","cols":80,"rows":24}' | python3 -c 'import sys,json; print(json.load(sys.stdin)["id"])')
curl -s localhost:7799/api/sessions | python3 -m json.tool
curl -sX POST localhost:7799/api/sessions/$ID/kill | python3 -m json.tool   # pty_status: exited
kill %1; sleep 1
# Al levantar de nuevo, la reconciliación deja todo consistente:
go run ./cmd/webterm -db /tmp/webterm-prueba.db -addr 127.0.0.1:7799 &
sleep 2
curl -s localhost:7799/api/sessions | python3 -m json.tool
kill %1
rm -f /tmp/webterm-prueba.db*
```
Expected: la sesión sigue listada después del reinicio, con `pty_status`
`exited`, y su historial intacto.

- [ ] **Step 7: Commit**

```bash
git add cmd/ internal/
git commit -m "feat(cmd): wiring del store y el manager, con apagado ordenado

La reconciliación de arranque corre antes de escuchar: si no, hay una
ventana en la que la API reporta vivas sesiones que ya no existen."
```

---

### Task 13: Frontend — sidebar de sesiones

**Files:**
- Create: `web/src/api.ts`
- Create: `web/src/SessionList.tsx`
- Modify: `web/src/TerminalView.tsx`
- Modify: `web/src/App.tsx`
- Modify: `web/src/index.css`

**Interfaces:**
- Consumes: la API REST (tarea 10) y el protocolo WebSocket (tarea 11).
- Produces: `api` y el tipo `Session` en `api.ts`; `<SessionList>`;
  `<TerminalView sessionId onState>` con `ConnState` extendido a
  `'connecting' | 'open' | 'readonly' | 'closed' | 'exited'`.

- [ ] **Step 1: Cliente de la API**

`web/src/api.ts`:

```ts
export type PtyStatus = 'running' | 'exited'

export interface Session {
  id: string
  title: string
  description: string
  folder_id: string | null
  cwd: string
  shell: string
  cols: number
  rows: number
  pty_status: PtyStatus
  exit_reason?: string
  exit_code?: number
  work_status: string
  kanban_status: string
  created_at: number
  last_active_at: number
  exited_at?: number
}

async function req<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, {
    headers: { 'Content-Type': 'application/json' },
    ...init,
  })
  if (!res.ok) {
    // El backend manda {"error":"..."}; si no, nos quedamos con el status.
    let msg = `${res.status} ${res.statusText}`
    try {
      const body = (await res.json()) as { error?: string }
      if (body.error) msg = body.error
    } catch {
      /* respuesta sin JSON */
    }
    throw new Error(msg)
  }
  if (res.status === 204) return undefined as T
  return (await res.json()) as T
}

export const api = {
  list: () => req<Session[]>('/api/sessions'),
  create: (body: { title?: string; cwd?: string; cols: number; rows: number }) =>
    req<Session>('/api/sessions', { method: 'POST', body: JSON.stringify(body) }),
  rename: (id: string, title: string) =>
    req<Session>(`/api/sessions/${id}`, { method: 'PATCH', body: JSON.stringify({ title }) }),
  kill: (id: string) => req<Session>(`/api/sessions/${id}/kill`, { method: 'POST' }),
  restart: (id: string, cols: number, rows: number) =>
    req<Session>(`/api/sessions/${id}/restart`, {
      method: 'POST',
      body: JSON.stringify({ cols, rows }),
    }),
  remove: (id: string) => req<void>(`/api/sessions/${id}`, { method: 'DELETE' }),
  health: () => req<{ status: string; auth: boolean; sessions: number }>('/api/health'),
}
```

- [ ] **Step 2: Adaptar `TerminalView` a una sesión existente**

En `web/src/TerminalView.tsx`:

- Extender el tipo de estado y los mensajes del servidor:

```ts
export type ConnState = 'connecting' | 'open' | 'readonly' | 'closed' | 'exited'

// Protocolo con el backend:
//   browser -> server : binario = input crudo | texto JSON = control (resize)
//   server -> browser : binario = replay y output vivo | texto JSON = eventos
type ServerMsg =
  | { type: 'attached'; session: { pty_status: string } }
  | { type: 'ready' }
  | { type: 'exit'; code: number; reason: string }
  | { type: 'error'; error: string }
```

- Cambiar la firma del componente y la URL del socket:

```tsx
export function TerminalView({
  sessionId,
  onState,
}: {
  sessionId: string
  onState: (s: ConnState) => void
}) {
```

```ts
    const proto = location.protocol === 'https:' ? 'wss:' : 'ws:'
    const ws = new WebSocket(
      `${proto}//${location.host}/ws/terminal?session_id=${encodeURIComponent(sessionId)}`,
    )
```

- Reemplazar el `ws.onmessage` por el manejo del protocolo nuevo:

```ts
    let live = true

    ws.onmessage = (ev) => {
      if (typeof ev.data !== 'string') {
        term.write(new Uint8Array(ev.data as ArrayBuffer))
        return
      }
      let msg: ServerMsg
      try {
        msg = JSON.parse(ev.data) as ServerMsg
      } catch {
        return
      }
      switch (msg.type) {
        case 'attached':
          // Una sesión ya terminada se attachea igual: llega el historial y
          // nada más, así que la mostramos de solo lectura.
          live = msg.session.pty_status === 'running'
          break
        case 'ready':
          onStateRef.current(live ? 'open' : 'readonly')
          if (live) {
            fit.fit()
            sendResize()
            term.focus()
          }
          break
        case 'exit':
          exited = true
          onStateRef.current('exited')
          term.write(
            `\r\n\x1b[90m— el proceso terminó (${msg.reason}, código ${msg.code}) —\x1b[0m\r\n`,
          )
          break
        case 'error':
          term.write(`\r\n\x1b[31m— ${msg.error} —\x1b[0m\r\n`)
          break
      }
    }
```

- El `ws.onopen` ya no manda nada: el resize sale recién en `ready`, cuando
  sabemos que la sesión está viva. Dejarlo solo con
  `onStateRef.current('connecting')`, y mover el `fit.fit()` al caso `ready`.

- Que el input no viaje si la sesión es de solo lectura:

```ts
    const dataSub = term.onData((data) => {
      if (live && ws.readyState === WebSocket.OPEN) ws.send(encoder.encode(data))
    })
```

- Agregar `sessionId` a las dependencias del `useEffect`.

- [ ] **Step 3: Escribir el sidebar**

`web/src/SessionList.tsx`:

```tsx
import { useState } from 'react'
import type { Session } from './api'

function label(s: Session): string {
  return s.title.trim() || s.cwd.split('/').filter(Boolean).pop() || s.id.slice(-6)
}

// relative formatea el "hace cuánto" de la última actividad, que es lo que
// más rápido dice cuál de todas las sesiones importa ahora.
function relative(ms: number): string {
  const secs = Math.max(0, Math.round((Date.now() - ms) / 1000))
  if (secs < 60) return 'recién'
  if (secs < 3600) return `hace ${Math.floor(secs / 60)} min`
  if (secs < 86400) return `hace ${Math.floor(secs / 3600)} h`
  return `hace ${Math.floor(secs / 86400)} d`
}

export function SessionList({
  sessions,
  selectedId,
  busy,
  onSelect,
  onCreate,
  onRename,
  onKill,
  onRestart,
  onDelete,
}: {
  sessions: Session[]
  selectedId: string | null
  busy: boolean
  onSelect: (id: string) => void
  onCreate: () => void
  onRename: (id: string, title: string) => void
  onKill: (id: string) => void
  onRestart: (id: string) => void
  onDelete: (id: string) => void
}) {
  const [editing, setEditing] = useState<string | null>(null)
  const [draft, setDraft] = useState('')

  const commit = (id: string) => {
    setEditing(null)
    const title = draft.trim()
    if (title) onRename(id, title)
  }

  return (
    <aside className="sidebar">
      <div className="sidebar-head">
        <span>Sesiones</span>
        <button onClick={onCreate} disabled={busy} title="Nueva sesión">
          + Nueva
        </button>
      </div>

      <ul className="session-list">
        {sessions.length === 0 && <li className="empty">todavía no hay ninguna</li>}
        {sessions.map((s) => (
          <li
            key={s.id}
            className={'session' + (s.id === selectedId ? ' selected' : '')}
            onClick={() => onSelect(s.id)}
          >
            <span className="dot" data-status={s.pty_status} />
            {editing === s.id ? (
              <input
                className="rename"
                autoFocus
                value={draft}
                onChange={(e) => setDraft(e.target.value)}
                onBlur={() => commit(s.id)}
                onKeyDown={(e) => {
                  if (e.key === 'Enter') commit(s.id)
                  if (e.key === 'Escape') setEditing(null)
                }}
                onClick={(e) => e.stopPropagation()}
              />
            ) : (
              <span
                className="name"
                onDoubleClick={(e) => {
                  e.stopPropagation()
                  setEditing(s.id)
                  setDraft(s.title || label(s))
                }}
                title={`${s.cwd} · doble click para renombrar`}
              >
                {label(s)}
              </span>
            )}
            <span className="when">{relative(s.last_active_at)}</span>
            <span className="actions" onClick={(e) => e.stopPropagation()}>
              {s.pty_status === 'running' ? (
                <button onClick={() => onKill(s.id)} disabled={busy} title="Matar el proceso">
                  ■
                </button>
              ) : (
                <button onClick={() => onRestart(s.id)} disabled={busy} title="Reanudar">
                  ▶
                </button>
              )}
              <button
                className="danger"
                onClick={() => onDelete(s.id)}
                disabled={busy}
                title="Borrar la sesión y su historial"
              >
                ✕
              </button>
            </span>
          </li>
        ))}
      </ul>
    </aside>
  )
}
```

- [ ] **Step 4: Rearmar `App`**

`web/src/App.tsx`:

```tsx
import { useCallback, useEffect, useRef, useState } from 'react'
import { TerminalView, type ConnState } from './TerminalView'
import { SessionList } from './SessionList'
import { api, type Session } from './api'

const label: Record<ConnState, string> = {
  connecting: 'conectando…',
  open: 'conectado',
  readonly: 'solo lectura',
  closed: 'desconectado',
  exited: 'sesión terminada',
}

// Cada cuánto se refresca la lista. M3 lo reemplaza por un canal de eventos,
// que ahí se justifica con los indicadores por tab.
const POLL_MS = 3000
const LAST_SESSION_KEY = 'webterm.lastSession'

export function App() {
  const [sessions, setSessions] = useState<Session[]>([])
  const [selected, setSelected] = useState<string | null>(
    () => localStorage.getItem(LAST_SESSION_KEY),
  )
  const [state, setState] = useState<ConnState>('connecting')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [auth, setAuth] = useState(false)
  const selectedRef = useRef(selected)
  selectedRef.current = selected

  const refresh = useCallback(async () => {
    try {
      const list = await api.list()
      setSessions(list)
      setError(null)
      // Si la sesión elegida ya no existe (la borramos en otro cliente),
      // caemos a la primera de la lista.
      if (!list.some((s) => s.id === selectedRef.current)) {
        setSelected(list[0]?.id ?? null)
      }
    } catch (err) {
      setError(String(err))
    }
  }, [])

  useEffect(() => {
    void refresh()
    const t = setInterval(() => void refresh(), POLL_MS)
    return () => clearInterval(t)
  }, [refresh])

  useEffect(() => {
    api.health().then((h) => setAuth(h.auth)).catch(() => {})
  }, [])

  useEffect(() => {
    if (selected) localStorage.setItem(LAST_SESSION_KEY, selected)
    else localStorage.removeItem(LAST_SESSION_KEY)
  }, [selected])

  // run envuelve las acciones del ABM: una sola a la vez, error visible y
  // refresco inmediato en vez de esperar al próximo poll.
  const run = useCallback(
    async (fn: () => Promise<unknown>) => {
      setBusy(true)
      try {
        await fn()
        setError(null)
      } catch (err) {
        setError(String(err))
      } finally {
        setBusy(false)
        await refresh()
      }
    },
    [refresh],
  )

  const create = () =>
    run(async () => {
      const s = await api.create({ cols: 80, rows: 24 })
      setSelected(s.id)
    })

  const remove = (id: string) =>
    run(async () => {
      if (!confirm('Borrar la sesión y todo su historial?')) return
      await api.remove(id)
      if (selectedRef.current === id) setSelected(null)
    })

  const current = sessions.find((s) => s.id === selected) ?? null

  return (
    <div className="app">
      <header className="topbar">
        <span className="brand">WebTerm</span>
        {current && <span className="cwd">{current.cwd}</span>}
        <span className="status" data-state={state}>
          <span className="dot" />
          {label[state]}
        </span>
        <span className="spacer" />
        {error && <span className="error">{error}</span>}
        {auth && (
          <a className="button" href="/api/logout">
            Salir
          </a>
        )}
      </header>

      <div className="layout">
        <SessionList
          sessions={sessions}
          selectedId={selected}
          busy={busy}
          onSelect={setSelected}
          onCreate={create}
          onRename={(id, title) => run(() => api.rename(id, title))}
          onKill={(id) => run(() => api.kill(id))}
          onRestart={(id) => run(() => api.restart(id, 80, 24))}
          onDelete={remove}
        />
        <main className="main">
          {selected ? (
            // key fuerza un remount al cambiar de sesión: cada una tiene su
            // propio xterm y su propio socket.
            <TerminalView key={selected} sessionId={selected} onState={setState} />
          ) : (
            <div className="placeholder">
              No hay ninguna sesión abierta. Creá una con <b>+ Nueva</b>.
            </div>
          )}
        </main>
      </div>
    </div>
  )
}
```

- [ ] **Step 5: Estilos del sidebar**

Agregar al final de `web/src/index.css`:

```css
.status[data-state="readonly"] .dot { background: var(--warn); }

.topbar .cwd {
  color: var(--fg-dim);
  font-size: 12px;
  max-width: 40ch;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.topbar .error {
  color: var(--danger);
  font-size: 12px;
}

.layout {
  display: flex;
  flex: 1 1 auto;
  min-height: 0;
}

.sidebar {
  flex: 0 0 240px;
  display: flex;
  flex-direction: column;
  background: var(--bg-chrome);
  border-right: 1px solid var(--border);
  min-height: 0;
}

.sidebar-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 8px 10px;
  font-size: 12px;
  color: var(--fg-dim);
  border-bottom: 1px solid var(--border);
}

.session-list {
  list-style: none;
  margin: 0;
  padding: 4px;
  overflow-y: auto;
  flex: 1 1 auto;
}

.session-list .empty {
  padding: 10px;
  color: var(--fg-dim);
  font-size: 12px;
}

.session {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 6px 8px;
  border-radius: 6px;
  cursor: pointer;
  font-size: 13px;
}

.session:hover { background: rgba(255, 255, 255, 0.04); }
.session.selected { background: rgba(90, 200, 168, 0.12); }

.session .dot {
  flex: 0 0 auto;
  width: 8px;
  height: 8px;
  border-radius: 50%;
  background: var(--fg-dim);
}

.session .dot[data-status="running"] { background: var(--accent); }
.session .dot[data-status="exited"] { background: var(--danger); }

.session .name {
  flex: 1 1 auto;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.session .rename {
  flex: 1 1 auto;
  min-width: 0;
  font: inherit;
  font-size: 13px;
  color: var(--fg);
  background: var(--bg);
  border: 1px solid var(--accent);
  border-radius: 4px;
  padding: 1px 4px;
}

.session .when {
  flex: 0 0 auto;
  color: var(--fg-dim);
  font-size: 11px;
}

/* Las acciones solo aparecen en la fila activa: son destructivas y no tienen
   por qué estar a un click de distancia todo el tiempo. */
.session .actions {
  display: none;
  gap: 2px;
}

.session:hover .actions,
.session.selected .actions { display: flex; }

.session .actions button {
  padding: 0 5px;
  font-size: 11px;
  line-height: 1.5;
}

.session .actions .danger:hover {
  border-color: var(--danger);
  color: var(--danger);
}

.main {
  flex: 1 1 auto;
  display: flex;
  flex-direction: column;
  min-width: 0;
  min-height: 0;
}

.placeholder {
  margin: auto;
  color: var(--fg-dim);
  font-size: 13px;
}
```

- [ ] **Step 6: Verificar tipos y build**

Run: `npm --prefix web run build`
Expected: compila sin errores de TypeScript.

- [ ] **Step 7: Probarlo a mano**

```bash
make run
```
Abrir http://127.0.0.1:7788 y verificar, en orden:
1. Crear una sesión con **+ Nueva**; aparece en el sidebar con el punto verde.
2. Correr `echo hola`, recargar la página: la sesión sigue ahí y el replay
   muestra el `hola`.
3. Crear una segunda sesión y alternar entre las dos: cada una conserva lo suyo.
4. **■** en una: el punto se pone rojo y el aviso de proceso terminado aparece
   en la terminal.
5. **▶** en la muerta: vuelve a arrancar y en el historial queda el marcador
   `— sesión reanudada —` arriba del prompt nuevo.
6. Doble click en el nombre: se renombra y sobrevive al refresh.
7. **✕**: pide confirmación y la saca de la lista.
8. Matar el backend con Ctrl-C y volver a levantarlo: las sesiones siguen
   listadas, todas en rojo, con su historial.

- [ ] **Step 8: Commit**

```bash
git add web/src/
git commit -m "feat(web): sidebar de sesiones con ABM

Una terminal visible por vez y polling de la lista cada 3 s: los tabs y
el canal de eventos en tiempo real son M3."
```

---

### Task 14: Documentación y verificación final

**Files:**
- Modify: `README.md`
- Modify: `webterm-diseno.md` (marcar M2 como hecho)
- Modify: `.gitignore`
- Modify: `Makefile` (target `test`)

**Interfaces:** ninguna nueva.

- [ ] **Step 1: Que la base no se cuele en el repo**

Agregar a `.gitignore`:

```
# Base de sesiones (queda en ~/.webterm por default, pero -db puede traerla acá)
*.db
*.db-wal
*.db-shm
```

- [ ] **Step 2: Target de tests en el Makefile**

En `Makefile`, agregar `test` a `.PHONY` y el target:

```make
## test: tests del backend con el detector de carreras
test:
	go test ./... -race
```

- [ ] **Step 3: Actualizar el README**

- En "Estado", marcar M2:

```markdown
## Estado: M2

- [x] **M1** — terminal web básica: un pty por conexión WebSocket, input/output,
      resize, true color, mouse.
- [x] **M2** — persistencia de sesiones (SQLite + session manager) y ABM.
- [ ] **M3** — UI multi-terminal (tabs).
```

  y reemplazar el párrafo "En M1 la sesión vive atada al WebSocket…" por:

```markdown
Desde M2 el pty vive en el backend, no en la conexión: cerrar la pestaña solo
cierra el socket. El estado, el KV y el último MB de output de cada sesión
quedan en SQLite, así que sobreviven al reinicio del backend — el proceso no,
porque es hijo suyo, y al arrancar se reconcilian a `exited` conservando el
historial.
```

- Agregar las filas nuevas a la tabla de flags:

```markdown
| `-db` | `~/.webterm/webterm.db` | base con el estado de las sesiones |
| `-history-bytes` | `1048576` | cuánto output se guarda por sesión |
```

- Reemplazar la sección "Protocolo WebSocket" por la documentación de la API
  REST y el protocolo nuevo:

````markdown
## API

| Método | Ruta | Qué hace |
|---|---|---|
| `GET` | `/api/sessions` | lista con estado, título y última actividad |
| `POST` | `/api/sessions` | crea y spawnea: `{title?, description?, cwd?, cols, rows}` |
| `GET` | `/api/sessions/{id}` | detalle |
| `PATCH` | `/api/sessions/{id}` | `{title?, description?, work_status?, kanban_status?}` |
| `POST` | `/api/sessions/{id}/kill` | mata el proceso, **conserva** el historial |
| `POST` | `/api/sessions/{id}/restart` | pty nuevo sobre la misma sesión |
| `DELETE` | `/api/sessions/{id}` | borra la sesión, su KV y su historial |
| `GET` | `/api/sessions/{id}/kv` | contexto persistido de la sesión |
| `PUT` | `/api/sessions/{id}/kv/{key}` | setea una clave (el body es el valor crudo) |
| `DELETE` | `/api/sessions/{id}/kv/{key}` | borra una clave |

`kill` y `DELETE` están separados a propósito: matar el proceso no tiene por
qué llevarse el historial.

## Protocolo WebSocket (`/ws/terminal?session_id=…`)

El `session_id` es obligatorio: las sesiones se crean por la API REST, no por
el upgrade.

| Sentido | Frame | Contenido |
|---|---|---|
| server → browser | texto | `{"type":"attached","session":{…}}` — primer mensaje |
| server → browser | binario | replay del historial y después output vivo |
| server → browser | texto | `{"type":"ready"}` — terminó el replay |
| server → browser | texto | `{"type":"exit","code":N,"reason":"normal"}` |
| server → browser | texto | `{"type":"error","error":"…"}` |
| browser → server | binario | input crudo del teclado |
| browser → server | texto | `{"type":"resize","cols":N,"rows":N}` |

Attachear a una sesión ya terminada manda el historial y `exit`: la conexión
queda de solo lectura, que es como la UI muestra lo que pasó en una sesión
muerta sin necesitar una vista aparte.
````

- Agregar la estructura nueva:

```markdown
cmd/webterm/          entrypoint y flags
internal/store/       SQLite: sesiones, KV e historial de output
internal/session/     session manager: ptys vivos, fan-out, reconciliación
internal/server/      HTTP, static file server, API REST, WebSocket
internal/terminal/    wrapper del pty (spawn, read/write, resize, wait)
web/                  frontend Vite + React + xterm.js
```

- Y una sección sobre la reconciliación, que es la parte menos obvia:

```markdown
## Estado de las sesiones

El pty es hijo del proceso Go: si el backend muere, mueren todas las sesiones.
Por eso lo que persiste es el *registro* de la sesión, no el proceso. Hay
cuatro mecanismos que mantienen la DB sincronizada con la realidad:

| Caso | Cómo se detecta | `exit_reason` |
|---|---|---|
| el shell hace `exit` | `cmd.Wait()` | `normal` |
| el shell muere pero un nieto retiene el pty | `cmd.Wait()` (el `Read` no da EOF) | `normal` |
| se reinició el backend | barrido al abrir la base | `backend_restart` |
| deriva entre la DB y las sesiones vivas | sweep cada 30 s | `orphaned` |
| lo mataste vos | `POST /kill` | `killed` |

Reanudar (`POST /restart`) reusa la misma fila: conserva id, título, cwd, KV e
historial, y deja un marcador `— sesión reanudada —` en el stream.
```

- [ ] **Step 4: Marcar el milestone en el diseño**

En `webterm-diseno.md`, al final de la sección `### M2`, agregar:

```markdown
**Hecho.** Ver `docs/superpowers/specs/2026-09-17-m2-persistencia-sesiones-design.md`
para el diseño detallado (esquema SQLite, cap del historial, reconciliación).
```

- [ ] **Step 5: Verificación final completa**

```bash
go vet ./...
go test ./... -race
npm --prefix web run build
make run
```

Expected: `vet` limpio, todos los tests en verde, el frontend buildea y la app
levanta. Verificar en el browser el recorrido del paso 7 de la tarea 13.

- [ ] **Step 6: Commit**

```bash
git add README.md webterm-diseno.md .gitignore Makefile
git commit -m "docs: documentar la API, el protocolo y la reconciliación de M2"
```

---

## Verificación de que el plan cubre la spec

| Sección de la spec | Tareas |
|---|---|
| Restricción del pty hijo del backend | 5, 9 (`cmd.Wait`, sweep), 2 (`ReconcileBoot`) |
| Modelo de datos completo (incl. M4/M6/M7) | 1, 2 |
| Migraciones versionadas | 1 |
| Pragmas WAL / busy_timeout / foreign_keys | 1 |
| Historial: escritura batcheada | 7 |
| Historial: poda al cap de 1 MB | 4, 7 |
| Historial: replay de chunks enteros y saneado | 4, 6, 8 (`sanitizeReplay`) |
| Ring buffer sembrado desde la DB | 8 (`startLive`) |
| Crear / spawn fallido persistido | 8 |
| Attach / detach sin matar el proceso | 8, 11 |
| Fan-out y expulsión del cliente lento | 6, 8, 11 |
| Muerte del proceso y exit code | 5, 8 |
| Reanudar reusando la fila + marcador | 9 |
| Matar vs. borrar | 9, 10 |
| Los cuatro mecanismos de reconciliación | 2, 5, 8, 9 |
| API REST completa | 10 |
| KV expuesto por HTTP | 3, 10 |
| Protocolo WebSocket | 11 |
| Sesión muerta en modo lectura | 8, 11 |
| Frontend: sidebar, ABM, polling, localStorage | 13 |
| Flags `-db` y `-history-bytes` | 12 |
| Documentación | 14 |
