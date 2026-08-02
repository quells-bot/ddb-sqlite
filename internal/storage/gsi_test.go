package storage

import (
	"context"
	"database/sql"
	"testing"
)

func TestGsiTableName(t *testing.T) {
	got := GsiTableName("Music", "gsi-all")
	want := TableName("Music") + "_" + gsiHex("gsi-all")
	if got != want {
		t.Errorf("GsiTableName = %q, want %q", got, want)
	}
}

func TestGsiCatalogCRUD(t *testing.T) {
	s := newTestStore(t)
	tx, _ := s.BeginTx(context.Background())
	defer tx.Rollback()

	def := sampleDef("T")
	id, err := s.InsertTableDef(tx, def)
	if err != nil {
		t.Fatalf("InsertTableDef: %v", err)
	}

	gd := GsiDef{
		Name:           "gsi-all",
		Hash:           "gsi_pk",
		HashType:       "S",
		Range:          "gsi_sk",
		RangeType:      "S",
		ProjectionType: "ALL",
	}
	if err := s.InsertGsiDef(tx, id, gd); err != nil {
		t.Fatalf("InsertGsiDef: %v", err)
	}

	got, err := s.GetGsiDef(tx, id, "gsi-all")
	if err != nil {
		t.Fatalf("GetGsiDef: %v", err)
	}
	if got.Hash != "gsi_pk" || got.Range != "gsi_sk" || got.ProjectionType != "ALL" {
		t.Errorf("GetGsiDef = %+v, want %+v", got, gd)
	}

	all, err := s.GetGsiDefs(tx, id)
	if err != nil {
		t.Fatalf("GetGsiDefs: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("GetGsiDefs = %d rows, want 1", len(all))
	}

	if _, err := s.GetGsiDef(tx, id, "missing"); err != ErrNotFound {
		t.Errorf("GetGsiDef(missing) err = %v, want ErrNotFound", err)
	}

	if err := s.DeleteGsiDefs(tx, id); err != nil {
		t.Fatalf("DeleteGsiDefs: %v", err)
	}
	all, _ = s.GetGsiDefs(tx, id)
	if len(all) != 0 {
		t.Errorf("after delete: %d rows, want 0", len(all))
	}
}

func TestGsiDefIncludeProjection(t *testing.T) {
	s := newTestStore(t)
	tx, _ := s.BeginTx(context.Background())
	defer tx.Rollback()

	id, _ := s.InsertTableDef(tx, sampleDef("T"))
	gd := GsiDef{
		Name:           "gsi-incl",
		Hash:           "gsi_pk",
		HashType:       "S",
		ProjectionType: "INCLUDE",
		Projected:      []string{"proj1", "proj2"},
	}
	if err := s.InsertGsiDef(tx, id, gd); err != nil {
		t.Fatalf("InsertGsiDef: %v", err)
	}
	got, _ := s.GetGsiDef(tx, id, "gsi-incl")
	if len(got.Projected) != 2 || got.Projected[0] != "proj1" || got.Projected[1] != "proj2" {
		t.Errorf("Projected = %v, want [proj1 proj2]", got.Projected)
	}
}

func TestGetTableDefLoadsGSIs(t *testing.T) {
	s := newTestStore(t)
	tx, _ := s.BeginTx(context.Background())
	defer tx.Rollback()

	id, _ := s.InsertTableDef(tx, sampleDef("T"))
	_ = s.InsertGsiDef(tx, id, GsiDef{Name: "g1", Hash: "a", HashType: "S", ProjectionType: "ALL"})
	_ = s.InsertGsiDef(tx, id, GsiDef{Name: "g2", Hash: "b", HashType: "N", ProjectionType: "KEYS_ONLY"})

	def, err := s.GetTableDef(tx, "T")
	if err != nil {
		t.Fatalf("GetTableDef: %v", err)
	}
	if len(def.GSIs) != 2 {
		t.Fatalf("def.GSIs = %d, want 2", len(def.GSIs))
	}
	// Unspecified order — check by name.
	names := map[string]bool{}
	for _, g := range def.GSIs {
		names[g.Name] = true
	}
	if !names["g1"] || !names["g2"] {
		t.Errorf("GSI names = %v, want {g1,g2}", names)
	}
}

// gsiHex returns the 16-hex suffix GsiTableName appends, for self-describing assertions.
func gsiHex(name string) string {
	return gsiHexFor(name)
}

func TestCreateGsiTableAndCascade(t *testing.T) {
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

	id, err := s.PutItem(tx, "T", "k1", nil, []byte("a"))
	if err != nil {
		t.Fatalf("PutItem: %v", err)
	}
	if err := s.UpsertGsiRow(tx, "T", "g1", id, "G1", nil); err != nil {
		t.Fatalf("UpsertGsiRow: %v", err)
	}

	// Index row exists, keyed by data_id.
	var gotID int64
	var gotHash string
	if err := tx.QueryRow(`SELECT data_id, hash FROM `+GsiTableName("T", "g1")).Scan(&gotID, &gotHash); err != nil {
		t.Fatalf("query index row: %v", err)
	}
	if gotID != id || gotHash != "G1" {
		t.Errorf("index row = (data_id %d, hash %q), want (%d, G1)", gotID, gotHash, id)
	}

	// Deleting the base item cascades to the index row (foreign_keys=ON).
	if _, err := s.DeleteItem(tx, "T", "k1", nil); err != nil {
		t.Fatalf("DeleteItem: %v", err)
	}
	var n int
	if err := tx.QueryRow(`SELECT count(*) FROM ` + GsiTableName("T", "g1")).Scan(&n); err != nil {
		t.Fatalf("count after delete: %v", err)
	}
	if n != 0 {
		t.Fatalf("after base delete, index rows = %d, want 0 (cascade failed)", n)
	}

	// DropGsiTable is idempotent (DROP TABLE IF EXISTS).
	if err := s.DropGsiTable(tx, "T", "g1"); err != nil {
		t.Fatalf("DropGsiTable: %v", err)
	}
	if err := s.DropGsiTable(tx, "T", "g1"); err != nil {
		t.Errorf("DropGsiTable idempotent: %v", err)
	}
}

func TestUpsertGsiRowUpdatesKey(t *testing.T) {
	s := newTestStore(t)
	tx, _ := s.BeginTx(context.Background())
	defer tx.Rollback()

	def := dataDef()
	s.CreateDataTable(tx, def)
	gsi := GsiDef{Name: "g1", Hash: "gsi_pk", HashType: "S", Range: "gsi_sk", RangeType: "S", ProjectionType: "ALL"}
	if err := s.CreateGsiTable(tx, def, gsi); err != nil {
		t.Fatalf("CreateGsiTable: %v", err)
	}
	id, _ := s.PutItem(tx, "T", "k1", nil, []byte("a"))

	// First write.
	if err := s.UpsertGsiRow(tx, "T", "g1", id, "G1", "sk1"); err != nil {
		t.Fatalf("UpsertGsiRow first: %v", err)
	}
	// Re-upserting the same data_id with a different index key updates in
	// place, not an insert/duplicate (data_id is the PRIMARY KEY).
	if err := s.UpsertGsiRow(tx, "T", "g1", id, "G2", "sk2"); err != nil {
		t.Fatalf("UpsertGsiRow second: %v", err)
	}

	var gotHash, gotRange string
	if err := tx.QueryRow(`SELECT hash, range FROM `+GsiTableName("T", "g1")).Scan(&gotHash, &gotRange); err != nil {
		t.Fatalf("query index row: %v", err)
	}
	if gotHash != "G2" || gotRange != "sk2" {
		t.Errorf("index key = (%q, %q), want (G2, sk2)", gotHash, gotRange)
	}
	var n int
	if err := tx.QueryRow(`SELECT count(*) FROM ` + GsiTableName("T", "g1")).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("index rows = %d, want 1 (upsert must not duplicate)", n)
	}
}

