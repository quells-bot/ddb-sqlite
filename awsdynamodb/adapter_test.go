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

func TestAdapterRejectsGSI(t *testing.T) {
	ctx := context.Background()
	a := adapterClient(t)
	_, err := a.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:            aws.String("T"),
		KeySchema:            []types.KeySchemaElement{{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash}},
		AttributeDefinitions: []types.AttributeDefinition{{AttributeName: aws.String("pk"), AttributeType: types.ScalarAttributeTypeS}},
		GlobalSecondaryIndexes: []types.GlobalSecondaryIndex{
			{IndexName: aws.String("g1"), KeySchema: []types.KeySchemaElement{{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash}}, Projection: &types.Projection{ProjectionType: types.ProjectionTypeAll}},
		},
	})
	var ve smithy.APIError
	if !errors.As(err, &ve) || ve.ErrorCode() != "ValidationException" {
		t.Errorf("GSI CreateTable: err = %v, want ValidationException", err)
	}
}
