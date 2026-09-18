# M10 — Daemon de sesiones: plan de implementación

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** que reiniciar el orquestador (HTTP, UI, API, MCP, recursos) deje de matar las sesiones de terminal que están corriendo.

**Architecture:** el proceso se parte en dos. Un **daemon** (`webterm daemon`) es dueño de los ptys, del ring buffer y del historial de output; expone HTTP + WebSocket sobre un socket Unix. El **orquestador** (`webterm`) es todo lo demás y le habla al daemon por ese socket, actuando como proxy de bytes hacia el browser. Los dos abren la misma base SQLite con dueños separados por tabla. Un paquete `internal/ptyapi` define el contrato que implementan tanto el manager en proceso como el cliente remoto, así que los tests corren sin levantar un daemon.

**Tech Stack:** Go 1.25, `creack/pty`, `gorilla/websocket`, SQLite (`modernc.org/sqlite`), React + TypeScript.

**Spec:** `docs/superpowers/specs/2026-09-18-m10-daemon-de-sesiones-design.md`

## Global Constraints

- **Comentarios y mensajes de commit en español**, explicando el *porqué*. Identificadores en inglés.
- **El invariante que justifica todo el milestone:** agregar una feature al orquestador no debe requerir tocar el daemon. Si una tarea te obliga a sumar un verbo al protocolo para algo que no es un pty, el diseño está mal — pará y decilo.
- **El protocolo del daemon son cinco rutas.** `GET /info`, `GET /sessions`, `POST /sessions`, `POST /sessions/{id}/kill`, `GET /sessions/{id}/attach`. Sin prefijo de versión.
- **`protocolVersion = 1`.** Se bumpea solo si cambia el protocolo del socket, nunca por un build.
- **`internal/ptyapi` no importa nada del proyecto.** Solo stdlib. Es el contrato, no una capa.
- **`internal/session` no vuelve a tocar metadata**: ni `title`, ni KV, ni recursos, ni `TouchActive`. Solo `session_output` y las columnas de pty.
- **Permisos `0600`** para el socket, el lock, el log y el archivo de token.
- **Ningún test levanta un daemon salvo los que lo prueban a propósito** (tareas 6, 7, 8 y 11), y esos usan `t.TempDir()` para socket y base.
- **`go test ./... -race` verde** es la condición de cierre de cada tarea.

## Estructura de archivos

| Archivo | Responsabilidad |
|---|---|
| `internal/ptyapi/ptyapi.go` | **nuevo**: `SpawnOpts`, `Attachment`, `Client`, `ErrNotLive` |
| `internal/store/session.go` | (modificar) `StatusStarting`, `ReasonDaemonRestart`, `ActiveIDs`, borrar `ReconcileBoot` |
| `internal/session/manager.go` | (modificar) se reduce a la superficie de pty; implementa `ptyapi.Client` |
| `internal/session/attachment.go` | **nuevo**: `*Attachment` con los métodos de `ptyapi.Attachment` |
| `internal/daemon/paths.go` | **nuevo**: socket/lock/log derivados del path de la base |
| `internal/daemon/server.go` | **nuevo**: las cuatro rutas de control sobre el socket Unix |
| `internal/daemon/attach.go` | **nuevo**: el upgrade a WebSocket y el bombeo bidireccional |
| `internal/daemonclient/client.go` | **nuevo**: el cliente HTTP sobre el socket |
| `internal/daemonclient/attach.go` | **nuevo**: la `ptyapi.Attachment` remota |
| `internal/control/manager.go` | **nuevo**: el manager del orquestador (store + recursos + `ptyapi.Client`) |
| `internal/control/attach.go` | **nuevo**: `Attach` con el camino de solo lectura |
| `internal/server/token.go` | **nuevo**: token persistido en disco |
| `internal/server/server.go` | (modificar) depende de `*control.Manager` |
| `internal/server/terminal.go` | (modificar) `Write`/`Resize` pasan por el attachment |
| `internal/server/sessions.go` | (modificar) tipos del manager nuevo |
| `internal/mcp/tools.go` | (modificar) tipo del manager nuevo |
| `cmd/webterm/main.go` | (modificar) despacho de subcomandos |
| `cmd/webterm/daemon.go` | **nuevo**: `webterm daemon` y el arranque on-demand |
| `Makefile` | (modificar) `go build` en vez de `go run` |

## Lo que hay que saber antes de empezar

Cinco cosas del código actual que el plan da por sentadas.

**1. `pty_status` no tiene `CHECK`.** Es un `TEXT NOT NULL` pelado, así que sumar el estado `starting` no necesita migración: alcanza la constante en Go.

**2. El orden dentro de `reap()` es load-bearing.** Hoy hace `MarkExited(...)` **y después** `hub.closeAll()`. Gracias a eso, cuando al cliente se le cierra el canal de output la fila ya dice `exited` con su código. Todo el manejo de fin de sesión —tanto el del daemon como el del orquestador— depende de ese orden. **No lo inviertas.**

**3. `broadcast` no copia el chunk.** Los suscriptores comparten el slice. El daemon lo escribe al WebSocket sin modificarlo; si alguna vez hace falta mutarlo, hay que copiar primero.

**4. `go run` borra el binario al salir.** El orquestador spawnea el daemon con `os.Executable()`, así que los targets del Makefile pasan a `go build -o bin/webterm` y correr el binario. Con `go run`, un `webterm daemon restart` posterior apuntaría a un archivo borrado.

**5. `gorilla/websocket` no admite writers concurrentes.** El patrón ya resuelto es `wsClient` en `internal/server/terminal.go:...`: un mutex alrededor de `WriteMessage`. El daemon y el cliente necesitan el suyo; copiá el patrón, no lo reinventes.

---

### Task 1: `internal/ptyapi` — el contrato compartido

Es el primer paso porque todo lo demás lo implementa o lo consume. Es un paquete de tipos sin lógica: por eso no lleva tests propios, lo prueban sus implementaciones (tareas 5 y 8).

**Files:**
- Create: `internal/ptyapi/ptyapi.go`

**Interfaces:**
- Consumes: nada. Solo stdlib.
- Produces:
  - `type SpawnOpts struct { ID, Shell, Cwd string; Cols, Rows int; Env []string; Banner string }`
  - `type Attachment interface { History() []byte; Output() <-chan []byte; Write([]byte) error; Resize(rows, cols uint16) error; Detach(); Dropped() bool }`
  - `type Client interface { Spawn(SpawnOpts) error; Attach(id string) (Attachment, error); Kill(id string) error; LiveIDs() ([]string, error); Close() error }`
  - `var ErrNotLive = errors.New("la sesión no está corriendo")`

- [ ] **Step 1: Escribir el paquete**

`internal/ptyapi/ptyapi.go`:

```go
// Package ptyapi es el contrato entre quien tiene los ptys y quien los usa.
//
// Existe para que el orquestador no sepa si del otro lado hay un daemon en
// otro proceso o un manager en el mismo: lo implementan las dos cosas, y los
// tests usan el segundo sin levantar nada.
//
// No importa nada del proyecto a propósito. Es el contrato, no una capa.
package ptyapi

import "errors"

// ErrNotLive lo devuelve todo lo que necesita un proceso vivo del otro lado.
// El orquestador lo traduce al camino de solo lectura, que no es un error
// sino cómo se mira una sesión terminada.
var ErrNotLive = errors.New("la sesión no está corriendo")

// ErrAlreadyLive lo devuelve Spawn sobre una sesión que ya tiene proceso.
//
// Es un error distinto de ErrNotLive y no su negación: viajan por el socket
// como status HTTP distintos (410 y 409) justamente para que el cliente pueda
// reconstruir cuál era sin parsear el texto.
var ErrAlreadyLive = errors.New("la sesión ya está corriendo")

// SpawnOpts describe el pty a arrancar.
//
// La fila en la base ya existe cuando esto llega: el que spawnea no inserta.
// Ese orden no es arbitrario, session_output tiene FK contra sessions.
type SpawnOpts struct {
	ID    string `json:"id"`
	Shell string `json:"shell"`
	Cwd   string `json:"cwd"`
	Cols  int    `json:"cols"`
	Rows  int    `json:"rows"`
	// Env son variables que se suman al entorno del pty. Las arma el
	// orquestador: quien spawnea no sabe qué es un token ni le importa. Es lo
	// que permite sumar una variable nueva sin tocar el daemon.
	Env []string `json:"env,omitempty"`
	// Banner queda en el historial antes del primer prompt. Lo escribe quien
	// posee el pty porque es el único que puede ordenarlo contra el writer.
	Banner string `json:"banner,omitempty"`
}

// Attachment es la conexión de un cliente a una sesión viva.
//
// Write y Resize cuelgan de acá y no del Client porque en la implementación
// remota viajan por el mismo socket que el output: son parte de la conexión,
// no operaciones sueltas.
type Attachment interface {
	// History es el replay que hay que mandar antes del stream vivo.
	History() []byte
	// Output se cierra cuando la sesión termina o se desattachea.
	Output() <-chan []byte
	// Write manda input crudo al pty.
	Write(p []byte) error
	// Resize cambia el tamaño de la ventana del pty.
	Resize(rows, cols uint16) error
	// Detach desconecta al cliente sin tocar la sesión.
	Detach()
	// Dropped dice si al cliente lo expulsaron por no leer a tiempo. Solo es
	// significativo después de que Output se haya cerrado.
	Dropped() bool
}

// Client es quien tiene los ptys.
type Client interface {
	// Spawn arranca el pty de una sesión cuya fila ya existe.
	Spawn(o SpawnOpts) error
	// Attach devuelve ErrNotLive si la sesión no tiene proceso corriendo.
	Attach(id string) (Attachment, error)
	// Kill es sincrónico: al volver, la fila ya refleja la muerte.
	Kill(id string) error
	// LiveIDs son las sesiones con proceso corriendo. Es la fuente de verdad
	// del sweep del orquestador.
	LiveIDs() ([]string, error)
	// Close suelta los recursos del cliente. No mata las sesiones remotas.
	Close() error
}
```

- [ ] **Step 2: Verificar que compila**

Run: `go build ./internal/ptyapi/`
Expected: sin salida.

- [ ] **Step 3: Verificar que no arrastra dependencias**

Run: `go list -deps ./internal/ptyapi/ | grep giuliano || echo "limpio"`
Expected: `limpio`

- [ ] **Step 4: Commit**

```bash
git add internal/ptyapi/
git commit -m "feat(ptyapi): contrato entre quien tiene los ptys y quien los usa

Lo van a implementar el manager en proceso y el cliente del daemon, así
que el orquestador no se entera de cuál tiene enfrente y los tests no
necesitan levantar un daemon.

Write y Resize cuelgan del Attachment y no del Client porque en la
implementación remota viajan por el mismo socket que el output."
```

---

### Task 2: Store — estado `starting`, `daemon_restart` y `ActiveIDs`

**Files:**
- Modify: `internal/store/session.go`
- Modify: `internal/store/session_test.go`

**Interfaces:**
- Consumes: `(*Store).db`, `(*Store).mu`, `ErrNotFound`, `execAffecting` (M2).
- Produces:
  - `const StatusStarting PtyStatus = "starting"`
  - `const ReasonDaemonRestart ExitReason = "daemon_restart"` (reemplaza `ReasonBackendRestart`)
  - `(*Store).ActiveIDs() ([]string, error)` (reemplaza `RunningIDs`)
  - `(*Store).MarkStarting(id string) error`
  - `ReconcileBoot` deja de existir.

**Por qué `ActiveIDs` y no `RunningIDs`:** el orquestador inserta la fila en
`starting` y recién después le pide el spawn al daemon. Si crashea en el medio,
una fila que filtre solo por `running` queda trabada en `starting` para siempre
porque el sweep no la mira nunca.

- [ ] **Step 1: Escribir los tests que fallan**

Agregar a `internal/store/session_test.go`:

```go
func TestActiveIDsIncluyeStartingYRunning(t *testing.T) {
	st := openTestStore(t)

	corriendo := &Session{ID: "a", Cwd: "/tmp", Shell: "/bin/sh", Cols: 80, Rows: 24, PtyStatus: StatusRunning}
	arrancando := &Session{ID: "b", Cwd: "/tmp", Shell: "/bin/sh", Cols: 80, Rows: 24, PtyStatus: StatusStarting}
	muerta := &Session{ID: "c", Cwd: "/tmp", Shell: "/bin/sh", Cols: 80, Rows: 24, PtyStatus: StatusExited}
	for _, s := range []*Session{corriendo, arrancando, muerta} {
		if err := st.CreateSession(s); err != nil {
			t.Fatal(err)
		}
	}

	ids, err := st.ActiveIDs()
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(ids)
	if !reflect.DeepEqual(ids, []string{"a", "b"}) {
		t.Fatalf("ActiveIDs = %v; quería [a b]", ids)
	}
}

// Una fila que quedó en starting porque el orquestador crasheó entre el
// insert y el spawn tiene que poder marcarse muerta como cualquier otra.
func TestMarkExitedSobreStarting(t *testing.T) {
	st := openTestStore(t)
	sess := &Session{ID: "x", Cwd: "/tmp", Shell: "/bin/sh", Cols: 80, Rows: 24, PtyStatus: StatusStarting}
	if err := st.CreateSession(sess); err != nil {
		t.Fatal(err)
	}

	if err := st.MarkExited("x", ReasonOrphaned, nil); err != nil {
		t.Fatal(err)
	}

	got, err := st.GetSession("x")
	if err != nil {
		t.Fatal(err)
	}
	if got.PtyStatus != StatusExited || got.ExitReason != string(ReasonOrphaned) {
		t.Fatalf("quedó %s/%s; quería exited/orphaned", got.PtyStatus, got.ExitReason)
	}
}

func TestMarkStarting(t *testing.T) {
	st := openTestStore(t)
	sess := &Session{ID: "x", Cwd: "/tmp", Shell: "/bin/sh", Cols: 80, Rows: 24, PtyStatus: StatusExited}
	sess.ExitReason = string(ReasonNormal)
	if err := st.CreateSession(sess); err != nil {
		t.Fatal(err)
	}

	if err := st.MarkStarting("x"); err != nil {
		t.Fatal(err)
	}

	got, err := st.GetSession("x")
	if err != nil {
		t.Fatal(err)
	}
	if got.PtyStatus != StatusStarting {
		t.Fatalf("pty_status = %s; quería starting", got.PtyStatus)
	}
	// Los rastros de la salida anterior se borran: si no, la UI muestra un
	// exit code al lado de una sesión que está arrancando.
	if got.ExitReason != "" || got.ExitCode != nil || got.ExitedAt != nil {
		t.Fatalf("quedaron rastros de la salida anterior: %+v", got)
	}
	if err := st.MarkStarting("no-existe"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("MarkStarting sobre una sesión inexistente dio %v; quería ErrNotFound", err)
	}
}
```

Agregá `"sort"`, `"reflect"` y `"errors"` a los imports del archivo si faltan.

- [ ] **Step 2: Correr los tests para verificar que fallan**

Run: `go test ./internal/store/ -run 'ActiveIDs|MarkStarting|MarkExitedSobreStarting' -v`
Expected: FAIL — `undefined: StatusStarting`, `st.ActiveIDs undefined`, `st.MarkStarting undefined`.

- [ ] **Step 3: Implementar**

En `internal/store/session.go`, en el bloque de constantes de `PtyStatus`:

```go
const (
	// StatusStarting es la ventana entre que el orquestador inserta la fila y
	// el daemon confirma el spawn. No necesita migración: pty_status es un
	// TEXT sin CHECK.
	StatusStarting PtyStatus = "starting"
	StatusRunning  PtyStatus = "running"
	StatusExited   PtyStatus = "exited"
)
```

En el bloque de `ExitReason`, reemplazar `ReasonBackendRestart` por:

```go
	// ReasonDaemonRestart es lo que le pasó a las sesiones cuando el daemon
	// arrancó de nuevo. Antes se llamaba backend_restart, cuando backend y
	// daemon eran el mismo proceso.
	ReasonDaemonRestart ExitReason = "daemon_restart"
```

Reemplazar `RunningIDs` por `ActiveIDs`:

```go
// ActiveIDs devuelve los ids que la DB cree con proceso detrás: los que están
// corriendo y los que están arrancando. El sweep del orquestador los contrasta
// contra lo que el daemon dice tener vivo.
//
// Incluir starting no es cosmético: una fila que quedó ahí porque el
// orquestador crasheó entre el insert y el spawn no la levantaría nadie más.
func (s *Store) ActiveIDs() ([]string, error) {
	rows, err := s.db.Query(`SELECT id FROM sessions WHERE pty_status IN (?, ?)`,
		StatusRunning, StatusStarting)
	if err != nil {
		return nil, fmt.Errorf("listando sesiones activas: %w", err)
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

// MarkStarting deja la fila lista para que el daemon la spawnee, borrando los
// rastros de la salida anterior para que la UI no muestre un exit code al lado
// de una sesión que está arrancando.
func (s *Store) MarkStarting(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.execAffecting(`
		UPDATE sessions
		SET pty_status = ?, exit_reason = NULL, exit_code = NULL, exited_at = NULL,
		    last_active_at = ?
		WHERE id = ?`,
		StatusStarting, time.Now().UnixMilli(), id)
}
```

Borrar `ReconcileBoot` entero. Ya no tiene sentido: era un caso particular de
"marcá muertas las filas activas sin proceso detrás", y ahora eso lo contesta
el daemon.

- [ ] **Step 4: Borrar el test de `ReconcileBoot`**

Buscarlo y borrarlo:

Run: `grep -rn "ReconcileBoot\|RunningIDs\|ReasonBackendRestart" --include='*.go' .`
Expected: las únicas menciones que queden son en `internal/session/` y `cmd/`, que se arreglan en las tareas 5 y 11. Borrá de `internal/store/session_test.go` los tests `TestReconcileBoot` y `TestRunningIDs` — el segundo lo reemplaza `TestActiveIDsIncluyeStartingYRunning`, que cubre lo mismo y además el caso nuevo.

