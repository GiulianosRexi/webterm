package store

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrDuplicate lo devuelve AddResource cuando el recurso ya está linkeado a
// esa sesión.
var ErrDuplicate = errors.New("ya existe")

// Resource es un recurso externo linkeado a una sesión. Solo guarda el link:
// el estado traído del sistema externo es caché en memoria y no se persiste.
type Resource struct {
	ID        int64  `json:"id"`
	SessionID string `json:"session_id"`
	System    string `json:"system"`
	Type      string `json:"type"`
	Ref       string `json:"ref"`
	CreatedAt int64  `json:"created_at"`
}

// AddResource linkea el recurso y completa ID y CreatedAt.
func (s *Store) AddResource(r *Resource) error {
	if r.CreatedAt == 0 {
		r.CreatedAt = time.Now().UnixMilli()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`
		INSERT INTO session_resources (session_id, system, type, ref, created_at)
		VALUES (?, ?, ?, ?, ?)`,
		r.SessionID, r.System, r.Type, r.Ref, r.CreatedAt)
	if err != nil {
		// modernc/sqlite no expone un código tipado para la violación de
		// UNIQUE, así que se reconoce por el texto del error.
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return ErrDuplicate
		}
		return fmt.Errorf("linkeando %s a %s: %w", r.Ref, r.SessionID, err)
	}
	r.ID, err = res.LastInsertId()
	return err
}

// ListResources devuelve los recursos de una sesión, el más viejo primero: el
// orden en que se fueron linkeando es el que tiene sentido para leerlos.
func (s *Store) ListResources(sessionID string) ([]*Resource, error) {
	rows, err := s.db.Query(`
		SELECT id, session_id, system, type, ref, created_at
		FROM session_resources WHERE session_id = ? ORDER BY id`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("listando recursos de %s: %w", sessionID, err)
	}
	defer rows.Close()

	out := []*Resource{}
	for rows.Next() {
		var r Resource
		if err := rows.Scan(&r.ID, &r.SessionID, &r.System, &r.Type, &r.Ref, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}

// DeleteResource borra el link. Filtra por sesión además de por id: el id es
// global, y sin ese filtro una sesión podría borrar recursos de otra.
func (s *Store) DeleteResource(sessionID string, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.execAffecting(`DELETE FROM session_resources WHERE session_id = ? AND id = ?`,
		sessionID, id)
}

// SessionsForRef devuelve las sesiones que tienen linkeado ese recurso. Es la
// búsqueda inversa que va a necesitar cualquier notificación entrante.
func (s *Store) SessionsForRef(ref string) ([]string, error) {
	rows, err := s.db.Query(`
		SELECT session_id FROM session_resources WHERE ref = ? ORDER BY session_id`, ref)
	if err != nil {
		return nil, fmt.Errorf("buscando sesiones de %s: %w", ref, err)
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
