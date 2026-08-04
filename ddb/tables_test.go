package ddb

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/quells-bot/ddb-sqlite-core/attrval"
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
		{"missing hash", CreateTableInput{TableName: "Tbl", KeySchema: []KeySchemaElement{{"sk", "RANGE"}}, AttributeDefinitions: []AttributeDefinition{{"sk", "S"}}}},
		{"bad key type", CreateTableInput{TableName: "Tbl", KeySchema: []KeySchemaElement{{"pk", "HASH"}}, AttributeDefinitions: []AttributeDefinition{{"pk", "X"}}}},
		{"uncovered key", CreateTableInput{TableName: "Tbl", KeySchema: []KeySchemaElement{{"pk", "HASH"}}, AttributeDefinitions: []AttributeDefinition{{"other", "S"}}}},
		{"extra hash key", CreateTableInput{TableName: "Tbl", KeySchema: []KeySchemaElement{{"pk", "HASH"}, {"pk2", "HASH"}}, AttributeDefinitions: []AttributeDefinition{{"pk", "S"}, {"pk2", "S"}}}},
		{"same attr as HASH and RANGE", CreateTableInput{TableName: "Tbl", KeySchema: []KeySchemaElement{{"pk", "HASH"}, {"pk", "RANGE"}}, AttributeDefinitions: []AttributeDefinition{{"pk", "S"}}}},
		{"two RANGE keys", CreateTableInput{TableName: "Tbl", KeySchema: []KeySchemaElement{{"pk", "RANGE"}, {"sk", "RANGE"}}, AttributeDefinitions: []AttributeDefinition{{"pk", "S"}, {"sk", "S"}}}},
		{"three key schema elements", CreateTableInput{TableName: "Tbl", KeySchema: []KeySchemaElement{{"pk", "HASH"}, {"sk", "RANGE"}, {"sk2", "RANGE"}}, AttributeDefinitions: []AttributeDefinition{{"pk", "S"}, {"sk", "S"}, {"sk2", "S"}}}},
		{"extra AttributeDefinition not in key schema", CreateTableInput{TableName: "Tbl", KeySchema: []KeySchemaElement{{"pk", "HASH"}}, AttributeDefinitions: []AttributeDefinition{{"pk", "S"}, {"extra", "S"}}}},
		{"table name too short", CreateTableInput{TableName: "Us", KeySchema: []KeySchemaElement{{"pk", "HASH"}}, AttributeDefinitions: []AttributeDefinition{{"pk", "S"}}}},
		{"table name too long", CreateTableInput{TableName: strings.Repeat("t", 256), KeySchema: []KeySchemaElement{{"pk", "HASH"}}, AttributeDefinitions: []AttributeDefinition{{"pk", "S"}}}},
		{"table name with space", CreateTableInput{TableName: "bad name", KeySchema: []KeySchemaElement{{"pk", "HASH"}}, AttributeDefinitions: []AttributeDefinition{{"pk", "S"}}}},
		{"table name with bang", CreateTableInput{TableName: "bad!name", KeySchema: []KeySchemaElement{{"pk", "HASH"}}, AttributeDefinitions: []AttributeDefinition{{"pk", "S"}}}},
		{"table name with slash", CreateTableInput{TableName: "bad/name", KeySchema: []KeySchemaElement{{"pk", "HASH"}}, AttributeDefinitions: []AttributeDefinition{{"pk", "S"}}}},
		{"table name non-ascii", CreateTableInput{TableName: "táblé", KeySchema: []KeySchemaElement{{"pk", "HASH"}}, AttributeDefinitions: []AttributeDefinition{{"pk", "S"}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := c.CreateTable(ctx, tc.in); !errors.Is(err, ErrValidation) {
				t.Errorf("err = %v, want ErrValidation", err)
			}
		})
	}
}

func TestCreateTableNameBoundaries(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	for _, name := range []string{"abc", strings.Repeat("t", 255), "with.dot_and-dash123"} {
		if _, err := c.CreateTable(ctx, CreateTableInput{
			TableName:            name,
			KeySchema:            []KeySchemaElement{{AttributeName: "pk", KeyType: "HASH"}},
			AttributeDefinitions: []AttributeDefinition{{AttributeName: "pk", AttributeType: "S"}},
		}); err != nil {
			t.Errorf("CreateTable %q: %v", name, err)
		}
	}
}

func TestCreateTableAlreadyExists(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	in := CreateTableInput{TableName: "Tbl", KeySchema: []KeySchemaElement{{"pk", "HASH"}}, AttributeDefinitions: []AttributeDefinition{{"pk", "S"}}}
	c.CreateTable(ctx, in)
	if _, err := c.CreateTable(ctx, in); !errors.Is(err, ErrTableInUse) {
		t.Errorf("err = %v, want ErrTableInUse", err)
	}
}