- [ ] **Step 5: Correr los tests del store**

Run: `go test ./internal/store/ -race -v`
Expected: PASS. (`go build ./...` todavía falla por `internal/session`; se arregla en la tarea 5.)

- [ ] **Step 6: Commit**

```bash
git add internal/store/
git commit -m "feat(store): estado starting, daemon_restart y ActiveIDs

El orquestador inserta la fila antes de pedirle el spawn al daemon,
porque session_output tiene FK contra ella. La ventana entre las dos
cosas es el estado starting, que no necesita migración: pty_status es un
TEXT sin CHECK.

El sweep pasa a mirar starting además de running. Si no, una fila que
quedó ahí porque el orquestador crasheó en el medio no la levantaría
nadie nunca.

ReconcileBoot se va: era un caso particular de 'marcá muertas las filas
activas sin proceso detrás', y eso ahora lo contesta el daemon."
```

---

### Task 3: `internal/daemon` — rutas derivadas del path de la base

Una función pura, primero, porque el arranque on-demand de la tarea 11 la
necesita y porque es lo único de esta tarea que puede salir mal en silencio.

**Files:**
- Create: `internal/daemon/paths.go`
- Create: `internal/daemon/paths_test.go`

**Interfaces:**
- Consumes: nada. Solo stdlib.
- Produces:
  - `type Paths struct { Socket, Lock, Log string }`
  - `func PathsFor(dbPath string) Paths`

**Por qué derivarlas y no fijarlas:** con rutas fijas en `~/.webterm/`, una
instancia de desarrollo levantada con `-db ~/.webterm/dev.db` se conectaría al
daemon de producción y le spawnearía y mataría sesiones. Derivándolas del `-db`
que igual hay que pasar, la instancia de desarrollo queda aislada sin flags
nuevos que acordarse.

- [ ] **Step 1: Escribir el test que falla**

`internal/daemon/paths_test.go`:

```go
package daemon

import "testing"

func TestPathsFor(t *testing.T) {
	casos := []struct {
		db                   string
		socket, lock, logPath string
	}{
		{
			db:      "/home/g/.webterm/webterm.db",
			socket:  "/home/g/.webterm/webterm.sock",
			lock:    "/home/g/.webterm/webterm.lock",
			logPath: "/home/g/.webterm/webterm.log",
		},
		{
			db:      "/home/g/.webterm/dev.db",
			socket:  "/home/g/.webterm/dev.sock",
			lock:    "/home/g/.webterm/dev.lock",
			logPath: "/home/g/.webterm/dev.log",
		},
		// Sin extensión: se le pegan los sufijos igual, no se le come nada.
		{
			db:      "/tmp/webterm",
			socket:  "/tmp/webterm.sock",
			lock:    "/tmp/webterm.lock",
			logPath: "/tmp/webterm.log",
		},
		// Relativo: se conserva relativo. Quien lo use resuelve.
		{
			db:      "webterm.db",
			socket:  "webterm.sock",
			lock:    "webterm.lock",
			logPath: "webterm.log",
		},
	}

	for _, c := range casos {
		got := PathsFor(c.db)
		if got.Socket != c.socket || got.Lock != c.lock || got.Log != c.logPath {
			t.Errorf("PathsFor(%q) = %+v; quería %s / %s / %s",
				c.db, got, c.socket, c.lock, c.logPath)
		}
	}
}

// Dos bases distintas nunca comparten socket: es lo que aísla una instancia de
// desarrollo del daemon de producción.
func TestPathsForAislaBasesDistintas(t *testing.T) {
	prod := PathsFor("/home/g/.webterm/webterm.db")
	dev := PathsFor("/home/g/.webterm/dev.db")
	if prod.Socket == dev.Socket || prod.Lock == dev.Lock {
		t.Fatalf("dev y prod comparten rutas: %+v vs %+v", prod, dev)
	}
}
```

- [ ] **Step 2: Correr el test para verificar que falla**

Run: `go test ./internal/daemon/ -run TestPathsFor -v`
Expected: FAIL — `undefined: PathsFor`.

- [ ] **Step 3: Implementar**

`internal/daemon/paths.go`:

```go
// Package daemon es el proceso dueño de los ptys: los spawnea, los mantiene
// vivos entre reinicios del orquestador y baja su output a SQLite.
//
// Es deliberadamente flaco. No sabe de títulos, KV, recursos externos ni
// tokens: todo eso vive en el orquestador, que puede reiniciarse cuantas veces
// quiera sin tocar lo que corre acá.
package daemon

import (
	"path/filepath"
	"strings"
)

// Paths son los archivos del daemon para una base dada.
type Paths struct {
	Socket string // socket Unix donde escucha
	Lock   string // flock que evita dos daemons para la misma base
	Log    string // stdout y stderr del daemon
}

// PathsFor deriva las rutas del path de la base.
//
// Derivarlas en vez de fijarlas es lo que aísla una instancia de desarrollo:
// con rutas fijas, un `-db dev.db` en otro puerto se conectaría igual al
// daemon de producción y le spawnearía y mataría sesiones.
func PathsFor(dbPath string) Paths {
	base := strings.TrimSuffix(dbPath, filepath.Ext(dbPath))
	return Paths{
		Socket: base + ".sock",
		Lock:   base + ".lock",
		Log:    base + ".log",
	}
}
```

- [ ] **Step 4: Correr el test para verificar que pasa**

Run: `go test ./internal/daemon/ -race -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/
git commit -m "feat(daemon): rutas del daemon derivadas del path de la base

Con rutas fijas en ~/.webterm/, una instancia de desarrollo levantada
con -db dev.db se conectaría al daemon de producción y le spawnearía y
mataría sesiones. Derivándolas del -db que igual hay que pasar, queda
aislada de punta a punta sin flags nuevos."
```

---

### Task 4: Token persistido en disco

**Files:**
- Create: `internal/server/token.go`
- Create: `internal/server/token_test.go`

**Interfaces:**
- Consumes: `NewToken()` (existe en `internal/server/auth.go`).
- Produces: `func LoadOrCreateToken(path string) (string, error)`

**Por qué es parte de M10 y no un extra:** `main.go` hoy genera un token nuevo
en cada arranque. Con sesiones que sobreviven al reinicio del orquestador, el
`WEBTERM_TOKEN` inyectado en los ptys vivos queda viejo y el servidor MCP de M9
deja de autenticar **justo adentro de las sesiones que M10 existe para salvar**.
Sin esto, M10 rompe M9.

- [ ] **Step 1: Escribir los tests que fallan**

`internal/server/token_test.go`:

```go
package server

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadOrCreateTokenGeneraYPersiste(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")

	primero, err := LoadOrCreateToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if primero == "" {
		t.Fatal("no generó token")
	}

	// La segunda llamada tiene que devolver el mismo: si no, cada reinicio
	// invalidaría el WEBTERM_TOKEN de los ptys que siguen vivos.
	segundo, err := LoadOrCreateToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if segundo != primero {
		t.Fatalf("el token cambió entre llamadas: %q vs %q", primero, segundo)
	}
}

func TestLoadOrCreateTokenUsaPermisosRestrictivos(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if _, err := LoadOrCreateToken(path); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("permisos %o; quería 600 (el token da shell)", perm)
	}
}

func TestLoadOrCreateTokenIgnoraEspaciosYArchivoVacio(t *testing.T) {
	dir := t.TempDir()

	conEspacios := filepath.Join(dir, "a")
	if err := os.WriteFile(conEspacios, []byte("  secreto\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadOrCreateToken(conEspacios)
	if err != nil {
		t.Fatal(err)
	}
	if got != "secreto" {
		t.Fatalf("token = %q; quería %q", got, "secreto")
	}

	// Un archivo vacío es basura, no un token: hay que regenerarlo en vez de
	// arrancar con auth efectivamente desactivada.
	vacio := filepath.Join(dir, "b")
	if err := os.WriteFile(vacio, []byte("\n  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = LoadOrCreateToken(vacio)
	if err != nil {
		t.Fatal(err)
	}
	if got == "" {
		t.Fatal("con el archivo vacío tendría que haber generado uno nuevo")
	}
}

func TestLoadOrCreateTokenCreaElDirectorio(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "dir", "token")
	if _, err := LoadOrCreateToken(path); err != nil {
		t.Fatalf("tendría que crear el directorio: %v", err)
	}
}
```

- [ ] **Step 2: Correr los tests para verificar que fallan**

Run: `go test ./internal/server/ -run TestLoadOrCreateToken -v`
Expected: FAIL — `undefined: LoadOrCreateToken`.

- [ ] **Step 3: Implementar**

`internal/server/token.go`:

```go
package server

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// LoadOrCreateToken lee el token de disco y, si no hay, genera uno y lo guarda.
//
// Persistirlo es un requisito de M10, no una comodidad: con sesiones que
// sobreviven al reinicio del orquestador, un token nuevo por arranque invalida
// el WEBTERM_TOKEN que quedó inyectado en los ptys vivos y rompe el servidor
// MCP justo adentro de las sesiones que sobrevivieron.
func LoadOrCreateToken(path string) (string, error) {
	switch data, err := os.ReadFile(path); {
	case err == nil:
		// Un archivo vacío o con solo espacios es basura, no un token:
		// devolverlo sería arrancar con la auth efectivamente desactivada.
		if tok := strings.TrimSpace(string(data)); tok != "" {
			return tok, nil
		}
	case !os.IsNotExist(err):
		return "", fmt.Errorf("leyendo el token de %s: %w", path, err)
	}

	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", fmt.Errorf("creando %s: %w", dir, err)
		}
	}
	tok := NewToken()
	if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("guardando el token en %s: %w", path, err)
	}
	return tok, nil
}
```

- [ ] **Step 4: Correr los tests para verificar que pasan**

Run: `go test ./internal/server/ -run TestLoadOrCreateToken -race -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/server/token.go internal/server/token_test.go
git commit -m "feat(server): persistir el token de acceso en disco

Hoy se genera uno nuevo en cada arranque. Con sesiones que sobreviven al
reinicio del orquestador, eso invalida el WEBTERM_TOKEN inyectado en los
ptys vivos y rompe el MCP de M9 justo adentro de las sesiones que M10
existe para salvar.

Un archivo vacío se trata como ausente y se regenera: devolverlo sería
arrancar con la auth efectivamente desactivada."
```

---
### Task 5: `internal/session` — reducir el Manager a la superficie de pty

La tarea más invasiva del plan. El `Manager` hoy hace dos trabajos —ptys vivos y
filas en la base—; acá se queda solo con el primero y pasa a implementar
`ptyapi.Client`. Todo lo que sacamos reaparece en `internal/control` (tarea 9),
así que **no borres los tests: movelos a un archivo de staging** y la tarea 9
los reubica.

**Files:**
- Modify: `internal/session/manager.go`
- Create: `internal/session/attachment.go`
- Modify: `internal/session/manager_test.go`
- Modify: `internal/session/lifecycle_test.go`
- Create: `docs/superpowers/plans/.m10-tests-a-mover.md` (staging temporal, se borra en la tarea 9)

**Interfaces:**
- Consumes: `ptyapi.SpawnOpts`, `ptyapi.Attachment`, `ptyapi.ErrNotLive`, `ptyapi.ErrAlreadyLive`, `store.MarkRunning`, `store.MarkExited`, `store.UpdateSize`, `store.GetSession`, `store.ReadOutput`.
- Produces (`*Manager` satisface `ptyapi.Client`):
  - `type Config struct { HistoryBytes int64 }`
  - `func NewManager(st *store.Store, cfg Config) *Manager`
  - `func (m *Manager) Spawn(o ptyapi.SpawnOpts) error`
  - `func (m *Manager) Attach(id string) (ptyapi.Attachment, error)`
  - `func (m *Manager) Kill(id string) error`
  - `func (m *Manager) LiveIDs() ([]string, error)`
  - `func (m *Manager) Close() error`
  - `type Attachment struct{…}` con `History/Output/Write/Resize/Detach/Dropped`

**Lo que se va del Manager:** `Create`, `Restart`, `Delete`, `List`, `Get`,
`UpdateMeta`, `ListKV`, `SetKV`, `DeleteKV`, `ListResources`, `AddResource`,
`DeleteResource`, `LiveCount`, `Sweep`, `sweepLoop`, `Start`, y los campos
`Resources`, `ExtraEnv`, `Shell` y `SweepEvery` de `Config`.

**Lo que se queda tal cual:** `liveSession`, `startLive`, `pump`, `reap`, `ring`,
`hub`, `outputWriter`, `sanitizeReplay`, `resumeBanner` (que pasa a ser del
orquestador, ver tarea 9 — acá se borra porque el banner llega por `SpawnOpts`).

- [ ] **Step 1: Mover a staging los tests que dejan de aplicar**

Cortar de `manager_test.go` y `lifecycle_test.go` estos tests y pegarlos tal
cual en `docs/superpowers/plans/.m10-tests-a-mover.md` dentro de un bloque de
código — la tarea 9 los reescribe contra `control`:

`TestCreatePersisteYCorre`, `TestAttachASesionMuerta`, `TestAttachInexistente`,
`TestUpdateMeta`, `TestRestartReusaLaFila`, `TestRestartSobreSesionViva`,
`TestDeleteBorraTodo`, `TestSweepMarcaHuerfanas`.

Los que **se quedan** en `internal/session` y solo cambian de API:
`TestSobreviveAlDetach`, `TestExitDelShellSeReconcilia`, `TestFanOutADosClientes`,
`TestResizeLlegaAlPtyYSePersiste`, `TestKillConservaElHistorial`,
`TestKillEsIdempotente`, `TestCloseMataTodo`, `TestExtraEnvLlegaAlPty`,
`TestSinExtraEnvNoHayToken`. `writer_test.go` y `fanout_test.go` no se tocan.

- [ ] **Step 2: Reescribir el helper y los tests que se quedan**

En `internal/session/manager_test.go`, reemplazar `newTestManager` por:

```go
// newTestManager arma un manager sobre una base temporal. A diferencia de M2,
// el manager ya no inserta filas: la fila la crea quien lo llama, igual que
// hace el orquestador en producción.
func newTestManager(t *testing.T) (*Manager, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(st, Config{HistoryBytes: 64 << 10})
	t.Cleanup(func() {
		_ = m.Close()
		_ = st.Close()
	})
	return m, st
}

// spawnTest inserta la fila y arranca el pty, que es la secuencia que hace el
// orquestador. Devuelve el id.
func spawnTest(t *testing.T, m *Manager, st *store.Store, env []string) string {
	t.Helper()
	rec := &store.Session{
		ID: store.NewID(), Cwd: t.TempDir(), Shell: "/bin/sh",
		Cols: 80, Rows: 24, PtyStatus: store.StatusStarting,
	}
	if err := st.CreateSession(rec); err != nil {
		t.Fatal(err)
	}
	if err := m.Spawn(ptyapi.SpawnOpts{
		ID: rec.ID, Shell: rec.Shell, Cwd: rec.Cwd, Cols: 80, Rows: 24, Env: env,
	}); err != nil {
		t.Fatal(err)
	}
	return rec.ID
}

// waitFor espera hasta que cond sea verdadera. Los ptys son asincrónicos y un
// sleep fijo es la receta de un test que falla una vez cada veinte.
func waitFor(t *testing.T, plazo time.Duration, motivo string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(plazo)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout esperando: %s", motivo)
}
```

Adaptar los tests que se quedan: donde decían `m.Create(CreateOpts{…})` ahora
va `spawnTest(t, m, st, nil)`; donde leían `att.Session` usan la fila del store;
donde llamaban `m.Write(id, p)` o `m.Resize(id, r, c)` ahora usan
`att.Write(p)` y `att.Resize(r, c)` sobre el attachment.

Sumar estos tests nuevos, que son los que definen la superficie nueva:

