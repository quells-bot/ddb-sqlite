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

	if err := c.PutItem(ctx, PutItemInput{TableName: "T", Item: allTypesItem()}); err != nil {
		t.Fatalf("PutItem: %v", err)
	}
}

func TestPutItemValidation(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	c.CreateTable(ctx, CreateTableInput{TableName: "T", KeySchema: []KeySchemaElement{{"pk", "HASH"}}, AttributeDefinitions: []AttributeDefinition{{"pk", "S"}}})

	// Missing partition key.
	if err := c.PutItem(ctx, PutItemInput{TableName: "T", Item: Item{"other": attrval.NewString("x")}}); !errors.Is(err, ErrValidation) {
		t.Errorf("missing key: err = %v, want ErrValidation", err)
	}
	// Type mismatch: declared S, supplied N.
	if err := c.PutItem(ctx, PutItemInput{TableName: "T", Item: Item{"pk": attrval.NewNumber(mustNum("1"))}}); !errors.Is(err, ErrValidation) {
		t.Errorf("type mismatch: err = %v, want ErrValidation", err)
	}
}

func TestPutItemSizeLimit(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	c.CreateTable(ctx, CreateTableInput{TableName: "T", KeySchema: []KeySchemaElement{{"pk", "HASH"}}, AttributeDefinitions: []AttributeDefinition{{"pk", "S"}}})

	big := Item{"pk": attrval.NewString("k"), "data": attrval.NewString(strings.Repeat("x", 400*1024+1))}
	if err := c.PutItem(ctx, PutItemInput{TableName: "T", Item: big}); !errors.Is(err, ErrValidation) {
		t.Errorf("oversized: err = %v, want ErrValidation", err)
	}
}

func TestPutItemUnknownTable(t *testing.T) {
	c := newClient(t)
	if err := c.PutItem(context.Background(), PutItemInput{TableName: "nope", Item: Item{"pk": attrval.NewString("k")}}); !errors.Is(err, ErrTableNotFound) {
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

	if err := c.DeleteItem(ctx, DeleteItemInput{TableName: "T", Key: Item{"pk": attrval.NewString("k")}}); err != nil {
		t.Fatalf("DeleteItem: %v", err)
	}
	out, _ := c.GetItem(ctx, GetItemInput{TableName: "T", Key: Item{"pk": attrval.NewString("k")}})
	if len(out.Item) != 0 {
		t.Errorf("after delete, item = %+v, want empty", out.Item)
	}

	// Idempotent delete of missing key.
	if err := c.DeleteItem(ctx, DeleteItemInput{TableName: "T", Key: Item{"pk": attrval.NewString("k")}}); err != nil {
		t.Errorf("delete missing: %v, want nil", err)
	}
}

func TestDeleteItemUnknownTable(t *testing.T) {
	c := newClient(t)
	if err := c.DeleteItem(context.Background(), DeleteItemInput{TableName: "nope", Key: Item{"pk": attrval.NewString("k")}}); !errors.Is(err, ErrTableNotFound) {
		t.Errorf("err = %v, want ErrTableNotFound", err)
	}
}
