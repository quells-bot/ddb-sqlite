package ddb

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/quells-bot/ddb-sqlite-core/attrval"
	"github.com/quells-bot/ddb-sqlite-core/internal/num"
)

func allTypesItem() Item {
	dec, _ := num.Parse("12.5")
	return Item{
		"pk":   attrval.NewString("user#1"),
		"str":  attrval.NewString("hello"),
		"num":  attrval.NewNumber(dec),
		"bin":  attrval.NewBinary([]byte{0x00, 0xff}),
		"bool": attrval.NewBool(true),
		"null": attrval.NewNull(),
		"list": attrval.NewList([]attrval.Value{attrval.NewString("a"), attrval.NewNull()}),
		"map":  attrval.NewMap(map[string]attrval.Value{"inner": attrval.NewString("v")}),
		"ss":   attrval.NewStringSet([]string{"a", "b"}),
		"ns":   attrval.NewNumberSet([]num.Decimal{dec}),
		"bs":   attrval.NewBinarySet([][]byte{{1}, {2}}),
	}
}

func TestPutItemAllTypes(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	c.CreateTable(ctx, CreateTableInput{TableName: "Tbl", KeySchema: []KeySchemaElement{{"pk", "HASH"}}, AttributeDefinitions: []AttributeDefinition{{"pk", "S"}}})

	if _, err := c.PutItem(ctx, PutItemInput{TableName: "Tbl", Item: allTypesItem()}); err != nil {
		t.Fatalf("PutItem: %v", err)
	}
}

func TestPutItemValidation(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	c.CreateTable(ctx, CreateTableInput{TableName: "Tbl", KeySchema: []KeySchemaElement{{"pk", "HASH"}}, AttributeDefinitions: []AttributeDefinition{{"pk", "S"}}})

	// Missing partition key.
	if _, err := c.PutItem(ctx, PutItemInput{TableName: "Tbl", Item: Item{"other": attrval.NewString("x")}}); !errors.Is(err, ErrValidation) {
		t.Errorf("missing key: err = %v, want ErrValidation", err)
	}
	// Type mismatch: declared S, supplied N.
	if _, err := c.PutItem(ctx, PutItemInput{TableName: "Tbl", Item: Item{"pk": attrval.NewNumber(mustNum("1"))}}); !errors.Is(err, ErrValidation) {
		t.Errorf("type mismatch: err = %v, want ErrValidation", err)
	}
}

func TestPutItemSizeLimit(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	c.CreateTable(ctx, CreateTableInput{TableName: "Tbl", KeySchema: []KeySchemaElement{{"pk", "HASH"}}, AttributeDefinitions: []AttributeDefinition{{"pk", "S"}}})

	// Exactly 409600 bytes: accepted (probe-verified).
	ok := Item{"pk": attrval.NewString("k"), "big": attrval.NewString(strings.Repeat("x", 409594))}
	if _, err := c.PutItem(ctx, PutItemInput{TableName: "Tbl", Item: ok}); err != nil {
		t.Fatalf("exact-boundary item: err = %v, want nil", err)
	}

	// 409601 bytes: rejected.
	over := Item{"pk": attrval.NewString("k"), "big": attrval.NewString(strings.Repeat("x", 409595))}
	if _, err := c.PutItem(ctx, PutItemInput{TableName: "Tbl", Item: over}); !errors.Is(err, ErrValidation) {
		t.Errorf("oversized: err = %v, want ErrValidation", err)
	}
}

func TestPutItemDepthLimit(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	c.CreateTable(ctx, CreateTableInput{TableName: "Tbl", KeySchema: []KeySchemaElement{{"pk", "HASH"}}, AttributeDefinitions: []AttributeDefinition{{"pk", "S"}}})

	// Build a 33-level-deep nested map: depth 33 -> rejected.
	inner := attrval.NewString("leaf")
	for i := 0; i < 32; i++ {
		inner = attrval.NewMap(map[string]attrval.Value{"d": inner})
	}
	deep := Item{"pk": attrval.NewString("k"), "nest": inner}
	if _, err := c.PutItem(ctx, PutItemInput{TableName: "Tbl", Item: deep}); !errors.Is(err, ErrValidation) {
		t.Errorf("depth 33: err = %v, want ErrValidation", err)
	}
}

