package storage

import (
	"database/sql"
	"fmt"
	"strings"
)

// GsiResume carries the (range, data_id) keyset cursor for GSI resume. For a
// partition-only GSI (no sort key), Range is nil and only DataID is used.
type GsiResume struct {
	Range  any   // the LEK's GSI sort-key value; nil for partition-only GSIs
	DataID int64 // the LEK item's data_id (rowid)
}

// QueryGSI selects rows for one GSI partition key value, ordered by the GSI
// sort key then data_id (stable tiebreak), and returns their item blobs via a
// JOIN to the data table. sortCond is nil for a partition-only GSI with no
// resume; a non-nil sortCond with Op=="" means partition-equality-only with
// optional resume. scanForward controls ASC vs DESC. limit <= 0 = unlimited.
func (s *Store) QueryGSI(tx *sql.Tx, table, gsi string, hashVal any, sortCond *SortKeyCond, resume *GsiResume, scanForward bool, limit int) ([][]byte, error) {
	gtbl := GsiTableName(table, gsi)
	dtbl := TableName(table)

	var b strings.Builder
	var args []any
	b.WriteString(`SELECT d.data FROM `)
	b.WriteString(gtbl)
	b.WriteString(` g JOIN `)
	b.WriteString(dtbl)
	b.WriteString(` d ON g.data_id = d.id WHERE g.hash = ?`)
	args = append(args, hashVal)

	hasSort := sortCond != nil && sortCond.Op != ""
	if hasSort {
		switch sortCond.Op {
		case "=":
			b.WriteString(` AND g.range = ?`)
			args = append(args, sortCond.Lo)
		case "<":
			b.WriteString(` AND g.range < ?`)
			args = append(args, sortCond.Lo)
		case "<=":
			b.WriteString(` AND g.range <= ?`)
			args = append(args, sortCond.Lo)
		case ">":
			b.WriteString(` AND g.range > ?`)
			args = append(args, sortCond.Lo)
		case ">=":
			b.WriteString(` AND g.range >= ?`)
			args = append(args, sortCond.Lo)
		case "BETWEEN":
			b.WriteString(` AND g.range >= ? AND g.range <= ?`)
			args = append(args, sortCond.Lo, sortCond.Hi)
		case "BEGINS_WITH":
			b.WriteString(` AND g.range >= ?`)
			args = append(args, sortCond.Lo)
			if sortCond.Hi != nil {
				b.WriteString(` AND g.range < ?`)
				args = append(args, sortCond.Hi)
			}
		}
	}

	// Resume cursor is direction-aware: for sort-key GSIs it is
	// (range X ? OR (range = ? AND data_id X ?)) where X is > for ASC and < for
	// DESC; partition-only GSIs resume by data_id X ? alone. Resume is appended
	// after the sort bounds.
	if resume != nil {
		op := `>`
		if !scanForward {
			op = `<`
		}
		if resume.Range != nil {
			b.WriteString(` AND (g.range ` + op + ` ? OR (g.range = ? AND g.data_id ` + op + ` ?))`)
			args = append(args, resume.Range, resume.Range, resume.DataID)
		} else {
			b.WriteString(` AND g.data_id ` + op + ` ?`)
			args = append(args, resume.DataID)
		}
	}

	// A composite GSI (sortCond != nil, including a partition-equality-only
	// query whose Op is "") must order by the GSI range key then data_id as the
	// stable tiebreak. Only a partition-only GSI (sortCond == nil, no range)
	// orders by data_id alone. (Previously the empty-op partition query on a
	// composite GSI fell through to data_id-only ordering, which hid range order
	// behind insertion order and broke resume-based pagination.)
	if sortCond != nil || resume != nil && resume.Range != nil {
		b.WriteString(` ORDER BY g.range`)
		if scanForward {
			b.WriteString(` ASC, g.data_id ASC`)
		} else {
			b.WriteString(` DESC, g.data_id DESC`)
		}
	} else {
		// Partition-only GSI: order by data_id only.
		b.WriteString(` ORDER BY g.data_id`)
		if scanForward {
			b.WriteString(` ASC`)
		} else {
			b.WriteString(` DESC`)
		}
	}

	if limit > 0 {
		b.WriteString(` LIMIT ?`)
		args = append(args, limit)
	}

	rows, err := tx.Query(b.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("storage: query gsi %q.%q: %w", table, gsi, err)
	}
	defer rows.Close()

	var blobs [][]byte
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return nil, fmt.Errorf("storage: query gsi %q.%q scan: %w", table, gsi, err)
		}
		blobs = append(blobs, data)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: query gsi %q.%q rows: %w", table, gsi, err)
	}
	return blobs, nil
}

// ScanGSI selects all rows in the GSI in data_id order, JOINed to the data
// table. afterID > 0 resumes after that data_id. limit <= 0 = unlimited.
func (s *Store) ScanGSI(tx *sql.Tx, table, gsi string, afterID int64, limit int) ([][]byte, error) {
	gtbl := GsiTableName(table, gsi)
	dtbl := TableName(table)

	var b strings.Builder
	var args []any
	b.WriteString(`SELECT d.data FROM `)
	b.WriteString(gtbl)
	b.WriteString(` g JOIN `)
	b.WriteString(dtbl)
	b.WriteString(` d ON g.data_id = d.id`)
	if afterID > 0 {
		b.WriteString(` WHERE g.data_id > ?`)
		args = append(args, afterID)
	}
	b.WriteString(` ORDER BY g.data_id`)
	if limit > 0 {
		b.WriteString(` LIMIT ?`)
		args = append(args, limit)
	}

	rows, err := tx.Query(b.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("storage: scan gsi %q.%q: %w", table, gsi, err)
	}
	defer rows.Close()

	var blobs [][]byte
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return nil, fmt.Errorf("storage: scan gsi %q.%q scan: %w", table, gsi, err)
		}
		blobs = append(blobs, data)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: scan gsi %q.%q rows: %w", table, gsi, err)
	}
	return blobs, nil
}
