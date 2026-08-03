package awsdynamodb_test

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/smithy-go"

	"github.com/quells-bot/ddb-sqlite/awsdynamodb"
)

func TestOpenClose(t *testing.T) {
	a, err := awsdynamodb.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if a == nil {
		t.Fatal("nil adapter")
	}
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func adapterClient(t *testing.T) *awsdynamodb.Adapter {
	t.Helper()
	a, err := awsdynamodb.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

func TestAdapterCreateDescribePutGet(t *testing.T) {
	ctx := context.Background()
	a := adapterClient(t)

	_, err := a.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:            aws.String("Users"),
		KeySchema:            []types.KeySchemaElement{{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash}},
		AttributeDefinitions: []types.AttributeDefinition{{AttributeName: aws.String("pk"), AttributeType: types.ScalarAttributeTypeS}},
	})
	if err != nil {
		t.Fatalf("CreateTable: %v", err)
	}

	out, err := a.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String("Users")})
	if err != nil {
		t.Fatalf("DescribeTable: %v", err)
	}
	if aws.ToString(out.Table.TableName) != "Users" {
		t.Errorf("TableName = %q", aws.ToString(out.Table.TableName))
	}

	_, err = a.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String("Users"),
		Item:      map[string]types.AttributeValue{"pk": &types.AttributeValueMemberS{Value: "u1"}, "v": &types.AttributeValueMemberS{Value: "hi"}},
	})
	if err != nil {
		t.Fatalf("PutItem: %v", err)
	}

	got, err := a.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String("Users"), Key: map[string]types.AttributeValue{"pk": &types.AttributeValueMemberS{Value: "u1"}}})
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if got.Item == nil {
		t.Fatal("nil Item")
	}
	if got.Item["v"].(*types.AttributeValueMemberS).Value != "hi" {
		t.Errorf("v = %q", got.Item["v"].(*types.AttributeValueMemberS).Value)
	}
}

func TestAdapterErrorMapping(t *testing.T) {
	ctx := context.Background()
	a := adapterClient(t)

	_, err := a.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String("missing")})
	var rnfe *types.ResourceNotFoundException
	if !errors.As(err, &rnfe) {
		t.Errorf("DescribeTable missing: err = %v, want ResourceNotFoundException", err)
	}

	_, err = a.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String("missing"), Item: map[string]types.AttributeValue{"pk": &types.AttributeValueMemberS{Value: "k"}}})
	if !errors.As(err, &rnfe) {
		t.Errorf("PutItem missing table: err = %v, want ResourceNotFoundException", err)
	}
}

func TestAdapterGetItemMissingKey(t *testing.T) {
	ctx := context.Background()
	a := adapterClient(t)
	a.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:            aws.String("T"),
		KeySchema:            []types.KeySchemaElement{{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash}},
		AttributeDefinitions: []types.AttributeDefinition{{AttributeName: aws.String("pk"), AttributeType: types.ScalarAttributeTypeS}},
	})
	got, err := a.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String("T"), Key: map[string]types.AttributeValue{"pk": &types.AttributeValueMemberS{Value: "nope"}}})
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if got.Item != nil {
		t.Errorf("missing key Item = %v, want nil", got.Item)
	}
}

func TestAdapterRejectsLegacyParameters(t *testing.T) {
	ctx := context.Background()
	a, err := awsdynamodb.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer a.Close()
	mustAdapterTable(t, a, ctx, "T")

	item := map[string]types.AttributeValue{"pk": &types.AttributeValueMemberS{Value: "k"}}

	t.Run("PutItem Expected", func(t *testing.T) {
		_, err := a.PutItem(ctx, &dynamodb.PutItemInput{
			TableName: aws.String("T"),
			Item:      item,
			Expected:  map[string]types.ExpectedAttributeValue{"v": {Exists: aws.Bool(false)}},
		})
		assertValidation(t, err)
	})

	t.Run("PutItem ConditionalOperator", func(t *testing.T) {
		_, err := a.PutItem(ctx, &dynamodb.PutItemInput{
			TableName:           aws.String("T"),
			Item:                item,
			ConditionalOperator: types.ConditionalOperatorAnd,
		})
		assertValidation(t, err)
	})

	t.Run("DeleteItem Expected", func(t *testing.T) {
		_, err := a.DeleteItem(ctx, &dynamodb.DeleteItemInput{
			TableName: aws.String("T"),
			Key:       item,
			Expected:  map[string]types.ExpectedAttributeValue{"v": {Exists: aws.Bool(false)}},
		})
		assertValidation(t, err)
	})
}

