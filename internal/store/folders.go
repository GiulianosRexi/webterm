package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrNombreVacio lo devuelven CreateFolder y RenameFolder cuando el nombre no
// tiene nada más que espacios. Un folder sin nombre no se puede ni elegir en
// la UI ni nombrar por MCP.
var ErrNombreVacio = errors.New("el nombre no puede estar vacío")

// Folder agrupa sesiones. Un folder es un proyecto: no se anidan.
type Folder struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	CreatedAt int64  `json:"created_at"`
}

// normalizeName recorta los espacios de los extremos y rechaza lo que quede
// vacío. Se aplica en toda entrada para que " Iceberg " y "Iceberg" no puedan
// convivir.
func normalizeName(name string) (string, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return "", ErrNombreVacio
	}
	return trimmed, nil
}

// CreateFolder crea el folder y completa ID y CreatedAt.
func (s *Store) CreateFolder(name string) (*Folder, error) {
	clean, err := normalizeName(name)
	if err != nil {
		return nil, err
	}

	f := &Folder{ID: NewID(), Name: clean, CreatedAt: time.Now().UnixMilli()}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.Exec(
		`INSERT INTO folders (id, name, created_at) VALUES (?, ?, ?)`,
		f.ID, f.Name, f.CreatedAt,
	); err != nil {
		// Igual que en AddResource: modernc/sqlite no expone un código tipado
		// para la violación de UNIQUE, así que se reconoce por el texto.
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return nil, ErrDuplicate
		}
		return nil, fmt.Errorf("creando el folder %q: %w", clean, err)
	}
	return f, nil
}

// ListFolders devuelve los folders por nombre.
//
// Alfabético y no por fecha de creación: el orden tiene que ser estable entre
// una carga y la siguiente, por lo mismo que la lista de sesiones no se ordena
// por actividad reciente —una lista que se mueve sola arruina la memoria
// espacial de quien la mira.
func (s *Store) ListFolders() ([]*Folder, error) {
	rows, err := s.db.Query(
		`SELECT id, name, created_at FROM folders ORDER BY name COLLATE NOCASE`)
	if err != nil {
		return nil, fmt.Errorf("listando folders: %w", err)
	}
	defer rows.Close()

	out := []*Folder{}
	for rows.Next() {
		f := &Folder{}
		if err := rows.Scan(&f.ID, &f.Name, &f.CreatedAt); err != nil {
			return nil, fmt.Errorf("leyendo folder: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// RenameFolder le cambia el nombre.
func (s *Store) RenameFolder(id, name string) error {
	clean, err := normalizeName(name)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`UPDATE folders SET name = ? WHERE id = ?`, clean, id)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return ErrDuplicate
		}
		return fmt.Errorf("renombrando el folder %s: %w", id, err)
	}
	return rowsTouched(res)
}

// DeleteFolder borra el folder y saca del folder a sus sesiones, sin borrarlas.
//
// Las dos cosas van en la misma transacción: si se borrara el folder y fallara
// la limpieza, las sesiones quedarían apuntando a un folder que ya no existe
// —y no hay foreign key que lo impida, por lo que explica schemaV3.
func (s *Store) DeleteFolder(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("borrando el folder %s: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`UPDATE sessions SET folder_id = NULL WHERE folder_id = ?`, id); err != nil {
		return fmt.Errorf("sacando las sesiones del folder %s: %w", id, err)
	}
	res, err := tx.Exec(`DELETE FROM folders WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("borrando el folder %s: %w", id, err)
	}
	if err := rowsTouched(res); err != nil {
		return err
	}
	return tx.Commit()
}

// SetSessionFolder mueve una sesión a un folder, o la saca de todos con nil.
//
// Verifica que el folder exista antes de escribir: sin foreign key, esta es la
// única barrera contra dejar una sesión apuntando a la nada.
func (s *Store) SetSessionFolder(sessionID string, folderID *string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if folderID != nil {
		var existe int
		err := s.db.QueryRow(`SELECT 1 FROM folders WHERE id = ?`, *folderID).Scan(&existe)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("verificando el folder %s: %w", *folderID, err)
		}
	}

	res, err := s.db.Exec(
		`UPDATE sessions SET folder_id = ? WHERE id = ?`, folderID, sessionID)
	if err != nil {
		return fmt.Errorf("moviendo la sesión %s: %w", sessionID, err)
	}
	return rowsTouched(res)
}
