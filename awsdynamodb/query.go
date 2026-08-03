package awsdynamodb

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/smithy-go"

	"github.com/quells-bot/ddb-sqlite/ddb"
)

// rejectLegacyQuery refuses the deprecated pre-expression parameters on Query.
func rejectLegacyQuery(params *dynamodb.QueryInput) error {
	if len(params.KeyConditions) > 0 {
		return &smithy.GenericAPIError{Code: "ValidationException", Message: "awsdynamodb: Query: the legacy KeyConditions parameter is not supported; use KeyConditionExpression"}
	}
	if len(params.QueryFilter) > 0 {
		return &smithy.GenericAPIError{Code: "ValidationException", Message: "awsdynamodb: Query: the legacy QueryFilter parameter is not supported; use FilterExpression"}
	}
	if params.ConditionalOperator != "" {
		return &smithy.GenericAPIError{Code: "ValidationException", Message: "awsdynamodb: Query: the legacy ConditionalOperator parameter is not supported"}
	}
	if len(params.AttributesToGet) > 0 {
		return &smithy.GenericAPIError{Code: "ValidationException", Message: "awsdynamodb: Query: the legacy AttributesToGet parameter is not supported; use ProjectionExpression"}
	}
	return nil
}

func (a *Adapter) Query(ctx context.Context, params *dynamodb.QueryInput, optFns ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
	if err := rejectLegacyQuery(params); err != nil {
		return nil, err
	}
	keyCond, err := exprString(params.KeyConditionExpression, "KeyConditionExpression")
	if err != nil {
		return nil, err
	}
	filter, err := exprString(params.FilterExpression, "FilterExpression")
	if err != nil {
		return nil, err
	}
	proj, err := exprString(params.ProjectionExpression, "ProjectionExpression")
	if err != nil {
		return nil, err
	}
	if err := rejectEmptySubMaps(params.ExpressionAttributeNames, params.ExpressionAttributeValues, keyCond, filter, proj); err != nil {
		return nil, err
	}
	// Present-but-zero Limit → ValidationException (only the adapter can
	// distinguish SDK nil from a pointer to 0).
	if params.Limit != nil && *params.Limit == 0 {
		return nil, &smithy.GenericAPIError{Code: "ValidationException", Message: "awsdynamodb: Query: Limit must be greater than or equal to 1"}
	}
	values, err := exprValues(params.ExpressionAttributeValues)
	if err != nil {
		return nil, mapError(fmt.Errorf("%w: %v", ddb.ErrValidation, err))
	}
	var esk ddb.Item
	if len(params.ExclusiveStartKey) > 0 {
		esk, err = FromSDKMap(params.ExclusiveStartKey)
		if err != nil {
			return nil, mapError(fmt.Errorf("%w: %v", ddb.ErrValidation, err))
		}
	}
	out, err := a.client.Query(ctx, ddb.QueryInput{
		TableName:                 aws.ToString(params.TableName),
		KeyConditionExpression:    keyCond,
		FilterExpression:          filter,
		ProjectionExpression:      proj,
		ExpressionAttributeNames:  params.ExpressionAttributeNames,
		ExpressionAttributeValues: values,
		ExclusiveStartKey:         esk,
		Limit:                     aws.ToInt32(params.Limit),
		ScanIndexForward:          params.ScanIndexForward == nil || *params.ScanIndexForward,
		ConsistentRead:            aws.ToBool(params.ConsistentRead),
		Select:                    string(params.Select),
		IndexName:                 aws.ToString(params.IndexName),
	})
	if err != nil {
		return nil, mapError(err)
	}
	res := &dynamodb.QueryOutput{
		Count:        out.Count,
		ScannedCount: out.ScannedCount,
	}
	for _, item := range out.Items {
		res.Items = append(res.Items, ToSDKMap(item))
	}
	if len(out.LastEvaluatedKey) > 0 {
		res.LastEvaluatedKey = ToSDKMap(out.LastEvaluatedKey)
	}
	return res, nil
}

// rejectLegacyScan refuses the deprecated pre-expression parameters on Scan.
func rejectLegacyScan(params *dynamodb.ScanInput) error {
	if len(params.ScanFilter) > 0 {
		return &smithy.GenericAPIError{Code: "ValidationException", Message: "awsdynamodb: Scan: the legacy ScanFilter parameter is not supported; use FilterExpression"}
	}
	if params.ConditionalOperator != "" {
		return &smithy.GenericAPIError{Code: "ValidationException", Message: "awsdynamodb: Scan: the legacy ConditionalOperator parameter is not supported"}
	}
	if len(params.AttributesToGet) > 0 {
		return &smithy.GenericAPIError{Code: "ValidationException", Message: "awsdynamodb: Scan: the legacy AttributesToGet parameter is not supported; use ProjectionExpression"}
	}
	return nil
}

func (a *Adapter) Scan(ctx context.Context, params *dynamodb.ScanInput, optFns ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
	if err := rejectLegacyScan(params); err != nil {
		return nil, err
	}
	filter, err := exprString(params.FilterExpression, "FilterExpression")
	if err != nil {
		return nil, err
	}
	proj, err := exprString(params.ProjectionExpression, "ProjectionExpression")
	if err != nil {
		return nil, err
	}
	if err := rejectEmptySubMaps(params.ExpressionAttributeNames, params.ExpressionAttributeValues, filter, proj); err != nil {
		return nil, err
	}
	// Present-but-zero Limit → ValidationException.
	if params.Limit != nil && *params.Limit == 0 {
		return nil, &smithy.GenericAPIError{Code: "ValidationException", Message: "awsdynamodb: Scan: Limit must be greater than or equal to 1"}
	}
	values, err := exprValues(params.ExpressionAttributeValues)
	if err != nil {
		return nil, mapError(fmt.Errorf("%w: %v", ddb.ErrValidation, err))
	}
	var esk ddb.Item
	if len(params.ExclusiveStartKey) > 0 {
		esk, err = FromSDKMap(params.ExclusiveStartKey)
		if err != nil {
			return nil, mapError(fmt.Errorf("%w: %v", ddb.ErrValidation, err))
		}
	}
	out, err := a.client.Scan(ctx, ddb.ScanInput{
		TableName:                 aws.ToString(params.TableName),
		FilterExpression:          filter,
		ProjectionExpression:      proj,
		ExpressionAttributeNames:  params.ExpressionAttributeNames,
		ExpressionAttributeValues: values,
		ExclusiveStartKey:         esk,
		Limit:                     aws.ToInt32(params.Limit),
		Segment:                   aws.ToInt32(params.Segment),
		TotalSegments:             aws.ToInt32(params.TotalSegments),
		ConsistentRead:            aws.ToBool(params.ConsistentRead),
		Select:                    string(params.Select),
		IndexName:                 aws.ToString(params.IndexName),
	})
	if err != nil {
		return nil, mapError(err)
	}
	res := &dynamodb.ScanOutput{
		Count:        out.Count,
		ScannedCount: out.ScannedCount,
	}
	for _, item := range out.Items {
		res.Items = append(res.Items, ToSDKMap(item))
	}
	if len(out.LastEvaluatedKey) > 0 {
		res.LastEvaluatedKey = ToSDKMap(out.LastEvaluatedKey)
	}
	return res, nil
}
