# Instalación de WebTerm (instructivo para agentes)

Este archivo está escrito para que lo siga un agente (Claude Code u otro) y deje
WebTerm funcionando en esta máquina. Seguí los pasos en orden, verificá cada
uno antes de pasar al siguiente y, si algo no da lo esperado, pará y avisale al
usuario en vez de improvisar.

El estado final es:

- el **daemon** de sesiones corriendo (dueño de los ptys);
- el **orquestador** (HTTP, UI, API, MCP) corriendo **desatado** de la sesión
  que lo lanzó, así el usuario puede cerrar esa terminal/sesión de Claude sin
  perder el acceso a WebTerm;
- el servidor MCP `webterm` registrado en Claude Code;
- el `gh` CLI autenticado, para que funcione la integración de PRs;
- `WEBTERM_TOKEN` exportado en el entorno del usuario.

El ciclo de desarrollo (qué recompilar y qué reiniciar según lo que cambie) está
en [DEVELOPMENT.md](DEVELOPMENT.md).

---

## ⚠️ Antes de empezar: NO reinicies ni mates el daemon

WebTerm son dos procesos:

| Proceso | Cómo se ve en `ps` | Se puede reiniciar |
|---|---|---|
| **daemon** | `bin/webterm daemon -db ...` | **NO** — es padre de todos los ptys |
| **orquestador** | `bin/webterm -addr ...` | sí, siempre que haga falta |

Cada sesión de terminal de WebTerm es un pty **hijo del daemon**. Si el daemon
muere, mueren todas las sesiones del usuario — incluidas otras sesiones de
Claude Code con trabajo en curso, y probablemente la tuya misma si estás
corriendo adentro de WebTerm. Eso es pérdida de datos real, no un
inconveniente de entorno.

Reglas, sin excepciones salvo pedido explícito del usuario:

- **Nunca** corras `webterm daemon stop`, `webterm daemon restart`,
  `make daemon-stop` ni `make daemon-restart`.
- **Nunca** hagas `kill`/`pkill`/`killall` que pueda alcanzar al daemon. En
  particular, `pkill webterm` o `pkill -f bin/webterm` matan también al daemon.
  El único patrón seguro para el orquestador es `pkill -f 'bin/webterm -addr'`.
- **Nunca** borres `~/.webterm/webterm.sock` ni `~/.webterm/webterm.lock`.
- Recompilar `bin/webterm` **no** requiere reiniciar el daemon: el daemon ya
  corriendo sigue con su binario en memoria, y el orquestador nuevo se conecta
  a él por el socket. Ningún cambio de frontend, API, MCP, recursos o store
  necesita reiniciar el daemon.
- Si el orquestador dice que el protocolo del daemon es distinto y pide
  `webterm daemon restart`, **no lo hagas**: explicale al usuario que eso mata
  todas las sesiones vivas y dejá que él decida cuándo.

`webterm daemon status` es de solo lectura y siempre es seguro.

---

## 1. Requisitos

Verificá que estén instalados:

```bash
go version      # >= 1.25
node --version  # >= 20
npm --version
gh --version    # GitHub CLI, para la integración de PRs
claude --version
```

Si falta alguno, en macOS se instala con Homebrew (`brew install go node gh`).
Pedile confirmación al usuario antes de instalar nada.

## 2. Detectar si ya hay algo corriendo

```bash
ps -Ao pid,ppid,pgid,command | grep '[b]in/webterm'
```

- Si aparece una línea con `bin/webterm daemon`: **el daemon ya existe y tiene
  sesiones del usuario. No lo toques** (ver la sección de arriba). Seguí con la
  instalación: el orquestador lo va a reusar.
- Si aparece una línea con `bin/webterm -addr`: ya hay un orquestador. Si vas a
  levantar uno nuevo, primero detené solo ese con
  `pkill -f 'bin/webterm -addr'` (seguro: no toca sesiones).
