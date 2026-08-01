package ddb

import (
	"context"
	"errors"
	"testing"
)

func newClient(t *testing.T) *Client {
	t.Helper()
	c, err := Open(context.Background(), Options{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestCreateTable(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	desc, err := c.CreateTable(ctx, CreateTableInput{
		TableName:            "Users",
		KeySchema:            []KeySchemaElement{{AttributeName: "pk", KeyType: "HASH"}, {AttributeName: "sk", KeyType: "RANGE"}},
		AttributeDefinitions: []AttributeDefinition{{AttributeName: "pk", AttributeType: "S"}, {AttributeName: "sk", AttributeType: "N"}},
	})
	if err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	if desc.Name != "Users" || desc.Hash != "pk" || desc.Range != "sk" || desc.HashType != "S" || desc.RangeType != "N" {
		t.Errorf("desc = %+v", desc)
	}
	if desc.CreationTime.IsZero() {
		t.Error("CreationTime not set")
	}
}

func TestCreateTableValidation(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()

	cases := []struct {
		name string
		in   CreateTableInput
	}{
		{"empty name", CreateTableInput{KeySchema: []KeySchemaElement{{"pk", "HASH"}}, AttributeDefinitions: []AttributeDefinition{{"pk", "S"}}}},
		{"missing hash", CreateTableInput{TableName: "T", KeySchema: []KeySchemaElement{{"sk", "RANGE"}}, AttributeDefinitions: []AttributeDefinition{{"sk", "S"}}}},
		{"bad key type", CreateTableInput{TableName: "T", KeySchema: []KeySchemaElement{{"pk", "HASH"}}, AttributeDefinitions: []AttributeDefinition{{"pk", "X"}}}},
		{"uncovered key", CreateTableInput{TableName: "T", KeySchema: []KeySchemaElement{{"pk", "HASH"}}, AttributeDefinitions: []AttributeDefinition{{"other", "S"}}}},
		{"extra hash key", CreateTableInput{TableName: "T", KeySchema: []KeySchemaElement{{"pk", "HASH"}, {"pk2", "HASH"}}, AttributeDefinitions: []AttributeDefinition{{"pk", "S"}, {"pk2", "S"}}}},
		{"same attr as HASH and RANGE", CreateTableInput{TableName: "T", KeySchema: []KeySchemaElement{{"pk", "HASH"}, {"pk", "RANGE"}}, AttributeDefinitions: []AttributeDefinition{{"pk", "S"}}}},
		{"two RANGE keys", CreateTableInput{TableName: "T", KeySchema: []KeySchemaElement{{"pk", "RANGE"}, {"sk", "RANGE"}}, AttributeDefinitions: []AttributeDefinition{{"pk", "S"}, {"sk", "S"}}}},
		{"three key schema elements", CreateTableInput{TableName: "T", KeySchema: []KeySchemaElement{{"pk", "HASH"}, {"sk", "RANGE"}, {"sk2", "RANGE"}}, AttributeDefinitions: []AttributeDefinition{{"pk", "S"}, {"sk", "S"}, {"sk2", "S"}}}},
		{"extra AttributeDefinition not in key schema", CreateTableInput{TableName: "T", KeySchema: []KeySchemaElement{{"pk", "HASH"}}, AttributeDefinitions: []AttributeDefinition{{"pk", "S"}, {"extra", "S"}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := c.CreateTable(ctx, tc.in); !errors.Is(err, ErrValidation) {
				t.Errorf("err = %v, want ErrValidation", err)
			}
		})
	}
}

func TestCreateTableAlreadyExists(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	in := CreateTableInput{TableName: "T", KeySchema: []KeySchemaElement{{"pk", "HASH"}}, AttributeDefinitions: []AttributeDefinition{{"pk", "S"}}}
	c.CreateTable(ctx, in)
	if _, err := c.CreateTable(ctx, in); !errors.Is(err, ErrTableInUse) {
		t.Errorf("err = %v, want ErrTableInUse", err)
	}
}

func TestDescribeTable(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	c.CreateTable(ctx, CreateTableInput{TableName: "T", KeySchema: []KeySchemaElement{{"pk", "HASH"}}, AttributeDefinitions: []AttributeDefinition{{"pk", "S"}}})

	desc, err := c.DescribeTable(ctx, DescribeTableInput{TableName: "T"})
	if err != nil {
		t.Fatalf("DescribeTable: %v", err)
	}
	if desc.Name != "T" || desc.Hash != "pk" || desc.HashType != "S" {
		t.Errorf("desc = %+v", desc)
	}
}

func TestDescribeTableMissing(t *testing.T) {
	c := newClient(t)
	_, err := c.DescribeTable(context.Background(), DescribeTableInput{TableName: "nope"})
	if !errors.Is(err, ErrTableNotFound) {
		t.Errorf("err = %v, want ErrTableNotFound", err)
	}
}

func TestListTables(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	for _, n := range []string{"alpha", "bravo", "charlie", "delta"} {
		c.CreateTable(ctx, CreateTableInput{TableName: n, KeySchema: []KeySchemaElement{{"pk", "HASH"}}, AttributeDefinitions: []AttributeDefinition{{"pk", "S"}}})
	}

	out, err := c.ListTables(ctx, ListTablesInput{Limit: 2})
	if err != nil {
		t.Fatalf("ListTables: %v", err)
	}
	if len(out.TableNames) != 2 || out.TableNames[0] != "alpha" || out.TableNames[1] != "bravo" {
		t.Errorf("page 1 = %+v", out)
	}
	if out.LastEvaluatedTableName != "bravo" {
		t.Errorf("LastEvaluated = %q, want bravo", out.LastEvaluatedTableName)
	}

	out2, _ := c.ListTables(ctx, ListTablesInput{ExclusiveStartTableName: out.LastEvaluatedTableName, Limit: 2})
	if len(out2.TableNames) != 2 || out2.TableNames[0] != "charlie" || out2.TableNames[1] != "delta" {
		t.Errorf("page 2 = %+v", out2)
	}
	if out2.LastEvaluatedTableName != "" {
		t.Errorf("LastEvaluated = %q, want empty (end)", out2.LastEvaluatedTableName)
	}
}

func TestListTablesDefaultLimit(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	out, err := c.ListTables(ctx, ListTablesInput{})
	if err != nil {
		t.Fatalf("ListTables: %v", err)
	}
	if out.TableNames != nil && len(out.TableNames) != 0 {
		t.Errorf("empty list = %+v, want empty/nil", out)
	}
}

func TestDeleteTable(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	c.CreateTable(ctx, CreateTableInput{TableName: "T", KeySchema: []KeySchemaElement{{"pk", "HASH"}}, AttributeDefinitions: []AttributeDefinition{{"pk", "S"}}})

	if err := c.DeleteTable(ctx, DeleteTableInput{TableName: "T"}); err != nil {
		t.Fatalf("DeleteTable: %v", err)
	}
	if _, err := c.DescribeTable(ctx, DescribeTableInput{TableName: "T"}); !errors.Is(err, ErrTableNotFound) {
		t.Errorf("after delete, DescribeTable err = %v, want ErrTableNotFound", err)
	}
}

func TestDeleteTableMissing(t *testing.T) {
	c := newClient(t)
	if err := c.DeleteTable(context.Background(), DeleteTableInput{TableName: "nope"}); !errors.Is(err, ErrTableNotFound) {
		t.Errorf("err = %v, want ErrTableNotFound", err)
	}
}
