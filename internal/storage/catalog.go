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
	GSIs      []GsiDef
}

// GsiDef mirrors a ddb_gsi_defs catalog row. Projected is the decoded JSON
// attr list (INCLUDE); nil otherwise.
type GsiDef struct {
	Name           string
	Hash           string
	Range          string
	HashType       string
	RangeType      string
	ProjectionType string   // "ALL" | "KEYS_ONLY" | "INCLUDE"
	Projected      []string // INCLUDE only; nil otherwise
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
	gsis, err := s.GetGsiDefs(tx, d.ID)
	if err != nil {
		return TableDef{}, err
	}
	d.GSIs = gsis
	return d, nil
}

// InsertGsiDef stores one GSI definition. Projected is serialized as a JSON
// array into the catalog's projected TEXT column.
func (s *Store) InsertGsiDef(tx *sql.Tx, tableID int64, def GsiDef) error {
	var projected any
	if def.ProjectionType == "INCLUDE" {
		b, err := json.Marshal(def.Projected)
		if err != nil {
			return fmt.Errorf("storage: marshal gsi projected: %w", err)
		}
		projected = string(b)
	}
	_, err := tx.Exec(
		`INSERT INTO ddb_gsi_defs (table_id, name, hash, range, hash_type, range_type, projection_type, projected) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		tableID, def.Name, def.Hash, nilIfEmpty(def.Range), def.HashType, nilIfEmpty(def.RangeType), def.ProjectionType, projected,
	)
	if err != nil {
		return fmt.Errorf("storage: insert gsi def: %w", err)
	}
	return nil
}

// GetGsiDefs returns all GSI definitions for a table.
func (s *Store) GetGsiDefs(tx *sql.Tx, tableID int64) ([]GsiDef, error) {
	rows, err := tx.Query(
		`SELECT name, hash, range, hash_type, range_type, projection_type, projected FROM ddb_gsi_defs WHERE table_id = ?`,
		tableID,
	)
	if err != nil {
		return nil, fmt.Errorf("storage: get gsi defs: %w", err)
	}
	defer rows.Close()
	return scanGsiDefs(rows)
}

// GetGsiDef returns one GSI definition by name, or ErrNotFound.
func (s *Store) GetGsiDef(tx *sql.Tx, tableID int64, name string) (GsiDef, error) {
	rows, err := tx.Query(
		`SELECT name, hash, range, hash_type, range_type, projection_type, projected FROM ddb_gsi_defs WHERE table_id = ? AND name = ?`,
		tableID, name,
	)
	if err != nil {
		return GsiDef{}, fmt.Errorf("storage: get gsi def: %w", err)
	}
	defer rows.Close()
	defs, err := scanGsiDefs(rows)
	if err != nil {
		return GsiDef{}, err
	}
	if len(defs) == 0 {
		return GsiDef{}, ErrNotFound
	}
	return defs[0], nil
}

// DeleteGsiDefs removes all GSI definitions for a table.
func (s *Store) DeleteGsiDefs(tx *sql.Tx, tableID int64) error {
	if _, err := tx.Exec(`DELETE FROM ddb_gsi_defs WHERE table_id = ?`, tableID); err != nil {
		return fmt.Errorf("storage: delete gsi defs: %w", err)
	}
	return nil
}

func scanGsiDefs(rows *sql.Rows) ([]GsiDef, error) {
	var out []GsiDef
	for rows.Next() {
		var d GsiDef
		var range_, rangeType, projected sql.NullString
		if err := rows.Scan(&d.Name, &d.Hash, &range_, &d.HashType, &rangeType, &d.ProjectionType, &projected); err != nil {
			return nil, fmt.Errorf("storage: scan gsi def: %w", err)
		}
		d.Range = range_.String
		d.RangeType = rangeType.String
		if projected.Valid && projected.String != "" {
			if err := json.Unmarshal([]byte(projected.String), &d.Projected); err != nil {
				return nil, fmt.Errorf("storage: unmarshal gsi projected: %w", err)
			}
		}
		out = append(out, d)
	}
	return out, rows.Err()
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
