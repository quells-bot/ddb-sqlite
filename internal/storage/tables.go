package storage

import (
	"database/sql"
	"fmt"
	"strings"
)

// sqliteType maps a DynamoDB key type (S/N/B) to the SQLite column type used for
// key columns. N maps to REAL so the index gives numeric ordering.
// CAVEAT: N-typed keys rely on float64 ordering in the SQLite index — correct
// for normal-precision keys, theoretically divergent beyond float64. Acceptable
// for a test mock; the exact value is preserved in the JSON blob.
func sqliteType(t string) string {
	switch t {
	case "S":
		return "TEXT"
	case "N":
		return "REAL"
	case "B":
		return "BLOB"
	default:
		return "" // invalid; CreateDataTable rejects before reaching here
	}
}

// CreateDataTable generates and executes the per-table DDL. The data column is
// always BLOB (opaque wire-JSON bytes); ttl is NULL for now (populated M5).
func (s *Store) CreateDataTable(tx *sql.Tx, def TableDef) error {
	ht := sqliteType(def.HashType)
	if ht == "" {
		return fmt.Errorf("storage: invalid hash key type %q", def.HashType)
	}
	var b strings.Builder
	fmt.Fprintf(&b, `CREATE TABLE %s (id INTEGER NOT NULL PRIMARY KEY, hash %s NOT NULL`, TableName(def.Name), ht)
	if def.Range != "" {
		rt := sqliteType(def.RangeType)
		if rt == "" {
			return fmt.Errorf("storage: invalid range key type %q", def.RangeType)
		}
		fmt.Fprintf(&b, `, range %s NOT NULL`, rt)
	}
	b.WriteString(`, data BLOB NOT NULL, ttl INTEGER`)
	if def.Range != "" {
		b.WriteString(`, UNIQUE (hash, range)`)
	} else {
		b.WriteString(`, UNIQUE (hash)`)
	}
	b.WriteString(`) STRICT`)
	if _, err := tx.Exec(b.String()); err != nil {
		return fmt.Errorf("storage: create data table %q: %w", def.Name, err)
	}
	return nil
}

// DropDataTable drops the per-table data table for the given DynamoDB name.
func (s *Store) DropDataTable(tx *sql.Tx, name string) error {
	if _, err := tx.Exec(`DROP TABLE ` + TableName(name)); err != nil {
		return fmt.Errorf("storage: drop data table %q: %w", name, err)
	}
	return nil
}

// PutItem inserts or replaces the item blob for the given key. rangeVal is nil
// iff the table has no sort key (guaranteed by ddb validation).
func (s *Store) PutItem(tx *sql.Tx, table string, hashVal, rangeVal any, data []byte) error {
	tbl := TableName(table)
	var err error
	if rangeVal == nil {
		_, err = tx.Exec(`INSERT OR REPLACE INTO `+tbl+` (hash, data) VALUES (?, ?)`, hashVal, data)
	} else {
		_, err = tx.Exec(`INSERT OR REPLACE INTO `+tbl+` (hash, range, data) VALUES (?, ?, ?)`, hashVal, rangeVal, data)
	}
	if err != nil {
		return fmt.Errorf("storage: put item %q: %w", table, err)
	}
	return nil
}

// GetItem returns the item blob for the key. found is false (no error) if absent.
func (s *Store) GetItem(tx *sql.Tx, table string, hashVal, rangeVal any) (data []byte, found bool, err error) {
	tbl := TableName(table)
	var row *sql.Row
	if rangeVal == nil {
		row = tx.QueryRow(`SELECT data FROM `+tbl+` WHERE hash = ?`, hashVal)
	} else {
		row = tx.QueryRow(`SELECT data FROM `+tbl+` WHERE hash = ? AND range = ?`, hashVal, rangeVal)
	}
	if err := row.Scan(&data); err == sql.ErrNoRows {
		return nil, false, nil
	} else if err != nil {
		return nil, false, fmt.Errorf("storage: get item %q: %w", table, err)
	}
	return data, true, nil
}

// DeleteItem deletes the item for the key. found is false (no error) if the key
// did not exist (DynamoDB idempotency).
func (s *Store) DeleteItem(tx *sql.Tx, table string, hashVal, rangeVal any) (found bool, err error) {
	tbl := TableName(table)
	var res sql.Result
	if rangeVal == nil {
		res, err = tx.Exec(`DELETE FROM `+tbl+` WHERE hash = ?`, hashVal)
	} else {
		res, err = tx.Exec(`DELETE FROM `+tbl+` WHERE hash = ? AND range = ?`, hashVal, rangeVal)
	}
	if err != nil {
		return false, fmt.Errorf("storage: delete item %q: %w", table, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}