func TestPutItemReturnsDataID(t *testing.T) {
	s := newTestStore(t)
	tx, _ := s.BeginTx(context.Background())
	defer tx.Rollback()

	s.CreateDataTable(tx, dataDef())
	id1, err := s.PutItem(tx, "T", "k1", nil, []byte("a"))
	if err != nil {
		t.Fatalf("PutItem: %v", err)
	}
	if id1 <= 0 {
		t.Errorf("PutItem id = %d, want > 0", id1)
	}
	// GetItem returns the matching rowid.
	gotID, _, _, err := s.GetItem(tx, "T", "k1", nil)
	if err != nil || gotID != id1 {
		t.Errorf("GetItem id = %d (err %v), want %d", gotID, err, id1)
	}
	// INSERT OR REPLACE on the same key deletes the conflicting row (cascading
	// away the old GSI row) and inserts a fresh row with a NEW rowid, so the
	// GSI maintenance caller must re-UpsertGsiRow with the new id.
	id3, _ := s.PutItem(tx, "T", "k1", nil, []byte("a2"))
	if id3 == id1 {
		t.Errorf("overwrite returned same id %d; REPLACE should assign a new rowid", id1)
	}
	if id3 <= 0 {
		t.Errorf("overwrite id = %d, want > 0", id3)
	}
}