func TestPutItemUnknownTable(t *testing.T) {
	c := newClient(t)
	if _, err := c.PutItem(context.Background(), PutItemInput{TableName: "nope", Item: Item{"pk": attrval.NewString("k")}}); !errors.Is(err, ErrTableNotFound) {
		t.Errorf("err = %v, want ErrTableNotFound", err)
	}
}

func mustNum(s string) num.Decimal {
	d, err := num.Parse(s)
	if err != nil {
		panic(err)
	}
	return d
}

func TestGetItem(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	c.CreateTable(ctx, CreateTableInput{TableName: "Tbl", KeySchema: []KeySchemaElement{{"pk", "HASH"}}, AttributeDefinitions: []AttributeDefinition{{"pk", "S"}}})

	in := allTypesItem()
	c.PutItem(ctx, PutItemInput{TableName: "Tbl", Item: in})

	out, err := c.GetItem(ctx, GetItemInput{TableName: "Tbl", Key: Item{"pk": attrval.NewString("user#1")}})
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if len(out.Item) != len(in) {
		t.Errorf("got %d attrs, want %d", len(out.Item), len(in))
	}
	if out.Item["str"].Str() != "hello" {
		t.Errorf("str = %q, want hello", out.Item["str"].Str())
	}
}

func TestGetItemMissing(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	c.CreateTable(ctx, CreateTableInput{TableName: "Tbl", KeySchema: []KeySchemaElement{{"pk", "HASH"}}, AttributeDefinitions: []AttributeDefinition{{"pk", "S"}}})

	out, err := c.GetItem(ctx, GetItemInput{TableName: "Tbl", Key: Item{"pk": attrval.NewString("nope")}})
	if err != nil {
		t.Fatalf("GetItem missing: %v", err)
	}
	if len(out.Item) != 0 {
		t.Errorf("missing item = %+v, want empty", out.Item)
	}
}

func TestGetItemKeyValidation(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	c.CreateTable(ctx, CreateTableInput{TableName: "Tbl", KeySchema: []KeySchemaElement{{"pk", "HASH"}}, AttributeDefinitions: []AttributeDefinition{{"pk", "S"}}})

	// Extra attribute in key.
	if _, err := c.GetItem(ctx, GetItemInput{TableName: "Tbl", Key: Item{"pk": attrval.NewString("k"), "extra": attrval.NewString("x")}}); !errors.Is(err, ErrValidation) {
		t.Errorf("extra key attr: err = %v, want ErrValidation", err)
	}
	// Missing key attribute.
	if _, err := c.GetItem(ctx, GetItemInput{TableName: "Tbl", Key: Item{}}); !errors.Is(err, ErrValidation) {
		t.Errorf("missing key attr: err = %v, want ErrValidation", err)
	}
}

func TestPutOverwriteThenGet(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	c.CreateTable(ctx, CreateTableInput{TableName: "Tbl", KeySchema: []KeySchemaElement{{"pk", "HASH"}}, AttributeDefinitions: []AttributeDefinition{{"pk", "S"}}})

	c.PutItem(ctx, PutItemInput{TableName: "Tbl", Item: Item{"pk": attrval.NewString("k"), "v": attrval.NewString("first")}})
	c.PutItem(ctx, PutItemInput{TableName: "Tbl", Item: Item{"pk": attrval.NewString("k"), "v": attrval.NewString("second")}})

	out, _ := c.GetItem(ctx, GetItemInput{TableName: "Tbl", Key: Item{"pk": attrval.NewString("k")}})
	if out.Item["v"].Str() != "second" {
		t.Errorf("after overwrite, v = %q, want second", out.Item["v"].Str())
	}
}

