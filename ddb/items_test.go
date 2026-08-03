package ddb

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/quells-bot/ddb-sqlite/attrval"
	"github.com/quells-bot/ddb-sqlite/internal/num"
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
	c.CreateTable(ctx, CreateTableInput{TableName: "T", KeySchema: []KeySchemaElement{{"pk", "HASH"}}, AttributeDefinitions: []AttributeDefinition{{"pk", "S"}}})

	if _, err := c.PutItem(ctx, PutItemInput{TableName: "T", Item: allTypesItem()}); err != nil {
		t.Fatalf("PutItem: %v", err)
	}
}

func TestPutItemValidation(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	c.CreateTable(ctx, CreateTableInput{TableName: "T", KeySchema: []KeySchemaElement{{"pk", "HASH"}}, AttributeDefinitions: []AttributeDefinition{{"pk", "S"}}})

	// Missing partition key.
	if _, err := c.PutItem(ctx, PutItemInput{TableName: "T", Item: Item{"other": attrval.NewString("x")}}); !errors.Is(err, ErrValidation) {
		t.Errorf("missing key: err = %v, want ErrValidation", err)
	}
	// Type mismatch: declared S, supplied N.
	if _, err := c.PutItem(ctx, PutItemInput{TableName: "T", Item: Item{"pk": attrval.NewNumber(mustNum("1"))}}); !errors.Is(err, ErrValidation) {
		t.Errorf("type mismatch: err = %v, want ErrValidation", err)
	}
}

