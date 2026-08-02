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
