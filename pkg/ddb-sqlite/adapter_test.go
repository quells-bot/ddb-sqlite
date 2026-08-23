package ddbsqlite_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/expression"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/smithy-go"

	"github.com/quells-bot/ddb-sqlite/pkg/ddb-sqlite"
)

func TestOpenClose(t *testing.T) {
	a, err := ddbsqlite.Open(context.Background(), ":memory:")
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

func adapterClient(t *testing.T) *ddbsqlite.Adapter {
	t.Helper()
	a, err := ddbsqlite.Open(context.Background(), ":memory:")
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
		TableName:            aws.String("Tbl"),
		KeySchema:            []types.KeySchemaElement{{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash}},
		AttributeDefinitions: []types.AttributeDefinition{{AttributeName: aws.String("pk"), AttributeType: types.ScalarAttributeTypeS}},
	})
	got, err := a.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String("Tbl"), Key: map[string]types.AttributeValue{"pk": &types.AttributeValueMemberS{Value: "nope"}}})
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if got.Item != nil {
		t.Errorf("missing key Item = %v, want nil", got.Item)
	}
}

func TestAdapterRejectsLegacyParameters(t *testing.T) {
	ctx := context.Background()
	a, err := ddbsqlite.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer a.Close()
	mustAdapterTable(t, a, ctx, "Tbl")

	item := map[string]types.AttributeValue{"pk": &types.AttributeValueMemberS{Value: "k"}}

	t.Run("PutItem Expected", func(t *testing.T) {
		_, err := a.PutItem(ctx, &dynamodb.PutItemInput{
			TableName: aws.String("Tbl"),
			Item:      item,
			Expected:  map[string]types.ExpectedAttributeValue{"v": {Exists: aws.Bool(false)}},
		})
		assertValidation(t, err)
	})

	t.Run("PutItem ConditionalOperator", func(t *testing.T) {
		_, err := a.PutItem(ctx, &dynamodb.PutItemInput{
			TableName:           aws.String("Tbl"),
			Item:                item,
			ConditionalOperator: types.ConditionalOperatorAnd,
		})
		assertValidation(t, err)
	})

	t.Run("DeleteItem Expected", func(t *testing.T) {
		_, err := a.DeleteItem(ctx, &dynamodb.DeleteItemInput{
			TableName: aws.String("Tbl"),
			Key:       item,
			Expected:  map[string]types.ExpectedAttributeValue{"v": {Exists: aws.Bool(false)}},
		})
		assertValidation(t, err)
	})

	t.Run("DeleteItem ConditionalOperator", func(t *testing.T) {
		_, err := a.DeleteItem(ctx, &dynamodb.DeleteItemInput{
			TableName:           aws.String("Tbl"),
			Key:                 item,
			ConditionalOperator: types.ConditionalOperatorAnd,
		})
		assertValidation(t, err)
	})
}

func TestAdapterRejectsEmptyConditionExpression(t *testing.T) {
	ctx := context.Background()
	a, err := ddbsqlite.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer a.Close()
	mustAdapterTable(t, a, ctx, "Tbl")

	_, err = a.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:           aws.String("Tbl"),
		Item:                map[string]types.AttributeValue{"pk": &types.AttributeValueMemberS{Value: "k"}},
		ConditionExpression: aws.String(""),
	})
	assertValidation(t, err)
}

func TestAdapterConditionalCheckFailedCarriesItem(t *testing.T) {
	ctx := context.Background()
	a, err := ddbsqlite.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer a.Close()
	mustAdapterTable(t, a, ctx, "Tbl")

	if _, err := a.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String("Tbl"),
		Item: map[string]types.AttributeValue{
			"pk": &types.AttributeValueMemberS{Value: "k"},
			"v":  &types.AttributeValueMemberS{Value: "first"},
		},
	}); err != nil {
		t.Fatalf("seed PutItem: %v", err)
	}

	_, err = a.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:                           aws.String("Tbl"),
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

