package ddb

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/quells-bot/ddb-sqlite-core/attrval"
)

func TestBatchWriteItemMultiTable(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	mustCreateTable(t, c, "Abl")
	mustCreateTable(t, c, "Bbl")

	// Seed one row to delete.
	if _, err := c.PutItem(ctx, PutItemInput{TableName: "Abl", Item: Item{"pk": attrval.NewString("del")}}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	out, err := c.BatchWriteItem(ctx, BatchWriteItemInput{RequestItems: map[string][]WriteRequest{
		"Abl": {
			{Put: &PutRequest{Item: Item{"pk": attrval.NewString("a1"), "v": attrval.NewString("one")}}},
			{Delete: &DeleteRequest{Key: Item{"pk": attrval.NewString("del")}}},
		},
		"Bbl": {
			{Put: &PutRequest{Item: Item{"pk": attrval.NewString("b1")}}},
		},
	}})
	if err != nil {
		t.Fatalf("BatchWriteItem: %v", err)
	}
	if len(out.UnprocessedItems) != 0 {
		t.Errorf("UnprocessedItems = %v, want empty", out.UnprocessedItems)
	}

	get := func(table, pk string) Item {
		t.Helper()
		got, err := c.GetItem(ctx, GetItemInput{TableName: table, Key: Item{"pk": attrval.NewString(pk)}})
		if err != nil {
			t.Fatalf("GetItem %s/%s: %v", table, pk, err)
		}
		return got.Item
	}
	if it := get("Abl", "a1"); it["v"].Str() != "one" {
		t.Errorf("A/a1 = %v", it)
	}
	if it := get("Abl", "del"); len(it) != 0 {
		t.Errorf("A/del should be deleted, got %v", it)
	}
	if it := get("Bbl", "b1"); len(it) == 0 {
		t.Error("B/b1 missing")
	}

	// Overwrite via a second batch.
	if _, err := c.BatchWriteItem(ctx, BatchWriteItemInput{RequestItems: map[string][]WriteRequest{
		"Abl": {{Put: &PutRequest{Item: Item{"pk": attrval.NewString("a1"), "v": attrval.NewString("updated")}}}},
	}}); err != nil {
		t.Fatalf("BatchWriteItem overwrite: %v", err)
	}
	if it := get("Abl", "a1"); it["v"].Str() != "updated" {
		t.Errorf("after overwrite A/a1 = %v", it)
	}
}

func TestBatchWriteItemCountLimit(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	mustCreateTable(t, c, "Tbl")

	mk := func(n int) []WriteRequest {
		reqs := make([]WriteRequest, 0, n)
		for i := 0; i < n; i++ {
			reqs = append(reqs, WriteRequest{Put: &PutRequest{Item: Item{"pk": attrval.NewString(fmt.Sprintf("k%02d", i))}}})
		}
		return reqs
	}
	if _, err := c.BatchWriteItem(ctx, BatchWriteItemInput{RequestItems: map[string][]WriteRequest{"Tbl": mk(25)}}); err != nil {
		t.Errorf("25 requests: %v", err)
	}
	_, err := c.BatchWriteItem(ctx, BatchWriteItemInput{RequestItems: map[string][]WriteRequest{"Tbl": mk(26)}})
	if !errors.Is(err, ErrValidation) {
		t.Errorf("26 requests: err = %v, want ErrValidation", err)
	}
}

func TestBatchWriteItemEmptyRequests(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	mustCreateTable(t, c, "Tbl")

	if _, err := c.BatchWriteItem(ctx, BatchWriteItemInput{}); !errors.Is(err, ErrValidation) {
		t.Errorf("nil RequestItems: err = %v, want ErrValidation", err)
	}
	// A table mapping to an empty request slice is rejected (spec §2.4 —
	// keep this test aligned with the Task 1 probe outcome).
	if _, err := c.BatchWriteItem(ctx, BatchWriteItemInput{RequestItems: map[string][]WriteRequest{"Tbl": {}}}); !errors.Is(err, ErrValidation) {
		t.Errorf("empty per-table requests: err = %v, want ErrValidation", err)
	}
}

func TestBatchWriteItemWriteRequestShape(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	mustCreateTable(t, c, "Tbl")

	// Neither Put nor Delete.
	if _, err := c.BatchWriteItem(ctx, BatchWriteItemInput{RequestItems: map[string][]WriteRequest{"Tbl": {{}}}}); !errors.Is(err, ErrValidation) {
		t.Errorf("neither: err = %v, want ErrValidation", err)
	}
	// Both Put and Delete.
	both := WriteRequest{
		Put:    &PutRequest{Item: Item{"pk": attrval.NewString("k")}},
		Delete: &DeleteRequest{Key: Item{"pk": attrval.NewString("k")}},
	}
	if _, err := c.BatchWriteItem(ctx, BatchWriteItemInput{RequestItems: map[string][]WriteRequest{"Tbl": {both}}}); !errors.Is(err, ErrValidation) {
		t.Errorf("both: err = %v, want ErrValidation", err)
	}
}

func TestBatchWriteItemDuplicateKeys(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	mustCreateTable(t, c, "Abl")
	mustCreateTable(t, c, "Bbl")
	// N-key table: numeric identity canonicalizes at parse (1 ≡ 1.0).
	if _, err := c.CreateTable(ctx, CreateTableInput{
		TableName:            "Nbl",
		KeySchema:            []KeySchemaElement{{AttributeName: "pk", KeyType: "HASH"}},
		AttributeDefinitions: []AttributeDefinition{{AttributeName: "pk", AttributeType: "N"}},
	}); err != nil {
		t.Fatalf("CreateTable N: %v", err)
	}

	putS := func(k string) WriteRequest {
		return WriteRequest{Put: &PutRequest{Item: Item{"pk": attrval.NewString(k)}}}
	}
	delS := func(k string) WriteRequest {
		return WriteRequest{Delete: &DeleteRequest{Key: Item{"pk": attrval.NewString(k)}}}
	}

	cases := []struct {
		name    string
		request map[string][]WriteRequest
		wantErr bool
	}{
		{"two puts same key", map[string][]WriteRequest{"Abl": {putS("k"), putS("k")}}, true},
		{"put and delete same key", map[string][]WriteRequest{"Abl": {putS("k"), delS("k")}}, true},
		{"same key in different tables", map[string][]WriteRequest{"Abl": {putS("k")}, "Bbl": {putS("k")}}, false},
		{"numeric key spelling variants", map[string][]WriteRequest{"Nbl": {
			{Put: &PutRequest{Item: Item{"pk": attrval.NewNumber(mustNum("1"))}}},
			{Put: &PutRequest{Item: Item{"pk": attrval.NewNumber(mustNum("1.0"))}}},
		}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.BatchWriteItem(ctx, BatchWriteItemInput{RequestItems: tc.request})
			if tc.wantErr && !errors.Is(err, ErrValidation) {
				t.Errorf("err = %v, want ErrValidation", err)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("err = %v, want nil", err)
			}
		})
	}
}

func TestBatchWriteItemBadKey(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	mustCreateTable(t, c, "Tbl")

	cases := []struct {
		name string
		wr   WriteRequest
	}{
		{"put missing partition key", WriteRequest{Put: &PutRequest{Item: Item{"v": attrval.NewString("x")}}}},
		{"put wrong key type", WriteRequest{Put: &PutRequest{Item: Item{"pk": attrval.NewNumber(mustNum("1"))}}}},
		{"delete key missing pk", WriteRequest{Delete: &DeleteRequest{Key: Item{}}}},
		{"delete key with extra attr", WriteRequest{Delete: &DeleteRequest{Key: Item{"pk": attrval.NewString("k"), "v": attrval.NewString("x")}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A valid request rides along: nothing may be written when the
			// batch is rejected (no partial processing, spec §2.1).
			_, err := c.BatchWriteItem(ctx, BatchWriteItemInput{RequestItems: map[string][]WriteRequest{
				"Tbl": {
					{Put: &PutRequest{Item: Item{"pk": attrval.NewString("good")}}},
					tc.wr,
				},
			}})
			if !errors.Is(err, ErrValidation) {
				t.Errorf("err = %v, want ErrValidation", err)
			}
			got, _ := c.GetItem(ctx, GetItemInput{TableName: "Tbl", Key: Item{"pk": attrval.NewString("good")}})
			if len(got.Item) != 0 {
				t.Errorf("valid request written despite rejected batch: %v", got.Item)
			}
		})
	}
}

func TestBatchWriteItemUnknownTable(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	mustCreateTable(t, c, "Good")

	_, err := c.BatchWriteItem(ctx, BatchWriteItemInput{RequestItems: map[string][]WriteRequest{
		"Good": {{Put: &PutRequest{Item: Item{"pk": attrval.NewString("k")}}}},
		"Nope": {{Put: &PutRequest{Item: Item{"pk": attrval.NewString("k")}}}},
	}})
	if !errors.Is(err, ErrTableNotFound) {
		t.Errorf("err = %v, want ErrTableNotFound", err)
	}
	got, _ := c.GetItem(ctx, GetItemInput{TableName: "Good", Key: Item{"pk": attrval.NewString("k")}})
	if len(got.Item) != 0 {
		t.Errorf("valid table written despite rejected batch: %v", got.Item)
	}
}

func TestBatchWriteItemItemTooLarge(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	mustCreateTable(t, c, "Tbl")

	big := Item{"pk": attrval.NewString("big"), "data": attrval.NewString(strings.Repeat("x", 400*1024+1))}
	_, err := c.BatchWriteItem(ctx, BatchWriteItemInput{RequestItems: map[string][]WriteRequest{
		"Tbl": {
			{Put: &PutRequest{Item: Item{"pk": attrval.NewString("good")}}},
			{Put: &PutRequest{Item: big}},
		},
	}})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("oversized: err = %v, want ErrValidation", err)
	}
	got, _ := c.GetItem(ctx, GetItemInput{TableName: "Tbl", Key: Item{"pk": attrval.NewString("good")}})
	if len(got.Item) != 0 {
		t.Errorf("valid request written despite rejected batch: %v", got.Item)
	}
}

func TestBatchWriteItemSizeBoundary(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	c.CreateTable(ctx, CreateTableInput{TableName: "Tbl", KeySchema: []KeySchemaElement{{"pk", "HASH"}}, AttributeDefinitions: []AttributeDefinition{{"pk", "S"}}})

	// Exactly 409600 bytes: accepted.
	ok := Item{"pk": attrval.NewString("k"), "big": attrval.NewString(strings.Repeat("x", 409594))}
	_, err := c.BatchWriteItem(ctx, BatchWriteItemInput{
		RequestItems: map[string][]WriteRequest{"Tbl": {{Put: &PutRequest{Item: ok}}}},
	})
	if err != nil {
		t.Fatalf("exact-boundary batch put: err = %v", err)
	}

	// 409601 bytes: rejected, and the valid item is not written (all-or-nothing).
	over := Item{"pk": attrval.NewString("big"), "data": attrval.NewString(strings.Repeat("x", 409595))}
	_, err = c.BatchWriteItem(ctx, BatchWriteItemInput{
		RequestItems: map[string][]WriteRequest{"Tbl": {
			{Put: &PutRequest{Item: Item{"pk": attrval.NewString("good")}}},
			{Put: &PutRequest{Item: over}},
		}},
	})
	if !errors.Is(err, ErrValidation) {
		t.Errorf("oversized batch put: err = %v, want ErrValidation", err)
	}
	got, err := c.GetItem(ctx, GetItemInput{TableName: "Tbl", Key: Item{"pk": attrval.NewString("good")}})
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if len(got.Item) != 0 {
		t.Errorf("valid request written despite rejected batch: %v", got.Item)
	}
}

func TestBatchWriteItemGSI(t *testing.T) {
	c := newGsiClient(t) // T: pk/sk S; GSI gsi1 (gsi_pk HASH, gsi_sk RANGE, ALL)
	ctx := context.Background()

	countGsi := func(val string) int32 {
		t.Helper()
		out, err := c.Query(ctx, QueryInput{
			TableName:                 "Tbl",
			IndexName:                 "gsi1",
			KeyConditionExpression:    "gsi_pk = :g",
			ExpressionAttributeValues: map[string]attrval.Value{":g": attrval.NewString(val)},
		})
		if err != nil {
			t.Fatalf("Query gsi1: %v", err)
		}
		return out.Count
	}

	// Batch put: two indexed items + one sparse (no GSI attrs).
	_, err := c.BatchWriteItem(ctx, BatchWriteItemInput{RequestItems: map[string][]WriteRequest{
		"Tbl": {
			{Put: &PutRequest{Item: Item{"pk": attrval.NewString("p1"), "sk": attrval.NewString("s1"), "gsi_pk": attrval.NewString("g"), "gsi_sk": attrval.NewString("a")}}},
			{Put: &PutRequest{Item: Item{"pk": attrval.NewString("p2"), "sk": attrval.NewString("s2"), "gsi_pk": attrval.NewString("g"), "gsi_sk": attrval.NewString("b")}}},
			{Put: &PutRequest{Item: Item{"pk": attrval.NewString("p3"), "sk": attrval.NewString("s3")}}},
		},
	}})
	if err != nil {
		t.Fatalf("BatchWriteItem: %v", err)
	}
	if n := countGsi("g"); n != 2 {
		t.Fatalf("GSI count = %d, want 2 (sparse item must not be indexed)", n)
	}

	// Overwrite moving the GSI partition: old row gone, no phantom hits.
	_, err = c.BatchWriteItem(ctx, BatchWriteItemInput{RequestItems: map[string][]WriteRequest{
		"Tbl": {{Put: &PutRequest{Item: Item{"pk": attrval.NewString("p1"), "sk": attrval.NewString("s1"), "gsi_pk": attrval.NewString("g2"), "gsi_sk": attrval.NewString("a")}}}},
	}})
	if err != nil {
		t.Fatalf("BatchWriteItem overwrite: %v", err)
	}
	if n := countGsi("g"); n != 1 {
		t.Errorf("GSI count after overwrite = %d, want 1 (no phantom row)", n)
	}
	if n := countGsi("g2"); n != 1 {
		t.Errorf("GSI count on new key = %d, want 1", n)
	}

	// Batch delete cascades the GSI row.
	_, err = c.BatchWriteItem(ctx, BatchWriteItemInput{RequestItems: map[string][]WriteRequest{
		"Tbl": {{Delete: &DeleteRequest{Key: Item{"pk": attrval.NewString("p2"), "sk": attrval.NewString("s2")}}}},
	}})
	if err != nil {
		t.Fatalf("BatchWriteItem delete: %v", err)
	}
	if n := countGsi("g"); n != 0 {
		t.Errorf("GSI count after delete = %d, want 0", n)
	}
}

func TestBatchWriteItemGSIKeyInvalid(t *testing.T) {
	c := newGsiClient(t)
	ctx := context.Background()

	// gsi_pk declared S; a Number GSI key rejects the whole batch atomically.
	_, err := c.BatchWriteItem(ctx, BatchWriteItemInput{RequestItems: map[string][]WriteRequest{
		"Tbl": {{Put: &PutRequest{Item: Item{"pk": attrval.NewString("p1"), "sk": attrval.NewString("s1"), "gsi_pk": attrval.NewNumber(mustNum("1"))}}}},
	}})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("err = %v, want ErrValidation", err)
	}
	got, _ := c.GetItem(ctx, GetItemInput{TableName: "Tbl", Key: Item{"pk": attrval.NewString("p1"), "sk": attrval.NewString("s1")}})
	if len(got.Item) != 0 {
		t.Errorf("rejected put visible: %v", got.Item)
	}
}