func TestDeleteItem(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	c.CreateTable(ctx, CreateTableInput{TableName: "Tbl", KeySchema: []KeySchemaElement{{"pk", "HASH"}}, AttributeDefinitions: []AttributeDefinition{{"pk", "S"}}})
	c.PutItem(ctx, PutItemInput{TableName: "Tbl", Item: Item{"pk": attrval.NewString("k"), "v": attrval.NewString("x")}})

	if _, err := c.DeleteItem(ctx, DeleteItemInput{TableName: "Tbl", Key: Item{"pk": attrval.NewString("k")}}); err != nil {
		t.Fatalf("DeleteItem: %v", err)
	}
	out, _ := c.GetItem(ctx, GetItemInput{TableName: "Tbl", Key: Item{"pk": attrval.NewString("k")}})
	if len(out.Item) != 0 {
		t.Errorf("after delete, item = %+v, want empty", out.Item)
	}

	// Idempotent delete of missing key.
	if _, err := c.DeleteItem(ctx, DeleteItemInput{TableName: "Tbl", Key: Item{"pk": attrval.NewString("k")}}); err != nil {
		t.Errorf("delete missing: %v, want nil", err)
	}
}

func TestDeleteItemUnknownTable(t *testing.T) {
	c := newClient(t)
	if _, err := c.DeleteItem(context.Background(), DeleteItemInput{TableName: "nope", Key: Item{"pk": attrval.NewString("k")}}); !errors.Is(err, ErrTableNotFound) {
		t.Errorf("err = %v, want ErrTableNotFound", err)
	}
}

func mustCreateTable(t *testing.T, c *Client, name string) {
	t.Helper()
	if _, err := c.CreateTable(context.Background(), CreateTableInput{
		TableName:            name,
		KeySchema:            []KeySchemaElement{{AttributeName: "pk", KeyType: "HASH"}},
		AttributeDefinitions: []AttributeDefinition{{AttributeName: "pk", AttributeType: "S"}},
	}); err != nil {
		t.Fatalf("CreateTable %q: %v", name, err)
	}
}

func TestPutItemConditionSucceeds(t *testing.T) {
	c, ctx := newClient(t), context.Background()
	mustCreateTable(t, c, "Tbl")

	// attribute_not_exists on a fresh key succeeds.
	_, err := c.PutItem(ctx, PutItemInput{
		TableName:           "Tbl",
		Item:                Item{"pk": attrval.NewString("k"), "v": attrval.NewString("first")},
		ConditionExpression: "attribute_not_exists(pk)",
	})
	if err != nil {
		t.Fatalf("conditional PutItem: %v", err)
	}
}

func TestPutItemConditionFails(t *testing.T) {
	c, ctx := newClient(t), context.Background()
	mustCreateTable(t, c, "Tbl")
	if _, err := c.PutItem(ctx, PutItemInput{
		TableName: "Tbl",
		Item:      Item{"pk": attrval.NewString("k"), "v": attrval.NewString("first")},
	}); err != nil {
		t.Fatalf("seed PutItem: %v", err)
	}

	_, err := c.PutItem(ctx, PutItemInput{
		TableName:           "Tbl",
		Item:                Item{"pk": attrval.NewString("k"), "v": attrval.NewString("second")},
		ConditionExpression: "attribute_not_exists(pk)",
	})
	if !errors.Is(err, ErrConditionalCheck) {
		t.Fatalf("err = %v, want ErrConditionalCheck", err)
	}

	// The failed write must not have taken effect.
	out, err := c.GetItem(ctx, GetItemInput{TableName: "Tbl", Key: Item{"pk": attrval.NewString("k")}})
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if out.Item["v"].Str() != "first" {
		t.Errorf("v = %q, want first (write should have rolled back)", out.Item["v"].Str())
	}
}

func TestPutItemConditionOnExistingValue(t *testing.T) {
	c, ctx := newClient(t), context.Background()
	mustCreateTable(t, c, "Tbl")
	if _, err := c.PutItem(ctx, PutItemInput{
		TableName: "Tbl",
		Item:      Item{"pk": attrval.NewString("k"), "v": attrval.NewString("first")},
	}); err != nil {
		t.Fatalf("seed PutItem: %v", err)
	}

	_, err := c.PutItem(ctx, PutItemInput{
		TableName:                 "Tbl",
		Item:                      Item{"pk": attrval.NewString("k"), "v": attrval.NewString("second")},
		ConditionExpression:       "#a = :want",
		ExpressionAttributeNames:  map[string]string{"#a": "v"},
		ExpressionAttributeValues: map[string]attrval.Value{":want": attrval.NewString("first")},
	})
	if err != nil {
		t.Fatalf("conditional PutItem: %v", err)
	}
}

