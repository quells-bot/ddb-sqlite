package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"testing"
)

func TestCreateDataTableStringKey(t *testing.T) {
	s := newTestStore(t)
	tx, _ := s.BeginTx(context.Background())
	defer tx.Rollback()

	def := TableDef{Name: "Users", Hash: "pk", HashType: "S"}
	if err := s.CreateDataTable(tx, def); err != nil {
		t.Fatalf("CreateDataTable: %v", err)
	}
	// UNIQUE(hash) must reject a duplicate partition key.
	tx.Exec(`INSERT INTO `+TableName("Users")+` (hash, data) VALUES (?, ?)`, "k1", []byte("a"))
	_, err := tx.Exec(`INSERT INTO `+TableName("Users")+` (hash, data) VALUES (?, ?)`, "k1", []byte("b"))
	if err == nil {
		t.Error("duplicate hash key was accepted; want UNIQUE violation")
	}
}

func TestCreateDataTableNumberKeyWithRange(t *testing.T) {
	s := newTestStore(t)
	tx, _ := s.BeginTx(context.Background())
	defer tx.Rollback()

	def := TableDef{Name: "Orders", Hash: "pk", HashType: "N", Range: "sk", RangeType: "S"}
	if err := s.CreateDataTable(tx, def); err != nil {
		t.Fatalf("CreateDataTable: %v", err)
	}
	// N key column has REAL affinity: a float binds and is ordered numerically.
	_, err := tx.Exec(`INSERT INTO `+TableName("Orders")+` (hash, range, data) VALUES (?, ?, ?)`, 1.5, "a", []byte("x"))
	if err != nil {
		t.Errorf("insert with REAL hash: %v", err)
	}
	// UNIQUE(hash, range) rejects only full-key duplicates.
	tx.Exec(`INSERT INTO `+TableName("Orders")+` (hash, range, data) VALUES (?, ?, ?)`, 1.5, "a", []byte("dup"))
	_, err = tx.Exec(`INSERT INTO `+TableName("Orders")+` (hash, range, data) VALUES (?, ?, ?)`, 1.5, "b", []byte("ok"))
	if err != nil {
		t.Errorf("distinct range should be allowed: %v", err)
	}
}

func TestCreateDataTableBinaryBlobRoundTrip(t *testing.T) {
	s := newTestStore(t)
	tx, _ := s.BeginTx(context.Background())
	defer tx.Rollback()

	def := TableDef{Name: "Blobs", Hash: "pk", HashType: "B"}
	if err := s.CreateDataTable(tx, def); err != nil {
		t.Fatalf("CreateDataTable: %v", err)
	}
	blob := []byte{0x00, 0xff, 0x42, 'a', 'b'}
	if _, err := tx.Exec(`INSERT INTO `+TableName("Blobs")+` (hash, data) VALUES (?, ?)`, blob, blob); err != nil {
		t.Fatalf("insert BLOB key+data: %v", err)
	}
	var got []byte
	if err := tx.QueryRow(`SELECT data FROM `+TableName("Blobs")+` WHERE hash = ?`, blob).Scan(&got); err != nil {
		t.Fatalf("select: %v", err)
	}
	if string(got) != string(blob) {
		t.Errorf("blob round-trip = %v, want %v", got, blob)
	}
}

func TestDropDataTable(t *testing.T) {
	s := newTestStore(t)
	tx, _ := s.BeginTx(context.Background())
	defer tx.Rollback()

	s.CreateDataTable(tx, TableDef{Name: "Tmp", Hash: "pk", HashType: "S"})
	if err := s.DropDataTable(tx, "Tmp"); err != nil {
		t.Fatalf("DropDataTable: %v", err)
	}
	// After drop the table is gone: querying it errors.
	if err := tx.QueryRow(`SELECT 1 FROM ` + TableName("Tmp") + ` LIMIT 1`).Scan(); err == sql.ErrNoRows {
		t.Error("expected 'no such table' error, got ErrNoRows (table still exists)")
	} else if err == nil {
		t.Error("expected error after DropDataTable, got nil")
	}
}

func TestCreateDataTableNoTTLColumn(t *testing.T) {
	s := newTestStore(t)
	tx, _ := s.BeginTx(context.Background())
	defer tx.Rollback()

	if err := s.CreateDataTable(tx, TableDef{Name: "T", Hash: "pk", HashType: "S"}); err != nil {
		t.Fatalf("CreateDataTable: %v", err)
	}
	rows, err := tx.Query(`PRAGMA table_info(` + TableName("T") + `)`)
	if err != nil {
		t.Fatalf("PRAGMA: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if name == "ttl" {
			t.Error("data table has a ttl column; M5a removed it")
		}
	}
}

