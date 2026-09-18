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
| `-token` | autogenerado | token de acceso |
| `-no-auth` | `false` | no pedir token aunque escuche en la red |

## Acceso desde otra máquina de la red

```bash
make run-lan       # escucha en 0.0.0.0:7788 y genera un token
```

El log imprime las URLs con el token listo para copiar:

```
webterm escuchando en 0.0.0.0:7788
  → http://127.0.0.1:7788/?token=xxxxxxxx
  → http://192.168.0.250:7788/?token=xxxxxxxx
```

El primer acceso con `?token=` guarda una cookie, así que después alcanza con
la URL pelada. Para fijar un token estable entre reinicios: `-token mi-token`.

### Qué protege y qué no

`/ws/terminal` entrega una shell con tus permisos, así que el token es
obligatorio cuando el server no escucha solo en loopback (`-no-auth` lo
desactiva a propósito). El handshake del WebSocket además exige `Origin` del
mismo host, para que otra página abierta en tu browser no pueda conectarse
usando tu cookie.

Lo que **no** cubre: es HTTP plano, sin TLS. En tu LAN el token y todo lo que
tipeás viajan en claro y cualquiera con acceso a la red puede leerlos. Para
salir de la LAN no expongas el puerto en el router — usá Tailscale (`tailscale
serve` te da HTTPS) o un túnel SSH:

```bash
ssh -N -L 7788:127.0.0.1:7788 giuliano@192.168.0.250
```

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
