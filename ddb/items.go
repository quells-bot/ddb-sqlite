package ddb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/quells-bot/ddb-sqlite/attrval"
	"github.com/quells-bot/ddb-sqlite/internal/storage"
)

// Item is a DynamoDB item: a map of attribute names to typed values.
type Item map[string]attrval.Value

// PutItemInput carries the table name and item to insert/overwrite by key.
type PutItemInput struct {
	TableName string
	Item      Item
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

func (c *Client) PutItem(ctx context.Context, in PutItemInput) error {
	tx, err := c.store.BeginTx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	def, err := c.store.GetTableDef(tx, in.TableName)
	if errors.Is(err, storage.ErrNotFound) {
		return fmt.Errorf("%w: table %q not found", ErrTableNotFound, in.TableName)
	}
	if err != nil {
		return err
	}

	// Validate the partition key attribute is present with the right type.
	hv, ok := in.Item[def.Hash]
	if !ok {
		return fmt.Errorf("%w: item missing partition key %q", ErrValidation, def.Hash)
	}
	if hv.Tag() != tagForKeyType(def.HashType) {
		return fmt.Errorf("%w: partition key %q type %s != declared %s", ErrValidation, def.Hash, hv.Type(), def.HashType)
	}
	var rv attrval.Value
	if def.Range != "" {
		rv, ok = in.Item[def.Range]
		if !ok {
			return fmt.Errorf("%w: item missing sort key %q", ErrValidation, def.Range)
		}
		if rv.Tag() != tagForKeyType(def.RangeType) {
			return fmt.Errorf("%w: sort key %q type %s != declared %s", ErrValidation, def.Range, rv.Type(), def.RangeType)
		}
	}

	// Item-size limit (JSON byte-length proxy).
	wire, err := json.Marshal(in.Item)
	if err != nil {
		return fmt.Errorf("%w: marshal item: %v", ErrValidation, err)
	}
	if len(wire) > maxItemBytes {
		return fmt.Errorf("%w: item size %d exceeds %d bytes", ErrValidation, len(wire), maxItemBytes)
	}

	// Extract key column values.
	hashVal, err := keyValue(hv)
	if err != nil {
		return err
	}
	var rangeVal any
	if def.Range != "" {
		rangeVal, err = keyValue(rv)
		if err != nil {
			return err
		}
	}

	if err := c.store.PutItem(tx, in.TableName, hashVal, rangeVal, wire); err != nil {
		return err
	}
	return tx.Commit()
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

// DeleteItemInput carries the table name and the exact key to delete.
type DeleteItemInput struct {
	TableName string
	Key       Item
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

	data, found, err := c.store.GetItem(tx, in.TableName, hashVal, rangeVal)
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

func (c *Client) DeleteItem(ctx context.Context, in DeleteItemInput) error {
	tx, err := c.store.BeginTx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	def, err := c.store.GetTableDef(tx, in.TableName)
	if errors.Is(err, storage.ErrNotFound) {
		return fmt.Errorf("%w: table %q not found", ErrTableNotFound, in.TableName)
	}
	if err != nil {
		return err
	}

	hv, rv, err := validateKey(def, in.Key)
	if err != nil {
		return err
	}
	hashVal, err := keyValue(hv)
	if err != nil {
		return err
	}
	var rangeVal any
	if def.Range != "" {
		rangeVal, err = keyValue(rv)
		if err != nil {
			return err
		}
	}
	if _, err := c.store.DeleteItem(tx, in.TableName, hashVal, rangeVal); err != nil {
		return err
	}
	return tx.Commit()
}
