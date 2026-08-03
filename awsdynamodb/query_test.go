package awsdynamodb_test

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/expression"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/quells-bot/ddb-sqlite-core/awsdynamodb"
)

// createCompositeTable creates a table with a composite (pk HASH S, sk RANGE N)
// primary key, which Query requires for ordered results.
func createCompositeTable(t *testing.T, a *awsdynamodb.Adapter, ctx context.Context, name string) {
	t.Helper()
	_, err := a.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName: aws.String(name),
		KeySchema: []types.KeySchemaElement{
			{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash},
			{AttributeName: aws.String("sk"), KeyType: types.KeyTypeRange},
		},
		AttributeDefinitions: []types.AttributeDefinition{
			{AttributeName: aws.String("pk"), AttributeType: types.ScalarAttributeTypeS},
			{AttributeName: aws.String("sk"), AttributeType: types.ScalarAttributeTypeN},
		},
		BillingMode: types.BillingModePayPerRequest,
	})
	if err != nil {
		t.Fatalf("CreateTable %q: %v", name, err)
	}
}

func TestAdapterQueryBasic(t *testing.T) {
	ctx := context.Background()
	a, err := awsdynamodb.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer a.Close()

	createCompositeTable(t, a, ctx, "QT")

	// Seed items with sk 0..4.
	for i := range 5 {
		putConf(t, a, ctx, "QT", map[string]types.AttributeValue{
			"pk":  strVal("p1"),
			"sk":  numVal(string(rune('0' + i))),
			"val": strVal("data"),
		})
	}

	keyExpr := expression.Key("pk").Equal(expression.Value("p1"))
	expr := mustExpr(t, expression.NewBuilder().WithKeyCondition(keyExpr))
	out, err := a.Query(ctx, &dynamodb.QueryInput{
		TableName:                 aws.String("QT"),
		KeyConditionExpression:    expr.KeyCondition(),
		ExpressionAttributeNames:  expr.Names(),
		ExpressionAttributeValues: expr.Values(),
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if out.Count != 5 {
		t.Errorf("Count = %d, want 5", out.Count)
	}
	if out.ScannedCount != 5 {
		t.Errorf("ScannedCount = %d, want 5", out.ScannedCount)
	}
	if len(out.Items) != 5 {
		t.Fatalf("len(Items) = %d, want 5", len(out.Items))
	}
	if out.Items[0]["sk"].(*types.AttributeValueMemberN).Value != "0" {
		t.Errorf("Items[0].sk = %v, want 0 (ASC default)", out.Items[0]["sk"])
	}
}

func TestAdapterQueryLimitZero(t *testing.T) {
	ctx := context.Background()
	a, err := awsdynamodb.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer a.Close()
	mustCreate(t, a, ctx, "T")

	_, err = a.Query(ctx, &dynamodb.QueryInput{
		TableName:                 aws.String("T"),
		KeyConditionExpression:    aws.String("pk = :v"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":v": strVal("x")},
		Limit:                     aws.Int32(0),
	})
	asValidation(t, err, "Limit=0 should be rejected")
}

func TestAdapterQueryEmptyKeyCondition(t *testing.T) {
	ctx := context.Background()
	a, err := awsdynamodb.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer a.Close()
	mustCreate(t, a, ctx, "T")

	_, err = a.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String("T"),
		KeyConditionExpression: aws.String(""),
	})
	asValidation(t, err, "empty KeyConditionExpression should be rejected")
}

func TestAdapterQueryLegacyKeyConditions(t *testing.T) {
	ctx := context.Background()
	a, err := awsdynamodb.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer a.Close()
	mustCreate(t, a, ctx, "T")

	_, err = a.Query(ctx, &dynamodb.QueryInput{
		TableName:     aws.String("T"),
		KeyConditions: map[string]types.Condition{"pk": {}},
	})
	asValidation(t, err, "legacy KeyConditions should be rejected")
}

