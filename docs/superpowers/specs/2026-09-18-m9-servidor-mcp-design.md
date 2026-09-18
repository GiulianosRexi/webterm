# M9 — Servidor MCP en el backend

Fecha: 2026-09-18
Estado: aprobado, pendiente de implementación

## Objetivo

Que Claude, corriendo dentro de una sesión de WebTerm, pueda escribirle contexto
a esa sesión y linkearle PRs sin salir de ahí. El backend expone un servidor MCP
y Claude Code se conecta como cliente.

Hoy el modelo de sesión tiene `title`, `description` y el KV desde M2, y los
recursos linkeados desde M8, pero todo eso se llena a mano desde la UI. Con MCP
lo llena el agente que está haciendo el trabajo, que es el que sabe qué está
haciendo.

## Esto es un adaptador, no lógica nueva

Las tres operaciones que el MCP necesita ya existen y están testeadas:

```go
m.SetKV(id, key, value)              // M2
m.UpdateMeta(id, store.MetaPatch{…}) // M2
m.AddResource(id, url, "", "")       // M8
```

Así que MCP es **un tercer adaptador de protocolo sobre el mismo manager**, al
lado del REST y del WebSocket. Lo genuinamente nuevo es el plumbing del
protocolo, la identidad de sesión y el camino de auth.

## Verificación previa

Las dos incógnitas del diseño se resolvieron contra las herramientas reales
antes de escribir esto.

**1. Claude Code expande variables de entorno en los headers HTTP.** Se registró
un servidor MCP de prueba apuntando a un listener que volcaba los headers:

```
sin la variable seteada           →  x-webterm-session: ${WEBTERM_SESSION_ID}
con WEBTERM_SESSION_ID=abc-123    →  x-webterm-session: abc-123
```

La expansión ocurre en tiempo de request, no al guardar la config. Eso es lo que
hace viable identificar la sesión por header.

**2. El SDK oficial de Go está en v1.8.0** y expone lo que hace falta:

- `NewStreamableHTTPHandler(getServer func(*http.Request) *Server, opts)`
- `RequestExtra.Header http.Header` — cada tool handler llega a los headers HTTP
  originales, así que alcanza con **un solo servidor MCP** compartido en vez de
  uno por sesión.
- `StreamableHTTPOptions.Stateless` — sin `Mcp-Session-Id`.

## Decisiones tomadas

| Decisión | Elección |
|---|---|
| Transporte | HTTP sobre el backend que ya existe, en `/mcp` |
| Modo | `Stateless`: sin sesiones de MCP |
| Identidad de sesión | Header `X-Webterm-Session`, de `${WEBTERM_SESSION_ID}` |
| Auth | El token de siempre, por `Authorization: Bearer` |
| Cómo llega el token al pty | Se inyecta `WEBTERM_TOKEN` al entorno de la sesión |
| Tools | `set_context`, `get_context`, `set_title`, `link_pr`, `list_links` |

### Por qué HTTP y no stdio

Un servidor stdio sería un binario que Claude Code spawnea. Ese binario tendría
que hablarle al backend por HTTP igual, porque el backend es el dueño del estado
de las sesiones. Sería un rodeo con un ejecutable extra que mantener.

### Por qué `Stateless`

Las tools son llamadas sueltas contra una sesión identificada por header: no hay
estado que arrastrar entre requests. Además evita una trampa de nombres — una
"sesión MCP" y una "sesión de WebTerm" serían dos cosas distintas con el mismo
nombre conviviendo en el mismo servidor.

## Identidad de sesión

Cada tool lee `X-Webterm-Session` de `req.Extra.Header`. El valor sale de
`WEBTERM_SESSION_ID`, que `terminal.New` inyecta al pty desde M1 — o sea que del
lado del backend no hay nada que agregar para que la variable exista.

Si el header falta o la sesión no existe, la tool devuelve un error **de tool**
(`IsError`), no un error de transporte: así Claude lee el mensaje y puede
explicárselo al usuario en vez de ver una falla opaca de conexión.

## Auth

Hoy `withAuth` acepta el token por cookie o por `?token=`. Ninguna de las dos le
sirve a un cliente de API, así que se suma `Authorization: Bearer <token>`, que
es lo que corresponde.

Para que el token esté disponible adentro de la sesión, se inyecta
`WEBTERM_TOKEN` al entorno del pty, igual que ya se hace con
`WEBTERM_SESSION_ID`. `session.Config` gana un campo `ExtraEnv []string` y el
entrypoint es quien pone ahí el token: así el paquete `session` no necesita
saber nada de autenticación.

**Qué protege y qué no.** El id de sesión **no es un secreto**: se genera con
timestamp más contador, así que es adivinable. La protección real la da el
token. Un proceso dentro de una sesión puede mandar el id de otra y escribirle
—pero eso no es una frontera nueva: esa shell ya corre con tus permisos y puede
hacer cualquier cosa en tu máquina. Lo que el token sí evita es que alguien en
la red toque tus sesiones cuando levantás con `run-lan`.

## Tools

Las cinco operan siempre sobre la sesión del header.

