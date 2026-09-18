// Package store persiste el estado de las sesiones en SQLite: metadata, KV
// por sesión e historial de output.
//
// No conoce ptys ni WebSockets a propósito: es una capa de datos pura, para
// poder probarla contra una DB de verdad sin levantar procesos.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	_ "modernc.org/sqlite"
)

// ErrNotFound lo devuelven los getters cuando la fila no existe.
var ErrNotFound = errors.New("no encontrado")

// Store es el acceso a la base.
type Store struct {
	db *sql.DB
	// mu serializa las escrituras. Con WAL los lectores nunca se bloquean,
	// pero SQLite admite un solo escritor: es más barato esperar acá que
	// comerse un SQLITE_BUSY y reintentar.
	mu sync.Mutex
}

// Open abre (y crea si hace falta) la base en path y aplica las migraciones
// pendientes.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("creando %s: %w", dir, err)
		}
	}

	// Los pragmas van en el DSN porque en SQLite son por conexión: el pool de
	// database/sql abre varias y todas tienen que arrancar igual.
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("abriendo %s: %w", path, err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("conectando a %s: %w", path, err)
	}

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close cierra la base.
func (s *Store) Close() error { return s.db.Close() }

// DB expone la conexión cruda. Es para los tests y para nada más.
func (s *Store) DB() *sql.DB { return s.db }

// migrate aplica en orden las migraciones que falten, cada una junto con su
// marca de versión dentro de la misma transacción.
func (s *Store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER PRIMARY KEY)`); err != nil {
		return fmt.Errorf("creando schema_version: %w", err)
	}

	var current int
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&current); err != nil {
		return fmt.Errorf("leyendo schema_version: %w", err)
	}

	for i, stmt := range migrations {
		version := i + 1
		if version <= current {
			continue
		}
		tx, err := s.db.Begin()
		if err != nil {
			return fmt.Errorf("migración %d: %w", version, err)
		}
		if _, err := tx.Exec(stmt); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migración %d: %w", version, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_version (version) VALUES (?)`, version); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migración %d: marcando versión: %w", version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("migración %d: commit: %w", version, err)
		}
	}
	return nil
}