func TestAdapterQueryScanIndexForwardDefaultNil(t *testing.T) {
	// A nil ScanIndexForward must default to ASC (true) without panicking: a
	// query over sk 0..4 returns sk "0" first.
	ctx := context.Background()
	a, err := awsdynamodb.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer a.Close()
	createCompositeTable(t, a, ctx, "QT")

	for i := range 5 {
		putConf(t, a, ctx, "QT", map[string]types.AttributeValue{
			"pk": strVal("p1"),
			"sk": numVal(string(rune('0' + i))),
		})
	}

	keyExpr := expression.Key("pk").Equal(expression.Value("p1"))
	expr := mustExpr(t, expression.NewBuilder().WithKeyCondition(keyExpr))
	out, err := a.Query(ctx, &dynamodb.QueryInput{
		TableName:                 aws.String("QT"),
		KeyConditionExpression:    expr.KeyCondition(),
		ExpressionAttributeNames:  expr.Names(),
		ExpressionAttributeValues: expr.Values(),
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if out.Count != 5 {
		t.Fatalf("Count = %d, want 5", out.Count)
	}
	sk := out.Items[0]["sk"].(*types.AttributeValueMemberN).Value
	if sk != "0" {
		t.Errorf("Items[0].sk = %q, want %q (nil ScanIndexForward must default to ASC)", sk, "0")
	}
}

func TestAdapterScanBasic(t *testing.T) {
	ctx := context.Background()
	a, err := awsdynamodb.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer a.Close()
	mustCreate(t, a, ctx, "T")

	for i := range 3 {
		putConf(t, a, ctx, "T", map[string]types.AttributeValue{
			"pk": strVal("p" + string(rune('a'+i))),
		})
	}

	out, err := a.Scan(ctx, &dynamodb.ScanInput{TableName: aws.String("T")})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if out.Count != 3 {
		t.Errorf("Count = %d, want 3", out.Count)
	}
	if out.ScannedCount != 3 {
		t.Errorf("ScannedCount = %d, want 3", out.ScannedCount)
	}
	if len(out.Items) != 3 {
		t.Errorf("len(Items) = %d, want 3", len(out.Items))
	}
}

func TestAdapterScanLegacyScanFilter(t *testing.T) {
	ctx := context.Background()
	a, err := awsdynamodb.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer a.Close()
	mustCreate(t, a, ctx, "T")

	_, err = a.Scan(ctx, &dynamodb.ScanInput{
		TableName:  aws.String("T"),
		ScanFilter: map[string]types.Condition{"pk": {}},
	})
	asValidation(t, err, "legacy ScanFilter should be rejected")
}

func TestAdapterCreateTableWithGSI(t *testing.T) {
	ctx := context.Background()
	a, cleanup := newAdapterTarget(t)
	defer cleanup()
	_, err := a.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName: aws.String("GT"),
		KeySchema: []types.KeySchemaElement{
			{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash},
			{AttributeName: aws.String("sk"), KeyType: types.KeyTypeRange},
		},
		AttributeDefinitions: []types.AttributeDefinition{
			{AttributeName: aws.String("pk"), AttributeType: types.ScalarAttributeTypeS},
			{AttributeName: aws.String("sk"), AttributeType: types.ScalarAttributeTypeS},
			{AttributeName: aws.String("gpk"), AttributeType: types.ScalarAttributeTypeS},
		},
		GlobalSecondaryIndexes: []types.GlobalSecondaryIndex{{
			IndexName:  aws.String("gsi1"),
			KeySchema:  []types.KeySchemaElement{{AttributeName: aws.String("gpk"), KeyType: types.KeyTypeHash}},
			Projection: &types.Projection{ProjectionType: types.ProjectionTypeAll},
		}},
		BillingMode: types.BillingModePayPerRequest,
	})
	if err != nil {
		t.Fatalf("CreateTable with GSI: %v", err)
	}
	// Query with IndexName should now succeed (not rejected).
	a.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String("GT"),
		Item: map[string]types.AttributeValue{
			"pk":  &types.AttributeValueMemberS{Value: "A"},
			"sk":  &types.AttributeValueMemberS{Value: "a"},
			"gpk": &types.AttributeValueMemberS{Value: "G1"},
		},
	})
	keyExpr := expression.Key("gpk").Equal(expression.Value("G1"))
	expr := mustExpr(t, expression.NewBuilder().WithKeyCondition(keyExpr))
	out, err := a.Query(ctx, &dynamodb.QueryInput{
		TableName:                 aws.String("GT"),
		IndexName:                 aws.String("gsi1"),
		KeyConditionExpression:    expr.KeyCondition(),
		ExpressionAttributeNames:  expr.Names(),
		ExpressionAttributeValues: expr.Values(),
	})
	if err != nil {
		t.Fatalf("Query with IndexName: %v", err)
	}
	if len(out.Items) != 1 {
		t.Errorf("got %d items, want 1", len(out.Items))
	}
}