func TestAdapterRejectsEmptyConditionExpression(t *testing.T) {
	ctx := context.Background()
	a, err := awsdynamodb.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer a.Close()
	mustAdapterTable(t, a, ctx, "T")

	_, err = a.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:           aws.String("T"),
		Item:                map[string]types.AttributeValue{"pk": &types.AttributeValueMemberS{Value: "k"}},
		ConditionExpression: aws.String(""),
	})
	assertValidation(t, err)
}

func TestAdapterConditionalCheckFailedCarriesItem(t *testing.T) {
	ctx := context.Background()
	a, err := awsdynamodb.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer a.Close()
	mustAdapterTable(t, a, ctx, "T")

	if _, err := a.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String("T"),
		Item: map[string]types.AttributeValue{
			"pk": &types.AttributeValueMemberS{Value: "k"},
			"v":  &types.AttributeValueMemberS{Value: "first"},
		},
	}); err != nil {
		t.Fatalf("seed PutItem: %v", err)
	}

	_, err = a.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:                           aws.String("T"),
		Item:                                map[string]types.AttributeValue{"pk": &types.AttributeValueMemberS{Value: "k"}},
		ConditionExpression:                 aws.String("attribute_not_exists(pk)"),
		ReturnValuesOnConditionCheckFailure: types.ReturnValuesOnConditionCheckFailureAllOld,
	})
	var ccf *types.ConditionalCheckFailedException
	if !errors.As(err, &ccf) {
		t.Fatalf("err = %v, want ConditionalCheckFailedException", err)
	}
	got, ok := ccf.Item["v"].(*types.AttributeValueMemberS)
	if !ok || got.Value != "first" {
		t.Errorf("exception Item = %v, want v=first", ccf.Item)
	}
}

func mustAdapterTable(t *testing.T, a *awsdynamodb.Adapter, ctx context.Context, name string) {
	t.Helper()
	if _, err := a.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:            aws.String(name),
		KeySchema:            []types.KeySchemaElement{{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash}},
		AttributeDefinitions: []types.AttributeDefinition{{AttributeName: aws.String("pk"), AttributeType: types.ScalarAttributeTypeS}},
		BillingMode:          types.BillingModePayPerRequest,
	}); err != nil {
		t.Fatalf("CreateTable %q: %v", name, err)
	}
}

func assertValidation(t *testing.T, err error) {
	t.Helper()
	var ae smithy.APIError
	if !errors.As(err, &ae) || ae.ErrorCode() != "ValidationException" {
		t.Errorf("err = %v, want ValidationException", err)
	}
}

// newAdapterTable opens an adapter and creates a single-key table "T".
// adapterClient is the existing helper in this file; table creation is inline
// elsewhere, so this wrapper is new.
func newAdapterTable(t *testing.T) (*awsdynamodb.Adapter, context.Context) {
	t.Helper()
	a := adapterClient(t)
	ctx := context.Background()
	if _, err := a.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:            aws.String("T"),
		KeySchema:            []types.KeySchemaElement{{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash}},
		AttributeDefinitions: []types.AttributeDefinition{{AttributeName: aws.String("pk"), AttributeType: types.ScalarAttributeTypeS}},
	}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	return a, ctx
}