func TestBatchWriteItemEmptyTableName(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	mustCreateTable(t, c, "Tbl")
	_, err := c.BatchWriteItem(ctx, BatchWriteItemInput{RequestItems: map[string][]WriteRequest{
		"": {{Put: &PutRequest{Item: Item{"pk": attrval.NewString("k")}}}},
	}})
	if !errors.Is(err, ErrValidation) {
		t.Errorf("empty table name: err = %v, want ErrValidation", err)
	}
}

func TestBatchGetItemMultiTable(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	mustCreateTable(t, c, "Abl")
	mustCreateTable(t, c, "Bbl")

	c.PutItem(ctx, PutItemInput{TableName: "Abl", Item: Item{"pk": attrval.NewString("a1"), "v": attrval.NewString("one")}})
	c.PutItem(ctx, PutItemInput{TableName: "Abl", Item: Item{"pk": attrval.NewString("a2"), "n": attrval.NewNumber(mustNum("12.5"))}})
	c.PutItem(ctx, PutItemInput{TableName: "Bbl", Item: Item{"pk": attrval.NewString("b1")}})

	out, err := c.BatchGetItem(ctx, BatchGetItemInput{RequestItems: map[string]KeysAndAttributes{
		"Abl": {Keys: []Item{
			{"pk": attrval.NewString("a2")}, // shuffled order on purpose
			{"pk": attrval.NewString("missing")},
			{"pk": attrval.NewString("a1")},
		}},
		"Bbl": {Keys: []Item{{"pk": attrval.NewString("b1")}}, ConsistentRead: true},
	}})
	if err != nil {
		t.Fatalf("BatchGetItem: %v", err)
	}
	if len(out.UnprocessedKeys) != 0 {
		t.Errorf("UnprocessedKeys = %v, want empty", out.UnprocessedKeys)
	}
	if len(out.Responses) != 2 {
		t.Fatalf("len(Responses) = %d, want 2", len(out.Responses))
	}
	// Items sorted by pk ascending; missing key omitted.
	items := out.Responses["Abl"]
	if len(items) != 2 {
		t.Fatalf("len(Responses[A]) = %d, want 2", len(items))
	}
	if items[0]["v"].Str() != "one" || items[1]["n"].Num().String() != "12.5" {
		t.Errorf("Responses[A] = %v, want [a1 a2] sorted by pk ascending", items)
	}
	if len(out.Responses["Bbl"]) != 1 {
		t.Errorf("len(Responses[B]) = %d, want 1", len(out.Responses["Bbl"]))
	}
}

