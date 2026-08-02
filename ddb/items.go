package ddb

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/quells-bot/ddb-sqlite/attrval"
	"github.com/quells-bot/ddb-sqlite/internal/storage"
)

// Item is a DynamoDB item: a map of attribute names to typed values.
type Item map[string]attrval.Value

// PutItemInput carries the table name and item to insert/overwrite by key,
// plus the optional condition expression and ReturnValues mode.
type PutItemInput struct {
	TableName string
	Item      Item

	ConditionExpression       string
	ExpressionAttributeNames  map[string]string
	ExpressionAttributeValues map[string]attrval.Value

	// ReturnValues accepts NONE (default) or ALL_OLD.
	ReturnValues string
	// ReturnValuesOnConditionCheckFailure accepts NONE (default) or ALL_OLD;
	// ALL_OLD populates ConditionalCheckFailedError.Item.
	ReturnValuesOnConditionCheckFailure string
}

// PutItemOutput carries the pre-write item when ReturnValues=ALL_OLD.
type PutItemOutput struct {
	Attributes Item
}

// maxItemBytes is the 400KB item-size proxy (JSON byte length). Full accounting
// is deferred to M6; M1 uses this faithful-enough proxy.
const maxItemBytes = 400 * 1024

// tagForKeyType maps a DynamoDB key type to the attrval tag it must carry.
func tagForKeyType(t string) attrval.Tag {
	switch t {
	case "S":
		return attrval.TagString
	case "N":
		return attrval.TagNumber
	case "B":
		return attrval.TagBinary
	}
	return attrval.TagNull // invalid; caller has already validated the type
}

// keyValue converts an attrval.Value to the Go value storage expects for the
// indexed column: S->string, N->float64 (canonical string via strconv), B->[]byte.
func keyValue(v attrval.Value) (any, error) {
	switch v.Tag() {
	case attrval.TagString:
		return v.Str(), nil
	case attrval.TagNumber:
		f, err := strconv.ParseFloat(v.Num().String(), 64)
		if err != nil {
			return nil, fmt.Errorf("%w: number key not representable as float64", ErrValidation)
		}
		return f, nil
	case attrval.TagBinary:
		return v.Bin(), nil
	default:
		return nil, fmt.Errorf("%w: key value has non-key type %s", ErrValidation, v.Type())
	}
}

func (c *Client) PutItem(ctx context.Context, in PutItemInput) (PutItemOutput, error) {
	tx, err := c.store.BeginTx(ctx)
	if err != nil {
		return PutItemOutput{}, err
	}
	defer tx.Rollback()

	def, err := c.store.GetTableDef(tx, in.TableName)
	if errors.Is(err, storage.ErrNotFound) {
		return PutItemOutput{}, fmt.Errorf("%w: table %q not found", ErrTableNotFound, in.TableName)
	}
	if err != nil {
		return PutItemOutput{}, err
	}

	if err := validatePutKey(def, in.Item); err != nil {
		return PutItemOutput{}, err
	}

	// Expressions are validated only after the table and key, because that is
	// the order DynamoDB reports failures in: a request naming a missing table
	// gets ResourceNotFoundException even when its expression is also
	// malformed. Parsing still happens before the row read, so a bad
	// expression fails whether or not the item exists.
	rv, err := validateReturnValuesOldOnly(in.ReturnValues)
	if err != nil {
		return PutItemOutput{}, err
	}
	roc, err := validateReturnValuesOnConditionCheckFailure(in.ReturnValuesOnConditionCheckFailure)
	if err != nil {
		return PutItemOutput{}, err
	}
	ex, err := prepareExpressions(expressionRequest{
		Condition: in.ConditionExpression,
		Names:     in.ExpressionAttributeNames,
		Values:    in.ExpressionAttributeValues,
	})
	if err != nil {
		return PutItemOutput{}, err
	}
	cond := ex.Cond

	// Item-size limit (JSON byte-length proxy).
	wire, err := json.Marshal(in.Item)
	if err != nil {
		return PutItemOutput{}, fmt.Errorf("%w: marshal item: %v", ErrValidation, err)
	}
	if len(wire) > maxItemBytes {
		return PutItemOutput{}, fmt.Errorf("%w: item size %d exceeds %d bytes", ErrValidation, len(wire), maxItemBytes)
	}

	// Extract key column values.
	hashVal, err := keyValue(in.Item[def.Hash])
	if err != nil {
		return PutItemOutput{}, err
	}
	var rangeVal any
	if def.Range != "" {
		rangeVal, err = keyValue(in.Item[def.Range])
		if err != nil {
			return PutItemOutput{}, err
		}
	}

	// Read the existing item only when a condition or ALL_OLD needs it.
	var old Item
	if cond != nil || rv == returnValuesAllOld {
		old, err = c.readItem(tx, in.TableName, hashVal, rangeVal)
		if err != nil {
			return PutItemOutput{}, err
		}
	}
	if err := checkCondition(cond, old, roc); err != nil {
		return PutItemOutput{}, err
	}

	// GSI key validation BEFORE the storage write so a rejected write is atomic.
	if err := validateGsiKeys(in.Item, def.GSIs); err != nil {
		return PutItemOutput{}, err
	}

	dataID, err := c.store.PutItem(tx, in.TableName, hashVal, rangeVal, wire)
	if err != nil {
		return PutItemOutput{}, err
	}
	if err := c.maintainGsiRows(tx, in.TableName, def.GSIs, dataID, in.Item); err != nil {
		return PutItemOutput{}, err
	}
	if err := tx.Commit(); err != nil {
		return PutItemOutput{}, err
	}

	out := PutItemOutput{}
	if rv == returnValuesAllOld {
		out.Attributes = old
	}
	return out, nil
}

