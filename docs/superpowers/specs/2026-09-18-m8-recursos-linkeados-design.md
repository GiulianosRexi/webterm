# M8 — Recursos externos linkeados a una sesión

Fecha: 2026-09-18
Estado: aprobado, pendiente de implementación

## Objetivo

Colgarle a una sesión los recursos externos con los que se relaciona y ver su
estado sin salir de WebTerm. Se arranca solo con PRs de GitHub; el modelo es
genérico para sumar después Linear, Slack o lo que aparezca.

`work_status` (M6) dice qué está haciendo Claude *ahora*. El PR dice dónde está
*el trabajo*. Son señales ortogonales, y juntas son lo que hace útil al
dashboard de M7: "esta sesión tiene 3 comments sin resolver y los checks en
rojo" se ve sin entrar a la terminal.

## Verificación previa

La query de GraphQL se validó contra PRs reales de `cli/cli` antes de escribir
este documento. Una sola llamada devuelve todo lo que la card necesita:

```
state=OPEN  isDraft=false  mergeable=MERGEABLE  reviewDecision=REVIEW_REQUIRED
rollup=SUCCESS  checks=20  threads_sin_resolver=0
```

En un PR con threads mixtos, `reviewThreads` devolvió `totalCount: 2` con uno
`isResolved: true` y otro `false` — la cuenta de sin resolver sale de filtrar
esos nodos.

## Decisiones tomadas

| Decisión | Elección |
|---|---|
| Dónde vive el link | Tabla `session_resources`, no un campo JSON en `sessions` |
| Dónde vive el estado | Caché en memoria con TTL, nunca en SQLite |
| Cómo se consulta GitHub | GraphQL vía el `gh` CLI ya autenticado |
| Quién hace el polling | El backend; el frontend solo decide *cuándo pedir* |
| `system`/`type` | Los infiere el backend de la URL |
| Cards por tipo | Una sola, de PR, hardcodeada |
| Auto-link desde el `cwd` | Fuera de alcance, pero el modelo no lo bloquea |

## Modelo de datos

Migración v2:

```sql
CREATE TABLE session_resources (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id TEXT    NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  system     TEXT    NOT NULL,   -- gh | linear | slack | ...
  type       TEXT    NOT NULL,   -- pr | issue | ticket | message | ...
  ref        TEXT    NOT NULL,   -- URL canónica, normalizada
  created_at INTEGER NOT NULL,
  UNIQUE (session_id, ref)
);

CREATE INDEX idx_resources_ref ON session_resources(ref);
CREATE INDEX idx_resources_session ON session_resources(session_id);
```

Tabla y no blob JSON por la **búsqueda inversa**: cuando algo diga "el PR 123
cambió" hay que resolver qué sesiones se prenden. Con JSON en `sessions` eso es
un scan completo más parsear cada fila; con tabla es un índice. No suma
maquinaria — `session_kv` ya es una tabla y las migraciones existen desde M2.

El `UNIQUE (session_id, ref)` evita linkear dos veces el mismo PR a la misma
sesión. El mismo PR sí puede estar en varias sesiones.

**El estado no se persiste.** Es un caché con TTL en memoria. Si el backend se
reinicia, se vuelve a consultar: un check en verde de hace veinte minutos
mostrado como actual es peor que no mostrar nada.

## Arquitectura

```
internal/resources/     providers, matching de URL, caché con TTL
  ├── resources.go      tipos Ref/Snapshot, interfaz Provider, Registry
  ├── cache.go          caché con TTL y single-flight
  └── github.go         provider de PRs: query GraphQL y mapeo a PRState
internal/store/         + CRUD de session_resources (migración v2)
internal/session/       + métodos de ABM que el servidor consume
internal/server/        + endpoints REST
web/src/                + panel desplegable y card de PR
```

`resources` no conoce ni el store ni las sesiones: dada una URL devuelve un
`Ref`, y dado un `Ref` devuelve un `Snapshot`. Eso lo hace testeable sin DB y
sin red.

### Tipos