func TestBatchGetItemCompositeKeys(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	if _, err := c.CreateTable(ctx, CreateTableInput{
		TableName: "Cbl",
		KeySchema: []KeySchemaElement{
			{AttributeName: "pk", KeyType: "HASH"},
			{AttributeName: "sk", KeyType: "RANGE"},
		},
		AttributeDefinitions: []AttributeDefinition{
			{AttributeName: "pk", AttributeType: "S"},
			{AttributeName: "sk", AttributeType: "N"},
		},
	}); err != nil {
		t.Fatalf("CreateTable C: %v", err)
	}
	key := func(sk string) Item {
		return Item{"pk": attrval.NewString("p1"), "sk": attrval.NewNumber(mustNum(sk))}
	}
	c.PutItem(ctx, PutItemInput{TableName: "Cbl", Item: key("1")})
	c.PutItem(ctx, PutItemInput{TableName: "Cbl", Item: key("2")})

	out, err := c.BatchGetItem(ctx, BatchGetItemInput{RequestItems: map[string]KeysAndAttributes{
		"Cbl": {Keys: []Item{key("1"), key("99"), key("2")}},
	}})
	if err != nil {
		t.Fatalf("BatchGetItem: %v", err)
	}
	if len(out.Responses["Cbl"]) != 2 {
		t.Errorf("len(Responses[C]) = %d, want 2 (missing (p1,99) omitted)", len(out.Responses["Cbl"]))
	}
}

