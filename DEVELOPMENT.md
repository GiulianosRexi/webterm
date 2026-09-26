# Ciclo de desarrollo de WebTerm (instructivo para agentes)

Cómo aplicar cambios sobre un WebTerm que ya está corriendo (ver
[INSTALLATION.md](INSTALLATION.md)) sin romperle nada al usuario. Lo más
probable es que estés corriendo **adentro** de WebTerm: la terminal donde
trabajás es un pty del mismo daemon que estás desarrollando.

## ⚠️ La regla: el daemon no se reinicia

Ningún cambio del ciclo normal requiere reiniciar el daemon. Si en algún
momento pensás "para que el cambio tome efecto hay que reiniciar el daemon",
**estás equivocado o estás ante un caso excepcional que decide el usuario**:
parate y preguntá.

Reiniciar el daemon (`webterm daemon stop|restart`, `make daemon-stop|restart`,
`pkill webterm`, `kill` a su pid, borrar `~/.webterm/webterm.sock`) mata
**todas** las sesiones vivas del usuario: otras sesiones de Claude Code a mitad
de trabajo, procesos largos, y seguramente tu propia sesión. El historial queda
en la base, pero los procesos se pierden.

Lo que sí se reinicia todo el tiempo, y es seguro, es el **orquestador**: el
daemon es un proceso aparte, y el orquestador nuevo se reconecta por el socket
y reconoce las sesiones vivas sin tocarlas.

Además, por pedido del usuario: **no modifiques `internal/session` ni
`internal/daemon`** salvo que lo pida explícitamente. Son el código que corre
dentro del daemon; cambiarlo no tiene efecto sin reiniciarlo, y reiniciarlo no
es una opción. Los tests sí pueden importar `internal/session`.

## Qué hacer según qué cambiaste

| Cambiaste | Qué correr | ¿Reiniciar orquestador? | ¿Reiniciar daemon? |
|---|---|---|---|
| Frontend (`web/`) | `make build-web` | no | **no** |
| Backend Go del orquestador (`cmd/`, `internal/server`, `control`, `store`, `mcp`, `resources`, `events`, `daemonclient`, `ptyapi`) | `make build-go` + reiniciar orquestador | sí | **no** |
| Frontend y backend | `make build` + reiniciar orquestador | sí | **no** |
| Schema de la base (`internal/store/schema.go`) | igual que backend | sí | **no** (ver migraciones) |
| `internal/session`, `internal/daemon` | no tocar (ver arriba) | — | decide el usuario |
| Docs, tests | nada | no | **no** |

### Frontend

El orquestador sirve `web/dist` directo desde disco en cada request, así que
alcanza con rebuildear:

```bash
make build-web        # tsc -b && vite build → web/dist
```

Después el usuario recarga la página en el browser. No hay que reiniciar nada.
`make build-web` también corre el type-check (`tsc -b`): si falla, el build
falla y `web/dist` queda como estaba.

### Backend (orquestador)

```bash
make build-go                                   # go build -o bin/webterm ./cmd/webterm
pkill -f 'bin/webterm -addr'                     # SOLO el orquestador
sleep 1
cd /ruta/al/repo/webterm && \
nohup perl -MPOSIX -e 'POSIX::setsid() or die "setsid: $!"; exec @ARGV' \
  ./bin/webterm -addr 0.0.0.0:7788 \
  >> ~/.webterm/webterm-web.log 2>&1 < /dev/null &
sleep 2 && curl -s http://127.0.0.1:7788/api/health
```

Puntos importantes:

- El patrón de `pkill` es exactamente `'bin/webterm -addr'`. `pkill webterm` o
  `pkill -f bin/webterm` también matan el daemon.
- Relanzalo siempre desatado (`nohup` + `setsid` + redirecciones), como en
  INSTALLATION.md, y nunca con `run_in_background`: si queda atado a tu sesión,
  cuando esta termine el usuario pierde el acceso a WebTerm — y si estás
  adentro de WebTerm, sin orquestador tampoco puede volver a tu sesión.
- Usá el mismo `-addr` con el que estaba corriendo (fijate con
  `ps -Ao command | grep '[b]in/webterm -addr'` antes de matarlo).
- Arrancalo con el cwd en la raíz del repo: `-static` es relativo (`web/dist`).
- Tu pty no se corta al reiniciar el orquestador; la pestaña del browser se
  reconecta sola. Chequeá `"daemon":"ok"` en `/api/health` y la cantidad de
  sesiones con `./bin/webterm daemon status` antes y después.
- Si el orquestador no arranca, mirá `tail -30 ~/.webterm/webterm-web.log`. Si
  el error es de versión de protocolo del daemon, **no** hagas
  `daemon restart`: avisale al usuario.

### Migraciones de la base

El daemon viejo sigue usando la misma base con sus queries de siempre, así que
las migraciones tienen que ser **aditivas**: tablas, índices o columnas nuevas.
Renombrar o borrar columnas, cambiar tipos o endurecer un CHECK rompe al daemon
en uso (el síntoma: el historial deja de guardarse, mientras el orquestador se
ve perfecto). Si una migración destructiva fuera inevitable, requiere subir
`daemon.ProtocolVersion` y un reinicio del daemon: consultalo con el usuario
antes de escribir una línea.

## Tests

```bash
make test        # go test ./... -race
```

Los tests no tocan el daemon real: usan un `session.Manager` embebido y bases
temporales. Son seguros de correr en cualquier momento.

Para probar contra una instancia aislada sin tocar la del usuario, usá otro
puerto y otra base; el socket, lock y log del daemon se derivan del `-db`, así
que esa instancia levanta **su propio** daemon:

```bash
./bin/webterm -addr 127.0.0.1:7799 -db ~/.webterm/dev.db
```

Al terminar, lo que se puede detener es el daemon **de esa base**
(`./bin/webterm daemon stop -db ~/.webterm/dev.db -yes`). Revisá dos veces el
`-db` antes de correrlo: sin él apunta al daemon real.

## `make dev` (Vite con HMR)

`make dev` levanta un orquestador propio en `:7788` además de Vite en `:5173`.
Con el orquestador del usuario ya corriendo en ese puerto va a fallar; en ese
caso usá solo Vite, que proxea `/ws` y `/api` al orquestador existente:

```bash
npm --prefix web run dev     # http://localhost:5173
```

En la práctica, el ciclo habitual es `make build-web` + recargar.

## Commits

- Código (identificadores, nombres) en inglés; mensajes de commit, docs y
  comentarios en español, como el resto del repo.
- `bin/`, `web/dist/` y las bases no se commitean (están en `.gitignore`).