func TestAdapterUpdateItem(t *testing.T) {
	a, ctx := newAdapterTable(t)

	out, err := a.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                 aws.String("T"),
		Key:                       map[string]types.AttributeValue{"pk": &types.AttributeValueMemberS{Value: "k"}},
		UpdateExpression:          aws.String("SET s = :s"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":s": &types.AttributeValueMemberS{Value: "v"}},
		ReturnValues:              types.ReturnValueAllNew,
	})
	if err != nil {
		t.Fatalf("UpdateItem: %v", err)
	}
	if got := out.Attributes["s"].(*types.AttributeValueMemberS).Value; got != "v" {
		t.Errorf("Attributes[s] = %q, want v", got)
	}
	if _, ok := out.Attributes["pk"]; !ok {
		t.Error("ALL_NEW must include the key attributes")
	}
}

func TestAdapterUpdateItemRejections(t *testing.T) {
	a, ctx := newAdapterTable(t)
	key := map[string]types.AttributeValue{"pk": &types.AttributeValueMemberS{Value: "k"}}

	cases := []struct {
		name  string
		input *dynamodb.UpdateItemInput
	}{
		{
			"legacy AttributeUpdates",
			&dynamodb.UpdateItemInput{
				TableName:        aws.String("T"),
				Key:              key,
				AttributeUpdates: map[string]types.AttributeValueUpdate{"s": {Action: types.AttributeActionPut, Value: &types.AttributeValueMemberS{Value: "v"}}},
			},
		},
		{
			"legacy Expected",
			&dynamodb.UpdateItemInput{
				TableName: aws.String("T"),
				Key:       key,
				Expected:  map[string]types.ExpectedAttributeValue{"s": {Exists: aws.Bool(false)}},
			},
		},
		{
			"legacy ConditionalOperator",
			&dynamodb.UpdateItemInput{
				TableName:           aws.String("T"),
				Key:                 key,
				ConditionalOperator: types.ConditionalOperatorAnd,
			},
		},
		{
			"present-but-empty UpdateExpression",
			&dynamodb.UpdateItemInput{TableName: aws.String("T"), Key: key, UpdateExpression: aws.String("")},
		},
		{
			"present-but-empty ConditionExpression",
			&dynamodb.UpdateItemInput{TableName: aws.String("T"), Key: key, ConditionExpression: aws.String("")},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := a.UpdateItem(ctx, tc.input)
			var ae smithy.APIError
			if !errors.As(err, &ae) || ae.ErrorCode() != "ValidationException" {
				t.Errorf("err = %v, want ValidationException", err)
			}
		})
	}
}