func TestBatchGetItemCountLimit(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	mustCreateTable(t, c, "Tbl")

	mk := func(n int) []Item {
		keys := make([]Item, 0, n)
		for i := 0; i < n; i++ {
			keys = append(keys, Item{"pk": attrval.NewString(fmt.Sprintf("k%03d", i))})
		}
		return keys
	}
	if _, err := c.BatchGetItem(ctx, BatchGetItemInput{RequestItems: map[string]KeysAndAttributes{"Tbl": {Keys: mk(100)}}}); err != nil {
		t.Errorf("100 keys: %v", err)
	}
	_, err := c.BatchGetItem(ctx, BatchGetItemInput{RequestItems: map[string]KeysAndAttributes{"Tbl": {Keys: mk(101)}}})
	if !errors.Is(err, ErrValidation) {
		t.Errorf("101 keys: err = %v, want ErrValidation", err)
	}
}

func TestBatchGetItemEmptyRequests(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	mustCreateTable(t, c, "Tbl")

	if _, err := c.BatchGetItem(ctx, BatchGetItemInput{}); !errors.Is(err, ErrValidation) {
		t.Errorf("nil RequestItems: err = %v, want ErrValidation", err)
	}
	// Empty per-table Keys list (spec §2.4 — aligned with Task 1 probe).
	if _, err := c.BatchGetItem(ctx, BatchGetItemInput{RequestItems: map[string]KeysAndAttributes{"Tbl": {}}}); !errors.Is(err, ErrValidation) {
		t.Errorf("empty per-table keys: err = %v, want ErrValidation", err)
	}
}