```go
func TestSpawnNecesitaLaFila(t *testing.T) {
	m, _ := newTestManager(t)
	// Sin fila no hay spawn: session_output tiene FK contra sessions, así que
	// un pty sin fila dejaría el historial sin dónde escribirse.
	err := m.Spawn(ptyapi.SpawnOpts{ID: "no-existe", Shell: "/bin/sh", Cwd: t.TempDir(), Cols: 80, Rows: 24})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Spawn sin fila dio %v; quería ErrNotFound", err)
	}
}

func TestSpawnSobreSesionVivaEsConflicto(t *testing.T) {
	m, st := newTestManager(t)
	id := spawnTest(t, m, st, nil)

	err := m.Spawn(ptyapi.SpawnOpts{ID: id, Shell: "/bin/sh", Cwd: t.TempDir(), Cols: 80, Rows: 24})
	if !errors.Is(err, ptyapi.ErrAlreadyLive) {
		t.Fatalf("Spawn duplicado dio %v; quería ErrAlreadyLive", err)
	}
}

func TestSpawnMarcaRunning(t *testing.T) {
	m, st := newTestManager(t)
	id := spawnTest(t, m, st, nil)

	rec, err := st.GetSession(id)
	if err != nil {
		t.Fatal(err)
	}
	if rec.PtyStatus != store.StatusRunning {
		t.Fatalf("pty_status = %s; quería running", rec.PtyStatus)
	}
}

func TestSpawnFallidoMarcaSpawnFailed(t *testing.T) {
	m, st := newTestManager(t)
	rec := &store.Session{
		ID: store.NewID(), Cwd: t.TempDir(), Shell: "/no/existe/este/shell",
		Cols: 80, Rows: 24, PtyStatus: store.StatusStarting,
	}
	if err := st.CreateSession(rec); err != nil {
		t.Fatal(err)
	}

	if err := m.Spawn(ptyapi.SpawnOpts{
		ID: rec.ID, Shell: rec.Shell, Cwd: rec.Cwd, Cols: 80, Rows: 24,
	}); err == nil {
		t.Fatal("un shell inexistente tendría que fallar")
	}

	// El error tiene que quedar en la fila, no perderse en un log: es lo que
	// hace que aparezca en la UI.
	got, err := st.GetSession(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PtyStatus != store.StatusExited || got.ExitReason != string(store.ReasonSpawnFailed) {
		t.Fatalf("quedó %s/%s; quería exited/spawn_failed", got.PtyStatus, got.ExitReason)
	}
}

func TestSpawnConBannerLoDejaEnElHistorial(t *testing.T) {
	m, st := newTestManager(t)
	rec := &store.Session{
		ID: store.NewID(), Cwd: t.TempDir(), Shell: "/bin/sh",
		Cols: 80, Rows: 24, PtyStatus: store.StatusStarting,
	}
	if err := st.CreateSession(rec); err != nil {
		t.Fatal(err)
	}
	if err := m.Spawn(ptyapi.SpawnOpts{
		ID: rec.ID, Shell: rec.Shell, Cwd: rec.Cwd, Cols: 80, Rows: 24,
		Banner: "MARCADOR-DE-REANUDACION",
	}); err != nil {
		t.Fatal(err)
	}

	att, err := m.Attach(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer att.Detach()
	if !bytes.Contains(att.History(), []byte("MARCADOR-DE-REANUDACION")) {
		t.Fatalf("el banner no está en el replay: %q", att.History())
	}
}

func TestAttachASesionNoVivaDaErrNotLive(t *testing.T) {
	m, st := newTestManager(t)
	id := spawnTest(t, m, st, nil)
	if err := m.Kill(id); err != nil {
		t.Fatal(err)
	}

	// El daemon no sabe leer historiales de sesiones muertas: ese camino es
	// del orquestador, que lo resuelve contra la base sin consultarlo.
	if _, err := m.Attach(id); !errors.Is(err, ptyapi.ErrNotLive) {
		t.Fatalf("Attach a sesión muerta dio %v; quería ErrNotLive", err)
	}
}

func TestLiveIDs(t *testing.T) {
	m, st := newTestManager(t)
	a := spawnTest(t, m, st, nil)
	b := spawnTest(t, m, st, nil)

	ids, err := m.LiveIDs()
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(ids)
	quiero := []string{a, b}
	sort.Strings(quiero)
	if !reflect.DeepEqual(ids, quiero) {
		t.Fatalf("LiveIDs = %v; quería %v", ids, quiero)
	}

	if err := m.Kill(a); err != nil {
		t.Fatal(err)
	}
	ids, _ = m.LiveIDs()
	if !reflect.DeepEqual(ids, []string{b}) {
		t.Fatalf("después del kill LiveIDs = %v; quería [%s]", ids, b)
	}
}

func TestKillSesionNoVivaDaErrNotLive(t *testing.T) {
	m, st := newTestManager(t)
	id := spawnTest(t, m, st, nil)
	if err := m.Kill(id); err != nil {
		t.Fatal(err)
	}
	// La idempotencia es del orquestador, que sabe si la fila existe. Acá el
	// contrato es literal: no hay proceso que matar.
	if err := m.Kill(id); !errors.Is(err, ptyapi.ErrNotLive) {
		t.Fatalf("segundo Kill dio %v; quería ErrNotLive", err)
	}
}

// Garantía load-bearing: cuando al cliente se le cierra el canal de output, la
// fila YA dice exited. Todo el manejo de fin de sesión —el del daemon y el del
// orquestador— depende de este orden.
func TestLaFilaYaEstaMarcadaCuandoSeCierraElOutput(t *testing.T) {
	m, st := newTestManager(t)
	id := spawnTest(t, m, st, nil)

	att, err := m.Attach(id)
	if err != nil {
		t.Fatal(err)
	}
	defer att.Detach()
	if err := att.Write([]byte("exit\n")); err != nil {
		t.Fatal(err)
	}

	for range att.Output() {
		// drenar hasta que cierre
	}

	rec, err := st.GetSession(id)
	if err != nil {
		t.Fatal(err)
	}
	if rec.PtyStatus != store.StatusExited {
		t.Fatalf("al cerrarse el output la fila decía %s; quería exited", rec.PtyStatus)
	}
}
```

- [ ] **Step 3: Correr los tests para verificar que fallan**

Run: `go test ./internal/session/ -run 'TestSpawn|TestLiveIDs|TestAttachASesionNoViva' 2>&1 | head -20`
Expected: FAIL de compilación — `m.Spawn undefined`, `undefined: ptyapi`.

- [ ] **Step 4: Reescribir `internal/session/manager.go`**

Cambios concretos:

**4a.** En el bloque de constantes, borrar `defaultSweepInterval`. Dejar
`readBufSize`, `subBuffer`, `reapTimeout` y `DefaultHistoryBytes`.

**4b.** Reemplazar las variables de error y `resumeBanner` por nada: `ErrNotLive`
y `ErrAlreadyRunning` se van a `ptyapi` (`ErrNotLive`, `ErrAlreadyLive`), y el
banner ahora llega por `SpawnOpts`.

**4c.** `Config` queda:

```go
// Config parametriza el manager. Es corta a propósito: todo lo que no sea el
// pty —shell por defecto, variables de entorno, recursos externos— lo resuelve
// el orquestador y llega resuelto en cada SpawnOpts.
type Config struct {
	HistoryBytes int64 // cap de historial por sesión
}
```

**4d.** Borrar de `Manager` el campo `stop`, `stopOnce` ya no hace falta para el
sweep pero sí para `Close`; dejá `stop`, `stopOnce` y `wg`. Borrar `Start`,
`sweepLoop`, `Sweep`.

**4e.** Reemplazar `Create` y `Restart` por `Spawn`:

```go
// Spawn arranca el pty de una sesión cuya fila ya existe.
//
// No inserta nada: la fila la crea el orquestador antes de llamar acá, porque
// session_output tiene FK contra sessions y el historial empieza a escribirse
// apenas arranca el pump.
//
// Está serializado entero bajo spawnMu: spawnear es raro y barato de
// serializar, y sin eso dos Spawn concurrentes del mismo id podrían pasar los
// dos el chequeo de "no está vivo" y dejar un pty huérfano en el mapa.
func (m *Manager) Spawn(o ptyapi.SpawnOpts) error {
	m.spawnMu.Lock()
	defer m.spawnMu.Unlock()

	rec, err := m.st.GetSession(o.ID)
	if err != nil {
		return err
	}
	if m.lookup(o.ID) != nil {
		return ptyapi.ErrAlreadyLive
	}
	if o.Cols <= 0 {
		o.Cols = rec.Cols
	}
	if o.Rows <= 0 {
		o.Rows = rec.Rows
	}

	pt, err := terminal.New(o.ID, terminal.Config{
		Shell: o.Shell, Cwd: o.Cwd,
		Rows: uint16(o.Rows), Cols: uint16(o.Cols), Env: o.Env,
	})
	if err != nil {
		// El error queda en la fila, no en un log: así aparece en la UI en vez
		// de perderse.
		code := -1
		if merr := m.st.MarkExited(o.ID, store.ReasonSpawnFailed, &code); merr != nil {
			log.Printf("[%s] no se pudo registrar el spawn fallido: %v", o.ID, merr)
		}
		return fmt.Errorf("spawneando la sesión %s: %w", o.ID, err)
	}

	m.mu.Lock()
	if err := m.st.MarkRunning(o.ID, o.Cols, o.Rows); err != nil {
		m.mu.Unlock()
		_ = pt.Close()
		return err
	}
	m.startLive(rec, pt, []byte(o.Banner))
	m.mu.Unlock()

	log.Printf("[%s] pty arrancado (%dx%d) en %s", o.ID, o.Cols, o.Rows, o.Cwd)
	return nil
}
```

Sumar el campo `spawnMu sync.Mutex` al struct `Manager`.

**4f.** `Attach` deja de leer la base y de tocar `TouchActive`:

```go
// Attach conecta un cliente al pty. Una sesión sin proceso da ErrNotLive: el
// camino de solo lectura sobre el historial es del orquestador, que lo resuelve
// contra la base sin consultar acá.
func (m *Manager) Attach(id string) (ptyapi.Attachment, error) {
	l := m.lookup(id)
	if l == nil {
		return nil, ptyapi.ErrNotLive
	}
	hist, sub := l.attach(subBuffer)
	return &Attachment{m: m, live: l, history: sanitizeReplay(hist), sub: sub}, nil
}
```

**4g.** `Kill` deja de consultar la base:

```go
// Kill mata el proceso y conserva la fila y el historial. Es sincrónico: al
// volver, la DB ya refleja la muerte, así que un GET inmediato no miente.
func (m *Manager) Kill(id string) error {
	l := m.lookup(id)
	if l == nil {
		return ptyapi.ErrNotLive
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
```

**4h.** `LiveCount` se reemplaza por `LiveIDs`:

```go
// LiveIDs son las sesiones con proceso corriendo. Es la fuente de verdad del
// sweep del orquestador: lo que no está acá y la base cree activo, está muerto.
func (m *Manager) LiveIDs() ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := make([]string, 0, len(m.live))
	for id := range m.live {
		ids = append(ids, id)
	}
	return ids, nil
}
```

**4i.** Borrar `Write`, `Resize`, `List`, `Get`, `UpdateMeta`, `ListKV`, `SetKV`,
`DeleteKV`, `Delete`, `ListResources`, `AddResource`, `DeleteResource`,
`LinkedResource`, `ErrUnknownResource` y el tipo `CreateOpts`. `Write` y `Resize`
pasan al `Attachment`.

**4j.** Sumar al final del archivo la comprobación de que el contrato se cumple:

```go
// El manager en proceso es una implementación de ptyapi.Client igual que el
// cliente del daemon. Esta línea es lo que hace que romper el contrato falle
// al compilar y no en runtime.
var _ ptyapi.Client = (*Manager)(nil)
```

- [ ] **Step 5: Escribir `internal/session/attachment.go`**

```go
package session

import (
	"github.com/giuliano/webterm/internal/ptyapi"
)

// Attachment es la conexión de un cliente a un pty vivo.
//
// Write y Resize cuelgan de acá y no del Manager porque la implementación
// remota los manda por el mismo socket que el output: hacerlos operaciones
// sueltas del Client obligaría a una segunda conexión para lo mismo.
type Attachment struct {
	m       *Manager
	live    *liveSession
	history []byte
	sub     *subscriber
}

func (a *Attachment) History() []byte           { return a.history }
func (a *Attachment) Output() <-chan []byte     { return a.sub.out() }
func (a *Attachment) Dropped() bool             { return a.sub.wasDropped() }
func (a *Attachment) Detach()                   { a.live.detach(a.sub) }

// Write manda input crudo al pty.
func (a *Attachment) Write(p []byte) error {
	_, err := a.live.pty.Write(p)
	return err
}

// Resize cambia el tamaño del pty y lo persiste: cols y rows son estado del
// pty, así que su dueño es el daemon, y persistirlos es lo que hace que una
// sesión reanudada vuelva con las dimensiones que tenía.
func (a *Attachment) Resize(rows, cols uint16) error {
	if err := a.live.pty.Resize(rows, cols); err != nil {
		return err
	}
	return a.m.st.UpdateSize(a.live.id, int(cols), int(rows))
}

var _ ptyapi.Attachment = (*Attachment)(nil)
```

- [ ] **Step 6: Correr los tests del paquete**

Run: `go test ./internal/session/ -race -v`
Expected: PASS. (`go build ./...` sigue roto por `server`, `mcp` y `cmd`; se
arregla en las tareas 9, 10 y 11.)

- [ ] **Step 7: Commit**

```bash
git add internal/session/ internal/ptyapi/ docs/superpowers/plans/.m10-tests-a-mover.md
git commit -m "refactor(session): el manager se reduce a la superficie de pty

Hacía dos trabajos, ptys vivos y filas en la base. Se queda con el
primero y pasa a implementar ptyapi.Client, que es lo que le va a
permitir al orquestador no saber si del otro lado hay un daemon o un
manager en proceso.

Spawn ya no inserta: la fila la crea el orquestador antes, porque
session_output tiene FK contra sessions. Attach a una sesión muerta da
ErrNotLive en vez de leer el historial, porque ese camino es del
orquestador y no necesita molestar al daemon.

Los tests de ABM quedan en staging; la tarea de control los reubica."
```

---

### Task 6: `internal/daemon` — las cuatro rutas de control

**Files:**
- Create: `internal/daemon/server.go`
- Create: `internal/daemon/server_test.go`

**Interfaces:**
- Consumes: `ptyapi.Client`, `ptyapi.SpawnOpts`, `ptyapi.ErrNotLive`, `ptyapi.ErrAlreadyLive`, `PathsFor` (tarea 3).
- Produces:
  - `const ProtocolVersion = 1`
  - `type Info struct { ProtocolVersion int; PID int; StartedAt int64 }`
  - `type Server struct{…}`
  - `func NewServer(pty ptyapi.Client) *Server`
  - `func (s *Server) Handler() http.Handler`
  - `func (s *Server) Serve(socketPath string) error` — escucha y bloquea
  - `func (s *Server) Shutdown(ctx context.Context) error`

- [ ] **Step 1: Escribir los tests que fallan**

`internal/daemon/server_test.go`:

```go
package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/giuliano/webterm/internal/ptyapi"
	"github.com/giuliano/webterm/internal/session"
	"github.com/giuliano/webterm/internal/store"
)

// newTestDaemon arma un daemon con un manager real sobre una base temporal.
// Sin mocks: el objetivo es probar el daemon contra ptys de verdad.
func newTestDaemon(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	m := session.NewManager(st, session.Config{HistoryBytes: 64 << 10})
	srv := httptest.NewServer(NewServer(m).Handler())
	t.Cleanup(func() {
		srv.Close()
		_ = m.Close()
		_ = st.Close()
	})
	return srv, st
}

// nuevaFila inserta la fila que el spawn necesita, como hace el orquestador.
func nuevaFila(t *testing.T, st *store.Store) *store.Session {
	t.Helper()
	rec := &store.Session{
		ID: store.NewID(), Cwd: t.TempDir(), Shell: "/bin/sh",
		Cols: 80, Rows: 24, PtyStatus: store.StatusStarting,
	}
	if err := st.CreateSession(rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

func postJSON(t *testing.T, url string, body any) *http.Response {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.Post(url, "application/json", strings.NewReader(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestInfoAnunciaLaVersionDeProtocolo(t *testing.T) {
	srv, _ := newTestDaemon(t)

	res, err := http.Get(srv.URL + "/info")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	var info Info
	if err := json.NewDecoder(res.Body).Decode(&info); err != nil {
		t.Fatal(err)
	}
	if info.ProtocolVersion != ProtocolVersion {
		t.Fatalf("protocol_version = %d; quería %d", info.ProtocolVersion, ProtocolVersion)
	}
	if info.PID != os.Getpid() {
		t.Fatalf("pid = %d; quería %d", info.PID, os.Getpid())
	}
	if info.StartedAt == 0 {
		t.Fatal("started_at vacío")
	}
}

func TestSpawnYListado(t *testing.T) {
	srv, st := newTestDaemon(t)
	rec := nuevaFila(t, st)

	res := postJSON(t, srv.URL+"/sessions", ptyapi.SpawnOpts{
		ID: rec.ID, Shell: rec.Shell, Cwd: rec.Cwd, Cols: 80, Rows: 24,
	})
	defer res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("POST /sessions dio %d; quería 204", res.StatusCode)
	}

	lres, err := http.Get(srv.URL + "/sessions")
	if err != nil {
		t.Fatal(err)
	}
	defer lres.Body.Close()
	var out struct {
		IDs []string `json:"ids"`
	}
	if err := json.NewDecoder(lres.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.IDs) != 1 || out.IDs[0] != rec.ID {
		t.Fatalf("ids = %v; quería [%s]", out.IDs, rec.ID)
	}
}

func TestSpawnSinFilaDa404(t *testing.T) {
	srv, _ := newTestDaemon(t)

	res := postJSON(t, srv.URL+"/sessions", ptyapi.SpawnOpts{
		ID: "no-existe", Shell: "/bin/sh", Cwd: t.TempDir(), Cols: 80, Rows: 24,
	})
	defer res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("dio %d; quería 404", res.StatusCode)
	}
}

func TestSpawnDuplicadoDa409(t *testing.T) {
	srv, st := newTestDaemon(t)
	rec := nuevaFila(t, st)
	opts := ptyapi.SpawnOpts{ID: rec.ID, Shell: rec.Shell, Cwd: rec.Cwd, Cols: 80, Rows: 24}

	postJSON(t, srv.URL+"/sessions", opts).Body.Close()
	res := postJSON(t, srv.URL+"/sessions", opts)
	defer res.Body.Close()
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("dio %d; quería 409", res.StatusCode)
	}
}

func TestKillDevuelve204YDespues410(t *testing.T) {
	srv, st := newTestDaemon(t)
	rec := nuevaFila(t, st)
	postJSON(t, srv.URL+"/sessions", ptyapi.SpawnOpts{
		ID: rec.ID, Shell: rec.Shell, Cwd: rec.Cwd, Cols: 80, Rows: 24,
	}).Body.Close()

	res := postJSON(t, srv.URL+"/sessions/"+rec.ID+"/kill", nil)
	defer res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("kill dio %d; quería 204", res.StatusCode)
	}

	// Kill es sincrónico: al volver la fila ya tiene que estar marcada.
	got, err := st.GetSession(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PtyStatus != store.StatusExited {
		t.Fatalf("después del kill la fila decía %s; quería exited", got.PtyStatus)
	}

	// Un segundo kill no tiene proceso que matar: 410, no 404 — la fila sigue
	// existiendo. La idempotencia la resuelve el orquestador, que es el que
	// sabe distinguir las dos cosas.
	res2 := postJSON(t, srv.URL+"/sessions/"+rec.ID+"/kill", nil)
	defer res2.Body.Close()
	if res2.StatusCode != http.StatusGone {
		t.Fatalf("segundo kill dio %d; quería 410", res2.StatusCode)
	}
}

// El daemon escucha en un socket Unix, no en un puerto: nadie de la red puede
// llegarle, y por eso no hay token en esta capa.
func TestServeEscuchaEnSocketUnixCon0600(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m := session.NewManager(st, session.Config{})
	defer m.Close()

	sock := filepath.Join(t.TempDir(), "d.sock")
	srv := NewServer(m)
	go func() { _ = srv.Serve(sock) }()
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	waitForSocket(t, sock)

	fi, err := os.Stat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("permisos del socket %o; quería 600", perm)
	}
}

// Un .sock huérfano de un crash anterior no puede impedir el arranque.
func TestServeBorraUnSocketHuerfano(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m := session.NewManager(st, session.Config{})
	defer m.Close()

	sock := filepath.Join(t.TempDir(), "d.sock")
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	srv := NewServer(m)
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(sock) }()
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	waitForSocket(t, sock)
	select {
	case err := <-errc:
		t.Fatalf("Serve falló con un socket huérfano: %v", err)
	default:
	}
}
```