func TestAdapterUpdateTimeToLive(t *testing.T) {
	ctx := context.Background()
	a := adapterClient(t)
	a.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:            aws.String("T"),
		KeySchema:            []types.KeySchemaElement{{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash}},
		AttributeDefinitions: []types.AttributeDefinition{{AttributeName: aws.String("pk"), AttributeType: types.ScalarAttributeTypeS}},
	})

	// Enable -> echoed spec.
	out, err := a.UpdateTimeToLive(ctx, &dynamodb.UpdateTimeToLiveInput{
		TableName:               aws.String("T"),
		TimeToLiveSpecification: &types.TimeToLiveSpecification{Enabled: aws.Bool(true), AttributeName: aws.String("expire")},
	})
	if err != nil {
		t.Fatalf("enable: %v", err)
	}
	if !aws.ToBool(out.TimeToLiveSpecification.Enabled) || aws.ToString(out.TimeToLiveSpecification.AttributeName) != "expire" {
		t.Errorf("echoed spec = %+v", out.TimeToLiveSpecification)
	}

	// Describe reflects ENABLED.
	desc, err := a.DescribeTimeToLive(ctx, &dynamodb.DescribeTimeToLiveInput{TableName: aws.String("T")})
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if desc.TimeToLiveDescription.TimeToLiveStatus != types.TimeToLiveStatusEnabled {
		t.Errorf("status = %v, want ENABLED", desc.TimeToLiveDescription.TimeToLiveStatus)
	}
	if aws.ToString(desc.TimeToLiveDescription.AttributeName) != "expire" {
		t.Errorf("attr = %q, want expire", aws.ToString(desc.TimeToLiveDescription.AttributeName))
	}

	// Disable -> DISABLED, nil AttributeName.
	a.UpdateTimeToLive(ctx, &dynamodb.UpdateTimeToLiveInput{
		TableName:               aws.String("T"),
		TimeToLiveSpecification: &types.TimeToLiveSpecification{Enabled: aws.Bool(false), AttributeName: aws.String("expire")},
	})
	desc, _ = a.DescribeTimeToLive(ctx, &dynamodb.DescribeTimeToLiveInput{TableName: aws.String("T")})
	if desc.TimeToLiveDescription.TimeToLiveStatus != types.TimeToLiveStatusDisabled {
		t.Errorf("after disable: status = %v, want DISABLED", desc.TimeToLiveDescription.TimeToLiveStatus)
	}
	if desc.TimeToLiveDescription.AttributeName != nil {
		t.Errorf("after disable: AttributeName = %v, want nil", desc.TimeToLiveDescription.AttributeName)
	}

	// Missing table -> ResourceNotFoundException.
	_, err = a.UpdateTimeToLive(ctx, &dynamodb.UpdateTimeToLiveInput{
		TableName:               aws.String("nope"),
		TimeToLiveSpecification: &types.TimeToLiveSpecification{Enabled: aws.Bool(true), AttributeName: aws.String("expire")},
	})
	var rnfe *types.ResourceNotFoundException
	if !errors.As(err, &rnfe) {
		t.Errorf("missing table: err = %v, want ResourceNotFoundException", err)
	}

	// nil spec -> ValidationException.
	_, err = a.UpdateTimeToLive(ctx, &dynamodb.UpdateTimeToLiveInput{TableName: aws.String("T")})
	var ae smithy.APIError
	if !errors.As(err, &ae) || ae.ErrorCode() != "ValidationException" {
		t.Errorf("nil spec: err = %v, want ValidationException", err)
	}

	// nil Enabled treated as false (disable path); nil AttributeName -> engine ValidationException.
	_, err = a.UpdateTimeToLive(ctx, &dynamodb.UpdateTimeToLiveInput{
		TableName:               aws.String("T"),
		TimeToLiveSpecification: &types.TimeToLiveSpecification{Enabled: nil, AttributeName: nil},
	})
	if !errors.As(err, &ae) || ae.ErrorCode() != "ValidationException" {
		t.Errorf("nil Enabled/AttributeName: err = %v, want ValidationException", err)
	}
}

func TestAdapterDescribeTimeToLive(t *testing.T) {
	ctx := context.Background()
	a := adapterClient(t)
	a.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:            aws.String("T"),
		KeySchema:            []types.KeySchemaElement{{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash}},
		AttributeDefinitions: []types.AttributeDefinition{{AttributeName: aws.String("pk"), AttributeType: types.ScalarAttributeTypeS}},
	})

	// Never configured -> DISABLED, nil AttributeName.
	desc, err := a.DescribeTimeToLive(ctx, &dynamodb.DescribeTimeToLiveInput{TableName: aws.String("T")})
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if desc.TimeToLiveDescription.TimeToLiveStatus != types.TimeToLiveStatusDisabled {
		t.Errorf("status = %v, want DISABLED", desc.TimeToLiveDescription.TimeToLiveStatus)
	}
	if desc.TimeToLiveDescription.AttributeName != nil {
		t.Errorf("AttributeName = %v, want nil", desc.TimeToLiveDescription.AttributeName)
	}

	// Missing table -> ResourceNotFoundException.
	var rnfe *types.ResourceNotFoundException
	_, err = a.DescribeTimeToLive(ctx, &dynamodb.DescribeTimeToLiveInput{TableName: aws.String("nope")})
	if !errors.As(err, &rnfe) {
		t.Errorf("missing table: err = %v, want ResourceNotFoundException", err)
	}
}