func TestBatchGetItemDuplicateKeys(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	mustCreateTable(t, c, "Tbl")

	_, err := c.BatchGetItem(ctx, BatchGetItemInput{RequestItems: map[string]KeysAndAttributes{
		"Tbl": {Keys: []Item{{"pk": attrval.NewString("k")}, {"pk": attrval.NewString("k")}}},
	}})
	if !errors.Is(err, ErrValidation) {
		t.Errorf("duplicates: err = %v, want ErrValidation", err)
	}
}

func TestBatchGetItemKeyValidation(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	mustCreateTable(t, c, "Tbl")

	cases := []struct {
		name string
		key  Item
	}{
		{"missing pk", Item{}},
		{"extra attr", Item{"pk": attrval.NewString("k"), "v": attrval.NewString("x")}},
		{"wrong type", Item{"pk": attrval.NewNumber(mustNum("1"))}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.BatchGetItem(ctx, BatchGetItemInput{RequestItems: map[string]KeysAndAttributes{"Tbl": {Keys: []Item{tc.key}}}})
			if !errors.Is(err, ErrValidation) {
				t.Errorf("err = %v, want ErrValidation", err)
			}
		})
	}
}

func TestBatchGetItemUnknownTable(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	mustCreateTable(t, c, "Good")

	_, err := c.BatchGetItem(ctx, BatchGetItemInput{RequestItems: map[string]KeysAndAttributes{
		"Good": {Keys: []Item{{"pk": attrval.NewString("k")}}},
		"Nope": {Keys: []Item{{"pk": attrval.NewString("k")}}},
	}})
	if !errors.Is(err, ErrTableNotFound) {
		t.Errorf("err = %v, want ErrTableNotFound", err)
	}
}

func TestBatchGetItemAllMissTableEmptyEntry(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	mustCreateTable(t, c, "Abl")
	mustCreateTable(t, c, "Bbl")
	c.PutItem(ctx, PutItemInput{TableName: "Abl", Item: Item{"pk": attrval.NewString("a1")}})

	out, err := c.BatchGetItem(ctx, BatchGetItemInput{RequestItems: map[string]KeysAndAttributes{
		"Abl": {Keys: []Item{{"pk": attrval.NewString("a1")}}},
		"Bbl": {Keys: []Item{{"pk": attrval.NewString("ghost")}}},
	}})
	if err != nil {
		t.Fatalf("BatchGetItem: %v", err)
	}
	// All-miss table B is present with an empty slice, matching dynamodb-local.
	if len(out.Responses["Bbl"]) != 0 {
		t.Errorf("len(Responses[B]) = %d, want 0 (empty entry, matching dynamodb-local)", len(out.Responses["Bbl"]))
	}
	if len(out.Responses["Abl"]) != 1 {
		t.Errorf("len(Responses[A]) = %d, want 1", len(out.Responses["Abl"]))
	}

	// Single all-miss table: Responses still contains the empty entry for B.
	out, err = c.BatchGetItem(ctx, BatchGetItemInput{RequestItems: map[string]KeysAndAttributes{
		"Bbl": {Keys: []Item{{"pk": attrval.NewString("ghost")}}},
	}})
	if err != nil {
		t.Fatalf("BatchGetItem: %v", err)
	}
	if len(out.Responses) != 1 {
		t.Errorf("len(Responses) = %d, want 1", len(out.Responses))
	}
	if len(out.Responses["Bbl"]) != 0 {
		t.Errorf("len(Responses[B]) = %d, want 0 (empty entry for single all-miss table)", len(out.Responses["Bbl"]))
	}
}

func TestBatchGetItemAttributesToGetRejected(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	mustCreateTable(t, c, "Tbl")
	key := []Item{{"pk": attrval.NewString("k")}}

	// AttributesToGet is the remaining deliberate divergence — the engine
	// honors ProjectionExpression/ExpressionAttributeNames instead.
	_, err := c.BatchGetItem(ctx, BatchGetItemInput{RequestItems: map[string]KeysAndAttributes{
		"Tbl": {Keys: key, AttributesToGet: []string{"pk"}},
	}})
	if !errors.Is(err, ErrValidation) {
		t.Errorf("err = %v, want ErrValidation", err)
	}
}