Sumá este helper al archivo (los imports `context` y `time` van arriba):

```go
// waitForSocket espera a que el socket acepte conexiones. Serve arranca en otra
// goroutine, así que sin esto el test corre antes de que exista.
func waitForSocket(t *testing.T, path string) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if c, err := net.Dial("unix", path); err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("el socket %s nunca aceptó conexiones", path)
}
```

- [ ] **Step 2: Correr los tests para verificar que fallan**

Run: `go test ./internal/daemon/ 2>&1 | head -20`
Expected: FAIL de compilación — `undefined: NewServer`, `undefined: Info`.

- [ ] **Step 3: Implementar**

`internal/daemon/server.go`:

```go
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/giuliano/webterm/internal/ptyapi"
	"github.com/giuliano/webterm/internal/store"
)

// ProtocolVersion se bumpea SOLO cuando cambia el protocolo del socket, nunca
// por un build. Existe porque recompilar el binario no reemplaza al daemon que
// ya está corriendo: sin este número, un daemon viejo falla con un error raro
// a mitad de un attach en vez de decir que hay que reiniciarlo.
const ProtocolVersion = 1

// maxSpawnBody acota el body de un spawn. Nadie manda un cwd de más de unos KB.
const maxSpawnBody = 64 << 10

// Info es lo que contesta GET /info.
type Info struct {
	ProtocolVersion int   `json:"protocol_version"`
	PID             int   `json:"pid"`
	StartedAt       int64 `json:"started_at"`
}

// Server expone el manager de ptys sobre un socket Unix.
//
// No tiene auth: escucha en un socket con permisos 0600, así que quien puede
// abrirlo ya es el dueño de la máquina y ya tiene shell. Sumar un token acá
// sería ceremonia sin propiedad nueva.
type Server struct {
	pty       ptyapi.Client
	startedAt int64

	http *http.Server
}

func NewServer(pty ptyapi.Client) *Server {
	return &Server{pty: pty, startedAt: time.Now().UnixMilli()}
}

// Handler arma el router. Son cinco rutas y ese número no debería crecer: si
// hace falta un verbo nuevo para algo que no es un pty, el diseño está mal.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /info", s.handleInfo)
	mux.HandleFunc("GET /sessions", s.handleList)
	mux.HandleFunc("POST /sessions", s.handleSpawn)
	mux.HandleFunc("POST /sessions/{id}/kill", s.handleKill)
	mux.HandleFunc("GET /sessions/{id}/attach", s.handleAttach)
	return mux
}

// Serve escucha en el socket y bloquea.
func (s *Server) Serve(socketPath string) error {
	// Un .sock que quedó de un crash anterior haría fallar el bind. Borrarlo es
	// seguro porque el flock del arranque ya garantizó que no hay otro daemon
	// para esta base.
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("limpiando %s: %w", socketPath, err)
	}
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("escuchando en %s: %w", socketPath, err)
	}
	// El bind respeta el umask, así que los permisos se fijan después.
	if err := os.Chmod(socketPath, 0o600); err != nil {
		_ = ln.Close()
		return fmt.Errorf("ajustando permisos de %s: %w", socketPath, err)
	}

	s.http = &http.Server{Handler: s.Handler()}
	log.Printf("daemon escuchando en %s (protocolo %d, pid %d)", socketPath, ProtocolVersion, os.Getpid())
	if err := s.http.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Shutdown corta el servidor HTTP. No mata las sesiones: eso lo decide quien
// apaga el proceso.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.http == nil {
		return nil
	}
	return s.http.Shutdown(ctx)
}

func (s *Server) handleInfo(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, Info{
		ProtocolVersion: ProtocolVersion, PID: os.Getpid(), StartedAt: s.startedAt,
	})
}

func (s *Server) handleList(w http.ResponseWriter, _ *http.Request) {
	ids, err := s.pty.LiveIDs()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string][]string{"ids": ids})
}

func (s *Server) handleSpawn(w http.ResponseWriter, r *http.Request) {
	var o ptyapi.SpawnOpts
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSpawnBody)).Decode(&o); err != nil {
		http.Error(w, "body inválido", http.StatusBadRequest)
		return
	}
	if o.ID == "" {
		http.Error(w, "falta id", http.StatusBadRequest)
		return
	}
	if err := s.pty.Spawn(o); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleKill(w http.ResponseWriter, r *http.Request) {
	if err := s.pty.Kill(r.PathValue("id")); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("daemon: escribiendo respuesta: %v", err)
	}
}

// writeError traduce los errores del manager a un status que el cliente sabe
// volver a convertir en el error original.
//
// Los tres estados tienen status distintos a propósito. "No existe la fila" y
// "la fila existe pero no hay proceso" son situaciones distintas y el
// orquestador reacciona distinto a cada una —404 es un error de verdad, 410 es
// el camino de solo lectura—, así que si compartieran status el cliente
// tendría que adivinar parseando el texto.
func writeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		http.Error(w, err.Error(), http.StatusNotFound) // no existe la fila
	case errors.Is(err, ptyapi.ErrNotLive):
		http.Error(w, err.Error(), http.StatusGone) // existe, pero sin proceso
	case errors.Is(err, ptyapi.ErrAlreadyLive):
		http.Error(w, err.Error(), http.StatusConflict)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
```

**Nota:** `handleAttach` todavía no existe; escribí un stub que devuelva 501
para que compile, y la tarea 7 lo reemplaza:

```go
func (s *Server) handleAttach(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, "no implementado", http.StatusNotImplemented)
}
```

- [ ] **Step 4: Correr los tests para verificar que pasan**

Run: `go test ./internal/daemon/ -race -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/
git commit -m "feat(daemon): las cuatro rutas de control sobre el socket Unix

info, list, spawn y kill. Sin auth y sin prefijo de versión: escucha en
un socket 0600, así que quien puede abrirlo ya tiene shell, y cliente y
servidor salen del mismo binario.

ProtocolVersion no versiona la API: detecta que el daemon corriendo
quedó viejo respecto del binario recién compilado, que es el único caso
raro que introduce tener un solo binario con dos modos.

Un .sock huérfano de un crash se borra en vez de impedir el arranque; el
flock del arranque ya garantizó que no hay otro daemon para esta base."
```

---
### Task 7: `internal/daemon` — attach por WebSocket

El plano de datos. El framing es **el mismo que el orquestador ya le habla al
browser**, para que reenviar sea copiar frames y no traducir.

**Files:**
- Create: `internal/daemon/attach.go`
- Modify: `internal/daemon/server.go` (reemplazar el stub de `handleAttach`)
- Create: `internal/daemon/attach_test.go`

**Interfaces:**
- Consumes: `ptyapi.Attachment`, `gorilla/websocket`.
- Produces: `(*Server).handleAttach` real, y el framing documentado abajo.

**El framing del socket interno:**

| Sentido | Frame | Contenido |
|---|---|---|
| daemon → cliente | binario | replay del historial (un frame) y después output vivo |
| daemon → cliente | texto | `{"type":"ready"}` — terminó el replay |
| daemon → cliente | texto | `{"type":"dropped"}` — al cliente lo expulsaron por lento |
| cliente → daemon | binario | input crudo para el pty |
| cliente → daemon | texto | `{"type":"resize","rows":N,"cols":N}` |

**Lo que NO manda el daemon y por qué:**

- **No hay `attached`.** Lleva la fila completa con título y kanban, que el
  daemon no conoce. Lo compone el orquestador con su propia fila.
- **No hay `exit`.** Cuando la sesión termina, el canal de output se cierra y el
  cliente ve el fin del stream. Para ese momento la fila **ya** dice `exited`
  con su código, porque `reap()` hace `MarkExited` antes de `hub.closeAll()`.
  El orquestador lee la fila y arma el `exit` que ve el browser. Mandar un
  `exit` por acá sería duplicar un dato que ya está en la base.
- **No hay ping/pong.** El keepalive existe para detectar clientes muertos
  detrás de NAT y proxies ociosos. Sobre un socket Unix local, si el proceso del
  otro lado muere el socket se cierra y `ReadMessage` devuelve error enseguida.

- [ ] **Step 1: Escribir los tests que fallan**

`internal/daemon/attach_test.go`:

```go
package daemon

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/giuliano/webterm/internal/ptyapi"
	"github.com/giuliano/webterm/internal/store"
)

// dialAttach abre el WebSocket de attach contra el httptest.Server.
func dialAttach(t *testing.T, srv *httptest.Server, id string) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/sessions/" + id + "/attach"
	conn, res, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		if res != nil {
			t.Fatalf("attach falló con status %d: %v", res.StatusCode, err)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// spawnado inserta la fila, spawnea por HTTP y devuelve el id.
func spawnado(t *testing.T, srv *httptest.Server, st *store.Store) string {
	t.Helper()
	rec := nuevaFila(t, st)
	res := postJSON(t, srv.URL+"/sessions", ptyapi.SpawnOpts{
		ID: rec.ID, Shell: rec.Shell, Cwd: rec.Cwd, Cols: 80, Rows: 24,
	})
	defer res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("spawn dio %d", res.StatusCode)
	}
	return rec.ID
}

// leerHasta junta frames binarios hasta encontrar la marca o agotar el plazo.
func leerHasta(t *testing.T, conn *websocket.Conn, marca string, plazo time.Duration) []byte {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(plazo))
	var buf bytes.Buffer
	for {
		typ, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("esperando %q, junté %q: %v", marca, buf.String(), err)
		}
		if typ == websocket.BinaryMessage {
			buf.Write(data)
			if bytes.Contains(buf.Bytes(), []byte(marca)) {
				return buf.Bytes()
			}
		}
	}
}

func TestAttachMandaReadyDespuesDelReplay(t *testing.T) {
	srv, st := newTestDaemon(t)
	id := spawnado(t, srv, st)

	conn := dialAttach(t, srv, id)
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		typ, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		if typ == websocket.TextMessage {
			if !strings.Contains(string(data), `"ready"`) {
				t.Fatalf("primer frame de texto = %q; quería ready", data)
			}
			return
		}
		// Los binarios de antes de ready son el replay: está bien que haya.
	}
}

func TestAttachEcoDeInput(t *testing.T) {
	srv, st := newTestDaemon(t)
	id := spawnado(t, srv, st)
	conn := dialAttach(t, srv, id)

	if err := conn.WriteMessage(websocket.BinaryMessage, []byte("echo MARCA-ECO\n")); err != nil {
		t.Fatal(err)
	}
	// El eco del shell trae la marca dos veces (el tipeo y la salida); con
	// encontrarla alcanza para saber que el input llegó al pty.
	leerHasta(t, conn, "MARCA-ECO", 5*time.Second)
}

func TestAttachResizeLlegaAlPty(t *testing.T) {
	srv, st := newTestDaemon(t)
	id := spawnado(t, srv, st)
	conn := dialAttach(t, srv, id)

	if err := conn.WriteJSON(map[string]any{"type": "resize", "rows": 30, "cols": 100}); err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteMessage(websocket.BinaryMessage, []byte("stty size\n")); err != nil {
		t.Fatal(err)
	}
	leerHasta(t, conn, "30 100", 5*time.Second)

	// El resize también se persiste: es lo que hace que una sesión reanudada
	// vuelva con las dimensiones que tenía.
	esperar(t, 2*time.Second, "cols persistidas", func() bool {
		rec, err := st.GetSession(id)
		return err == nil && rec.Cols == 100 && rec.Rows == 30
	})
}

// Cuando el pty muere, el stream se corta. Para ese momento la fila ya tiene
// que decir exited: es la garantía sobre la que el orquestador arma el frame
// exit que ve el browser.
func TestAttachCierraAlMorirLaSesionYLaFilaYaEstaMarcada(t *testing.T) {
	srv, st := newTestDaemon(t)
	id := spawnado(t, srv, st)
	conn := dialAttach(t, srv, id)

	if err := conn.WriteMessage(websocket.BinaryMessage, []byte("exit\n")); err != nil {
		t.Fatal(err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			break
		}
	}

	rec, err := st.GetSession(id)
	if err != nil {
		t.Fatal(err)
	}
	if rec.PtyStatus != store.StatusExited {
		t.Fatalf("al cerrarse el stream la fila decía %s; quería exited", rec.PtyStatus)
	}
}

func TestAttachASesionNoVivaDa410(t *testing.T) {
	srv, st := newTestDaemon(t)
	rec := nuevaFila(t, st) // fila sin spawnear: existe pero no tiene proceso

	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/sessions/" + rec.ID + "/attach"
	_, res, err := websocket.DefaultDialer.Dial(url, nil)
	if err == nil {
		t.Fatal("tendría que haber fallado")
	}
	if res == nil || res.StatusCode != http.StatusGone {
		t.Fatalf("status = %v; quería 410", res)
	}
}

func TestDosClientesVenElMismoOutput(t *testing.T) {
	srv, st := newTestDaemon(t)
	id := spawnado(t, srv, st)

	a := dialAttach(t, srv, id)
	b := dialAttach(t, srv, id)

	if err := a.WriteMessage(websocket.BinaryMessage, []byte("echo MARCA-FANOUT\n")); err != nil {
		t.Fatal(err)
	}
	leerHasta(t, a, "MARCA-FANOUT", 5*time.Second)
	leerHasta(t, b, "MARCA-FANOUT", 5*time.Second)
}
```

Sumá este helper a `server_test.go` (lo usan las dos suites):

```go
// esperar reintenta cond hasta que sea verdadera o se agote el plazo. Los ptys
// son asincrónicos: un sleep fijo es la receta de un test que falla una vez
// cada veinte.
func esperar(t *testing.T, plazo time.Duration, motivo string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(plazo)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout esperando: %s", motivo)
}
```

- [ ] **Step 2: Correr los tests para verificar que fallan**

Run: `go test ./internal/daemon/ -run TestAttach -v 2>&1 | head -20`
Expected: FAIL — el stub devuelve 501, así que el dial falla con
`bad handshake` / status 501.

- [ ] **Step 3: Implementar**

`internal/daemon/attach.go`:

```go
package daemon

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// writeTimeout acota cuánto esperamos por una escritura al socket interno.
const writeTimeout = 10 * time.Second

// upgrader del socket interno.
//
// CheckOrigin siempre true porque del otro lado no hay un browser: es un
// socket Unix con permisos 0600, así que no existe el ataque que el chequeo de
// origin previene (una página cualquiera usando la cookie de la víctima).
var upgrader = websocket.Upgrader{
	ReadBufferSize:  4 * 1024,
	WriteBufferSize: 32 * 1024,
	CheckOrigin:     func(*http.Request) bool { return true },
}

// resizeMsg es el único mensaje de control que manda el cliente.
type resizeMsg struct {
	Type string `json:"type"`
	Rows uint16 `json:"rows"`
	Cols uint16 `json:"cols"`
}

// handleAttach conecta un cliente al pty.
//
// El framing es el mismo que el orquestador le habla al browser, a propósito:
// así reenviar es copiar frames y no traducir.
func (s *Server) handleAttach(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	// Attacheamos ANTES del upgrade para poder contestar un status HTTP claro
	// (410 si no está viva) en vez de abrir el socket y cerrarlo enseguida.
	att, err := s.pty.Attach(id)
	if err != nil {
		writeError(w, err)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		att.Detach()
		log.Printf("daemon [%s]: upgrade falló: %v", id, err)
		return
	}
	defer func() {
		att.Detach()
		_ = conn.Close()
	}()

	c := &wsWriter{conn: conn}

	// Handshake: primero el replay, después ready, y recién ahí el stream vivo.
	if hist := att.History(); len(hist) > 0 {
		if err := c.write(websocket.BinaryMessage, hist); err != nil {
			return
		}
	}
	if err := c.writeJSON(map[string]string{"type": "ready"}); err != nil {
		return
	}

	// pty -> cliente
	go func() {
		for chunk := range att.Output() {
			if err := c.write(websocket.BinaryMessage, chunk); err != nil {
				break
			}
		}
		// Si al cliente lo expulsamos por lento hay que decírselo: el cierre
		// del stream solo, sin esto, se confunde con el fin de la sesión.
		if att.Dropped() {
			_ = c.writeJSON(map[string]string{"type": "dropped"})
		}
		// Cerrar el socket destraba el ReadMessage del loop de abajo.
		_ = conn.Close()
	}()

	// cliente -> pty. Sin deadline de lectura: no hay keepalive en el socket
	// interno porque si el proceso del otro lado muere, el socket se cierra y
	// esto devuelve error enseguida.
	for {
		typ, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if typ == websocket.TextMessage && len(data) > 0 && data[0] == '{' {
			var msg resizeMsg
			if err := json.Unmarshal(data, &msg); err == nil && msg.Type == "resize" {
				if err := att.Resize(msg.Rows, msg.Cols); err != nil {
					log.Printf("daemon [%s]: resize falló: %v", id, err)
				}
				continue
			}
		}
		if err := att.Write(data); err != nil {
			log.Printf("daemon [%s]: write al pty falló: %v", id, err)
		}
	}
}

// wsWriter serializa las escrituras: gorilla no admite writers concurrentes y
// acá escriben el bombeo del pty y el handshake.
type wsWriter struct {
	mu   sync.Mutex
	conn *websocket.Conn
}

func (c *wsWriter) write(msgType int, data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	return c.conn.WriteMessage(msgType, data)
}

func (c *wsWriter) writeJSON(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.write(websocket.TextMessage, data)
}
```

