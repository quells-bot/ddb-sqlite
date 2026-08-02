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

	"github.com/quells-bot/ddb-sqlite/attrval"
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
	var ccf *ddb.ConditionalCheckFailedError
	if errors.As(err, &ccf) {
		e := &types.ConditionalCheckFailedException{Message: aws.String(ccf.Error())}
		if len(ccf.Item) > 0 {
			e.Item = ToSDKMap(ccf.Item)
		}
		return e
	}
	switch {
	case errors.Is(err, ddb.ErrResourceNotFound):
		return &types.ResourceNotFoundException{Message: aws.String(err.Error())}
	case errors.Is(err, ddb.ErrTableNotFound):
		return &types.ResourceNotFoundException{Message: aws.String(err.Error())}
	case errors.Is(err, ddb.ErrTableInUse):
		return &types.ResourceInUseException{Message: aws.String(err.Error())}
	case errors.Is(err, ddb.ErrGsiNotFound):
		return &smithy.GenericAPIError{Code: "ValidationException", Message: err.Error()}
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
	ks := make([]ddb.KeySchemaElement, 0, len(params.KeySchema))
	for _, k := range params.KeySchema {
		ks = append(ks, ddb.KeySchemaElement{AttributeName: aws.ToString(k.AttributeName), KeyType: string(k.KeyType)})
	}
	ads := make([]ddb.AttributeDefinition, 0, len(params.AttributeDefinitions))
	for _, ad := range params.AttributeDefinitions {
		ads = append(ads, ddb.AttributeDefinition{AttributeName: aws.ToString(ad.AttributeName), AttributeType: string(ad.AttributeType)})
	}
	gsis := make([]ddb.GlobalSecondaryIndex, 0, len(params.GlobalSecondaryIndexes))
	for _, g := range params.GlobalSecondaryIndexes {
		gsi := ddb.GlobalSecondaryIndex{
			IndexName: aws.ToString(g.IndexName),
		}
		for _, k := range g.KeySchema {
			gsi.KeySchema = append(gsi.KeySchema, ddb.KeySchemaElement{
				AttributeName: aws.ToString(k.AttributeName), KeyType: string(k.KeyType),
			})
		}
		if g.Projection != nil {
			gsi.Projection = ddb.Projection{
				Type:             string(g.Projection.ProjectionType),
				NonKeyAttributes: g.Projection.NonKeyAttributes,
			}
		}
		gsis = append(gsis, gsi)
	}
	desc, err := a.client.CreateTable(ctx, ddb.CreateTableInput{
		TableName:              aws.ToString(params.TableName),
		KeySchema:              ks,
		AttributeDefinitions:   ads,
		GlobalSecondaryIndexes: gsis,
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

// exprString unwraps an optional expression string. A present-but-empty string
// is a ValidationException, matching real DynamoDB; the engine's input structs
// use plain strings, where "" means absent, so this distinction can only be
// made here.
func exprString(p *string, field string) (string, error) {
	if p == nil {
		return "", nil
	}
	if *p == "" {
		return "", &smithy.GenericAPIError{Code: "ValidationException", Message: "awsdynamodb: " + field + " must not be empty"}
	}
	return *p, nil
}

// rejectEmptySubMaps rejects a present-but-empty substitution map when an
// expression is also present. Real DynamoDB rejects this with
// ValidationException; the SDK omits an empty map from the payload when no
// expression is present, so the service never sees it. Only the adapter can
// distinguish nil from empty here — the engine's input structs use plain maps.
func rejectEmptySubMaps(cond, update string, names map[string]string, values map[string]types.AttributeValue) error {
	if cond == "" && update == "" {
		return nil
	}
	if names != nil && len(names) == 0 {
		return &smithy.GenericAPIError{Code: "ValidationException", Message: "awsdynamodb: ExpressionAttributeNames must not be empty when an expression is present"}
	}
	if values != nil && len(values) == 0 {
		return &smithy.GenericAPIError{Code: "ValidationException", Message: "awsdynamodb: ExpressionAttributeValues must not be empty when an expression is present"}
	}
	return nil
}

// exprValues converts ExpressionAttributeValues to the engine's value model.
func exprValues(m map[string]types.AttributeValue) (map[string]attrval.Value, error) {
	if len(m) == 0 {
		return nil, nil
	}
	out := make(map[string]attrval.Value, len(m))
	for k, av := range m {
		v, err := FromSDK(av)
		if err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, nil
}

// rejectLegacy refuses the deprecated pre-expression parameters rather than
// ignoring them. Silently ignoring Expected would let a test believe it made a
// conditional write that was never evaluated.
func rejectLegacy(op string, expected map[string]types.ExpectedAttributeValue, condOp types.ConditionalOperator) error {
	if len(expected) > 0 {
		return &smithy.GenericAPIError{Code: "ValidationException", Message: "awsdynamodb: " + op + ": the legacy Expected parameter is not supported; use ConditionExpression"}
	}
	if condOp != "" {
		return &smithy.GenericAPIError{Code: "ValidationException", Message: "awsdynamodb: " + op + ": the legacy ConditionalOperator parameter is not supported; use ConditionExpression"}
	}
	return nil
}

func (a *Adapter) PutItem(ctx context.Context, params *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	if err := rejectLegacy("PutItem", params.Expected, params.ConditionalOperator); err != nil {
		return nil, err
	}
	cond, err := exprString(params.ConditionExpression, "ConditionExpression")
	if err != nil {
		return nil, err
	}
	if err := rejectEmptySubMaps(cond, "", params.ExpressionAttributeNames, params.ExpressionAttributeValues); err != nil {
		return nil, err
	}
	item, err := FromSDKMap(params.Item)
	if err != nil {
		return nil, mapError(fmt.Errorf("%w: %v", ddb.ErrValidation, err))
	}
	values, err := exprValues(params.ExpressionAttributeValues)
	if err != nil {
		return nil, mapError(fmt.Errorf("%w: %v", ddb.ErrValidation, err))
	}
	out, err := a.client.PutItem(ctx, ddb.PutItemInput{
		TableName:                           aws.ToString(params.TableName),
		Item:                                item,
		ConditionExpression:                 cond,
		ExpressionAttributeNames:            params.ExpressionAttributeNames,
		ExpressionAttributeValues:           values,
		ReturnValues:                        string(params.ReturnValues),
		ReturnValuesOnConditionCheckFailure: string(params.ReturnValuesOnConditionCheckFailure),
	})
	if err != nil {
		return nil, mapError(err)
	}
	res := &dynamodb.PutItemOutput{}
	if len(out.Attributes) > 0 {
		res.Attributes = ToSDKMap(out.Attributes)
	}
	return res, nil
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
	if err := rejectLegacy("DeleteItem", params.Expected, params.ConditionalOperator); err != nil {
		return nil, err
	}
	cond, err := exprString(params.ConditionExpression, "ConditionExpression")
	if err != nil {
		return nil, err
	}
	if err := rejectEmptySubMaps(cond, "", params.ExpressionAttributeNames, params.ExpressionAttributeValues); err != nil {
		return nil, err
	}
	key, err := FromSDKMap(params.Key)
	if err != nil {
		return nil, mapError(fmt.Errorf("%w: %v", ddb.ErrValidation, err))
	}
	values, err := exprValues(params.ExpressionAttributeValues)
	if err != nil {
		return nil, mapError(fmt.Errorf("%w: %v", ddb.ErrValidation, err))
	}
	out, err := a.client.DeleteItem(ctx, ddb.DeleteItemInput{
		TableName:                           aws.ToString(params.TableName),
		Key:                                 key,
		ConditionExpression:                 cond,
		ExpressionAttributeNames:            params.ExpressionAttributeNames,
		ExpressionAttributeValues:           values,
		ReturnValues:                        string(params.ReturnValues),
		ReturnValuesOnConditionCheckFailure: string(params.ReturnValuesOnConditionCheckFailure),
	})
	if err != nil {
		return nil, mapError(err)
	}
	res := &dynamodb.DeleteItemOutput{}
	if len(out.Attributes) > 0 {
		res.Attributes = ToSDKMap(out.Attributes)
	}
	return res, nil
}

func (a *Adapter) UpdateItem(ctx context.Context, params *dynamodb.UpdateItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	if err := rejectLegacy("UpdateItem", params.Expected, params.ConditionalOperator); err != nil {
		return nil, err
	}
	// AttributeUpdates is UpdateItem's own pre-expression parameter. Ignoring it
	// would let a test believe it wrote attributes that were never applied.
	if len(params.AttributeUpdates) > 0 {
		return nil, &smithy.GenericAPIError{Code: "ValidationException", Message: "awsdynamodb: UpdateItem: the legacy AttributeUpdates parameter is not supported; use UpdateExpression"}
	}
	cond, err := exprString(params.ConditionExpression, "ConditionExpression")
	if err != nil {
		return nil, err
	}
	update, err := exprString(params.UpdateExpression, "UpdateExpression")
	if err != nil {
		return nil, err
	}
	if err := rejectEmptySubMaps(cond, update, params.ExpressionAttributeNames, params.ExpressionAttributeValues); err != nil {
		return nil, err
	}
	key, err := FromSDKMap(params.Key)
	if err != nil {
		return nil, mapError(fmt.Errorf("%w: %v", ddb.ErrValidation, err))
	}
	values, err := exprValues(params.ExpressionAttributeValues)
	if err != nil {
		return nil, mapError(fmt.Errorf("%w: %v", ddb.ErrValidation, err))
	}
	out, err := a.client.UpdateItem(ctx, ddb.UpdateItemInput{
		TableName:                           aws.ToString(params.TableName),
		Key:                                 key,
		UpdateExpression:                    update,
		ConditionExpression:                 cond,
		ExpressionAttributeNames:            params.ExpressionAttributeNames,
		ExpressionAttributeValues:           values,
		ReturnValues:                        string(params.ReturnValues),
		ReturnValuesOnConditionCheckFailure: string(params.ReturnValuesOnConditionCheckFailure),
	})
	if err != nil {
		return nil, mapError(err)
	}
	res := &dynamodb.UpdateItemOutput{}
	if len(out.Attributes) > 0 {
		res.Attributes = ToSDKMap(out.Attributes)
	}
	return res, nil
}

// toSDKTableDescription maps the engine TableDescription to the SDK shape.
// KeySchema, AttributeDefinitions, TableName, CreationDateTime; GSIs/billing
// arrive later.
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
	for _, ad := range d.AttributeDefinitions {
		td.AttributeDefinitions = append(td.AttributeDefinitions, types.AttributeDefinition{
			AttributeName: aws.String(ad.AttributeName), AttributeType: types.ScalarAttributeType(ad.AttributeType),
		})
	}
	for _, g := range d.GlobalSecondaryIndexes {
		gd := types.GlobalSecondaryIndexDescription{
			IndexName: aws.String(g.IndexName),
		}
		for _, k := range g.KeySchema {
			kt := types.KeyTypeHash
			if k.KeyType == "RANGE" {
				kt = types.KeyTypeRange
			}
			gd.KeySchema = append(gd.KeySchema, types.KeySchemaElement{AttributeName: aws.String(k.AttributeName), KeyType: kt})
		}
		gd.Projection = &types.Projection{ProjectionType: types.ProjectionType(g.Projection.Type), NonKeyAttributes: g.Projection.NonKeyAttributes}
		gd.ItemCount = aws.Int64(0)
		gd.IndexSizeBytes = aws.Int64(0)
		td.GlobalSecondaryIndexes = append(td.GlobalSecondaryIndexes, gd)
	}
	return td
}
