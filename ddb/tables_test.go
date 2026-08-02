package ddb

import (
	"context"
	"errors"
	"testing"
	"time"
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

func TestCreateTableWithGSI(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	desc, err := c.CreateTable(ctx, CreateTableInput{
		TableName: "Music",
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
				IndexName: "gsi-all",
				KeySchema: []KeySchemaElement{
					{AttributeName: "gsi_pk", KeyType: "HASH"},
					{AttributeName: "gsi_sk", KeyType: "RANGE"},
				},
				Projection: Projection{Type: "ALL"},
			},
		},
	})
	if err != nil {
		t.Fatalf("CreateTable with GSI: %v", err)
	}
	if len(desc.GlobalSecondaryIndexes) != 1 {
		t.Fatalf("desc GSIs = %d, want 1", len(desc.GlobalSecondaryIndexes))
	}
	if desc.GlobalSecondaryIndexes[0].IndexName != "gsi-all" {
		t.Errorf("GSI name = %q, want gsi-all", desc.GlobalSecondaryIndexes[0].IndexName)
	}

	// DescribeTable returns the GSI too.
	d, err := c.DescribeTable(ctx, DescribeTableInput{TableName: "Music"})
	if err != nil {
		t.Fatalf("DescribeTable: %v", err)
	}
	if len(d.GlobalSecondaryIndexes) != 1 {
		t.Fatalf("describe GSIs = %d, want 1", len(d.GlobalSecondaryIndexes))
	}
}

func TestCreateTableDuplicateAttrDef(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	_, err := c.CreateTable(ctx, CreateTableInput{
		TableName: "T",
		KeySchema: []KeySchemaElement{{AttributeName: "pk", KeyType: "HASH"}},
		AttributeDefinitions: []AttributeDefinition{
			{AttributeName: "pk", AttributeType: "S"},
			{AttributeName: "pk", AttributeType: "S"},
		},
	})
	if !errors.Is(err, ErrValidation) {
		t.Errorf("duplicate AttributeDefinition: err = %v, want ErrValidation", err)
	}
}

func TestCreateTableGSIValidation(t *testing.T) {
	cases := []struct {
		name string
		gsi  GlobalSecondaryIndex
	}{
		{"bad name chars", GlobalSecondaryIndex{
			IndexName:  "bad name",
			KeySchema:  []KeySchemaElement{{AttributeName: "g", KeyType: "HASH"}},
			Projection: Projection{Type: "ALL"},
		}},
		{"name too short", GlobalSecondaryIndex{
			IndexName:  "ab",
			KeySchema:  []KeySchemaElement{{AttributeName: "g", KeyType: "HASH"}},
			Projection: Projection{Type: "ALL"},
		}},
		{"missing gsi attr def", GlobalSecondaryIndex{
			IndexName:  "ok-name",
			KeySchema:  []KeySchemaElement{{AttributeName: "nope", KeyType: "HASH"}},
			Projection: Projection{Type: "ALL"},
		}},
		{"ALL with NonKeyAttributes", GlobalSecondaryIndex{
			IndexName:  "ok-name",
			KeySchema:  []KeySchemaElement{{AttributeName: "g", KeyType: "HASH"}},
			Projection: Projection{Type: "ALL", NonKeyAttributes: []string{"x"}},
		}},
		{"INCLUDE names key attr", GlobalSecondaryIndex{
			IndexName:  "ok-name",
			KeySchema:  []KeySchemaElement{{AttributeName: "g", KeyType: "HASH"}},
			Projection: Projection{Type: "INCLUDE", NonKeyAttributes: []string{"g"}},
		}},
		{"two HASH", GlobalSecondaryIndex{
			IndexName: "ok-name",
			KeySchema: []KeySchemaElement{
				{AttributeName: "g", KeyType: "HASH"},
				{AttributeName: "h", KeyType: "HASH"},
			},
			Projection: Projection{Type: "ALL"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newClient(t)
			ctx := context.Background()
			_, err := c.CreateTable(ctx, CreateTableInput{
				TableName: "T",
				KeySchema: []KeySchemaElement{{AttributeName: "pk", KeyType: "HASH"}},
				AttributeDefinitions: []AttributeDefinition{
					{AttributeName: "pk", AttributeType: "S"},
					{AttributeName: "g", AttributeType: "S"},
					{AttributeName: "h", AttributeType: "S"},
				},
				GlobalSecondaryIndexes: []GlobalSecondaryIndex{tc.gsi},
			})
			if !errors.Is(err, ErrValidation) {
				t.Errorf("err = %v, want ErrValidation", err)
			}
		})
	}
}

func TestCreateTableGSIOverlappingKey(t *testing.T) {
	// GSI partition key = table partition key. Valid.
	c := newClient(t)
	ctx := context.Background()
	_, err := c.CreateTable(ctx, CreateTableInput{
		TableName: "T",
		KeySchema: []KeySchemaElement{{AttributeName: "pk", KeyType: "HASH"}},
		AttributeDefinitions: []AttributeDefinition{
			{AttributeName: "pk", AttributeType: "S"},
		},
		GlobalSecondaryIndexes: []GlobalSecondaryIndex{
			{
				IndexName:  "pk-index",
				KeySchema:  []KeySchemaElement{{AttributeName: "pk", KeyType: "HASH"}},
				Projection: Projection{Type: "KEYS_ONLY"},
			},
		},
	})
	if err != nil {
		t.Fatalf("overlapping key: %v", err)
	}
}

func TestInjectableClock(t *testing.T) {
	ctx := context.Background()

	// A fixed clock drives CreationTime.
	fixed := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	c, err := Open(ctx, Options{DSN: ":memory:", Now: func() time.Time { return fixed }})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	desc, err := c.CreateTable(ctx, CreateTableInput{
		TableName:            "T",
		KeySchema:            []KeySchemaElement{{AttributeName: "pk", KeyType: "HASH"}},
		AttributeDefinitions: []AttributeDefinition{{AttributeName: "pk", AttributeType: "S"}},
	})
	if err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	if !desc.CreationTime.Equal(fixed) {
		t.Errorf("CreationTime = %v, want %v", desc.CreationTime, fixed)
	}

	// Default (nil Now) uses time.Now.
	c2, err := Open(ctx, Options{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("Open default: %v", err)
	}
	t.Cleanup(func() { _ = c2.Close() })
	desc2, err := c2.CreateTable(ctx, CreateTableInput{
		TableName:            "T",
		KeySchema:            []KeySchemaElement{{AttributeName: "pk", KeyType: "HASH"}},
		AttributeDefinitions: []AttributeDefinition{{AttributeName: "pk", AttributeType: "S"}},
	})
	if err != nil {
		t.Fatalf("CreateTable default: %v", err)
	}
	if desc2.CreationTime.IsZero() {
		t.Error("default clock: CreationTime not set")
	}
}