Borrar el stub de `handleAttach` en `server.go`.

- [ ] **Step 4: Correr los tests para verificar que pasan**

Run: `go test ./internal/daemon/ -race -v`
Expected: PASS, las dos suites.

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/
git commit -m "feat(daemon): attach por WebSocket sobre el socket interno

El framing es el mismo que el orquestador le habla al browser, así
reenviar es copiar frames y no traducir.

No manda attached (lleva metadata que el daemon no conoce) ni exit
(cuando el stream se corta la fila ya dice exited, porque reap hace
MarkExited antes de cerrar el hub). Tampoco hay ping/pong: el keepalive
existe para NAT y proxies ociosos, y sobre un socket Unix la muerte del
otro proceso cierra el socket enseguida.

El frame dropped sí hace falta: sin él, la expulsión de un cliente lento
se confunde con el fin de la sesión."
```

---

### Task 8: `internal/daemonclient` — el cliente del socket

**Files:**
- Create: `internal/daemonclient/client.go`
- Create: `internal/daemonclient/attach.go`
- Create: `internal/daemonclient/client_test.go`

**Interfaces:**
- Consumes: `ptyapi.*`, `daemon.Info`, `daemon.ProtocolVersion`, `gorilla/websocket`.
- Produces (`*Client` satisface `ptyapi.Client`):
  - `func New(socketPath string) *Client`
  - `func (c *Client) Info() (daemon.Info, error)`
  - `func (c *Client) Spawn(o ptyapi.SpawnOpts) error`
  - `func (c *Client) Attach(id string) (ptyapi.Attachment, error)`
  - `func (c *Client) Kill(id string) error`
  - `func (c *Client) LiveIDs() ([]string, error)`
  - `func (c *Client) Close() error`
  - `var ErrProtocolMismatch = errors.New("...")`
  - `func (c *Client) Check() error` — `Info()` + comparación de versión

- [ ] **Step 1: Escribir los tests que fallan**

`internal/daemonclient/client_test.go`:

```go
package daemonclient

import (
	"bytes"
	"context"
	"errors"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/giuliano/webterm/internal/daemon"
	"github.com/giuliano/webterm/internal/ptyapi"
	"github.com/giuliano/webterm/internal/session"
	"github.com/giuliano/webterm/internal/store"
)