func mustAdapterTable(t *testing.T, a *ddbsqlite.Adapter, ctx context.Context, name string) {
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

// newAdapterTable opens an adapter and creates a single-key table "Tbl".
// adapterClient is the existing helper in this file; table creation is inline
// elsewhere, so this wrapper is new.
func newAdapterTable(t *testing.T) (*ddbsqlite.Adapter, context.Context) {
	t.Helper()
	a := adapterClient(t)
	ctx := context.Background()
	if _, err := a.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:            aws.String("Tbl"),
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
		TableName:                 aws.String("Tbl"),
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
				TableName:        aws.String("Tbl"),
				Key:              key,
				AttributeUpdates: map[string]types.AttributeValueUpdate{"s": {Action: types.AttributeActionPut, Value: &types.AttributeValueMemberS{Value: "v"}}},
			},
		},
		{
			"legacy Expected",
			&dynamodb.UpdateItemInput{
				TableName: aws.String("Tbl"),
				Key:       key,
				Expected:  map[string]types.ExpectedAttributeValue{"s": {Exists: aws.Bool(false)}},
			},
		},
		{
			"legacy ConditionalOperator",
			&dynamodb.UpdateItemInput{
				TableName:           aws.String("Tbl"),
				Key:                 key,
				ConditionalOperator: types.ConditionalOperatorAnd,
			},
		},
		{
			"present-but-empty UpdateExpression",
			&dynamodb.UpdateItemInput{TableName: aws.String("Tbl"), Key: key, UpdateExpression: aws.String("")},
		},
		{
			"present-but-empty ConditionExpression",
			&dynamodb.UpdateItemInput{TableName: aws.String("Tbl"), Key: key, ConditionExpression: aws.String("")},
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
		TableName:            aws.String("Tbl"),
		KeySchema:            []types.KeySchemaElement{{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash}},
		AttributeDefinitions: []types.AttributeDefinition{{AttributeName: aws.String("pk"), AttributeType: types.ScalarAttributeTypeS}},
	})

	// Enable -> echoed spec.
	out, err := a.UpdateTimeToLive(ctx, &dynamodb.UpdateTimeToLiveInput{
		TableName:               aws.String("Tbl"),
		TimeToLiveSpecification: &types.TimeToLiveSpecification{Enabled: aws.Bool(true), AttributeName: aws.String("expire")},
	})
	if err != nil {
		t.Fatalf("enable: %v", err)
	}
	if !aws.ToBool(out.TimeToLiveSpecification.Enabled) || aws.ToString(out.TimeToLiveSpecification.AttributeName) != "expire" {
		t.Errorf("echoed spec = %+v", out.TimeToLiveSpecification)
	}

	// Describe reflects ENABLED.
	desc, err := a.DescribeTimeToLive(ctx, &dynamodb.DescribeTimeToLiveInput{TableName: aws.String("Tbl")})
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
		TableName:               aws.String("Tbl"),
		TimeToLiveSpecification: &types.TimeToLiveSpecification{Enabled: aws.Bool(false), AttributeName: aws.String("expire")},
	})
	desc, _ = a.DescribeTimeToLive(ctx, &dynamodb.DescribeTimeToLiveInput{TableName: aws.String("Tbl")})
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
	_, err = a.UpdateTimeToLive(ctx, &dynamodb.UpdateTimeToLiveInput{TableName: aws.String("Tbl")})
	var ae smithy.APIError
	if !errors.As(err, &ae) || ae.ErrorCode() != "ValidationException" {
		t.Errorf("nil spec: err = %v, want ValidationException", err)
	}

	// nil Enabled treated as false (disable path); nil AttributeName -> engine ValidationException.
	_, err = a.UpdateTimeToLive(ctx, &dynamodb.UpdateTimeToLiveInput{
		TableName:               aws.String("Tbl"),
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
		TableName:            aws.String("Tbl"),
		KeySchema:            []types.KeySchemaElement{{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash}},
		AttributeDefinitions: []types.AttributeDefinition{{AttributeName: aws.String("pk"), AttributeType: types.ScalarAttributeTypeS}},
	})

	// Never configured -> DISABLED, nil AttributeName.
	desc, err := a.DescribeTimeToLive(ctx, &dynamodb.DescribeTimeToLiveInput{TableName: aws.String("Tbl")})
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
		TableName:            aws.String("Tbl"),
		KeySchema:            []types.KeySchemaElement{{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash}},
		AttributeDefinitions: []types.AttributeDefinition{{AttributeName: aws.String("pk"), AttributeType: types.ScalarAttributeTypeS}},
	})
	if err != nil {
		t.Fatalf("CreateTable: %v", err)
	}

	bw, err := a.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{RequestItems: map[string][]types.WriteRequest{
		"Tbl": {
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
		"Tbl": {Keys: []map[string]types.AttributeValue{
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
	if len(bg.Responses["Tbl"]) != 2 {
		t.Fatalf("len(Responses[T]) = %d, want 2", len(bg.Responses["Tbl"]))
	}
	if got := bg.Responses["Tbl"][0]["v"].(*types.AttributeValueMemberS).Value; got != "one" {
		t.Errorf("Responses[T][0] v = %q, want one", got)
	}

	// Delete via batch, confirm gone.
	if _, err := a.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{RequestItems: map[string][]types.WriteRequest{
		"Tbl": {{DeleteRequest: &types.DeleteRequest{Key: map[string]types.AttributeValue{"pk": &types.AttributeValueMemberS{Value: "k1"}}}}},
	}}); err != nil {
		t.Fatalf("BatchWriteItem delete: %v", err)
	}
	got, err := a.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String("Tbl"), Key: map[string]types.AttributeValue{"pk": &types.AttributeValueMemberS{Value: "k1"}}})
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if got.Item != nil {
		t.Errorf("after batch delete, Item = %v, want nil", got.Item)
	}

	// Engine validation flows through mapError: empty WriteRequest →
	// ValidationException.
	_, err = a.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{RequestItems: map[string][]types.WriteRequest{"Tbl": {{}}}})
	var ae smithy.APIError
	if !errors.As(err, &ae) || ae.ErrorCode() != "ValidationException" {
		t.Errorf("empty WriteRequest: err = %v, want ValidationException", err)
	}
}

