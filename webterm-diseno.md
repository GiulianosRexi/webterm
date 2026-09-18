# WebTerm — Diseño y Roadmap

## Objetivo

Reemplazar el uso de una terminal de escritorio (ej. cmux) por una solución propia:
una computadora de desarrollo (Mac mini) corriendo sesiones de terminal —incluyendo
`claude code`— controladas desde una web UI accesible desde cualquier cliente
(ej. MacBook Pro), sin la sobrecarga de tmux ni el consumo de memoria de Electron
por sesión.

## Stack

- **Backend:** Go. Expone HTTP/WebSocket, orquesta los procesos de terminal,
  persiste estado.
- **Frontend:** React "plano" (sin Next.js — no hace falta SSR/routing de servidor
  para este caso), buildeado como estático (`dist/` de Vite) y servido por el
  mismo servidor Go con un file server apuntando a esa carpeta — sin `embed`,
  total al ser uso personal alcanza con `go run` y listo.
- **Terminal en el browser:** `xterm.js` — soporta true color, mouse events,
  apps curses (vim, tmux, claude code), es el estándar (lo usa VS Code).
- **PTY en el backend:** `creack/pty` (equivalente Go de `node-pty`) para spawnear
  shells reales con pseudo-terminal.

## Arquitectura general

```
┌─────────────┐        WebSocket         ┌──────────────────────┐
│  React UI   │ ◄──────────────────────► │      Go backend       │
│  (xterm.js) │                          │                       │
└─────────────┘                          │  ┌─────────────────┐  │
                                          │  │ session manager  │  │
                                          │  │ (map[id]*Session)│  │
                                          │  └────────┬────────┘  │
                                          │           │           │
                                          │      creack/pty       │
                                          │           │           │
                                          │      shell / claude   │
                                          └───────────────────────┘
```

Principio central: el **pty vive en el proceso del backend Go**, no depende del
WebSocket. Cerrar la pestaña del browser solo cierra el socket — el proceso
sigue corriendo y emitiendo output al backend igual. No se usa tmux: la
persistencia frente a desconexión de clientes la resuelve el propio backend.

### Redibujado al reconectar (sin tmux, sin xterm-headless)

Como el backend es Go (no Node, donde existiría `@xterm/headless` +
`addon-serialize`), la forma más simple de mostrarle a un cliente nuevo el
estado actual de la pantalla es mantener un **ring buffer de los últimos N KB
de output crudo por sesión**. Al conectar un cliente:

1. Se le manda el contenido completo del ring buffer.
2. `xterm.js` en el browser parsea las secuencias ANSI y termina redibujando
   la pantalla correcta (igual que si el pty nunca se hubiera desconectado).
3. Se lo suscribe al stream en vivo.

No hace falta mantener un emulador de terminal corriendo en el servidor.

### Consumo de memoria / pausa de sesiones

- El buffer de scrollback en memoria: limitarlo (ring buffer acotado, no todo
  el historial).
- El cliente no debería instanciar un `xterm.js`/DOM completo por cada sesión
  abierta — solo para la que está visible; el resto queda como buffer en el
  servidor.
- Para el consumo real del proceso (shell + `claude code`), no existe
  checkpoint-a-disco real en macOS (eso es CRIU, Linux-only). La estrategia
  elegida es aplicativa: matar el proceso y persistir el `session_id` de
  Claude Code para poder hacer `claude --resume <id>` más tarde (detalle en
  el milestone de integración con Claude).

## Milestones

### M1 — MVP: terminal web básica

- Backend Go: endpoint WebSocket que spawnea un pty (`creack/pty` + shell)
  por conexión.
- Frontend React con `xterm.js` conectado a ese WebSocket.
- Sin persistencia: cerrar la pestaña mata la sesión.
- Objetivo: validar el pipeline completo (input/output, resize, true color,
  mouse) end-to-end.

### M2 — Persistencia de sesiones + ABM

- El pty deja de estar atado al WebSocket: vive en un `session manager` en
  el backend, identificado por `session_id`.
- Ring buffer de output por sesión para redibujar al reconectar.
- Endpoints de ABM:
  - Crear sesión nueva.
  - Listar sesiones activas (con su estado: corriendo / pausada).
  - Conectarse (attach) a una sesión existente.
  - Matar sesión.
- Cerrar la pestaña del browser ya no mata el proceso.

**Hecho.** El diseño detallado —esquema SQLite, cap del historial y los cuatro
mecanismos de reconciliación— está en
`docs/superpowers/specs/2026-09-17-m2-persistencia-sesiones-design.md`.

### M3 — UI multi-terminal

- Tabs (u otro mecanismo) en el frontend para tener varias sesiones visibles
  y poder switchear entre ellas sin perder las demás.
- Indicador de actividad/estado por tab (al menos: activa / con output nuevo
  no visto).

### M4 — Organización: folders

- Agrupar sesiones en folders/carpetas para ordenarlas (por proyecto, por
  tipo de trabajo, etc.).
