package storage

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

type Author struct {
	ID   string
	Name string
	Bio  string
}

func (a Author) asItem() authorItem {
	return authorItem{
		PK:         fmt.Sprintf("AUTHOR#%s", a.ID),
		SK:         "PROFILE",
		EntityType: "AUTHOR",
		EntityID:   a.ID,
		Name:       a.Name,
		Bio:        a.Bio,
	}
}

type authorItem struct {
	PK         string `dynamodbav:"PK"`
	SK         string `dynamodbav:"SK"`
	EntityType string `dynamodbav:"EntityType"`
	EntityID   string `dynamodbav:"EntityID"`
	Name       string `dynamodbav:"Name"`
	Bio        string `dynamodbav:"Bio"`
	Expires    *int64 `dynamodbav:"Expires,omitempty"`
}

func (i authorItem) toAuthor() Author {
	return Author{
		ID:   i.EntityID,
		Name: i.Name,
		Bio:  i.Bio,
	}
}

// authorKey builds the composite primary key for an author item. The wire
// attribute names are uppercase and flow through Get/Put/Update/Delete alike.
func authorKey(id string) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{
		"PK": &types.AttributeValueMemberS{Value: fmt.Sprintf("AUTHOR#%s", id)},
		"SK": &types.AttributeValueMemberS{Value: "PROFILE"},
	}
}

func (r *repo) GetAuthor(ctx context.Context, id string) (*Author, error) {
	out, err := r.ddb.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String("catalog"),
		Key:       authorKey(id),
	})
	if err != nil {
		return nil, err
	}
	if len(out.Item) == 0 {
		return nil, ErrNotFound
	}
	var item authorItem
	if err := attributevalue.UnmarshalMap(out.Item, &item); err != nil {
		return nil, err
	}
	if item.Expires != nil {
		return nil, ErrNotFound
	}
	a := item.toAuthor()
	return &a, nil
}

func (r *repo) PutAuthor(ctx context.Context, a *Author) error {
	item, err := attributevalue.MarshalMap(a.asItem())
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

func (r *repo) UpdateAuthor(ctx context.Context, id, name, bio string) error {
	_, err := r.ddb.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:           aws.String("catalog"),
		Key:                 authorKey(id),
		UpdateExpression:    aws.String("SET #Name = :Name, #Bio = :Bio"),
		ConditionExpression: aws.String("attribute_exists(PK) AND attribute_not_exists(Expires)"),
		ExpressionAttributeNames: map[string]string{
			"#Name": "Name",
			"#Bio":  "Bio",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":Name": &types.AttributeValueMemberS{Value: name},
			":Bio":  &types.AttributeValueMemberS{Value: bio},
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

// DeleteAuthor soft-deletes an author's profile item by setting Expires to
// now+1h. It does not cascade: books under this author's partition remain
// and must be deleted separately (the bus layer handles cascading).
func (r *repo) DeleteAuthor(ctx context.Context, id string) error {
	_, err := r.ddb.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:           aws.String("catalog"),
		Key:                 authorKey(id),
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

func (r *repo) ListAuthors(ctx context.Context) ([]*Author, error) {
	out, err := r.ddb.Query(ctx, &dynamodb.QueryInput{
		TableName:                aws.String("catalog"),
		IndexName:                aws.String("EntityTypeIndex"),
		KeyConditionExpression:   aws.String("#et = :et"),
		FilterExpression:         aws.String("attribute_not_exists(Expires)"),
		ExpressionAttributeNames: map[string]string{"#et": "EntityType"},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":et": &types.AttributeValueMemberS{Value: "AUTHOR"},
		},
	})
	if err != nil {
		return nil, err
	}
	authors := make([]*Author, 0, len(out.Items))
	for _, row := range out.Items {
		var item authorItem
		if err := attributevalue.UnmarshalMap(row, &item); err != nil {
			return nil, err
		}
		a := item.toAuthor()
		authors = append(authors, &a)
	}
	return authors, nil
}