// divergent rejections (adapter-only — dynamodb-local supports these).
// AttributesToGet is the remaining deliberate divergence (ProjectionExpression
// and ExpressionAttributeNames are now honored by the engine).

func TestAdapterBatchGetAttributesToGetRejected(t *testing.T) {
	ctx := context.Background()
	a := adapterClient(t)
	mustAdapterTable(t, a, ctx, "Tbl")

	_, err := a.BatchGetItem(ctx, &dynamodb.BatchGetItemInput{RequestItems: map[string]types.KeysAndAttributes{
		"Tbl": {
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
		TableName: aws.String("Tbl"),
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
	out, err := a.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String("Tbl")})
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
		TableName: aws.String("Tbl"),
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
	desc, _ := a.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String("Tbl")})
	if len(desc.Table.GlobalSecondaryIndexes) != 1 {
		t.Fatalf("GSIs = %d, want 1", len(desc.Table.GlobalSecondaryIndexes))
	}
	if desc.Table.GlobalSecondaryIndexes[0].IndexStatus != types.IndexStatusActive {
		t.Errorf("IndexStatus = %q, want ACTIVE", desc.Table.GlobalSecondaryIndexes[0].IndexStatus)
	}
	// Delete it.
	if _, err := a.UpdateTable(ctx, &dynamodb.UpdateTableInput{
		TableName: aws.String("Tbl"),
		GlobalSecondaryIndexUpdates: []types.GlobalSecondaryIndexUpdate{{
			Delete: &types.DeleteGlobalSecondaryIndexAction{IndexName: aws.String("g1x")},
		}},
	}); err != nil {
		t.Fatalf("UpdateTable delete: %v", err)
	}
	desc, _ = a.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String("Tbl")})
	if len(desc.Table.GlobalSecondaryIndexes) != 0 {
		t.Errorf("after delete GSIs = %d, want 0", len(desc.Table.GlobalSecondaryIndexes))
	}
}

