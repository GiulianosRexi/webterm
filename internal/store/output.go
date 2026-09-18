package store

import (
	"fmt"
	"time"
)

// AppendOutput guarda un chunk de output crudo del pty. Los chunks se guardan
// enteros a propósito: al replayar nunca hay que cortar uno al medio, y cortar
// ANSI por el medio ensucia la pantalla del cliente.
func (s *Store) AppendOutput(sessionID string, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT INTO session_output (session_id, data, created_at) VALUES (?, ?, ?)`,
		sessionID, data, time.Now().UnixMilli())
	if err != nil {
		return fmt.Errorf("guardando output de %s: %w", sessionID, err)
	}
	return nil
}

// OutputBytes devuelve cuánto historial hay guardado para la sesión.
func (s *Store) OutputBytes(sessionID string) (int64, error) {
	var n int64
	err := s.db.QueryRow(`SELECT COALESCE(SUM(LENGTH(data)), 0) FROM session_output WHERE session_id = ?`,
		sessionID).Scan(&n)
	return n, err
}

// PruneOutput borra los chunks más viejos hasta que el historial entre en
// maxBytes. Recorre de lo más nuevo a lo más viejo sumando tamaños: con un cap
// de 1 MB y batches de 64 KB son unas pocas decenas de filas, así que sale
// más barato que una agregación con window functions.
//
// Siempre conserva al menos el chunk más nuevo, aunque él solo supere el cap:
// dejar el historial en cero le sacaría al cliente lo único con lo que puede
// redibujar la pantalla.
func (s *Store) PruneOutput(sessionID string, maxBytes int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	rows, err := s.db.Query(`SELECT id, LENGTH(data) FROM session_output WHERE session_id = ? ORDER BY id DESC`,
		sessionID)
	if err != nil {
		return fmt.Errorf("podando el historial de %s: %w", sessionID, err)
	}

	var kept int64
	var cutoff int64 // primer id (de los viejos) a borrar, junto con todo lo anterior
	first := true
	for rows.Next() {
		var id, size int64
		if err := rows.Scan(&id, &size); err != nil {
			_ = rows.Close()
			return err
		}
		if !first && kept+size > maxBytes {
			cutoff = id
			break
		}
		kept += size
		first = false
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()

	if cutoff == 0 {
		return nil
	}
	if _, err := s.db.Exec(`DELETE FROM session_output WHERE session_id = ? AND id <= ?`, sessionID, cutoff); err != nil {
		return fmt.Errorf("podando el historial de %s: %w", sessionID, err)
	}
	return nil
}

// ReadOutput devuelve el historial completo de la sesión, en orden.
func (s *Store) ReadOutput(sessionID string) ([]byte, error) {
	rows, err := s.db.Query(`SELECT data FROM session_output WHERE session_id = ? ORDER BY id`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("leyendo el historial de %s: %w", sessionID, err)
	}
	defer rows.Close()

	var out []byte
	for rows.Next() {
		var chunk []byte
		if err := rows.Scan(&chunk); err != nil {
			return nil, err
		}
		out = append(out, chunk...)
	}
	return out, rows.Err()
}