```go
// Ref identifica un recurso externo.
type Ref struct {
    System string // gh
    Type   string // pr
    URL    string // URL canónica normalizada
}

// Snapshot es el estado traído del sistema externo en un momento dado.
type Snapshot struct {
    FetchedAt int64  `json:"fetched_at"`
    Error     string `json:"error,omitempty"`
    PR        *PRState `json:"pr,omitempty"`
}

type PRState struct {
    Number           int    `json:"number"`
    Title            string `json:"title"`
    Author           string `json:"author"`
    State            string `json:"state"`             // OPEN | CLOSED | MERGED
    IsDraft          bool   `json:"is_draft"`
    Mergeable        string `json:"mergeable"`         // MERGEABLE | CONFLICTING | UNKNOWN
    ReviewDecision   string `json:"review_decision"`   // APPROVED | CHANGES_REQUESTED | REVIEW_REQUIRED | ""
    UnresolvedCount  int    `json:"unresolved_count"`
    ThreadsTruncated bool   `json:"threads_truncated"`
    ChecksState      string `json:"checks_state"`      // SUCCESS | FAILURE | PENDING | ""
    ChecksTotal      int    `json:"checks_total"`
    ChecksFailing    int    `json:"checks_failing"`
}
```

`Snapshot` lleva `PR` como campo tipado en vez de un `any` opaco: con un solo
provider, un `any` complica el JSON y los tests sin comprar nada. Cuando entre
el segundo provider se suma su campo, y ahí se verá si conviene una interfaz.

### Registry

El matching de URL a `Ref` es el punto de extensión real, y es chico:

```go
// Provider traduce URLs a refs y refs a estado.
type Provider interface {
    Match(rawURL string) (Ref, bool)
    Fetch(ctx context.Context, ref Ref) (*Snapshot, error)
}

type Registry struct{ providers []Provider }

// Resolve encuentra el provider que reconoce la URL.
func (r *Registry) Resolve(rawURL string) (Ref, Provider, bool)
```

Hoy hay un solo provider. No se construye la abstracción de "una card por
`system`/`type`" en el frontend: diseñar un sistema de plugins contra una
muestra de uno es la forma clásica de errarle a la abstracción.

## Provider de GitHub

### Matching

`https://github.com/{owner}/{repo}/pull/{n}` — con o sin `https://`, con o sin
sufijos (`/files`, `#discussion_r123`, query string). Se normaliza a la URL
canónica sin sufijos, que es lo que se guarda en `ref`.

Se rechaza cualquier otra cosa con un error claro; nada de intentar adivinar.

### Query

```graphql
query($owner:String!, $name:String!, $number:Int!) {
  repository(owner:$owner, name:$name) {
    pullRequest(number:$number) {
      number title url state isDraft mergeable reviewDecision
      author { login }
      reviewThreads(first:100) { totalCount nodes { isResolved } }
      commits(last:1) { nodes { commit { statusCheckRollup {
        state
        contexts(first:100) { totalCount nodes {
          __typename
          ... on CheckRun { name conclusion status }
          ... on StatusContext { context state }
        } }
      } } } }
    }
  }
}
```

**GraphQL y no REST no es preferencia: la cuenta de comments sin resolver no
existe en la API REST.** Los review threads con `isResolved` solo están en
GraphQL. Por REST habría que paginar comments e inferir el estado, y saldría
mal. Esta query trae todo en una llamada; el equivalente REST son ~4 que igual
no dan el dato que importa.

`statusCheckRollup` puede venir `null` — pasa en PRs viejos cuyos checks
expiraron, y se vio en la verificación previa. Se mapea a `ChecksState: ""` y
la card no muestra checks, en vez de romper.

`reviewThreads(first:100)`: más de 100 threads se truncan. Se expone
`ThreadsTruncated` cuando `totalCount > 100` para que la card muestre "100+" en
vez de mentir con un número bajo. Paginar no vale la pena para el caso real.

### Credenciales

Se shellea a `gh api graphql`, que ya está instalado y autenticado en la
máquina con scope `repo`. No se introduce un secreto propio ni se lee el token:
`gh` maneja el keyring y el refresh.

El rate limit no es una restricción: 5000 req/h en GraphQL contra una query por
PR cada 30 s (120/h). Harían falta ~40 PRs visibles en simultáneo para
acercarse.

Para poder testear sin red, el provider recibe un `runner`:

```go
type runner func(ctx context.Context, args ...string) ([]byte, error)
```

En producción ejecuta `gh`; en los tests devuelve fixtures capturadas de
respuestas reales. Un único test de integración ejercita el `gh` de verdad y se
saltea con `t.Skip` si no está instalado o autenticado.

Subprocess con `context.WithTimeout` de 10 s. Los argumentos van como argv
separado, así que no hay inyección posible por la URL.

## Caché y refresco

Caché en memoria, clave `ref.URL`, TTL 30 s, con single-flight: N clientes
mirando el mismo PR cuestan una sola llamada a GitHub.

