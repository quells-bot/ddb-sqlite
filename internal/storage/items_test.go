package storage

import (
	"context"
	"reflect"
	"testing"
)

func dataDef() TableDef { return TableDef{Name: "T", Hash: "pk", HashType: "S"} }

func TestPutGetDeleteItem(t *testing.T) {
	s := newTestStore(t)
	tx, _ := s.BeginTx(context.Background())
	defer tx.Rollback()

	s.CreateDataTable(tx, dataDef())

	blob := []byte("hello")
	if err := s.PutItem(tx, "T", "k1", nil, blob); err != nil {
		t.Fatalf("PutItem: %v", err)
	}
	_, got, found, err := s.GetItem(tx, "T", "k1", nil)
	if err != nil || !found {
		t.Fatalf("GetItem: got=%q found=%v err=%v", got, found, err)
	}
	if !reflect.DeepEqual(got, blob) {
		t.Errorf("GetItem = %q, want %q", got, blob)
	}

	// Overwrite.
	s.PutItem(tx, "T", "k1", nil, []byte("world"))
	_, got, _, _ = s.GetItem(tx, "T", "k1", nil)
	if string(got) != "world" {
		t.Errorf("overwrite = %q, want world", got)
	}

	// Missing key.
	_, _, found, _ = s.GetItem(tx, "T", "missing", nil)
	if found {
		t.Error("missing key returned found=true")
	}

	// Delete.
	found, err = s.DeleteItem(tx, "T", "k1", nil)
	if err != nil || !found {
		t.Fatalf("DeleteItem: found=%v err=%v", found, err)
	}
	_, _, found, _ = s.GetItem(tx, "T", "k1", nil)
	if found {
		t.Error("after delete, key still found")
	}

	// Idempotent delete of missing key.
	found, err = s.DeleteItem(tx, "T", "k1", nil)
	if err != nil || found {
		t.Errorf("delete missing: found=%v err=%v, want false/nil", found, err)
	}
}

func TestPutGetItemWithRange(t *testing.T) {
	s := newTestStore(t)
	tx, _ := s.BeginTx(context.Background())
	defer tx.Rollback()

	s.CreateDataTable(tx, TableDef{Name: "R", Hash: "pk", HashType: "S", Range: "sk", RangeType: "S"})

	s.PutItem(tx, "R", "p1", "s1", []byte("a"))
	s.PutItem(tx, "R", "p1", "s2", []byte("b"))

	_, got, found, _ := s.GetItem(tx, "R", "p1", "s2")
	if !found || string(got) != "b" {
		t.Errorf("GetItem(p1,s2) = %q found=%v", got, found)
	}
	_, _, found, _ = s.GetItem(tx, "R", "p1", "missing")
	if found {
		t.Error("missing range returned found=true")
	}
}
