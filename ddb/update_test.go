package ddb

import (
	"context"
	"errors"
	"testing"

	"github.com/quells-bot/ddb-sqlite/attrval"
)

// newUpdateTable opens a client with a single-key table "T" and returns both.
func newUpdateTable(t *testing.T) (*Client, context.Context) {
	t.Helper()
	c := newClient(t)
	ctx := context.Background()
	if _, err := c.CreateTable(ctx, CreateTableInput{
		TableName:            "T",
		KeySchema:            []KeySchemaElement{{"pk", "HASH"}},
		AttributeDefinitions: []AttributeDefinition{{"pk", "S"}},
	}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	return c, ctx
}

func mustGet(t *testing.T, c *Client, ctx context.Context, pk string) Item {
	t.Helper()
	out, err := c.GetItem(ctx, GetItemInput{TableName: "T", Key: Item{"pk": attrval.NewString(pk)}})
	if err != nil {
		t.Fatalf("GetItem(%q): %v", pk, err)
	}
	return out.Item
}

func TestUpdateItemUpsert(t *testing.T) {
	c, ctx := newUpdateTable(t)

	// An update against an absent key creates the item.
	if _, err := c.UpdateItem(ctx, UpdateItemInput{
		TableName:                 "T",
		Key:                       Item{"pk": attrval.NewString("k")},
		UpdateExpression:          "SET s = :s",
		ExpressionAttributeValues: map[string]attrval.Value{":s": attrval.NewString("v")},
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got := mustGet(t, c, ctx, "k")
	if got["pk"].Str() != "k" || got["s"].Str() != "v" {
		t.Errorf("item = %v, want pk=k s=v", got)
	}

	// No UpdateExpression at all creates a key-only item.
	if _, err := c.UpdateItem(ctx, UpdateItemInput{TableName: "T", Key: Item{"pk": attrval.NewString("bare")}}); err != nil {
		t.Fatalf("key-only upsert: %v", err)
	}
	if got := mustGet(t, c, ctx, "bare"); len(got) != 1 || got["pk"].Str() != "bare" {
		t.Errorf("item = %v, want the key alone", got)
	}
}

func TestUpdateItemActions(t *testing.T) {
	c, ctx := newUpdateTable(t)
	seed := func() {
		t.Helper()
		if _, err := c.PutItem(ctx, PutItemInput{TableName: "T", Item: Item{
			"pk": attrval.NewString("k"),
			"s":  attrval.NewString("hello"),
			"n":  attrval.NewNumber(mustNum("42")),
			"ss": attrval.NewStringSet([]string{"a", "b"}),
		}}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	cases := []struct {
		name   string
		expr   string
		values map[string]attrval.Value
		check  func(t *testing.T, got Item)
	}{
		{
			name:   "SET",
			expr:   "SET s = :s",
			values: map[string]attrval.Value{":s": attrval.NewString("bye")},
			check: func(t *testing.T, got Item) {
				if got["s"].Str() != "bye" {
					t.Errorf("s = %v", got["s"])
				}
			},
		},
		{
			name: "REMOVE",
			expr: "REMOVE s",
			check: func(t *testing.T, got Item) {
				if _, ok := got["s"]; ok {
					t.Error("s survived REMOVE")
				}
			},
		},
		{
			name:   "ADD number",
			expr:   "ADD n :one",
			values: map[string]attrval.Value{":one": attrval.NewNumber(mustNum("1"))},
			check: func(t *testing.T, got Item) {
				if got["n"].Num().String() != "43" {
					t.Errorf("n = %v, want 43", got["n"])
				}
			},
		},
		{
			name:   "DELETE emptying a set removes the attribute",
			expr:   "DELETE ss :all",
			values: map[string]attrval.Value{":all": attrval.NewStringSet([]string{"a", "b"})},
			check: func(t *testing.T, got Item) {
				if _, ok := got["ss"]; ok {
					t.Error("an emptied set must be removed")
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seed()
			if _, err := c.UpdateItem(ctx, UpdateItemInput{
				TableName:                 "T",
				Key:                       Item{"pk": attrval.NewString("k")},
				UpdateExpression:          tc.expr,
				ExpressionAttributeValues: tc.values,
			}); err != nil {
				t.Fatalf("UpdateItem(%q): %v", tc.expr, err)
			}
			tc.check(t, mustGet(t, c, ctx, "k"))
		})
	}
}

func TestUpdateItemReturnValues(t *testing.T) {
	c, ctx := newUpdateTable(t)
	if _, err := c.PutItem(ctx, PutItemInput{TableName: "T", Item: Item{
		"pk":    attrval.NewString("k"),
		"s":     attrval.NewString("old"),
		"other": attrval.NewString("keep"),
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	cases := []struct {
		mode  string
		want  map[string]string // attribute -> expected string value
		empty bool
	}{
		{mode: "", empty: true},
		{mode: "NONE", empty: true},
		{mode: "ALL_OLD", want: map[string]string{"pk": "k", "s": "old", "other": "keep"}},
		{mode: "ALL_NEW", want: map[string]string{"pk": "k", "s": "new", "other": "keep"}},
		{mode: "UPDATED_OLD", want: map[string]string{"s": "old"}},
		{mode: "UPDATED_NEW", want: map[string]string{"s": "new"}},
	}
	for _, tc := range cases {
		name := tc.mode
		if name == "" {
			name = "default"
		}
		t.Run(name, func(t *testing.T) {
			// Re-seed so every mode sees the same before-state.
			if _, err := c.PutItem(ctx, PutItemInput{TableName: "T", Item: Item{
				"pk": attrval.NewString("k"), "s": attrval.NewString("old"), "other": attrval.NewString("keep"),
			}}); err != nil {
				t.Fatalf("re-seed: %v", err)
			}
			out, err := c.UpdateItem(ctx, UpdateItemInput{
				TableName:                 "T",
				Key:                       Item{"pk": attrval.NewString("k")},
				UpdateExpression:          "SET s = :s",
				ExpressionAttributeValues: map[string]attrval.Value{":s": attrval.NewString("new")},
				ReturnValues:              tc.mode,
			})
			if err != nil {
				t.Fatalf("UpdateItem: %v", err)
			}
			if tc.empty {
				if len(out.Attributes) != 0 {
					t.Errorf("Attributes = %v, want none", out.Attributes)
				}
				return
			}
			if len(out.Attributes) != len(tc.want) {
				t.Fatalf("Attributes = %v, want %v", out.Attributes, tc.want)
			}
			for k, want := range tc.want {
				if v, ok := out.Attributes[k]; !ok || v.Str() != want {
					t.Errorf("Attributes[%s] = %v, want %q", k, v, want)
				}
			}
		})
	}

	// ALL_OLD against an absent key returns nothing.
	out, err := c.UpdateItem(ctx, UpdateItemInput{
		TableName:                 "T",
		Key:                       Item{"pk": attrval.NewString("absent")},
		UpdateExpression:          "SET s = :s",
		ExpressionAttributeValues: map[string]attrval.Value{":s": attrval.NewString("x")},
		ReturnValues:              "ALL_OLD",
	})
	if err != nil {
		t.Fatalf("UpdateItem on an absent key: %v", err)
	}
	if len(out.Attributes) != 0 {
		t.Errorf("Attributes = %v, want none for an absent item", out.Attributes)
	}
}

func TestUpdateItemCondition(t *testing.T) {
	c, ctx := newUpdateTable(t)
	if _, err := c.PutItem(ctx, PutItemInput{TableName: "T", Item: Item{
		"pk": attrval.NewString("k"), "s": attrval.NewString("old"),
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Unsatisfied condition: the item is untouched and the error carries it.
	_, err := c.UpdateItem(ctx, UpdateItemInput{
		TableName:        "T",
		Key:              Item{"pk": attrval.NewString("k")},
		UpdateExpression: "SET s = :new",
		// #a and :want are used only by the condition; :new only by the update.
		ConditionExpression:      "#a = :want",
		ExpressionAttributeNames: map[string]string{"#a": "s"},
		ExpressionAttributeValues: map[string]attrval.Value{
			":want": attrval.NewString("nope"),
			":new":  attrval.NewString("new"),
		},
		ReturnValuesOnConditionCheckFailure: "ALL_OLD",
	})
	if !errors.Is(err, ErrConditionalCheck) {
		t.Fatalf("err = %v, want ErrConditionalCheck", err)
	}
	var ccf *ConditionalCheckFailedError
	if !errors.As(err, &ccf) {
		t.Fatalf("err = %v, want *ConditionalCheckFailedError", err)
	}
	if ccf.Item["s"].Str() != "old" {
		t.Errorf("ccf.Item = %v, want the pre-write item", ccf.Item)
	}
	if got := mustGet(t, c, ctx, "k"); got["s"].Str() != "old" {
		t.Errorf("item changed despite a failed condition: %v", got)
	}
}

func TestUpdateItemValidation(t *testing.T) {
	c, ctx := newUpdateTable(t)
	key := Item{"pk": attrval.NewString("k")}

	cases := []struct {
		name string
		in   UpdateItemInput
	}{
		{"key attribute target", UpdateItemInput{TableName: "T", Key: key, UpdateExpression: "SET pk = :v", ExpressionAttributeValues: map[string]attrval.Value{":v": attrval.NewString("other")}}},
		{"REMOVE of a key attribute", UpdateItemInput{TableName: "T", Key: key, UpdateExpression: "REMOVE pk"}},
		{"nested ADD", UpdateItemInput{TableName: "T", Key: key, UpdateExpression: "ADD a.b :v", ExpressionAttributeValues: map[string]attrval.Value{":v": attrval.NewNumber(mustNum("1"))}}},
		{"malformed expression", UpdateItemInput{TableName: "T", Key: key, UpdateExpression: "SET a"}},
		{"unsupported ReturnValues", UpdateItemInput{TableName: "T", Key: key, ReturnValues: "NOPE"}},
		{"unsupported ReturnValuesOnConditionCheckFailure", UpdateItemInput{TableName: "T", Key: key, ReturnValuesOnConditionCheckFailure: "NOPE"}},
		{"overlapping paths", UpdateItemInput{TableName: "T", Key: key, UpdateExpression: "SET a = :v REMOVE a", ExpressionAttributeValues: map[string]attrval.Value{":v": attrval.NewString("x")}}},
		{"key with an extra attribute", UpdateItemInput{TableName: "T", Key: Item{"pk": attrval.NewString("k"), "extra": attrval.NewString("x")}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := c.UpdateItem(ctx, tc.in); !errors.Is(err, ErrValidation) {
				t.Errorf("err = %v, want ErrValidation", err)
			}
		})
	}

	if _, err := c.UpdateItem(ctx, UpdateItemInput{TableName: "missing", Key: key}); !errors.Is(err, ErrTableNotFound) {
		t.Errorf("unknown table: err = %v, want ErrTableNotFound", err)
	}
}
