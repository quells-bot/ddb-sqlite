package storage

import (
	"context"
	"path/filepath"
	"testing"
)

func TestOpenInMemory(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	// Catalog tables must exist after open.
	var n int
	err = s.db.QueryRow("SELECT count(*) FROM ddb_table_defs").Scan(&n)
	if err != nil {
		t.Fatalf("query ddb_table_defs: %v", err)
	}
	if n != 0 {
		t.Errorf("empty catalog has %d rows, want 0", n)
	}
}

func TestBeginTx(t *testing.T) {
	ctx := context.Background()
	s, _ := Open(ctx, ":memory:")
	defer s.Close()

	tx, err := s.BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

func TestOpenReopenFileIdempotent(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dsn := filepath.Join(dir, "test.db")

	s1, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("Open 1: %v", err)
	}
	s1.Close()

	s2, err := Open(ctx, dsn) // must not error on existing catalog
	if err != nil {
		t.Fatalf("Open 2 (reopen): %v", err)
	}
	defer s2.Close()
}