- Esto es un cambio principalmente de modelo de datos (sesión → folder) y de
  UI (árbol o sidebar de navegación), no de la capa de transporte.

### M5 — Interfaz local (`webterm` CLI)

Binario chico (Go) que corre **dentro** de una sesión y le habla al backend,
para que los programas que corren ahí puedan interactuar con el sistema:

```
webterm set state <clave>=<valor>
webterm get state <clave>
webterm spawn -- <cmd>
```

- Identifica la sesión actual vía una env var que el backend inyecta al
  spawnear el pty (`WEBTERM_SESSION_ID`).
- Habla con el backend por socket Unix local o HTTP a `localhost`.
- `set state` / `get state`: KV persistido por sesión (mismo mecanismo que
  el "contexto persistido" de M7).
- `spawn`: le pide al backend que cree una sesión **nueva e independiente**
  (no hija del proceso que la pidió) — así sobrevive aunque se cierre la
  sesión que la originó. Útil para handoffs entre agentes
  (`webterm spawn -- claude -p "instrucciones..."`).

### M6 — Integración con Claude Code (hooks)

Usar los hooks de Claude Code para automatizar lo que hoy se haría a mano:

| Hook | Trigger | Acción vía `webterm` |
|---|---|---|
| `SessionStart` | arranca/resume la sesión | leer `session_id` del JSON de stdin y guardarlo (`webterm set state claude_session_id=...`) |
| `UserPromptSubmit` | se manda un prompt | `work_status=working` |
| `PreToolUse` | va a correr una tool | `work_status=working` |
| `PermissionRequest` | necesita aprobación | `work_status=waiting_input` |
| `Notification` (matcher `idle_prompt`) | idle esperando input | `work_status=waiting_input` |
| `Stop` | terminó de responder | `work_status=idle` |
| `StopFailure` | error de API | `work_status=error` |

Notas:
- El `session_id` de Claude Code **no** está disponible como env var en bash
  (`$CLAUDE_SESSION_ID` no existe) — hay que leerlo del JSON que Claude Code
  manda por stdin al hook.
- Con `claude_session_id` guardado y el `cwd` de la sesión, "pausar" una
  sesión con Claude Code adentro = matar el proceso; "reanudar" =
  `cd <cwd> && claude --resume <claude_session_id>` en un pty nuevo.
- El `session_id` es válido para `--resume` solo desde el mismo directorio
  de proyecto (y sus worktrees) en el que se creó.

### M7 — Estado administrativo, contexto y dashboard

- **`work_status`** (idle / working / waiting_input / error): automático,
  viene de los hooks de M6. Refleja qué está pasando *ahora* en la sesión.
- **`kanban_status`** (todo / in progress / done, u otros): manual/administrativo,
  no tiene nada que ver con Claude Code. Lo gestiona el usuario desde la UI
  (opcionalmente automatizable: `todo → in_progress` en el primer prompt).
- **Contexto persistido**: el mismo KV por sesión de M5/M6, de uso libre.
- **Modelo de datos de sesión** (resumen de todo lo anterior):
  ```
  session {
    id, folder_id, cwd,
    pty_status:     running | paused
    work_status:    idle | working | waiting_input | error
    kanban_status:  todo | in_progress | done
    kv:             {...}
    created_at, last_active_at
  }
  ```
- **Dashboard**: vista de solo lectura sobre esa tabla. Agrupar por
  `kanban_status` da un board tipo kanban; `work_status` da las señales de
  "esto necesita tu atención ahora". Son ortogonales a propósito.
- Pulido general de UI en base a lo anterior.

### M8 — Recursos externos linkeados a una sesión

**Hecho.** El diseño detallado está en
`docs/superpowers/specs/2026-09-18-m8-recursos-linkeados-design.md`.

Poder colgarle a una sesión los recursos externos con los que se relaciona, y
ver su estado sin salir de WebTerm. Se arranca solo con PRs de GitHub, pero el
modelo es genérico para sumar después tickets de Linear, mensajes de Slack, lo
que aparezca.

La razón de fondo: `work_status` dice qué está haciendo Claude *ahora*; el PR
dice dónde está *el trabajo*. Son señales ortogonales, y juntas son lo que hace
útil al dashboard de M7 — "esta sesión tiene 3 comments sin resolver y los
checks en rojo" se ve sin entrar a la terminal.

#### Modelo de datos

Tabla propia, no un campo JSON en `sessions`:

```sql
CREATE TABLE session_resources (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  system     TEXT NOT NULL,   -- gh | linear | slack | ...
  type       TEXT NOT NULL,   -- pr | issue | ticket | message | ...
  ref        TEXT NOT NULL,   -- la URL canónica del recurso
  created_at INTEGER NOT NULL,
  UNIQUE (session_id, ref)
);
CREATE INDEX idx_resources_ref ON session_resources(ref);
```

