package storage

import (
	"context"
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite" // driver side-effect registration
)

// Store owns the *sql.DB for one SQLite database (one "region"). All mutating
// ops run their statements on a single *sql.Tx obtained from BeginTx.
type Store struct {
	db *sql.DB
}

// Open registers the modernc driver (imported for side effect), opens the DSN,
// serializes access (MaxOpenConns=1), runs pragmas, and bootstraps the catalog
// tables if absent. One Store = one SQLite DB = one region.
func Open(ctx context.Context, dsn string) (*Store, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("storage: open %q: %w", dsn, err)
	}
	// Serialized single-writer semantics: at most one logical connection in
	// flight at a time; concurrent ops queue on the pool.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	if err := bootstrap(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// BeginTx wraps db.BeginTx; every mutating op runs all statements on one tx.
func (s *Store) BeginTx(ctx context.Context) (*sql.Tx, error) {
	return s.db.BeginTx(ctx, nil)
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

func bootstrap(ctx context.Context, db *sql.DB) error {
	// foreign_keys=ON matters from M1 for the ddb_gsi_defs FK to ddb_table_defs.
	if _, err := db.ExecContext(ctx, `PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON;`); err != nil {
		return fmt.Errorf("storage: pragmas: %w", err)
	}
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS ddb_table_defs (
  id INTEGER NOT NULL PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  hash TEXT NOT NULL,
  range TEXT,
  hash_type TEXT NOT NULL,
  range_type TEXT,
  ttl TEXT,
  meta TEXT NOT NULL
) STRICT`,
		`CREATE TABLE IF NOT EXISTS ddb_gsi_defs (
  table_id INTEGER NOT NULL REFERENCES ddb_table_defs (id),
  name TEXT NOT NULL,
  hash TEXT NOT NULL,
  range TEXT,
  hash_type TEXT NOT NULL,
  range_type TEXT,
  projection_type TEXT NOT NULL,
  projected TEXT,
  PRIMARY KEY (table_id, name)
) STRICT`,
	}
	for _, st := range stmts {
		if _, err := db.ExecContext(ctx, st); err != nil {
			return fmt.Errorf("storage: bootstrap catalog: %w", err)
		}
	}
	return nil
}