func TestAdapterUpdateTableRejections(t *testing.T) {
	ctx := context.Background()
	a := adapterClient(t)
	mustAdapterTable(t, a, ctx, "Tbl")

	// Empty table name.
	_, err := a.UpdateTable(ctx, &dynamodb.UpdateTableInput{
		GlobalSecondaryIndexUpdates: []types.GlobalSecondaryIndexUpdate{{
			Create: &types.CreateGlobalSecondaryIndexAction{IndexName: aws.String("g1x")},
		}},
	})
	assertValidation(t, err)

	// Two entries.
	_, err = a.UpdateTable(ctx, &dynamodb.UpdateTableInput{
		TableName: aws.String("Tbl"),
		GlobalSecondaryIndexUpdates: []types.GlobalSecondaryIndexUpdate{
			{Delete: &types.DeleteGlobalSecondaryIndexAction{IndexName: aws.String("g1x")}},
			{Delete: &types.DeleteGlobalSecondaryIndexAction{IndexName: aws.String("g2x")}},
		},
	})
	assertValidation(t, err)

	// GSI Update action + BillingMode (two operations).
	_, err = a.UpdateTable(ctx, &dynamodb.UpdateTableInput{
		TableName:   aws.String("Tbl"),
		BillingMode: types.BillingModePayPerRequest,
		GlobalSecondaryIndexUpdates: []types.GlobalSecondaryIndexUpdate{{
			Update: &types.UpdateGlobalSecondaryIndexAction{IndexName: aws.String("g1x")},
		}},
	})
	assertValidation(t, err)

	// Throughput-only no-op is accepted.
	if _, err := a.UpdateTable(ctx, &dynamodb.UpdateTableInput{
		TableName:   aws.String("Tbl"),
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
	mustAdapterTable(t, a, ctx, "Tbl")

	// Seed: create g1x once (must succeed).
	if _, err := a.UpdateTable(ctx, &dynamodb.UpdateTableInput{
		TableName: aws.String("Tbl"),
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
	// Create existing GSI -> ResourceInUseException (ErrGsiInUse).
	_, err := a.UpdateTable(ctx, &dynamodb.UpdateTableInput{
		TableName: aws.String("Tbl"),
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

	// Delete unknown GSI -> ResourceNotFoundException (ErrGsiNotFoundForDelete).
	_, err = a.UpdateTable(ctx, &dynamodb.UpdateTableInput{
		TableName: aws.String("Tbl"),
		GlobalSecondaryIndexUpdates: []types.GlobalSecondaryIndexUpdate{{
			Delete: &types.DeleteGlobalSecondaryIndexAction{IndexName: aws.String("nope")},
		}},
	})
	assertTyped[*types.ResourceNotFoundException](t, err, "ResourceNotFoundException")
}

func TestAdapterGetItemAttributesToGetRejected(t *testing.T) {
	ctx := context.Background()
	a := adapterClient(t)
	mustAdapterTable(t, a, ctx, "Tbl")

	_, err := a.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:       aws.String("Tbl"),
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
	mustAdapterTable(t, a, ctx, "Tbl")

	_, err := a.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:            aws.String("Tbl"),
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
	mustAdapterTable(t, a, ctx, "Tbl")

	_, err := a.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:                aws.String("Tbl"),
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
	mustAdapterTable(t, a, ctx, "Tbl")

	_, err := a.BatchGetItem(ctx, &dynamodb.BatchGetItemInput{RequestItems: map[string]types.KeysAndAttributes{
		"Tbl": {
			Keys:                 []map[string]types.AttributeValue{{"pk": &types.AttributeValueMemberS{Value: "k"}}},
			ProjectionExpression: aws.String(""),
		},
	}})
	var ae smithy.APIError
	if !errors.As(err, &ae) || ae.ErrorCode() != "ValidationException" {
		t.Errorf("err = %v, want ValidationException", err)
	}
}

func TestAdapterDescribeTableStats(t *testing.T) {
	ctx := context.Background()
	a := adapterClient(t)
	if _, err := a.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:            aws.String("Tbl"),
		KeySchema:            []types.KeySchemaElement{{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash}},
		AttributeDefinitions: []types.AttributeDefinition{{AttributeName: aws.String("pk"), AttributeType: types.ScalarAttributeTypeS}},
	}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	// Empty: 0/0.
	desc, err := a.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String("Tbl")})
	if err != nil {
		t.Fatalf("DescribeTable: %v", err)
	}
	if aws.ToInt64(desc.Table.ItemCount) != 0 || aws.ToInt64(desc.Table.TableSizeBytes) != 0 {
		t.Errorf("empty = (count %d, size %d), want (0, 0)", aws.ToInt64(desc.Table.ItemCount), aws.ToInt64(desc.Table.TableSizeBytes))
	}
	// {pk:k1} = 4 bytes.
	if _, err := a.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String("Tbl"),
		Item:      map[string]types.AttributeValue{"pk": &types.AttributeValueMemberS{Value: "k1"}},
	}); err != nil {
		t.Fatalf("PutItem: %v", err)
	}
	desc, _ = a.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String("Tbl")})
	if aws.ToInt64(desc.Table.ItemCount) != 1 || aws.ToInt64(desc.Table.TableSizeBytes) != 4 {
		t.Errorf("after put = (count %d, size %d), want (1, 4)", aws.ToInt64(desc.Table.ItemCount), aws.ToInt64(desc.Table.TableSizeBytes))
	}
}

