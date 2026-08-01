package storage

import (
	"context"
	"database/sql"
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
