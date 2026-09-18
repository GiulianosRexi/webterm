# M2 — Persistencia de sesiones con SQLite

Fecha: 2026-09-17
Estado: aprobado, pendiente de implementación

## Objetivo

Desatar el pty del WebSocket. Hoy (M1) el pty es dueño de la conexión: cerrar la
pestaña mata el proceso. Después de M2 el pty vive en un *session manager* del
backend, identificado por `session_id`, y el WebSocket pasa a ser un cliente
más que se attachea y se desattachea sin afectarlo.

Además, el estado de cada sesión —metadata, KV y el historial de output— se
persiste en SQLite, de modo que sobreviva al reinicio del backend y sirva de
base para M4 (folders), M5 (CLI `webterm`), M6 (hooks de Claude Code) y M7
(dashboard).

## Restricción de base: el pty es hijo del proceso Go

El pty se spawnea con `pty.StartWithSize`, así que su proceso es hijo del
backend. Si el backend muere, mueren todas las sesiones. No existe forma de
reconectarse a un pty huérfano sin un daemon o un doble fork por sesión, y eso
está fuera del alcance de M2.

Esto parte la reconciliación en dos problemas distintos:

- **En caliente:** el backend está vivo y el proceso de una sesión muere. Hay
  que detectarlo y actualizar la DB.
- **En frío:** el backend arrancó de nuevo. Todo lo que la DB diga `running`
  es mentira por definición.

El diseño asume esto de forma explícita. Sesiones que sobrevivan al reinicio
del backend serían un milestone aparte.

## Decisiones tomadas

| Decisión | Elección |
|---|---|
| Sesiones `running` tras reiniciar el backend | Se marcan `exited` con `exit_reason='backend_restart'`. Se conserva historial, título, cwd y KV. Reanudar es una acción manual. |
| Historial persistido por sesión | Cap por bytes: 1 MB. Tabla append-only podada. |
| Alcance de UI | Sidebar con lista de sesiones y ABM. Una terminal visible por vez. Los tabs son M3. |
| Driver de SQLite | `modernc.org/sqlite` (puro Go, sin cgo). |
| Fuente de verdad del historial | SQLite. El ring buffer en memoria es solo caché de lectura/escritura. |

## Arquitectura

```
internal/store/      SQLite: schema, migraciones, CRUD de sesiones, KV, chunks de output
internal/session/    Manager: dueño de los ptys vivos, fan-out a N clientes, reconciliación
internal/terminal/   (existente) + Wait() con exit code y separación entre kill y cierre del fd
internal/server/     (existente) + REST /api/sessions + ws con ?session_id=
```

Reglas de dependencia:

- `store` no conoce a `terminal` ni a `session`. Es una capa de datos pura,
  testeable contra una DB en un directorio temporal.
- `session` es el único paquete que compone `terminal` con `store`.
- `server` no toca `terminal` ni `store` directamente: habla solo con
  `session.Manager`.

```
┌──────────┐   REST + WS    ┌──────────────────────────────────────┐
│ React UI │ ◄────────────► │ server                               │
└──────────┘                │   └── session.Manager                │
                            │         ├── map[id]*liveSession      │
                            │         │     ├── terminal.Session   │
                            │         │     ├── ring buffer (1 MB) │
                            │         │     ├── hub (fan-out)      │
                            │         │     └── output writer      │
                            │         └── store.Store ──► SQLite   │
                            └──────────────────────────────────────┘
```

## Modelo de datos

El esquema se escribe completo desde el día uno, incluso los campos que la UI
de M2 no usa (`folder_id`, `work_status`, `kanban_status`). Son M4, M6 y M7, y
dejarlos afuera obliga a una migración después sin ganar nada ahora.