func TestAdapterBatchWriteGetRoundTrip(t *testing.T) {
	ctx := context.Background()
	a := adapterClient(t)

	_, err := a.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:            aws.String("T"),
		KeySchema:            []types.KeySchemaElement{{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash}},
		AttributeDefinitions: []types.AttributeDefinition{{AttributeName: aws.String("pk"), AttributeType: types.ScalarAttributeTypeS}},
	})
	if err != nil {
		t.Fatalf("CreateTable: %v", err)
	}

	bw, err := a.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{RequestItems: map[string][]types.WriteRequest{
		"T": {
			{PutRequest: &types.PutRequest{Item: map[string]types.AttributeValue{"pk": &types.AttributeValueMemberS{Value: "k1"}, "v": &types.AttributeValueMemberS{Value: "one"}}}},
			{PutRequest: &types.PutRequest{Item: map[string]types.AttributeValue{"pk": &types.AttributeValueMemberS{Value: "k2"}}}},
		},
	}})
	if err != nil {
		t.Fatalf("BatchWriteItem: %v", err)
	}
	if len(bw.UnprocessedItems) != 0 {
		t.Errorf("UnprocessedItems = %v, want empty", bw.UnprocessedItems)
	}

	bg, err := a.BatchGetItem(ctx, &dynamodb.BatchGetItemInput{RequestItems: map[string]types.KeysAndAttributes{
		"T": {Keys: []map[string]types.AttributeValue{
			{"pk": &types.AttributeValueMemberS{Value: "k1"}},
			{"pk": &types.AttributeValueMemberS{Value: "ghost"}},
			{"pk": &types.AttributeValueMemberS{Value: "k2"}},
		}},
	}})
	if err != nil {
		t.Fatalf("BatchGetItem: %v", err)
	}
	if len(bg.UnprocessedKeys) != 0 {
		t.Errorf("UnprocessedKeys = %v, want empty", bg.UnprocessedKeys)
	}
	if len(bg.Responses["T"]) != 2 {
		t.Fatalf("len(Responses[T]) = %d, want 2", len(bg.Responses["T"]))
	}
	if got := bg.Responses["T"][0]["v"].(*types.AttributeValueMemberS).Value; got != "one" {
		t.Errorf("Responses[T][0] v = %q, want one", got)
	}

	// Delete via batch, confirm gone.
	if _, err := a.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{RequestItems: map[string][]types.WriteRequest{
		"T": {{DeleteRequest: &types.DeleteRequest{Key: map[string]types.AttributeValue{"pk": &types.AttributeValueMemberS{Value: "k1"}}}}},
	}}); err != nil {
		t.Fatalf("BatchWriteItem delete: %v", err)
	}
	got, err := a.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String("T"), Key: map[string]types.AttributeValue{"pk": &types.AttributeValueMemberS{Value: "k1"}}})
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if got.Item != nil {
		t.Errorf("after batch delete, Item = %v, want nil", got.Item)
	}

	// Engine validation flows through mapError: empty WriteRequest →
	// ValidationException.
	_, err = a.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{RequestItems: map[string][]types.WriteRequest{"T": {{}}}})
	var ae smithy.APIError
	if !errors.As(err, &ae) || ae.ErrorCode() != "ValidationException" {
		t.Errorf("empty WriteRequest: err = %v, want ValidationException", err)
	}
}

// §6.3 divergent rejections (adapter-only — dynamodb-local supports these).
// AttributesToGet is the remaining deliberate divergence (ProjectionExpression
// and ExpressionAttributeNames are now honored by the engine).