func TestPutItemSizeLimit(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	c.CreateTable(ctx, CreateTableInput{TableName: "T", KeySchema: []KeySchemaElement{{"pk", "HASH"}}, AttributeDefinitions: []AttributeDefinition{{"pk", "S"}}})

	big := Item{"pk": attrval.NewString("k"), "data": attrval.NewString(strings.Repeat("x", 400*1024+1))}
	if _, err := c.PutItem(ctx, PutItemInput{TableName: "T", Item: big}); !errors.Is(err, ErrValidation) {
		t.Errorf("oversized: err = %v, want ErrValidation", err)
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
	c.CreateTable(ctx, CreateTableInput{TableName: "T", KeySchema: []KeySchemaElement{{"pk", "HASH"}}, AttributeDefinitions: []AttributeDefinition{{"pk", "S"}}})

	in := allTypesItem()
	c.PutItem(ctx, PutItemInput{TableName: "T", Item: in})

	out, err := c.GetItem(ctx, GetItemInput{TableName: "T", Key: Item{"pk": attrval.NewString("user#1")}})
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
	c.CreateTable(ctx, CreateTableInput{TableName: "T", KeySchema: []KeySchemaElement{{"pk", "HASH"}}, AttributeDefinitions: []AttributeDefinition{{"pk", "S"}}})

	out, err := c.GetItem(ctx, GetItemInput{TableName: "T", Key: Item{"pk": attrval.NewString("nope")}})
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
	c.CreateTable(ctx, CreateTableInput{TableName: "T", KeySchema: []KeySchemaElement{{"pk", "HASH"}}, AttributeDefinitions: []AttributeDefinition{{"pk", "S"}}})

	// Extra attribute in key.
	if _, err := c.GetItem(ctx, GetItemInput{TableName: "T", Key: Item{"pk": attrval.NewString("k"), "extra": attrval.NewString("x")}}); !errors.Is(err, ErrValidation) {
		t.Errorf("extra key attr: err = %v, want ErrValidation", err)
	}
	// Missing key attribute.
	if _, err := c.GetItem(ctx, GetItemInput{TableName: "T", Key: Item{}}); !errors.Is(err, ErrValidation) {
		t.Errorf("missing key attr: err = %v, want ErrValidation", err)
	}
}

func TestPutOverwriteThenGet(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	c.CreateTable(ctx, CreateTableInput{TableName: "T", KeySchema: []KeySchemaElement{{"pk", "HASH"}}, AttributeDefinitions: []AttributeDefinition{{"pk", "S"}}})

	c.PutItem(ctx, PutItemInput{TableName: "T", Item: Item{"pk": attrval.NewString("k"), "v": attrval.NewString("first")}})
	c.PutItem(ctx, PutItemInput{TableName: "T", Item: Item{"pk": attrval.NewString("k"), "v": attrval.NewString("second")}})

	out, _ := c.GetItem(ctx, GetItemInput{TableName: "T", Key: Item{"pk": attrval.NewString("k")}})
	if out.Item["v"].Str() != "second" {
		t.Errorf("after overwrite, v = %q, want second", out.Item["v"].Str())
	}
}

func TestDeleteItem(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	c.CreateTable(ctx, CreateTableInput{TableName: "T", KeySchema: []KeySchemaElement{{"pk", "HASH"}}, AttributeDefinitions: []AttributeDefinition{{"pk", "S"}}})
	c.PutItem(ctx, PutItemInput{TableName: "T", Item: Item{"pk": attrval.NewString("k"), "v": attrval.NewString("x")}})

	if _, err := c.DeleteItem(ctx, DeleteItemInput{TableName: "T", Key: Item{"pk": attrval.NewString("k")}}); err != nil {
		t.Fatalf("DeleteItem: %v", err)
	}
	out, _ := c.GetItem(ctx, GetItemInput{TableName: "T", Key: Item{"pk": attrval.NewString("k")}})
	if len(out.Item) != 0 {
		t.Errorf("after delete, item = %+v, want empty", out.Item)
	}

	// Idempotent delete of missing key.
	if _, err := c.DeleteItem(ctx, DeleteItemInput{TableName: "T", Key: Item{"pk": attrval.NewString("k")}}); err != nil {
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
	mustCreateTable(t, c, "T")

	// attribute_not_exists on a fresh key succeeds.
	_, err := c.PutItem(ctx, PutItemInput{
		TableName:           "T",
		Item:                Item{"pk": attrval.NewString("k"), "v": attrval.NewString("first")},
		ConditionExpression: "attribute_not_exists(pk)",
	})
	if err != nil {
		t.Fatalf("conditional PutItem: %v", err)
	}
}

func TestPutItemConditionFails(t *testing.T) {
	c, ctx := newClient(t), context.Background()
	mustCreateTable(t, c, "T")
	if _, err := c.PutItem(ctx, PutItemInput{
		TableName: "T",
		Item:      Item{"pk": attrval.NewString("k"), "v": attrval.NewString("first")},
	}); err != nil {
		t.Fatalf("seed PutItem: %v", err)
	}

	_, err := c.PutItem(ctx, PutItemInput{
		TableName:           "T",
		Item:                Item{"pk": attrval.NewString("k"), "v": attrval.NewString("second")},
		ConditionExpression: "attribute_not_exists(pk)",
	})
	if !errors.Is(err, ErrConditionalCheck) {
		t.Fatalf("err = %v, want ErrConditionalCheck", err)
	}

	// The failed write must not have taken effect.
	out, err := c.GetItem(ctx, GetItemInput{TableName: "T", Key: Item{"pk": attrval.NewString("k")}})
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if out.Item["v"].Str() != "first" {
		t.Errorf("v = %q, want first (write should have rolled back)", out.Item["v"].Str())
	}
}

func TestPutItemConditionOnExistingValue(t *testing.T) {
	c, ctx := newClient(t), context.Background()
	mustCreateTable(t, c, "T")
	if _, err := c.PutItem(ctx, PutItemInput{
		TableName: "T",
		Item:      Item{"pk": attrval.NewString("k"), "v": attrval.NewString("first")},
	}); err != nil {
		t.Fatalf("seed PutItem: %v", err)
	}

	_, err := c.PutItem(ctx, PutItemInput{
		TableName:                 "T",
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
	mustCreateTable(t, c, "T")

	// No prior item -> no Attributes.
	out, err := c.PutItem(ctx, PutItemInput{
		TableName:    "T",
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
		TableName:    "T",
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
	mustCreateTable(t, c, "T")
	_, err := c.PutItem(ctx, PutItemInput{
		TableName:    "T",
		Item:         Item{"pk": attrval.NewString("k")},
		ReturnValues: "ALL_NEW",
	})
	if !errors.Is(err, ErrValidation) {
		t.Errorf("err = %v, want ErrValidation", err)
	}
}

func TestPutItemConditionFailureCarriesItem(t *testing.T) {
	c, ctx := newClient(t), context.Background()
	mustCreateTable(t, c, "T")
	if _, err := c.PutItem(ctx, PutItemInput{
		TableName: "T",
		Item:      Item{"pk": attrval.NewString("k"), "v": attrval.NewString("first")},
	}); err != nil {
		t.Fatalf("seed PutItem: %v", err)
	}

	_, err := c.PutItem(ctx, PutItemInput{
		TableName:                           "T",
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
	mustCreateTable(t, c, "T")
	_, err := c.PutItem(ctx, PutItemInput{
		TableName:           "T",
		Item:                Item{"pk": attrval.NewString("k")},
		ConditionExpression: "attribute_exists(",
	})
	if !errors.Is(err, ErrValidation) {
		t.Errorf("err = %v, want ErrValidation", err)
	}
}

func TestDeleteItemConditionSucceeds(t *testing.T) {
	c, ctx := newClient(t), context.Background()
	mustCreateTable(t, c, "T")
	if _, err := c.PutItem(ctx, PutItemInput{
		TableName: "T",
		Item:      Item{"pk": attrval.NewString("k"), "v": attrval.NewString("first")},
	}); err != nil {
		t.Fatalf("seed PutItem: %v", err)
	}

	if _, err := c.DeleteItem(ctx, DeleteItemInput{
		TableName:                 "T",
		Key:                       Item{"pk": attrval.NewString("k")},
		ConditionExpression:       "#a = :want",
		ExpressionAttributeNames:  map[string]string{"#a": "v"},
		ExpressionAttributeValues: map[string]attrval.Value{":want": attrval.NewString("first")},
	}); err != nil {
		t.Fatalf("conditional DeleteItem: %v", err)
	}

	out, err := c.GetItem(ctx, GetItemInput{TableName: "T", Key: Item{"pk": attrval.NewString("k")}})
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if len(out.Item) != 0 {
		t.Errorf("item = %v, want deleted", out.Item)
	}
}

func TestDeleteItemConditionFails(t *testing.T) {
	c, ctx := newClient(t), context.Background()
	mustCreateTable(t, c, "T")
	if _, err := c.PutItem(ctx, PutItemInput{
		TableName: "T",
		Item:      Item{"pk": attrval.NewString("k"), "v": attrval.NewString("first")},
	}); err != nil {
		t.Fatalf("seed PutItem: %v", err)
	}

	_, err := c.DeleteItem(ctx, DeleteItemInput{
		TableName:                 "T",
		Key:                       Item{"pk": attrval.NewString("k")},
		ConditionExpression:       "#a = :want",
		ExpressionAttributeNames:  map[string]string{"#a": "v"},
		ExpressionAttributeValues: map[string]attrval.Value{":want": attrval.NewString("other")},
	})
	if !errors.Is(err, ErrConditionalCheck) {
		t.Fatalf("err = %v, want ErrConditionalCheck", err)
	}

	out, err := c.GetItem(ctx, GetItemInput{TableName: "T", Key: Item{"pk": attrval.NewString("k")}})
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if out.Item["v"].Str() != "first" {
		t.Errorf("item = %v, want intact (delete should have rolled back)", out.Item)
	}
}

func TestDeleteItemConditionOnMissingItem(t *testing.T) {
	c, ctx := newClient(t), context.Background()
	mustCreateTable(t, c, "T")

	// A condition against an absent item sees every path as missing.
	if _, err := c.DeleteItem(ctx, DeleteItemInput{
		TableName:           "T",
		Key:                 Item{"pk": attrval.NewString("k")},
		ConditionExpression: "attribute_not_exists(pk)",
	}); err != nil {
		t.Fatalf("DeleteItem: %v", err)
	}

	_, err := c.DeleteItem(ctx, DeleteItemInput{
		TableName:           "T",
		Key:                 Item{"pk": attrval.NewString("k")},
		ConditionExpression: "attribute_exists(pk)",
	})
	if !errors.Is(err, ErrConditionalCheck) {
		t.Errorf("err = %v, want ErrConditionalCheck", err)
	}
}

func TestDeleteItemReturnValuesAllOld(t *testing.T) {
	c, ctx := newClient(t), context.Background()
	mustCreateTable(t, c, "T")
	if _, err := c.PutItem(ctx, PutItemInput{
		TableName: "T",
		Item:      Item{"pk": attrval.NewString("k"), "v": attrval.NewString("first")},
	}); err != nil {
		t.Fatalf("seed PutItem: %v", err)
	}

	out, err := c.DeleteItem(ctx, DeleteItemInput{
		TableName:    "T",
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
		TableName:    "T",
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
	mustCreateTable(t, c, "T")
	if _, err := c.PutItem(ctx, PutItemInput{TableName: "T", Item: Item{
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
	out, err := c.GetItem(ctx, GetItemInput{TableName: "T", Key: key, ProjectionExpression: "top"})
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if len(out.Item) != 1 || out.Item["top"].Str() != "topval" {
		t.Errorf("Item = %v, want only {top}", out.Item)
	}

	// Nested spine + #name substitution.
	out, err = c.GetItem(ctx, GetItemInput{
		TableName: "T", Key: key,
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
	out, err = c.GetItem(ctx, GetItemInput{TableName: "T", Key: key, ProjectionExpression: "ghost"})
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if len(out.Item) != 0 {
		t.Errorf("Item = %v, want empty", out.Item)
	}

	// Key not found: unchanged behavior (empty Item, no error).
	out, err = c.GetItem(ctx, GetItemInput{TableName: "T", Key: Item{"pk": attrval.NewString("ghost")}, ProjectionExpression: "top"})
	if err != nil || len(out.Item) != 0 {
		t.Errorf("not-found: Item = %v err = %v, want empty/nil", out.Item, err)
	}

	// Overlap and unused names rejected.
	if _, err := c.GetItem(ctx, GetItemInput{TableName: "T", Key: key, ProjectionExpression: "obj, obj.a"}); !errors.Is(err, ErrValidation) {
		t.Errorf("overlap: err = %v, want ErrValidation", err)
	}
	if _, err := c.GetItem(ctx, GetItemInput{TableName: "T", Key: key, ExpressionAttributeNames: map[string]string{"#x": "top"}}); !errors.Is(err, ErrValidation) {
		t.Errorf("unused names: err = %v, want ErrValidation", err)
	}

	// Table-then-key-then-expression precedence: bad key + bad projection -> key error.
	if _, err := c.GetItem(ctx, GetItemInput{TableName: "T", Key: Item{}, ProjectionExpression: "a = :v"}); !errors.Is(err, ErrValidation) {
		t.Errorf("bad key: err = %v, want ErrValidation", err)
	}
}
