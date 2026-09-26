package store

// migrations se aplican en orden; el índice + 1 es el número de versión.
// Nunca se edita una migración ya publicada: se agrega otra al final.
//
// Desde M10 hay una restricción más, que no es de estilo sino del diseño de dos
// procesos: **las migraciones tienen que ser aditivas**. El flujo canónico de
// desarrollo es tocar control + store, recompilar y reiniciar SOLO el
// orquestador, dejando vivo al daemon con las sesiones —es literalmente lo que
// M10 vino a comprar—. Ese reinicio corre las migraciones sobre una base que un
// daemon VIEJO tiene abierta y sigue escribiendo: el daemon no se enteró de
// nada y sus queries son las de antes.
//
// Agregar una tabla, un índice o una columna nueva es invisible para él y anda.
// Renombrar o borrar una columna, cambiar un tipo o poner un CHECK más estricto
// rompe al daemon vivo en pleno uso, y el síntoma sale por el lado más
// confuso posible: el historial de las sesiones dejando de guardarse, o los
// reaps fallando, mientras el orquestador nuevo se ve perfecto.
//
// Si una migración destructiva es inevitable, el cambio incluye subir
// daemon.ProtocolVersion y avisar que hace falta `webterm daemon restart`
// —que mata las sesiones, y por eso es explícito—. Lo mismo está resumido en el
// README, en "Arquitectura: daemon y orquestador".
var migrations = []string{schemaV1, schemaV2, schemaV3, schemaV4}

// schemaV1 define el modelo completo de sesión del diseño, incluidos los
// campos que la UI de M2 todavía no usa (folder_id es M4, work_status y
// kanban_status son M6/M7). Dejarlos afuera obligaría a migrar después sin
// ganar nada ahora.
const schemaV1 = `
CREATE TABLE sessions (
  id             TEXT PRIMARY KEY,
  title          TEXT    NOT NULL DEFAULT '',
  description    TEXT    NOT NULL DEFAULT '',
  folder_id      TEXT,
  cwd            TEXT    NOT NULL,
  shell          TEXT    NOT NULL,
  cols           INTEGER NOT NULL,
  rows           INTEGER NOT NULL,
  pty_status     TEXT    NOT NULL,
  exit_reason    TEXT,
  exit_code      INTEGER,
  work_status    TEXT    NOT NULL DEFAULT 'idle',
  kanban_status  TEXT    NOT NULL DEFAULT 'todo',
  created_at     INTEGER NOT NULL,
  last_active_at INTEGER NOT NULL,
  exited_at      INTEGER
);

CREATE TABLE session_kv (
  session_id TEXT    NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  key        TEXT    NOT NULL,
  value      TEXT    NOT NULL,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (session_id, key)
);

CREATE TABLE session_output (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id TEXT    NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  data       BLOB    NOT NULL,
  created_at INTEGER NOT NULL
);

CREATE INDEX idx_output_session ON session_output(session_id, id);
CREATE INDEX idx_sessions_status ON sessions(pty_status);
`

// schemaV2 agrega los recursos externos linkeados a una sesión (M8).
//
// Es tabla y no un campo JSON en sessions por la búsqueda inversa: dado un PR
// hay que resolver qué sesiones se prenden, y sobre un blob eso sería un scan
// completo de sessions más parsear cada fila.
const schemaV2 = `
CREATE TABLE session_resources (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id TEXT    NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  system     TEXT    NOT NULL,
  type       TEXT    NOT NULL,
  ref        TEXT    NOT NULL,
  created_at INTEGER NOT NULL,
  UNIQUE (session_id, ref)
);

CREATE INDEX idx_resources_ref ON session_resources(ref);
CREATE INDEX idx_resources_session ON session_resources(session_id);
`

// schemaV3 agrega los folders de M4. sessions.folder_id ya existe desde
// schemaV1, así que esto es puramente aditivo y un daemon viejo no se entera.
//
// Sin foreign key contra sessions a propósito: agregársela obligaría a recrear
// la tabla sessions, que es exactamente la clase de migración destructiva que
// el comentario de arriba prohíbe. La consistencia la mantiene DeleteFolder,
// que saca a las sesiones del folder en la misma transacción en que lo borra.
//
// El índice único es NOCASE para que "Iceberg" e "iceberg" sean el mismo
// folder: si no, crear desde la UI y crear desde el MCP terminan produciendo
// dos que en pantalla se ven idénticos.
const schemaV3 = `
CREATE TABLE folders (
  id         TEXT    PRIMARY KEY,
  name       TEXT    NOT NULL,
  created_at INTEGER NOT NULL
);

CREATE UNIQUE INDEX idx_folders_name ON folders(name COLLATE NOCASE);
CREATE INDEX idx_sessions_folder ON sessions(folder_id);
`

// schemaV4 agrega los tags por sesión: el tipo de trabajo —bugfix, consulta,
// implementación—, varios por sesión y transversales al folder.
//
// Tabla aparte y no una columna en sessions por dos motivos: agregar columnas a
// sessions obliga a tocar sessionColumns, que es lo que lee el daemon; y lo que
// hay que contestar seguido es la pregunta inversa —qué tags existen y cuánto
// se usa cada uno, para el autocompletado—, que sobre un campo serializado
// sería parsear todas las filas.
//
// Acá sí va foreign key con cascada, como en session_kv: la tabla es nueva, así
// que no hay que recrear nada, y borrar una sesión se lleva sus tags solo. El
// tag se guarda ya normalizado (ver NormalizeTag), por eso no hace falta NOCASE.
const schemaV4 = `
CREATE TABLE session_tags (
  session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  tag        TEXT NOT NULL,
  PRIMARY KEY (session_id, tag)
);

CREATE INDEX idx_session_tags_tag ON session_tags(tag);
`
