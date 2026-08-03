package ddb

import (
	"context"
	"errors"
	"testing"

	"github.com/quells-bot/ddb-sqlite-core/attrval"
	"github.com/quells-bot/ddb-sqlite-core/internal/storage"
)

func newGsiClient(t *testing.T) *Client {
	t.Helper()
	c := newClient(t)
	ctx := context.Background()
	_, err := c.CreateTable(ctx, CreateTableInput{
		TableName: "Tbl",
		KeySchema: []KeySchemaElement{
			{AttributeName: "pk", KeyType: "HASH"},
			{AttributeName: "sk", KeyType: "RANGE"},
		},
		AttributeDefinitions: []AttributeDefinition{
			{AttributeName: "pk", AttributeType: "S"},
			{AttributeName: "sk", AttributeType: "S"},
			{AttributeName: "gsi_pk", AttributeType: "S"},
			{AttributeName: "gsi_sk", AttributeType: "S"},
		},
		GlobalSecondaryIndexes: []GlobalSecondaryIndex{
			{
				IndexName: "gsi1",
				KeySchema: []KeySchemaElement{
					{AttributeName: "gsi_pk", KeyType: "HASH"},
					{AttributeName: "gsi_sk", KeyType: "RANGE"},
				},
				Projection: Projection{Type: "ALL"},
			},
		},
	})
	if err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	return c
}

func TestPutItemGSIKeyTypeMismatch(t *testing.T) {
	c := newGsiClient(t)
	ctx := context.Background()
	// gsi_pk declared S; writing a Number -> ErrValidation, atomic (no data row).
	_, err := c.PutItem(ctx, PutItemInput{
		TableName: "Tbl",
		Item: Item{
			"pk":     attrval.NewString("A"),
			"sk":     attrval.NewString("a"),
			"gsi_pk": attrval.NewNumber(mustNum("123")),
			"gsi_sk": attrval.NewString("s1"),
		},
	})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("err = %v, want ErrValidation", err)
	}
	// Atomic: GetItem finds nothing.
	got, _ := c.GetItem(ctx, GetItemInput{TableName: "Tbl", Key: Item{
		"pk": attrval.NewString("A"), "sk": attrval.NewString("a"),
	}})
	if len(got.Item) != 0 {
		t.Errorf("after rejected put, GetItem = %v, want empty (atomic)", got.Item)
	}
}

func TestPutItemGSINonScalarKey(t *testing.T) {
	c := newGsiClient(t)
	ctx := context.Background()
	_, err := c.PutItem(ctx, PutItemInput{
		TableName: "Tbl",
		Item: Item{
			"pk":     attrval.NewString("A"),
			"sk":     attrval.NewString("a"),
			"gsi_pk": attrval.NewList([]attrval.Value{attrval.NewString("x")}),
			"gsi_sk": attrval.NewString("s1"),
		},
	})
	if !errors.Is(err, ErrValidation) {
		t.Errorf("non-scalar gsi key: err = %v, want ErrValidation", err)
	}
}

func TestPutItemGSIEmptyStringKey(t *testing.T) {
	c := newGsiClient(t)
	ctx := context.Background()
	_, err := c.PutItem(ctx, PutItemInput{
		TableName: "Tbl",
		Item: Item{
			"pk":     attrval.NewString("A"),
			"sk":     attrval.NewString("a"),
			"gsi_pk": attrval.NewString(""),
			"gsi_sk": attrval.NewString("s1"),
		},
	})
	if !errors.Is(err, ErrValidation) {
		t.Errorf("empty gsi key: err = %v, want ErrValidation", err)
	}
}

func TestPutItemCompositeGSISortAbsentAccepted(t *testing.T) {
	// Item with gsi_pk but no gsi_sk: write accepted, item absent from GSI.
	c := newGsiClient(t)
	ctx := context.Background()
	_, err := c.PutItem(ctx, PutItemInput{
		TableName: "Tbl",
		Item: Item{
			"pk":     attrval.NewString("A"),
			"sk":     attrval.NewString("a"),
			"gsi_pk": attrval.NewString("G1"),
			// no gsi_sk
		},
	})
	if err != nil {
		t.Fatalf("PutItem with absent gsi_sk: %v (should be accepted)", err)
	}
}

