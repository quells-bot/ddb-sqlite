package ddb

import (
	"context"
	"errors"
	"testing"

	"github.com/quells-bot/ddb-sqlite-core/attrval"
	"github.com/quells-bot/ddb-sqlite-core/internal/storage"
)

// newEmptyTable creates a table "T" with pk HASH S, sk RANGE S and no GSIs —
// the base for UpdateTable add tests (its AttributeDefinitions are pk, sk only).
func newEmptyTable(t *testing.T) *Client {
	t.Helper()
	c := newClient(t)
	ctx := context.Background()
	_, err := c.CreateTable(ctx, CreateTableInput{
		TableName: "T",
		KeySchema: []KeySchemaElement{
			{AttributeName: "pk", KeyType: "HASH"},
			{AttributeName: "sk", KeyType: "RANGE"},
		},
		AttributeDefinitions: []AttributeDefinition{
			{AttributeName: "pk", AttributeType: "S"},
			{AttributeName: "sk", AttributeType: "S"},
		},
	})
	if err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	return c
}

func strAttr(name, v string) AttributeDefinition {
	return AttributeDefinition{AttributeName: name, AttributeType: v}
}

func TestValidateUpdateTableShape(t *testing.T) {
	gsi := GlobalSecondaryIndex{
		IndexName:  "g1",
		KeySchema:  []KeySchemaElement{{AttributeName: "gp", KeyType: "HASH"}},
		Projection: Projection{Type: "ALL"},
	}
	cases := []struct {
		name string
		in   UpdateTableInput
		want error
	}{
		{
			"truly empty rejected",
			UpdateTableInput{TableName: "T"},
			ErrValidation,
		},
		{
			"throughput-only no-op ok",
			UpdateTableInput{TableName: "T", NonGsiFieldsPresent: true},
			nil,
		},
		{
			"two entries rejected",
			UpdateTableInput{TableName: "T", GlobalSecondaryIndexUpdates: []GlobalSecondaryIndexUpdate{
				{Create: &gsi}, {Create: &gsi},
			}},
			ErrValidation,
		},
		{
			"neither set rejected",
			UpdateTableInput{TableName: "T", GlobalSecondaryIndexUpdates: []GlobalSecondaryIndexUpdate{{}}},
			ErrValidation,
		},
		{
			"both set rejected",
			UpdateTableInput{TableName: "T", GlobalSecondaryIndexUpdates: []GlobalSecondaryIndexUpdate{
				{Create: &gsi, Delete: strPtr("g1")},
			}},
			ErrValidation,
		},
		{
			"create plus ignored field rejected",
			UpdateTableInput{TableName: "T", NonGsiFieldsPresent: true, GlobalSecondaryIndexUpdates: []GlobalSecondaryIndexUpdate{{Create: &gsi}}},
			ErrValidation,
		},
		{
			"attribute defs without create rejected",
			UpdateTableInput{TableName: "T", NonGsiFieldsPresent: true, AttributeDefinitions: []AttributeDefinition{strAttr("gp", "S")}},
			ErrValidation,
		},
		{
			"attribute defs with delete rejected",
			UpdateTableInput{TableName: "T", GlobalSecondaryIndexUpdates: []GlobalSecondaryIndexUpdate{{Delete: strPtr("g1")}}, AttributeDefinitions: []AttributeDefinition{strAttr("gp", "S")}},
			ErrValidation,
		},
		{
			"create only ok",
			UpdateTableInput{TableName: "T", AttributeDefinitions: []AttributeDefinition{strAttr("gp", "S")}, GlobalSecondaryIndexUpdates: []GlobalSecondaryIndexUpdate{{Create: &gsi}}},
			nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateUpdateTableShape(tc.in)
			if tc.want == nil {
				if err != nil {
					t.Errorf("err = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func strPtr(s string) *string { return &s }

func baseCreateInput(name string, ks []KeySchemaElement, defs []AttributeDefinition) UpdateTableInput {
	return UpdateTableInput{
		TableName:            "T",
		AttributeDefinitions: defs,
		GlobalSecondaryIndexUpdates: []GlobalSecondaryIndexUpdate{{
			Create: &GlobalSecondaryIndex{IndexName: name, KeySchema: ks, Projection: Projection{Type: "ALL"}},
		}},
	}
}

func hashKey(attr string) []KeySchemaElement {
	return []KeySchemaElement{{AttributeName: attr, KeyType: "HASH"}}
}

func TestValidateCreateGsi(t *testing.T) {
	// Table "T": pk HASH S, sk RANGE S, no GSIs.
	def := storage.TableDef{
		ID: 1, Name: "T", Hash: "pk", HashType: "S", Range: "sk", RangeType: "S",
	}
	gp := strAttr("gp", "S")

	cases := []struct {
		name string
		in   UpdateTableInput
		def  storage.TableDef
		want error
	}{
		{
			"valid create",
			baseCreateInput("g1x", hashKey("gp"), []AttributeDefinition{gp}),
			def, nil,
		},
		{
			"invalid name format",
			baseCreateInput("x", hashKey("gp"), []AttributeDefinition{gp}),
			def, ErrValidation,
		},
		{
			"missing attribute declaration",
			baseCreateInput("g1x", hashKey("gp"), nil),
			def, ErrValidation,
		},
		{
			"conflicting type redeclaration",
			baseCreateInput("g1x", hashKey("sk"), []AttributeDefinition{strAttr("sk", "N")}),
			def, ErrValidation,
		},
		{
			"unused input attr",
			baseCreateInput("g1x", hashKey("gp"), []AttributeDefinition{gp, strAttr("extra", "S")}),
			def, ErrValidation,
		},
		{
			"same-type redeclaration accepted",
			baseCreateInput("g1x", hashKey("sk"), []AttributeDefinition{strAttr("sk", "S")}),
			def, nil, // sk already declared S in the table; re-declaring S is fine
		},
		{
			"already-declared key not repeated in call accepted",
			baseCreateInput("g1x", hashKey("sk"), nil),
			def, nil, // sk already declared; merged map supplies it (P11 default)
		},
		{
			"name already in use",
			baseCreateInput("g1x", hashKey("gp"), []AttributeDefinition{gp}),
			storage.TableDef{
				ID: 1, Name: "T", Hash: "pk", HashType: "S", Range: "sk", RangeType: "S",
				GSIs: []storage.GsiDef{{Name: "g1x", Hash: "gp", HashType: "S", ProjectionType: "ALL"}},
			},
			ErrGsiInUse,
		},
		{
			"20-GSI cap",
			baseCreateInput("g21", hashKey("gp"), []AttributeDefinition{gp}),
			storage.TableDef{
				ID: 1, Name: "T", Hash: "pk", HashType: "S", Range: "sk", RangeType: "S",
				GSIs: func() []storage.GsiDef {
					out := make([]storage.GsiDef, 20)
					for i := range out {
						out[i] = storage.GsiDef{Name: string(rune('a'+i)) + "xx", Hash: "gp", HashType: "S", ProjectionType: "ALL"}
					}
					return out
				}(),
			},
			ErrLimitExceeded,
		},
		{
			"composite valid",
			UpdateTableInput{
				TableName:            "T",
				AttributeDefinitions: []AttributeDefinition{strAttr("gh", "S"), strAttr("gr", "S")},
				GlobalSecondaryIndexUpdates: []GlobalSecondaryIndexUpdate{{
					Create: &GlobalSecondaryIndex{
						IndexName: "g1x",
						KeySchema: []KeySchemaElement{
							{AttributeName: "gh", KeyType: "HASH"},
							{AttributeName: "gr", KeyType: "RANGE"},
						},
						Projection: Projection{Type: "ALL"},
					},
				}},
			},
			def, nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := validateCreateGsi(tc.def, tc.in)
			if tc.want == nil {
				if err != nil {
					t.Errorf("err = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestUpdateTableCreateOnEmpty(t *testing.T) {
	c := newEmptyTable(t)
	ctx := context.Background()
	out, err := c.UpdateTable(ctx, UpdateTableInput{
		TableName:            "T",
		AttributeDefinitions: []AttributeDefinition{strAttr("gp", "S")},
		GlobalSecondaryIndexUpdates: []GlobalSecondaryIndexUpdate{{
			Create: &GlobalSecondaryIndex{
				IndexName:  "g1a",
				KeySchema:  []KeySchemaElement{{AttributeName: "gp", KeyType: "HASH"}},
				Projection: Projection{Type: "ALL"},
			},
		}},
	})
	if err != nil {
		t.Fatalf("UpdateTable create: %v", err)
	}
	if len(out.TableDescription.GlobalSecondaryIndexes) != 1 {
		t.Fatalf("desc GSIs = %d, want 1", len(out.TableDescription.GlobalSecondaryIndexes))
	}
	if out.TableDescription.GlobalSecondaryIndexes[0].IndexName != "g1a" {
		t.Errorf("GSI name = %q, want g1a", out.TableDescription.GlobalSecondaryIndexes[0].IndexName)
	}
	// Query the (empty) GSI works and returns no items.
	q, err := c.Query(ctx, QueryInput{
		TableName: "T", IndexName: "g1a",
		KeyConditionExpression:    "gp = :v",
		ExpressionAttributeValues: map[string]attrval.Value{":v": attrval.NewString("any")},
	})
	if err != nil {
		t.Fatalf("query new GSI: %v", err)
	}
	if len(q.Items) != 0 {
		t.Errorf("empty GSI query returned %d items, want 0", len(q.Items))
	}
}

func TestUpdateTableCreateBackfill(t *testing.T) {
	c := newEmptyTable(t)
	ctx := context.Background()
	// Seed items before the GSI exists: two indexable, one sparse (no gp).
	c.PutItem(ctx, PutItemInput{TableName: "T", Item: Item{
		"pk": attrval.NewString("A"), "sk": attrval.NewString("a"),
		"gp": attrval.NewString("G1"), "gr": attrval.NewString("s1"),
	}})
	c.PutItem(ctx, PutItemInput{TableName: "T", Item: Item{
		"pk": attrval.NewString("B"), "sk": attrval.NewString("b"),
		"gp": attrval.NewString("G1"), "gr": attrval.NewString("s2"),
	}})
	c.PutItem(ctx, PutItemInput{TableName: "T", Item: Item{
		"pk": attrval.NewString("D"), "sk": attrval.NewString("d"), // sparse: no gp
	}})

	if _, err := c.UpdateTable(ctx, UpdateTableInput{
		TableName:            "T",
		AttributeDefinitions: []AttributeDefinition{strAttr("gp", "S"), strAttr("gr", "S")},
		GlobalSecondaryIndexUpdates: []GlobalSecondaryIndexUpdate{{
			Create: &GlobalSecondaryIndex{
				IndexName: "g1a",
				KeySchema: []KeySchemaElement{
					{AttributeName: "gp", KeyType: "HASH"},
					{AttributeName: "gr", KeyType: "RANGE"},
				},
				Projection: Projection{Type: "ALL"},
			},
		}},
	}); err != nil {
		t.Fatalf("UpdateTable: %v", err)
	}

	q, err := c.Query(ctx, QueryInput{
		TableName: "T", IndexName: "g1a",
		KeyConditionExpression:    "gp = :v",
		ExpressionAttributeValues: map[string]attrval.Value{":v": attrval.NewString("G1")},
		ScanIndexForward:          true,
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(q.Items) != 2 {
		t.Fatalf("backfill query returned %d items, want 2 (sparse item absent)", len(q.Items))
	}
	got := []string{q.Items[0]["pk"].Str(), q.Items[1]["pk"].Str()}
	want := []string{"A", "B"} // gr ASC: s1, s2
	for i, w := range want {
		if got[i] != w {
			t.Errorf("item[%d].pk = %q, want %q", i, got[i], w)
		}
	}
}

// TestUpdateTableBackfillSkipsNonIndexable is the regression test for the §2.2
// indexability predicate: pre-GSI items with a wrong-typed, non-scalar, or empty
// value under the new GSI's key-attr name must be skipped, not abort the call.
func TestUpdateTableBackfillSkipsNonIndexable(t *testing.T) {
	c := newEmptyTable(t)
	ctx := context.Background()
	// Table has no GSI on "gp" yet, so these puts are accepted unconditionally.
	c.PutItem(ctx, PutItemInput{TableName: "T", Item: Item{
		"pk": attrval.NewString("good"), "sk": attrval.NewString("a"),
		"gp": attrval.NewString("G1"), // indexable
	}})
	c.PutItem(ctx, PutItemInput{TableName: "T", Item: Item{
		"pk": attrval.NewString("num"), "sk": attrval.NewString("b"),
		"gp": attrval.NewNumber(mustNum("5")), // wrong type (S declared)
	}})
	c.PutItem(ctx, PutItemInput{TableName: "T", Item: Item{
		"pk": attrval.NewString("list"), "sk": attrval.NewString("c"),
		"gp": attrval.NewList([]attrval.Value{attrval.NewString("x")}), // non-scalar
	}})
	c.PutItem(ctx, PutItemInput{TableName: "T", Item: Item{
		"pk": attrval.NewString("empty"), "sk": attrval.NewString("d"),
		"gp": attrval.NewString(""), // empty string
	}})

	if _, err := c.UpdateTable(ctx, UpdateTableInput{
		TableName:            "T",
		AttributeDefinitions: []AttributeDefinition{strAttr("gp", "S")},
		GlobalSecondaryIndexUpdates: []GlobalSecondaryIndexUpdate{{
			Create: &GlobalSecondaryIndex{
				IndexName:  "g1a",
				KeySchema:  []KeySchemaElement{{AttributeName: "gp", KeyType: "HASH"}},
				Projection: Projection{Type: "ALL"},
			},
		}},
	}); err != nil {
		t.Fatalf("UpdateTable with non-indexable pre-GSI items: %v", err)
	}
	q, err := c.Query(ctx, QueryInput{
		TableName: "T", IndexName: "g1a",
		KeyConditionExpression:    "gp = :v",
		ExpressionAttributeValues: map[string]attrval.Value{":v": attrval.NewString("G1")},
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(q.Items) != 1 || q.Items[0]["pk"].Str() != "good" {
		t.Errorf("index = %v, want only [good]", q.Items)
	}
}

// TestUpdateTableCreateOverlappingKey: a GSI whose partition key is the table's
// sort key (already declared) is allowed and indexed correctly (P11 default).
func TestUpdateTableCreateOverlappingKey(t *testing.T) {
	c := newEmptyTable(t)
	ctx := context.Background()
	c.PutItem(ctx, PutItemInput{TableName: "T", Item: Item{
		"pk": attrval.NewString("A"), "sk": attrval.NewString("sortval"),
	}})
	if _, err := c.UpdateTable(ctx, UpdateTableInput{
		TableName: "T",
		// No AttributeDefinitions: sk is already declared on the table.
		GlobalSecondaryIndexUpdates: []GlobalSecondaryIndexUpdate{{
			Create: &GlobalSecondaryIndex{
				IndexName:  "bysk",
				KeySchema:  []KeySchemaElement{{AttributeName: "sk", KeyType: "HASH"}},
				Projection: Projection{Type: "KEYS_ONLY"},
			},
		}},
	}); err != nil {
		t.Fatalf("UpdateTable overlapping key: %v", err)
	}
	q, err := c.Query(ctx, QueryInput{
		TableName: "T", IndexName: "bysk",
		KeyConditionExpression:    "sk = :v",
		ExpressionAttributeValues: map[string]attrval.Value{":v": attrval.NewString("sortval")},
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(q.Items) != 1 || q.Items[0]["pk"].Str() != "A" {
		t.Errorf("overlapping-key query = %v, want [A]", q.Items)
	}
}

func TestUpdateTableDelete(t *testing.T) {
	c := newEmptyTable(t)
	ctx := context.Background()
	// Add a GSI, then delete it.
	c.UpdateTable(ctx, UpdateTableInput{
		TableName:            "T",
		AttributeDefinitions: []AttributeDefinition{strAttr("gp", "S")},
		GlobalSecondaryIndexUpdates: []GlobalSecondaryIndexUpdate{{
			Create: &GlobalSecondaryIndex{IndexName: "g1a", KeySchema: hashKey("gp"), Projection: Projection{Type: "ALL"}},
		}},
	})
	if _, err := c.UpdateTable(ctx, UpdateTableInput{
		TableName:                   "T",
		GlobalSecondaryIndexUpdates: []GlobalSecondaryIndexUpdate{{Delete: strPtr("g1a")}},
	}); err != nil {
		t.Fatalf("UpdateTable delete: %v", err)
	}
	desc, _ := c.DescribeTable(ctx, DescribeTableInput{TableName: "T"})
	if len(desc.GlobalSecondaryIndexes) != 0 {
		t.Errorf("after delete, GSIs = %v, want none", desc.GlobalSecondaryIndexes)
	}
	_, err := c.Query(ctx, QueryInput{
		TableName: "T", IndexName: "g1a",
		KeyConditionExpression:    "gp = :v",
		ExpressionAttributeValues: map[string]attrval.Value{":v": attrval.NewString("x")},
	})
	if !errors.Is(err, ErrGsiNotFound) {
		t.Errorf("query deleted GSI: err = %v, want ErrGsiNotFound", err)
	}
}

func TestUpdateTableDeleteUnknown(t *testing.T) {
	c := newEmptyTable(t)
	ctx := context.Background()
	_, err := c.UpdateTable(ctx, UpdateTableInput{
		TableName:                   "T",
		GlobalSecondaryIndexUpdates: []GlobalSecondaryIndexUpdate{{Delete: strPtr("nope")}},
	})
	if !errors.Is(err, ErrGsiNotFoundForDelete) {
		t.Errorf("delete unknown GSI: err = %v, want ErrGsiNotFoundForDelete", err)
	}
}

func TestUpdateTableNoOpThroughputOnly(t *testing.T) {
	c := newEmptyTable(t)
	ctx := context.Background()
	before, _ := c.DescribeTable(ctx, DescribeTableInput{TableName: "T"})
	out, err := c.UpdateTable(ctx, UpdateTableInput{TableName: "T", NonGsiFieldsPresent: true})
	if err != nil {
		t.Fatalf("throughput-only UpdateTable: %v", err)
	}
	if len(out.TableDescription.GlobalSecondaryIndexes) != len(before.GlobalSecondaryIndexes) {
		t.Errorf("no-op changed GSI count: before=%d after=%d", len(before.GlobalSecondaryIndexes), len(out.TableDescription.GlobalSecondaryIndexes))
	}
}

func TestUpdateTableUnknownTablePrecedes(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	_, err := c.UpdateTable(ctx, UpdateTableInput{TableName: "missing"})
	if !errors.Is(err, ErrTableNotFound) {
		t.Errorf("err = %v, want ErrTableNotFound", err)
	}
}

// TestUpdateTableFailedAddRollback: a backfill decode error rolls the whole
// UpdateTable tx back — the GSI catalog row and index table are absent after.
func TestUpdateTableFailedAddRollback(t *testing.T) {
	c := newEmptyTable(t)
	ctx := context.Background()
	// Inject a malformed blob directly via storage on a prior committed tx.
	rtx, _ := c.store.BeginTx(ctx)
	c.store.PutItem(rtx, "T", "badrow", "r", []byte("not json"))
	if err := rtx.Commit(); err != nil {
		t.Fatalf("seed bad blob: %v", err)
	}

	_, err := c.UpdateTable(ctx, UpdateTableInput{
		TableName:            "T",
		AttributeDefinitions: []AttributeDefinition{strAttr("gp", "S")},
		GlobalSecondaryIndexUpdates: []GlobalSecondaryIndexUpdate{{
			Create: &GlobalSecondaryIndex{IndexName: "g1a", KeySchema: hashKey("gp"), Projection: Projection{Type: "ALL"}},
		}},
	})
	if err == nil {
		t.Fatal("UpdateTable with a bad blob should fail during backfill")
	}
	// After rollback, the GSI must be absent.
	desc, _ := c.DescribeTable(ctx, DescribeTableInput{TableName: "T"})
	if len(desc.GlobalSecondaryIndexes) != 0 {
		t.Errorf("after failed add, GSIs = %v, want none (rollback)", desc.GlobalSecondaryIndexes)
	}
}