func TestAdapterBatchGetAttributesToGetRejected(t *testing.T) {
	ctx := context.Background()
	a := adapterClient(t)
	mustAdapterTable(t, a, ctx, "T")

	_, err := a.BatchGetItem(ctx, &dynamodb.BatchGetItemInput{RequestItems: map[string]types.KeysAndAttributes{
		"T": {
			Keys:            []map[string]types.AttributeValue{{"pk": &types.AttributeValueMemberS{Value: "k"}}},
			AttributesToGet: []string{"pk"},
		},
	}})
	var ae smithy.APIError
	if !errors.As(err, &ae) || ae.ErrorCode() != "ValidationException" {
		t.Errorf("err = %v, want ValidationException", err)
	}
}

func TestAdapterDescribeTableGsiStatuses(t *testing.T) {
	ctx := context.Background()
	a := adapterClient(t)
	// Create a table with one GSI on gp.
	if _, err := a.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName: aws.String("T"),
		KeySchema: []types.KeySchemaElement{{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash}},
		AttributeDefinitions: []types.AttributeDefinition{
			{AttributeName: aws.String("pk"), AttributeType: types.ScalarAttributeTypeS},
			{AttributeName: aws.String("gp"), AttributeType: types.ScalarAttributeTypeS},
		},
		GlobalSecondaryIndexes: []types.GlobalSecondaryIndex{{
			IndexName:  aws.String("g1x"),
			KeySchema:  []types.KeySchemaElement{{AttributeName: aws.String("gp"), KeyType: types.KeyTypeHash}},
			Projection: &types.Projection{ProjectionType: types.ProjectionTypeAll},
		}},
	}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	out, err := a.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String("T")})
	if err != nil {
		t.Fatalf("DescribeTable: %v", err)
	}
	if out.Table.TableStatus != types.TableStatusActive {
		t.Errorf("TableStatus = %q, want ACTIVE", out.Table.TableStatus)
	}
	if len(out.Table.GlobalSecondaryIndexes) != 1 {
		t.Fatalf("GSIs = %d, want 1", len(out.Table.GlobalSecondaryIndexes))
	}
	if out.Table.GlobalSecondaryIndexes[0].IndexStatus != types.IndexStatusActive {
		t.Errorf("IndexStatus = %q, want ACTIVE", out.Table.GlobalSecondaryIndexes[0].IndexStatus)
	}
}

func TestAdapterUpdateTableCreateAndDelete(t *testing.T) {
	ctx := context.Background()
	a, ctx := newAdapterTable(t)
	// Create GSI g1x on gp.
	if _, err := a.UpdateTable(ctx, &dynamodb.UpdateTableInput{
		TableName: aws.String("T"),
		AttributeDefinitions: []types.AttributeDefinition{
			{AttributeName: aws.String("gp"), AttributeType: types.ScalarAttributeTypeS},
		},
		GlobalSecondaryIndexUpdates: []types.GlobalSecondaryIndexUpdate{{
			Create: &types.CreateGlobalSecondaryIndexAction{
				IndexName:  aws.String("g1x"),
				KeySchema:  []types.KeySchemaElement{{AttributeName: aws.String("gp"), KeyType: types.KeyTypeHash}},
				Projection: &types.Projection{ProjectionType: types.ProjectionTypeAll},
			},
		}},
	}); err != nil {
		t.Fatalf("UpdateTable create: %v", err)
	}
	desc, _ := a.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String("T")})
	if len(desc.Table.GlobalSecondaryIndexes) != 1 {
		t.Fatalf("GSIs = %d, want 1", len(desc.Table.GlobalSecondaryIndexes))
	}
	if desc.Table.GlobalSecondaryIndexes[0].IndexStatus != types.IndexStatusActive {
		t.Errorf("IndexStatus = %q, want ACTIVE", desc.Table.GlobalSecondaryIndexes[0].IndexStatus)
	}
	// Delete it.
	if _, err := a.UpdateTable(ctx, &dynamodb.UpdateTableInput{
		TableName: aws.String("T"),
		GlobalSecondaryIndexUpdates: []types.GlobalSecondaryIndexUpdate{{
			Delete: &types.DeleteGlobalSecondaryIndexAction{IndexName: aws.String("g1x")},
		}},
	}); err != nil {
		t.Fatalf("UpdateTable delete: %v", err)
	}
	desc, _ = a.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String("T")})
	if len(desc.Table.GlobalSecondaryIndexes) != 0 {
		t.Errorf("after delete GSIs = %d, want 0", len(desc.Table.GlobalSecondaryIndexes))
	}
}

