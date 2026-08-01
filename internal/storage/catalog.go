package storage

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrNotFound is returned by catalog lookups when no row matches. The ddb
// engine maps it to ddb.ErrTableNotFound.
var ErrNotFound = errors.New("storage: not found")

// TableDef mirrors a ddb_table_defs catalog row. Meta is the raw JSON metadata
// blob (class, creationTime, ...). ID is the catalog primary key.
type TableDef struct {
	ID        int64
	Name      string
	Hash      string
	Range     string
	HashType  string
	RangeType string
	TTL       string
	Meta      json.RawMessage
}

func (s *Store) InsertTableDef(tx *sql.Tx, def TableDef) (int64, error) {
	res, err := tx.Exec(
		`INSERT INTO ddb_table_defs (name, hash, range, hash_type, range_type, ttl, meta) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		def.Name, def.Hash, nilIfEmpty(def.Range), def.HashType, nilIfEmpty(def.RangeType), nilIfEmpty(def.TTL), string(def.Meta),
	)
	if err != nil {
		return 0, fmt.Errorf("storage: insert table def: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("storage: insert table def id: %w", err)
	}
	return id, nil
}

func (s *Store) GetTableDef(tx *sql.Tx, name string) (TableDef, error) {
	var d TableDef
	var range_, rangeType, ttl sql.NullString
	var meta []byte
	err := tx.QueryRow(
		`SELECT id, name, hash, range, hash_type, range_type, ttl, meta FROM ddb_table_defs WHERE name = ?`,
		name,
	).Scan(&d.ID, &d.Name, &d.Hash, &range_, &d.HashType, &rangeType, &ttl, &meta)
	if errors.Is(err, sql.ErrNoRows) {
		return TableDef{}, ErrNotFound
	}
	if err != nil {
		return TableDef{}, fmt.Errorf("storage: get table def: %w", err)
	}
	d.Range = range_.String
	d.RangeType = rangeType.String
	d.TTL = ttl.String
	d.Meta = json.RawMessage(meta)
	return d, nil
}

func (s *Store) ListTableDefs(tx *sql.Tx) ([]TableDef, error) {
	rows, err := tx.Query(`SELECT id, name, hash, range, hash_type, range_type, ttl, meta FROM ddb_table_defs ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("storage: list table defs: %w", err)
	}
	defer rows.Close()
	return scanDefs(rows)
}

// ListTableDefsPage returns up to limit rows with name > start, ordered by name.
func (s *Store) ListTableDefsPage(tx *sql.Tx, start string, limit int) ([]TableDef, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := tx.Query(
		`SELECT id, name, hash, range, hash_type, range_type, ttl, meta FROM ddb_table_defs WHERE name > ? ORDER BY name LIMIT ?`,
		start, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("storage: list table defs page: %w", err)
	}
	defer rows.Close()
	return scanDefs(rows)
}

func (s *Store) DeleteTableDef(tx *sql.Tx, id int64) error {
	if _, err := tx.Exec(`DELETE FROM ddb_table_defs WHERE id = ?`, id); err != nil {
		return fmt.Errorf("storage: delete table def: %w", err)
	}
	return nil
}

func (s *Store) TableExists(tx *sql.Tx, name string) (bool, error) {
	var one int
	err := tx.QueryRow(`SELECT 1 FROM ddb_table_defs WHERE name = ?`, name).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("storage: table exists: %w", err)
	}
	return true, nil
}

func scanDefs(rows *sql.Rows) ([]TableDef, error) {
	var out []TableDef
	for rows.Next() {
		var d TableDef
		var range_, rangeType, ttl sql.NullString
		var meta []byte
		if err := rows.Scan(&d.ID, &d.Name, &d.Hash, &range_, &d.HashType, &rangeType, &ttl, &meta); err != nil {
			return nil, fmt.Errorf("storage: scan table def: %w", err)
		}
		d.Range = range_.String
		d.RangeType = rangeType.String
		d.TTL = ttl.String
		d.Meta = json.RawMessage(meta)
		out = append(out, d)
	}
	return out, rows.Err()
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
