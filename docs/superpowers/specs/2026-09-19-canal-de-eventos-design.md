# Canal de eventos servidor → navegador

Fecha: 2026-09-19
Estado: aprobado, pendiente de plan de implementación

## Problema

La UI se entera de los cambios por polling: `App.tsx` pide la lista de sesiones
cada 3 s y `ResourcePanel.tsx` la de recursos cada 15 s con el panel abierto. El
caso que lo hace evidente es linkear un PR desde el MCP (`link_pr`) mientras se
mira la UI: el recurso ya está en la base y el panel lo muestra hasta 15 s
después.

## Qué se vuelve instantáneo y qué no

Instantáneo, porque el server es el que lo origina:

- alta y baja de recursos linkeados, tanto por REST como por MCP
- alta, renombre, kill, restart y borrado de sesiones

No instantáneo, y es una limitación consciente:

- **la muerte espontánea de un pty de una sesión que no se está mirando.** El
  que marca ese exit es el daemon, en `reap()` (`internal/session/manager.go:340`),
  de forma sincrónica: la fila nunca queda desactualizada, así que el sweep del
  orquestador (`internal/control/manager.go:344`) nunca la encuentra para
  marcar y por lo tanto nunca publica nada para ese caso. No es que se entere
  tarde: no se entera. El navegador se entera por el polling de 60 s que queda
  como red de seguridad —o, si es la sesión que se está mirando, al instante
  por el WS del terminal, que ya se entera hoy—, así que el agujero es acotado
  pero real.

Cerrar ese último caso pide un canal daemon → orquestador. Queda afuera: es un
proyecto propio y obliga a tocar el daemon.

## No-objetivos

- **No se toca `internal/session` ni `internal/daemon`.** El daemon es el dueño
  de los ptys de las sesiones de Claude que están corriendo; cualquier cambio
  ahí las pone en riesgo. Todo el trabajo vive en `internal/events` (nuevo),
  `internal/control`, `internal/server` y `web/`.
- No se empuja el estado de los PR. El refresh contra GitHub sigue como está,
  con su cache de 30 s.
- No se elimina el polling: baja a 60 s como red de seguridad.

## Arquitectura

### `internal/events` (paquete nuevo)

```go
type Kind string

const (
    ResourceAdded   Kind = "resource.added"
    ResourceRemoved Kind = "resource.removed"
    SessionCreated  Kind = "session.created"
    SessionUpdated  Kind = "session.updated"
    SessionDeleted  Kind = "session.deleted"
)

type Event struct {
    Seq       uint64 `json:"seq"`
    Kind      Kind   `json:"kind"`
    SessionID string `json:"session_id"`
}

// buffer es el tamaño del canal de CADA suscriptor, no un buffer compartido.
func New(buffer int) *Bus
func (b *Bus) Publish(kind Kind, sessionID string)
func (b *Bus) Subscribe() (<-chan Event, func())
```

`Seq` es un contador global monótono que asigna el bus. No es decorativo: es lo
que le permite al cliente detectar que se perdió un evento, sin que el server
tenga que mantener historial ni acuses.

**Un publisher lento no puede existir.** Cada suscriptor tiene su propio canal
con buffer; si está lleno, `Publish` descarta el evento para ese suscriptor y
sigue. Nunca bloquea. El hueco queda visible en la secuencia de `Seq`, y el
cliente que lo ve refetchea todo. `internal/session/hub.go` ya resuelve este
mismo problema para el fanout del terminal y es la referencia de forma.

### `internal/control.Manager`

Recibe el bus por `Config`, opcional. Si es `nil` no publica — mismo patrón que
`cfg.Resources`, y es lo que deja los tests actuales sin tocar.

Publica en los puntos de escritura que ya existen: `AddResource`,
`DeleteResource`, `Create`, `UpdateMeta`, `Kill`, `Restart`, `Delete`, y en el
sweep cuando marca un exit. Siempre después de que el store confirmó, nunca
antes: un evento sobre una escritura que después falla deja a la UI mostrando
algo que no pasó.

### `GET /api/events`

SSE, detrás del mismo middleware de auth que el resto de `/api`. Por cada
suscriptor:

1. Al abrir manda `event: resync`. El cliente refetchea todo. Esto cubre el
   arranque en frío y **toda** reconexión con el mismo camino, sin necesidad de
   replay ni de interpretar `Last-Event-ID`.
2. Después, un frame por evento, con `id: <seq>`.
3. `: keepalive` cada 25 s, para que ningún proxy corte el stream por inactivo.
4. Cierra cuando se cancela el `Context` del request, liberando la suscripción.

Los eventos van sin filtrar por sesión: es un server local con pocas sesiones y
el cliente ya sabe cuál le importa. Filtrar en el server sería estado extra a
cambio de nada.

### Web

`useEvents()`, montado una vez en `App`:

- `resync`, o un salto en `seq`: refetch completo de sesiones y recursos.
- `session.*`: `App` refetchea la lista.
- `resource.*`: `ResourcePanel` refetchea si el `session_id` es el suyo.

`EventSource` reconecta solo, y manda la cookie de auth por ser same-origin. Los
polls de 3 s y 15 s pasan a 60 s.

## Formato

```
event: resync
data: {}

id: 41
event: resource.added
data: {"seq":41,"kind":"resource.added","session_id":"abc"}
```

El evento dice qué cambió y de qué sesión, nunca el objeto. El cliente refetchea
el endpoint REST que ya existe. Mandar el recurso serializado ahorraría un
round-trip que en un server local no duele, a cambio de una segunda fuente de
verdad que puede divergir del store y de duplicar la serialización en dos
lugares.

## Tests

- `internal/events`: fanout a varios suscriptores, que `unsubscribe` libera y no
  deja goroutines, y que un suscriptor lleno no bloquea al publisher y deja
  hueco en `Seq`.
- `internal/server`: formato del frame SSE, el `resync` inicial y el cierre por
  cancelación del contexto.
- `internal/control`: que `AddResource` publica después de escribir, y que con
  bus `nil` no rompe.

El lado web queda con verificación manual: no hay runner de tests de frontend en
el repo y montarlo excede este trabajo.

## Notas operativas

Para levantar esto hay que reiniciar el orquestador (`make build` + restart),
**no el daemon**. Las sesiones vivas no se ven afectadas: el sweep del
orquestador pregunta `LiveIDs()` al daemon antes de marcar nada
(`internal/control/manager.go:300`), así que al volver las reconoce y las deja
como están. Con el daemon caído no marca nada, por diseño.

## Riesgos

- **Conexiones por origen.** HTTP/1.1 permite ~6 por dominio y ahora suma una
  permanente. Con el WS del terminal más los fetch sigue habiendo margen, pero
  si en algún momento se abren varias pestañas contra el mismo server puede
  apretar. Sale gratis de resolver si el server pasa a HTTP/2.
- **Refetch en tormenta.** Muchos eventos seguidos disparan muchos refetch. Si
  aparece, se resuelve con un debounce de ~100 ms en el cliente; no se hace
  ahora porque el volumen real es de unos pocos eventos por minuto.