func TestPutItemReturnValuesAllOld(t *testing.T) {
	c, ctx := newClient(t), context.Background()
	mustCreateTable(t, c, "Tbl")

	// No prior item -> no Attributes.
	out, err := c.PutItem(ctx, PutItemInput{
		TableName:    "Tbl",
		Item:         Item{"pk": attrval.NewString("k"), "v": attrval.NewString("first")},
		ReturnValues: "ALL_OLD",
	})
	if err != nil {
		t.Fatalf("PutItem: %v", err)
	}
	if len(out.Attributes) != 0 {
		t.Errorf("Attributes = %v, want empty on first write", out.Attributes)
	}

	// Overwrite -> the previous item comes back.
	out, err = c.PutItem(ctx, PutItemInput{
		TableName:    "Tbl",
		Item:         Item{"pk": attrval.NewString("k"), "v": attrval.NewString("second")},
		ReturnValues: "ALL_OLD",
	})
	if err != nil {
		t.Fatalf("PutItem: %v", err)
	}
	if out.Attributes["v"].Str() != "first" {
		t.Errorf("Attributes[v] = %q, want first", out.Attributes["v"].Str())
	}
}

func TestPutItemReturnValuesRejected(t *testing.T) {
	c, ctx := newClient(t), context.Background()
	mustCreateTable(t, c, "Tbl")
	_, err := c.PutItem(ctx, PutItemInput{
		TableName:    "Tbl",
		Item:         Item{"pk": attrval.NewString("k")},
		ReturnValues: "ALL_NEW",
	})
	if !errors.Is(err, ErrValidation) {
		t.Errorf("err = %v, want ErrValidation", err)
	}
}

func TestPutItemConditionFailureCarriesItem(t *testing.T) {
	c, ctx := newClient(t), context.Background()
	mustCreateTable(t, c, "Tbl")
	if _, err := c.PutItem(ctx, PutItemInput{
		TableName: "Tbl",
		Item:      Item{"pk": attrval.NewString("k"), "v": attrval.NewString("first")},
	}); err != nil {
		t.Fatalf("seed PutItem: %v", err)
	}

	_, err := c.PutItem(ctx, PutItemInput{
		TableName:                           "Tbl",
		Item:                                Item{"pk": attrval.NewString("k")},
		ConditionExpression:                 "attribute_not_exists(pk)",
		ReturnValuesOnConditionCheckFailure: "ALL_OLD",
	})
	var ccf *ConditionalCheckFailedError
	if !errors.As(err, &ccf) {
		t.Fatalf("err = %v, want *ConditionalCheckFailedError", err)
	}
	if ccf.Item["v"].Str() != "first" {
		t.Errorf("Item = %v, want the pre-write item", ccf.Item)
	}
}

func TestPutItemBadExpression(t *testing.T) {
	c, ctx := newClient(t), context.Background()
	mustCreateTable(t, c, "Tbl")
	_, err := c.PutItem(ctx, PutItemInput{
		TableName:           "Tbl",
		Item:                Item{"pk": attrval.NewString("k")},
		ConditionExpression: "attribute_exists(",
	})
	if !errors.Is(err, ErrValidation) {
		t.Errorf("err = %v, want ErrValidation", err)
	}
}

