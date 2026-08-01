// Package awsdynamodb adapts the ddb engine to the AWS SDK v2 DynamoDBAPI
// surface for the supported operation subset. It is a separate Go module so the
// SDK dependency is isolated from the SDK-free root. The adapter is
// goroutine-safe because *ddb.Client is; no extra locking.
package awsdynamodb

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/smithy-go"

	"github.com/quells-bot/ddb-sqlite/ddb"
)

// Adapter implements the supported subset of the SDK's DynamoDBAPI methods
// (exact SDK signatures) by translating to *ddb.Client.
type Adapter struct {
	client *ddb.Client
}

// New wraps an existing *ddb.Client.
func New(client *ddb.Client) *Adapter {
	return &Adapter{client: client}
}

// Open is a convenience that opens an in-memory-or-file ddb client and wraps it.
// It lets callers (and the conformance harness) build an adapter through this
// package alone, without importing ddb.
func Open(ctx context.Context, dsn string) (*Adapter, error) {
	c, err := ddb.Open(ctx, ddb.Options{DSN: dsn})
	if err != nil {
		return nil, err
	}
	return New(c), nil
}

// Close closes the underlying ddb client.
func (a *Adapter) Close() error { return a.client.Close() }

// mapError converts a ddb typed error to the matching SDK exception type so
// errors.As works in the conformance suite. Marshal/validation failures map to
// ValidationException.
func mapError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, ddb.ErrTableNotFound):
		return &types.ResourceNotFoundException{Message: aws.String(err.Error())}
	case errors.Is(err, ddb.ErrTableInUse):
		return &types.ResourceInUseException{Message: aws.String(err.Error())}
	case errors.Is(err, ddb.ErrValidation):
		// DynamoDB has no generated ValidationException type; real DynamoDB
		// surfaces 400/validation failures as a generic API error carrying the
		// service error code. Match that so errors.As sees the same shape as the
		// reference target.
		return &smithy.GenericAPIError{Code: "ValidationException", Message: err.Error()}
	case errors.Is(err, ddb.ErrConditionalCheck):
		return &types.ConditionalCheckFailedException{Message: aws.String(err.Error())}
	default:
		return err
	}
}

func (a *Adapter) CreateTable(ctx context.Context, params *dynamodb.CreateTableInput, optFns ...func(*dynamodb.Options)) (*dynamodb.CreateTableOutput, error) {
	if len(params.GlobalSecondaryIndexes) > 0 {
		return nil, &smithy.GenericAPIError{Code: "ValidationException", Message: "awsdynamodb: GlobalSecondaryIndexes not supported in M1"}
	}
	ks := make([]ddb.KeySchemaElement, 0, len(params.KeySchema))
	for _, k := range params.KeySchema {
		ks = append(ks, ddb.KeySchemaElement{AttributeName: aws.ToString(k.AttributeName), KeyType: string(k.KeyType)})
	}
	ads := make([]ddb.AttributeDefinition, 0, len(params.AttributeDefinitions))
	for _, ad := range params.AttributeDefinitions {
		ads = append(ads, ddb.AttributeDefinition{AttributeName: aws.ToString(ad.AttributeName), AttributeType: string(ad.AttributeType)})
	}
	desc, err := a.client.CreateTable(ctx, ddb.CreateTableInput{
		TableName:            aws.ToString(params.TableName),
		KeySchema:            ks,
		AttributeDefinitions: ads,
	})
	if err != nil {
		return nil, mapError(err)
	}
	return &dynamodb.CreateTableOutput{TableDescription: toSDKTableDescription(desc)}, nil
}

func (a *Adapter) DescribeTable(ctx context.Context, params *dynamodb.DescribeTableInput, optFns ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error) {
	desc, err := a.client.DescribeTable(ctx, ddb.DescribeTableInput{TableName: aws.ToString(params.TableName)})
	if err != nil {
		return nil, mapError(err)
	}
	return &dynamodb.DescribeTableOutput{Table: toSDKTableDescription(desc)}, nil
}