El motivo de la tabla es la **búsqueda inversa**: cuando algo diga "el PR 123
cambió" hay que resolver qué sesiones se prenden. Con un blob JSON eso es un
scan completo de `sessions` más parsear cada fila; con tabla es un índice. Y no
suma maquinaria: `session_kv` ya es una tabla y el mecanismo de migraciones ya
existe desde M2.

**El link y el estado son cosas distintas.** El link (`system`, `type`, `ref`)
es dato del usuario y es lo único que se persiste acá. El estado traído de
GitHub —checks, comments, approval— es un caché con TTL que vive aparte y
siempre viaja con su `fetched_at`, para que la UI pueda decir "hace 2 min" en
vez de mentir con datos viejos.

#### Estado de un PR

Lo que muestra la card:

| Dato | De dónde sale |
|---|---|
| estado del PR | `state`, `isDraft`, `mergeable` |
| aprobado o no | `reviewDecision` |
| checks | `statusCheckRollup` |
| comments sin resolver | `reviewThreads` filtrando `isResolved == false` |

**Se consulta por GraphQL, no por REST.** No es preferencia: *la cuenta de
comments sin resolver no existe en la API REST*. Los review threads con
`isResolved` solo están en GraphQL. Por REST habría que paginar comments e
inferir el estado, y saldría mal. Una sola query de GraphQL trae las cuatro
filas de la tabla de arriba; el equivalente REST son unas cuatro llamadas que
igual no dan el dato que importa.

#### Credenciales

Se reusa el `gh` CLI que ya está instalado y autenticado en la máquina, en vez
de introducir un secreto propio: `gh api graphql`, o leer el token con
`gh auth token`. Preferible shellear a `gh`, que ya maneja el keyring y el
refresh.

El rate limit no es una restricción real acá: 5000 req/h en GraphQL, y una
query por PR cada 30 s son 120/h. Habría que tener ~40 PRs visibles en
simultáneo para acercarse.

#### Refresco

El polling vive en el **backend**, no en el frontend: el frontend no puede
llamar a GitHub sin que el token termine en el browser. El cliente declara qué
recursos está mirando y el backend consulta y cachea con un TTL corto.

Así "solo cuando está visible" queda como lo que es —una decisión del frontend
sobre *cuándo pedir*— y no como un problema de rate limit. De paso, tres
clientes mirando el mismo PR cuestan una sola llamada a GitHub.

#### UI

Panel desplegable en la sesión con la lista de recursos linkeados. Linkear es
manual: se pega la URL del PR.

**No se construye todavía la abstracción de "una card por `system`/`type`".**
El dato es genérico (`system`, `type`, `ref`, más un `state` opaco que llena el
backend), pero en el frontend hay una sola card de PR, hardcodeada. Diseñar un
sistema de plugins contra una muestra de uno es la forma clásica de errarle a
la abstracción; que la fuerce la segunda integración, con dos casos reales
sobre la mesa.

Los estados de error son parte del alcance, no un detalle: token sin scope,
repo privado, PR borrado, GitHub caído. La card los muestra explícitamente, y
los datos viejos se marcan como viejos. Un check en verde de hace veinte
minutos mostrado como si fuera de ahora es peor que no mostrar nada.

#### Fuera de alcance, pero sin bloquearlo

**Auto-link desde el `cwd`.** La sesión ya sabe su directorio; de ahí salen el
remote de git y la branch, y de ahí el PR abierto. Sería lo que convierte la
feature en algo que no hay que mantener a mano, pero se deja para después: el
modelo de datos tiene que soportarlo sin cambios, nada más. Con el CLI de M5,
`webterm link pr` desde adentro de la sesión es la versión barata de lo mismo.

### M9 — Servidor MCP

**Hecho.**

El backend expone un servidor MCP para que Claude, corriendo dentro de una
sesión, le escriba contexto a esa sesión y le linkee PRs sin salir de ahí.

Hoy `title`, `description`, el KV (M2) y los recursos linkeados (M8) se llenan a
mano desde la UI. Con MCP los llena el agente que está haciendo el trabajo, que
es el que sabe de qué se trata.

**No es lógica nueva.** Las operaciones ya existen en el manager desde M2 y M8;
MCP es un tercer adaptador de protocolo sobre lo mismo, al lado del REST y del
WebSocket.

- **Transporte:** HTTP en `/mcp`, modo `Stateless`, con el SDK oficial de Go.
- **Identidad de sesión:** el header `X-Webterm-Session`, que Claude Code llena
  expandiendo `${WEBTERM_SESSION_ID}` —la variable que el pty ya recibe desde
  M1— en cada request.
- **Auth:** el token de siempre, ahora también por `Authorization: Bearer`. Para
  que esté disponible adentro de la sesión se inyecta `WEBTERM_TOKEN` al pty.
- **Tools:** `set_context`, `get_context`, `set_title`, `link_pr`, `list_links`.
  Ninguna borra nada: deslinkear y borrar siguen siendo decisiones humanas.

El diseño detallado está en
`docs/superpowers/specs/2026-09-18-m9-servidor-mcp-design.md`.
