package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// PtyStatus dice si el proceso de la sesión está vivo.
type PtyStatus string

const (
	// StatusStarting es la ventana entre que el orquestador inserta la fila y
	// el daemon confirma el spawn. No necesita migración: pty_status es un
	// TEXT sin CHECK.
	StatusStarting PtyStatus = "starting"
	StatusRunning  PtyStatus = "running"
	StatusExited   PtyStatus = "exited"
)

// ExitReason explica por qué murió una sesión. Importa distinguirlas: un
// `exit` del usuario y un reinicio del backend se ven igual en la UI si no
// se guarda el motivo.
type ExitReason string

const (
	ReasonNormal ExitReason = "normal"
	ReasonKilled ExitReason = "killed"
	// ReasonDaemonRestart es lo que le pasó a las sesiones cuando el daemon
	// arrancó de nuevo. Antes se llamaba backend_restart, cuando backend y
	// daemon eran el mismo proceso.
	ReasonDaemonRestart ExitReason = "daemon_restart"
	ReasonOrphaned      ExitReason = "orphaned"
	ReasonSpawnFailed   ExitReason = "spawn_failed"
)

// Session es la fila de una sesión. work_status, kanban_status y folder_id se
// persisten desde M2 aunque los usen M4/M6/M7.
type Session struct {
	ID           string    `json:"id"`
	Title        string    `json:"title"`
	Description  string    `json:"description"`
	FolderID     *string   `json:"folder_id"`
	Cwd          string    `json:"cwd"`
	Shell        string    `json:"shell"`
	Cols         int       `json:"cols"`
	Rows         int       `json:"rows"`
	PtyStatus    PtyStatus `json:"pty_status"`
	ExitReason   string    `json:"exit_reason,omitempty"`
	ExitCode     *int      `json:"exit_code,omitempty"`
	WorkStatus   string    `json:"work_status"`
	KanbanStatus string    `json:"kanban_status"`
	CreatedAt    int64     `json:"created_at"`
	LastActiveAt int64     `json:"last_active_at"`
	ExitedAt     *int64    `json:"exited_at,omitempty"`
	// Tags no es una columna de sessions sino de session_tags (ver schemaV4).
	// Los getters la completan; CreateSession la ignora.
	Tags []string `json:"tags"`
	// RunningAgents tampoco es columna: sale de session_agents (ver
	// schemaV5). Son los subagentes de Claude Code que siguen corriendo.
	RunningAgents int `json:"running_agents"`
}

// MetaPatch es un update parcial: los campos en nil no se tocan.
type MetaPatch struct {
	Title        *string
	Description  *string
	WorkStatus   *string
	KanbanStatus *string
}

var idSeq struct {
	mu sync.Mutex
	ms int64
	n  uint16
}

// NewID devuelve un id estrictamente ordenable por tiempo de creación: millis
// en base36 con padding fijo, más un contador que desempata las sesiones
// creadas dentro del mismo milisegundo. La lista se ordena por id, así que el
// orden tiene que ser total: con un sufijo aleatorio, dos sesiones del mismo
// milisegundo podían salir invertidas.
func NewID() string {
	now := time.Now().UnixMilli()

	idSeq.mu.Lock()
	if now != idSeq.ms {
		idSeq.ms, idSeq.n = now, 0
	} else {
		idSeq.n++
	}
	seq := idSeq.n
	idSeq.mu.Unlock()

	return fmt.Sprintf("%09s%04x", strconv.FormatInt(now, 36), seq)
}

const sessionColumns = `id, title, description, folder_id, cwd, shell, cols, rows,
	pty_status, exit_reason, exit_code, work_status, kanban_status,
	created_at, last_active_at, exited_at`

func scanSession(row interface{ Scan(...any) error }) (*Session, error) {
	var s Session
	var reason sql.NullString
	err := row.Scan(&s.ID, &s.Title, &s.Description, &s.FolderID, &s.Cwd, &s.Shell,
		&s.Cols, &s.Rows, &s.PtyStatus, &reason, &s.ExitCode,
		&s.WorkStatus, &s.KanbanStatus, &s.CreatedAt, &s.LastActiveAt, &s.ExitedAt)
	if err != nil {
		return nil, err
	}
	s.ExitReason = reason.String
	return &s, nil
}