func TestBatchGetItemExpiredVisible(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	mustCreateTable(t, c, "Tbl")

	// Enable TTL and write an already-expired item (epoch 1 = 1970).
	if _, err := c.UpdateTimeToLive(ctx, UpdateTimeToLiveInput{
		TableName:               "Tbl",
		TimeToLiveSpecification: TimeToLiveSpecification{Enabled: true, AttributeName: "expire"},
	}); err != nil {
		t.Fatalf("UpdateTimeToLive: %v", err)
	}
	c.PutItem(ctx, PutItemInput{TableName: "Tbl", Item: Item{
		"pk":     attrval.NewString("k"),
		"expire": attrval.NewNumber(mustNum("1")),
	}})

	// Faithful read model: expired items are visible until ExpireExpired runs.
	out, err := c.BatchGetItem(ctx, BatchGetItemInput{RequestItems: map[string]KeysAndAttributes{
		"Tbl": {Keys: []Item{{"pk": attrval.NewString("k")}}},
	}})
	if err != nil {
		t.Fatalf("BatchGetItem: %v", err)
	}
	if len(out.Responses["Tbl"]) != 1 {
		t.Errorf("len(Responses[T]) = %d, want 1 (expired item visible, M5a Faithful model)", len(out.Responses["Tbl"]))
	}
}

func TestBatchGetItemEmptyTableName(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	mustCreateTable(t, c, "Tbl")
	_, err := c.BatchGetItem(ctx, BatchGetItemInput{RequestItems: map[string]KeysAndAttributes{
		"": {Keys: []Item{{"pk": attrval.NewString("k")}}},
	}})
	if !errors.Is(err, ErrValidation) {
		t.Errorf("empty table name: err = %v, want ErrValidation", err)
	}
}

func TestBatchGetItemProjection(t *testing.T) {
	c, ctx := newClient(t), context.Background()
	mustCreateTable(t, c, "Tbl")
	for _, it := range []Item{
		{"pk": attrval.NewString("k1"), "v": attrval.NewString("a"), "w": attrval.NewString("x")},
		{"pk": attrval.NewString("k2"), "v": attrval.NewString("b"), "w": attrval.NewString("y")},
		{"pk": attrval.NewString("k3"), "v": attrval.NewString("c"), "w": attrval.NewString("z")},
	} {
		if _, err := c.PutItem(ctx, PutItemInput{TableName: "Tbl", Item: it}); err != nil {
			t.Fatalf("PutItem: %v", err)
		}
	}
	keys := func(ks ...string) []Item {
		out := make([]Item, 0, len(ks))
		for _, k := range ks {
			out = append(out, Item{"pk": attrval.NewString(k)})
		}
		return out
	}

	// Projection honored; shuffled request order still returns key-ascending
	// order — proof of sort-first-project-after: "v" is projected, keys are
	// not, so only sorting before projection can order by key.
	out, err := c.BatchGetItem(ctx, BatchGetItemInput{RequestItems: map[string]KeysAndAttributes{
		"Tbl": {Keys: keys("k3", "k1", "k2"), ProjectionExpression: "v"},
	}})
	if err != nil {
		t.Fatalf("BatchGetItem: %v", err)
	}
	got := out.Responses["Tbl"]
	if len(got) != 3 {
		t.Fatalf("len(Responses[T]) = %d, want 3", len(got))
	}
	for i, want := range []string{"a", "b", "c"} {
		if len(got[i]) != 1 || got[i]["v"].Str() != want {
			t.Errorf("item[%d] = %v, want {v:%s} (key-ascending order)", i, got[i], want)
		}
	}

	// #name substitution honored.
	out, err = c.BatchGetItem(ctx, BatchGetItemInput{RequestItems: map[string]KeysAndAttributes{
		"Tbl": {Keys: keys("k1"), ProjectionExpression: "#x", ExpressionAttributeNames: map[string]string{"#x": "w"}},
	}})
	if err != nil {
		t.Fatalf("BatchGetItem #name: %v", err)
	}
	if len(out.Responses["Tbl"]) != 1 || out.Responses["Tbl"][0]["w"].Str() != "x" {
		t.Errorf("Responses[T] = %v, want [{w:x}]", out.Responses["Tbl"])
	}

	// Names without a projection are unused -> ErrValidation.
	if _, err := c.BatchGetItem(ctx, BatchGetItemInput{RequestItems: map[string]KeysAndAttributes{
		"Tbl": {Keys: keys("k1"), ExpressionAttributeNames: map[string]string{"#x": "w"}},
	}}); !errors.Is(err, ErrValidation) {
		t.Errorf("names w/o projection: err = %v, want ErrValidation", err)
	}

	// Overlap rejected; AttributesToGet still rejected (deliberate divergence).
	if _, err := c.BatchGetItem(ctx, BatchGetItemInput{RequestItems: map[string]KeysAndAttributes{
		"Tbl": {Keys: keys("k1"), ProjectionExpression: "v, v"},
	}}); !errors.Is(err, ErrValidation) {
		t.Errorf("overlap: err = %v, want ErrValidation", err)
	}
	if _, err := c.BatchGetItem(ctx, BatchGetItemInput{RequestItems: map[string]KeysAndAttributes{
		"Tbl": {Keys: keys("k1"), AttributesToGet: []string{"v"}},
	}}); !errors.Is(err, ErrValidation) {
		t.Errorf("AttributesToGet: err = %v, want ErrValidation", err)
	}
}