func seedGsiData(t *testing.T, s *Store, tx *sql.Tx) (string, []int64) {
	t.Helper()
	def := TableDef{Name: "G", Hash: "pk", HashType: "S", Range: "sk", RangeType: "S"}
	s.CreateDataTable(tx, def)
	gsi := GsiDef{Name: "g1", Hash: "gsi_pk", HashType: "S", Range: "gsi_sk", RangeType: "S", ProjectionType: "ALL"}
	s.CreateGsiTable(tx, def, gsi)

	// Two items share gsi_pk=G1, gsi_sk=s1 (non-unique sort key); one shares G1 but different sk.
	ids := make([]int64, 0, 3)
	for _, it := range []struct{ pk, sk, gpk, gsk, blob string }{
		{"A", "a", "G1", "s1", "A"},
		{"C", "c", "G1", "s1", "C"}, // tied sort key with A
		{"B", "b", "G1", "s2", "B"},
	} {
		id, _ := s.PutItem(tx, "G", it.pk, it.sk, []byte(it.blob))
		s.UpsertGsiRow(tx, "G", "g1", id, it.gpk, it.gsk)
		ids = append(ids, id)
	}
	return "g1", ids
}

func TestQueryGSIAllAndOrder(t *testing.T) {
	s := newTestStore(t)
	tx, _ := s.BeginTx(context.Background())
	defer tx.Rollback()
	_, _ = seedGsiData(t, s, tx)

	blobs, err := s.QueryGSI(tx, "G", "g1", "G1", &SortKeyCond{Op: ""}, nil, true, 0)
	if err != nil {
		t.Fatalf("QueryGSI: %v", err)
	}
	// Order: (G1, s1, data_id ASC) -> A, C ; then (G1, s2) -> B.
	want := []string{"A", "C", "B"}
	if len(blobs) != 3 {
		t.Fatalf("got %d blobs, want 3", len(blobs))
	}
	for i, w := range want {
		if string(blobs[i]) != w {
			t.Errorf("blob[%d] = %q, want %q", i, blobs[i], w)
		}
	}
}

