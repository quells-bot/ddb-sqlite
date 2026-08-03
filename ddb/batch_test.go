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
	mustCreateTable(t, c, "A")
	mustCreateTable(t, c, "B")

	// Seed one row to delete.
	if _, err := c.PutItem(ctx, PutItemInput{TableName: "A", Item: Item{"pk": attrval.NewString("del")}}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	out, err := c.BatchWriteItem(ctx, BatchWriteItemInput{RequestItems: map[string][]WriteRequest{
		"A": {
			{Put: &PutRequest{Item: Item{"pk": attrval.NewString("a1"), "v": attrval.NewString("one")}}},
			{Delete: &DeleteRequest{Key: Item{"pk": attrval.NewString("del")}}},
		},
		"B": {
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
	if it := get("A", "a1"); it["v"].Str() != "one" {
		t.Errorf("A/a1 = %v", it)
	}
	if it := get("A", "del"); len(it) != 0 {
		t.Errorf("A/del should be deleted, got %v", it)
	}
	if it := get("B", "b1"); len(it) == 0 {
		t.Error("B/b1 missing")
	}

	// Overwrite via a second batch.
	if _, err := c.BatchWriteItem(ctx, BatchWriteItemInput{RequestItems: map[string][]WriteRequest{
		"A": {{Put: &PutRequest{Item: Item{"pk": attrval.NewString("a1"), "v": attrval.NewString("updated")}}}},
	}}); err != nil {
		t.Fatalf("BatchWriteItem overwrite: %v", err)
	}
	if it := get("A", "a1"); it["v"].Str() != "updated" {
		t.Errorf("after overwrite A/a1 = %v", it)
	}
}

func TestBatchWriteItemCountLimit(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	mustCreateTable(t, c, "T")

	mk := func(n int) []WriteRequest {
		reqs := make([]WriteRequest, 0, n)
		for i := 0; i < n; i++ {
			reqs = append(reqs, WriteRequest{Put: &PutRequest{Item: Item{"pk": attrval.NewString(fmt.Sprintf("k%02d", i))}}})
		}
		return reqs
	}
	if _, err := c.BatchWriteItem(ctx, BatchWriteItemInput{RequestItems: map[string][]WriteRequest{"T": mk(25)}}); err != nil {
		t.Errorf("25 requests: %v", err)
	}
	_, err := c.BatchWriteItem(ctx, BatchWriteItemInput{RequestItems: map[string][]WriteRequest{"T": mk(26)}})
	if !errors.Is(err, ErrValidation) {
		t.Errorf("26 requests: err = %v, want ErrValidation", err)
	}
}

func TestBatchWriteItemEmptyRequests(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	mustCreateTable(t, c, "T")

	if _, err := c.BatchWriteItem(ctx, BatchWriteItemInput{}); !errors.Is(err, ErrValidation) {
		t.Errorf("nil RequestItems: err = %v, want ErrValidation", err)
	}
	// A table mapping to an empty request slice is rejected (spec §2.4 —
	// keep this test aligned with the Task 1 probe outcome).
	if _, err := c.BatchWriteItem(ctx, BatchWriteItemInput{RequestItems: map[string][]WriteRequest{"T": {}}}); !errors.Is(err, ErrValidation) {
		t.Errorf("empty per-table requests: err = %v, want ErrValidation", err)
	}
}

func TestBatchWriteItemWriteRequestShape(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	mustCreateTable(t, c, "T")

	// Neither Put nor Delete.
	if _, err := c.BatchWriteItem(ctx, BatchWriteItemInput{RequestItems: map[string][]WriteRequest{"T": {{}}}}); !errors.Is(err, ErrValidation) {
		t.Errorf("neither: err = %v, want ErrValidation", err)
	}
	// Both Put and Delete.
	both := WriteRequest{
		Put:    &PutRequest{Item: Item{"pk": attrval.NewString("k")}},
		Delete: &DeleteRequest{Key: Item{"pk": attrval.NewString("k")}},
	}
	if _, err := c.BatchWriteItem(ctx, BatchWriteItemInput{RequestItems: map[string][]WriteRequest{"T": {both}}}); !errors.Is(err, ErrValidation) {
		t.Errorf("both: err = %v, want ErrValidation", err)
	}
}

func TestBatchWriteItemDuplicateKeys(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	mustCreateTable(t, c, "A")
	mustCreateTable(t, c, "B")
	// N-key table: numeric identity canonicalizes at parse (1 ≡ 1.0).
	if _, err := c.CreateTable(ctx, CreateTableInput{
		TableName:            "N",
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
		{"two puts same key", map[string][]WriteRequest{"A": {putS("k"), putS("k")}}, true},
		{"put and delete same key", map[string][]WriteRequest{"A": {putS("k"), delS("k")}}, true},
		{"same key in different tables", map[string][]WriteRequest{"A": {putS("k")}, "B": {putS("k")}}, false},
		{"numeric key spelling variants", map[string][]WriteRequest{"N": {
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
	mustCreateTable(t, c, "T")

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
				"T": {
					{Put: &PutRequest{Item: Item{"pk": attrval.NewString("good")}}},
					tc.wr,
				},
			}})
			if !errors.Is(err, ErrValidation) {
				t.Errorf("err = %v, want ErrValidation", err)
			}
			got, _ := c.GetItem(ctx, GetItemInput{TableName: "T", Key: Item{"pk": attrval.NewString("good")}})
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
	mustCreateTable(t, c, "T")

	big := Item{"pk": attrval.NewString("big"), "data": attrval.NewString(strings.Repeat("x", 400*1024+1))}
	_, err := c.BatchWriteItem(ctx, BatchWriteItemInput{RequestItems: map[string][]WriteRequest{
		"T": {
			{Put: &PutRequest{Item: Item{"pk": attrval.NewString("good")}}},
			{Put: &PutRequest{Item: big}},
		},
	}})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("oversized: err = %v, want ErrValidation", err)
	}
	got, _ := c.GetItem(ctx, GetItemInput{TableName: "T", Key: Item{"pk": attrval.NewString("good")}})
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
			TableName:                 "T",
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
		"T": {
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
		"T": {{Put: &PutRequest{Item: Item{"pk": attrval.NewString("p1"), "sk": attrval.NewString("s1"), "gsi_pk": attrval.NewString("g2"), "gsi_sk": attrval.NewString("a")}}}},
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
		"T": {{Delete: &DeleteRequest{Key: Item{"pk": attrval.NewString("p2"), "sk": attrval.NewString("s2")}}}},
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
		"T": {{Put: &PutRequest{Item: Item{"pk": attrval.NewString("p1"), "sk": attrval.NewString("s1"), "gsi_pk": attrval.NewNumber(mustNum("1"))}}}},
	}})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("err = %v, want ErrValidation", err)
	}
	got, _ := c.GetItem(ctx, GetItemInput{TableName: "T", Key: Item{"pk": attrval.NewString("p1"), "sk": attrval.NewString("s1")}})
	if len(got.Item) != 0 {
		t.Errorf("rejected put visible: %v", got.Item)
	}
}

func TestBatchWriteItemEmptyTableName(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	mustCreateTable(t, c, "T")
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
	mustCreateTable(t, c, "A")
	mustCreateTable(t, c, "B")

	c.PutItem(ctx, PutItemInput{TableName: "A", Item: Item{"pk": attrval.NewString("a1"), "v": attrval.NewString("one")}})
	c.PutItem(ctx, PutItemInput{TableName: "A", Item: Item{"pk": attrval.NewString("a2"), "n": attrval.NewNumber(mustNum("12.5"))}})
	c.PutItem(ctx, PutItemInput{TableName: "B", Item: Item{"pk": attrval.NewString("b1")}})

	out, err := c.BatchGetItem(ctx, BatchGetItemInput{RequestItems: map[string]KeysAndAttributes{
		"A": {Keys: []Item{
			{"pk": attrval.NewString("a2")}, // shuffled order on purpose
			{"pk": attrval.NewString("missing")},
			{"pk": attrval.NewString("a1")},
		}},
		"B": {Keys: []Item{{"pk": attrval.NewString("b1")}}, ConsistentRead: true},
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
	items := out.Responses["A"]
	if len(items) != 2 {
		t.Fatalf("len(Responses[A]) = %d, want 2", len(items))
	}
	if items[0]["v"].Str() != "one" || items[1]["n"].Num().String() != "12.5" {
		t.Errorf("Responses[A] = %v, want [a1 a2] sorted by pk ascending", items)
	}
	if len(out.Responses["B"]) != 1 {
		t.Errorf("len(Responses[B]) = %d, want 1", len(out.Responses["B"]))
	}
}

func TestBatchGetItemCompositeKeys(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	if _, err := c.CreateTable(ctx, CreateTableInput{
		TableName: "C",
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
	c.PutItem(ctx, PutItemInput{TableName: "C", Item: key("1")})
	c.PutItem(ctx, PutItemInput{TableName: "C", Item: key("2")})

	out, err := c.BatchGetItem(ctx, BatchGetItemInput{RequestItems: map[string]KeysAndAttributes{
		"C": {Keys: []Item{key("1"), key("99"), key("2")}},
	}})
	if err != nil {
		t.Fatalf("BatchGetItem: %v", err)
	}
	if len(out.Responses["C"]) != 2 {
		t.Errorf("len(Responses[C]) = %d, want 2 (missing (p1,99) omitted)", len(out.Responses["C"]))
	}
}

func TestBatchGetItemCountLimit(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	mustCreateTable(t, c, "T")

	mk := func(n int) []Item {
		keys := make([]Item, 0, n)
		for i := 0; i < n; i++ {
			keys = append(keys, Item{"pk": attrval.NewString(fmt.Sprintf("k%03d", i))})
		}
		return keys
	}
	if _, err := c.BatchGetItem(ctx, BatchGetItemInput{RequestItems: map[string]KeysAndAttributes{"T": {Keys: mk(100)}}}); err != nil {
		t.Errorf("100 keys: %v", err)
	}
	_, err := c.BatchGetItem(ctx, BatchGetItemInput{RequestItems: map[string]KeysAndAttributes{"T": {Keys: mk(101)}}})
	if !errors.Is(err, ErrValidation) {
		t.Errorf("101 keys: err = %v, want ErrValidation", err)
	}
}

func TestBatchGetItemEmptyRequests(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	mustCreateTable(t, c, "T")

	if _, err := c.BatchGetItem(ctx, BatchGetItemInput{}); !errors.Is(err, ErrValidation) {
		t.Errorf("nil RequestItems: err = %v, want ErrValidation", err)
	}
	// Empty per-table Keys list (spec §2.4 — aligned with Task 1 probe).
	if _, err := c.BatchGetItem(ctx, BatchGetItemInput{RequestItems: map[string]KeysAndAttributes{"T": {}}}); !errors.Is(err, ErrValidation) {
		t.Errorf("empty per-table keys: err = %v, want ErrValidation", err)
	}
}

func TestBatchGetItemDuplicateKeys(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	mustCreateTable(t, c, "T")

	_, err := c.BatchGetItem(ctx, BatchGetItemInput{RequestItems: map[string]KeysAndAttributes{
		"T": {Keys: []Item{{"pk": attrval.NewString("k")}, {"pk": attrval.NewString("k")}}},
	}})
	if !errors.Is(err, ErrValidation) {
		t.Errorf("duplicates: err = %v, want ErrValidation", err)
	}
}

func TestBatchGetItemKeyValidation(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	mustCreateTable(t, c, "T")

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
			_, err := c.BatchGetItem(ctx, BatchGetItemInput{RequestItems: map[string]KeysAndAttributes{"T": {Keys: []Item{tc.key}}}})
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
	mustCreateTable(t, c, "A")
	mustCreateTable(t, c, "B")
	c.PutItem(ctx, PutItemInput{TableName: "A", Item: Item{"pk": attrval.NewString("a1")}})

	out, err := c.BatchGetItem(ctx, BatchGetItemInput{RequestItems: map[string]KeysAndAttributes{
		"A": {Keys: []Item{{"pk": attrval.NewString("a1")}}},
		"B": {Keys: []Item{{"pk": attrval.NewString("ghost")}}},
	}})
	if err != nil {
		t.Fatalf("BatchGetItem: %v", err)
	}
	// All-miss table B is present with an empty slice, matching dynamodb-local.
	if len(out.Responses["B"]) != 0 {
		t.Errorf("len(Responses[B]) = %d, want 0 (empty entry, matching dynamodb-local)", len(out.Responses["B"]))
	}
	if len(out.Responses["A"]) != 1 {
		t.Errorf("len(Responses[A]) = %d, want 1", len(out.Responses["A"]))
	}

	// Single all-miss table: Responses still contains the empty entry for B.
	out, err = c.BatchGetItem(ctx, BatchGetItemInput{RequestItems: map[string]KeysAndAttributes{
		"B": {Keys: []Item{{"pk": attrval.NewString("ghost")}}},
	}})
	if err != nil {
		t.Fatalf("BatchGetItem: %v", err)
	}
	if len(out.Responses) != 1 {
		t.Errorf("len(Responses) = %d, want 1", len(out.Responses))
	}
	if len(out.Responses["B"]) != 0 {
		t.Errorf("len(Responses[B]) = %d, want 0 (empty entry for single all-miss table)", len(out.Responses["B"]))
	}
}

func TestBatchGetItemAttributesToGetRejected(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	mustCreateTable(t, c, "T")
	key := []Item{{"pk": attrval.NewString("k")}}

	// AttributesToGet is the remaining deliberate divergence — the engine
	// honors ProjectionExpression/ExpressionAttributeNames instead.
	_, err := c.BatchGetItem(ctx, BatchGetItemInput{RequestItems: map[string]KeysAndAttributes{
		"T": {Keys: key, AttributesToGet: []string{"pk"}},
	}})
	if !errors.Is(err, ErrValidation) {
		t.Errorf("err = %v, want ErrValidation", err)
	}
}

func TestBatchGetItemExpiredVisible(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	mustCreateTable(t, c, "T")

	// Enable TTL and write an already-expired item (epoch 1 = 1970).
	if _, err := c.UpdateTimeToLive(ctx, UpdateTimeToLiveInput{
		TableName:               "T",
		TimeToLiveSpecification: TimeToLiveSpecification{Enabled: true, AttributeName: "expire"},
	}); err != nil {
		t.Fatalf("UpdateTimeToLive: %v", err)
	}
	c.PutItem(ctx, PutItemInput{TableName: "T", Item: Item{
		"pk":     attrval.NewString("k"),
		"expire": attrval.NewNumber(mustNum("1")),
	}})

	// Faithful read model: expired items are visible until ExpireExpired runs.
	out, err := c.BatchGetItem(ctx, BatchGetItemInput{RequestItems: map[string]KeysAndAttributes{
		"T": {Keys: []Item{{"pk": attrval.NewString("k")}}},
	}})
	if err != nil {
		t.Fatalf("BatchGetItem: %v", err)
	}
	if len(out.Responses["T"]) != 1 {
		t.Errorf("len(Responses[T]) = %d, want 1 (expired item visible, M5a Faithful model)", len(out.Responses["T"]))
	}
}

func TestBatchGetItemEmptyTableName(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	mustCreateTable(t, c, "T")
	_, err := c.BatchGetItem(ctx, BatchGetItemInput{RequestItems: map[string]KeysAndAttributes{
		"": {Keys: []Item{{"pk": attrval.NewString("k")}}},
	}})
	if !errors.Is(err, ErrValidation) {
		t.Errorf("empty table name: err = %v, want ErrValidation", err)
	}
}

func TestBatchGetItemProjection(t *testing.T) {
	c, ctx := newClient(t), context.Background()
	mustCreateTable(t, c, "T")
	for _, it := range []Item{
		{"pk": attrval.NewString("k1"), "v": attrval.NewString("a"), "w": attrval.NewString("x")},
		{"pk": attrval.NewString("k2"), "v": attrval.NewString("b"), "w": attrval.NewString("y")},
		{"pk": attrval.NewString("k3"), "v": attrval.NewString("c"), "w": attrval.NewString("z")},
	} {
		if _, err := c.PutItem(ctx, PutItemInput{TableName: "T", Item: it}); err != nil {
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
		"T": {Keys: keys("k3", "k1", "k2"), ProjectionExpression: "v"},
	}})
	if err != nil {
		t.Fatalf("BatchGetItem: %v", err)
	}
	got := out.Responses["T"]
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
		"T": {Keys: keys("k1"), ProjectionExpression: "#x", ExpressionAttributeNames: map[string]string{"#x": "w"}},
	}})
	if err != nil {
		t.Fatalf("BatchGetItem #name: %v", err)
	}
	if len(out.Responses["T"]) != 1 || out.Responses["T"][0]["w"].Str() != "x" {
		t.Errorf("Responses[T] = %v, want [{w:x}]", out.Responses["T"])
	}

	// Names without a projection are unused -> ErrValidation.
	if _, err := c.BatchGetItem(ctx, BatchGetItemInput{RequestItems: map[string]KeysAndAttributes{
		"T": {Keys: keys("k1"), ExpressionAttributeNames: map[string]string{"#x": "w"}},
	}}); !errors.Is(err, ErrValidation) {
		t.Errorf("names w/o projection: err = %v, want ErrValidation", err)
	}

	// Overlap rejected; AttributesToGet still rejected (deliberate divergence).
	if _, err := c.BatchGetItem(ctx, BatchGetItemInput{RequestItems: map[string]KeysAndAttributes{
		"T": {Keys: keys("k1"), ProjectionExpression: "v, v"},
	}}); !errors.Is(err, ErrValidation) {
		t.Errorf("overlap: err = %v, want ErrValidation", err)
	}
	if _, err := c.BatchGetItem(ctx, BatchGetItemInput{RequestItems: map[string]KeysAndAttributes{
		"T": {Keys: keys("k1"), AttributesToGet: []string{"v"}},
	}}); !errors.Is(err, ErrValidation) {
		t.Errorf("AttributesToGet: err = %v, want ErrValidation", err)
	}
}
