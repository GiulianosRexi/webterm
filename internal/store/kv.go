package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SetKV guarda (o pisa) una clave del contexto persistido de la sesión. Es la
// superficie que va a consumir `webterm set state` en M5.
func (s *Store) SetKV(sessionID, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`
		INSERT INTO session_kv (session_id, key, value, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(session_id, key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		sessionID, key, value, time.Now().UnixMilli())
	if err != nil {
		return fmt.Errorf("guardando %s[%s]: %w", sessionID, key, err)
	}
	return nil
}

// GetKV devuelve una clave, o ErrNotFound.
func (s *Store) GetKV(sessionID, key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM session_kv WHERE session_id = ? AND key = ?`,
		sessionID, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return v, err
}

// ListKV devuelve todo el contexto de una sesión.
func (s *Store) ListKV(sessionID string) (map[string]string, error) {
	rows, err := s.db.Query(`SELECT key, value FROM session_kv WHERE session_id = ? ORDER BY key`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("listando kv de %s: %w", sessionID, err)
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// DeleteKV borra una clave, o devuelve ErrNotFound si no estaba.
func (s *Store) DeleteKV(sessionID, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.execAffecting(`DELETE FROM session_kv WHERE session_id = ? AND key = ?`, sessionID, key)
}