// newPair levanta un daemon de verdad sobre un socket temporal y devuelve un
// cliente apuntado a él. Sin mocks: lo que se está probando es exactamente que
// los dos lados hablen el mismo protocolo.
func newPair(t *testing.T) (*Client, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	m := session.NewManager(st, session.Config{HistoryBytes: 64 << 10})
	srv := daemon.NewServer(m)
	sock := filepath.Join(dir, "d.sock")
	go func() { _ = srv.Serve(sock) }()

	for i := 0; i < 200; i++ {
		if c, err := net.Dial("unix", sock); err == nil {
			_ = c.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	cl := New(sock)
	t.Cleanup(func() {
		_ = cl.Close()
		_ = srv.Shutdown(context.Background())
		_ = m.Close()
		_ = st.Close()
	})
	return cl, st
}

func filaNueva(t *testing.T, st *store.Store) *store.Session {
	t.Helper()
	rec := &store.Session{
		ID: store.NewID(), Cwd: t.TempDir(), Shell: "/bin/sh",
		Cols: 80, Rows: 24, PtyStatus: store.StatusStarting,
	}
	if err := st.CreateSession(rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestCheckAceptaLaMismaVersion(t *testing.T) {
	cl, _ := newPair(t)
	if err := cl.Check(); err != nil {
		t.Fatalf("Check contra un daemon de la misma versión falló: %v", err)
	}
}

func TestClienteImplementaElContrato(t *testing.T) {
	var _ ptyapi.Client = (*Client)(nil)
}

func TestSpawnAttachEscribirYLeer(t *testing.T) {
	cl, st := newPair(t)
	rec := filaNueva(t, st)

	if err := cl.Spawn(ptyapi.SpawnOpts{
		ID: rec.ID, Shell: rec.Shell, Cwd: rec.Cwd, Cols: 80, Rows: 24,
	}); err != nil {
		t.Fatal(err)
	}

	att, err := cl.Attach(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer att.Detach()

	if err := att.Write([]byte("echo MARCA-REMOTA\n")); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	deadline := time.After(5 * time.Second)
	for !bytes.Contains(buf.Bytes(), []byte("MARCA-REMOTA")) {
		select {
		case chunk, ok := <-att.Output():
			if !ok {
				t.Fatalf("el stream cerró antes de la marca: %q", buf.String())
			}
			buf.Write(chunk)
		case <-deadline:
			t.Fatalf("timeout; junté %q", buf.String())
		}
	}
}

// El historial llega ANTES del primer chunk vivo y como History(), no como
// output: si no, el orquestador no sabría dónde termina el replay.
func TestAttachTraeElHistorialAparte(t *testing.T) {
	cl, st := newPair(t)
	rec := filaNueva(t, st)
	if err := cl.Spawn(ptyapi.SpawnOpts{
		ID: rec.ID, Shell: rec.Shell, Cwd: rec.Cwd, Cols: 80, Rows: 24,
		Banner: "MARCA-BANNER",
	}); err != nil {
		t.Fatal(err)
	}

	att, err := cl.Attach(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer att.Detach()

	if !bytes.Contains(att.History(), []byte("MARCA-BANNER")) {
		t.Fatalf("History() = %q; quería el banner adentro", att.History())
	}
}

func TestLiveIDsYKill(t *testing.T) {
	cl, st := newPair(t)
	rec := filaNueva(t, st)
	if err := cl.Spawn(ptyapi.SpawnOpts{
		ID: rec.ID, Shell: rec.Shell, Cwd: rec.Cwd, Cols: 80, Rows: 24,
	}); err != nil {
		t.Fatal(err)
	}

	ids, err := cl.LiveIDs()
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != rec.ID {
		t.Fatalf("LiveIDs = %v; quería [%s]", ids, rec.ID)
	}

	if err := cl.Kill(rec.ID); err != nil {
		t.Fatal(err)
	}
	ids, _ = cl.LiveIDs()
	if len(ids) != 0 {
		t.Fatalf("después del kill LiveIDs = %v; quería vacío", ids)
	}
}

// Los tres errores del contrato tienen que sobrevivir el viaje por el socket:
// el orquestador reacciona distinto a cada uno.
func TestLosErroresDelContratoViajan(t *testing.T) {
	cl, st := newPair(t)

	// Fila inexistente -> ErrNotFound.
	err := cl.Spawn(ptyapi.SpawnOpts{ID: "no-existe", Shell: "/bin/sh", Cwd: t.TempDir(), Cols: 80, Rows: 24})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("spawn sin fila dio %v; quería ErrNotFound", err)
	}

	// Fila sin proceso -> ErrNotLive.
	rec := filaNueva(t, st)
	if _, err := cl.Attach(rec.ID); !errors.Is(err, ptyapi.ErrNotLive) {
		t.Fatalf("attach a fila sin proceso dio %v; quería ErrNotLive", err)
	}
	if err := cl.Kill(rec.ID); !errors.Is(err, ptyapi.ErrNotLive) {
		t.Fatalf("kill a fila sin proceso dio %v; quería ErrNotLive", err)
	}

	// Ya corriendo -> ErrAlreadyLive.
	opts := ptyapi.SpawnOpts{ID: rec.ID, Shell: rec.Shell, Cwd: rec.Cwd, Cols: 80, Rows: 24}
	if err := cl.Spawn(opts); err != nil {
		t.Fatal(err)
	}
	if err := cl.Spawn(opts); !errors.Is(err, ptyapi.ErrAlreadyLive) {
		t.Fatalf("spawn duplicado dio %v; quería ErrAlreadyLive", err)
	}
}

// Cuando la sesión muere, el canal de output se cierra. Es la señal sobre la
// que el orquestador arma el frame exit que ve el browser.
func TestElOutputSeCierraAlMorirLaSesion(t *testing.T) {
	cl, st := newPair(t)
	rec := filaNueva(t, st)
	if err := cl.Spawn(ptyapi.SpawnOpts{
		ID: rec.ID, Shell: rec.Shell, Cwd: rec.Cwd, Cols: 80, Rows: 24,
	}); err != nil {
		t.Fatal(err)
	}
	att, err := cl.Attach(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer att.Detach()

	if err := att.Write([]byte("exit\n")); err != nil {
		t.Fatal(err)
	}

	cerrado := make(chan struct{})
	go func() {
		for range att.Output() {
		}
		close(cerrado)
	}()
	select {
	case <-cerrado:
	case <-time.After(5 * time.Second):
		t.Fatal("el canal de output no se cerró al morir la sesión")
	}
}

func TestSinDaemonDelOtroLadoFallaClaro(t *testing.T) {
	cl := New(filepath.Join(t.TempDir(), "no-existe.sock"))
	defer cl.Close()

	if err := cl.Check(); err == nil {
		t.Fatal("sin daemon del otro lado Check tendría que fallar")
	}
}
```

- [ ] **Step 2: Correr los tests para verificar que fallan**

Run: `go test ./internal/daemonclient/ 2>&1 | head -20`
Expected: FAIL de compilación — `undefined: New`.

- [ ] **Step 3: Implementar el cliente**

`internal/daemonclient/client.go`:

```go
// Package daemonclient habla con el daemon por su socket Unix.
//
// Implementa ptyapi.Client igual que el manager en proceso, así que el
// orquestador no sabe cuál de los dos tiene enfrente.
package daemonclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"

	"github.com/gorilla/websocket"

	"github.com/giuliano/webterm/internal/daemon"
	"github.com/giuliano/webterm/internal/ptyapi"
	"github.com/giuliano/webterm/internal/store"
)

// ErrProtocolMismatch dice que el daemon que está corriendo quedó viejo. Es el
// precio de tener un solo binario con dos modos: recompilar no reemplaza al
// proceso que ya corre.
var ErrProtocolMismatch = errors.New("el daemon corriendo habla otra versión del protocolo")

// El host de las URLs es irrelevante —se disca siempre el mismo socket— pero
// http.Client necesita una URL bien formada.
const baseURL = "http://daemon"

// Client es el lado cliente del socket del daemon.
type Client struct {
	socket string
	http   *http.Client
	dialer *websocket.Dialer
}

func New(socketPath string) *Client {
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socketPath)
	}
	return &Client{
		socket: socketPath,
		http:   &http.Client{Transport: &http.Transport{DialContext: dial}},
		dialer: &websocket.Dialer{
			NetDialContext:  dial,
			ReadBufferSize:  4 * 1024,
			WriteBufferSize: 32 * 1024,
		},
	}
}

// Info pregunta quién está del otro lado.
func (c *Client) Info() (daemon.Info, error) {
	var info daemon.Info
	res, err := c.http.Get(baseURL + "/info")
	if err != nil {
		return info, fmt.Errorf("consultando el daemon en %s: %w", c.socket, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return info, fmt.Errorf("el daemon contestó %d a /info", res.StatusCode)
	}
	if err := json.NewDecoder(res.Body).Decode(&info); err != nil {
		return info, fmt.Errorf("respuesta de /info ilegible: %w", err)
	}
	return info, nil
}

// Check verifica que del otro lado haya un daemon que hable nuestro protocolo.
// El orquestador lo corre antes de escuchar: es mejor no arrancar que arrancar
// y fallar raro a mitad de un attach.
func (c *Client) Check() error {
	info, err := c.Info()
	if err != nil {
		return err
	}
	if info.ProtocolVersion != daemon.ProtocolVersion {
		return fmt.Errorf("%w: el daemon (pid %d) habla %d y este binario habla %d",
			ErrProtocolMismatch, info.PID, info.ProtocolVersion, daemon.ProtocolVersion)
	}
	return nil
}

func (c *Client) Spawn(o ptyapi.SpawnOpts) error {
	body, err := json.Marshal(o)
	if err != nil {
		return err
	}
	res, err := c.http.Post(baseURL+"/sessions", "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("spawneando %s: %w", o.ID, err)
	}
	defer res.Body.Close()
	return statusError(res)
}

func (c *Client) Kill(id string) error {
	res, err := c.http.Post(baseURL+"/sessions/"+id+"/kill", "application/json", nil)
	if err != nil {
		return fmt.Errorf("matando %s: %w", id, err)
	}
	defer res.Body.Close()
	return statusError(res)
}

func (c *Client) LiveIDs() ([]string, error) {
	res, err := c.http.Get(baseURL + "/sessions")
	if err != nil {
		return nil, fmt.Errorf("listando sesiones vivas: %w", err)
	}
	defer res.Body.Close()
	if err := statusError(res); err != nil {
		return nil, err
	}
	var out struct {
		IDs []string `json:"ids"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.IDs, nil
}

// Close suelta las conexiones ociosas. No toca las sesiones del daemon: el
// orquestador se va, los ptys se quedan, que es todo el punto de M10.
func (c *Client) Close() error {
	c.http.CloseIdleConnections()
	return nil
}

// statusError reconstruye el error del contrato a partir del status.
//
// Los tres tienen status distintos justamente para poder hacer esto sin
// parsear el texto del mensaje.
func statusError(res *http.Response) error {
	switch res.StatusCode {
	case http.StatusOK, http.StatusNoContent:
		return nil
	case http.StatusNotFound:
		return store.ErrNotFound
	case http.StatusGone:
		return ptyapi.ErrNotLive
	case http.StatusConflict:
		return ptyapi.ErrAlreadyLive
	default:
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 4<<10))
		return fmt.Errorf("el daemon contestó %d: %s", res.StatusCode, bytes.TrimSpace(msg))
	}
}

var _ ptyapi.Client = (*Client)(nil)
```

- [ ] **Step 4: Implementar el attachment remoto**

`internal/daemonclient/attach.go`:

```go
package daemonclient

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/giuliano/webterm/internal/ptyapi"
)

// outBuffer es cuántos chunks se bufferean entre el socket del daemon y quien
// lee acá. El daemon ya expulsa a los clientes lentos por su cuenta; esto es
// el segundo nivel de backpressure, el que protege este proceso.
const outBuffer = 256

const writeTimeout = 10 * time.Second

// Attach abre el WebSocket y consume el handshake antes de devolver: al volver,
// History() ya está completo y Output() es solo stream vivo.
func (c *Client) Attach(id string) (ptyapi.Attachment, error) {
	conn, res, err := c.dialer.Dial("ws://daemon/sessions/"+id+"/attach", nil)
	if err != nil {
		if res != nil {
			defer res.Body.Close()
			if serr := statusError(res); serr != nil {
				return nil, serr
			}
		}
		return nil, fmt.Errorf("attacheando a %s: %w", id, err)
	}

	a := &attachment{conn: conn, out: make(chan []byte, outBuffer), done: make(chan struct{})}
	if err := a.readHandshake(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	go a.pump()
	return a, nil
}

// attachment es una ptyapi.Attachment del otro lado del socket.
type attachment struct {
	conn    *websocket.Conn
	history []byte
	out     chan []byte

	wmu sync.Mutex // gorilla no admite writers concurrentes

	mu      sync.Mutex
	dropped bool

	done     chan struct{}
	doneOnce sync.Once
}

// readHandshake junta los frames binarios hasta el "ready".
//
// Consumirlo acá y no dejárselo al que lee es lo que hace que History() y
// Output() sean cosas distintas: si no, quien consume tendría que saber dónde
// termina el replay, que es exactamente lo que el protocolo ya le dice.
func (a *attachment) readHandshake() error {
	for {
		typ, data, err := a.conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("leyendo el handshake: %w", err)
		}
		switch typ {
		case websocket.BinaryMessage:
			a.history = append(a.history, data...)
		case websocket.TextMessage:
			var msg struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(data, &msg); err == nil && msg.Type == "ready" {
				return nil
			}
		}
	}
}

// pump traduce los frames del socket al canal de output.
func (a *attachment) pump() {
	defer close(a.out)
	for {
		typ, data, err := a.conn.ReadMessage()
		if err != nil {
			return
		}
		switch typ {
		case websocket.BinaryMessage:
			select {
			case a.out <- data:
			case <-a.done:
				return
			}
		case websocket.TextMessage:
			var msg struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(data, &msg); err == nil && msg.Type == "dropped" {
				a.mu.Lock()
				a.dropped = true
				a.mu.Unlock()
			}
		}
	}
}

func (a *attachment) History() []byte       { return a.history }
func (a *attachment) Output() <-chan []byte { return a.out }

func (a *attachment) Dropped() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.dropped
}

func (a *attachment) Write(p []byte) error {
	return a.send(websocket.BinaryMessage, p)
}

func (a *attachment) Resize(rows, cols uint16) error {
	data, err := json.Marshal(map[string]any{"type": "resize", "rows": rows, "cols": cols})
	if err != nil {
		return err
	}
	return a.send(websocket.TextMessage, data)
}

func (a *attachment) send(msgType int, data []byte) error {
	a.wmu.Lock()
	defer a.wmu.Unlock()
	_ = a.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	return a.conn.WriteMessage(msgType, data)
}

// Detach cierra el socket. El daemon ve el cierre y desuscribe al cliente; la
// sesión no se entera.
func (a *attachment) Detach() {
	a.doneOnce.Do(func() { close(a.done) })
	_ = a.conn.Close()
}

var _ ptyapi.Attachment = (*attachment)(nil)
```

Los imports de este archivo son `encoding/json`, `fmt`, `sync`, `time`,
`gorilla/websocket` y `ptyapi`. **No lleva `net/http`**: `statusError` recibe el
`*http.Response` que devuelve el dialer, pero no nombra el tipo acá.

- [ ] **Step 5: Correr los tests para verificar que pasan**

Run: `go test ./internal/daemonclient/ -race -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/daemonclient/
git commit -m "feat(daemonclient): el cliente del socket del daemon

Implementa ptyapi.Client igual que el manager en proceso, así que el
orquestador no sabe cuál tiene enfrente.

Attach consume el handshake antes de devolver: al volver, History() está
completo y Output() es solo stream vivo. Dejárselo al que lee lo
obligaría a saber dónde termina el replay, que es justo lo que el
protocolo ya le dice con el frame ready.

Los tres errores del contrato viajan como status distintos (404, 410,
409) para reconstruirlos sin parsear el texto del mensaje.

Check compara la versión de protocolo: es lo que convierte 'el daemon
que corre quedó viejo' en un mensaje que dice qué hacer."
```

---
### Task 9: `internal/control` — el manager del orquestador

Acá vuelve todo lo que la tarea 5 sacó de `session`, pero resuelto contra la
base directamente y delegando al `ptyapi.Client` solo lo que es pty. Es la
forma concreta del invariante: una feature de contexto nueva se agrega acá y en
`store`, y el daemon no se entera.

**Files:**
- Create: `internal/control/manager.go`
- Create: `internal/control/attach.go`
- Create: `internal/control/manager_test.go`
- Create: `internal/control/lifecycle_test.go`
- Modify: `internal/ptyapi/ptyapi.go` (se muda `SanitizeReplay`)
- Modify: `internal/session/manager.go` (usa `ptyapi.SanitizeReplay`)
- Delete: `docs/superpowers/plans/.m10-tests-a-mover.md`

**Interfaces:**
- Consumes: `ptyapi.Client`, `store.*`, `resources.Cache`.
- Produces:
  - `type Config struct { Shell string; Resources *resources.Cache; ExtraEnv []string; SweepEvery time.Duration }`
  - `func NewManager(st *store.Store, pty ptyapi.Client, cfg Config) *Manager`
  - `func (m *Manager) Start() error` / `Close() error`
  - `Create(CreateOpts) (*store.Session, error)`, `Attach(id) (*Attachment, error)`, `Kill(id) error`, `Restart(id string, cols, rows int) (*store.Session, error)`, `Delete(id) error`
  - `List`, `Get`, `UpdateMeta`, `ListKV`, `SetKV`, `DeleteKV`, `ListResources`, `AddResource`, `DeleteResource`, `Sweep() int`, `LiveCount() int` — mismas firmas que tenía `session.Manager`, para que `server` y `mcp` no cambien sus call sites
  - `var ErrAlreadyRunning = errors.New("la sesión ya está corriendo")`
  - `type LinkedResource struct{…}` (se muda tal cual desde `session`)

- [ ] **Step 1: Mudar `SanitizeReplay` a `ptyapi`**

Las dos puntas la necesitan: el daemon para el tail del ring, el orquestador
para el historial de una sesión muerta que lee de la base. Sacarla de `session`
evita que `control` importe el paquete de ptys solo por una función pura.

Agregar a `internal/ptyapi/ptyapi.go`:

```go
// SanitizeReplay prepara un tail de historial para un cliente nuevo.
//
// El historial está cortado en el cap, así que puede empezar en medio de un
// carácter UTF-8 y arrastrar atributos de color abiertos antes del corte: sin
// esto, el primer renglón del replay sale con basura y con el color de algo
// que el cliente nunca vio empezar.
func SanitizeReplay(p []byte) []byte {
	for len(p) > 0 && p[0]&0xC0 == 0x80 {
		p = p[1:]
	}
	if len(p) == 0 {
		return nil
	}
	return append([]byte("\x1b[0m"), p...)
}
```

Y su test, en `internal/ptyapi/ptyapi_test.go`:

```go
package ptyapi

import (
	"bytes"
	"testing"
)

func TestSanitizeReplay(t *testing.T) {
	if got := SanitizeReplay(nil); got != nil {
		t.Fatalf("vacío dio %q; quería nil", got)
	}
	// Un corte en medio de un carácter multibyte: "ñ" es 0xC3 0xB1, y el tail
	// arranca en el continuation byte.
	got := SanitizeReplay([]byte{0xB1, 'h', 'o', 'l', 'a'})
	if !bytes.HasSuffix(got, []byte("hola")) {
		t.Fatalf("no descartó el byte de continuación: %q", got)
	}
	if !bytes.HasPrefix(got, []byte("\x1b[0m")) {
		t.Fatalf("no antepuso el reset de atributos: %q", got)
	}
}
```

En `internal/session/manager.go`, borrar `sanitizeReplay` y usar
`ptyapi.SanitizeReplay` en `Attach`.

- [ ] **Step 2: Reinstalar los tests del staging contra `control`**

Sacá de `docs/superpowers/plans/.m10-tests-a-mover.md` los ocho tests y
pegalos en `internal/control/manager_test.go` y `lifecycle_test.go`, con estos
cambios mecánicos:

| Antes | Ahora |
|---|---|
| `package session` | `package control` |
| `newTestManager(t)` | el helper nuevo del paso siguiente |
| `m.Create(CreateOpts{…})` | `m.Create(CreateOpts{…})` (igual) |
| `m.Write(id, p)` / `m.Resize(id, r, c)` | `att.Write(p)` / `att.Resize(r, c)` |
| `session.ErrAlreadyRunning` | `ErrAlreadyRunning` |
| `TestSweepMarcaHuerfanas` | ver el test nuevo del paso 3, que lo reemplaza |

Helper para los dos archivos:

```go
// newTestManager arma el orquestador con un manager de ptys EN PROCESO.
//
// Que los tests no levanten un daemon no es un atajo: es la propiedad que da
// ptyapi. El daemon de verdad ya está probado en internal/daemonclient contra
// un socket real; acá lo que se prueba es la lógica del orquestador.
func newTestManager(t *testing.T) (*Manager, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	pty := session.NewManager(st, session.Config{HistoryBytes: 64 << 10})
	m := NewManager(st, pty, Config{Shell: "/bin/sh"})
	t.Cleanup(func() {
		_ = m.Close()
		_ = pty.Close()
		_ = st.Close()
	})
	return m, st
}
```

- [ ] **Step 3: Escribir los tests nuevos**

Agregar a `internal/control/lifecycle_test.go`:

```go
// El sweep se alimenta del daemon, no de un mapa en memoria. Una fila que la
// base cree activa y el cliente de ptys no reporta viva, está muerta.
func TestSweepUsaLoQueReportaElClienteDePtys(t *testing.T) {
	m, st := newTestManager(t)

	// Una fila "viva" que nunca se spawneó: exactamente lo que queda después
	// de que el daemon arranque de nuevo.
	huerfana := &store.Session{
		ID: store.NewID(), Cwd: t.TempDir(), Shell: "/bin/sh",
		Cols: 80, Rows: 24, PtyStatus: store.StatusRunning,
	}
	if err := st.CreateSession(huerfana); err != nil {
		t.Fatal(err)
	}
	// Y una de verdad, que el sweep no debe tocar.
	viva, err := m.Create(CreateOpts{Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	if n := m.Sweep(); n != 1 {
		t.Fatalf("el sweep corrigió %d filas; quería 1", n)
	}

	got, err := st.GetSession(huerfana.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PtyStatus != store.StatusExited {
		t.Fatalf("la huérfana quedó %s; quería exited", got.PtyStatus)
	}
	if got.ExitReason != string(store.ReasonDaemonRestart) {
		t.Fatalf("exit_reason = %s; quería daemon_restart (la fila es anterior al arranque del daemon)",
			got.ExitReason)
	}

	sigue, err := st.GetSession(viva.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sigue.PtyStatus != store.StatusRunning {
		t.Fatalf("el sweep se llevó puesta una sesión viva: %s", sigue.PtyStatus)
	}
}

// Una fila trabada en starting —el orquestador crasheó entre el insert y el
// spawn— también la levanta el sweep. Si no, queda así para siempre.
func TestSweepLevantaFilasTrabadasEnStarting(t *testing.T) {
	m, st := newTestManager(t)
	trabada := &store.Session{
		ID: store.NewID(), Cwd: t.TempDir(), Shell: "/bin/sh",
		Cols: 80, Rows: 24, PtyStatus: store.StatusStarting,
	}
	if err := st.CreateSession(trabada); err != nil {
		t.Fatal(err)
	}

	if n := m.Sweep(); n != 1 {
		t.Fatalf("el sweep corrigió %d filas; quería 1", n)
	}
	got, _ := st.GetSession(trabada.ID)
	if got.PtyStatus != store.StatusExited {
		t.Fatalf("quedó %s; quería exited", got.PtyStatus)
	}
}

// Attach a una sesión muerta no consulta al cliente de ptys: lee el historial
// de la base y devuelve una conexión de solo lectura.
func TestAttachASesionMuertaEsDeSoloLectura(t *testing.T) {
	m, _ := newTestManager(t)
	rec, err := m.Create(CreateOpts{Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	att, err := m.Attach(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := att.Write([]byte("echo MARCA-HISTORIAL\n")); err != nil {
		t.Fatal(err)
	}
	esperarOutput(t, att, "MARCA-HISTORIAL")
	att.Detach()

	if err := m.Kill(rec.ID); err != nil {
		t.Fatal(err)
	}

	muerta, err := m.Attach(rec.ID)
	if err != nil {
		t.Fatalf("attach a sesión muerta tendría que funcionar: %v", err)
	}
	defer muerta.Detach()
	if muerta.Live {
		t.Fatal("Live = true en una sesión muerta")
	}
	if muerta.Output() != nil {
		t.Fatal("una sesión muerta no tiene stream vivo")
	}
	if !bytes.Contains(muerta.History, []byte("MARCA-HISTORIAL")) {
		t.Fatalf("el historial no sobrevivió al kill: %q", muerta.History)
	}
}

func TestKillEsIdempotenteYDistingueInexistente(t *testing.T) {
	m, _ := newTestManager(t)
	rec, err := m.Create(CreateOpts{Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	if err := m.Kill(rec.ID); err != nil {
		t.Fatal(err)
	}
	// Segundo kill: la fila existe y ya está muerta, así que no es un error.
	if err := m.Kill(rec.ID); err != nil {
		t.Fatalf("segundo kill dio %v; quería nil", err)
	}
	// Pero una sesión que no existe sí lo es.
	if err := m.Kill("no-existe"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("kill inexistente dio %v; quería ErrNotFound", err)
	}
}

func TestCreateDejaLaFilaEnRunning(t *testing.T) {
	m, st := newTestManager(t)
	rec, err := m.Create(CreateOpts{Title: "t", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.GetSession(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PtyStatus != store.StatusRunning {
		t.Fatalf("pty_status = %s; quería running", got.PtyStatus)
	}
	if got.Title != "t" {
		t.Fatalf("title = %q; quería t", got.Title)
	}
}

// El spawn fallido no puede dejar la fila trabada en starting: la UI mostraría
// una sesión arrancando para siempre.
func TestCreateConShellInvalidoNoDejaLaFilaEnStarting(t *testing.T) {
	m, st := newTestManager(t)
	m.cfg.Shell = "/no/existe/este/shell"

	rec, err := m.Create(CreateOpts{Cwd: t.TempDir()})
	if err == nil {
		t.Fatal("tendría que fallar")
	}
	// Create devuelve error pero la fila queda, marcada, para que el error se
	// vea en la UI. Buscamos la única fila que haya.
	list, lerr := st.ListSessions()
	if lerr != nil || len(list) != 1 {
		t.Fatalf("esperaba una fila; list=%v err=%v", list, lerr)
	}
	got := list[0]
	if rec != nil && got.ID != rec.ID {
		t.Fatalf("la fila no es la de la sesión creada")
	}
	if got.PtyStatus == store.StatusStarting {
		t.Fatal("la fila quedó trabada en starting")
	}
	if got.ExitReason != string(store.ReasonSpawnFailed) {
		t.Fatalf("exit_reason = %q; quería spawn_failed", got.ExitReason)
	}
}
```

Y el helper de espera sobre un attachment:

```go
// esperarOutput drena el stream hasta encontrar la marca.
func esperarOutput(t *testing.T, att *Attachment, marca string) {
	t.Helper()
	var buf bytes.Buffer
	deadline := time.After(5 * time.Second)
	for {
		select {
		case chunk, ok := <-att.Output():
			if !ok {
				t.Fatalf("el stream cerró antes de %q; junté %q", marca, buf.String())
			}
			buf.Write(chunk)
			if bytes.Contains(buf.Bytes(), []byte(marca)) {
				return
			}
		case <-deadline:
			t.Fatalf("timeout esperando %q; junté %q", marca, buf.String())
		}
	}
}
```

- [ ] **Step 4: Correr los tests para verificar que fallan**

Run: `go test ./internal/control/ 2>&1 | head -20`
Expected: FAIL de compilación — el paquete no existe todavía.

- [ ] **Step 5: Implementar `internal/control/manager.go`**

```go
// Package control es el manager del orquestador.
//
// Resuelve contra la base todo lo que no es un pty —metadata, KV, recursos
// externos— y le delega a un ptyapi.Client lo que sí. Esa división es el
// invariante de M10: una feature de contexto nueva se agrega acá y en store, y
// el daemon no se entera.
package control

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/giuliano/webterm/internal/ptyapi"
	"github.com/giuliano/webterm/internal/resources"
	"github.com/giuliano/webterm/internal/store"
)

// defaultSweepInterval es cada cuánto se verifica que la base y lo que el
// daemon reporta vivo coincidan.
const defaultSweepInterval = 30 * time.Second

// ErrAlreadyRunning lo devuelve Restart sobre una sesión que no murió.
var ErrAlreadyRunning = errors.New("la sesión ya está corriendo")

// ErrUnknownResource se re-exporta para que el servidor traduzca el error a un
// status sin importar el paquete resources.
var ErrUnknownResource = resources.ErrUnknownResource

// resumeBanner queda en el historial para que el replay muestre dónde se cortó
// la sesión anterior. Lo escribe el daemon —llega por SpawnOpts.Banner— porque
// es el único que puede ordenarlo contra el writer del historial.
const resumeBanner = "\r\n\x1b[90m— sesión reanudada —\x1b[0m\r\n"

// Config parametriza el orquestador.
type Config struct {
	Shell string // shell a spawnear; vacío = $SHELL
	// Resources resuelve el estado de los recursos externos linkeados. Si es
	// nil, linkear por URL deja de funcionar pero el resto anda igual.
	Resources *resources.Cache
	// ExtraEnv son variables que se suman al entorno de cada pty. El
	// entrypoint las arma; así el daemon no necesita saber que existe un token.
	ExtraEnv []string
	// SweepEvery es cada cuánto corre la verificación de invariante.
	SweepEvery time.Duration
}

// Manager es el dueño del estado de las sesiones. Los ptys son de otro.
type Manager struct {
	st  *store.Store
	pty ptyapi.Client
	cfg Config

	// daemonStartedAt sirve para distinguir por qué murió una sesión huérfana:
	// si la fila es anterior al arranque del daemon, se la llevó el reinicio;
	// si es posterior, es deriva y el motivo honesto es "huérfana".
	daemonStartedAt int64

	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

func NewManager(st *store.Store, pty ptyapi.Client, cfg Config) *Manager {
	if cfg.SweepEvery <= 0 {
		cfg.SweepEvery = defaultSweepInterval
	}
	return &Manager{st: st, pty: pty, cfg: cfg, stop: make(chan struct{})}
}

// SetDaemonStartedAt lo llama el entrypoint con lo que contestó /info.
func (m *Manager) SetDaemonStartedAt(ms int64) { m.daemonStartedAt = ms }

// Start hace el primer sweep y arranca la verificación periódica.
//
// Tiene que correr ANTES de escuchar: si no, hay una ventana en la que la API
// reporta vivas sesiones que el daemon no tiene.
func (m *Manager) Start() error {
	if n := m.Sweep(); n > 0 {
		log.Printf("reconciliadas %d sesiones que el daemon ya no tiene", n)
	}
	m.wg.Add(1)
	go m.sweepLoop()
	return nil
}

// Close para la verificación periódica y suelta el cliente.
//
// NO mata las sesiones: que sobrevivan al apagado del orquestador es
// literalmente el objetivo de M10.
func (m *Manager) Close() error {
	m.stopOnce.Do(func() { close(m.stop) })
	m.wg.Wait()
	return m.pty.Close()
}

// CreateOpts describe la sesión a crear.
type CreateOpts struct {
	Title       string
	Description string
	Cwd         string
	Cols, Rows  int
}

// Create inserta la fila y le pide al daemon que la spawnee.
//
// El orden importa: session_output tiene FK contra sessions, así que la fila
// tiene que existir antes de que el historial empiece a escribirse. La ventana
// entre las dos cosas es el estado starting.
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
		PtyStatus: store.StatusStarting,
	}
	if err := m.st.CreateSession(rec); err != nil {
		return nil, err
	}

	if err := m.spawn(rec, ""); err != nil {
		return rec, err
	}
	return m.st.GetSession(rec.ID)
}

// spawn le pide el pty al daemon y se asegura de que la fila no quede trabada
// en starting si algo sale mal.
//
// El daemon marca spawn_failed cuando el que falla es el pty, pero si el que
// falla es el transporte —el daemon se cayó entre medio— nadie más lo haría, y
// la UI mostraría una sesión arrancando para siempre.
func (m *Manager) spawn(rec *store.Session, banner string) error {
	err := m.pty.Spawn(ptyapi.SpawnOpts{
		ID: rec.ID, Shell: rec.Shell, Cwd: rec.Cwd,
		Cols: rec.Cols, Rows: rec.Rows, Env: m.cfg.ExtraEnv, Banner: banner,
	})
	if err == nil {
		return nil
	}
	if got, gerr := m.st.GetSession(rec.ID); gerr == nil && got.PtyStatus == store.StatusStarting {
		code := -1
		if merr := m.st.MarkExited(rec.ID, store.ReasonSpawnFailed, &code); merr != nil {
			log.Printf("[%s] no se pudo registrar el spawn fallido: %v", rec.ID, merr)
		}
	}
	return err
}

// Restart spawnea un pty nuevo sobre la misma fila: conserva id, título, cwd,
// KV e historial, y sigue apendeando al mismo historial.
func (m *Manager) Restart(id string, cols, rows int) (*store.Session, error) {
	rec, err := m.st.GetSession(id)
	if err != nil {
		return nil, err
	}
	// La fuente de verdad de "está viva" es el daemon, no la fila: la fila
	// puede estar desactualizada por la ventana entre la muerte y el reap.
	vivas, err := m.pty.LiveIDs()
	if err != nil {
		return nil, err
	}
	for _, vid := range vivas {
		if vid == id {
			return nil, ErrAlreadyRunning
		}
	}

	if cols > 0 {
		rec.Cols = cols
	}
	if rows > 0 {
		rec.Rows = rows
	}
	if err := m.st.MarkStarting(id); err != nil {
		return nil, err
	}
	if err := m.spawn(rec, resumeBanner); err != nil {
		return nil, fmt.Errorf("reanudando %s: %w", id, err)
	}

	log.Printf("[%s] sesión reanudada (%dx%d)", id, rec.Cols, rec.Rows)
	return m.st.GetSession(id)
}

// Kill mata el proceso y conserva la fila y el historial.
//
// Es idempotente sobre una sesión ya muerta —el daemon contesta ErrNotLive y
// eso no es un error acá— pero distingue "ya estaba muerta" de "no existe",
// que es lo que el cliente necesita para saber si mostrar un 404.
func (m *Manager) Kill(id string) error {
	err := m.pty.Kill(id)
	if errors.Is(err, ptyapi.ErrNotLive) {
		_, gerr := m.st.GetSession(id)
		return gerr
	}
	return err
}

// Delete mata el proceso si vive y borra la fila con su KV, sus recursos y su
// historial. Es el acto destructivo explícito, separado de Kill a propósito.
func (m *Manager) Delete(id string) error {
	if err := m.Kill(id); err != nil && !errors.Is(err, store.ErrNotFound) {
		log.Printf("[%s] no se pudo matar antes de borrar; se borra igual: %v", id, err)
	}
	return m.st.DeleteSession(id)
}

// Sweep marca como muertas las filas que la base cree activas y el daemon no
// reporta vivas. Es una verificación de invariante, no el camino principal.
//
// Reemplaza al ReconcileBoot de M2: con un daemon recién arrancado, LiveIDs
// devuelve vacío y esto marca todo lo que había quedado activo. El
// comportamiento viejo, derivado en vez de hardcodeado.
func (m *Manager) Sweep() int {
	activos, err := m.st.ActiveIDs()
	if err != nil {
		log.Printf("sweep: %v", err)
		return 0
	}
	if len(activos) == 0 {
		return 0
	}
	vivas, err := m.pty.LiveIDs()
	if err != nil {
		log.Printf("sweep: no se pudo consultar al daemon: %v", err)
		return 0
	}
	viva := make(map[string]bool, len(vivas))
	for _, id := range vivas {
		viva[id] = true
	}

	n := 0
	for _, id := range activos {
		if viva[id] {
			continue
		}
		if err := m.st.MarkExited(id, m.reasonFor(id), nil); err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				log.Printf("sweep [%s]: %v", id, err)
			}
			continue
		}
		log.Printf("[%s] la base la daba por activa pero el daemon no la tiene", id)
		n++
	}
	return n
}

// reasonFor distingue las dos formas de quedar huérfana. Una sesión creada
// antes de que el daemon arrancara se la llevó el reinicio; una posterior es
// deriva, y decir "daemon_restart" ahí sería mentir.
func (m *Manager) reasonFor(id string) store.ExitReason {
	if m.daemonStartedAt == 0 {
		return store.ReasonDaemonRestart
	}
	rec, err := m.st.GetSession(id)
	if err != nil || rec.CreatedAt < m.daemonStartedAt {
		return store.ReasonDaemonRestart
	}
	return store.ReasonOrphaned
}

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

// LiveCount es cuántas sesiones tienen proceso corriendo ahora mismo.
func (m *Manager) LiveCount() int {
	ids, err := m.pty.LiveIDs()
	if err != nil {
		return 0
	}
	return len(ids)
}

// A partir de acá, todo se resuelve contra la base sin tocar al daemon. Es la
// parte que puede crecer sin costo: agregar una feature de contexto es sumar
// métodos acá.

func (m *Manager) List() ([]*store.Session, error)        { return m.st.ListSessions() }
func (m *Manager) Get(id string) (*store.Session, error)  { return m.st.GetSession(id) }
func (m *Manager) ListKV(id string) (map[string]string, error) { return m.st.ListKV(id) }
func (m *Manager) DeleteKV(id, key string) error          { return m.st.DeleteKV(id, key) }

func (m *Manager) UpdateMeta(id string, p store.MetaPatch) (*store.Session, error) {
	if err := m.st.UpdateMeta(id, p); err != nil {
		return nil, err
	}
	return m.st.GetSession(id)
}

func (m *Manager) SetKV(id, key, value string) error {
	if _, err := m.st.GetSession(id); err != nil {
		return err
	}
	return m.st.SetKV(id, key, value)
}

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

// AddResource linkea un recurso a la sesión. system y type se infieren del
// propio link; se aceptan explícitos como escape hatch.
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
	r := &store.Resource{SessionID: sessionID, System: ref.System, Type: ref.Type, Ref: ref.URL}
	if err := m.st.AddResource(r); err != nil {
		return nil, err
	}
	log.Printf("[%s] recurso linkeado: %s", sessionID, r.Ref)
	return r, nil
}

func (m *Manager) DeleteResource(sessionID string, id int64) error {
	if _, err := m.st.GetSession(sessionID); err != nil {
		return err
	}
	return m.st.DeleteResource(sessionID, id)
}
```

- [ ] **Step 6: Implementar `internal/control/attach.go`**

```go
package control

import (
	"errors"
	"log"

	"github.com/giuliano/webterm/internal/ptyapi"
	"github.com/giuliano/webterm/internal/store"
)

// Attachment es la conexión de un cliente a una sesión.
//
// Session viene de la base y no del daemon: el daemon no conoce títulos ni
// kanban. Es el único dato del handshake que el orquestador no reenvía sino
// que compone.
type Attachment struct {
	Session *store.Session
	// History es el replay que hay que mandar antes del stream vivo.
	History []byte
	// Live dice si hay proceso corriendo. Si es false, la conexión es de solo
	// lectura y Output() devuelve nil.
	Live bool

	inner ptyapi.Attachment
}

func (a *Attachment) Output() <-chan []byte {
	if a.inner == nil {
		return nil
	}
	return a.inner.Output()
}

func (a *Attachment) Write(p []byte) error {
	if a.inner == nil {
		return ptyapi.ErrNotLive
	}
	return a.inner.Write(p)
}

func (a *Attachment) Resize(rows, cols uint16) error {
	if a.inner == nil {
		return ptyapi.ErrNotLive
	}
	return a.inner.Resize(rows, cols)
}

func (a *Attachment) Detach() {
	if a.inner != nil {
		a.inner.Detach()
	}
}

func (a *Attachment) Dropped() bool {
	return a.inner != nil && a.inner.Dropped()
}

// Attach conecta un cliente. Una sesión muerta se attachea igual, en modo
// lectura: así ver su historial no necesita una vista aparte.
func (m *Manager) Attach(id string) (*Attachment, error) {
	rec, err := m.st.GetSession(id)
	if err != nil {
		return nil, err
	}
	if rec.PtyStatus == store.StatusExited {
		return m.readOnly(rec)
	}

	inner, err := m.pty.Attach(id)
	if errors.Is(err, ptyapi.ErrNotLive) {
		// La fila estaba desactualizada: el pty murió y el reap del daemon
		// todavía no la había marcado. No es un error, es el mismo camino de
		// solo lectura al que hubiéramos ido con la fila al día.
		if fresh, ferr := m.st.GetSession(id); ferr == nil {
			rec = fresh
		}
		return m.readOnly(rec)
	}
	if err != nil {
		return nil, err
	}

	if terr := m.st.TouchActive(id); terr != nil {
		log.Printf("[%s] no se pudo actualizar last_active_at: %v", id, terr)
	}
	return &Attachment{Session: rec, History: inner.History(), Live: true, inner: inner}, nil
}

// readOnly arma la conexión de solo lectura sobre el historial de la base. No
// consulta al daemon: una sesión muerta no le compete.
func (m *Manager) readOnly(rec *store.Session) (*Attachment, error) {
	hist, err := m.st.ReadOutput(rec.ID)
	if err != nil {
		return nil, err
	}
	return &Attachment{Session: rec, History: ptyapi.SanitizeReplay(hist)}, nil
}
```

- [ ] **Step 7: Correr los tests y borrar el staging**

Run: `go test ./internal/control/ ./internal/session/ ./internal/ptyapi/ -race`
Expected: PASS.

```bash
rm docs/superpowers/plans/.m10-tests-a-mover.md
```

- [ ] **Step 8: Commit**

```bash
git add internal/control/ internal/ptyapi/ internal/session/
git rm --cached docs/superpowers/plans/.m10-tests-a-mover.md 2>/dev/null || true
git commit -m "feat(control): el manager del orquestador

Resuelve contra la base todo lo que no es un pty y le delega a un
ptyapi.Client lo que sí. Esa división es el invariante de M10: sumar una
feature de contexto es sumar métodos acá, y el daemon no se entera.

El sweep reemplaza a ReconcileBoot. Se alimenta de lo que el daemon
reporta vivo, así que un daemon recién arrancado devuelve vacío y todo
lo que había quedado activo se marca: el comportamiento viejo, derivado
en vez de hardcodeado. Y distingue daemon_restart de orphaned comparando
la fila contra el arranque del daemon, en vez de mentir con un motivo
fijo.

Attach cae al camino de solo lectura también cuando el daemon contesta
ErrNotLive con la fila diciendo running: es la ventana entre la muerte
del pty y el reap, y no es un error."
```

---
### Task 10: `internal/server` y `internal/mcp` — pasar a `*control.Manager`

Casi todo son renombres. La única lógica que cambia es que `Write` y `Resize`
dejan de ser operaciones del manager y pasan por el attachment. Que sea así de
chico es la prueba de que la partición quedó en el lugar correcto.

**Files:**
- Modify: `internal/server/server.go:30,35`
- Modify: `internal/server/sessions.go:42,44,91`
- Modify: `internal/server/resources.go:60`
- Modify: `internal/server/terminal.go` (`closeAfterStream`, `Write`, `Resize`)
- Modify: `internal/mcp/server.go:37`
- Modify: `internal/server/sessions_test.go:21` (`newTestServer`)
- Modify: `internal/mcp/tools_test.go`, `internal/server/*_test.go` (construcción del manager)

**Interfaces:**
- Consumes: todo lo que produjo la tarea 9.
- Produces: `func New(cfg Config, mgr *control.Manager) *Server`

- [ ] **Step 1: Cambiar los tipos**

Sustituciones mecánicas en los archivos de producción:

| Archivo:línea | Antes | Ahora |
|---|---|---|
| `server/server.go:30` | `mgr *session.Manager` | `mgr *control.Manager` |
| `server/server.go:35` | `New(cfg Config, mgr *session.Manager)` | `New(cfg Config, mgr *control.Manager)` |
| `server/sessions.go:42` | `session.ErrAlreadyRunning` | `control.ErrAlreadyRunning` |
| `server/sessions.go:44` | `session.ErrNotLive` | `ptyapi.ErrNotLive` |
| `server/sessions.go:91` | `session.CreateOpts{…}` | `control.CreateOpts{…}` |
| `server/resources.go:60` | `session.ErrUnknownResource` | `control.ErrUnknownResource` |
| `mcp/server.go:37` | `[]*session.LinkedResource` | `[]*control.LinkedResource` |
| `server/terminal.go:155` | `att *session.Attachment` | `att *control.Attachment` |

`internal/mcp/server.go` ya declara una interfaz para su manager, así que no
hay nada más que tocar ahí: es exactamente el desacople que ahora paga.

- [ ] **Step 2: Mover `Write` y `Resize` al attachment en `handleTerminal`**

En `internal/server/terminal.go`, en el loop `browser -> pty`, reemplazar:

```go
				if err := s.mgr.Resize(id, msg.Rows, msg.Cols); err != nil {
					log.Printf("[%s] resize falló: %v", id, err)
				}
				continue
			}
		}
		if err := s.mgr.Write(id, data); err != nil {
			log.Printf("[%s] write al pty falló: %v", id, err)
		}
```

por:

```go
				// Write y Resize pasan por el attachment: en el camino remoto
				// viajan por el mismo socket que el output, así que son parte
				// de la conexión y no operaciones sueltas del manager.
				if err := att.Resize(msg.Rows, msg.Cols); err != nil {
					log.Printf("[%s] resize falló: %v", id, err)
				}
				continue
			}
		}
		if err := att.Write(data); err != nil {
			log.Printf("[%s] write al pty falló: %v", id, err)
		}
```

Y en el bombeo `pty -> browser`, `att.Output` pasa a ser `att.Output()`.

- [ ] **Step 3: Actualizar los helpers de test**

En `internal/server/sessions_test.go:21`, `newTestServer` devuelve
`*control.Manager` y lo construye con el manager de ptys en proceso:

```go
func newTestServer(t *testing.T) (*httptest.Server, *control.Manager) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	pty := session.NewManager(st, session.Config{HistoryBytes: 64 << 10})
	mgr := control.NewManager(st, pty, control.Config{Shell: "/bin/sh"})
	srv := httptest.NewServer(New(Config{}, mgr).Handler())
	t.Cleanup(func() {
		srv.Close()
		_ = mgr.Close()
		_ = pty.Close()
		_ = st.Close()
	})
	return srv, mgr
}
```

Aplicá el mismo patrón en los helpers de `internal/mcp/tools_test.go`,
`internal/server/mcp_test.go`, `internal/server/server_test.go`,
`internal/server/auth_test.go` y `internal/server/resources_test.go`.

- [ ] **Step 4: Verificar que TODO compila y pasa**

Run: `go build ./... && go test ./... -race`
Expected: falla solo `cmd/webterm`, que es la tarea 11. Todo `internal/...`
en verde, incluido **`TestSesionSobreviveAlCierreDelSocket`**, que es el test
que define M2 y que esta refactorización no puede romper.

Si ese test falla, pará: significa que la partición rompió la propiedad
central del sistema y hay que revisar el diseño, no el test.

- [ ] **Step 5: Commit**

```bash
git add internal/server/ internal/mcp/
git commit -m "refactor(server,mcp): depender de control.Manager

Casi todo son renombres, que es la prueba de que la partición quedó
donde tenía que quedar: el servidor HTTP y el MCP no sabían nada del
pty, solo del manager.

Lo único que cambia de verdad es que Write y Resize pasan por el
attachment. En el camino remoto viajan por el mismo socket que el
output, así que son parte de la conexión y no operaciones sueltas.

mcp/server.go no necesitó tocarse más allá del tipo porque ya declaraba
una interfaz para su manager."
```

---

### Task 11: `cmd/webterm` — subcomandos, arranque on-demand y handshake

**Files:**
- Modify: `cmd/webterm/main.go`
- Create: `cmd/webterm/daemon.go`
- Create: `cmd/webterm/daemon_test.go`
- Modify: `Makefile`

**Interfaces:**
- Consumes: `daemon.PathsFor`, `daemon.NewServer`, `daemonclient.New`, `control.NewManager`, `server.LoadOrCreateToken`, `session.NewManager`.
- Produces: `func ensureDaemon(paths daemon.Paths, dbPath string, historyBytes int64) (*daemonclient.Client, error)`
- Imports nuevos en `main.go`: `strconv`, `strings`, `internal/daemon`, `internal/daemonclient`, `internal/control`.

**Subcomandos:** `webterm` (orquestador, default), `webterm daemon`,
`webterm daemon status|restart|stop|logs`.

- [ ] **Step 1: Escribir el test que falla**

`cmd/webterm/daemon_test.go`:

```go
package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/giuliano/webterm/internal/daemon"
)

// El arranque on-demand tiene que ser capaz de levantar un daemon de cero y
// que el cliente le hable. Es el camino que corre cada vez que arrancás
// webterm con el daemon caído.
func TestEnsureDaemonLevantaYConecta(t *testing.T) {
	if testing.Short() {
		t.Skip("levanta un proceso de verdad")
	}
	bin := buildBinary(t)

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	paths := daemon.PathsFor(dbPath)

	cl, err := ensureDaemonWith(bin, paths, dbPath, session.DefaultHistoryBytes)
	if err != nil {
		t.Fatalf("ensureDaemon falló: %v", err)
	}
	t.Cleanup(func() {
		_ = cl.Close()
		stopDaemon(paths)
	})

	if err := cl.Check(); err != nil {
		t.Fatalf("Check falló contra el daemon recién levantado: %v", err)
	}

	// Una segunda llamada tiene que reusar el mismo daemon, no levantar otro.
	info1, _ := cl.Info()
	cl2, err := ensureDaemonWith(bin, paths, dbPath, session.DefaultHistoryBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer cl2.Close()
	info2, _ := cl2.Info()
	if info1.PID != info2.PID {
		t.Fatalf("levantó un segundo daemon: pid %d vs %d", info1.PID, info2.PID)
	}

	// Y el log tiene que existir: es donde va a parar todo lo que el daemon
	// diga, porque no tiene terminal.
	if _, err := os.Stat(paths.Log); err != nil {
		t.Fatalf("no se creó el log del daemon: %v", err)
	}
}

// buildBinary compila el binario en un temporal. No se puede usar os.Executable
// desde un test porque apunta al binario del test.
func buildBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "webterm")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("compilando: %v\n%s", err, out)
	}
	return bin
}
```

Agregá `"os/exec"` a los imports.

- [ ] **Step 2: Correr el test para verificar que falla**

Run: `go test ./cmd/webterm/ -run TestEnsureDaemon 2>&1 | head -20`
Expected: FAIL de compilación — `undefined: ensureDaemonWith`.

- [ ] **Step 3: Escribir `cmd/webterm/daemon.go`**

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/giuliano/webterm/internal/daemon"
	"github.com/giuliano/webterm/internal/daemonclient"
	"github.com/giuliano/webterm/internal/session"
	"github.com/giuliano/webterm/internal/store"
)

// daemonReadyTimeout es cuánto esperamos a que el daemon recién spawneado
// conteste en su socket.
const daemonReadyTimeout = 2 * time.Second

// runDaemon es el modo daemon: abre la base, arma el manager de ptys y escucha
// en el socket hasta que lo apaguen.
func runDaemon(dbPath string, historyBytes int64) error {
	paths := daemon.PathsFor(dbPath)

	// El flock es lo que impide dos daemons sobre la misma base. Se mantiene
	// tomado durante toda la vida del proceso: el SO lo suelta solo al morir,
	// así que un crash no deja el lock trabado.
	lock, err := acquireLock(paths.Lock)
	if err != nil {
		return err
	}
	defer lock.Close()

	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	mgr := session.NewManager(st, session.Config{HistoryBytes: historyBytes})
	srv := daemon.NewServer(mgr)

	// Al apagar, matamos los ptys y esperamos el último flush del historial.
	// Lo que quede marcado activo lo corrige el sweep del orquestador.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigs
		log.Print("daemon: apagando, cerrando las sesiones vivas")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		_ = mgr.Close()
		_ = st.Close()
		_ = os.Remove(paths.Socket)
		os.Exit(0)
	}()

	return srv.Serve(paths.Socket)
}

// ensureDaemon devuelve un cliente contra un daemon vivo, levantándolo si hace
// falta. Es lo que corre el orquestador antes de escuchar.
//
// historyBytes se le reenvía al daemon porque el historial es suyo: el flag
// del orquestador sería letra muerta si no se propagara.
func ensureDaemon(paths daemon.Paths, dbPath string, historyBytes int64) (*daemonclient.Client, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("no se pudo resolver el binario propio: %w", err)
	}
	return ensureDaemonWith(self, paths, dbPath, historyBytes)
}

// ensureDaemonWith es ensureDaemon con el binario explícito, para poder
// probarlo sin depender de os.Executable (que en un test apunta al test).
func ensureDaemonWith(bin string, paths daemon.Paths, dbPath string, historyBytes int64) (*daemonclient.Client, error) {
	cl := daemonclient.New(paths.Socket)
	switch err := cl.Check(); {
	case err == nil:
		return cl, nil
	case errors.Is(err, daemonclient.ErrProtocolMismatch):
		// Recompilaste y el daemon que corre quedó viejo. No arrancamos: es
		// mejor decir qué hacer que fallar raro a mitad de un attach.
		_ = cl.Close()
		return nil, fmt.Errorf("%w\ncorré `webterm daemon restart` (mata las sesiones vivas)", err)
	}
	_ = cl.Close()

	if err := spawnDaemon(bin, dbPath, paths, historyBytes); err != nil {
		return nil, err
	}

	cl = daemonclient.New(paths.Socket)
	deadline := time.Now().Add(daemonReadyTimeout)
	for {
		if err := cl.Check(); err == nil {
			return cl, nil
		} else if errors.Is(err, daemonclient.ErrProtocolMismatch) {
			_ = cl.Close()
			return nil, err
		}
		if time.Now().After(deadline) {
			_ = cl.Close()
			return nil, fmt.Errorf("el daemon no respondió en %s; mirá %s", daemonReadyTimeout, paths.Log)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// spawnDaemon lanza el daemon desatado de este proceso.
func spawnDaemon(bin, dbPath string, paths daemon.Paths, historyBytes int64) error {
	logFile, err := os.OpenFile(paths.Log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("abriendo %s: %w", paths.Log, err)
	}
	defer logFile.Close()

	cmd := exec.Command(bin, "daemon", "-db", dbPath,
		"-history-bytes", strconv.FormatInt(historyBytes, 10))
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// Setsid es lo que saca al daemon de nuestro grupo de procesos. Sin esto,
	// el Ctrl-C que le des a la terminal del orquestador le llega también al
	// daemon y mata exactamente lo que M10 existe para salvar.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("levantando el daemon: %w", err)
	}
	// No esperamos al proceso: es un daemon, tiene que sobrevivirnos. Soltarlo
	// lo deja huérfano y lo adopta init/launchd, que es lo que queremos.
	go func() { _ = cmd.Process.Release() }()

	log.Printf("daemon levantado (pid %d); log en %s", cmd.Process.Pid, paths.Log)
	return nil
}

// acquireLock toma un flock exclusivo y no bloqueante.
func acquireLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("abriendo %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("ya hay un daemon para esta base (%s): %w", path, err)
	}
	return f, nil
}

// stopDaemon le manda SIGTERM al daemon de estas rutas.
func stopDaemon(paths daemon.Paths) error {
	cl := daemonclient.New(paths.Socket)
	defer cl.Close()
	info, err := cl.Info()
	if err != nil {
		return fmt.Errorf("no hay daemon corriendo en %s", paths.Socket)
	}
	p, err := os.FindProcess(info.PID)
	if err != nil {
		return err
	}
	if err := p.Signal(syscall.SIGTERM); err != nil {
		return err
	}
	// Esperamos a que el socket deje de aceptar: así `daemon restart` no
	// intenta levantar el nuevo antes de que el viejo suelte el lock.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.Dial("unix", paths.Socket); err != nil {
			return nil
		} else {
			_ = c.Close()
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("el daemon (pid %d) no terminó a tiempo", info.PID)
}
```

- [ ] **Step 4: Reescribir el despacho en `cmd/webterm/main.go`**

Antes del `flag.Parse()` de hoy, meter el despacho de subcomandos:

```go
func main() {
	// El subcomando va antes de los flags: `webterm daemon -db x`, no
	// `webterm -db x daemon`. Es la convención de git y de go, y evita tener
	// que parsear flags dos veces.
	if len(os.Args) > 1 && os.Args[1] == "daemon" {
		os.Args = append(os.Args[:1], os.Args[2:]...)
		runDaemonCommand()
		return
	}
	runOrchestrator()
}

// runDaemonCommand despacha `webterm daemon` y sus subcomandos.
func runDaemonCommand() {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	dbPath := fs.String("db", defaultDBPath(), "base SQLite con el estado de las sesiones")
	historyBytes := fs.Int64("history-bytes", session.DefaultHistoryBytes, "cuánto output se guarda por sesión")

	sub := ""
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-") {
		sub = os.Args[1]
		os.Args = append(os.Args[:1], os.Args[2:]...)
	}
	_ = fs.Parse(os.Args[1:])
	paths := daemon.PathsFor(*dbPath)

	switch sub {
	case "": // `webterm daemon`: corre en foreground
		if err := runDaemon(*dbPath, *historyBytes); err != nil {
			log.Fatal(err)
		}
	case "status":
		cl := daemonclient.New(paths.Socket)
		defer cl.Close()
		info, err := cl.Info()
		if err != nil {
			fmt.Printf("no hay daemon corriendo en %s\n", paths.Socket)
			os.Exit(1)
		}
		vivas, _ := cl.LiveIDs()
		fmt.Printf("daemon pid %d, protocolo %d, %d sesiones vivas\n",
			info.PID, info.ProtocolVersion, len(vivas))
		if info.ProtocolVersion != daemon.ProtocolVersion {
			fmt.Printf("¡atención! este binario habla protocolo %d: hace falta `webterm daemon restart`\n",
				daemon.ProtocolVersion)
		}
	case "stop":
		if err := stopDaemon(paths); err != nil {
			log.Fatal(err)
		}
		fmt.Println("daemon detenido; las sesiones que tenía vivas murieron con él")
	case "restart":
		if err := stopDaemon(paths); err != nil {
			log.Printf("no había daemon que detener: %v", err)
		}
		cl, err := ensureDaemon(paths, *dbPath, *historyBytes)
		if err != nil {
			log.Fatal(err)
		}
		defer cl.Close()
		fmt.Println("daemon reiniciado")
	case "logs":
		data, err := os.ReadFile(paths.Log)
		if err != nil {
			log.Fatal(err)
		}
		os.Stdout.Write(data)
	default:
		log.Fatalf("subcomando desconocido: daemon %s", sub)
	}
}
```

`runOrchestrator` es el `main` de hoy con estos cambios:

```go
	st, err := store.Open(dbPath)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("estado en %s", dbPath)

	// El daemon va antes que todo: sin él no se pueden crear sesiones, y el
	// sweep necesita preguntarle qué tiene vivo.
	paths := daemon.PathsFor(dbPath)
	pty, err := ensureDaemon(paths, dbPath, historyBytes)
	if err != nil {
		log.Fatal(err)
	}

	// historyBytes ya no va acá: el historial lo escribe el daemon, y el flag
	// se le reenvía en ensureDaemon.
	sess.Resources = resources.NewCache(resources.NewRegistry(resources.NewGitHub()))
	if cfg.Token != "" {
		sess.ExtraEnv = append(sess.ExtraEnv, "WEBTERM_TOKEN="+cfg.Token)
	}
	mgr := control.NewManager(st, pty, sess)
	if info, ierr := pty.Info(); ierr == nil {
		// Le decimos cuándo arrancó el daemon para que el sweep pueda
		// distinguir "se la llevó el reinicio" de "deriva".
		mgr.SetDaemonStartedAt(info.StartedAt)
	}
	cfg.MCP = webmcp.New(mgr).Handler()

	// Start hace el primer sweep contra el daemon. Va antes de escuchar: si
	// no, hay una ventana en la que la API reporta vivas sesiones que el
	// daemon no tiene.
	if err := mgr.Start(); err != nil {
		log.Fatal(err)
	}
```

Y en el handler de señales del orquestador, **no** matar las sesiones:

```go
	go func() {
		<-sigs
		log.Print("apagando el orquestador; las sesiones siguen corriendo en el daemon")
		_ = mgr.Close()
		_ = st.Close()
		os.Exit(0)
	}()
```

Reemplazar la resolución del token por la persistida:

```go
	// El env var y el flag siguen teniendo prioridad; si no hay ninguno, el
	// token sale de disco. Generar uno nuevo por arranque invalidaría el
	// WEBTERM_TOKEN inyectado en los ptys que sobrevivieron al reinicio.
	if cfg.Token == "" {
		cfg.Token = os.Getenv("WEBTERM_TOKEN")
	}
	if cfg.Token == "" && !noAuth && !server.IsLoopback(cfg.Addr) {
		cfg.Token, err = server.LoadOrCreateToken(tokenPath(dbPath))
		if err != nil {
			log.Fatal(err)
		}
	}
```

Con:

```go
// tokenPath deja el token al lado de la base, para que una instancia de
// desarrollo tenga el suyo igual que tiene su propio socket.
func tokenPath(dbPath string) string {
	return strings.TrimSuffix(dbPath, filepath.Ext(dbPath)) + ".token"
}
```

`sess` pasa a ser `control.Config` en vez de `session.Config`.

- [ ] **Step 5: Actualizar el Makefile**

```makefile
## run: buildea el frontend y levanta el backend sirviendo web/dist
run: build
	./bin/webterm -addr $(ADDR)

## run-lan: igual que run pero accesible desde la red local (genera token)
run-lan: build
	./bin/webterm -addr 0.0.0.0:$(PORT)

## dev: backend + Vite con HMR (frontend en http://localhost:5173)
dev: build web/node_modules
	./bin/webterm -addr $(ADDR) & \
	npm --prefix web run dev; \
	kill %1
```

**Por qué se va `go run`:** borra el binario al salir, y el orquestador spawnea
el daemon ejecutando `os.Executable()`. El daemon sobreviviría —el inode vive
mientras el proceso lo tenga abierto— pero un `webterm daemon restart`
apuntaría a un archivo que ya no existe.

Agregar también:

```makefile
.PHONY: run run-lan build build-web dev test clean daemon-status daemon-restart daemon-stop

## daemon-status: qué daemon está corriendo y cuántas sesiones tiene
daemon-status: build
	./bin/webterm daemon status

## daemon-restart: reinicia el daemon. MATA LAS SESIONES VIVAS.
daemon-restart: build
	./bin/webterm daemon restart
```

- [ ] **Step 6: Verificación completa**

Run: `go build ./... && go test ./... -race`
Expected: PASS, todo.

- [ ] **Step 7: Verificación manual del comportamiento que define M10**

Esto no lo cubre ningún test automático porque requiere matar y levantar
procesos de verdad. Corré los pasos y anotá el resultado:

```bash
make build
./bin/webterm -addr 127.0.0.1:7799 -db /tmp/m10/webterm.db &
# 1. En el browser: crear una sesión, correr `sleep 600` adentro.
# 2. Matar el orquestador:
kill %1
# 3. El daemon tiene que seguir vivo:
./bin/webterm daemon status -db /tmp/m10/webterm.db
#    → "daemon pid N, protocolo 1, 1 sesiones vivas"
# 4. Levantar el orquestador de nuevo:
./bin/webterm -addr 127.0.0.1:7799 -db /tmp/m10/webterm.db &
# 5. En el browser: la sesión sigue corriendo, con su output presente,
#    y el `sleep` sigue ahí.
```

Expected: la sesión sobrevive al paso 2 y el paso 5 la muestra viva con su
historial. **Si esto no pasa, el milestone no está hecho**, por más que los
tests estén en verde.

- [ ] **Step 8: Commit**

```bash
git add cmd/webterm/ Makefile
git commit -m "feat(cmd): subcomandos del daemon y arranque on-demand

El orquestador levanta el daemon si no está, con setsid para sacarlo de
su grupo de procesos: sin eso, el Ctrl-C de la terminal le llega también
al daemon y mata lo que M10 existe para salvar.

Al apagarse, el orquestador ya no mata las sesiones. El flock impide dos
daemons sobre la misma base y lo suelta el SO al morir, así que un crash
no lo deja trabado.

El Makefile deja go run: borra el binario al salir y el spawn del daemon
usa os.Executable(), así que un daemon restart posterior apuntaría a un
archivo que ya no existe."
```

---

### Task 12: Documentación y verificación final

**Files:**
- Modify: `README.md`
- Modify: `webterm-diseno.md` (marcar M10 como hecho)

- [ ] **Step 1: Actualizar el README**

Cambios concretos:

1. **Estado:** `## Estado: M10`, y marcar `- [x] **M10** — daemon de sesiones.`
2. **El párrafo que dice "el proceso no, porque es hijo suyo"** ya no es cierto.
   Reemplazarlo por la explicación de los dos procesos.
3. **La tabla "Estado de las sesiones"**: `se reinició el backend /
   backend_restart` pasa a `se reinició el daemon / daemon_restart`, y la
   explicación de por qué el pty muere cambia de "es hijo del backend" a "es
   hijo del daemon".
4. **Flags:** documentar que el socket, el lock y el log se derivan del `-db`.
5. **Sección nueva "Arquitectura: daemon y orquestador"** con la tabla de
   subcomandos `daemon status|restart|stop|logs` y la advertencia de que
   `restart` mata las sesiones.
6. **Estructura:** sumar `internal/ptyapi/`, `internal/daemon/`,
   `internal/daemonclient/` y `internal/control/`, y corregir la descripción de
   `internal/session/`.

- [ ] **Step 2: Marcar M10 como hecho en el diseño**

En `webterm-diseno.md`, sección M10, agregar `**Hecho.**` al principio, igual
que M2, M8 y M9. Y actualizar el diagrama de "Arquitectura general", que
todavía muestra un solo proceso Go.

- [ ] **Step 3: Verificación final**

Run: `go build ./... && go test ./... -race && go vet ./...`
Expected: todo en verde.

Run: `grep -rn "backend_restart\|ReconcileBoot\|RunningIDs" --include='*.go' --include='*.md' . | grep -v docs/superpowers`
Expected: sin resultados. Si queda alguno, es una referencia que se olvidó de
actualizar.

- [ ] **Step 4: Commit**

```bash
git add README.md webterm-diseno.md
git commit -m "docs: M10, daemon de sesiones

El README decía que el pty es hijo del backend y que reiniciarlo mata
todas las sesiones. Dejó de ser cierto: es hijo del daemon, y el
orquestador se reinicia sin tocarlas."
```

---

## Notas para quien ejecute esto

**El orden importa.** Las tareas 1-4 son independientes entre sí y se pueden
hacer en cualquier orden, pero 5 depende de 1, 6 y 7 dependen de 5, 8 depende
de 6 y 7, 9 depende de 8, y 10 y 11 dependen de 9. Entre la tarea 2 y la 10 el
repo **no compila entero**: es una refactorización en el medio, y cada tarea
dice qué paquetes tienen que estar verdes en su punto.

**Si `TestSesionSobreviveAlCierreDelSocket` se pone rojo, pará.** Es el test que
define M2 y esta refactorización no puede romperlo. Si falla, el problema está
en el diseño de la partición, no en el test.

**El cutover es destructivo una sola vez.** La primera vez que corras el binario
nuevo como tu webterm de verdad se mueren todas las sesiones vivas: el daemon
todavía no existía para sostenerlas. De ahí en adelante ya no.

**No trabajes adentro de webterm mientras implementás esto.** Vas a reiniciar
el backend decenas de veces y, hasta que la tarea 11 esté hecha, cada reinicio
mata todas las sesiones — incluida aquella en la que estés trabajando.
