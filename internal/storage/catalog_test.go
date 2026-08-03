package storage

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func sampleDef(name string) TableDef {
	meta, _ := json.Marshal(map[string]string{"class": "STANDARD"})
	return TableDef{Name: name, Hash: "pk", HashType: "S", Meta: meta}
}

func TestInsertAndGetTableDef(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	tx, _ := s.BeginTx(ctx)
	defer tx.Rollback()

	def := sampleDef("Users")
	id, err := s.InsertTableDef(tx, def)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if id <= 0 {
		t.Errorf("id = %d, want > 0", id)
	}

	got, err := s.GetTableDef(tx, "Users")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "Users" || got.Hash != "pk" || got.HashType != "S" {
		t.Errorf("got = %+v", got)
	}
}

func TestGetTableDefMissing(t *testing.T) {
	s := newTestStore(t)
	tx, _ := s.BeginTx(context.Background())
	defer tx.Rollback()

	_, err := s.GetTableDef(tx, "nope")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("got err = %v, want ErrNotFound", err)
	}
}

func TestTableExists(t *testing.T) {
	s := newTestStore(t)
	tx, _ := s.BeginTx(context.Background())
	defer tx.Rollback()

	s.InsertTableDef(tx, sampleDef("Users"))
	if ok, _ := s.TableExists(tx, "Users"); !ok {
		t.Error("Users should exist")
	}
	if ok, _ := s.TableExists(tx, "Orders"); ok {
		t.Error("Orders should not exist")
	}
}

func TestListTableDefsPage(t *testing.T) {
	s := newTestStore(t)
	tx, _ := s.BeginTx(context.Background())
	defer tx.Rollback()

	for _, n := range []string{"a", "b", "c", "d"} {
		s.InsertTableDef(tx, sampleDef(n))
	}

	page, err := s.ListTableDefsPage(tx, "", 2)
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if len(page) != 2 || page[0].Name != "a" || page[1].Name != "b" {
		t.Errorf("page 1 = %+v", page)
	}

	page2, _ := s.ListTableDefsPage(tx, "b", 2)
	if len(page2) != 2 || page2[0].Name != "c" || page2[1].Name != "d" {
		t.Errorf("page 2 = %+v", page2)
	}

	// Past the end returns nothing.
	page3, _ := s.ListTableDefsPage(tx, "d", 2)
	if len(page3) != 0 {
		t.Errorf("page 3 = %+v, want empty", page3)
	}
}

func TestDeleteTableDef(t *testing.T) {
	s := newTestStore(t)
	tx, _ := s.BeginTx(context.Background())
	defer tx.Rollback()

	id, _ := s.InsertTableDef(tx, sampleDef("Users"))
	if err := s.DeleteTableDef(tx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.GetTableDef(tx, "Users"); !errors.Is(err, ErrNotFound) {
		t.Errorf("after delete, err = %v, want ErrNotFound", err)
	}
}

func TestUpdateTableTTL(t *testing.T) {
	s := newTestStore(t)
	tx, _ := s.BeginTx(context.Background())
	defer tx.Rollback()

	id, _ := s.InsertTableDef(tx, sampleDef("T"))

	// Enable sets the catalog attr name.
	if err := s.UpdateTableTTL(tx, id, "expire"); err != nil {
		t.Fatalf("enable: %v", err)
	}
	def, _ := s.GetTableDef(tx, "T")
	if def.TTL != "expire" {
		t.Errorf("after enable: TTL = %q, want %q", def.TTL, "expire")
	}

	// Re-enable with a different name overwrites.
	if err := s.UpdateTableTTL(tx, id, "ttl"); err != nil {
		t.Fatalf("re-specify: %v", err)
	}
	def, _ = s.GetTableDef(tx, "T")
	if def.TTL != "ttl" {
		t.Errorf("after re-specify: TTL = %q, want %q", def.TTL, "ttl")
	}

	// Disable sets the catalog column to NULL.
	if err := s.UpdateTableTTL(tx, id, ""); err != nil {
		t.Fatalf("disable: %v", err)
	}
	def, _ = s.GetTableDef(tx, "T")
	if def.TTL != "" {
		t.Errorf("after disable: TTL = %q, want empty", def.TTL)
	}
}

func TestDeleteGsiDef(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	tx, _ := s.BeginTx(ctx)
	defer tx.Rollback()

	id, _ := s.InsertTableDef(tx, TableDef{
		Name: "T", Hash: "pk", HashType: "S",
		Meta: json.RawMessage(`{}`),
	})
	s.InsertGsiDef(tx, id, GsiDef{Name: "g1", Hash: "a", HashType: "S", ProjectionType: "ALL"})
	s.InsertGsiDef(tx, id, GsiDef{Name: "g2", Hash: "b", HashType: "S", ProjectionType: "ALL"})

	if err := s.DeleteGsiDef(tx, id, "g1"); err != nil {
		t.Fatalf("DeleteGsiDef: %v", err)
	}
	got, err := s.GetGsiDefs(tx, id)
	if err != nil {
		t.Fatalf("GetGsiDefs: %v", err)
	}
	if len(got) != 1 || got[0].Name != "g2" {
		t.Errorf("after delete got %v, want [g2]", got)
	}
	// Deleting a missing name is a no-op (existence is checked at the ddb layer).
	if err := s.DeleteGsiDef(tx, id, "nope"); err != nil {
		t.Errorf("delete missing: %v, want nil", err)
	}
}