func TestQueryGSIDesc(t *testing.T) {
	s := newTestStore(t)
	tx, _ := s.BeginTx(context.Background())
	defer tx.Rollback()
	seedGsiData(t, s, tx)

	blobs, _ := s.QueryGSI(tx, "G", "g1", "G1", &SortKeyCond{Op: ""}, nil, false, 0)
	want := []string{"B", "C", "A"} // s2 desc -> B; s1 desc, data_id desc -> C, A
	if len(blobs) != 3 {
		t.Fatalf("got %d, want 3", len(blobs))
	}
	for i, w := range want {
		if string(blobs[i]) != w {
			t.Errorf("desc blob[%d] = %q, want %q", i, blobs[i], w)
		}
	}
}

func TestQueryGSIResumeAfterTiedSortKey(t *testing.T) {
	s := newTestStore(t)
	tx, _ := s.BeginTx(context.Background())
	defer tx.Rollback()
	_, ids := seedGsiData(t, s, tx)

	// Resume after A (the first of the two tied s1 items): must return C then B.
	resume := &GsiResume{Range: "s1", DataID: ids[0]}
	blobs, err := s.QueryGSI(tx, "G", "g1", "G1", &SortKeyCond{Op: ""}, resume, true, 0)
	if err != nil {
		t.Fatalf("QueryGSI resume: %v", err)
	}
	want := []string{"C", "B"}
	if len(blobs) != 2 {
		t.Fatalf("resume got %d, want 2", len(blobs))
	}
	for i, w := range want {
		if string(blobs[i]) != w {
			t.Errorf("resume blob[%d] = %q, want %q", i, blobs[i], w)
		}
	}
}

func TestQueryGSISortOps(t *testing.T) {
	s := newTestStore(t)
	tx, _ := s.BeginTx(context.Background())
	defer tx.Rollback()
	seedGsiData(t, s, tx)

	cases := []struct {
		name      string
		cond      *SortKeyCond
		wantBlobs []string
	}{
		{"sk = s1", &SortKeyCond{Op: "=", Lo: "s1"}, []string{"A", "C"}},
		{"sk < s2", &SortKeyCond{Op: "<", Lo: "s2"}, []string{"A", "C"}},
		{"sk >= s2", &SortKeyCond{Op: ">=", Lo: "s2"}, []string{"B"}},
		{"sk BETWEEN s1 AND s2", &SortKeyCond{Op: "BETWEEN", Lo: "s1", Hi: "s2"}, []string{"A", "C", "B"}},
		{"BEGINS_WITH s", &SortKeyCond{Op: "BEGINS_WITH", Lo: "s", Hi: "t"}, []string{"A", "C", "B"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			blobs, err := s.QueryGSI(tx, "G", "g1", "G1", tc.cond, nil, true, 0)
			if err != nil {
				t.Fatalf("QueryGSI: %v", err)
			}
			if len(blobs) != len(tc.wantBlobs) {
				t.Fatalf("got %d, want %d (%v)", len(blobs), len(tc.wantBlobs), blobs)
			}
			for i, w := range tc.wantBlobs {
				if string(blobs[i]) != w {
					t.Errorf("blob[%d] = %q, want %q", i, blobs[i], w)
				}
			}
		})
	}
}

func TestQueryGSILimit(t *testing.T) {
	s := newTestStore(t)
	tx, _ := s.BeginTx(context.Background())
	defer tx.Rollback()
	seedGsiData(t, s, tx)

	blobs, _ := s.QueryGSI(tx, "G", "g1", "G1", &SortKeyCond{Op: ""}, nil, true, 2)
	if len(blobs) != 2 {
		t.Fatalf("limit 2: got %d, want 2", len(blobs))
	}
	if string(blobs[0]) != "A" || string(blobs[1]) != "C" {
		t.Errorf("limit order = %q %q, want A C", blobs[0], blobs[1])
	}
}