// --- M6c W6: BatchGetItem 16MiB response cap ---

// bigItem builds a {"pk","big"} item whose W1 accounting size is exactly
// 8+payloadLen bytes (len("pk")+len(key) for a 3-char key, len("big")+payloadLen).
func bigItem(key string, payloadLen int) Item {
	return Item{
		"pk":  attrval.NewString(key),
		"big": attrval.NewString(strings.Repeat("x", payloadLen)),
	}
}

// seedBigItems puts n items prefix00..prefix{n-1} with the given payload size.
func seedBigItems(t *testing.T, c *Client, table, prefix string, n, payloadLen int) {
	t.Helper()
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("%s%02d", prefix, i)
		if _, err := c.PutItem(context.Background(), PutItemInput{TableName: table, Item: bigItem(key, payloadLen)}); err != nil {
			t.Fatalf("PutItem %s: %v", key, err)
		}
	}
}

// bigKeys builds n request keys prefix00..prefix{n-1}, plus any extras.
func bigKeys(prefix string, n int, extras ...string) []Item {
	keys := make([]Item, 0, n+len(extras))
	for i := 0; i < n; i++ {
		keys = append(keys, Item{"pk": attrval.NewString(fmt.Sprintf("%s%02d", prefix, i))})
	}
	for _, e := range extras {
		keys = append(keys, Item{"pk": attrval.NewString(e)})
	}
	return keys
}

// Exactly at the cap: 100 items of 167,772 bytes = 16,777,200 ≤ 16,777,216 —
// everything returned, nothing spilled.
func TestBatchGetItemResponseCapExactFit(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	mustCreateTable(t, c, "Tbl")
	seedBigItems(t, c, "Tbl", "k", 100, 167764) // per-item 8+167764 = 167772

	out, err := c.BatchGetItem(ctx, BatchGetItemInput{RequestItems: map[string]KeysAndAttributes{
		"Tbl": {Keys: bigKeys("k", 100)},
	}})
	if err != nil {
		t.Fatalf("BatchGetItem: %v", err)
	}
	if got := len(out.Responses["Tbl"]); got != 100 {
		t.Errorf("len(Responses[Tbl]) = %d, want 100", got)
	}
	if len(out.UnprocessedKeys) != 0 {
		t.Errorf("UnprocessedKeys = %v, want empty (exact fit under the cap)", out.UnprocessedKeys)
	}
}

// One byte over: per-item 167,773 — 99 fit (16,609,527), the 100th trips the
// budget (16,777,300 > 16,777,216). Key-ascending order makes k99 the spill.
func TestBatchGetItemResponseCapSpill(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	mustCreateTable(t, c, "Tbl")
	seedBigItems(t, c, "Tbl", "k", 100, 167765) // per-item 167773

	out, err := c.BatchGetItem(ctx, BatchGetItemInput{RequestItems: map[string]KeysAndAttributes{
		"Tbl": {Keys: bigKeys("k", 100)},
	}})
	if err != nil {
		t.Fatalf("BatchGetItem: %v", err)
	}
	if got := len(out.Responses["Tbl"]); got != 99 {
		t.Errorf("len(Responses[Tbl]) = %d, want 99", got)
	}
	spilled, ok := out.UnprocessedKeys["Tbl"]
	if !ok {
		t.Fatalf("UnprocessedKeys missing Tbl entry: %v", out.UnprocessedKeys)
	}
	if len(spilled.Keys) != 1 || spilled.Keys[0]["pk"].Str() != "k99" {
		t.Errorf("spilled keys = %v, want [k99]", spilled.Keys)
	}
}

// The budget is whole-response: table Aaa consumes most of it, Bbb trips.
func TestBatchGetItemResponseCapCrossTable(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	mustCreateTable(t, c, "Aaa")
	mustCreateTable(t, c, "Bbb")
	seedBigItems(t, c, "Aaa", "a", 80, 200000) // 80 × 200008 = 16,000,640
	seedBigItems(t, c, "Bbb", "b", 20, 200000) // 3 fit (16,600,664); 4th trips (16,800,672 > cap)

	out, err := c.BatchGetItem(ctx, BatchGetItemInput{RequestItems: map[string]KeysAndAttributes{
		"Aaa": {Keys: bigKeys("a", 80)},
		"Bbb": {Keys: bigKeys("b", 20)},
	}})
	if err != nil {
		t.Fatalf("BatchGetItem: %v", err)
	}
	if got := len(out.Responses["Aaa"]); got != 80 {
		t.Errorf("len(Responses[Aaa]) = %d, want 80", got)
	}
	if _, ok := out.UnprocessedKeys["Aaa"]; ok {
		t.Errorf("Aaa spilled keys = %v, want no entry", out.UnprocessedKeys["Aaa"])
	}
	if got := len(out.Responses["Bbb"]); got != 3 {
		t.Errorf("len(Responses[Bbb]) = %d, want 3 (shared accumulator)", got)
	}
	spilled := out.UnprocessedKeys["Bbb"]
	if len(spilled.Keys) != 17 {
		t.Fatalf("len(Bbb spilled) = %d, want 17", len(spilled.Keys))
	}
	// Spilled keys sorted key-ascending: b03..b19.
	for i, k := range spilled.Keys {
		want := fmt.Sprintf("b%02d", i+3)
		if k["pk"].Str() != want {
			t.Errorf("spilled[%d] = %q, want %q (key-ascending)", i, k["pk"].Str(), want)
		}
	}
}