func (a *Adapter) ListTables(ctx context.Context, params *dynamodb.ListTablesInput, optFns ...func(*dynamodb.Options)) (*dynamodb.ListTablesOutput, error) {
	in := ddb.ListTablesInput{ExclusiveStartTableName: aws.ToString(params.ExclusiveStartTableName)}
	if params.Limit != nil {
		in.Limit = aws.ToInt32(params.Limit)
	}
	out, err := a.client.ListTables(ctx, in)
	if err != nil {
		return nil, mapError(err)
	}
	res := &dynamodb.ListTablesOutput{TableNames: out.TableNames}
	if out.LastEvaluatedTableName != "" {
		res.LastEvaluatedTableName = aws.String(out.LastEvaluatedTableName)
	}
	return res, nil
}

func (a *Adapter) DeleteTable(ctx context.Context, params *dynamodb.DeleteTableInput, optFns ...func(*dynamodb.Options)) (*dynamodb.DeleteTableOutput, error) {
	err := a.client.DeleteTable(ctx, ddb.DeleteTableInput{TableName: aws.ToString(params.TableName)})
	if err != nil {
		return nil, mapError(err)
	}
	return &dynamodb.DeleteTableOutput{}, nil
}

func (a *Adapter) PutItem(ctx context.Context, params *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	item, err := FromSDKMap(params.Item)
	if err != nil {
		return nil, mapError(fmt.Errorf("%w: %v", ddb.ErrValidation, err))
	}
	if err := a.client.PutItem(ctx, ddb.PutItemInput{TableName: aws.ToString(params.TableName), Item: item}); err != nil {
		return nil, mapError(err)
	}
	return &dynamodb.PutItemOutput{}, nil
}

func (a *Adapter) GetItem(ctx context.Context, params *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	key, err := FromSDKMap(params.Key)
	if err != nil {
		return nil, mapError(fmt.Errorf("%w: %v", ddb.ErrValidation, err))
	}
	out, err := a.client.GetItem(ctx, ddb.GetItemInput{TableName: aws.ToString(params.TableName), Key: key, ConsistentRead: aws.ToBool(params.ConsistentRead)})
	if err != nil {
		return nil, mapError(err)
	}
	if len(out.Item) == 0 {
		return &dynamodb.GetItemOutput{}, nil // no Item field — faithful "not found"
	}
	return &dynamodb.GetItemOutput{Item: ToSDKMap(out.Item)}, nil
}

func (a *Adapter) DeleteItem(ctx context.Context, params *dynamodb.DeleteItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error) {
	key, err := FromSDKMap(params.Key)
	if err != nil {
		return nil, mapError(fmt.Errorf("%w: %v", ddb.ErrValidation, err))
	}
	if err := a.client.DeleteItem(ctx, ddb.DeleteItemInput{TableName: aws.ToString(params.TableName), Key: key}); err != nil {
		return nil, mapError(err)
	}
	return &dynamodb.DeleteItemOutput{}, nil
}

// toSDKTableDescription maps the engine TableDescription to the SDK shape. Only
// M1 fields are populated (KeySchema, AttributeDefinitions, TableName,
// CreationDateTime); GSIs/billing arrive later.
func toSDKTableDescription(d ddb.TableDescription) *types.TableDescription {
	td := &types.TableDescription{
		TableName:        aws.String(d.Name),
		CreationDateTime: aws.Time(d.CreationTime),
	}
	if d.Hash != "" {
		td.KeySchema = append(td.KeySchema, types.KeySchemaElement{AttributeName: aws.String(d.Hash), KeyType: types.KeyTypeHash})
	}
	if d.Range != "" {
		td.KeySchema = append(td.KeySchema, types.KeySchemaElement{AttributeName: aws.String(d.Range), KeyType: types.KeyTypeRange})
	}
	if d.Hash != "" {
		td.AttributeDefinitions = append(td.AttributeDefinitions, types.AttributeDefinition{AttributeName: aws.String(d.Hash), AttributeType: types.ScalarAttributeType(d.HashType)})
	}
	if d.Range != "" {
		td.AttributeDefinitions = append(td.AttributeDefinitions, types.AttributeDefinition{AttributeName: aws.String(d.Range), AttributeType: types.ScalarAttributeType(d.RangeType)})
	}
	return td
}
