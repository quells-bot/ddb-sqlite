package storage

import (
	"database/sql"
	"fmt"
	"strings"
)

// SortKeyCond is the storage-level sort-key predicate. ddb translates
// attrval.Value operands to Go values (string/float64/[]byte) before passing
// them here, matching the key column affinities. BEGINS_WITH carries Lo=prefix
// and Hi=successor (computed by ddb); storage emits a half-open range.
// Op == "" is legal: ddb builds such a condition when resuming a
// partition-equality-only Query from an ExclusiveStartKey, so that only the
// ResumeAfter clause is emitted.
type SortKeyCond struct {
	Op          string // "", "=", "<", "<=", ">", ">=", "BETWEEN", "BEGINS_WITH"
	Lo          any    // set for every non-"" Op
	Hi          any    // BETWEEN: always set. BEGINS_WITH: nil means no successor
	ResumeAfter any    // appended as "AND range > ?" (ASC) / "range < ?" (DESC); nil = no resume
}

// Query selects rows for one partition key value, ordered by the sort key,
// and returns their item blobs. sortCond is nil for a partition-only seek.
// scanForward controls ASC (true) vs DESC (false) ordering. limit <= 0 means
// unlimited; otherwise at most limit rows are returned.
func (s *Store) Query(tx *sql.Tx, table string, hashVal any, sortCond *SortKeyCond, scanForward bool, limit int) ([][]byte, error) {
	tbl := TableName(table)

	// Partition-only table: no sort column, no ORDER BY, at most one row.
	if sortCond == nil {
		// Probe whether the table has a range column by checking TableDef — but
		// storage doesn't have the def here. The caller (ddb) passes sortCond=nil
		// only for partition-only tables, and passes a non-nil sortCond (possibly
		// Op=="") for sort-key tables. So nil sortCond → partition-only query.
		var data []byte
		err := tx.QueryRow(`SELECT data FROM `+tbl+` WHERE hash = ?`, hashVal).Scan(&data)
		if err == sql.ErrNoRows {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("storage: query %q: %w", table, err)
		}
		return [][]byte{data}, nil
	}

	// Sort-key table: build the WHERE clause.
	var b strings.Builder
	var args []any
	b.WriteString(`SELECT data FROM `)
	b.WriteString(tbl)
	b.WriteString(` WHERE hash = ?`)
	args = append(args, hashVal)

	switch sortCond.Op {
	case "":
		// No sort condition — only ResumeAfter applies.
	case "=", "<", "<=", ">", ">=":
		b.WriteString(` AND range `)
		b.WriteString(sortCond.Op)
		b.WriteString(` ?`)
		args = append(args, sortCond.Lo)
	case "BETWEEN":
		b.WriteString(` AND range >= ? AND range <= ?`)
		args = append(args, sortCond.Lo, sortCond.Hi)
	case "BEGINS_WITH":
		b.WriteString(` AND range >= ?`)
		args = append(args, sortCond.Lo)
		if sortCond.Hi != nil {
			b.WriteString(` AND range < ?`)
			args = append(args, sortCond.Hi)
		}
	default:
		return nil, fmt.Errorf("storage: query %q: unknown sort op %q", table, sortCond.Op)
	}

	if sortCond.ResumeAfter != nil {
		if scanForward {
			b.WriteString(` AND range > ?`)
		} else {
			b.WriteString(` AND range < ?`)
		}
		args = append(args, sortCond.ResumeAfter)
	}

	b.WriteString(` ORDER BY range`)
	if scanForward {
		b.WriteString(` ASC`)
	} else {
		b.WriteString(` DESC`)
	}

	if limit > 0 {
		b.WriteString(` LIMIT ?`)
		args = append(args, limit)
	}

	rows, err := tx.Query(b.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("storage: query %q: %w", table, err)
	}
	defer rows.Close()

	var blobs [][]byte
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return nil, fmt.Errorf("storage: query %q scan: %w", table, err)
		}
		blobs = append(blobs, data)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: query %q rows: %w", table, err)
	}
	return blobs, nil
}

// Scan selects rows in rowid order and returns their item blobs. segment and
// totalSegments implement parallel scan: when totalSegments > 1, only rows
// with (id % totalSegments) == segment are returned. afterID > 0 resumes the
// scan after that rowid. limit <= 0 means unlimited.
func (s *Store) Scan(tx *sql.Tx, table string, segment, totalSegments int, afterID int64, limit int) ([][]byte, error) {
	tbl := TableName(table)

	var b strings.Builder
	var args []any
	b.WriteString(`SELECT data FROM `)
	b.WriteString(tbl)

	hasWhere := false
	if totalSegments > 1 {
		b.WriteString(` WHERE (id % ?) = ?`)
		args = append(args, totalSegments, segment)
		hasWhere = true
	}
	if afterID > 0 {
		if hasWhere {
			b.WriteString(` AND id > ?`)
		} else {
			b.WriteString(` WHERE id > ?`)
			hasWhere = true
		}
		args = append(args, afterID)
	}
	b.WriteString(` ORDER BY id`)
	if limit > 0 {
		b.WriteString(` LIMIT ?`)
		args = append(args, limit)
	}

	rows, err := tx.Query(b.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("storage: scan %q: %w", table, err)
	}
	defer rows.Close()

	var blobs [][]byte
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return nil, fmt.Errorf("storage: scan %q scan: %w", table, err)
		}
		blobs = append(blobs, data)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: scan %q rows: %w", table, err)
	}
	return blobs, nil
}