func TestQueryGSIPartitionOnly(t *testing.T) {
	s := newTestStore(t)
	tx, _ := s.BeginTx(context.Background())
	defer tx.Rollback()

	def := TableDef{Name: "PO", Hash: "pk", HashType: "S"}
	s.CreateDataTable(tx, def)
	gsi := GsiDef{Name: "g1", Hash: "gsi_pk", HashType: "S", ProjectionType: "KEYS_ONLY"}
	s.CreateGsiTable(tx, def, gsi)

	id1, _ := s.PutItem(tx, "PO", "k1", nil, []byte("one"))
	s.UpsertGsiRow(tx, "PO", "g1", id1, "G1", nil)
	id2, _ := s.PutItem(tx, "PO", "k2", nil, []byte("two"))
	s.UpsertGsiRow(tx, "PO", "g1", id2, "G1", nil)

	blobs, err := s.QueryGSI(tx, "PO", "g1", "G1", nil, nil, true, 0)
	if err != nil {
		t.Fatalf("QueryGSI partition-only: %v", err)
	}
	if len(blobs) != 2 {
		t.Fatalf("got %d, want 2", len(blobs))
	}
	// Resume by data_id only (Range nil).
	resume := &GsiResume{Range: nil, DataID: id1}
	blobs, _ = s.QueryGSI(tx, "PO", "g1", "G1", nil, resume, true, 0)
	if len(blobs) != 1 || string(blobs[0]) != "two" {
		t.Errorf("resume partition-only: %v, want [two]", blobs)
	}
}

func TestScanGSI(t *testing.T) {
	s := newTestStore(t)
	tx, _ := s.BeginTx(context.Background())
	defer tx.Rollback()
	_, ids := seedGsiData(t, s, tx)

	blobs, err := s.ScanGSI(tx, "G", "g1", 0, 0)
	if err != nil {
		t.Fatalf("ScanGSI: %v", err)
	}
	if len(blobs) != 3 {
		t.Fatalf("full scan: got %d, want 3", len(blobs))
	}
	// data_id order.
	want := []string{"A", "C", "B"} // insertion order = data_id order
	for i, w := range want {
		if string(blobs[i]) != w {
			t.Errorf("scan blob[%d] = %q, want %q", i, blobs[i], w)
		}
	}

	// Resume after the first id.
	blobs, _ = s.ScanGSI(tx, "G", "g1", ids[0], 0)
	if len(blobs) != 2 {
		t.Errorf("resume scan: got %d, want 2", len(blobs))
	}

	// Limit.
	blobs, _ = s.ScanGSI(tx, "G", "g1", 0, 2)
	if len(blobs) != 2 {
		t.Errorf("limit 2: got %d, want 2", len(blobs))
	}
}

func TestQueryGSIDescResume(t *testing.T) {
	s := newTestStore(t)
	tx, _ := s.BeginTx(context.Background())
	defer tx.Rollback()
	_, ids := seedGsiData(t, s, tx)

	// DESC scan resumed after B (Range s2, B's data_id) must continue from the
	// lower sort key s1 in data_id DESC order: C then A.
	resume := &GsiResume{Range: "s2", DataID: ids[2]} // ids: A, C, B -> ids[2] = B
	blobs, err := s.QueryGSI(tx, "G", "g1", "G1", &SortKeyCond{Op: ""}, resume, false, 0)
	if err != nil {
		t.Fatalf("QueryGSI desc resume: %v", err)
	}
	want := []string{"C", "A"}
	if len(blobs) != 2 {
		t.Fatalf("desc resume got %d, want 2 (%v)", len(blobs), blobs)
	}
	for i, w := range want {
		if string(blobs[i]) != w {
			t.Errorf("desc resume blob[%d] = %q, want %q", i, blobs[i], w)
		}
	}
}