// readItem fetches and unmarshals the item at a key, returning a nil Item when
// no row exists. Shared by the conditional write paths.
func (c *Client) readItem(tx *sql.Tx, table string, hashVal, rangeVal any) (Item, error) {
	_, data, found, err := c.store.GetItem(tx, table, hashVal, rangeVal)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}
	item := Item{}
	if err := json.Unmarshal(data, &item); err != nil {
		return nil, fmt.Errorf("ddb: unmarshal item: %w", err)
	}
	return item, nil
}

// GetItemInput carries the table name and the exact key to look up.
// ConsistentRead is accepted and ignored (the engine is always consistent).
type GetItemInput struct {
	TableName      string
	Key            Item
	ConsistentRead bool
}

// GetItemOutput holds the item, or an empty Item if the key was not found.
type GetItemOutput struct {
	Item Item
}

// DeleteItemInput carries the table name and the exact key to delete, plus the
// optional condition expression and ReturnValues mode.
type DeleteItemInput struct {
	TableName string
	Key       Item

	ConditionExpression       string
	ExpressionAttributeNames  map[string]string
	ExpressionAttributeValues map[string]attrval.Value

	// ReturnValues accepts NONE (default) or ALL_OLD.
	ReturnValues string
	// ReturnValuesOnConditionCheckFailure accepts NONE (default) or ALL_OLD.
	ReturnValuesOnConditionCheckFailure string
}

// DeleteItemOutput carries the deleted item when ReturnValues=ALL_OLD.
type DeleteItemOutput struct {
	Attributes Item
}

// validatePutKey checks the item carries the table's key attributes with
// matching types. Shared by PutItem and BatchWriteItem.
func validatePutKey(def storage.TableDef, item Item) error {
	hv, ok := item[def.Hash]
	if !ok {
		return fmt.Errorf("%w: item missing partition key %q", ErrValidation, def.Hash)
	}
	if hv.Tag() != tagForKeyType(def.HashType) {
		return fmt.Errorf("%w: partition key %q type %s != declared %s", ErrValidation, def.Hash, hv.Type(), def.HashType)
	}
	if def.Range != "" {
		rv, ok := item[def.Range]
		if !ok {
			return fmt.Errorf("%w: item missing sort key %q", ErrValidation, def.Range)
		}
		if rv.Tag() != tagForKeyType(def.RangeType) {
			return fmt.Errorf("%w: sort key %q type %s != declared %s", ErrValidation, def.Range, rv.Type(), def.RangeType)
		}
	}
	return nil
}

// validateKey checks the Key carries exactly the table's key attributes with
// matching types. Returns the hash/range attrval.Values on success.
func validateKey(def storage.TableDef, key Item) (hash, range_ attrval.Value, err error) {
	hv, ok := key[def.Hash]
	if !ok {
		return attrval.Value{}, attrval.Value{}, fmt.Errorf("%w: key missing partition key %q", ErrValidation, def.Hash)
	}
	if hv.Tag() != tagForKeyType(def.HashType) {
		return attrval.Value{}, attrval.Value{}, fmt.Errorf("%w: partition key %q type mismatch", ErrValidation, def.Hash)
	}
	var rv attrval.Value
	if def.Range != "" {
		rv, ok = key[def.Range]
		if !ok {
			return attrval.Value{}, attrval.Value{}, fmt.Errorf("%w: key missing sort key %q", ErrValidation, def.Range)
		}
		if rv.Tag() != tagForKeyType(def.RangeType) {
			return attrval.Value{}, attrval.Value{}, fmt.Errorf("%w: sort key %q type mismatch", ErrValidation, def.Range)
		}
	}
	// Key must contain exactly the key attributes.
	if len(key) != mapSizeForKeys(def) {
		return attrval.Value{}, attrval.Value{}, fmt.Errorf("%w: key must contain exactly the key attributes", ErrValidation)
	}
	return hv, rv, nil
}

