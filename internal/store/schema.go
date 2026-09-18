package store

// migrations se aplican en orden; el índice + 1 es el número de versión.
// Nunca se edita una migración ya publicada: se agrega otra al final.
var migrations = []string{schemaV1, schemaV2}

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