```sql
CREATE TABLE sessions (
  id             TEXT PRIMARY KEY,
  title          TEXT    NOT NULL DEFAULT '',
  description    TEXT    NOT NULL DEFAULT '',
  folder_id      TEXT,                              -- M4, siempre NULL en M2
  cwd            TEXT    NOT NULL,
  shell          TEXT    NOT NULL,
  cols           INTEGER NOT NULL,
  rows           INTEGER NOT NULL,
  pty_status     TEXT    NOT NULL,                  -- running | exited
  exit_reason    TEXT,                              -- normal | killed | backend_restart | orphaned | spawn_failed
  exit_code      INTEGER,
  work_status    TEXT    NOT NULL DEFAULT 'idle',   -- M6/M7: idle|working|waiting_input|error
  kanban_status  TEXT    NOT NULL DEFAULT 'todo',   -- M7: todo|in_progress|done
  created_at     INTEGER NOT NULL,                  -- unix millis
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
```

**IDs:** ordenables por tiempo de creación, generados en el backend
(timestamp en millis en base36 + 4 bytes de aleatoriedad en hex). Evita
colisiones y que la lista dependa de un `ORDER BY created_at` con empates.

**Migraciones:** tabla `schema_version(version INTEGER)`. `store.Open` aplica
las migraciones pendientes en orden dentro de una transacción. En M2 hay una
sola migración; el mecanismo existe para que M4 y M7 no lo tengan que inventar.

**Pragmas:** `journal_mode=WAL`, `busy_timeout=5000`, `foreign_keys=ON`.
Las escrituras se serializan detrás de un mutex del `Store`; con WAL los
lectores no se bloquean.

**Ubicación:** `~/.webterm/webterm.db`, configurable con el flag `-db`.

## Historial de output

### Escritura

Por cada sesión viva corre un *output writer*: acumula los chunks que lee del
pty y hace un `INSERT` batcheado cada 250 ms o cada 64 KB acumulados, lo que
pase primero. Sin el batch, un `cat` de un archivo grande generaría miles de
inserts por segundo.

Después de cada flush, poda: mientras el total de bytes de la sesión supere
1 MB, borra las filas más viejas. El writer lleva la cuenta de bytes en
memoria (la inicializa con un `SELECT SUM(LENGTH(data))` al arrancar la
sesión, que sobre ≤1 MB es trivial), así la poda es un `DELETE ... WHERE id <= ?`
sin agregaciones en el camino caliente.

### Lectura (replay)

Al attachear, el cliente recibe el historial y después el stream vivo. Si la
sesión está viva, el replay sale del ring buffer en memoria; si está muerta,
de SQLite.

Cortar un stream de ANSI crudo por el medio puede partir una secuencia de
escape o un carácter UTF-8 multibyte. Mitigación:

1. Replayar siempre chunks enteros, nunca cortar uno al medio.
2. Descartar los bytes de continuación UTF-8 (`0x80`–`0xBF`) del inicio.
3. Prefijar `\x1b[0m` para no heredar un atributo colgado.

**Limitación aceptada:** el techo de un historial truncado puede mostrar unos
pocos caracteres de basura. Es cosmético y solo ocurre en sesiones que
superaron el megabyte.

## Ciclo de vida de una sesión

### Crear

`POST /api/sessions` → fila en `sessions` con `pty_status='running'` →
`terminal.New` → registro en el mapa de vivas. Si el spawn falla, la fila
queda `exited` con `exit_reason='spawn_failed'` (así el error es visible en la
UI en vez de perderse en un log).

### Attach / detach

Cada cliente WS se suscribe al *hub* de la sesión y recibe un canal bufferado
(256 chunks). El lector del pty hace broadcast no bloqueante: si el canal de
un suscriptor está lleno, se desconecta a ese cliente con un close de
"demasiado lento". **El pty nunca se bloquea por un cliente.**

Desattachear (cerrar la pestaña) solo quita el suscriptor. La sesión sigue
corriendo y acumulando output en el ring buffer y en la DB.

Varios clientes pueden estar attacheados a la vez:

- **Input:** cualquiera escribe; last-writer-wins, como una sesión compartida.
- **Resize:** last-writer-wins. Se persiste `cols`/`rows` para restaurarlos al
  reanudar.

### Muerte del proceso

Una goroutine dedicada por sesión espera en `cmd.Wait()`. Al volver, obtiene el
exit code, marca la fila `exited` y cierra el ptmx —lo que además destraba al
lector, que puede estar colgado en `Read`.