// The adapter maps the engine's 16MiB-cap spill into SDK
// UnprocessedKeys, echoing ConsistentRead/projection/EAN. 100 items of
// size 167,773 each: 99 fit (16,609,527), the 100th trips (16,777,300 >
// 16,777,216) — key-ascending order spills k99.
func TestAdapterBatchGetUnprocessedKeys(t *testing.T) {
	ctx := context.Background()
	a := adapterClient(t)
	mustAdapterTable(t, a, ctx, "Tbl")

	payload := strings.Repeat("x", 167765) // per-item size 8+167765 = 167773
	for start := 0; start < 100; start += 25 {
		reqs := make([]types.WriteRequest, 0, 25)
		for i := start; i < start+25; i++ {
			reqs = append(reqs, types.WriteRequest{PutRequest: &types.PutRequest{Item: map[string]types.AttributeValue{
				"pk":  &types.AttributeValueMemberS{Value: fmt.Sprintf("k%02d", i)},
				"big": &types.AttributeValueMemberS{Value: payload},
			}}})
		}
		if _, err := a.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{RequestItems: map[string][]types.WriteRequest{"Tbl": reqs}}); err != nil {
			t.Fatalf("BatchWriteItem seed: %v", err)
		}
	}

	keys := make([]map[string]types.AttributeValue, 0, 100)
	for i := 0; i < 100; i++ {
		keys = append(keys, map[string]types.AttributeValue{"pk": &types.AttributeValueMemberS{Value: fmt.Sprintf("k%02d", i)}})
	}
	out, err := a.BatchGetItem(ctx, &dynamodb.BatchGetItemInput{RequestItems: map[string]types.KeysAndAttributes{
		"Tbl": {
			Keys:                     keys,
			ConsistentRead:           aws.Bool(true),
			ProjectionExpression:     aws.String("#b"),
			ExpressionAttributeNames: map[string]string{"#b": "big"},
		},
	}})
	if err != nil {
		t.Fatalf("BatchGetItem: %v", err)
	}
	if got := len(out.Responses["Tbl"]); got != 99 {
		t.Errorf("len(Responses[Tbl]) = %d, want 99", got)
	}
	spilled, ok := out.UnprocessedKeys["Tbl"]
	if !ok {
		t.Fatalf("UnprocessedKeys missing Tbl entry: %v", out.UnprocessedKeys)
	}
	if len(spilled.Keys) != 1 {
		t.Fatalf("len(spilled.Keys) = %d, want 1", len(spilled.Keys))
	}
	if got := spilled.Keys[0]["pk"].(*types.AttributeValueMemberS).Value; got != "k99" {
		t.Errorf("spilled key = %q, want k99", got)
	}
	if !aws.ToBool(spilled.ConsistentRead) {
		t.Errorf("spilled ConsistentRead = %v, want true", spilled.ConsistentRead)
	}
	if got := aws.ToString(spilled.ProjectionExpression); got != "#b" {
		t.Errorf("spilled ProjectionExpression = %q, want #b", got)
	}
	if spilled.ExpressionAttributeNames["#b"] != "big" {
		t.Errorf("spilled ExpressionAttributeNames = %v, want {#b:big}", spilled.ExpressionAttributeNames)
	}
}

