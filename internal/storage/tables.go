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
// always BLOB (opaque wire-JSON bytes). TTL is configured at the catalog level
// (ddb_table_defs.ttl stores the attribute name) and reaped by ExpireExpired,
// which parses the attribute out of the blob — no per-item ttl column is needed.
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
	b.WriteString(`, data BLOB NOT NULL`)
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
// iff the table has no sort key (guaranteed by ddb validation). It returns the
// rowid of the inserted/replaced row (LastInsertId), which callers use as the
// item's data id when maintaining GSI index rows.
func (s *Store) PutItem(tx *sql.Tx, table string, hashVal, rangeVal any, data []byte) (int64, error) {
	tbl := TableName(table)
	var res sql.Result
	var err error
	if rangeVal == nil {
		res, err = tx.Exec(`INSERT OR REPLACE INTO `+tbl+` (hash, data) VALUES (?, ?)`, hashVal, data)
	} else {
		res, err = tx.Exec(`INSERT OR REPLACE INTO `+tbl+` (hash, range, data) VALUES (?, ?, ?)`, hashVal, rangeVal, data)
	}
	if err != nil {
		return 0, fmt.Errorf("storage: put item %q: %w", table, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("storage: put item %q id: %w", table, err)
	}
	return id, nil
}

// CreateGsiTable generates and executes the DDL for a GSI index table. It
// stores one row per base item that participates in the index, keyed by the
// base data table's id (data_id is the PRIMARY KEY / rowid). data_id references
// the base table with ON DELETE CASCADE, so deleting a base item removes its
// index rows. The hash (and optional range) columns hold the projected index
// key values; a non-unique index over them supports equality/range lookups.
func (s *Store) CreateGsiTable(tx *sql.Tx, tableDef TableDef, gsi GsiDef) error {
	gtbl := GsiTableName(tableDef.Name, gsi.Name)
	ht := sqliteType(gsi.HashType)
	if ht == "" {
		return fmt.Errorf("storage: invalid gsi hash key type %q", gsi.HashType)
	}
	var b strings.Builder
	fmt.Fprintf(&b, `CREATE TABLE %s (data_id INTEGER NOT NULL PRIMARY KEY REFERENCES %s (id) ON DELETE CASCADE, hash %s NOT NULL`, gtbl, TableName(tableDef.Name), ht)
	if gsi.Range != "" {
		rt := sqliteType(gsi.RangeType)
		if rt == "" {
			return fmt.Errorf("storage: invalid gsi range key type %q", gsi.RangeType)
		}
		fmt.Fprintf(&b, `, range %s NOT NULL`, rt)
	}
	b.WriteString(`) STRICT`)
	if _, err := tx.Exec(b.String()); err != nil {
		return fmt.Errorf("storage: create gsi table %q/%q: %w", tableDef.Name, gsi.Name, err)
	}
	// Non-unique index over the index key for lookups.
	idx := gtbl + `_idx`
	var isql string
	if gsi.Range != "" {
		isql = `CREATE INDEX ` + idx + ` ON ` + gtbl + ` (hash, range)`
	} else {
		isql = `CREATE INDEX ` + idx + ` ON ` + gtbl + ` (hash)`
	}
	if _, err := tx.Exec(isql); err != nil {
		return fmt.Errorf("storage: create gsi index %q/%q: %w", tableDef.Name, gsi.Name, err)
	}
	return nil
}

// UpsertGsiRow inserts or replaces the index entry for one base item. dataID is
// the base data table's rowid; the hash (and optional range) values are the
// projected index key. Because data_id is the PRIMARY KEY, re-upserting the
// same item updates its index key values in place (rangeVal is nil for a GSI
// with no sort key).
func (s *Store) UpsertGsiRow(tx *sql.Tx, table, gsi string, dataID int64, hashVal, rangeVal any) error {
	tbl := GsiTableName(table, gsi)
	var err error
	if rangeVal == nil {
		_, err = tx.Exec(`INSERT OR REPLACE INTO `+tbl+` (data_id, hash) VALUES (?, ?)`, dataID, hashVal)
	} else {
		_, err = tx.Exec(`INSERT OR REPLACE INTO `+tbl+` (data_id, hash, range) VALUES (?, ?, ?)`, dataID, hashVal, rangeVal)
	}
	if err != nil {
		return fmt.Errorf("storage: upsert gsi row %q/%q: %w", table, gsi, err)
	}
	return nil
}

// DropGsiTable drops the GSI index table for the given table and GSI name.
func (s *Store) DropGsiTable(tx *sql.Tx, table, gsi string) error {
	if _, err := tx.Exec(`DROP TABLE IF EXISTS ` + GsiTableName(table, gsi)); err != nil {
		return fmt.Errorf("storage: drop gsi table %q/%q: %w", table, gsi, err)
	}
	return nil
}

// GetItem returns the item blob for the key, along with its rowid. found is
// false (no error) if absent.
func (s *Store) GetItem(tx *sql.Tx, table string, hashVal, rangeVal any) (id int64, data []byte, found bool, err error) {
	tbl := TableName(table)
	var row *sql.Row
	if rangeVal == nil {
		row = tx.QueryRow(`SELECT id, data FROM `+tbl+` WHERE hash = ?`, hashVal)
	} else {
		row = tx.QueryRow(`SELECT id, data FROM `+tbl+` WHERE hash = ? AND range = ?`, hashVal, rangeVal)
	}
	if err := row.Scan(&id, &data); err == sql.ErrNoRows {
		return 0, nil, false, nil
	} else if err != nil {
		return 0, nil, false, fmt.Errorf("storage: get item %q: %w", table, err)
	}
	return id, data, true, nil
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

// ExpireExpired scans all rows in the table's data table, calls expired(data)
// for each blob, and deletes the rows for which expired returns true. Returns
// the count of deleted rows. GSI index rows are cleaned by the ON DELETE
// CASCADE foreign key on GSI tables. The expired callback is provided by ddb
// and handles blob unmarshalling + TTL attribute extraction; storage stays
// blob-agnostic.
func (s *Store) ExpireExpired(tx *sql.Tx, table string, expired func([]byte) (bool, error)) (int64, error) {
	tbl := TableName(table)
	rows, err := tx.Query(`SELECT id, data FROM ` + tbl)
	if err != nil {
		return 0, fmt.Errorf("storage: expire scan %q: %w", table, err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		var data []byte
		if err := rows.Scan(&id, &data); err != nil {
			rows.Close()
			return 0, fmt.Errorf("storage: expire scan %q: %w", table, err)
		}
		ok, err := expired(data)
		if err != nil {
			rows.Close()
			return 0, fmt.Errorf("storage: expire %q: %w", table, err)
		}
		if ok {
			ids = append(ids, id)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("storage: expire scan %q: %w", table, err)
	}
	rows.Close() // cannot issue DELETE while iterating on the same tx

	var n int64
	for _, id := range ids {
		if _, err := tx.Exec(`DELETE FROM `+tbl+` WHERE id = ?`, id); err != nil {
			return n, fmt.Errorf("storage: expire delete %q: %w", table, err)
		}
		n++
	}
	return n, nil
}