func TestDescribeTable(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	c.CreateTable(ctx, CreateTableInput{TableName: "Tbl", KeySchema: []KeySchemaElement{{"pk", "HASH"}}, AttributeDefinitions: []AttributeDefinition{{"pk", "S"}}})

	desc, err := c.DescribeTable(ctx, DescribeTableInput{TableName: "Tbl"})
	if err != nil {
		t.Fatalf("DescribeTable: %v", err)
	}
	if desc.Name != "Tbl" || desc.Hash != "pk" || desc.HashType != "S" {
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
	c.CreateTable(ctx, CreateTableInput{TableName: "Tbl", KeySchema: []KeySchemaElement{{"pk", "HASH"}}, AttributeDefinitions: []AttributeDefinition{{"pk", "S"}}})

	if err := c.DeleteTable(ctx, DeleteTableInput{TableName: "Tbl"}); err != nil {
		t.Fatalf("DeleteTable: %v", err)
	}
	if _, err := c.DescribeTable(ctx, DescribeTableInput{TableName: "Tbl"}); !errors.Is(err, ErrTableNotFound) {
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
		TableName: "Tbl",
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
				TableName: "Tbl",
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
		TableName: "Tbl",
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
		TableName:            "Tbl",
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
		TableName:            "Tbl",
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

func TestEmptyTableNameRejected(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()

	check := func(op string, err error) {
		t.Helper()
		if !errors.Is(err, ErrValidation) {
			t.Errorf("%s: err = %v, want ErrValidation", op, err)
		}
	}

	_, err := c.CreateTable(ctx, CreateTableInput{KeySchema: []KeySchemaElement{{"pk", "HASH"}}, AttributeDefinitions: []AttributeDefinition{{"pk", "S"}}})
	check("CreateTable", err)
	_, err = c.DescribeTable(ctx, DescribeTableInput{})
	check("DescribeTable", err)
	check("DeleteTable", c.DeleteTable(ctx, DeleteTableInput{}))
	_, err = c.PutItem(ctx, PutItemInput{Item: Item{"pk": attrval.NewString("k")}})
	check("PutItem", err)
	_, err = c.GetItem(ctx, GetItemInput{Key: Item{"pk": attrval.NewString("k")}})
	check("GetItem", err)
	_, err = c.DeleteItem(ctx, DeleteItemInput{Key: Item{"pk": attrval.NewString("k")}})
	check("DeleteItem", err)
	_, err = c.UpdateItem(ctx, UpdateItemInput{Key: Item{"pk": attrval.NewString("k")}})
	check("UpdateItem", err)
	_, err = c.Query(ctx, QueryInput{KeyConditionExpression: "pk = :pk"})
	check("Query", err)
	_, err = c.Scan(ctx, ScanInput{})
	check("Scan", err)
	_, err = c.UpdateTable(ctx, UpdateTableInput{NonGsiFieldsPresent: true})
	check("UpdateTable", err)
	_, err = c.UpdateTimeToLive(ctx, UpdateTimeToLiveInput{TimeToLiveSpecification: TimeToLiveSpecification{Enabled: true, AttributeName: "ttl"}})
	check("UpdateTimeToLive", err)
	_, err = c.DescribeTimeToLive(ctx, DescribeTimeToLiveInput{})
	check("DescribeTimeToLive", err)
	_, err = c.ExpireExpired(ctx, "")
	check("ExpireExpired", err)
	_, err = c.BatchWriteItem(ctx, BatchWriteItemInput{RequestItems: map[string][]WriteRequest{"": {{Put: &PutRequest{Item: Item{"pk": attrval.NewString("k")}}}}}})
	check("BatchWriteItem", err)
	_, err = c.BatchGetItem(ctx, BatchGetItemInput{RequestItems: map[string]KeysAndAttributes{"": {Keys: []Item{{"pk": attrval.NewString("k")}}}}})
	check("BatchGetItem", err)
}

func TestIndexAttrNameLengths(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()

	gsiWithHash := func(attrName string) GlobalSecondaryIndex {
		return GlobalSecondaryIndex{
			IndexName:  "gsi1",
			KeySchema:  []KeySchemaElement{{AttributeName: attrName, KeyType: "HASH"}},
			Projection: Projection{Type: "ALL"},
		}
	}
	base := func(name string, g GlobalSecondaryIndex, extraAttr string) CreateTableInput {
		ads := []AttributeDefinition{{AttributeName: "pk", AttributeType: "S"}}
		if extraAttr != "" {
			ads = append(ads, AttributeDefinition{AttributeName: extraAttr, AttributeType: "S"})
		}
		return CreateTableInput{
			TableName:              name,
			KeySchema:              []KeySchemaElement{{AttributeName: "pk", KeyType: "HASH"}},
			AttributeDefinitions:   ads,
			GlobalSecondaryIndexes: []GlobalSecondaryIndex{g},
		}
	}

	// GSI key attribute name: 255 accepted, 256 rejected (distinct table names
	// so the rejected case isn't masked by ErrTableInUse on the accepted one).
	name255 := strings.Repeat("g", 255)
	if _, err := c.CreateTable(ctx, base("Idx255", gsiWithHash(name255), name255)); err != nil {
		t.Errorf("gsi key attr 255: %v", err)
	}
	name256 := strings.Repeat("g", 256)
	if _, err := c.CreateTable(ctx, base("Idx256", gsiWithHash(name256), name256)); !errors.Is(err, ErrValidation) {
		t.Errorf("gsi key attr 256: err = %v, want ErrValidation", err)
	}

	// INCLUDE NonKeyAttributes name: 255 accepted, 256 rejected.
	incl := func(attrs ...string) GlobalSecondaryIndex {
		return GlobalSecondaryIndex{
			IndexName:  "gsi2",
			KeySchema:  []KeySchemaElement{{AttributeName: "pk", KeyType: "HASH"}},
			Projection: Projection{Type: "INCLUDE", NonKeyAttributes: attrs},
		}
	}
	inclBase := func(name string, g GlobalSecondaryIndex) CreateTableInput {
		return CreateTableInput{
			TableName:              name,
			KeySchema:              []KeySchemaElement{{AttributeName: "pk", KeyType: "HASH"}},
			AttributeDefinitions:   []AttributeDefinition{{AttributeName: "pk", AttributeType: "S"}},
			GlobalSecondaryIndexes: []GlobalSecondaryIndex{g},
		}
	}
	if _, err := c.CreateTable(ctx, inclBase("Incl255", incl(strings.Repeat("a", 255)))); err != nil {
		t.Errorf("include attr 255: %v", err)
	}
	if _, err := c.CreateTable(ctx, inclBase("Incl256", incl(strings.Repeat("a", 256)))); !errors.Is(err, ErrValidation) {
		t.Errorf("include attr 256: err = %v, want ErrValidation", err)
	}
}

func TestCrossIndexProjectionSum(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()

	// attrs generates n distinct non-key attribute names.
	attrs := func(prefix string, n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("%s%03d", prefix, i)
		}
		return out
	}
	inclGsi := func(name string, projected []string) GlobalSecondaryIndex {
		return GlobalSecondaryIndex{
			IndexName:  name,
			KeySchema:  []KeySchemaElement{{AttributeName: "pk", KeyType: "HASH"}},
			Projection: Projection{Type: "INCLUDE", NonKeyAttributes: projected},
		}
	}
	base := func(table string, gsis ...GlobalSecondaryIndex) CreateTableInput {
		return CreateTableInput{
			TableName:              table,
			KeySchema:              []KeySchemaElement{{AttributeName: "pk", KeyType: "HASH"}},
			AttributeDefinitions:   []AttributeDefinition{{AttributeName: "pk", AttributeType: "S"}},
			GlobalSecondaryIndexes: gsis,
		}
	}

	// 50 + 50 = 100: accepted.
	if _, err := c.CreateTable(ctx, base("SumOk", inclGsi("sumg1", attrs("a", 50)), inclGsi("sumg2", attrs("b", 50)))); err != nil {
		t.Errorf("sum 100: %v", err)
	}
	// 51 + 50 = 101: rejected.
	if _, err := c.CreateTable(ctx, base("SumOver", inclGsi("sumg1", attrs("a", 51)), inclGsi("sumg2", attrs("b", 50)))); !errors.Is(err, ErrValidation) {
		t.Errorf("sum 101: err = %v, want ErrValidation", err)
	}
	// 99 INCLUDE attrs + table/GSI key attrs: accepted (key attrs do not count).
	if _, err := c.CreateTable(ctx, base("SumKeys", inclGsi("sumg1", attrs("a", 99)))); err != nil {
		t.Errorf("sum 99 + keys: %v", err)
	}
}

func TestDescribeTableStats(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	c.CreateTable(ctx, CreateTableInput{
		TableName:            "Tbl",
		KeySchema:            []KeySchemaElement{{AttributeName: "pk", KeyType: "HASH"}},
		AttributeDefinitions: []AttributeDefinition{{AttributeName: "pk", AttributeType: "S"}},
	})
	// Empty table: 0/0.
	desc, err := c.DescribeTable(ctx, DescribeTableInput{TableName: "Tbl"})
	if err != nil {
		t.Fatalf("DescribeTable empty: %v", err)
	}
	if desc.ItemCount != 0 || desc.TableSizeBytes != 0 {
		t.Errorf("empty stats = (%d, %d), want (0, 0)", desc.ItemCount, desc.TableSizeBytes)
	}
	// {pk:k1,gp:G1}=8, {pk:k2,gp:G1}=8.
	c.PutItem(ctx, PutItemInput{TableName: "Tbl", Item: Item{"pk": attrval.NewString("k1"), "gp": attrval.NewString("G1")}})
	c.PutItem(ctx, PutItemInput{TableName: "Tbl", Item: Item{"pk": attrval.NewString("k2"), "gp": attrval.NewString("G1")}})
	desc, _ = c.DescribeTable(ctx, DescribeTableInput{TableName: "Tbl"})
	if desc.ItemCount != 2 || desc.TableSizeBytes != 16 {
		t.Errorf("after puts = (%d, %d), want (2, 16)", desc.ItemCount, desc.TableSizeBytes)
	}
	// Overwrite k1 larger: {pk:k1,gp:G1,extra:hello}=18; total 18+8=26.
	c.PutItem(ctx, PutItemInput{TableName: "Tbl", Item: Item{"pk": attrval.NewString("k1"), "gp": attrval.NewString("G1"), "extra": attrval.NewString("hello")}})
	desc, _ = c.DescribeTable(ctx, DescribeTableInput{TableName: "Tbl"})
	if desc.ItemCount != 2 || desc.TableSizeBytes != 26 {
		t.Errorf("after overwrite = (%d, %d), want (2, 26)", desc.ItemCount, desc.TableSizeBytes)
	}
	// Delete k2: remaining {pk:k1,gp:G1,extra:hello}=18.
	c.DeleteItem(ctx, DeleteItemInput{TableName: "Tbl", Key: Item{"pk": attrval.NewString("k2")}})
	desc, _ = c.DescribeTable(ctx, DescribeTableInput{TableName: "Tbl"})
	if desc.ItemCount != 1 || desc.TableSizeBytes != 18 {
		t.Errorf("after delete = (%d, %d), want (1, 18)", desc.ItemCount, desc.TableSizeBytes)
	}
}

func TestDescribeTableGsiStats(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	c.CreateTable(ctx, CreateTableInput{
		TableName:            "Tbl",
		KeySchema:            []KeySchemaElement{{AttributeName: "pk", KeyType: "HASH"}},
		AttributeDefinitions: []AttributeDefinition{{AttributeName: "pk", AttributeType: "S"}, {AttributeName: "gp", AttributeType: "S"}},
		GlobalSecondaryIndexes: []GlobalSecondaryIndex{
			{IndexName: "g-all", KeySchema: []KeySchemaElement{{AttributeName: "gp", KeyType: "HASH"}}, Projection: Projection{Type: "ALL"}},
			{IndexName: "g-keys", KeySchema: []KeySchemaElement{{AttributeName: "gp", KeyType: "HASH"}}, Projection: Projection{Type: "KEYS_ONLY"}},
		},
	})
	// {pk:a,gp:G1}=3+4=7, {pk:bb,gp:G1}=4+4=8, {pk:ccc}=5 (sparse).
	c.PutItem(ctx, PutItemInput{TableName: "Tbl", Item: Item{"pk": attrval.NewString("a"), "gp": attrval.NewString("G1")}})
	c.PutItem(ctx, PutItemInput{TableName: "Tbl", Item: Item{"pk": attrval.NewString("bb"), "gp": attrval.NewString("G1")}})
	c.PutItem(ctx, PutItemInput{TableName: "Tbl", Item: Item{"pk": attrval.NewString("ccc")}})
	desc, err := c.DescribeTable(ctx, DescribeTableInput{TableName: "Tbl"})
	if err != nil {
		t.Fatalf("DescribeTable: %v", err)
	}
	// Table: 3 items, 7+8+5=20 bytes.
	if desc.ItemCount != 3 || desc.TableSizeBytes != 20 {
		t.Errorf("table = (%d, %d), want (3, 20)", desc.ItemCount, desc.TableSizeBytes)
	}
	// Each GSI: 2 indexed (ccc sparse), 7+8=15 bytes (projection-independent).
	for _, g := range desc.GlobalSecondaryIndexes {
		if g.ItemCount != 2 {
			t.Errorf("GSI %q count = %d, want 2", g.IndexName, g.ItemCount)
		}
		if g.IndexSizeBytes != 15 {
			t.Errorf("GSI %q size = %d, want 15", g.IndexName, g.IndexSizeBytes)
		}
	}
}
