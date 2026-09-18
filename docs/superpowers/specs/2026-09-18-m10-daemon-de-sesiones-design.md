# M10 — Daemon de sesiones

Fecha: 2026-09-18
Estado: aprobado, pendiente de implementación

## Objetivo

Que reiniciar el backend deje de matar las sesiones.

Hoy el pty es hijo del proceso Go, así que cada recompilación se lleva puesto
todo lo que estaba corriendo. Eso convierte cualquier cambio en el orquestador
—una feature de contexto, un provider de recursos nuevo, una tool de MCP— en un
costo: perdés los `claude` a medio trabajo, los builds, los `ssh`, los `tail
-f`. El resultado práctico es que iterás menos de lo que querrías sobre tu
propia herramienta.

El proceso se parte en dos:

- un **daemon** que es dueño de los ptys y del historial de output, y nada más;
- el **orquestador**, que es todo lo demás —HTTP, UI, API REST, MCP, recursos,
  KV— y que puede reiniciarse cuantas veces quieras sin tocar lo que corre.

## La analogía correcta

Es la separación `dockerd` / `containerd`, por el mismo motivo: que la capa que
cambia seguido no sostenga los procesos que tienen que durar.

Docker va un paso más allá y pone un `containerd-shim` **por contenedor**, que
es lo que le permite actualizar `containerd` sin matar nada. Acá eso se
descarta a propósito: Docker lo paga porque actualiza containerd en máquinas
ajenas que no puede reiniciar a voluntad. En una Mac mini personal alcanza con
que reiniciar el daemon sea un acto explícito y ruidoso.

## Alcance

Lo que tiene que sobrevivir: **reiniciar el orquestador**. Nada más.

Queda explícitamente afuera sobrevivir a un reboot de la máquina — ningún
proceso lo hace. Ese caso es M6 (`claude --resume` sobre el `claude_session_id`
persistido), es complementario y no bloquea nada de esto.

Queda afuera también sobrevivir a un upgrade del propio daemon: cambiar código
del daemon mata las sesiones. Es el único modo de falla que la feature no
cubre, y se acepta a cambio de que el daemon sea flaco y casi nunca haya que
tocarlo.

## El invariante que hace que esto valga la pena

> Agregar una feature al orquestador no debe requerir tocar el daemon.

Toda la ganancia depende de eso. Si lógica de contexto, de recursos o de MCP se
filtra al daemon, cada feature nueva vuelve a costar un reinicio destructivo y
la separación no compró nada. Las dos decisiones grandes del diseño —la base
compartida con dueños por tabla y el protocolo de cinco rutas— existen para
sostener este invariante, no por elegancia.

## Topología y ciclo de vida

### Un binario, dos modos

`webterm` sigue siendo el orquestador; `webterm daemon` es el modo daemon. Un
solo artefacto mantiene el build y la distribución como están, y evita dos
binarios que se desincronicen.

Subcomandos nuevos: `webterm daemon` (foreground), `daemon status`, `daemon
restart`, `daemon stop`, `daemon logs`.

### Arranque on-demand

El orquestador intenta conectarse al socket. Si no hay nadie:

1. toma un `flock` sobre el lockfile, para que dos orquestadores levantando a la
   vez no spawneen dos daemons;
2. lanza `webterm daemon` con `SysProcAttr{Setsid: true}` y stdio redirigido al
   log;
3. espera a que el socket conteste, con backoff, hasta ~2 s.

Un `.sock` huérfano de un crash anterior se borra y se reintenta.

**El `setsid` no es cosmético.** Sin él, el daemon queda en el grupo de
procesos del orquestador y el Ctrl-C que le des a la terminal le llega también
al daemon — matando exactamente lo que la feature existe para salvar.

### El daemon no se apaga solo

Nunca por inactividad ni porque se fue el último cliente: eso sería "cerrar el
browser mata las sesiones", que es el bug original con pasos extra. Se apaga
solo con `webterm daemon stop` o SIGTERM, y ahí sí mata los ptys y hace el
último flush del historial.

### Rutas derivadas del path de la base

Las tres rutas del daemon salen del `-db`:

| `-db` | socket | lock | log |
|---|---|---|---|
| `~/.webterm/webterm.db` | `~/.webterm/webterm.sock` | `~/.webterm/webterm.lock` | `~/.webterm/webterm.log` |
| `~/.webterm/dev.db` | `~/.webterm/dev.sock` | `~/.webterm/dev.lock` | `~/.webterm/dev.log` |