func TestUpdateItemChangesGSIKey(t *testing.T) {
	c := newGsiClient(t)
	ctx := context.Background()
	c.PutItem(ctx, PutItemInput{TableName: "Tbl", Item: Item{
		"pk": attrval.NewString("A"), "sk": attrval.NewString("a"),
		"gsi_pk": attrval.NewString("G1"), "gsi_sk": attrval.NewString("s1"),
	}})
	if _, err := c.UpdateItem(ctx, UpdateItemInput{
		TableName:        "Tbl",
		Key:              Item{"pk": attrval.NewString("A"), "sk": attrval.NewString("a")},
		UpdateExpression: "SET gsi_pk = :v",
		ExpressionAttributeValues: map[string]attrval.Value{
			":v": attrval.NewString("G2"),
		},
	}); err != nil {
		t.Fatalf("UpdateItem change gsi key: %v", err)
	}
}

func TestUpdateItemGSIKeyTypeMismatch(t *testing.T) {
	c := newGsiClient(t)
	ctx := context.Background()
	c.PutItem(ctx, PutItemInput{TableName: "Tbl", Item: Item{
		"pk": attrval.NewString("A"), "sk": attrval.NewString("a"),
		"gsi_pk": attrval.NewString("G1"), "gsi_sk": attrval.NewString("s1"),
	}})
	_, err := c.UpdateItem(ctx, UpdateItemInput{
		TableName:        "Tbl",
		Key:              Item{"pk": attrval.NewString("A"), "sk": attrval.NewString("a")},
		UpdateExpression: "SET gsi_pk = :v",
		ExpressionAttributeValues: map[string]attrval.Value{
			":v": attrval.NewNumber(mustNum("9")),
		},
	})
	if !errors.Is(err, ErrValidation) {
		t.Errorf("update gsi key type mismatch: err = %v, want ErrValidation", err)
	}
}

func TestGSIQueryBasic(t *testing.T) {
	c := newGsiClient(t)
	ctx := context.Background()
	put := func(pk, sk, gpk, gsk string) {
		c.PutItem(ctx, PutItemInput{TableName: "Tbl", Item: Item{
			"pk": attrval.NewString(pk), "sk": attrval.NewString(sk),
			"gsi_pk": attrval.NewString(gpk), "gsi_sk": attrval.NewString(gsk),
		}})
	}
	put("A", "a", "G1", "s1")
	put("C", "c", "G1", "s1") // tied
	put("B", "b", "G1", "s2")

	out, err := c.Query(ctx, QueryInput{
		TableName:                 "Tbl",
		IndexName:                 "gsi1",
		KeyConditionExpression:    "gsi_pk = :v",
		ExpressionAttributeValues: map[string]attrval.Value{":v": attrval.NewString("G1")},
		ScanIndexForward:          true, // ASC order (s1, s1, s2)
	})
	if err != nil {
		t.Fatalf("GSI Query: %v", err)
	}
	// Order: (s1, A), (s1, C), (s2, B).
	if len(out.Items) != 3 {
		t.Fatalf("got %d items, want 3", len(out.Items))
	}
	pks := []string{out.Items[0]["pk"].Str(), out.Items[1]["pk"].Str(), out.Items[2]["pk"].Str()}
	want := []string{"A", "C", "B"}
	for i, w := range want {
		if pks[i] != w {
			t.Errorf("item[%d].pk = %q, want %q", i, pks[i], w)
		}
	}
	if out.ScannedCount != 3 || out.Count != 3 {
		t.Errorf("Scanned=%d Count=%d, want 3/3", out.ScannedCount, out.Count)
	}
}