func mapSizeForKeys(def storage.TableDef) int {
	n := 1
	if def.Range != "" {
		n = 2
	}
	return n
}

func (c *Client) GetItem(ctx context.Context, in GetItemInput) (GetItemOutput, error) {
	tx, err := c.store.BeginTx(ctx)
	if err != nil {
		return GetItemOutput{}, err
	}
	defer tx.Rollback()

	def, err := c.store.GetTableDef(tx, in.TableName)
	if errors.Is(err, storage.ErrNotFound) {
		return GetItemOutput{}, fmt.Errorf("%w: table %q not found", ErrTableNotFound, in.TableName)
	}
	if err != nil {
		return GetItemOutput{}, err
	}

	hv, rv, err := validateKey(def, in.Key)
	if err != nil {
		return GetItemOutput{}, err
	}
	hashVal, err := keyValue(hv)
	if err != nil {
		return GetItemOutput{}, err
	}
	var rangeVal any
	if def.Range != "" {
		rangeVal, err = keyValue(rv)
		if err != nil {
			return GetItemOutput{}, err
		}
	}

	_, data, found, err := c.store.GetItem(tx, in.TableName, hashVal, rangeVal)
	if err != nil {
		return GetItemOutput{}, err
	}
	if !found {
		return GetItemOutput{Item: Item{}}, nil
	}
	item := Item{}
	if err := json.Unmarshal(data, &item); err != nil {
		return GetItemOutput{}, fmt.Errorf("ddb: unmarshal item: %w", err)
	}
	return GetItemOutput{Item: item}, tx.Commit()
}

func (c *Client) DeleteItem(ctx context.Context, in DeleteItemInput) (DeleteItemOutput, error) {
	tx, err := c.store.BeginTx(ctx)
	if err != nil {
		return DeleteItemOutput{}, err
	}
	defer tx.Rollback()

	def, err := c.store.GetTableDef(tx, in.TableName)
	if errors.Is(err, storage.ErrNotFound) {
		return DeleteItemOutput{}, fmt.Errorf("%w: table %q not found", ErrTableNotFound, in.TableName)
	}
	if err != nil {
		return DeleteItemOutput{}, err
	}

	hv, rvKey, err := validateKey(def, in.Key)
	if err != nil {
		return DeleteItemOutput{}, err
	}

	// Table and key first, then expressions — see PutItem.
	rv, err := validateReturnValuesOldOnly(in.ReturnValues)
	if err != nil {
		return DeleteItemOutput{}, err
	}
	roc, err := validateReturnValuesOnConditionCheckFailure(in.ReturnValuesOnConditionCheckFailure)
	if err != nil {
		return DeleteItemOutput{}, err
	}
	ex, err := prepareExpressions(expressionRequest{
		Condition: in.ConditionExpression,
		Names:     in.ExpressionAttributeNames,
		Values:    in.ExpressionAttributeValues,
	})
	if err != nil {
		return DeleteItemOutput{}, err
	}
	cond := ex.Cond

	hashVal, err := keyValue(hv)
	if err != nil {
		return DeleteItemOutput{}, err
	}
	var rangeVal any
	if def.Range != "" {
		rangeVal, err = keyValue(rvKey)
		if err != nil {
			return DeleteItemOutput{}, err
		}
	}

	var old Item
	if cond != nil || rv == returnValuesAllOld {
		old, err = c.readItem(tx, in.TableName, hashVal, rangeVal)
		if err != nil {
			return DeleteItemOutput{}, err
		}
	}
	if err := checkCondition(cond, old, roc); err != nil {
		return DeleteItemOutput{}, err
	}

	if _, err := c.store.DeleteItem(tx, in.TableName, hashVal, rangeVal); err != nil {
		return DeleteItemOutput{}, err
	}
	if err := tx.Commit(); err != nil {
		return DeleteItemOutput{}, err
	}

	out := DeleteItemOutput{}
	if rv == returnValuesAllOld {
		out.Attributes = old
	}
	return out, nil
}