Con un `-daemon-socket` como escape hatch.

Esto no es un detalle de nomenclatura: **con rutas fijas, una instancia de
desarrollo en otro puerto se conectaría al daemon de producción** y le
spawnearía y mataría sesiones — peor que el problema que vinimos a resolver.
Derivándolas del `-db` que igual hay que pasar, una instancia de desarrollo
queda aislada de punta a punta sin flags nuevos para acordarse.

### Handshake de versión de protocolo

La trampa del binario único: recompilás, el archivo en disco es nuevo, pero el
daemon que sigue corriendo es el viejo — en Unix reemplazar el archivo no toca
al proceso vivo.

El daemon anuncia un `protocol_version`, una constante que se bumpea **solo**
cuando cambia el protocolo del socket, no en cada build. Si coincide, el
orquestador conecta callado. Si no, se niega a arrancar:

```
el daemon corriendo habla protocolo 1, este binario habla 2.
corré `webterm daemon restart` (mata las sesiones vivas).
```

No es versionado de API —las rutas no llevan `/v1`, y no hace falta porque
cliente y servidor salen del mismo binario y se iteran juntos—. Es detección de
binario viejo corriendo. Sin esto, un daemon stale falla con un error raro a
mitad de un attach en vez de decirte qué hacer.

## Paquetes

| Paquete | Qué pasa |
|---|---|
| `internal/store` | sin cambios; librería compartida, la abren los dos procesos |
| `internal/terminal` | sin cambios; se lo lleva el daemon |
| `internal/session` | queda con lo del pty (`live`, pump, reap, hub, ring, writer); vive en el daemon |
| `internal/control` | **nuevo**: el manager del orquestador |
| `internal/daemon` | **nuevo**: el lado servidor del socket |
| `internal/daemonclient` | **nuevo**: el lado cliente |
| `internal/server` | cambia el tipo de su dependencia; los call sites quedan igual |
| `internal/mcp` | ídem |
| `internal/resources` | sin cambios; se queda en el orquestador |

### La partición de `session.Manager`

Hoy hace dos trabajos: ptys vivos y filas en la base. Se separan.

`internal/session` se queda con el pty. `internal/control` es el manager del
orquestador: tiene el `store`, el caché de recursos y un cliente del daemon.
Resuelve `List`, `Get`, `UpdateMeta`, el KV y los recursos **contra la base
directamente**, y delega al daemon solo `Create`, `Attach`, `Kill` y `Restart`.

Eso es lo que hace que `internal/server` y `internal/mcp` casi no cambien: hoy
reciben un `*session.Manager` y pasan a recibir un `*control.Manager` con los
mismos métodos. Y es la forma concreta del invariante: agregar una feature de
contexto es tocar `control` + `store`, y el daemon ni se entera.

### El seam de tests

`control` se construye con un `PtyClient`, una interfaz de cuatro métodos
(`Spawn`, `Attach`, `Kill`, `LiveIDs`). La implementación real habla por el
socket; una adaptadora envuelve un `*session.Manager` en proceso.

Con eso el suite actual sigue corriendo sin levantar un daemon, y
`TestSesionSobreviveAlCierreDelSocket` —el test que define M2— queda intacto.

**El adaptador no se expone como flag.** Existe para los tests; un "modo
embebido" de usuario sería un segundo modo de correr el producto que hay que
mantener y que se va a pudrir.

## Protocolo

Sobre el socket Unix, con `net/http` y `gorilla/websocket` sobre un
`net.Listen("unix", …)`. Permisos `0600`: quien puede abrir el socket ya sos
vos y ya tiene shell, así que no hay token en esta capa.

| Ruta | Qué hace |
|---|---|
| `GET /info` | `{protocol_version, pid, started_at}` |
| `GET /sessions` | los ids vivos; fuente de verdad del sweep |
| `POST /sessions` | spawnea: `{id, shell, cwd, cols, rows, env, banner?}` |
| `POST /sessions/{id}/kill` | SIGKILL, sincrónico hasta que el reap terminó |
| `GET /sessions/{id}/attach` | upgrade a WebSocket: el plano de datos |

Cuatro verbos de control y uno de datos.

