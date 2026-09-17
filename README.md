# WebTerm

Sesiones de terminal corriendo en una máquina de desarrollo, controladas desde
una UI web. Backend en Go (pty real vía `creack/pty`), frontend React plano con
`xterm.js`, servido como estático por el mismo backend.

El diseño completo y el roadmap por milestones están en
[`webterm-diseno.md`](./webterm-diseno.md).

## Estado: M1 (MVP)

- [x] **M1** — terminal web básica: un pty por conexión WebSocket, input/output,
      resize, true color, mouse.
- [ ] **M2** — persistencia de sesiones (session manager + ring buffer) y ABM.
- [ ] **M3** — UI multi-terminal (tabs).
- [ ] **M4** — folders.
- [ ] **M5** — CLI local `webterm`.
- [ ] **M6** — integración con Claude Code (hooks).
- [ ] **M7** — estado administrativo y dashboard.

En M1 la sesión vive atada al WebSocket: cerrar la pestaña mata el proceso.
Eso cambia en M2.

## Correr

Requiere Go ≥ 1.25 y Node ≥ 20.

```bash
make run           # buildea el frontend y levanta el server en :7788
```

Abrir http://127.0.0.1:7788

Para desarrollo del frontend con hot reload:

```bash
make dev           # backend en :7788 + Vite en :5173 (proxea /ws y /api)
```

Flags del backend:

| Flag | Default | Qué hace |
|---|---|---|
| `-addr` | `127.0.0.1:7788` | dirección de escucha |
| `-static` | `web/dist` | carpeta con el build del frontend |
| `-shell` | `$SHELL` | shell a spawnear |

> El default escucha solo en loopback. Para llegar desde otra máquina (ej. la
> MacBook) usá `-addr 0.0.0.0:7788` detrás de Tailscale o un túnel SSH — el
> endpoint todavía no tiene autenticación.

## Tests

```bash
go test ./...
```

Los tests de `internal/server` levantan el servidor, abren una sesión real por
WebSocket y validan el pipeline completo: eco de un comando, propagación del
resize al pty (`stty size`) y el aviso de `exit` al browser.

## Estructura

```
cmd/webterm/          entrypoint y flags
internal/server/      HTTP, static file server, endpoint WebSocket
internal/terminal/    wrapper del pty (spawn, read/write, resize, close)
web/                  frontend Vite + React + xterm.js
```

## Protocolo WebSocket (`/ws/terminal`)

Query params opcionales: `cols`, `rows`, `cwd`.

| Sentido | Frame | Contenido |
|---|---|---|
| browser → server | binario | input crudo del teclado |
| browser → server | texto | `{"type":"resize","cols":N,"rows":N}` |
| server → browser | binario | output crudo del pty (ANSI incluido) |
| server → browser | texto | `{"type":"exit"}` cuando el proceso termina |

El input viaja como frame binario justamente para que nunca se confunda con un
mensaje de control.