| Tool | Argumentos | Qué hace |
|---|---|---|
| `set_context` | `key`, `value` | escribe una clave del KV de la sesión |
| `get_context` | `key` (opcional) | devuelve una clave, o todo el KV si no se pasa |
| `set_title` | `title`, `description` (opcional) | nombra la sesión |
| `link_pr` | `url` | linkea un PR; `system` y `type` los infiere el backend |
| `list_links` | — | los recursos linkeados **con su estado actual** |

`list_links` devuelve el snapshot completo, así que Claude puede preguntar por
sus propios checks y comments sin resolver sin salir de la sesión. Sale gratis:
es el mismo `ListResources` que ya usa el endpoint REST.

`set_title` es lo que hace útil al sidebar. Hoy toda sesión se muestra con el
basename del `cwd`; con esto el agente puede ponerle de qué se trata el trabajo.

**Fuera de alcance a propósito:** nada de borrar. Deslinkear y borrar sesiones
siguen siendo decisiones humanas, y la UI ya las tiene. Un agente no necesita
poder destruir su propio contexto.

`kanban_status` tampoco se expone: no hay UI que lo muestre hasta M7, y sería
estado que se escribe y nadie ve.

## Arquitectura

```
internal/mcp/          servidor MCP: registro de tools y handlers
  ├── server.go        construcción del servidor y del handler HTTP
  └── tools.go         las cinco tools, cada una sobre un método del manager
internal/server/       (modificar) ruta /mcp y Bearer en withAuth
internal/session/      (modificar) ExtraEnv en Config
cmd/webterm/           (modificar) inyecta WEBTERM_TOKEN y arma el servidor MCP
```

`internal/mcp` depende de `session.Manager` por una interfaz chica declarada en
el propio paquete, no del struct concreto. Eso mantiene la dependencia en una
sola dirección y deja los tests sin necesidad de levantar ptys:

```go
// Sessions es lo que el servidor MCP necesita del manager.
type Sessions interface {
	Get(id string) (*store.Session, error)
	SetKV(id, key, value string) error
	ListKV(id string) (map[string]string, error)
	UpdateMeta(id string, p store.MetaPatch) (*store.Session, error)
	AddResource(id, rawURL, system, typ string) (*store.Resource, error)
	ListResources(ctx context.Context, id string) ([]*session.LinkedResource, error)
}
```

El servidor MCP se construye una sola vez al arrancar, con las cinco tools
registradas, y se monta con `NewStreamableHTTPHandler` devolviendo siempre esa
misma instancia.

## Configuración del lado del cliente

El backend sabe en qué dirección escucha y con qué token, así que puede imprimir
la línea exacta. Un flag `-mcp-config` la escribe y sale:

```bash
$ webterm -mcp-config
claude mcp add --transport http webterm http://127.0.0.1:7788/mcp \
  -H 'X-Webterm-Session: ${WEBTERM_SESSION_ID}' \
  -H 'Authorization: Bearer ${WEBTERM_TOKEN}'
```

Las comillas simples importan y el flag las emite: sin ellas el shell expande
las variables al registrar el servidor y quedarían congeladas en la config, que
es exactamente lo contrario de lo que se busca.

El header de `Authorization` se omite cuando el servidor corre sin token.

## Errores

| Caso | Qué devuelve |
|---|---|
| Falta el header de sesión | error de tool explicando que hay que configurarlo |
| La sesión no existe | error de tool con el id que se recibió |
| URL que ningún provider reconoce | el mensaje de `ErrUnknownResource` |
| El PR ya estaba linkeado | el mensaje de `ErrDuplicate`, no un error genérico |
| Token inválido o ausente | 401 del middleware, antes de llegar al MCP |

## Testing

- **`internal/mcp` sin red ni ptys:** las cinco tools contra un `Sessions`
  falso, incluidos los casos de header ausente, sesión inexistente y URL
  inválida.
- **End-to-end por HTTP:** un test que levanta el servidor con `httptest`, se
  conecta con el **cliente del propio SDK** mandando los dos headers, hace
  `tools/list` y `tools/call`, y después verifica contra el store que el KV
  quedó escrito. Es el test que prueba que el protocolo entero cierra.
- **Auth:** que `Authorization: Bearer` funcione, que un token equivocado dé
  401, y que los caminos viejos (cookie y `?token=`) sigan andando.
- **Inyección del entorno:** que una sesión creada tenga `WEBTERM_TOKEN` en su
  entorno cuando el servidor corre con token, y que no lo tenga cuando no.

## Limitaciones conocidas

1. **`WEBTERM_TOKEN` queda visible en el entorno de la sesión**, así que un
   `env` dentro de la terminal lo imprime y eso puede terminar en el historial
   que guardamos en SQLite. Es tu propia máquina y tu propio token, pero conviene
   saberlo.
2. **El id de sesión es adivinable.** Ver la sección de auth: no es una frontera
   de seguridad, el token sí.
3. **La config del MCP es por proyecto o por usuario, no por sesión.** Una sola
   configuración sirve para todas las sesiones justamente porque la variable se
   resuelve en cada request; pero si Claude corre fuera de una sesión de WebTerm,
   la variable no existe y las tools devuelven el error de header ausente.
