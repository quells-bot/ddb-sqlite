package ddb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/quells-bot/ddb-sqlite-core/attrval"
)

func TestValidateSelect(t *testing.T) {
	cases := []struct {
		in      string
		gsiProj string
		hasProj bool
		want    string
		wantErr bool
	}{
		{"", "", false, "ALL_ATTRIBUTES", false},
		{"", "KEYS_ONLY", false, "ALL_PROJECTED_ATTRIBUTES", false},
		{"", "INCLUDE", false, "ALL_PROJECTED_ATTRIBUTES", false},
		{"", "ALL", false, "ALL_ATTRIBUTES", false},
		{"", "", true, "ALL_ATTRIBUTES", false},          // projection governs
		{"", "KEYS_ONLY", true, "ALL_ATTRIBUTES", false}, // projection governs on GSI too
		{"ALL_ATTRIBUTES", "", false, "ALL_ATTRIBUTES", false},
		{"ALL_ATTRIBUTES", "KEYS_ONLY", false, "", true}, // non-ALL GSI -> err
		{"ALL_ATTRIBUTES", "KEYS_ONLY", true, "", true},  // + projection -> err
		{"ALL_ATTRIBUTES", "ALL", false, "ALL_ATTRIBUTES", false},
		{"ALL_PROJECTED_ATTRIBUTES", "", false, "", true}, // base table -> err
		{"ALL_PROJECTED_ATTRIBUTES", "KEYS_ONLY", false, "ALL_PROJECTED_ATTRIBUTES", false},
		{"ALL_PROJECTED_ATTRIBUTES", "KEYS_ONLY", true, "", true}, // + projection -> err
		{"ALL_PROJECTED_ATTRIBUTES", "ALL", false, "ALL_PROJECTED_ATTRIBUTES", false},
		{"COUNT", "", false, "COUNT", false},
		{"COUNT", "", true, "", true},                // + projection -> err
		{"SPECIFIC_ATTRIBUTES", "", false, "", true}, // needs projection
		{"SPECIFIC_ATTRIBUTES", "", true, "SPECIFIC_ATTRIBUTES", false},
		{"count", "", false, "", true}, // case-sensitive
	}
	for _, tc := range cases {
		t.Run(tc.in+"/"+tc.gsiProj, func(t *testing.T) {
			got, err := validateSelect(tc.in, tc.gsiProj, tc.hasProj)
			if tc.wantErr {
				if !errors.Is(err, ErrValidation) {
					t.Errorf("validateSelect(%q,%q,%v): err = %v, want ErrValidation", tc.in, tc.gsiProj, tc.hasProj, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateSelect(%q,%q,%v): %v", tc.in, tc.gsiProj, tc.hasProj, err)
			}
			if got != tc.want {
				t.Errorf("validateSelect(%q,%q,%v) = %q, want %q", tc.in, tc.gsiProj, tc.hasProj, got, tc.want)
			}
		})
	}
}

func TestBeginsWithSuccessor(t *testing.T) {
	cases := []struct {
		name    string
		prefix  []byte
		wantHi  any
		wantNil bool // true = no successor (Hi should be nil)
	}{
		{"normal", []byte("abc"), []byte("abd"), false},
		{"last byte 0xFF", []byte{0xFF}, nil, true},
		{"all 0xFF", []byte{0xFF, 0xFF}, nil, true},
		{"empty", []byte{}, nil, true},
		{"carry", []byte{0x41, 0xFF}, []byte{0x42}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hi := beginsWithSuccessor(tc.prefix)
			if tc.wantNil {
				if hi != nil {
					t.Errorf("beginsWithSuccessor(% x) = %v, want nil", tc.prefix, hi)
				}
				return
			}
			b, ok := hi.([]byte)
			if !ok {
				t.Fatalf("beginsWithSuccessor(% x) = %T, want []byte", tc.prefix, hi)
			}
			want := tc.wantHi.([]byte)
			if !bytes.Equal(b, want) {
				t.Errorf("beginsWithSuccessor(% x) = %x, want %x", tc.prefix, b, want)
			}
		})
	}
}

func TestQueryBasic(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	c.CreateTable(ctx, CreateTableInput{
		TableName:            "T",
		KeySchema:            []KeySchemaElement{{AttributeName: "pk", KeyType: "HASH"}, {AttributeName: "sk", KeyType: "RANGE"}},
		AttributeDefinitions: []AttributeDefinition{{AttributeName: "pk", AttributeType: "S"}, {AttributeName: "sk", AttributeType: "N"}},
	})

	for i := range 5 {
		c.PutItem(ctx, PutItemInput{TableName: "T", Item: Item{
			"pk":  attrval.NewString("p1"),
			"sk":  attrval.NewNumber(mustNum(fmt.Sprintf("%d", i))),
			"val": attrval.NewString(fmt.Sprintf("v%d", i)),
		}})
	}

	out, err := c.Query(ctx, QueryInput{
		TableName:                 "T",
		KeyConditionExpression:    "pk = :pk",
		ExpressionAttributeValues: map[string]attrval.Value{":pk": attrval.NewString("p1")},
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if out.Count != 5 {
		t.Errorf("Count = %d, want 5", out.Count)
	}
	if out.ScannedCount != 5 {
		t.Errorf("ScannedCount = %d, want 5", out.ScannedCount)
	}
	if out.LastEvaluatedKey != nil {
		t.Errorf("LEK = %v, want nil (no Limit)", out.LastEvaluatedKey)
	}
}

func TestQueryWithLimit(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	createQueryTable(t, c, ctx, 10) // 10 items in partition p1

	out, err := c.Query(ctx, QueryInput{
		TableName:                 "T",
		KeyConditionExpression:    "pk = :pk",
		ExpressionAttributeValues: map[string]attrval.Value{":pk": attrval.NewString("p1")},
		Limit:                     3,
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if out.ScannedCount != 3 || out.Count != 3 {
		t.Errorf("Scanned=%d Count=%d, want 3/3", out.ScannedCount, out.Count)
	}
	if out.LastEvaluatedKey == nil {
		t.Error("LEK = nil, want non-nil (Limit < scope)")
	}
}

func TestQueryLimitEqualsAvailable(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	createQueryTable(t, c, ctx, 10)

	out, err := c.Query(ctx, QueryInput{
		TableName:                 "T",
		KeyConditionExpression:    "pk = :pk",
		ExpressionAttributeValues: map[string]attrval.Value{":pk": attrval.NewString("p1")},
		Limit:                     10,
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if out.ScannedCount != 10 {
		t.Errorf("ScannedCount = %d, want 10", out.ScannedCount)
	}
	if out.LastEvaluatedKey == nil {
		t.Error("LEK = nil, want non-nil (ScannedCount == Limit)")
	}

	// Resume: should be empty trailing page.
	out2, err := c.Query(ctx, QueryInput{
		TableName:                 "T",
		KeyConditionExpression:    "pk = :pk",
		ExpressionAttributeValues: map[string]attrval.Value{":pk": attrval.NewString("p1")},
		Limit:                     10,
		ExclusiveStartKey:         out.LastEvaluatedKey,
	})
	if err != nil {
		t.Fatalf("Query resume: %v", err)
	}
	if out2.ScannedCount != 0 || out2.Count != 0 {
		t.Errorf("trailing page: Scanned=%d Count=%d, want 0/0", out2.ScannedCount, out2.Count)
	}
	if out2.LastEvaluatedKey != nil {
		t.Error("trailing page LEK = non-nil, want nil")
	}
}

func TestQueryLimitExceedsAvailable(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	createQueryTable(t, c, ctx, 10)

	out, err := c.Query(ctx, QueryInput{
		TableName:                 "T",
		KeyConditionExpression:    "pk = :pk",
		ExpressionAttributeValues: map[string]attrval.Value{":pk": attrval.NewString("p1")},
		Limit:                     15,
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if out.ScannedCount != 10 {
		t.Errorf("ScannedCount = %d, want 10", out.ScannedCount)
	}
	if out.LastEvaluatedKey != nil {
		t.Error("LEK = non-nil, want nil (exhausted)")
	}
}

func TestQueryFilterExpression(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	createQueryTable(t, c, ctx, 10)

	out, err := c.Query(ctx, QueryInput{
		TableName:              "T",
		KeyConditionExpression: "pk = :pk",
		FilterExpression:       "val = :match",
		ExpressionAttributeValues: map[string]attrval.Value{
			":pk":    attrval.NewString("p1"),
			":match": attrval.NewString("v2"),
		},
		ScanIndexForward: true, // forward order so first 3 are sk 0,1,2
		Limit:            3,
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if out.ScannedCount != 3 {
		t.Errorf("ScannedCount = %d, want 3", out.ScannedCount)
	}
	if out.Count != 1 {
		t.Errorf("Count = %d, want 1 (only v2 matches in first 3)", out.Count)
	}
	if out.LastEvaluatedKey == nil {
		t.Error("LEK = nil, want non-nil")
	}
}

func TestQueryScanIndexForwardFalse(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	createQueryTable(t, c, ctx, 5)

	out, err := c.Query(ctx, QueryInput{
		TableName:                 "T",
		KeyConditionExpression:    "pk = :pk",
		ExpressionAttributeValues: map[string]attrval.Value{":pk": attrval.NewString("p1")},
		ScanIndexForward:          false,
		Limit:                     1,
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if out.Count != 1 {
		t.Fatalf("Count = %d, want 1", out.Count)
	}
	sk := out.Items[0]["sk"].Num().String()
	if sk != "4" {
		t.Errorf("first reverse item sk = %s, want 4", sk)
	}
}

func TestQuerySelectCount(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	createQueryTable(t, c, ctx, 5)

	out, err := c.Query(ctx, QueryInput{
		TableName:                 "T",
		KeyConditionExpression:    "pk = :pk",
		ExpressionAttributeValues: map[string]attrval.Value{":pk": attrval.NewString("p1")},
		Select:                    "COUNT",
		Limit:                     3,
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if out.Items != nil {
		t.Errorf("Items = %v, want nil (Select=COUNT)", out.Items)
	}
	if out.Count != 3 || out.ScannedCount != 3 {
		t.Errorf("Count=%d Scanned=%d, want 3/3", out.Count, out.ScannedCount)
	}
	if out.LastEvaluatedKey == nil {
		t.Error("LEK = nil, want non-nil (ScannedCount == Limit)")
	}
}

func TestQueryExclusiveStartKeyMismatch(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	createQueryTable(t, c, ctx, 5)

	_, err := c.Query(ctx, QueryInput{
		TableName:                 "T",
		KeyConditionExpression:    "pk = :pk",
		ExpressionAttributeValues: map[string]attrval.Value{":pk": attrval.NewString("p1")},
		ExclusiveStartKey:         Item{"pk": attrval.NewString("WRONG"), "sk": attrval.NewNumber(mustNum("0"))},
	})
	if !errors.Is(err, ErrValidation) {
		t.Errorf("err = %v, want ErrValidation", err)
	}
}

func TestQueryBeginsWithOnNumber(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	c.CreateTable(ctx, CreateTableInput{
		TableName:            "T",
		KeySchema:            []KeySchemaElement{{AttributeName: "pk", KeyType: "HASH"}, {AttributeName: "sk", KeyType: "RANGE"}},
		AttributeDefinitions: []AttributeDefinition{{AttributeName: "pk", AttributeType: "S"}, {AttributeName: "sk", AttributeType: "N"}},
	})

	_, err := c.Query(ctx, QueryInput{
		TableName:              "T",
		KeyConditionExpression: "pk = :pk AND begins_with(sk, :pre)",
		ExpressionAttributeValues: map[string]attrval.Value{
			":pk":  attrval.NewString("p1"),
			":pre": attrval.NewNumber(mustNum("1")),
		},
	})
	if !errors.Is(err, ErrValidation) {
		t.Errorf("err = %v, want ErrValidation (begins_with on N)", err)
	}
}

// TestQueryBeginsWithOnStringSortKey is the positive S-sort-key counterpart
// to the begins_with-on-N rejection above. It exercises the P0 fix (the upper
// bound successor on an S key must bind as TEXT, not BLOB): before the fix the
// []byte successor bound as a SQLite BLOB, which always compares greater than
// every TEXT value, so the upper bound was a no-op and every row in the
// partition with range >= prefix matched (here: all 5), instead of only the 2
// prefix-matching items.
func TestQueryBeginsWithOnStringSortKey(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	c.CreateTable(ctx, CreateTableInput{
		TableName:            "T",
		KeySchema:            []KeySchemaElement{{AttributeName: "pk", KeyType: "HASH"}, {AttributeName: "sk", KeyType: "RANGE"}},
		AttributeDefinitions: []AttributeDefinition{{AttributeName: "pk", AttributeType: "S"}, {AttributeName: "sk", AttributeType: "S"}},
	})
	for _, sk := range []string{"apple", "apricot", "avocado", "banana", "cherry"} {
		c.PutItem(ctx, PutItemInput{TableName: "T", Item: Item{
			"pk":  attrval.NewString("p1"),
			"sk":  attrval.NewString(sk),
			"val": attrval.NewString(sk),
		}})
	}

	// begins_with(sk, "ap") matches apple and apricot; must exclude
	// avocado (starts with "av"), banana, and cherry.
	out, err := c.Query(ctx, QueryInput{
		TableName:              "T",
		KeyConditionExpression: "pk = :pk AND begins_with(sk, :pre)",
		ExpressionAttributeValues: map[string]attrval.Value{
			":pk":  attrval.NewString("p1"),
			":pre": attrval.NewString("ap"),
		},
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if out.Count != 2 {
		t.Fatalf("Count = %d, want 2 (only prefix-matching items)", out.Count)
	}
	got := map[string]bool{}
	for _, it := range out.Items {
		got[it["sk"].Str()] = true
	}
	for _, want := range []string{"apple", "apricot"} {
		if !got[want] {
			t.Errorf("missing %q from begins_with result", want)
		}
	}
	for _, excl := range []string{"avocado", "banana", "cherry"} {
		if got[excl] {
			t.Errorf("non-prefix item %q included in begins_with result", excl)
		}
	}
}

// createQueryTable is a shared helper: creates a sort-key table and seeds n
// items in partition p1 with sk 0..n-1 and val "v<sk>".
func createQueryTable(t *testing.T, c *Client, ctx context.Context, n int) {
	t.Helper()
	c.CreateTable(ctx, CreateTableInput{
		TableName:            "T",
		KeySchema:            []KeySchemaElement{{AttributeName: "pk", KeyType: "HASH"}, {AttributeName: "sk", KeyType: "RANGE"}},
		AttributeDefinitions: []AttributeDefinition{{AttributeName: "pk", AttributeType: "S"}, {AttributeName: "sk", AttributeType: "N"}},
	})
	for i := range n {
		c.PutItem(ctx, PutItemInput{TableName: "T", Item: Item{
			"pk":  attrval.NewString("p1"),
			"sk":  attrval.NewNumber(mustNum(fmt.Sprintf("%d", i))),
			"val": attrval.NewString(fmt.Sprintf("v%d", i)),
		}})
	}
}

func TestScanBasic(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	createQueryTable(t, c, ctx, 5)

	out, err := c.Scan(ctx, ScanInput{TableName: "T"})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if out.Count != 5 || out.ScannedCount != 5 {
		t.Errorf("Count=%d Scanned=%d, want 5/5", out.Count, out.ScannedCount)
	}
	if out.LastEvaluatedKey != nil {
		t.Error("LEK = non-nil, want nil (no Limit)")
	}
}

func TestScanWithLimit(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	createQueryTable(t, c, ctx, 10)

	out, err := c.Scan(ctx, ScanInput{TableName: "T", Limit: 3})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if out.ScannedCount != 3 {
		t.Errorf("ScannedCount = %d, want 3", out.ScannedCount)
	}
	if out.LastEvaluatedKey == nil {
		t.Error("LEK = nil, want non-nil")
	}
}

func TestScanPagination(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	createQueryTable(t, c, ctx, 10)

	var got []Item
	var start Item
	for {
		out, err := c.Scan(ctx, ScanInput{TableName: "T", Limit: 3, ExclusiveStartKey: start})
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		got = append(got, out.Items...)
		if out.LastEvaluatedKey == nil {
			break
		}
		start = out.LastEvaluatedKey
	}
	if len(got) != 10 {
		t.Errorf("pagination: got %d items, want 10", len(got))
	}
}

func TestQueryNegativeLimit(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	createQueryTable(t, c, ctx, 5)

	_, err := c.Query(ctx, QueryInput{
		TableName:                 "T",
		KeyConditionExpression:    "pk = :pk",
		ExpressionAttributeValues: map[string]attrval.Value{":pk": attrval.NewString("p1")},
		Limit:                     -1,
	})
	if !errors.Is(err, ErrValidation) {
		t.Errorf("err = %v, want ErrValidation (negative Limit)", err)
	}
}

func TestScanNegativeLimit(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	createQueryTable(t, c, ctx, 5)

	_, err := c.Scan(ctx, ScanInput{TableName: "T", Limit: -1})
	if !errors.Is(err, ErrValidation) {
		t.Errorf("err = %v, want ErrValidation (negative Limit)", err)
	}
}

func TestScanNegativeTotalSegments(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	createQueryTable(t, c, ctx, 5)

	_, err := c.Scan(ctx, ScanInput{TableName: "T", TotalSegments: -1})
	if !errors.Is(err, ErrValidation) {
		t.Errorf("err = %v, want ErrValidation (negative TotalSegments)", err)
	}
}

func TestQueryProjection(t *testing.T) {
	c, ctx := newClient(t), context.Background()
	createQueryTable(t, c, ctx, 5) // partition p1, sk 0..4, val "v<sk>"

	out, err := c.Query(ctx, QueryInput{
		TableName:              "T",
		KeyConditionExpression: "pk = :pk",
		ExpressionAttributeValues: map[string]attrval.Value{
			":pk": attrval.NewString("p1"),
		},
		ProjectionExpression: "val",
		ScanIndexForward:     true, // engine default is reverse; this test wants forward order
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if out.Count != 5 || out.ScannedCount != 5 {
		t.Fatalf("Count=%d ScannedCount=%d, want 5/5 (projection does not change counts)", out.Count, out.ScannedCount)
	}
	for i, item := range out.Items {
		if len(item) != 1 || item["val"].Str() != fmt.Sprintf("v%d", i) {
			t.Errorf("item[%d] = %v, want only {val}", i, item)
		}
	}

	// Projection applied after filter: Count=1, ScannedCount=2 with Limit=2.
	out, err = c.Query(ctx, QueryInput{
		TableName:              "T",
		KeyConditionExpression: "pk = :pk",
		FilterExpression:       "val = :v",
		ExpressionAttributeValues: map[string]attrval.Value{
			":pk": attrval.NewString("p1"),
			":v":  attrval.NewString("v1"),
		},
		ProjectionExpression: "val",
		Limit:                2,
		ScanIndexForward:     true, // engine default is reverse; this test wants forward order
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if out.Count != 1 || out.ScannedCount != 2 {
		t.Errorf("Count=%d ScannedCount=%d, want 1/2", out.Count, out.ScannedCount)
	}
	if len(out.Items) != 1 || len(out.Items[0]) != 1 || out.Items[0]["val"].Str() != "v1" {
		t.Errorf("Items = %v, want [{val:v1}]", out.Items)
	}
	// LEK still built from the raw blob even though keys are projected away.
	if len(out.LastEvaluatedKey) != 2 {
		t.Errorf("LEK = %v, want {pk, sk} (from raw blob, not projected item)", out.LastEvaluatedKey)
	}
}

func TestQuerySelectProjectionInteraction(t *testing.T) {
	c, ctx := newClient(t), context.Background()
	createQueryTable(t, c, ctx, 3)
	base := QueryInput{
		TableName:              "T",
		KeyConditionExpression: "pk = :pk",
		ExpressionAttributeValues: map[string]attrval.Value{
			":pk": attrval.NewString("p1"),
		},
		ProjectionExpression: "val",
	}

	// COUNT + projection -> ErrValidation.
	in := base
	in.Select = "COUNT"
	if _, err := c.Query(ctx, in); !errors.Is(err, ErrValidation) {
		t.Errorf("COUNT+proj: err = %v, want ErrValidation", err)
	}

	// SPECIFIC_ATTRIBUTES + projection -> accepted, projected attrs.
	in = base
	in.Select = "SPECIFIC_ATTRIBUTES"
	out, err := c.Query(ctx, in)
	if err != nil {
		t.Fatalf("SPECIFIC_ATTRIBUTES+proj: %v", err)
	}
	if len(out.Items) != 3 || len(out.Items[0]) != 1 {
		t.Errorf("Items = %v, want 3 single-attr items", out.Items)
	}

	// SPECIFIC_ATTRIBUTES without projection -> ErrValidation.
	in = base
	in.Select = "SPECIFIC_ATTRIBUTES"
	in.ProjectionExpression = ""
	if _, err := c.Query(ctx, in); !errors.Is(err, ErrValidation) {
		t.Errorf("SPECIFIC_ATTRIBUTES w/o proj: err = %v, want ErrValidation", err)
	}
}

func TestScanProjection(t *testing.T) {
	c, ctx := newClient(t), context.Background()
	createQueryTable(t, c, ctx, 4)

	out, err := c.Scan(ctx, ScanInput{TableName: "T", ProjectionExpression: "val"})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if out.Count != 4 {
		t.Fatalf("Count = %d, want 4", out.Count)
	}
	for i, item := range out.Items {
		if len(item) != 1 || item["val"].Str() != fmt.Sprintf("v%d", i) {
			t.Errorf("item[%d] = %v, want only {val}", i, item)
		}
	}

	// Gate broadening: names-only and values-only requests now hit the
	// unused-substitution check (previously values-only escaped it).
	if _, err := c.Scan(ctx, ScanInput{TableName: "T", ExpressionAttributeNames: map[string]string{"#x": "val"}}); !errors.Is(err, ErrValidation) {
		t.Errorf("names-only: err = %v, want ErrValidation", err)
	}
	if _, err := c.Scan(ctx, ScanInput{TableName: "T", ExpressionAttributeValues: map[string]attrval.Value{":x": attrval.NewString("y")}}); !errors.Is(err, ErrValidation) {
		t.Errorf("values-only: err = %v, want ErrValidation", err)
	}
}

func TestQueryGsiProjectionRestriction(t *testing.T) {
	c, ctx := newClient(t), context.Background()
	// pk HASH S; GSI gkeys KEYS_ONLY on gsi_pk; GSI gincl INCLUDE [proj1] on gsi_pk.
	if _, err := c.CreateTable(ctx, CreateTableInput{
		TableName: "T",
		KeySchema: []KeySchemaElement{{AttributeName: "pk", KeyType: "HASH"}},
		AttributeDefinitions: []AttributeDefinition{
			{AttributeName: "pk", AttributeType: "S"},
			{AttributeName: "gsi_pk", AttributeType: "S"},
		},
		GlobalSecondaryIndexes: []GlobalSecondaryIndex{
			{
				IndexName:  "gkeys",
				KeySchema:  []KeySchemaElement{{AttributeName: "gsi_pk", KeyType: "HASH"}},
				Projection: Projection{Type: "KEYS_ONLY"},
			},
			{
				IndexName:  "gincl",
				KeySchema:  []KeySchemaElement{{AttributeName: "gsi_pk", KeyType: "HASH"}},
				Projection: Projection{Type: "INCLUDE", NonKeyAttributes: []string{"proj1"}},
			},
		},
	}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	if _, err := c.PutItem(ctx, PutItemInput{TableName: "T", Item: Item{
		"pk": attrval.NewString("A"), "gsi_pk": attrval.NewString("G1"),
		"proj1": attrval.NewString("p"), "extra": attrval.NewString("e"),
	}}); err != nil {
		t.Fatalf("PutItem: %v", err)
	}
	q := func(index, proj string) (QueryOutput, error) {
		return c.Query(ctx, QueryInput{
			TableName:              "T",
			IndexName:              index,
			KeyConditionExpression: "gsi_pk = :g",
			ExpressionAttributeValues: map[string]attrval.Value{
				":g": attrval.NewString("G1"),
			},
			ProjectionExpression: proj,
		})
	}

	// KEYS_ONLY: non-projected attr rejected; key attrs accepted.
	if _, err := q("gkeys", "extra"); !errors.Is(err, ErrValidation) {
		t.Errorf("gkeys project extra: err = %v, want ErrValidation", err)
	}
	out, err := q("gkeys", "pk, gsi_pk")
	if err != nil {
		t.Fatalf("gkeys project keys: %v", err)
	}
	if len(out.Items) != 1 || len(out.Items[0]) != 2 {
		t.Errorf("gkeys keys: Items = %v, want one {pk, gsi_pk} item", out.Items)
	}

	// INCLUDE: included attr accepted; non-included rejected.
	if _, err := q("gincl", "proj1"); err != nil {
		t.Errorf("gincl project proj1: %v", err)
	}
	if _, err := q("gincl", "extra"); !errors.Is(err, ErrValidation) {
		t.Errorf("gincl project extra: err = %v, want ErrValidation", err)
	}

	// ALL-projected GSI (no restriction) is covered by the adapter
	// conformance suite; here verify base-table Query is unrestricted.
	if _, err := c.Query(ctx, QueryInput{
		TableName:              "T",
		KeyConditionExpression: "pk = :pk",
		ExpressionAttributeValues: map[string]attrval.Value{
			":pk": attrval.NewString("A"),
		},
		ProjectionExpression: "extra",
	}); err != nil {
		t.Errorf("base table project extra: %v", err)
	}

	// GSI Scan applies the same restriction (spec §4.4).
	if _, err := c.Scan(ctx, ScanInput{TableName: "T", IndexName: "gkeys", ProjectionExpression: "extra"}); !errors.Is(err, ErrValidation) {
		t.Errorf("gkeys Scan project extra: err = %v, want ErrValidation", err)
	}

	// ALL_PROJECTED_ATTRIBUTES + projection on a GSI -> ErrValidation.
	if _, err := c.Query(ctx, QueryInput{
		TableName:              "T",
		IndexName:              "gincl",
		KeyConditionExpression: "gsi_pk = :g",
		ExpressionAttributeValues: map[string]attrval.Value{
			":g": attrval.NewString("G1"),
		},
		Select:               "ALL_PROJECTED_ATTRIBUTES",
		ProjectionExpression: "proj1",
	}); !errors.Is(err, ErrValidation) {
		t.Errorf("ALL_PROJECTED_ATTRIBUTES+proj: err = %v, want ErrValidation", err)
	}
}
