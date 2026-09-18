# WebTerm

Sesiones de terminal corriendo en una máquina de desarrollo, controladas desde
una UI web. Backend en Go (pty real vía `creack/pty`), frontend React plano con
`xterm.js`, servido como estático por el mismo backend.

El diseño completo y el roadmap por milestones están en
[`webterm-diseno.md`](./webterm-diseno.md).

## Estado: M10

- [x] **M1** — terminal web básica: un pty por conexión WebSocket, input/output,
      resize, true color, mouse.
- [x] **M2** — persistencia de sesiones (SQLite + session manager) y ABM.
- [x] **M8** — recursos externos linkeados a una sesión (PRs de GitHub).
- [x] **M9** — servidor MCP para que Claude escriba en su sesión.
- [x] **M10** — daemon de sesiones.
- [ ] **M3** — UI multi-terminal (tabs). ← próximo
- [ ] **M4** — folders.
- [ ] **M5** — CLI local `webterm`.
- [ ] **M6** — integración con Claude Code (hooks).
- [ ] **M7** — estado administrativo y dashboard.

Los números son ids estables, no orden de ejecución: M8 va antes que M3.

Desde M2 el pty no vive en la conexión: cerrar la pestaña solo cierra el
socket, no el proceso. Desde M10 el pty tampoco vive en el mismo proceso que
sirve la UI: es hijo del **daemon**, un segundo proceso aparte del
**orquestador** (todo lo demás — HTTP, API, MCP, recursos, KV). Reiniciar el
orquestador, que es lo que se hace todo el rato mientras se desarrolla sobre
WebTerm, ya no mata nada. El detalle de esa partición está en "Arquitectura:
daemon y orquestador" más abajo. El estado, el KV y el último MB de output de
cada sesión quedan en SQLite, así que sobreviven también a un reinicio del
daemon — el proceso no, y al arrancar se reconcilian a `exited` conservando el
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
| `-mcp-config` | — | imprime cómo registrar el servidor MCP y sale |

El socket, el lock y el log del daemon no son flags propios: se derivan del
path de `-db` (`~/.webterm/webterm.db` da `webterm.sock`, `webterm.lock` y
`webterm.log` al lado; `~/.webterm/dev.db` da su propio trío). No son rutas
fijas a propósito: si lo fueran, levantar una instancia de desarrollo con otro
`-db` conectaría igual al daemon de producción y le spawnearía y mataría
sesiones ajenas — peor que el problema que esta feature vino a resolver. La
derivación está en `internal/daemon/paths.go`.

<!-- PENDIENTE M10: flags y subcomandos del daemon, cuando la tarea 11 fije la superficie -->

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

## Arquitectura: daemon y orquestador

Desde M10 esto no es un solo proceso. El backend se partió en dos:

- el **daemon** es dueño de los ptys y del historial de output, y nada más.
  Es lo que hoy vive en `internal/daemon`, corriendo por encima de
  `internal/session` (spawn, pump, reap, hub, ring, writer), que no cambió de
  lógica, solo de proceso.
- el **orquestador** es todo lo demás: HTTP, la UI estática, la API REST, el
  servidor MCP, los recursos externos, el KV. Su manager,
  `internal/control`, resuelve contra la base directamente todo lo que no es
  un pty (`List`, `Get`, `UpdateMeta`, KV, recursos) y le delega al daemon
  solo `Create`, `Attach`, `Kill` y `Restart`.

Los dos hablan por un socket Unix: `internal/ptyapi` define el contrato
(cuatro métodos: `Spawn`, `Attach`, `Kill`, `LiveIDs`) y `internal/daemonclient`
es la implementación que lo habla de verdad, así que el orquestador no
distingue si del otro lado hay un daemon en otro proceso o —como en los
tests— un `internal/session.Manager` embebido en el mismo.

<!-- PENDIENTE M10: flags y subcomandos del daemon, cuando la tarea 11 fije la superficie -->

El invariante que sostiene la partición: **agregar una feature al orquestador
no tiene que requerir tocar el daemon**. Por eso el daemon no sabe qué es un
título, un token o un PR de GitHub — sumar cualquiera de esas cosas es tocar
`internal/control` y `internal/store`, nunca `internal/daemon`. Es la misma
separación que `dockerd`/`containerd`, por el mismo motivo: que la capa que
cambia seguido no sostenga los procesos que tienen que durar.

