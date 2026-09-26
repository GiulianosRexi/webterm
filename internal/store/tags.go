package store

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// ErrInvalidTag lo devuelven las escrituras de tags cuando uno no sobrevive a
// la normalización: vacío o demasiado largo.
var ErrInvalidTag = errors.New("tag inválido")

// maxTagLen acota el largo de un tag. Un tag es una etiqueta de una o dos
// palabras; algo más largo es una descripción metida en el lugar equivocado.
const maxTagLen = 32

// TagCount es un tag junto con cuántas sesiones lo llevan.
type TagCount struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// NormalizeTag lleva un tag a su forma canónica: minúsculas, sin espacios en
// los extremos y con los de adentro convertidos en guiones.
//
// Es lo que evita que "Bugfix", "bugfix " y "bug fix" terminen siendo tres
// tags distintos: la mitad del problema la resuelve el autocompletado de la UI,
// y la otra mitad esto, que se aplica a toda escritura venga de donde venga.
func NormalizeTag(tag string) (string, error) {
	clean := strings.ToLower(strings.Join(strings.Fields(tag), "-"))
	if clean == "" || utf8.RuneCountInString(clean) > maxTagLen {
		return "", fmt.Errorf("%w: %q", ErrInvalidTag, tag)
	}
	return clean, nil
}

// normalizeTags normaliza una lista, saca repetidos y la ordena.
func normalizeTags(tags []string) ([]string, error) {
	seen := map[string]bool{}
	out := []string{}
	for _, t := range tags {
		clean, err := NormalizeTag(t)
		if err != nil {
			return nil, err
		}
		if !seen[clean] {
			seen[clean] = true
			out = append(out, clean)
		}
	}
	sort.Strings(out)
	return out, nil
}

// SetSessionTags reemplaza el conjunto entero de tags de la sesión.
//
// Borrar e insertar van en la misma transacción: si no, un lector concurrente
// podría ver la sesión sin tags en el medio.
func (s *Store) SetSessionTags(sessionID string, tags []string) error {
	clean, err := normalizeTags(tags)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("taggeando %s: %w", sessionID, err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := sessionExists(tx, sessionID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM session_tags WHERE session_id = ?`, sessionID); err != nil {
		return fmt.Errorf("limpiando los tags de %s: %w", sessionID, err)
	}
	for _, t := range clean {
		if _, err := tx.Exec(
			`INSERT INTO session_tags (session_id, tag) VALUES (?, ?)`, sessionID, t); err != nil {
			return fmt.Errorf("taggeando %s con %s: %w", sessionID, t, err)
		}
	}
	return tx.Commit()
}

// AddSessionTags suma tags sin tocar los que ya estaban. Es lo que usa el MCP:
// que Claude agregue "bugfix" no puede borrar los tags que puso el usuario.
func (s *Store) AddSessionTags(sessionID string, tags []string) error {
	clean, err := normalizeTags(tags)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("taggeando %s: %w", sessionID, err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := sessionExists(tx, sessionID); err != nil {
		return err
	}
	for _, t := range clean {
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO session_tags (session_id, tag) VALUES (?, ?)`, sessionID, t); err != nil {
			return fmt.Errorf("taggeando %s con %s: %w", sessionID, t, err)
		}
	}
	return tx.Commit()
}

// RemoveSessionTags saca esos tags de la sesión. Sacar uno que no tenía no es
// un error: el resultado es el que se pidió.
func (s *Store) RemoveSessionTags(sessionID string, tags []string) error {
	clean, err := normalizeTags(tags)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("sacando tags de %s: %w", sessionID, err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := sessionExists(tx, sessionID); err != nil {
		return err
	}
	for _, t := range clean {
		if _, err := tx.Exec(
			`DELETE FROM session_tags WHERE session_id = ? AND tag = ?`, sessionID, t); err != nil {
			return fmt.Errorf("sacando %s de %s: %w", t, sessionID, err)
		}
	}
	return tx.Commit()
}

// ListTags devuelve los tags en uso con cuántas sesiones lleva cada uno,
// alfabéticos. Un tag existe mientras alguna sesión lo tenga: no hay ABM de
// tags aparte, así que no pueden quedar tags huérfanos.
func (s *Store) ListTags() ([]TagCount, error) {
	rows, err := s.db.Query(
		`SELECT tag, COUNT(*) FROM session_tags GROUP BY tag ORDER BY tag`)
	if err != nil {
		return nil, fmt.Errorf("listando tags: %w", err)
	}
	defer rows.Close()

	out := []TagCount{}
	for rows.Next() {
		var tc TagCount
		if err := rows.Scan(&tc.Name, &tc.Count); err != nil {
			return nil, fmt.Errorf("leyendo tag: %w", err)
		}
		out = append(out, tc)
	}
	return out, rows.Err()
}

// sessionTags devuelve los tags de una sesión, alfabéticos. Nunca nil: en JSON
// una sesión sin tags es [], no null.
func (s *Store) sessionTags(sessionID string) ([]string, error) {
	rows, err := s.db.Query(
		`SELECT tag FROM session_tags WHERE session_id = ? ORDER BY tag`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("leyendo los tags de %s: %w", sessionID, err)
	}
	defer rows.Close()

	out := []string{}
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// allSessionTags devuelve los tags de todas las sesiones de una sola query,
// para que listar no sea una query por sesión.
func (s *Store) allSessionTags() (map[string][]string, error) {
	rows, err := s.db.Query(`SELECT session_id, tag FROM session_tags ORDER BY tag`)
	if err != nil {
		return nil, fmt.Errorf("leyendo tags: %w", err)
	}
	defer rows.Close()

	out := map[string][]string{}
	for rows.Next() {
		var id, t string
		if err := rows.Scan(&id, &t); err != nil {
			return nil, err
		}
		out[id] = append(out[id], t)
	}
	return out, rows.Err()
}

// sessionExists traduce una sesión inexistente a ErrNotFound. Hace falta
// aunque haya foreign key: sin esto, taggear una sesión que no existe saldría
// como un error de constraint genérico —un 500 en vez de un 404— y sacarle tags
// no fallaría nunca.
func sessionExists(tx *sql.Tx, sessionID string) error {
	var one int
	err := tx.QueryRow(`SELECT 1 FROM sessions WHERE id = ?`, sessionID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("verificando la sesión %s: %w", sessionID, err)
	}
	return nil
}