Es importante que la señal autoritativa sea `cmd.Wait()` y no el EOF del
`Read`: si el shell muere pero un nieto heredó el fd del esclavo del pty, el
`Read` no da EOF nunca, y la sesión quedaría `running` para siempre. Ese es
exactamente el caso de desincronización que M2 tiene que resolver.

### Reanudar

`POST /api/sessions/{id}/restart` spawnea un pty nuevo **sobre la misma fila**:
conserva id, título, descripción, cwd, KV e historial, y sigue apendeando al
mismo historial. Se inyecta un marcador visible en el stream
(`— sesión reanudada —`) para que el replay muestre dónde ocurrió el corte.

Reusar la fila es lo que M6 va a necesitar para colgarle el
`claude --resume <claude_session_id>` usando el KV de la propia sesión.

### Matar vs. borrar

Dos operaciones separadas a propósito, porque "matar sesión" en el roadmap es
ambiguo:

- `POST /api/sessions/{id}/kill`: mata el proceso, conserva la fila y el
  historial. `exit_reason='killed'`.
- `DELETE /api/sessions/{id}`: mata el proceso si vive y borra la fila, su KV
  y su historial (cascada). Acto explícito y destructivo.

## Reconciliación

Cuatro mecanismos, cada uno cubriendo un caso que los otros no ven:

| Caso | Mecanismo | `exit_reason` |
|---|---|---|
| El shell hace `exit` | goroutine en `cmd.Wait()` | `normal` |
| El shell muere pero un nieto retiene el fd del pty | la misma goroutine en `cmd.Wait()` (el EOF del `Read` no llega) | `normal` |
| El backend se reinició | barrido en el boot, antes de aceptar requests: `UPDATE sessions SET pty_status='exited', exit_reason='backend_restart', exited_at=? WHERE pty_status='running'` | `backend_restart` |
| Deriva entre memoria y DB | sweep cada 30 s: toda fila `running` sin entrada en el mapa de vivas se marca `exited` | `orphaned` |

El barrido de boot corre dentro de `store.Open`, antes de que el servidor
escuche, para que no exista una ventana en la que la API reporte sesiones vivas
que no lo están.

El sweep periódico es una verificación de invariante, no el camino principal:
en condiciones normales `cmd.Wait()` siempre gana. Existe para que un bug en el
camino principal se autocorrija en 30 segundos en vez de dejar la UI mintiendo.

## API HTTP

Todas bajo la autenticación por token que ya existe.

| Método | Ruta | Qué hace |
|---|---|---|
| `GET` | `/api/sessions` | lista con estado, título, `last_active_at` |
| `POST` | `/api/sessions` | crea y spawnea: `{title?, description?, cwd?, cols, rows}` |
| `GET` | `/api/sessions/{id}` | detalle + KV |
| `PATCH` | `/api/sessions/{id}` | `{title?, description?, kanban_status?, work_status?}` |
| `POST` | `/api/sessions/{id}/kill` | mata el pty, conserva el historial |
| `POST` | `/api/sessions/{id}/restart` | pty nuevo sobre la misma fila |
| `DELETE` | `/api/sessions/{id}` | borra fila, KV e historial |
| `GET` | `/api/sessions/{id}/kv` | el KV completo |
| `PUT` | `/api/sessions/{id}/kv/{key}` | setea una clave (body: valor crudo) |
| `DELETE` | `/api/sessions/{id}/kv/{key}` | borra una clave |

El KV se expone ya en M2 aunque la UI no lo use: es la superficie exacta que
va a consumir el CLI `webterm set/get state` de M5, y dejarlo probado ahora
convierte M5 en un cliente HTTP y nada más.

**Errores:** JSON `{"error": "..."}` con el status apropiado
(`404` sesión inexistente, `409` operación inválida para el estado actual —
p. ej. `restart` sobre una sesión viva—, `400` body inválido).

## Protocolo WebSocket

`GET /ws/terminal?session_id=<id>` — el `session_id` es obligatorio. Sin él,
`400`: crear sesiones es responsabilidad del endpoint REST, no del upgrade.

