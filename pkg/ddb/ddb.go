package ddb

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

type API interface {
	CreateTable(ctx context.Context, params *dynamodb.CreateTableInput, optFns ...func(*dynamodb.Options)) (*dynamodb.CreateTableOutput, error)
	DescribeTable(ctx context.Context, params *dynamodb.DescribeTableInput, optFns ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error)
	ListTables(ctx context.Context, params *dynamodb.ListTablesInput, optFns ...func(*dynamodb.Options)) (*dynamodb.ListTablesOutput, error)
	DeleteTable(ctx context.Context, params *dynamodb.DeleteTableInput, optFns ...func(*dynamodb.Options)) (*dynamodb.DeleteTableOutput, error)
	UpdateTable(ctx context.Context, params *dynamodb.UpdateTableInput, optFns ...func(*dynamodb.Options)) (*dynamodb.UpdateTableOutput, error)
	PutItem(ctx context.Context, params *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
	GetItem(ctx context.Context, params *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	DeleteItem(ctx context.Context, params *dynamodb.DeleteItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error)
	UpdateItem(ctx context.Context, params *dynamodb.UpdateItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error)
	Query(ctx context.Context, params *dynamodb.QueryInput, optFns ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error)
	Scan(ctx context.Context, params *dynamodb.ScanInput, optFns ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error)
	BatchWriteItem(ctx context.Context, params *dynamodb.BatchWriteItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.BatchWriteItemOutput, error)
	BatchGetItem(ctx context.Context, params *dynamodb.BatchGetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.BatchGetItemOutput, error)
	UpdateTimeToLive(ctx context.Context, params *dynamodb.UpdateTimeToLiveInput, optFns ...func(*dynamodb.Options)) (*dynamodb.UpdateTimeToLiveOutput, error)
	DescribeTimeToLive(ctx context.Context, params *dynamodb.DescribeTimeToLiveInput, optFns ...func(*dynamodb.Options)) (*dynamodb.DescribeTimeToLiveOutput, error)
}

// Expirer is implemented by adapters that support manual TTL expiry. The
// core *ddbsqlite.Adapter satisfies it; real-DynamoDB clients do not, since
// DynamoDB removes expired items asynchronously with no client-facing RPC.
type Expirer interface {
	ExpireExpired(ctx context.Context, tableName string) (int, error)
}

// ExpireExpired runs manual TTL expiry against api when it supports the
// extension. Callers that only hold a ddb.API (such as a repository bound to
// either the in-memory adapter or a real AWS client) can reach the engine
// extension without importing the concrete adapter package or performing a
// type assertion themselves. Returns an error for backends without the
// extension (e.g. real DynamoDB).
func ExpireExpired(ctx context.Context, api API, tableName string) (int, error) {
	e, ok := api.(Expirer)
	if !ok {
		return 0, fmt.Errorf("ddb: API %T does not support ExpireExpired", api)
	}
	return e.ExpireExpired(ctx, tableName)
}