// CreateSession inserta la fila y completa los timestamps que falten.
//
// Ojo con el default de PtyStatus: una fila sin estado explícito entra como
// running. Es legacy de M2, cuando el pty nacía en el mismo proceso que
// insertaba y la fila ya era cierta al volver de acá. Desde M10 el único que
// crea sesiones de verdad es control.Create, que SIEMPRE pasa starting porque
// el pty lo spawnea otro proceso y todavía no existe cuando esto corre.
//
// Se deja como está en vez de defaultear a starting porque cambiarlo ahora
// movería a todos los tests que arman filas a mano —que son los que se apoyan
// en el default— sin arreglar ningún camino de producción. Pero es una trampa:
// una fila creada sin estado queda diciendo que hay un pty detrás, y lo único
// que la corrige es el sweep, 30 s después. Si escribís código nuevo que
// inserta sesiones, poné el estado explícito.
func (s *Store) CreateSession(sess *Session) error {
	now := time.Now().UnixMilli()
	if sess.CreatedAt == 0 {
		sess.CreatedAt = now
	}
	if sess.LastActiveAt == 0 {
		sess.LastActiveAt = now
	}
	if sess.PtyStatus == "" {
		sess.PtyStatus = StatusRunning
	}
	if sess.WorkStatus == "" {
		sess.WorkStatus = "idle"
	}
	if sess.KanbanStatus == "" {
		sess.KanbanStatus = "todo"
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`
		INSERT INTO sessions (`+sessionColumns+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		sess.ID, sess.Title, sess.Description, sess.FolderID, sess.Cwd, sess.Shell,
		sess.Cols, sess.Rows, sess.PtyStatus, nullString(sess.ExitReason), sess.ExitCode,
		sess.WorkStatus, sess.KanbanStatus, sess.CreatedAt, sess.LastActiveAt, sess.ExitedAt)
	if err != nil {
		return fmt.Errorf("insertando sesión %s: %w", sess.ID, err)
	}
	return nil
}

// GetSession devuelve una sesión por id, o ErrNotFound.
func (s *Store) GetSession(id string) (*Session, error) {
	row := s.db.QueryRow(`SELECT `+sessionColumns+` FROM sessions WHERE id = ?`, id)
	sess, err := scanSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("leyendo sesión %s: %w", id, err)
	}
	if sess.Tags, err = s.sessionTags(id); err != nil {
		return nil, err
	}
	if sess.RunningAgents, err = s.runningAgents(id); err != nil {
		return nil, err
	}
	return sess, nil
}

// ListSessions devuelve todas las sesiones, la más nueva primero.
func (s *Store) ListSessions() ([]*Session, error) {
	rows, err := s.db.Query(`SELECT ` + sessionColumns + ` FROM sessions ORDER BY id DESC`)
	if err != nil {
		return nil, fmt.Errorf("listando sesiones: %w", err)
	}
	defer rows.Close()

	out := []*Session{}
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, fmt.Errorf("listando sesiones: %w", err)
		}
		out = append(out, sess)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	tags, err := s.allSessionTags()
	if err != nil {
		return nil, err
	}
	agents, err := s.allRunningAgents()
	if err != nil {
		return nil, err
	}
	for _, sess := range out {
		if sess.Tags = tags[sess.ID]; sess.Tags == nil {
			sess.Tags = []string{}
		}
		sess.RunningAgents = agents[sess.ID]
	}
	return out, nil
}

