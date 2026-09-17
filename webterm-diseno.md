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