func TestDeleteItemConditionSucceeds(t *testing.T) {
	c, ctx := newClient(t), context.Background()
	mustCreateTable(t, c, "Tbl")
	if _, err := c.PutItem(ctx, PutItemInput{
		TableName: "Tbl",
		Item:      Item{"pk": attrval.NewString("k"), "v": attrval.NewString("first")},
	}); err != nil {
		t.Fatalf("seed PutItem: %v", err)
	}

	if _, err := c.DeleteItem(ctx, DeleteItemInput{
		TableName:                 "Tbl",
		Key:                       Item{"pk": attrval.NewString("k")},
		ConditionExpression:       "#a = :want",
		ExpressionAttributeNames:  map[string]string{"#a": "v"},
		ExpressionAttributeValues: map[string]attrval.Value{":want": attrval.NewString("first")},
	}); err != nil {
		t.Fatalf("conditional DeleteItem: %v", err)
	}

	out, err := c.GetItem(ctx, GetItemInput{TableName: "Tbl", Key: Item{"pk": attrval.NewString("k")}})
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if len(out.Item) != 0 {
		t.Errorf("item = %v, want deleted", out.Item)
	}
}

func TestDeleteItemConditionFails(t *testing.T) {
	c, ctx := newClient(t), context.Background()
	mustCreateTable(t, c, "Tbl")
	if _, err := c.PutItem(ctx, PutItemInput{
		TableName: "Tbl",
		Item:      Item{"pk": attrval.NewString("k"), "v": attrval.NewString("first")},
	}); err != nil {
		t.Fatalf("seed PutItem: %v", err)
	}

	_, err := c.DeleteItem(ctx, DeleteItemInput{
		TableName:                 "Tbl",
		Key:                       Item{"pk": attrval.NewString("k")},
		ConditionExpression:       "#a = :want",
		ExpressionAttributeNames:  map[string]string{"#a": "v"},
		ExpressionAttributeValues: map[string]attrval.Value{":want": attrval.NewString("other")},
	})
	if !errors.Is(err, ErrConditionalCheck) {
		t.Fatalf("err = %v, want ErrConditionalCheck", err)
	}

	out, err := c.GetItem(ctx, GetItemInput{TableName: "Tbl", Key: Item{"pk": attrval.NewString("k")}})
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if out.Item["v"].Str() != "first" {
		t.Errorf("item = %v, want intact (delete should have rolled back)", out.Item)
	}
}

func TestDeleteItemConditionOnMissingItem(t *testing.T) {
	c, ctx := newClient(t), context.Background()
	mustCreateTable(t, c, "Tbl")

	// A condition against an absent item sees every path as missing.
	if _, err := c.DeleteItem(ctx, DeleteItemInput{
		TableName:           "Tbl",
		Key:                 Item{"pk": attrval.NewString("k")},
		ConditionExpression: "attribute_not_exists(pk)",
	}); err != nil {
		t.Fatalf("DeleteItem: %v", err)
	}

	_, err := c.DeleteItem(ctx, DeleteItemInput{
		TableName:           "Tbl",
		Key:                 Item{"pk": attrval.NewString("k")},
		ConditionExpression: "attribute_exists(pk)",
	})
	if !errors.Is(err, ErrConditionalCheck) {
		t.Errorf("err = %v, want ErrConditionalCheck", err)
	}
}

func TestDeleteItemReturnValuesAllOld(t *testing.T) {
	c, ctx := newClient(t), context.Background()
	mustCreateTable(t, c, "Tbl")
	if _, err := c.PutItem(ctx, PutItemInput{
		TableName: "Tbl",
		Item:      Item{"pk": attrval.NewString("k"), "v": attrval.NewString("first")},
	}); err != nil {
		t.Fatalf("seed PutItem: %v", err)
	}

	out, err := c.DeleteItem(ctx, DeleteItemInput{
		TableName:    "Tbl",
		Key:          Item{"pk": attrval.NewString("k")},
		ReturnValues: "ALL_OLD",
	})
	if err != nil {
		t.Fatalf("DeleteItem: %v", err)
	}
	if out.Attributes["v"].Str() != "first" {
		t.Errorf("Attributes = %v, want the deleted item", out.Attributes)
	}

	// Deleting a missing key is idempotent and returns nothing.
	out, err = c.DeleteItem(ctx, DeleteItemInput{
		TableName:    "Tbl",
		Key:          Item{"pk": attrval.NewString("k")},
		ReturnValues: "ALL_OLD",
	})
	if err != nil {
		t.Fatalf("DeleteItem: %v", err)
	}
	if len(out.Attributes) != 0 {
		t.Errorf("Attributes = %v, want empty", out.Attributes)
	}
}