No hay `delete`: borrar es una fila, la borra el orquestador después de matar.
No hay `resize`: viaja por el WS de attach, como el frame `{"type":"resize"}`
que el browser ya manda.

### Por qué HTTP+WS y no un protocolo propio

El plano de datos es **el mismo protocolo que el orquestador ya le habla al
browser**: `attached` / binario / `ready` / `exit` del lado del servidor,
binario (input) y texto (resize) del lado del cliente. Está diseñado, escrito y
testeado desde M2.

Reusarlo significa que el orquestador reenvía los frames binarios sin mirarlos
—es un proxy de bytes, no un traductor—, que no se suma ninguna dependencia
(`net/http` y `gorilla/websocket` ya están), y que el daemon se debuggea con
`curl --unix-socket` sin levantar la UI.

Se descartaron:

- **Frames length-prefixed propios.** Más puro, sin el framing de WS arriba de
  un socket que ya es ordenado y confiable. Pero son ~150 líneas de protocolo
  nuevo para diseñar, versionar y testear, resolviendo un problema ya resuelto.
- **gRPC.** Mete protobuf + grpc-go en un `go.mod` de 978 bytes, y para bytes
  crudos de un pty protobuf no aporta nada: envolvés `bytes` en un mensaje.
- **SQLite como bus** (el daemon escribe output, el orquestador lo tailea). No
  sirve: el `outputWriter` batchea cada 250 ms y una terminal con 250 ms de lag
  es inusable. El stream vivo tiene que ir por un canal aparte del historial.

### La única excepción al proxy puro

El frame `attached` que ve el browser lleva la sesión completa, con título y
`kanban_status`; el daemon no conoce esa metadata. Ese frame lo compone el
orquestador con su propia fila y descarta el del daemon. Todo el resto pasa sin
tocarse.

### Backpressure en dos niveles

El `hub` y la expulsión del cliente lento ya existen y se quedan en el daemon;
el orquestador es un suscriptor más. Hacia el browser, el orquestador aplica su
propio drop. Cada tramo protege al pty de su propio lector lento, que es la
propiedad que ya tiene el sistema hoy.

## Dueños de la base

Los dos procesos abren la misma base. WAL y `busy_timeout(5000)` ya están en el
DSN desde M2.

| Dato | Dueño |
|---|---|
| `session_output` | daemon (el writer batcheado) |
| `sessions.pty_status`, `exit_reason`, `exit_code`, `exited_at` | daemon (lo escribe `reap()`) |
| `sessions.cols` / `rows` | orquestador los fija en el insert; de ahí en adelante los actualiza el daemon en cada resize |
| insert y delete de la fila `sessions` | orquestador |
| `title`, `description`, `work_status`, `kanban_status`, `last_active_at` | orquestador |
| `session_kv`, `session_resources` | orquestador |

La alternativa —daemon dueño exclusivo, orquestador pidiendo todo por IPC—
tiene un boundary más limpio y un solo escritor, pero hace crecer el protocolo
con cada feature: cada campo de contexto nuevo sería un verbo nuevo en el
daemon, o sea un reinicio del daemon. Trabaja directamente en contra del
invariante.

La división de quién escribe qué es convención, no la fuerza el motor. Es el
costo aceptado de esta opción.

## Flujos

**Create.** El orquestador inserta la fila en estado `starting` y llama a `POST
/sessions`. El daemon spawnea, arranca pump/reap/writer y la marca `running`; si
el spawn falla la marca `exited`/`spawn_failed` y devuelve error. El orden
importa: la fila tiene que existir antes, porque `session_output` tiene FK
contra ella.

`starting` es un estado nuevo (migración chica; el mecanismo existe desde M2).
La UI lo muestra como "arrancando", y una fila trabada en `starting` sin sesión
viva la levanta el sweep como cualquier otra.

El `env` lo arma el orquestador —`WEBTERM_TOKEN` hoy, lo que venga mañana— y el
daemon lo pega tal cual sobre `TERM`, `COLORTERM` y `WEBTERM_SESSION_ID`. Así
sumar una variable nueva no toca el daemon.

**Attach.** El orquestador mira su fila. Si dice muerta, **ni siquiera consulta
al daemon**: lee el historial de la base y manda replay + `exit`, la conexión de
solo lectura que ya existe. Si dice viva, abre el WS contra el daemon, compone
su propio `attached` y reenvía; write y resize son frames sobre ese mismo WS.

