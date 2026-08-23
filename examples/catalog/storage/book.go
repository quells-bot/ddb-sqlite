package storage

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

type Book struct {
	ID       string
	AuthorID string
	Title    string
	Year     int
}

func (b Book) asItem() bookItem {
	return bookItem{
		PK:         fmt.Sprintf("AUTHOR#%s", b.AuthorID),
		SK:         fmt.Sprintf("BOOK#%s", b.ID),
		EntityType: "BOOK",
		EntityID:   b.ID,
		Title:      b.Title,
		Year:       b.Year,
	}
}

// bookKey builds the composite key for a book item: books live under their
// author's partition key, discriminated by the BOOK# sort-key prefix.
func bookKey(authorID, bookID string) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{
		"PK": &types.AttributeValueMemberS{Value: fmt.Sprintf("AUTHOR#%s", authorID)},
		"SK": &types.AttributeValueMemberS{Value: fmt.Sprintf("BOOK#%s", bookID)},
	}
}

type bookItem struct {
	PK         string `dynamodbav:"PK"`
	SK         string `dynamodbav:"SK"`
	EntityType string `dynamodbav:"EntityType"`
	EntityID   string `dynamodbav:"EntityID"`
	Title      string `dynamodbav:"Title"`
	Year       int    `dynamodbav:"Year"`
	Expires    *int64 `dynamodbav:"Expires,omitempty"`
}

func (i bookItem) toBook() Book {
	return Book{
		ID:       i.EntityID,
		AuthorID: strings.TrimPrefix(i.PK, "AUTHOR#"),
		Title:    i.Title,
		Year:     i.Year,
	}
}

func (r *repo) GetBook(ctx context.Context, authorID, bookID string) (*Book, error) {
	out, err := r.ddb.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String("catalog"),
		Key:       bookKey(authorID, bookID),
	})
	if err != nil {
		return nil, err
	}
	if len(out.Item) == 0 {
		return nil, ErrNotFound
	}
	var item bookItem
	if err := attributevalue.UnmarshalMap(out.Item, &item); err != nil {
		return nil, err
	}
	if item.Expires != nil {
		return nil, ErrNotFound
	}
	b := item.toBook()
	return &b, nil
}

func (r *repo) PutBook(ctx context.Context, b *Book) error {
	item, err := attributevalue.MarshalMap(b.asItem())
	if err != nil {
		return err
	}
	_, err = r.ddb.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:           aws.String("catalog"),
		Item:                item,
		ConditionExpression: aws.String("attribute_not_exists(PK)"),
	})
	if err != nil {
		var ccf *types.ConditionalCheckFailedException
		if errors.As(err, &ccf) {
			return ErrConflict
		}
		return err
	}
	return nil
}

func (r *repo) UpdateBook(ctx context.Context, authorID, bookID, title string, year int) error {
	_, err := r.ddb.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		Key:                 bookKey(authorID, bookID),
		TableName:           aws.String("catalog"),
		UpdateExpression:    aws.String("SET #Title = :title, #Year = :year"),
		ConditionExpression: aws.String("attribute_exists(PK) AND attribute_not_exists(Expires)"),
		ExpressionAttributeNames: map[string]string{
			"#Title": "Title",
			"#Year":  "Year",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":title": &types.AttributeValueMemberS{Value: title},
			":year":  &types.AttributeValueMemberN{Value: strconv.Itoa(year)},
		},
	})
	if err != nil {
		var ccf *types.ConditionalCheckFailedException
		if errors.As(err, &ccf) {
			return ErrNotFound
		}
		return err
	}
	return nil
}

func (r *repo) DeleteBook(ctx context.Context, authorID, bookID string) error {
	_, err := r.ddb.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:           aws.String("catalog"),
		Key:                 bookKey(authorID, bookID),
		UpdateExpression:    aws.String("SET Expires = :expires"),
		ConditionExpression: aws.String("attribute_exists(PK) AND attribute_not_exists(Expires)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":expires": &types.AttributeValueMemberN{Value: strconv.FormatInt(time.Now().Unix()+3600, 10)},
		},
	})
	if err != nil {
		var ccf *types.ConditionalCheckFailedException
		if errors.As(err, &ccf) {
			return ErrNotFound
		}
		return err
	}
	return nil
}

func (r *repo) ListBooks(ctx context.Context, authorID string) ([]*Book, error) {
	out, err := r.ddb.Query(ctx, &dynamodb.QueryInput{
		TableName:                aws.String("catalog"),
		KeyConditionExpression:   aws.String("#PK = :pk AND begins_with(#SK, :sk)"),
		FilterExpression:         aws.String("attribute_not_exists(Expires)"),
		ExpressionAttributeNames: map[string]string{"#PK": "PK", "#SK": "SK"},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk": &types.AttributeValueMemberS{Value: fmt.Sprintf("AUTHOR#%s", authorID)},
			":sk": &types.AttributeValueMemberS{Value: "BOOK#"},
		},
	})
	if err != nil {
		return nil, err
	}
	books := make([]*Book, 0, len(out.Items))
	for _, row := range out.Items {
		var item bookItem
		if err := attributevalue.UnmarshalMap(row, &item); err != nil {
			return nil, err
		}
		b := item.toBook()
		books = append(books, &b)
	}
	return books, nil
}
func (r *repo) ListAllBooks(ctx context.Context) ([]*Book, error) {
	out, err := r.ddb.Query(ctx, &dynamodb.QueryInput{
		TableName:                aws.String("catalog"),
		IndexName:                aws.String("EntityTypeIndex"),
		KeyConditionExpression:   aws.String("#et = :et"),
		FilterExpression:         aws.String("attribute_not_exists(Expires)"),
		ExpressionAttributeNames: map[string]string{"#et": "EntityType"},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":et": &types.AttributeValueMemberS{Value: "BOOK"},
		},
	})
	if err != nil {
		return nil, err
	}
	books := make([]*Book, 0, len(out.Items))
	for _, row := range out.Items {
		var item bookItem
		if err := attributevalue.UnmarshalMap(row, &item); err != nil {
			return nil, err
		}
		b := item.toBook()
		books = append(books, &b)
	}
	return books, nil
}

// SoftDeleteBooks batch soft-deletes the given books by setting Expires to
// now+1h on each. Uses BatchWriteItem (PutRequest) in chunks of 25 — the
// DynamoDB batch limit. BatchWriteItem has no ConditionExpression support,
// so the books are re-put as full items with Expires added. Callers should
// read the books via ListBooks (which filters soft-deleted) before calling
// this to avoid overwriting concurrent changes.
func (r *repo) SoftDeleteBooks(ctx context.Context, books []*Book) error {
	expires := time.Now().Unix() + 3600
	const batchSize = 25
	for i := 0; i < len(books); i += batchSize {
		end := min(i+batchSize, len(books))
		batch := books[i:end]
		requests := make([]types.WriteRequest, 0, len(batch))
		for _, b := range batch {
			item := b.asItem()
			item.Expires = &expires
			marshaled, err := attributevalue.MarshalMap(item)
			if err != nil {
				return err
			}
			requests = append(requests, types.WriteRequest{
				PutRequest: &types.PutRequest{Item: marshaled},
			})
		}
		_, err := r.ddb.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{
			RequestItems: map[string][]types.WriteRequest{"catalog": requests},
		})
		if err != nil {
			return err
		}
	}
	return nil
}
