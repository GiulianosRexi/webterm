# Folders (fase 2 de organización)

Fecha: 2026-09-21
Estado: pendiente de revisión

## Problema

Las sesiones viven en una lista plana. Un folder = un proyecto: agrupa todas
las sesiones de ese proyecto y las separa visualmente del resto.

Es la fase 2 de tres. La 1 (buscador con Cmd+K) ya está. La 3 son tags, para
el tipo de trabajo — bugfix, consulta, implementación.

## Decisiones tomadas

**Un solo nivel, sin anidar.** Un proyecto no contiene proyectos. El caso que
justificaría anidar —subdividir un proyecto grande en frontend/backend— lo
resuelven mejor los tags de la fase 3: `folder=Iceberg` + `tag=frontend` deja
además que una sesión que toca las dos puntas lleve los dos tags, cosa que un
subfolder no permite. Si el anidamiento hiciera falta igual, agregar
`parent_id TEXT` a `folders` es una migración aditiva más.

**Orden alfabético por nombre, no manual.** Sin campo `position` hasta que haga
falta. Es estable por construcción, que es lo que importa: la sidebar no se
reordena sola, por la misma razón por la que no se ordena por actividad
reciente.

**Las sesiones sin folder no son un caso especial del modelo.** `folder_id`
NULL ya significa eso y no hace falta un folder "Sin proyecto" real; la UI lo
muestra como un grupo más, al final.

## Modelo de datos

Migración `schemaV3`, aditiva:

```sql
CREATE TABLE folders (
  id         TEXT PRIMARY KEY,
  name       TEXT NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE INDEX idx_sessions_folder ON sessions(folder_id);
```

`sessions.folder_id TEXT` ya existe desde `schemaV1`, reservado justamente para
esto. No se toca.

**Sin foreign key, a propósito.** Agregarle una FK a `sessions.folder_id`
obligaría a recrear la tabla, y eso rompe la regla de que las migraciones sean
aditivas: el daemon viejo sigue con la base abierta y escribiendo mientras el
orquestador nuevo migra. La integridad la mantiene la aplicación — borrar un
folder pone en NULL el `folder_id` de sus sesiones, en la misma transacción.

Un `folder_id` que apunta a un folder que ya no existe se trata como NULL al
leer, así nada se vuelve invisible si alguna vez quedara una fila colgada.

## Backend

**Store** (`internal/store/folders.go`): `CreateFolder`, `ListFolders`,
`RenameFolder`, `DeleteFolder` (que además limpia las sesiones en la misma
transacción) y `SetSessionFolder`.

**Nombres**: se recortan los espacios de los extremos, no pueden quedar vacíos,
y son únicos **sin distinguir mayúsculas** — "Iceberg" e "iceberg" son el mismo
folder. Sin eso, crear por MCP y crear por UI terminan produciendo duplicados
que se ven idénticos en pantalla. La unicidad la garantiza un índice:

```sql
CREATE UNIQUE INDEX idx_folders_name ON folders(name COLLATE NOCASE);
```

**Control** (`internal/control/manager.go`): los métodos equivalentes, que
publican al bus de eventos después de que el store confirma, igual que todo lo
demás.

**Eventos**: tres kinds nuevos en `internal/events` — `folder.created`,
`folder.updated`, `folder.deleted`. Mover una sesión ya emite `session.updated`,
que la UI sabe interpretar.

**API REST**:

| Método | Ruta | Cuerpo |
|---|---|---|
| `GET` | `/api/folders` | — |
| `POST` | `/api/folders` | `{name}` |
| `PATCH` | `/api/folders/{id}` | `{name}` |
| `DELETE` | `/api/folders/{id}` | — |

Mover una sesión no estrena endpoint: `PATCH /api/sessions/{id}` acepta
`folder_id` (con `null` para sacarla de su folder).

## MCP

Tres tools nuevas:

- **`list_folders`** — devuelve los folders con su nombre, id y cuántas
  sesiones tiene cada uno. Existe sobre todo para que Claude use los que ya
  hay: sin esto inventa "Iceberg" cuando ya existe "iceberg-migration".
- **`create_folder(name)`** — falla si ya existe uno con ese nombre (sin
  distinguir mayúsculas), y en el error devuelve el id y el nombre del
  existente, para que la respuesta natural sea usar ese en vez de reintentar
  con una variante.
- **`move_session(folder, session_id?)`** — mueve la sesión (por defecto, la
  que llama) al folder. `folder` acepta id o nombre exacto; con `null` la saca
  del folder. No crea folders: para eso está `create_folder`, y así "movelo a
  Iceberg" mal escrito falla en vez de crear un duplicado silencioso.

Borrar folders no se expone por MCP. Es la única operación destructiva del
conjunto y se hace desde la UI, donde se ve qué se está borrando.

## UI

**Sidebar agrupada.** Cada folder es un encabezado con su nombre, la cantidad
de sesiones y un triángulo de colapso; debajo van sus sesiones. Las que no
tienen folder van en un grupo "Sin proyecto" al final. El estado de colapso de
cada folder se guarda en `localStorage`, junto al ancho y el colapso de la
sidebar.

Con cero folders la sidebar se ve exactamente como hoy: sin encabezados, sin
"Sin proyecto". La agrupación aparece recién cuando existe el primer folder.

**Crear, renombrar y borrar.** Renombrar es doble click en el encabezado, igual
que en las sesiones. Click derecho sobre el encabezado abre el mismo tipo de
menú que las filas, con Renombrar y Borrar. Crear un folder se hace desde
"Mover a › Nuevo folder…", que lo crea y mueve la sesión en un paso: un folder
vacío no le sirve a nadie, y ese es el momento real en que uno lo necesita.

Borrar pide confirmación y aclara que las sesiones no se borran, solo salen del
folder.

**Mover una sesión.** El menú `⋯` de cada fila suma "Mover a ›" con la lista de
folders, "Sin proyecto" y "Nuevo folder…".

**Drag & drop.** Arrastrar una fila sobre el encabezado de un folder la mueve.
Es lo último que se construye, de manera que si se complica, todo lo demás ya
funciona. Un folder colapsado se resalta igual al pasar por encima y acepta el
drop sin expandirse.

**Buscador.** El palette suma una sección de folders, separada de las sesiones.
Elegir un folder no salta a ningún lado: acota la búsqueda a ese folder y lo
muestra como un chip delante del input, para poder seguir escribiendo el nombre
de la sesión. `Backspace` con el input vacío saca el chip. Los resultados
siguen ordenados con el mismo criterio estable de la fase 1.

## Orden de implementación

1. Store + migración
2. Control + eventos + API REST
3. MCP tools
4. Sidebar agrupada (sin mover nada todavía)
5. "Mover a" en el menú, más crear/renombrar/borrar
6. Folders en el buscador
7. Drag & drop

Cada paso deja la app usable. Del 4 en adelante ya se ve algo en pantalla.

## Verificación

Tests de Go para store, control y los handlers, siguiendo lo que ya hay. El
frontend se verifica con `tsc -b` y a mano, como el resto del proyecto.

Punto específico a probar a mano: que con cero folders la sidebar quede igual
que antes, porque es el estado en el que va a estar la primera vez que se abra.