La fila puede estar desactualizada por una ventana chica —el pty murió y el
`reap()` del daemon todavía no la marcó—, así que el attach contra el daemon
puede devolver 404. Ese 404 no es un error: el orquestador cae al camino de
solo lectura, el mismo que hubiera tomado si la fila hubiese estado al día.

**Kill.** `POST /sessions/{id}/kill`, sincrónico: al volver, el daemon ya
reapeó y la fila ya dice `exited`/`killed`, así que un GET inmediato no miente
— la misma garantía que hoy.

**Restart.** El orquestador verifica con `GET /sessions` que no esté viva y hace
`POST /sessions` con el mismo id y `banner: "— sesión reanudada —"`. El banner
lo escribe el daemon: tiene que quedar entre el historial viejo y el prompt
nuevo, y el único que puede ordenarlo contra el writer es quien lo posee.

**Delete.** Kill si vive, después borrar la fila; la cascada se lleva KV,
recursos e historial.

## Reconciliación

- **`ReconcileBoot` desaparece.** Siempre fue un caso particular de "marcá
  muertas las filas vivas sin proceso detrás", y ahora eso lo contesta `GET
  /sessions`.
- **`Sweep` se queda en el orquestador**, pero su fuente deja de ser el mapa en
  memoria y pasa a ser `GET /sessions`. Corre antes de escuchar —para no mentir
  en la ventana inicial, igual que hoy— y después cada 30 s.
- Eso fija el orden de arranque del orquestador: conectar al daemon (spawneándolo
  si no está) → sweep → recién ahí escuchar. Si el daemon no levanta, el
  orquestador falla con un error claro en vez de arrancar sin poder crear
  sesiones.
- Con un daemon recién arrancado, `GET /sessions` devuelve vacío y el sweep
  marca todo lo que había quedado `running` como `daemon_restart`. Es el
  comportamiento viejo, derivado en vez de hardcodeado.
- **`reap()` se queda en el daemon**: si el orquestador está caído y un shell
  hace `exit`, la fila se marca igual.

`exit_reason` `backend_restart` pasa a llamarse `daemon_restart`, que es lo que
ahora significa de verdad.

## Modos de falla

| Qué pasa | Consecuencia |
|---|---|
| se reinicia el orquestador | nada: los ptys siguen y el writer sigue bajando output a SQLite. Al volver, `GET /sessions` le dice qué sigue vivo; el browser reattachea y el replay sale de la base + el ring |
| crashea el daemon | mueren las sesiones. El orquestador no puede conectar, respawnea uno nuevo y el sweep marca todo `daemon_restart`. Lo importante es que la UI no miente |
| se reinicia el daemon a mano | ídem, pero pedido: el comando avisa antes |
| los dos escriben a la vez | WAL admite un escritor por vez y `busy_timeout(5000)` ya está puesto. Las escrituras son cortas y el caso caliente —un flush de output cada 250 ms por sesión viva— es todo del daemon, sin competencia |

## El token tiene que persistirse

Hoy `main.go` genera un token nuevo en cada arranque cuando no escucha solo en
loopback. Con sesiones que sobreviven al reinicio, el `WEBTERM_TOKEN` inyectado
en los ptys vivos queda viejo y **el servidor MCP de M9 deja de autenticar
justo adentro de las sesiones que esta feature existe para salvar**.

Se persiste en `~/.webterm/token` (`0600`), generado la primera vez. `-token` y
`$WEBTERM_TOKEN` lo siguen pisando.

No es un extra: sin esto, M10 rompe M9.

## Flujo de desarrollo

`make run` y `make dev` levantan el daemon si no está y siguen siendo un
comando. `webterm daemon restart` es el acto explícito que mata las sesiones.

Para desarrollar sobre webterm sin tocar el webterm real, una instancia
separada: `-addr 127.0.0.1:7789 -db ~/.webterm/dev.db`, que por las rutas
derivadas se lleva su propio daemon.

**El cutover es destructivo una sola vez.** La primera vez que se levante el
binario nuevo como el webterm de verdad se mueren todas las sesiones vivas: el
daemon todavía no existía para sostenerlas. Es el último reinicio que cuesta
sesiones.