// UpdateMeta aplica un update parcial sobre los campos administrativos.
func (s *Store) UpdateMeta(id string, p MetaPatch) error {
	sets := []string{}
	args := []any{}
	if p.Title != nil {
		sets, args = append(sets, "title = ?"), append(args, *p.Title)
	}
	if p.Description != nil {
		sets, args = append(sets, "description = ?"), append(args, *p.Description)
	}
	if p.WorkStatus != nil {
		sets, args = append(sets, "work_status = ?"), append(args, *p.WorkStatus)
	}
	if p.KanbanStatus != nil {
		sets, args = append(sets, "kanban_status = ?"), append(args, *p.KanbanStatus)
	}
	if len(sets) == 0 {
		// Un PATCH vacío igual tiene que distinguir "no existe" de "nada que hacer".
		_, err := s.GetSession(id)
		return err
	}
	args = append(args, id)

	s.mu.Lock()
	defer s.mu.Unlock()
	return s.execAffecting(`UPDATE sessions SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)
}

// MarkExited registra la muerte del proceso: estado, motivo, código y momento.
func (s *Store) MarkExited(id string, reason ExitReason, code *int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UnixMilli()
	return s.execAffecting(`
		UPDATE sessions
		SET pty_status = ?, exit_reason = ?, exit_code = ?, exited_at = ?, last_active_at = ?
		WHERE id = ?`,
		StatusExited, string(reason), code, now, now, id)
}

// MarkExitedIfUnchanged marca la muerte solo si la fila sigue exactamente como
// la leyó ActiveSessions, y dice si le tocó a ella hacerlo.
//
// Es MarkExited con un compare-and-swap en el mismo UPDATE, y ese CAS es lo
// único que hace atómico al sweep del orquestador. Cubre dos carreras
// distintas, y ninguna de las dos se puede cerrar chequeando antes con un
// SELECT —eso achica la ventana, no la elimina—:
//
//   - la sesión murió mientras el sweep corría: su exit_reason real —normal,
//     killed— ya lo escribió el reap, y sin el CAS el sweep se lo pisaría con
//     su motivo genérico. Dato corrupto que el usuario ve en la UI.
//   - la sesión NACIÓ mientras el sweep corría: entró en ActiveSessions en
//     starting y todavía no estaba en LiveIDs, pero para cuando el sweep va a
//     marcarla ya tiene pty y la fila dice running. Sin el CAS el sweep mata en
//     la base una sesión viva, y eso no se autorrepara (ver el test
//     TestSweepNoMataUnaSesionQueEstaNaciendo en internal/control).
//
// El CAS compara las dos columnas que describen "en qué momento de su vida
// estaba esta fila": pty_status y last_active_at. Alcanzaría con la segunda
// —TODA transición de estado la bumpea: MarkRunning, MarkStarting, MarkExited y
// esta misma— pero last_active_at tiene resolución de milisegundo, así que dos
// escrituras del mismo milisegundo se ven iguales. Sumar pty_status tapa
// justamente el caso que trajo el bug (starting → running), que es el que puede
// pasar rápido. Para colarse ahora hay que terminar en el mismo estado Y en el
// mismo milisegundo.
//
// La contracara del CAS: cualquier escritura inocente sobre la fila —un
// TouchActive de un attach, un UpdateSize— hace que este barrido no la toque.
// Es el lado seguro del error y se corrige solo en el barrido siguiente, 30 s
// después.
//
// Cero filas afectadas NO es ErrNotFound: es el caso normal de "la fila cambió"
// (o de una borrada mientras tanto), y por eso la firma devuelve un bool en vez
// de mentir con un error.
func (s *Store) MarkExitedIfUnchanged(prev ActiveSession, reason ExitReason, code *int) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UnixMilli()
	res, err := s.db.Exec(`
		UPDATE sessions
		SET pty_status = ?, exit_reason = ?, exit_code = ?, exited_at = ?, last_active_at = ?
		WHERE id = ? AND pty_status = ? AND last_active_at = ?`,
		StatusExited, string(reason), code, now, now,
		prev.ID, prev.PtyStatus, prev.LastActiveAt)
	if err != nil {
		return false, fmt.Errorf("marcando la salida de %s: %w", prev.ID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// MarkRunning vuelve a marcar la sesión como viva y borra los rastros de la
// salida anterior, para que la UI no muestre un exit code junto a una sesión
// que está corriendo.
func (s *Store) MarkRunning(id string, cols, rows int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.execAffecting(`
		UPDATE sessions
		SET pty_status = ?, exit_reason = NULL, exit_code = NULL, exited_at = NULL,
		    cols = ?, rows = ?, last_active_at = ?
		WHERE id = ?`,
		StatusRunning, cols, rows, time.Now().UnixMilli(), id)
}

// UpdateSize persiste el último tamaño conocido de la terminal, para que una
// sesión reanudada vuelva con las dimensiones que tenía.
func (s *Store) UpdateSize(id string, cols, rows int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.execAffecting(`UPDATE sessions SET cols = ?, rows = ?, last_active_at = ? WHERE id = ?`,
		cols, rows, time.Now().UnixMilli(), id)
}

// TouchActive actualiza la marca de última actividad.
func (s *Store) TouchActive(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.execAffecting(`UPDATE sessions SET last_active_at = ? WHERE id = ?`,
		time.Now().UnixMilli(), id)
}

// DeleteSession borra la sesión, su KV y su historial (por la cascada).
func (s *Store) DeleteSession(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.execAffecting(`DELETE FROM sessions WHERE id = ?`, id)
}

// ActiveSession es una fila que la base cree con proceso detrás, junto con lo
// que hace falta para detectar que cambió después de leerla.
//
// No es un Session entero a propósito: es la entrada de un CAS, y lo único que
// le compete son las columnas contra las que se compara.
type ActiveSession struct {
	ID           string
	PtyStatus    PtyStatus
	LastActiveAt int64
}

// ActiveSessions devuelve las filas que la DB cree con proceso detrás: las que
// están corriendo y las que están arrancando. El sweep del orquestador las
// contrasta contra lo que el daemon dice tener vivo.
//
// Incluir starting no es cosmético: una fila que quedó ahí porque el
// orquestador crasheó entre el insert y el spawn no la levantaría nadie más.
// Pero es también lo que obliga a devolver pty_status y last_active_at y no
// solo el id: una fila en starting puede convertirse en una sesión viva entre
// esta lectura y el UPDATE del sweep, y el CAS de MarkExitedIfUnchanged es lo
// que evita que el sweep la mate. Antes esto era ActiveIDs y devolvía ids
// pelados, que es información insuficiente para escribir sin riesgo.
func (s *Store) ActiveSessions() ([]ActiveSession, error) {
	rows, err := s.db.Query(
		`SELECT id, pty_status, last_active_at FROM sessions WHERE pty_status IN (?, ?)`,
		StatusRunning, StatusStarting)
	if err != nil {
		return nil, fmt.Errorf("listando sesiones activas: %w", err)
	}
	defer rows.Close()

	out := []ActiveSession{}
	for rows.Next() {
		var a ActiveSession
		if err := rows.Scan(&a.ID, &a.PtyStatus, &a.LastActiveAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// MarkStarting deja la fila lista para que el daemon la spawnee, borrando los
// rastros de la salida anterior para que la UI no muestre un exit code al lado
// de una sesión que está arrancando. También vuelve work_status a idle y
// olvida los subagentes: el Claude que los había movido murió con el pty viejo.
func (s *Store) MarkStarting(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.Exec(`DELETE FROM session_agents WHERE session_id = ?`, id); err != nil {
		return fmt.Errorf("limpiando subagentes de %s: %w", id, err)
	}
	return s.execAffecting(`
		UPDATE sessions
		SET pty_status = ?, exit_reason = NULL, exit_code = NULL, exited_at = NULL,
		    work_status = 'idle', last_active_at = ?
		WHERE id = ?`,
		StatusStarting, time.Now().UnixMilli(), id)
}

// execAffecting corre un statement que tiene que tocar exactamente una fila y
// traduce "ninguna fila" a ErrNotFound.
func (s *Store) execAffecting(query string, args ...any) error {
	res, err := s.db.Exec(query, args...)
	if err != nil {
		return err
	}
	return rowsTouched(res)
}

// rowsTouched traduce "ninguna fila" a ErrNotFound. Está separado de
// execAffecting porque hay statements que necesitan correr su propio Exec: los
// que distinguen la violación de UNIQUE por el texto del error, y los que van
// adentro de una transacción.
func rowsTouched(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func nullString(v string) any {
	if v == "" {
		return nil
	}
	return v
}