func TestGetItemProjection(t *testing.T) {
	c, ctx := newClient(t), context.Background()
	mustCreateTable(t, c, "Tbl")
	if _, err := c.PutItem(ctx, PutItemInput{TableName: "Tbl", Item: Item{
		"pk":  attrval.NewString("k"),
		"top": attrval.NewString("topval"),
		"num": attrval.NewNumber(mustNum("42")),
		"obj": attrval.NewMap(map[string]attrval.Value{
			"a": attrval.NewString("aval"),
			"b": attrval.NewString("bval"),
		}),
	}}); err != nil {
		t.Fatalf("PutItem: %v", err)
	}
	key := Item{"pk": attrval.NewString("k")}

	// Single attr: only that attr returned — keys are NOT auto-returned.
	out, err := c.GetItem(ctx, GetItemInput{TableName: "Tbl", Key: key, ProjectionExpression: "top"})
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if len(out.Item) != 1 || out.Item["top"].Str() != "topval" {
		t.Errorf("Item = %v, want only {top}", out.Item)
	}

	// Nested spine + #name substitution.
	out, err = c.GetItem(ctx, GetItemInput{
		TableName: "Tbl", Key: key,
		ProjectionExpression:     "#o.a, num",
		ExpressionAttributeNames: map[string]string{"#o": "obj"},
	})
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if len(out.Item) != 2 || out.Item["obj"].Map()["a"].Str() != "aval" || len(out.Item["obj"].Map()) != 1 {
		t.Errorf("Item = %v, want {obj:{a}, num}", out.Item)
	}

	// Missing path: omitted, no error.
	out, err = c.GetItem(ctx, GetItemInput{TableName: "Tbl", Key: key, ProjectionExpression: "ghost"})
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if len(out.Item) != 0 {
		t.Errorf("Item = %v, want empty", out.Item)
	}

	// Key not found: unchanged behavior (empty Item, no error).
	out, err = c.GetItem(ctx, GetItemInput{TableName: "Tbl", Key: Item{"pk": attrval.NewString("ghost")}, ProjectionExpression: "top"})
	if err != nil || len(out.Item) != 0 {
		t.Errorf("not-found: Item = %v err = %v, want empty/nil", out.Item, err)
	}

	// Overlap and unused names rejected.
	if _, err := c.GetItem(ctx, GetItemInput{TableName: "Tbl", Key: key, ProjectionExpression: "obj, obj.a"}); !errors.Is(err, ErrValidation) {
		t.Errorf("overlap: err = %v, want ErrValidation", err)
	}
	if _, err := c.GetItem(ctx, GetItemInput{TableName: "Tbl", Key: key, ExpressionAttributeNames: map[string]string{"#x": "top"}}); !errors.Is(err, ErrValidation) {
		t.Errorf("unused names: err = %v, want ErrValidation", err)
	}

	// Table-then-key-then-expression precedence: bad key + bad projection -> key error.
	if _, err := c.GetItem(ctx, GetItemInput{TableName: "Tbl", Key: Item{}, ProjectionExpression: "a = :v"}); !errors.Is(err, ErrValidation) {
		t.Errorf("bad key: err = %v, want ErrValidation", err)
	}
}