`GET /api/sessions/{id}/resources` devuelve cada recurso con su snapshot. Si el
snapshot está vencido se consulta de forma sincrónica antes de responder. Una
llamada de ~300 ms cada 30 s es invisible, y evita el estado intermedio de
"todavía no sé" que complicaría la UI.

**El polling vive en el backend.** El frontend no puede llamar a GitHub sin que
el token termine en el browser. El cliente solo decide *cuándo pedir*: consulta
este endpoint cada 15 s **mientras el panel está abierto**, y no consulta
cuando está cerrado. El TTL de 30 s hace que ese polling no se traduzca uno a
uno en llamadas a GitHub.

Los errores se cachean igual, con un TTL más corto (10 s): si GitHub está
caído, no conviene reintentar en cada request, pero tampoco quedarse pegado al
error medio minuto.

## API

| Método | Ruta | Qué hace |
|---|---|---|
| `GET` | `/api/sessions/{id}/resources` | lista los recursos con su estado |
| `POST` | `/api/sessions/{id}/resources` | linkea: `{"ref":"https://github.com/o/r/pull/1"}` |
| `DELETE` | `/api/sessions/{id}/resources/{rid}` | deslinkea |

El `POST` manda **solo la URL**: `system` y `type` los infiere el backend. Es
mejor UX (pegás y listo) y es justo el seam que generaliza — sumar Linear es
sumar un provider al registry, no cambiar el contrato.

Se aceptan `system`/`type` explícitos en el body como override, para no cerrarle
la puerta a un recurso cuyo formato de URL el registry todavía no conozca.

Errores: `404` sesión inexistente, `400` URL que ningún provider reconoce,
`409` recurso ya linkeado a esa sesión.

## UI

Panel desplegable en la sesión seleccionada, colapsado por defecto, que lista
los recursos linkeados. El encabezado muestra la cantidad para que se vea que
hay algo sin tener que abrirlo.

Linkear es manual: un input donde se pega la URL del PR.

La card de PR muestra, en una línea de estado y una de detalle:

- **título y número**, linkeados al PR
- **estado**: abierto / borrador / mergeado / cerrado
- **review**: aprobado / cambios pedidos / falta review
- **checks**: verde, rojo con la cuenta de los que fallan, o corriendo
- **comments sin resolver**: la cuenta, o "100+" si se truncó

Los estados de error son parte del alcance, no un detalle:

| Caso | Qué muestra |
|---|---|
| `gh` no instalado | "instalá el gh CLI para ver el estado" |
| `gh` sin autenticar | "corré `gh auth login`" |
| PR inexistente o sin permisos | "no se pudo acceder al PR" |
| GitHub caído o timeout | el último estado conocido, marcado como viejo |

Los datos con más de un TTL de antigüedad se marcan con su edad ("hace 2 min").
Un check en verde de hace veinte minutos mostrado como actual es peor que no
mostrar nada.

## Testing

- **`resources` (sin red):** matching de URL —variantes válidas y basura que
  debe rechazarse—, mapeo de fixtures reales de GraphQL a `PRState` (incluido
  `statusCheckRollup: null` y el truncado de threads), TTL del caché,
  single-flight bajo concurrencia, y caché de errores con su TTL corto.
- **`resources` (con red):** un test de integración contra un PR público real,
  con `t.Skip` si no hay `gh` o no está autenticado.
- **`store`:** CRUD de `session_resources`, cascada al borrar la sesión,
  rechazo del duplicado por el `UNIQUE`.
- **`server`:** los tres endpoints incluidos sus casos de error, con un
  registry de prueba que no toca la red.

## Fuera de alcance

- **Auto-link desde el `cwd`**: la sesión conoce su directorio, de ahí salen
  remote y branch, y de ahí el PR abierto. El modelo lo soporta sin cambios;
  se implementa después. Con el CLI de M5, `webterm link pr` es la versión
  barata de lo mismo.
- **Webhooks**: el índice sobre `ref` deja lista la búsqueda inversa, pero
  recibir eventos de GitHub necesita exponer el backend a internet, que hoy no
  es el caso.
- **Providers de Linear y Slack**: entran cuando haya un segundo caso real que
  fuerce la abstracción de las cards.

## Limitaciones conocidas

1. **Depende del `gh` CLI.** Sin él, linkear funciona pero el estado muestra un
   error explicativo. La app no deja de andar por eso.
2. **Más de 100 review threads o 100 checks se truncan.** Se marca en la UI en
   vez de mentir.
3. **El estado puede estar hasta 30 s viejo**, por el TTL. La UI muestra la
   edad del dato.