// createCompositeTable creates a table with a composite (pk HASH S, sk RANGE N)
// primary key, which Query requires for ordered results.
func createCompositeTable(t *testing.T, a *ddbsqlite.Adapter, ctx context.Context, name string) {
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
	a, err := ddbsqlite.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer a.Close()

	createCompositeTable(t, a, ctx, "Qbl")

	// Seed items with sk 0..4.
	for i := range 5 {
		putConf(t, a, ctx, "Qbl", map[string]types.AttributeValue{
			"pk":  strVal("p1"),
			"sk":  numVal(string(rune('0' + i))),
			"val": strVal("data"),
		})
	}

	keyExpr := expression.Key("pk").Equal(expression.Value("p1"))
	expr := mustExpr(t, expression.NewBuilder().WithKeyCondition(keyExpr))
	out, err := a.Query(ctx, &dynamodb.QueryInput{
		TableName:                 aws.String("Qbl"),
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
	a, err := ddbsqlite.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer a.Close()
	mustCreate(t, a, ctx, "Tbl")

	_, err = a.Query(ctx, &dynamodb.QueryInput{
		TableName:                 aws.String("Tbl"),
		KeyConditionExpression:    aws.String("pk = :v"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":v": strVal("x")},
		Limit:                     aws.Int32(0),
	})
	asValidation(t, err, "Limit=0 should be rejected")
}

func TestAdapterQueryEmptyKeyCondition(t *testing.T) {
	ctx := context.Background()
	a, err := ddbsqlite.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer a.Close()
	mustCreate(t, a, ctx, "Tbl")

	_, err = a.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String("Tbl"),
		KeyConditionExpression: aws.String(""),
	})
	asValidation(t, err, "empty KeyConditionExpression should be rejected")
}

