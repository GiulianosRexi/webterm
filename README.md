# WebTerm

Sesiones de terminal corriendo en una máquina de desarrollo, controladas desde
una UI web. Backend en Go (pty real vía `creack/pty`), frontend React plano con
`xterm.js`, servido como estático por el mismo backend.

El diseño completo y el roadmap por milestones están en
[`webterm-diseno.md`](./webterm-diseno.md).

## Estado: M2

- [x] **M1** — terminal web básica: un pty por conexión WebSocket, input/output,
      resize, true color, mouse.
- [x] **M2** — persistencia de sesiones (SQLite + session manager) y ABM.
- [ ] **M8** — recursos externos linkeados a una sesión (PRs de GitHub). ← próximo
- [ ] **M3** — UI multi-terminal (tabs).
- [ ] **M4** — folders.
- [ ] **M5** — CLI local `webterm`.
- [ ] **M6** — integración con Claude Code (hooks).
- [ ] **M7** — estado administrativo y dashboard.

Los números son ids estables, no orden de ejecución: M8 va antes que M3.

Desde M2 el pty vive en el backend, no en la conexión: cerrar la pestaña solo
cierra el socket. El estado, el KV y el último MB de output de cada sesión
quedan en SQLite, así que sobreviven al reinicio del backend — el proceso no,
porque es hijo suyo, y al arrancar se reconcilian a `exited` conservando el
historial.

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
| `-token` | `$WEBTERM_TOKEN`, o autogenerado | token de acceso |
| `-no-auth` | `false` | no pedir token aunque escuche en la red |
| `-db` | `~/.webterm/webterm.db` | base con el estado de las sesiones |
| `-history-bytes` | `1048576` | cuánto output se guarda por sesión |

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

Desde el teléfono conviene entrar a la URL pelada (`http://192.168.0.250:7788`):
aparece una pantalla de login donde pegás el token. La sesión queda en una
cookie por 30 días, así que se pide una sola vez por dispositivo. El `?token=`
del log es el atajo para saltear ese paso.

Para fijar un token propio en vez del autogenerado:

```bash
WEBTERM_TOKEN='el-que-quieras' make run-lan   # el env var lo mantiene fuera del historial
go run ./cmd/webterm -addr 0.0.0.0:7788 -token el-que-quieras
```

El botón **Salir** de la barra superior borra la cookie de ese dispositivo.

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

Sin mocks: el store corre contra SQLite de verdad y el manager contra ptys
reales. El test que define M2 es `TestSesionSobreviveAlCierreDelSocket`: abre un
WebSocket, corre un comando, **cierra el socket**, y verifica que la sesión
sigue viva y que al reattachear llega el replay con lo de antes.

## Estructura

```
cmd/webterm/          entrypoint y flags
internal/store/       SQLite: sesiones, KV e historial de output
internal/session/     session manager: ptys vivos, fan-out, reconciliación
internal/server/      HTTP, static file server, API REST, WebSocket
internal/terminal/    wrapper del pty (spawn, read/write, resize, wait)
web/                  frontend Vite + React + xterm.js
```

## Estado de las sesiones

El pty es hijo del proceso Go: si el backend muere, mueren todas las sesiones.
Por eso lo que persiste es el *registro* de la sesión, no el proceso. Hay cinco
mecanismos que mantienen la DB sincronizada con la realidad:

| Caso | Cómo se detecta | `exit_reason` |
|---|---|---|
| el shell hace `exit` | `cmd.Wait()` | `normal` |
| el shell muere pero un nieto retiene el pty | `cmd.Wait()` (el `Read` no da EOF nunca) | `normal` |
| se reinició el backend | barrido al abrir la base, antes de escuchar | `backend_restart` |
| deriva entre la DB y las sesiones vivas | sweep cada 30 s | `orphaned` |
| lo mataste vos | `POST /kill` | `killed` |

Reanudar (`POST /restart`) reusa la misma fila: conserva id, título, cwd, KV e
historial, y deja un marcador `— sesión reanudada —` en el stream.

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

El KV ya está expuesto aunque la UI todavía no lo use: es la superficie exacta
que va a consumir `webterm set/get state` en M5.

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

El input viaja como frame binario justamente para que nunca se confunda con un
mensaje de control.

Attachear a una sesión ya terminada manda el historial y `exit`: la conexión
queda de solo lectura, que es como la UI muestra lo que pasó en una sesión
muerta sin necesitar una vista aparte.

Varios clientes pueden estar attacheados a la vez y ven el mismo output; el
input y el resize son last-writer-wins. Al cliente que deja de leer se lo
desconecta en vez de frenar el pty.