// TestQueryGSICompositePartitionOnlyOrdersByRange is a regression test for a
// QueryGSI ordering bug: a partition-equality-only query on a composite GSI
// (sortCond = &SortKeyCond{Op: ""}) must order by the GSI range key (then
// data_id as a stable tiebreak), NOT by data_id/insertion order. The items are
// inserted deliberately out of range order (B/s2 first) so data_id order
// [B,A,C] differs from range order [A,C,B]; the previous code fell through to
// data_id-only ordering and returned insertion order, which also broke
// resume-based pagination.
func TestQueryGSICompositePartitionOnlyOrdersByRange(t *testing.T) {
	s := newTestStore(t)
	tx, _ := s.BeginTx(context.Background())
	defer tx.Rollback()

	def := TableDef{Name: "G", Hash: "pk", HashType: "S", Range: "sk", RangeType: "S"}
	s.CreateDataTable(tx, def)
	gsi := GsiDef{Name: "g1", Hash: "gsi_pk", HashType: "S", Range: "gsi_sk", RangeType: "S", ProjectionType: "ALL"}
	s.CreateGsiTable(tx, def, gsi)

	// Insert B (s2) FIRST so data_id order [B,A,C] != range order [A,C,B].
	for _, it := range []struct{ pk, sk, gpk, gsk, blob string }{
		{"B", "b", "G1", "s2", "B"},
		{"A", "a", "G1", "s1", "A"},
		{"C", "c", "G1", "s1", "C"},
	} {
		id, _ := s.PutItem(tx, "G", it.pk, it.sk, []byte(it.blob))
		s.UpsertGsiRow(tx, "G", "g1", id, it.gpk, it.gsk)
	}

	// ASC: range order s1(A), s1(C) [data_id tiebreak], s2(B).
	blobs, err := s.QueryGSI(tx, "G", "g1", "G1", &SortKeyCond{Op: ""}, nil, true, 0)
	if err != nil {
		t.Fatalf("QueryGSI ASC: %v", err)
	}
	wantASC := []string{"A", "C", "B"}
	if len(blobs) != 3 {
		t.Fatalf("ASC got %d blobs, want 3 (%q)", len(blobs), blobs)
	}
	for i, w := range wantASC {
		if string(blobs[i]) != w {
			t.Errorf("ASC blob[%d] = %q, want %q (range order, not insertion order)", i, blobs[i], w)
		}
	}

	// DESC: range order s2(B), s1(C), s1(A).
	blobs, err = s.QueryGSI(tx, "G", "g1", "G1", &SortKeyCond{Op: ""}, nil, false, 0)
	if err != nil {
		t.Fatalf("QueryGSI DESC: %v", err)
	}
	wantDESC := []string{"B", "C", "A"}
	if len(blobs) != 3 {
		t.Fatalf("DESC got %d blobs, want 3 (%q)", len(blobs), blobs)
	}
	for i, w := range wantDESC {
		if string(blobs[i]) != w {
			t.Errorf("DESC blob[%d] = %q, want %q (range order, not insertion order)", i, blobs[i], w)
		}
	}

	// Resume pagination: a Limit=2 page takes A,C (range order) and resuming
	// after C (range s1, C's data_id) returns B — only possible when the initial
	// page was range-ordered.
	page, err := s.QueryGSI(tx, "G", "g1", "G1", &SortKeyCond{Op: ""}, nil, true, 2)
	if err != nil {
		t.Fatalf("QueryGSI page: %v", err)
	}
	if len(page) != 2 || string(page[0]) != "A" || string(page[1]) != "C" {
		t.Fatalf("page = %q, want [A C]", page)
	}
	cID, _, _, err := s.GetItem(tx, "G", "C", "c")
	if err != nil {
		t.Fatalf("GetItem C: %v", err)
	}
	blobs, err = s.QueryGSI(tx, "G", "g1", "G1", &SortKeyCond{Op: ""}, &GsiResume{Range: "s1", DataID: cID}, true, 0)
	if err != nil {
		t.Fatalf("QueryGSI resume: %v", err)
	}
	if len(blobs) != 1 || string(blobs[0]) != "B" {
		t.Errorf("resume = %q, want [B]", blobs)
	}
}