// Every deprecated pre-expression Query parameter is rejected (KeyConditions,
// QueryFilter, ConditionalOperator, AttributesToGet) rather than silently
// ignored. The reference honors them; rejection is the adapter's deliberate
// divergence.
func TestAdapterQueryLegacyRejections(t *testing.T) {
	ctx := context.Background()
	a, err := ddbsqlite.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer a.Close()
	createCompositeTable(t, a, ctx, "Qbl")

	cases := []struct {
		name  string
		input *dynamodb.QueryInput
	}{
		{
			"legacy KeyConditions",
			&dynamodb.QueryInput{
				TableName:     aws.String("Qbl"),
				KeyConditions: map[string]types.Condition{"pk": {}},
			},
		},
		{
			"legacy QueryFilter",
			&dynamodb.QueryInput{
				TableName:   aws.String("Qbl"),
				QueryFilter: map[string]types.Condition{"v": {}},
			},
		},
		{
			"legacy ConditionalOperator",
			&dynamodb.QueryInput{
				TableName:           aws.String("Qbl"),
				ConditionalOperator: types.ConditionalOperatorAnd,
			},
		},
		{
			"legacy AttributesToGet",
			&dynamodb.QueryInput{
				TableName:       aws.String("Qbl"),
				AttributesToGet: []string{"v"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := a.Query(ctx, tc.input)
			asValidation(t, err, tc.name+" should be rejected")
		})
	}
}

func TestAdapterQueryScanIndexForwardDefaultNil(t *testing.T) {
	// A nil ScanIndexForward must default to ASC (true) without panicking: a
	// query over sk 0..4 returns sk "0" first.
	ctx := context.Background()
	a, err := ddbsqlite.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer a.Close()
	createCompositeTable(t, a, ctx, "Qbl")

	for i := range 5 {
		putConf(t, a, ctx, "Qbl", map[string]types.AttributeValue{
			"pk": strVal("p1"),
			"sk": numVal(string(rune('0' + i))),
		})
	}

	keyExpr := expression.Key("pk").Equal(expression.Value("p1"))
	expr := mustExpr(t, expression.NewBuilder().WithKeyCondition(keyExpr))
	out, err := a.Query(ctx, &dynamodb.QueryInput{
		TableName:                 aws.String("Qbl"),
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
	a, err := ddbsqlite.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer a.Close()
	mustCreate(t, a, ctx, "Tbl")

	for i := range 3 {
		putConf(t, a, ctx, "Tbl", map[string]types.AttributeValue{
			"pk": strVal("p" + string(rune('a'+i))),
		})
	}

	out, err := a.Scan(ctx, &dynamodb.ScanInput{TableName: aws.String("Tbl")})
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

// Every deprecated pre-expression Scan parameter is rejected (ScanFilter,
// ConditionalOperator, AttributesToGet) rather than silently ignored.
func TestAdapterScanLegacyRejections(t *testing.T) {
	ctx := context.Background()
	a, err := ddbsqlite.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer a.Close()
	mustCreate(t, a, ctx, "Tbl")

	cases := []struct {
		name  string
		input *dynamodb.ScanInput
	}{
		{
			"legacy ScanFilter",
			&dynamodb.ScanInput{
				TableName:  aws.String("Tbl"),
				ScanFilter: map[string]types.Condition{"pk": {}},
			},
		},
		{
			"legacy ConditionalOperator",
			&dynamodb.ScanInput{
				TableName:           aws.String("Tbl"),
				ConditionalOperator: types.ConditionalOperatorAnd,
			},
		},
		{
			"legacy AttributesToGet",
			&dynamodb.ScanInput{
				TableName:       aws.String("Tbl"),
				AttributesToGet: []string{"v"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := a.Scan(ctx, tc.input)
			asValidation(t, err, tc.name+" should be rejected")
		})
	}
}

func TestAdapterCreateTableWithGSI(t *testing.T) {
	ctx := context.Background()
	a, cleanup := newAdapterTarget(t)
	defer cleanup()
	_, err := a.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName: aws.String("Gbl"),
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
		TableName: aws.String("Gbl"),
		Item: map[string]types.AttributeValue{
			"pk":  &types.AttributeValueMemberS{Value: "A"},
			"sk":  &types.AttributeValueMemberS{Value: "a"},
			"gpk": &types.AttributeValueMemberS{Value: "G1"},
		},
	})
	keyExpr := expression.Key("gpk").Equal(expression.Value("G1"))
	expr := mustExpr(t, expression.NewBuilder().WithKeyCondition(keyExpr))
	out, err := a.Query(ctx, &dynamodb.QueryInput{
		TableName:                 aws.String("Gbl"),
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
