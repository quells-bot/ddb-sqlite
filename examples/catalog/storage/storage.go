package storage

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/quells-bot/ddb-sqlite/pkg/ddb"
)

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
)

type Repository interface {
	EnsureTable(ctx context.Context) error
	GetAuthor(ctx context.Context, id string) (*Author, error)
	PutAuthor(ctx context.Context, a *Author) error
	UpdateAuthor(ctx context.Context, id, name, bio string) error
	DeleteAuthor(ctx context.Context, id string) error
	ListAuthors(ctx context.Context) ([]*Author, error)
	GetBook(ctx context.Context, authorID, bookID string) (*Book, error)
	PutBook(ctx context.Context, a *Book) error
	UpdateBook(ctx context.Context, authorID, bookID, title string, year int) error
	DeleteBook(ctx context.Context, authorID, bookID string) error
	ListBooks(ctx context.Context, authorID string) ([]*Book, error)
	ListAllBooks(ctx context.Context) ([]*Book, error)
	SoftDeleteBooks(ctx context.Context, books []*Book) error
	ExpireExpired(ctx context.Context) (int, error)
}

var _ Repository = (*repo)(nil)

type repo struct {
	ddb ddb.API
}

func NewRepo(ddb ddb.API) *repo {
	return &repo{
		ddb: ddb,
	}
}

func (r *repo) EnsureTable(ctx context.Context) (err error) {
	_, err = r.ddb.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName: aws.String("catalog"),
		KeySchema: []types.KeySchemaElement{
			{
				KeyType:       types.KeyTypeHash,
				AttributeName: aws.String("PK"),
			},
			{
				KeyType:       types.KeyTypeRange,
				AttributeName: aws.String("SK"),
			},
		},
		AttributeDefinitions: []types.AttributeDefinition{
			{
				AttributeName: aws.String("PK"),
				AttributeType: types.ScalarAttributeTypeS,
			},
			{
				AttributeName: aws.String("SK"),
				AttributeType: types.ScalarAttributeTypeS,
			},
			{
				AttributeName: aws.String("EntityType"),
				AttributeType: types.ScalarAttributeTypeS,
			},
			{
				AttributeName: aws.String("EntityID"),
				AttributeType: types.ScalarAttributeTypeS,
			},
		},
		GlobalSecondaryIndexes: []types.GlobalSecondaryIndex{
			{
				IndexName: aws.String("EntityTypeIndex"),
				KeySchema: []types.KeySchemaElement{
					{
						KeyType:       types.KeyTypeHash,
						AttributeName: aws.String("EntityType"),
					},
					{
						KeyType:       types.KeyTypeRange,
						AttributeName: aws.String("EntityID"),
					},
				},
				Projection: &types.Projection{
					ProjectionType: types.ProjectionTypeAll,
				},
			},
		},
		BillingMode: types.BillingModePayPerRequest,
	})
	if err != nil {
		// A table that already exists surfaces as ResourceInUseException.
		// Creating is idempotent: treat "already exists" as success so the
		// example can run repeatedly without manual cleanup.
		var inUse *types.ResourceInUseException
		if errors.As(err, &inUse) {
			err = nil
		} else {
			return err
		}
	}

	// Enable TTL on the Expires attribute. Idempotent: re-enabling on the
	// same attribute is a no-op. Runs whether the table was just created or
	// already existed.
	_, err = r.ddb.UpdateTimeToLive(ctx, &dynamodb.UpdateTimeToLiveInput{
		TableName: aws.String("catalog"),
		TimeToLiveSpecification: &types.TimeToLiveSpecification{
			Enabled:       aws.Bool(true),
			AttributeName: aws.String("Expires"),
		},
	})
	return err
}

// ExpireExpired manually deletes TTL-expired items from the catalog table.
// Real DynamoDB removes expired items asynchronously; this adapter never
// auto-deletes, so callers must invoke this explicitly. It reaches the
// engine extension through the ddb.ExpireExpired helper, which asserts the
// Expirer capability on the underlying ddb.API — no concrete-adapter import
// or type cast needed here. Returns the count of deleted items.
func (r *repo) ExpireExpired(ctx context.Context) (int, error) {
	return ddb.ExpireExpired(ctx, r.ddb, "catalog")
}