- Si no aparece nada: instalación limpia.

Si estás corriendo dentro de WebTerm (existe `$WEBTERM_SESSION_ID` en tu
entorno), el daemon existe con certeza: tu propia shell es hija suya.

## 3. Compilar

Todo se corre desde la raíz del repo.

```bash
cd /ruta/al/repo/webterm
make build        # npm install (si hace falta) + web/dist + bin/webterm
```

Verificá:

```bash
ls -l bin/webterm web/dist/index.html
```

## 4. Levantar el orquestador desatado

El orquestador sirve el frontend desde `web/dist` con un path **relativo**
(flag `-static`), así que tiene que arrancar con el cwd en la raíz del repo.
Al arrancar levanta el daemon solo si no hay uno (y si hay, lo reusa).

Tiene que quedar desatado de tu sesión: si queda como hijo de tu shell o en tu
grupo de procesos, cuando el usuario cierre esta sesión de Claude se lleva
puesto el orquestador y pierde el acceso a la UI. Por eso:

- **no** uses `run_in_background` ni herramientas que monitoreen el proceso
  (el harness lo mata al terminar la sesión);
- **no** uses `make run` / `make run-lan` para esto: corren en foreground;
- sí usá `nohup` + `setsid` (vía perl, porque macOS no trae el comando
  `setsid`), con stdin/stdout/stderr redirigidos:

```bash
cd /ruta/al/repo/webterm && \
nohup perl -MPOSIX -e 'POSIX::setsid() or die "setsid: $!"; exec @ARGV' \
  ./bin/webterm -addr 0.0.0.0:7788 \
  >> ~/.webterm/webterm-web.log 2>&1 < /dev/null &
```

`-addr 0.0.0.0:7788` expone WebTerm en la red local (con token obligatorio),
que es la configuración en uso. Si el usuario lo quiere solo local, usá
`-addr 127.0.0.1:7788` (sin token). Si `~/.webterm` todavía no existe, crealo
antes con `mkdir -p ~/.webterm` (la redirección del log lo necesita).

Verificá que quedó bien:

```bash
sleep 2
ps -o pid,ppid,pgid,command -p "$(pgrep -f 'bin/webterm -addr')"
# esperado: PGID == PID (sesión propia). PPID pasa a 1 cuando tu shell termina.
ps -Ao pid,command | grep '[b]in/webterm daemon'   # el daemon tiene que existir
curl -s http://127.0.0.1:7788/api/health           # "daemon":"ok"
tail -5 ~/.webterm/webterm-web.log                 # URLs con el token
```

Si `/api/health` responde `"daemon":"unreachable"` o el log muestra un error
de protocolo del daemon, **pará y avisale al usuario**. No reinicies el daemon.

## 5. Token de acceso y variables de entorno

Escuchando en `0.0.0.0`, el orquestador genera un token la primera vez y lo
persiste en `~/.webterm/webterm.token` (con `127.0.0.1` no hay token y este
paso no aplica). Prioridad: flag `-token` > env `WEBTERM_TOKEN` > archivo.

Dentro de cada sesión de WebTerm el backend ya inyecta `WEBTERM_TOKEN` y
`WEBTERM_SESSION_ID` al pty. Igual se exporta `WEBTERM_TOKEN` en el entorno del
usuario, leyéndolo del archivo para que siga al token si alguna vez rota.
Agregá esto a `~/.zshenv` (si el usuario usa zsh; si no, al rc de su shell),
solo si no está ya:

```bash
grep -q 'webterm.token' ~/.zshenv 2>/dev/null || cat >> ~/.zshenv <<'EOF'

# webterm: token de acceso para el cliente MCP (se lee del archivo en disco,
# así sigue al token aunque rote). El header del MCP usa ${WEBTERM_TOKEN}.
[ -f "$HOME/.webterm/webterm.token" ] && export WEBTERM_TOKEN="$(cat "$HOME/.webterm/webterm.token")"
EOF
```