func TestAdapterUpdateTableRejections(t *testing.T) {
	ctx := context.Background()
	a := adapterClient(t)
	mustAdapterTable(t, a, ctx, "T")

	// Empty table name.
	_, err := a.UpdateTable(ctx, &dynamodb.UpdateTableInput{
		GlobalSecondaryIndexUpdates: []types.GlobalSecondaryIndexUpdate{{
			Create: &types.CreateGlobalSecondaryIndexAction{IndexName: aws.String("g1x")},
		}},
	})
	assertValidation(t, err)

	// Two entries.
	_, err = a.UpdateTable(ctx, &dynamodb.UpdateTableInput{
		TableName: aws.String("T"),
		GlobalSecondaryIndexUpdates: []types.GlobalSecondaryIndexUpdate{
			{Delete: &types.DeleteGlobalSecondaryIndexAction{IndexName: aws.String("g1x")}},
			{Delete: &types.DeleteGlobalSecondaryIndexAction{IndexName: aws.String("g2x")}},
		},
	})
	assertValidation(t, err)

	// GSI Update action + BillingMode (two operations).
	_, err = a.UpdateTable(ctx, &dynamodb.UpdateTableInput{
		TableName:   aws.String("T"),
		BillingMode: types.BillingModePayPerRequest,
		GlobalSecondaryIndexUpdates: []types.GlobalSecondaryIndexUpdate{{
			Update: &types.UpdateGlobalSecondaryIndexAction{IndexName: aws.String("g1x")},
		}},
	})
	assertValidation(t, err)

	// Throughput-only no-op is accepted.
	if _, err := a.UpdateTable(ctx, &dynamodb.UpdateTableInput{
		TableName:   aws.String("T"),
		BillingMode: types.BillingModePayPerRequest,
	}); err != nil {
		t.Errorf("throughput-only: %v, want nil", err)
	}
}

// assertTyped checks the returned error is the named SDK exception type.
func assertTyped[T error](t *testing.T, err error, want string) {
	t.Helper()
	var e T
	if !errors.As(err, &e) {
		t.Errorf("%s: err = %v, want %s", want, err, want)
	}
}

// TestAdapterUpdateTableErrorTypes exercises mapError for the three new
// sentinels through the public UpdateTable surface.
func TestAdapterUpdateTableErrorTypes(t *testing.T) {
	ctx := context.Background()
	a := adapterClient(t)
	mustAdapterTable(t, a, ctx, "T")

	// Seed: create g1x once (must succeed).
	if _, err := a.UpdateTable(ctx, &dynamodb.UpdateTableInput{
		TableName: aws.String("T"),
		AttributeDefinitions: []types.AttributeDefinition{
			{AttributeName: aws.String("gp"), AttributeType: types.ScalarAttributeTypeS},
		},
		GlobalSecondaryIndexUpdates: []types.GlobalSecondaryIndexUpdate{{
			Create: &types.CreateGlobalSecondaryIndexAction{
				IndexName:  aws.String("g1x"),
				KeySchema:  []types.KeySchemaElement{{AttributeName: aws.String("gp"), KeyType: types.KeyTypeHash}},
				Projection: &types.Projection{ProjectionType: types.ProjectionTypeAll},
			},
		}},
	}); err != nil {
		t.Fatalf("seed create g1x: %v", err)
	}
	// Create existing GSI -> ResourceInUseException (ErrGsiInUse, probe P2 default).
	_, err := a.UpdateTable(ctx, &dynamodb.UpdateTableInput{
		TableName: aws.String("T"),
		AttributeDefinitions: []types.AttributeDefinition{
			{AttributeName: aws.String("gp"), AttributeType: types.ScalarAttributeTypeS},
		},
		GlobalSecondaryIndexUpdates: []types.GlobalSecondaryIndexUpdate{{
			Create: &types.CreateGlobalSecondaryIndexAction{
				IndexName:  aws.String("g1x"),
				KeySchema:  []types.KeySchemaElement{{AttributeName: aws.String("gp"), KeyType: types.KeyTypeHash}},
				Projection: &types.Projection{ProjectionType: types.ProjectionTypeAll},
			},
		}},
	})
	assertTyped[*types.ResourceInUseException](t, err, "ResourceInUseException")

	// Delete unknown GSI -> ResourceNotFoundException (ErrGsiNotFoundForDelete, probe P1 default).
	_, err = a.UpdateTable(ctx, &dynamodb.UpdateTableInput{
		TableName: aws.String("T"),
		GlobalSecondaryIndexUpdates: []types.GlobalSecondaryIndexUpdate{{
			Delete: &types.DeleteGlobalSecondaryIndexAction{IndexName: aws.String("nope")},
		}},
	})
	assertTyped[*types.ResourceNotFoundException](t, err, "ResourceNotFoundException")
}

