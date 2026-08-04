package storage

import (
	"context"
	"fmt"
	"testing"
)

func TestGetItemReturnsID(t *testing.T) {
	s := newTestStore(t)
	tx, _ := s.BeginTx(context.Background())
	defer tx.Rollback()

	s.CreateDataTable(tx, TableDef{Name: "T", Hash: "pk", HashType: "S", Range: "sk", RangeType: "S"})
	s.PutItem(tx, "T", "p1", "s1", []byte("hello"), 0)

	id, data, found, err := s.GetItem(tx, "T", "p1", "s1")
	if err != nil || !found {
		t.Fatalf("GetItem: id=%d found=%v err=%v", id, found, err)
	}
	if id <= 0 {
		t.Errorf("id = %d, want > 0", id)
	}
	if string(data) != "hello" {
		t.Errorf("data = %q, want hello", data)
	}
}

func TestQuerySortKeyTable(t *testing.T) {
	s := newTestStore(t)
	tx, _ := s.BeginTx(context.Background())
	defer tx.Rollback()

	def := TableDef{Name: "T", Hash: "pk", HashType: "S", Range: "sk", RangeType: "N"}
	s.CreateDataTable(tx, def)

	// Seed 5 items in one partition, sorted by sk.
	for i := range 5 {
		s.PutItem(tx, "T", "p1", float64(i*2), []byte{byte('a' + i)}, 0)
	}

	cases := []struct {
		name        string
		sortCond    *SortKeyCond
		scanForward bool
		limit       int
		wantCount   int
	}{
		{"all ASC", &SortKeyCond{Op: ""}, true, 0, 5},
		{"all DESC", &SortKeyCond{Op: ""}, false, 0, 5},
		{"limit 3", &SortKeyCond{Op: ""}, true, 3, 3},
		{"sk < 4", &SortKeyCond{Op: "<", Lo: float64(4)}, true, 0, 2},
		{"sk >= 2", &SortKeyCond{Op: ">=", Lo: float64(2)}, true, 0, 4},
		{"sk BETWEEN 2 AND 6", &SortKeyCond{Op: "BETWEEN", Lo: float64(2), Hi: float64(6)}, true, 0, 3},
		{"resume after 2", &SortKeyCond{Op: "", ResumeAfter: float64(2)}, true, 0, 3},
		{"BEGINS_WITH no successor", &SortKeyCond{Op: "BEGINS_WITH", Lo: float64(0), Hi: nil}, true, 0, 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			blobs, err := s.Query(tx, "T", "p1", tc.sortCond, tc.scanForward, tc.limit)
			if err != nil {
				t.Fatalf("Query: %v", err)
			}
			if len(blobs) != tc.wantCount {
				t.Errorf("got %d blobs, want %d", len(blobs), tc.wantCount)
			}
		})
	}
}

func TestQueryPartitionOnlyTable(t *testing.T) {
	s := newTestStore(t)
	tx, _ := s.BeginTx(context.Background())
	defer tx.Rollback()

	def := TableDef{Name: "PO", Hash: "pk", HashType: "S"}
	s.CreateDataTable(tx, def)
	s.PutItem(tx, "PO", "k1", nil, []byte("data"), 0)

	blobs, err := s.Query(tx, "PO", "k1", nil, true, 0)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(blobs) != 1 {
		t.Fatalf("got %d blobs, want 1", len(blobs))
	}
	if string(blobs[0]) != "data" {
		t.Errorf("blob = %q, want data", blobs[0])
	}
}

func TestScan(t *testing.T) {
	s := newTestStore(t)
	tx, _ := s.BeginTx(context.Background())
	defer tx.Rollback()

	def := TableDef{Name: "T", Hash: "pk", HashType: "S", Range: "sk", RangeType: "N"}
	s.CreateDataTable(tx, def)

	for i := range 10 {
		s.PutItem(tx, "T", fmt.Sprintf("p%d", i), float64(i), []byte{byte(i)}, 0)
	}

	// Full scan.
	blobs, err := s.Scan(tx, "T", 0, 0, 0, 0)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(blobs) != 10 {
		t.Errorf("full scan: got %d, want 10", len(blobs))
	}

	// Limit.
	blobs, _ = s.Scan(tx, "T", 0, 0, 0, 4)
	if len(blobs) != 4 {
		t.Errorf("limit 4: got %d, want 4", len(blobs))
	}

	// Parallel segments.
	var all [][]byte
	for seg := range 3 {
		blobs, err := s.Scan(tx, "T", seg, 3, 0, 0)
		if err != nil {
			t.Fatalf("Scan segment %d: %v", seg, err)
		}
		all = append(all, blobs...)
	}
	if len(all) != 10 {
		t.Errorf("parallel scan union: got %d, want 10", len(all))
	}

	// Resume by afterID.
	first, _, _, _ := s.GetItem(tx, "T", "p0", float64(0))
	blobs, err = s.Scan(tx, "T", 0, 0, first, 0)
	if err != nil {
		t.Fatalf("Scan resume: %v", err)
	}
	if len(blobs) != 9 {
		t.Errorf("resume scan: got %d, want 9", len(blobs))
	}
}