Verificá en una shell nueva:

```bash
zsh -c 'echo ${WEBTERM_TOKEN:+ok}'   # imprime ok
```

No imprimas el token completo en la conversación salvo que el usuario lo pida.
Para darle la URL de acceso, apuntalo a `~/.webterm/webterm-web.log`.

## 6. Servidor MCP en Claude Code

El orquestador expone MCP en `/mcp`. Se registra una vez, a nivel usuario:

```bash
./bin/webterm -addr 0.0.0.0:7788 -mcp-config   # imprime el comando exacto
```

Registralo con `--scope user` (el comando impreso no lo trae) para que esté
disponible en todas las sesiones, sin importar el directorio:

```bash
claude mcp add --scope user --transport http webterm http://127.0.0.1:7788/mcp \
  -H 'X-Webterm-Session: ${WEBTERM_SESSION_ID}' \
  -H 'Authorization: Bearer ${WEBTERM_TOKEN}'
```

**Las comillas simples son obligatorias.** Claude Code expande
`${WEBTERM_SESSION_ID}` y `${WEBTERM_TOKEN}` en cada request contra el entorno
de la sesión que llama; si las expandiera el shell al registrar, todas las
sesiones escribirían sobre la misma.

Si ya existe un servidor `webterm` registrado, revisalo con
`claude mcp get webterm`; si está bien, no lo vuelvas a agregar.

Verificá:

```bash
claude mcp list | grep webterm    # connected (desde una sesión de WebTerm)
```

Fuera de una sesión de WebTerm no existe `WEBTERM_SESSION_ID` y las tools lo
dicen explícitamente: es esperado. La prueba real es abrir una sesión nueva
desde la UI, correr `claude` adentro y pedirle que use `set_title`.

## 7. GitHub CLI (integración de PRs)

El estado de los PRs linkeados sale de `gh`, ejecutado por el **orquestador**
con su propio entorno. Por eso `gh` tiene que estar en el `PATH` que tenía la
shell que lanzó el orquestador, y autenticado para ese usuario.

```bash
gh auth status
```

Si no está logueado, el login es interactivo: pedile al usuario que corra
`! gh auth login` en el prompt (no lo podés completar vos). Después:

```bash
gh auth status                                   # Logged in to github.com
gh api graphql -f query='{ viewer { login } }'   # la integración usa GraphQL
```

Para organizaciones con SSO, el token tiene que estar autorizado para la org
(`gh auth refresh` si un PR de la org da error de permisos).

## 8. Verificación final

```bash
./bin/webterm daemon status                          # pid, protocolo, sesiones vivas
curl -s http://127.0.0.1:7788/api/health             # status ok, daemon ok
ps -o pid,ppid,pgid,command -p "$(pgrep -f 'bin/webterm -addr')"   # PGID == PID
```

Y contale al usuario:

- la URL (`http://127.0.0.1:7788`, o la IP de LAN del log) y que el token está
  en `~/.webterm/webterm-web.log` / `~/.webterm/webterm.token`;
- que puede cerrar esta sesión: el orquestador y el daemon siguen corriendo;
- que las sesiones abiertas antes de ahora no se tocaron.

## Archivos y rutas

| Ruta | Qué es |
|---|---|
| `bin/webterm` | binario único: orquestador y (con `daemon`) daemon |
| `web/dist/` | build del frontend que sirve el orquestador |
| `~/.webterm/webterm.db` | SQLite: sesiones, KV, historial, recursos |
| `~/.webterm/webterm.sock` | socket orquestador ↔ daemon (**no borrar**) |
| `~/.webterm/webterm.lock` | flock del daemon (**no borrar**) |
| `~/.webterm/webterm.log` | log del daemon |
| `~/.webterm/webterm-web.log` | log del orquestador (la redirección del paso 4) |
| `~/.webterm/webterm.token` | token de acceso autogenerado |