| Sentido | Frame | Contenido |
|---|---|---|
| server → browser | texto | `{"type":"attached","session":{…}}` — primer mensaje |
| server → browser | binario | historial (replay) y después output vivo |
| server → browser | texto | `{"type":"ready"}` — terminó el replay, lo que sigue es vivo |
| server → browser | texto | `{"type":"exit","code":N,"reason":"normal"}` |
| browser → server | binario | input crudo |
| browser → server | texto | `{"type":"resize","cols":N,"rows":N}` |

Attachear a una sesión **muerta** manda `attached`, el historial, `ready` y
`exit`. La conexión queda en modo lectura: el input entrante se descarta. Así
"ver el historial de una sesión terminada" sale gratis con el mismo código de
la UI, sin un endpoint ni una vista aparte.

## Frontend

- **Sidebar** con la lista de sesiones: punto de estado (corriendo / terminada),
  título, botón de nueva sesión.
- **Acciones por sesión:** renombrar inline, matar, reanudar, borrar (con
  confirmación).
- **Una terminal visible por vez.** `TerminalView` recibe `sessionId` y se
  remonta con `key={sessionId}`. Los tabs son M3.
- **Última sesión activa** recordada en `localStorage`.
- **Refresco de la lista:** polling de `GET /api/sessions` cada 3 s, más un
  refetch inmediato después de cada acción. Un canal de eventos en tiempo real
  para la lista pertenece a M3, donde los indicadores por tab lo justifican.

Fuera de alcance explícito: tabs, folders, UI de kanban, UI de KV,
descripción editable.

## Testing

TDD, de abajo hacia arriba.

**`internal/store`** (DB en directorio temporal, sin mocks):
- migraciones sobre DB vacía y reaplicación idempotente
- CRUD de sesiones y de KV, cascada del `DELETE`
- append de output y poda al cap de 1 MB
- barrido de boot: sesión `running` preexistente queda `exited` con
  `exit_reason='backend_restart'`

**`internal/session`** (ptys reales, sin mocks):
- attach, detach y reattach: el proceso sobrevive al detach y el reattach
  recibe el replay
- `exit` del shell → `pty_status='exited'` con el exit code correcto
- `kill` → `exit_reason='killed'`, historial intacto
- `restart` reusa la fila y conserva el KV
- fan-out: dos clientes suscriptos reciben el mismo output
- suscriptor lento: se lo desconecta sin frenar al pty
- sweep de huérfanas

**`internal/server`**:
- cada endpoint REST, incluidos los casos de error
- ws sin `session_id` → 400; con un id inexistente → cierre con error
- **end-to-end de la premisa de M2:** abrir ws, correr un comando, cerrar el
  ws, reabrir con el mismo `session_id` y verificar que el output del comando
  aparece en el replay
- los tests de M1 (eco, resize vía `stty size`, aviso de `exit`) siguen
  pasando, adaptados al nuevo flujo de creación

## Dependencias nuevas

`modernc.org/sqlite` — puro Go, sin cgo, así el binario sigue cross-compilando
sin toolchain de C. `mattn/go-sqlite3` es más rápido pero ata el build a cgo,
y el volumen de escritura acá (un batch cada 250 ms por sesión) no justifica
esa atadura.

## Flags nuevos

| Flag | Default | Qué hace |
|---|---|---|
| `-db` | `~/.webterm/webterm.db` | ubicación de la base |
| `-history-bytes` | `1048576` | cap de historial por sesión |

## Riesgos y limitaciones conocidas

1. **Las sesiones no sobreviven al reinicio del backend.** Es una consecuencia
   de que el pty sea hijo del proceso Go, y está asumido explícitamente.
2. **Basura cosmética al tope de un historial truncado**, por cortar ANSI crudo
   en el cap del megabyte.
3. **Resize last-writer-wins** con varios clientes de tamaños distintos: el
   último que se conecte impone el suyo. tmux resuelve esto tomando el mínimo;
   acá no vale la complejidad mientras el caso normal sea un cliente.
4. **El polling de 3 s** deja la lista desactualizada hasta 3 segundos. M3 lo
   reemplaza por un canal de eventos.