func TestKeyValueLengthLimits(t *testing.T) {
	c, ctx := newClient(t), context.Background()
	// Composite table: pk HASH S, sk RANGE S.
	if _, err := c.CreateTable(ctx, CreateTableInput{
		TableName:            "Kvs",
		KeySchema:            []KeySchemaElement{{AttributeName: "pk", KeyType: "HASH"}, {AttributeName: "sk", KeyType: "RANGE"}},
		AttributeDefinitions: []AttributeDefinition{{AttributeName: "pk", AttributeType: "S"}, {AttributeName: "sk", AttributeType: "S"}},
	}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	// Binary-key table: pk HASH B.
	if _, err := c.CreateTable(ctx, CreateTableInput{
		TableName:            "Kbs",
		KeySchema:            []KeySchemaElement{{AttributeName: "pk", KeyType: "HASH"}},
		AttributeDefinitions: []AttributeDefinition{{AttributeName: "pk", AttributeType: "B"}},
	}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}

	put := func(table string, item Item) error {
		_, err := c.PutItem(ctx, PutItemInput{TableName: table, Item: item})
		return err
	}
	strItem := func(pk, sk string) Item {
		return Item{"pk": attrval.NewString(pk), "sk": attrval.NewString(sk)}
	}

	cases := []struct {
		name    string
		table   string
		item    Item
		wantErr bool
	}{
		{"pk 2048 accepted", "Kvs", strItem(strings.Repeat("k", 2048), "s"), false},
		{"pk 2049 rejected", "Kvs", strItem(strings.Repeat("k", 2049), "s"), true},
		{"sk 1024 accepted", "Kvs", strItem("k", strings.Repeat("s", 1024)), false},
		{"sk 1025 rejected", "Kvs", strItem("k", strings.Repeat("s", 1025)), true},
		{"empty pk rejected", "Kvs", strItem("", "s"), true},
		{"empty sk rejected", "Kvs", strItem("k", ""), true},
		{"binary pk 2048 accepted", "Kbs", Item{"pk": attrval.NewBinary(make([]byte, 2048))}, false},
		{"binary pk 2049 rejected", "Kbs", Item{"pk": attrval.NewBinary(make([]byte, 2049))}, true},
		{"empty binary pk rejected", "Kbs", Item{"pk": attrval.NewBinary([]byte{})}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := put(tc.table, tc.item)
			if tc.wantErr && !errors.Is(err, ErrValidation) {
				t.Errorf("err = %v, want ErrValidation", err)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("err = %v, want nil", err)
			}
		})
	}

	// Read paths share the validator via validateKey.
	if _, err := c.GetItem(ctx, GetItemInput{TableName: "Kvs", Key: strItem(strings.Repeat("k", 2049), "s")}); !errors.Is(err, ErrValidation) {
		t.Errorf("GetItem oversize pk: err = %v, want ErrValidation", err)
	}
	if _, err := c.GetItem(ctx, GetItemInput{TableName: "Kvs", Key: strItem("", "s")}); !errors.Is(err, ErrValidation) {
		t.Errorf("GetItem empty pk: err = %v, want ErrValidation", err)
	}
	if _, err := c.DeleteItem(ctx, DeleteItemInput{TableName: "Kvs", Key: strItem("k", strings.Repeat("s", 1025))}); !errors.Is(err, ErrValidation) {
		t.Errorf("DeleteItem oversize sk: err = %v, want ErrValidation", err)
	}
	if _, err := c.UpdateItem(ctx, UpdateItemInput{TableName: "Kvs", Key: strItem("", "s")}); !errors.Is(err, ErrValidation) {
		t.Errorf("UpdateItem empty pk: err = %v, want ErrValidation", err)
	}
	// BatchWriteItem: one oversize put fails the whole batch.
	_, err := c.BatchWriteItem(ctx, BatchWriteItemInput{RequestItems: map[string][]WriteRequest{
		"Kvs": {
			{Put: &PutRequest{Item: strItem("ok", "s")}},
			{Put: &PutRequest{Item: strItem(strings.Repeat("k", 2049), "s")}},
		},
	}})
	if !errors.Is(err, ErrValidation) {
		t.Errorf("BatchWriteItem oversize pk: err = %v, want ErrValidation", err)
	}
	// BatchGetItem: oversize key rejected.
	_, err = c.BatchGetItem(ctx, BatchGetItemInput{RequestItems: map[string]KeysAndAttributes{
		"Kvs": {Keys: []Item{strItem(strings.Repeat("k", 2049), "s")}},
	}})
	if !errors.Is(err, ErrValidation) {
		t.Errorf("BatchGetItem oversize pk: err = %v, want ErrValidation", err)
	}
}