func TestExpireExpired(t *testing.T) {
	s := newTestStore(t)
	tx, _ := s.BeginTx(context.Background())
	defer tx.Rollback()

	def := dataDef()
	if err := s.CreateDataTable(tx, def); err != nil {
		t.Fatalf("CreateDataTable: %v", err)
	}
	gsi := GsiDef{Name: "g1", Hash: "gsi_pk", HashType: "S", ProjectionType: "ALL"}
	if err := s.CreateGsiTable(tx, def, gsi); err != nil {
		t.Fatalf("CreateGsiTable: %v", err)
	}

	// Seed three items; the one with blob "expire" is marked expired.
	id1, _ := s.PutItem(tx, "T", "k1", nil, []byte("keep1"))
	id2, _ := s.PutItem(tx, "T", "k2", nil, []byte("expire"))
	id3, _ := s.PutItem(tx, "T", "k3", nil, []byte("keep2"))
	if err := s.UpsertGsiRow(tx, "T", "g1", id1, "G1", nil); err != nil {
		t.Fatalf("UpsertGsiRow 1: %v", err)
	}
	if err := s.UpsertGsiRow(tx, "T", "g1", id2, "G2", nil); err != nil {
		t.Fatalf("UpsertGsiRow 2: %v", err)
	}
	if err := s.UpsertGsiRow(tx, "T", "g1", id3, "G3", nil); err != nil {
		t.Fatalf("UpsertGsiRow 3: %v", err)
	}

	expired := func(data []byte) (bool, error) { return string(data) == "expire", nil }
	n, err := s.ExpireExpired(tx, "T", expired)
	if err != nil {
		t.Fatalf("ExpireExpired: %v", err)
	}
	if n != 1 {
		t.Errorf("deleted = %d, want 1", n)
	}

	// Survivors remain.
	var count int
	if err := tx.QueryRow(`SELECT count(*) FROM ` + TableName("T")).Scan(&count); err != nil {
		t.Fatalf("count survivors: %v", err)
	}
	if count != 2 {
		t.Errorf("surviving rows = %d, want 2", count)
	}

	// The expired item's GSI row is cascade-deleted.
	if err := tx.QueryRow(`SELECT count(*) FROM ` + GsiTableName("T", "g1")).Scan(&count); err != nil {
		t.Fatalf("count gsi rows: %v", err)
	}
	if count != 2 {
		t.Errorf("GSI rows = %d, want 2 (cascade failed)", count)
	}
}

func TestExpireExpiredCallbackError(t *testing.T) {
	s := newTestStore(t)
	tx, _ := s.BeginTx(context.Background())
	defer tx.Rollback()

	s.CreateDataTable(tx, dataDef())
	s.PutItem(tx, "T", "k1", nil, []byte("bad"))

	boom := func(data []byte) (bool, error) { return false, fmt.Errorf("corrupt blob") }
	if _, err := s.ExpireExpired(tx, "T", boom); err == nil {
		t.Error("ExpireExpired with failing callback: err = nil, want error")
	}
}

func TestScanAllDataOrderAndEOF(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	tx, _ := s.BeginTx(ctx)
	defer tx.Rollback()

	s.CreateDataTable(tx, TableDef{Name: "T", Hash: "pk", HashType: "S"})
	// Insert rows with hashes out of hash order; ScanAllData must yield them in
	// id order (auto-increment rowid == insertion order), NOT hash order.
	for _, v := range []string{"c", "a", "b"} {
		s.PutItem(tx, "T", v, nil, []byte("d"+v))
	}

	next, err := s.ScanAllData(tx, "T")
	if err != nil {
		t.Fatalf("ScanAllData: %v", err)
	}
	var got []string
	for {
		_, data, err := next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		got = append(got, string(data))
	}
	want := []string{"dc", "da", "db"} // id order (insertion c,a,b -> ids 1,2,3)
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestScanAllDataInterleave pins §5's different-table interleave: while
// iterating a SELECT on table A, INSERT each visited row's id into table B on
// the same tx, then assert every row was visited and the writes committed.
func TestScanAllDataInterleave(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	tx, _ := s.BeginTx(ctx)
	defer tx.Rollback()

	s.CreateDataTable(tx, TableDef{Name: "A", Hash: "pk", HashType: "S"})
	s.CreateDataTable(tx, TableDef{Name: "B", Hash: "id", HashType: "N"})
	for i, v := range []string{"a", "b", "c"} {
		s.PutItem(tx, "A", v, nil, []byte(v))
		_ = i
	}

	next, err := s.ScanAllData(tx, "A")
	if err != nil {
		t.Fatalf("ScanAllData: %v", err)
	}
	var visited []int64
	for {
		id, _, err := next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		visited = append(visited, id)
		if _, err := tx.Exec(`INSERT INTO `+TableName("B")+` (hash, data) VALUES (?, ?)`, float64(id), []byte("x")); err != nil {
			t.Fatalf("interleave insert: %v", err)
		}
	}
	if len(visited) != 3 {
		t.Fatalf("visited %d rows, want 3", len(visited))
	}
	var n int
	if err := tx.QueryRow(`SELECT count(*) FROM ` + TableName("B")).Scan(&n); err != nil {
		t.Fatalf("count B: %v", err)
	}
	if n != 3 {
		t.Errorf("table B has %d rows, want 3", n)
	}
}
