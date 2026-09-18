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
	return out, rows.Err()
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

// MarkExitedIfActive marca la muerte solo si la fila todavía se cree activa, y
// dice si le tocó a ella hacerlo.
//
// Es MarkExited con una guarda en el mismo UPDATE, y la guarda es lo único que
// hace atómico al sweep del orquestador: sin ella, una sesión que muere
// mientras el sweep corre termina con su exit_reason real —normal, killed—
// pisado por el motivo genérico del sweep, y eso es un dato corrupto que el
// usuario ve en la UI. Chequear antes con un SELECT achica la ventana pero no
// la cierra; esto la elimina.
//
// Cero filas afectadas NO es ErrNotFound: es el caso normal de "alguien llegó
// antes" (o de una fila borrada mientras tanto), y por eso la firma devuelve un
// bool en vez de mentir con un error.
func (s *Store) MarkExitedIfActive(id string, reason ExitReason, code *int) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UnixMilli()
	res, err := s.db.Exec(`
		UPDATE sessions
		SET pty_status = ?, exit_reason = ?, exit_code = ?, exited_at = ?, last_active_at = ?
		WHERE id = ? AND pty_status IN (?, ?)`,
		StatusExited, string(reason), code, now, now, id, StatusRunning, StatusStarting)
	if err != nil {
		return false, fmt.Errorf("marcando la salida de %s: %w", id, err)
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

// ActiveIDs devuelve los ids que la DB cree con proceso detrás: los que están
// corriendo y los que están arrancando. El sweep del orquestador los contrasta
// contra lo que el daemon dice tener vivo.
//
// Incluir starting no es cosmético: una fila que quedó ahí porque el
// orquestador crasheó entre el insert y el spawn no la levantaría nadie más.
func (s *Store) ActiveIDs() ([]string, error) {
	rows, err := s.db.Query(`SELECT id FROM sessions WHERE pty_status IN (?, ?)`,
		StatusRunning, StatusStarting)
	if err != nil {
		return nil, fmt.Errorf("listando sesiones activas: %w", err)
	}
	defer rows.Close()

	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// MarkStarting deja la fila lista para que el daemon la spawnee, borrando los
// rastros de la salida anterior para que la UI no muestre un exit code al lado
// de una sesión que está arrancando.
func (s *Store) MarkStarting(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.execAffecting(`
		UPDATE sessions
		SET pty_status = ?, exit_reason = NULL, exit_code = NULL, exited_at = NULL,
		    last_active_at = ?
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