func TestGSIQuerySparse(t *testing.T) {
	c := newGsiClient(t)
	ctx := context.Background()
	c.PutItem(ctx, PutItemInput{TableName: "Tbl", Item: Item{
		"pk": attrval.NewString("D"), "sk": attrval.NewString("d"),
		// no gsi_pk -> sparse
	}})
	c.PutItem(ctx, PutItemInput{TableName: "Tbl", Item: Item{
		"pk": attrval.NewString("E"), "sk": attrval.NewString("e"),
		"gsi_pk": attrval.NewString("G2"), "gsi_sk": attrval.NewString("s3"),
	}})
	out, err := c.Query(ctx, QueryInput{
		TableName:                 "Tbl",
		IndexName:                 "gsi1",
		KeyConditionExpression:    "gsi_pk = :v",
		ExpressionAttributeValues: map[string]attrval.Value{":v": attrval.NewString("G2")},
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(out.Items) != 1 || out.Items[0]["pk"].Str() != "E" {
		t.Errorf("sparse query = %v, want [E]", out.Items)
	}
}

func TestGSIQueryConsistentReadRejected(t *testing.T) {
	c := newGsiClient(t)
	ctx := context.Background()
	_, err := c.Query(ctx, QueryInput{
		TableName:                 "Tbl",
		IndexName:                 "gsi1",
		KeyConditionExpression:    "gsi_pk = :v",
		ExpressionAttributeValues: map[string]attrval.Value{":v": attrval.NewString("G1")},
		ConsistentRead:            true,
	})
	if !errors.Is(err, ErrValidation) {
		t.Errorf("err = %v, want ErrValidation", err)
	}
}

func TestGSIQueryNonGsiAttrInKeyCond(t *testing.T) {
	c := newGsiClient(t)
	ctx := context.Background()
	_, err := c.Query(ctx, QueryInput{
		TableName:                 "Tbl",
		IndexName:                 "gsi1",
		KeyConditionExpression:    "pk = :v",
		ExpressionAttributeValues: map[string]attrval.Value{":v": attrval.NewString("A")},
	})
	if !errors.Is(err, ErrValidation) {
		t.Errorf("err = %v, want ErrValidation", err)
	}
}

func TestGSIQueryIndexNotFound(t *testing.T) {
	c := newGsiClient(t)
	ctx := context.Background()
	_, err := c.Query(ctx, QueryInput{
		TableName:                 "Tbl",
		IndexName:                 "nope",
		KeyConditionExpression:    "gsi_pk = :v",
		ExpressionAttributeValues: map[string]attrval.Value{":v": attrval.NewString("G1")},
	})
	if !errors.Is(err, ErrGsiNotFound) {
		t.Errorf("err = %v, want ErrGsiNotFound", err)
	}
}

func newGsiProjectionClient(t *testing.T) *Client {
	t.Helper()
	c := newClient(t)
	ctx := context.Background()
	_, err := c.CreateTable(ctx, CreateTableInput{
		TableName: "Pbl",
		KeySchema: []KeySchemaElement{
			{AttributeName: "pk", KeyType: "HASH"},
			{AttributeName: "sk", KeyType: "RANGE"},
		},
		AttributeDefinitions: []AttributeDefinition{
			{AttributeName: "pk", AttributeType: "S"},
			{AttributeName: "sk", AttributeType: "S"},
			{AttributeName: "gpk", AttributeType: "S"},
			{AttributeName: "gsk", AttributeType: "S"},
		},
		GlobalSecondaryIndexes: []GlobalSecondaryIndex{
			{
				IndexName: "keys", KeySchema: []KeySchemaElement{{AttributeName: "gpk", KeyType: "HASH"}},
				Projection: Projection{Type: "KEYS_ONLY"},
			},
			{
				IndexName: "incl", KeySchema: []KeySchemaElement{
					{AttributeName: "gpk", KeyType: "HASH"},
					{AttributeName: "gsk", KeyType: "RANGE"},
				},
				Projection: Projection{Type: "INCLUDE", NonKeyAttributes: []string{"proj1", "proj2"}},
			},
			{
				IndexName: "allg", KeySchema: []KeySchemaElement{
					{AttributeName: "gpk", KeyType: "HASH"},
					{AttributeName: "gsk", KeyType: "RANGE"},
				},
				Projection: Projection{Type: "ALL"},
			},
		},
	})
	if err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	return c
}

func TestGSIProjectionKeysOnly(t *testing.T) {
	c := newGsiProjectionClient(t)
	ctx := context.Background()
	c.PutItem(ctx, PutItemInput{TableName: "Pbl", Item: Item{
		"pk": attrval.NewString("A"), "sk": attrval.NewString("a"),
		"gpk": attrval.NewString("G1"), "proj1": attrval.NewString("foo"), "extra": attrval.NewString("baz"),
	}})
	out, err := c.Query(ctx, QueryInput{
		TableName: "Pbl", IndexName: "keys",
		KeyConditionExpression:    "gpk = :v",
		ExpressionAttributeValues: map[string]attrval.Value{":v": attrval.NewString("G1")},
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(out.Items) != 1 {
		t.Fatalf("got %d, want 1", len(out.Items))
	}
	got := sortedKeys(out.Items[0])
	want := []string{"gpk", "pk", "sk"} // GSI key + table keys only
	if !equalSlices(got, want) {
		t.Errorf("KEYS_ONLY attrs = %v, want %v", got, want)
	}
}

func TestGSIProjectionInclude(t *testing.T) {
	c := newGsiProjectionClient(t)
	ctx := context.Background()
	c.PutItem(ctx, PutItemInput{TableName: "Pbl", Item: Item{
		"pk": attrval.NewString("A"), "sk": attrval.NewString("a"),
		"gpk": attrval.NewString("G1"), "gsk": attrval.NewString("s1"),
		"proj1": attrval.NewString("foo"), "proj2": attrval.NewString("bar"), "extra": attrval.NewString("baz"),
	}})
	out, err := c.Query(ctx, QueryInput{
		TableName: "Pbl", IndexName: "incl",
		KeyConditionExpression: "gpk = :v AND gsk = :s",
		ExpressionAttributeValues: map[string]attrval.Value{
			":v": attrval.NewString("G1"), ":s": attrval.NewString("s1"),
		},
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(out.Items) != 1 {
		t.Fatalf("got %d, want 1", len(out.Items))
	}
	got := sortedKeys(out.Items[0])
	want := []string{"gpk", "gsk", "pk", "proj1", "proj2", "sk"}
	if !equalSlices(got, want) {
		t.Errorf("INCLUDE attrs = %v, want %v", got, want)
	}
}

func TestGSIProjectionIncludeAbsentAttrsOmitted(t *testing.T) {
	c := newGsiProjectionClient(t)
	ctx := context.Background()
	c.PutItem(ctx, PutItemInput{TableName: "Pbl", Item: Item{
		"pk": attrval.NewString("C"), "sk": attrval.NewString("c"),
		"gpk": attrval.NewString("G1"), "gsk": attrval.NewString("s1"),
		// no proj1/proj2
	}})
	out, _ := c.Query(ctx, QueryInput{
		TableName: "Pbl", IndexName: "incl",
		KeyConditionExpression: "gpk = :v AND gsk = :s",
		ExpressionAttributeValues: map[string]attrval.Value{
			":v": attrval.NewString("G1"), ":s": attrval.NewString("s1"),
		},
	})
	if len(out.Items) != 1 {
		t.Fatalf("got %d, want 1", len(out.Items))
	}
	got := sortedKeys(out.Items[0])
	want := []string{"gpk", "gsk", "pk", "sk"} // projected absent -> omitted
	if !equalSlices(got, want) {
		t.Errorf("INCLUDE absent attrs = %v, want %v", got, want)
	}
}

// helpers
func sortedKeys(m map[string]attrval.Value) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// insertion sort
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}
func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestGSIQueryPagination(t *testing.T) {
	c := newGsiClient(t)
	ctx := context.Background()
	for i, gsk := range []string{"s1", "s2", "s3"} {
		c.PutItem(ctx, PutItemInput{TableName: "Tbl", Item: Item{
			"pk":     attrval.NewString(string(rune('A' + i))),
			"sk":     attrval.NewString(string(rune('a' + i))),
			"gsi_pk": attrval.NewString("G1"),
			"gsi_sk": attrval.NewString(gsk),
		}})
	}

	var collected []string
	var lek Item
	for {
		out, err := c.Query(ctx, QueryInput{
			TableName:                 "Tbl",
			IndexName:                 "gsi1",
			KeyConditionExpression:    "gsi_pk = :v",
			ExpressionAttributeValues: map[string]attrval.Value{":v": attrval.NewString("G1")},
			Limit:                     2,
			ExclusiveStartKey:         lek,
		})
		if err != nil {
			t.Fatalf("Query page: %v", err)
		}
		for _, it := range out.Items {
			collected = append(collected, it["pk"].Str())
		}
		if len(out.LastEvaluatedKey) == 0 {
			break
		}
		lek = out.LastEvaluatedKey
	}
	if len(collected) != 3 {
		t.Errorf("collected %d, want 3", len(collected))
	}
}

func TestGSIQueryPaginationTrailingEmpty(t *testing.T) {
	c := newGsiClient(t)
	ctx := context.Background()
	for i, gsk := range []string{"s1", "s2"} {
		c.PutItem(ctx, PutItemInput{TableName: "Tbl", Item: Item{
			"pk":     attrval.NewString(string(rune('A' + i))),
			"sk":     attrval.NewString(string(rune('a' + i))),
			"gsi_pk": attrval.NewString("G1"),
			"gsi_sk": attrval.NewString(gsk),
		}})
	}
	// Limit == available (2): first page returns 2 with LEK set; resume returns empty with LEK nil.
	out, err := c.Query(ctx, QueryInput{
		TableName:                 "Tbl",
		IndexName:                 "gsi1",
		KeyConditionExpression:    "gsi_pk = :v",
		ExpressionAttributeValues: map[string]attrval.Value{":v": attrval.NewString("G1")},
		Limit:                     2,
	})
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if len(out.Items) != 2 || len(out.LastEvaluatedKey) == 0 {
		t.Fatalf("page 1: items=%d lek=%v, want 2 + non-nil LEK", len(out.Items), out.LastEvaluatedKey)
	}
	out2, err := c.Query(ctx, QueryInput{
		TableName:                 "Tbl",
		IndexName:                 "gsi1",
		KeyConditionExpression:    "gsi_pk = :v",
		ExpressionAttributeValues: map[string]attrval.Value{":v": attrval.NewString("G1")},
		Limit:                     2,
		ExclusiveStartKey:         out.LastEvaluatedKey,
	})
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	if len(out2.Items) != 0 || out2.ScannedCount != 0 || len(out2.LastEvaluatedKey) != 0 {
		t.Errorf("trailing empty: items=%d scanned=%d lek=%v, want 0/0/nil", len(out2.Items), out2.ScannedCount, out2.LastEvaluatedKey)
	}
}

func TestGSIQuerySortConditions(t *testing.T) {
	c := newGsiClient(t)
	ctx := context.Background()
	for _, it := range []struct{ pk, sk, gsk string }{
		{"A", "a", "s1"}, {"B", "b", "s2"}, {"C", "c", "s3"},
	} {
		c.PutItem(ctx, PutItemInput{TableName: "Tbl", Item: Item{
			"pk": attrval.NewString(it.pk), "sk": attrval.NewString(it.sk),
			"gsi_pk": attrval.NewString("G1"), "gsi_sk": attrval.NewString(it.gsk),
		}})
	}
	cases := []struct {
		name    string
		keyCond string
		vals    map[string]attrval.Value
		want    []string
	}{
		{"eq", "gsi_pk = :v AND gsi_sk = :s", map[string]attrval.Value{
			":v": attrval.NewString("G1"), ":s": attrval.NewString("s2"),
		}, []string{"B"}},
		{"lt", "gsi_pk = :v AND gsi_sk < :s", map[string]attrval.Value{
			":v": attrval.NewString("G1"), ":s": attrval.NewString("s2"),
		}, []string{"A"}},
		{"ge", "gsi_pk = :v AND gsi_sk >= :s", map[string]attrval.Value{
			":v": attrval.NewString("G1"), ":s": attrval.NewString("s2"),
		}, []string{"B", "C"}},
		{"between", "gsi_pk = :v AND gsi_sk BETWEEN :lo AND :hi", map[string]attrval.Value{
			":v": attrval.NewString("G1"), ":lo": attrval.NewString("s1"), ":hi": attrval.NewString("s2"),
		}, []string{"A", "B"}},
		{"begins_with", "gsi_pk = :v AND begins_with(gsi_sk, :p)", map[string]attrval.Value{
			":v": attrval.NewString("G1"), ":p": attrval.NewString("s"),
		}, []string{"A", "B", "C"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := c.Query(ctx, QueryInput{
				TableName:                 "Tbl",
				IndexName:                 "gsi1",
				KeyConditionExpression:    tc.keyCond,
				ExpressionAttributeValues: tc.vals,
				ScanIndexForward:          true, // ASC order for multi-row cases
			})
			if err != nil {
				t.Fatalf("Query: %v", err)
			}
			got := make([]string, len(out.Items))
			for i, it := range out.Items {
				got[i] = it["pk"].Str()
			}
			if !equalSlices(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestGSIQueryScanIndexForwardFalse(t *testing.T) {
	c := newGsiClient(t)
	ctx := context.Background()
	for _, it := range []struct{ pk, gsk string }{
		{"A", "s1"}, {"B", "s2"}, {"C", "s3"},
	} {
		c.PutItem(ctx, PutItemInput{TableName: "Tbl", Item: Item{
			"pk": attrval.NewString(it.pk), "sk": attrval.NewString("x"),
			"gsi_pk": attrval.NewString("G1"), "gsi_sk": attrval.NewString(it.gsk),
		}})
	}
	out, err := c.Query(ctx, QueryInput{
		TableName:                 "Tbl",
		IndexName:                 "gsi1",
		KeyConditionExpression:    "gsi_pk = :v",
		ExpressionAttributeValues: map[string]attrval.Value{":v": attrval.NewString("G1")},
		ScanIndexForward:          false,
	})
	if err != nil {
		t.Fatalf("Query desc: %v", err)
	}
	got := []string{out.Items[0]["pk"].Str(), out.Items[1]["pk"].Str(), out.Items[2]["pk"].Str()}
	want := []string{"C", "B", "A"}
	if !equalSlices(got, want) {
		t.Errorf("desc got %v, want %v", got, want)
	}
}

func TestGSIScan(t *testing.T) {
	c := newGsiClient(t)
	ctx := context.Background()
	c.PutItem(ctx, PutItemInput{TableName: "Tbl", Item: Item{
		"pk": attrval.NewString("A"), "sk": attrval.NewString("a"),
		"gsi_pk": attrval.NewString("G1"), "gsi_sk": attrval.NewString("s1"),
	}})
	c.PutItem(ctx, PutItemInput{TableName: "Tbl", Item: Item{
		"pk": attrval.NewString("D"), "sk": attrval.NewString("d"),
		// sparse
	}})
	out, err := c.Scan(ctx, ScanInput{TableName: "Tbl", IndexName: "gsi1"})
	if err != nil {
		t.Fatalf("GSI Scan: %v", err)
	}
	if len(out.Items) != 1 || out.Items[0]["pk"].Str() != "A" {
		t.Errorf("GSI scan = %v, want [A]", out.Items)
	}
	if len(out.LastEvaluatedKey) != 0 {
		t.Errorf("LEK = %v, want nil (exhausted)", out.LastEvaluatedKey)
	}
}

func TestGSIScanPagination(t *testing.T) {
	c := newGsiClient(t)
	ctx := context.Background()
	for i := range 3 {
		c.PutItem(ctx, PutItemInput{TableName: "Tbl", Item: Item{
			"pk":     attrval.NewString(string(rune('A' + i))),
			"sk":     attrval.NewString(string(rune('a' + i))),
			"gsi_pk": attrval.NewString("G" + string(rune('1'+i))),
			"gsi_sk": attrval.NewString("s"),
		}})
	}
	var collected []string
	var lek Item
	for {
		out, err := c.Scan(ctx, ScanInput{
			TableName: "Tbl", IndexName: "gsi1", Limit: 2, ExclusiveStartKey: lek,
		})
		if err != nil {
			t.Fatalf("Scan page: %v", err)
		}
		for _, it := range out.Items {
			collected = append(collected, it["pk"].Str())
		}
		if len(out.LastEvaluatedKey) == 0 {
			break
		}
		lek = out.LastEvaluatedKey
	}
	if len(collected) != 3 {
		t.Errorf("collected %d, want 3", len(collected))
	}
}

func TestGSILekComposition(t *testing.T) {
	c := newGsiClient(t)
	ctx := context.Background()
	c.PutItem(ctx, PutItemInput{TableName: "Tbl", Item: Item{
		"pk": attrval.NewString("A"), "sk": attrval.NewString("a"),
		"gsi_pk": attrval.NewString("G1"), "gsi_sk": attrval.NewString("s1"),
	}})
	c.PutItem(ctx, PutItemInput{TableName: "Tbl", Item: Item{
		"pk": attrval.NewString("B"), "sk": attrval.NewString("b"),
		"gsi_pk": attrval.NewString("G1"), "gsi_sk": attrval.NewString("s2"),
	}})
	out, _ := c.Query(ctx, QueryInput{
		TableName:                 "Tbl",
		IndexName:                 "gsi1",
		KeyConditionExpression:    "gsi_pk = :v",
		ExpressionAttributeValues: map[string]attrval.Value{":v": attrval.NewString("G1")},
		Limit:                     1,
	})
	if len(out.LastEvaluatedKey) != 4 {
		t.Errorf("LEK has %d keys, want 4 (gsi_pk, gsi_sk, pk, sk)", len(out.LastEvaluatedKey))
	}
	for _, k := range []string{"gsi_pk", "gsi_sk", "pk", "sk"} {
		if _, ok := out.LastEvaluatedKey[k]; !ok {
			t.Errorf("LEK missing %q", k)
		}
	}
}

func TestGSIQueryEskShapeValidation(t *testing.T) {
	c := newGsiClient(t)
	ctx := context.Background()
	c.PutItem(ctx, PutItemInput{TableName: "Tbl", Item: Item{
		"pk": attrval.NewString("A"), "sk": attrval.NewString("a"),
		"gsi_pk": attrval.NewString("G1"), "gsi_sk": attrval.NewString("s1"),
	}})
	c.PutItem(ctx, PutItemInput{TableName: "Tbl", Item: Item{
		"pk": attrval.NewString("B"), "sk": attrval.NewString("b"),
		"gsi_pk": attrval.NewString("G1"), "gsi_sk": attrval.NewString("s2"),
	}})
	// Table-keys-only ESK -> rejected (needs GSI keys too).
	_, err := c.Query(ctx, QueryInput{
		TableName:                 "Tbl",
		IndexName:                 "gsi1",
		KeyConditionExpression:    "gsi_pk = :v",
		ExpressionAttributeValues: map[string]attrval.Value{":v": attrval.NewString("G1")},
		ExclusiveStartKey:         Item{"pk": attrval.NewString("A"), "sk": attrval.NewString("a")},
	})
	if !errors.Is(err, ErrValidation) {
		t.Errorf("table-only ESK: err = %v, want ErrValidation", err)
	}
	// GSI-keys-only ESK -> rejected (needs table keys too).
	_, err = c.Query(ctx, QueryInput{
		TableName:                 "Tbl",
		IndexName:                 "gsi1",
		KeyConditionExpression:    "gsi_pk = :v",
		ExpressionAttributeValues: map[string]attrval.Value{":v": attrval.NewString("G1")},
		ExclusiveStartKey:         Item{"gsi_pk": attrval.NewString("G1"), "gsi_sk": attrval.NewString("s1")},
	})
	if !errors.Is(err, ErrValidation) {
		t.Errorf("gsi-only ESK: err = %v, want ErrValidation", err)
	}
	// Union+extra -> rejected.
	_, err = c.Query(ctx, QueryInput{
		TableName:                 "Tbl",
		IndexName:                 "gsi1",
		KeyConditionExpression:    "gsi_pk = :v",
		ExpressionAttributeValues: map[string]attrval.Value{":v": attrval.NewString("G1")},
		ExclusiveStartKey: Item{
			"pk": attrval.NewString("A"), "sk": attrval.NewString("a"),
			"gsi_pk": attrval.NewString("G1"), "gsi_sk": attrval.NewString("s1"),
			"extra": attrval.NewString("x"),
		},
	})
	if !errors.Is(err, ErrValidation) {
		t.Errorf("union+extra ESK: err = %v, want ErrValidation", err)
	}
}

func TestDescribeTableGsiIndexStatus(t *testing.T) {
	c := newGsiClient(t) // table "Tbl" with GSI "gsi1"
	ctx := context.Background()
	desc, err := c.DescribeTable(ctx, DescribeTableInput{TableName: "Tbl"})
	if err != nil {
		t.Fatalf("DescribeTable: %v", err)
	}
	if len(desc.GlobalSecondaryIndexes) != 1 {
		t.Fatalf("got %d GSIs, want 1", len(desc.GlobalSecondaryIndexes))
	}
	if got := desc.GlobalSecondaryIndexes[0].IndexStatus; got != "ACTIVE" {
		t.Errorf("IndexStatus = %q, want ACTIVE", got)
	}
}

func TestGsiIndexKey(t *testing.T) {
	g := storage.GsiDef{Hash: "gh", Range: "gr", HashType: "S", RangeType: "S"}

	cases := []struct {
		name      string
		item      Item
		indexable bool
	}{
		{"both present", Item{"gh": attrval.NewString("H"), "gr": attrval.NewString("R")}, true},
		{"hash absent", Item{"gr": attrval.NewString("R")}, false},
		{"range absent", Item{"gh": attrval.NewString("H")}, false},
		{"hash wrong type", Item{"gh": attrval.NewNumber(mustNum("1")), "gr": attrval.NewString("R")}, false},
		{"hash non-scalar", Item{"gh": attrval.NewList([]attrval.Value{attrval.NewString("x")}), "gr": attrval.NewString("R")}, false},
		{"hash empty string", Item{"gh": attrval.NewString(""), "gr": attrval.NewString("R")}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, ok := gsiIndexKey(tc.item, g)
			if ok != tc.indexable {
				t.Errorf("indexable = %v, want %v", ok, tc.indexable)
			}
		})
	}

	// Hash-only GSI: range attribute is irrelevant.
	gh := storage.GsiDef{Hash: "gh", HashType: "N"}
	hv, _, ok := gsiIndexKey(Item{"gh": attrval.NewNumber(mustNum("5"))}, gh)
	if !ok {
		t.Fatal("number hash should be indexable")
	}
	if f, _ := hv.(float64); f != 5 {
		t.Errorf("hashVal = %v, want 5.0", hv)
	}
}