// Measurement is PRE-projection: projecting to the tiny key attribute does
// not change which keys spill (probe P-batch).
func TestBatchGetItemResponseCapPreProjection(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	mustCreateTable(t, c, "Tbl")
	seedBigItems(t, c, "Tbl", "k", 100, 167765) // same shape as the spill case

	out, err := c.BatchGetItem(ctx, BatchGetItemInput{RequestItems: map[string]KeysAndAttributes{
		"Tbl": {Keys: bigKeys("k", 100), ProjectionExpression: "pk"},
	}})
	if err != nil {
		t.Fatalf("BatchGetItem: %v", err)
	}
	if got := len(out.Responses["Tbl"]); got != 99 {
		t.Errorf("len(Responses[Tbl]) = %d, want 99 (projection must not shrink the measurement)", got)
	}
	if got := len(out.UnprocessedKeys["Tbl"].Keys); got != 1 {
		t.Errorf("len(spilled) = %d, want 1", got)
	}
	for i, item := range out.Responses["Tbl"] {
		if len(item) != 1 {
			t.Errorf("returned item[%d] has %d attrs, want 1 (projection applied to returned items)", i, len(item))
		}
	}
}

// Spilled entries echo the request's ConsistentRead, ProjectionExpression,
// and ExpressionAttributeNames (probe P-batch).
func TestBatchGetItemResponseCapEcho(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	mustCreateTable(t, c, "Tbl")
	seedBigItems(t, c, "Tbl", "k", 100, 167765)

	out, err := c.BatchGetItem(ctx, BatchGetItemInput{RequestItems: map[string]KeysAndAttributes{
		"Tbl": {
			Keys:                     bigKeys("k", 100),
			ConsistentRead:           true,
			ProjectionExpression:     "#b",
			ExpressionAttributeNames: map[string]string{"#b": "big"},
		},
	}})
	if err != nil {
		t.Fatalf("BatchGetItem: %v", err)
	}
	spilled := out.UnprocessedKeys["Tbl"]
	if !spilled.ConsistentRead {
		t.Errorf("spilled ConsistentRead = false, want true")
	}
	if spilled.ProjectionExpression != "#b" {
		t.Errorf("spilled ProjectionExpression = %q, want #b", spilled.ProjectionExpression)
	}
	if spilled.ExpressionAttributeNames["#b"] != "big" {
		t.Errorf("spilled ExpressionAttributeNames = %v, want {#b:big}", spilled.ExpressionAttributeNames)
	}
	if len(spilled.Keys) != 1 {
		t.Errorf("len(spilled.Keys) = %d, want 1", len(spilled.Keys))
	}
}

// Once the budget trips, every unreturned request key spills — including
// misses (an unreturned key is unprocessed). Per-item 209,715 bytes: 80 fit
// (16,777,200 ≤ cap), the 81st trips.
func TestBatchGetItemResponseCapMissSpills(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	mustCreateTable(t, c, "Tbl")
	seedBigItems(t, c, "Tbl", "k", 81, 209707) // per-item 8+209707 = 209715

	out, err := c.BatchGetItem(ctx, BatchGetItemInput{RequestItems: map[string]KeysAndAttributes{
		"Tbl": {Keys: bigKeys("k", 81, "miss")}, // 82 keys; "miss" does not exist
	}})
	if err != nil {
		t.Fatalf("BatchGetItem: %v", err)
	}
	if got := len(out.Responses["Tbl"]); got != 80 {
		t.Errorf("len(Responses[Tbl]) = %d, want 80", got)
	}
	spilled := out.UnprocessedKeys["Tbl"]
	if len(spilled.Keys) != 2 {
		t.Fatalf("len(spilled.Keys) = %d, want 2 (k80 + the miss)", len(spilled.Keys))
	}
	// Sorted key-ascending: "k80" < "miss".
	if spilled.Keys[0]["pk"].Str() != "k80" || spilled.Keys[1]["pk"].Str() != "miss" {
		t.Errorf("spilled keys = [%q %q], want [k80 miss]",
			spilled.Keys[0]["pk"].Str(), spilled.Keys[1]["pk"].Str())
	}
	// returned + unprocessed = requested.
	if got := len(out.Responses["Tbl"]) + len(spilled.Keys); got != 82 {
		t.Errorf("returned + spilled = %d, want 82", got)
	}
}