func TestAdapterGetItemAttributesToGetRejected(t *testing.T) {
	ctx := context.Background()
	a := adapterClient(t)
	mustAdapterTable(t, a, ctx, "T")

	_, err := a.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:       aws.String("T"),
		Key:             map[string]types.AttributeValue{"pk": &types.AttributeValueMemberS{Value: "k"}},
		AttributesToGet: []string{"pk"},
	})
	var ae smithy.APIError
	if !errors.As(err, &ae) || ae.ErrorCode() != "ValidationException" {
		t.Errorf("err = %v, want ValidationException", err)
	}
}

func TestAdapterGetItemEmptyProjectionRejected(t *testing.T) {
	ctx := context.Background()
	a := adapterClient(t)
	mustAdapterTable(t, a, ctx, "T")

	_, err := a.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:            aws.String("T"),
		Key:                  map[string]types.AttributeValue{"pk": &types.AttributeValueMemberS{Value: "k"}},
		ProjectionExpression: aws.String(""),
	})
	var ae smithy.APIError
	if !errors.As(err, &ae) || ae.ErrorCode() != "ValidationException" {
		t.Errorf("err = %v, want ValidationException", err)
	}
}

func TestAdapterGetItemEmptyNamesWithProjectionRejected(t *testing.T) {
	ctx := context.Background()
	a := adapterClient(t)
	mustAdapterTable(t, a, ctx, "T")

	_, err := a.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:                aws.String("T"),
		Key:                      map[string]types.AttributeValue{"pk": &types.AttributeValueMemberS{Value: "k"}},
		ProjectionExpression:     aws.String("pk"),
		ExpressionAttributeNames: map[string]string{},
	})
	var ae smithy.APIError
	if !errors.As(err, &ae) || ae.ErrorCode() != "ValidationException" {
		t.Errorf("err = %v, want ValidationException", err)
	}
}

func TestAdapterBatchGetEmptyProjectionRejected(t *testing.T) {
	ctx := context.Background()
	a := adapterClient(t)
	mustAdapterTable(t, a, ctx, "T")

	_, err := a.BatchGetItem(ctx, &dynamodb.BatchGetItemInput{RequestItems: map[string]types.KeysAndAttributes{
		"T": {
			Keys:                 []map[string]types.AttributeValue{{"pk": &types.AttributeValueMemberS{Value: "k"}}},
			ProjectionExpression: aws.String(""),
		},
	}})
	var ae smithy.APIError
	if !errors.As(err, &ae) || ae.ErrorCode() != "ValidationException" {
		t.Errorf("err = %v, want ValidationException", err)
	}
}