La contraparte de esa libertad: **reiniciar el daemon sí mata todas las
sesiones vivas**, porque el pty es hijo suyo. Es un acto explícito y ruidoso,
no algo que pase de rebote reiniciando el orquestador — que es justamente lo
que esta partición vino a comprar.

## Estructura

```
cmd/webterm/            entrypoint, flags y arranque on-demand del daemon
internal/store/         SQLite: sesiones, KV, historial de output y recursos
internal/resources/     providers de sistemas externos (GitHub), caché con TTL
internal/mcp/           servidor MCP: las tools sobre el control manager
internal/control/       manager del orquestador: metadata, KV y recursos contra
                         la base; delega los ptys al daemon
internal/ptyapi/        contrato entre quien tiene los ptys (daemon o manager
                         en proceso) y quien los usa
internal/daemon/        proceso dueño de los ptys: lado servidor del socket
internal/daemonclient/  lado cliente del socket: habla ptyapi contra un daemon
                         remoto
internal/session/       ptys vivos: spawn, pump, reap, hub, ring, writer —
                         vive en el daemon, ya no hace ABM de sesiones
internal/server/        HTTP, static file server, API REST, WebSocket
internal/terminal/      wrapper del pty (spawn, read/write, resize, wait)
web/                    frontend Vite + React + xterm.js
```

## Estado de las sesiones

El pty es hijo del daemon, no del orquestador: si el daemon muere, mueren
todas las sesiones (reiniciar el orquestador no las toca — ver "Arquitectura:
daemon y orquestador"). Por eso lo que persiste en la base es el *registro* de
la sesión, no el proceso. Antes de tener pty el registro pasa por `starting`,
la ventana entre que el orquestador inserta la fila y el daemon confirma el
spawn.

Hay seis mecanismos que mantienen la DB sincronizada con la realidad:

| Caso | Cómo se detecta | `exit_reason` |
|---|---|---|
| el shell hace `exit` | `cmd.Wait()`, en el daemon | `normal` |
| el shell muere pero un nieto retiene el pty | `cmd.Wait()` (el `Read` no da EOF nunca), en el daemon | `normal` |
| el daemon no pudo spawnear el pty | falla `pty.Spawn` al crear o reanudar | `spawn_failed` |
| se reinició el daemon | sweep del orquestador contra lo que el daemon reporta vivo | `daemon_restart` |
| deriva entre la DB y las sesiones vivas | mismo sweep, cada 30 s | `orphaned` |
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
| `GET` | `/api/sessions/{id}/resources` | recursos linkeados, con su estado |
| `POST` | `/api/sessions/{id}/resources` | linkea: `{"ref":"https://github.com/o/r/pull/1"}` |
| `DELETE` | `/api/sessions/{id}/resources/{rid}` | deslinkea |

`kill` y `DELETE` están separados a propósito: matar el proceso no tiene por
qué llevarse el historial.

El KV ya está expuesto aunque la UI todavía no lo use: es la superficie exacta
que va a consumir `webterm set/get state` en M5.

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

## Servidor MCP

El backend expone un servidor MCP en `/mcp` para que Claude, corriendo **dentro**
de una sesión, le escriba contexto a esa sesión y le linkee PRs sin salir de la
terminal. Se registra una sola vez:

```bash
webterm -mcp-config          # imprime el comando con tu host, puerto y token
```

```bash
claude mcp add --transport http webterm http://127.0.0.1:7788/mcp \
  -H 'X-Webterm-Session: ${WEBTERM_SESSION_ID}' \
  -H 'Authorization: Bearer ${WEBTERM_TOKEN}'
```

**Las comillas simples importan.** Claude Code expande esas variables en *cada
request*, contra el entorno del proceso que hace la llamada; si las expandiera
el shell al registrar, quedarían congeladas y todas las sesiones escribirían
sobre la que registró el MCP. Las dos variables las inyecta el backend al pty,
así que dentro de una sesión ya están.

Tools disponibles:

| Tool | Qué hace |
|---|---|
| `set_context` / `get_context` | el contexto persistido de la sesión (el KV) |
| `set_title` | nombra la sesión: es lo que se ve en la lista |
| `link_pr` | linkea un PR de GitHub |
| `list_links` | los recursos linkeados, con su estado |

Ninguna borra nada: deslinkear y borrar sesiones siguen siendo decisiones
humanas, y la UI ya las tiene.

Si Claude corre fuera de una sesión de WebTerm la variable no existe y las tools
lo dicen explícitamente, en vez de fallar con un id que no se entiende.

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
