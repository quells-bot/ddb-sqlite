package ddb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/quells-bot/ddb-sqlite/attrval"
	"github.com/quells-bot/ddb-sqlite/internal/expr"
	"github.com/quells-bot/ddb-sqlite/internal/storage"
)

// UpdateItemInput carries the key to update, the update and condition
// expressions, and the ReturnValues mode. An absent key is created (upsert):
// the result is the key attributes plus the update's effects.
type UpdateItemInput struct {
	TableName string
	Key       Item

	UpdateExpression          string
	ConditionExpression       string
	ExpressionAttributeNames  map[string]string
	ExpressionAttributeValues map[string]attrval.Value

	// ReturnValues accepts NONE (default), ALL_OLD, ALL_NEW, UPDATED_OLD, or
	// UPDATED_NEW.
	ReturnValues string
	// ReturnValuesOnConditionCheckFailure accepts NONE (default) or ALL_OLD;
	// ALL_OLD populates ConditionalCheckFailedError.Item.
	ReturnValuesOnConditionCheckFailure string
}

// UpdateItemOutput carries the projection selected by ReturnValues.
type UpdateItemOutput struct {
	Attributes Item
}

// keyAttrs returns the table's key attribute names.
func keyAttrs(def storage.TableDef) []string {
	if def.Range == "" {
		return []string{def.Hash}
	}
	return []string{def.Hash, def.Range}
}

// UpdateItem reads, modifies, and writes one item inside a single transaction.
// Because storage runs on a serialized single-connection pool, holding the tx
// for the whole read-modify-write is what makes the operation atomic; no extra
// locking is needed.
func (c *Client) UpdateItem(ctx context.Context, in UpdateItemInput) (UpdateItemOutput, error) {
	tx, err := c.store.BeginTx(ctx)
	if err != nil {
		return UpdateItemOutput{}, err
	}
	defer tx.Rollback()

	def, err := c.store.GetTableDef(tx, in.TableName)
	if errors.Is(err, storage.ErrNotFound) {
		return UpdateItemOutput{}, fmt.Errorf("%w: table %q not found", ErrTableNotFound, in.TableName)
	}
	if err != nil {
		return UpdateItemOutput{}, err
	}

	hv, rk, err := validateKey(def, in.Key)
	if err != nil {
		return UpdateItemOutput{}, err
	}

	// Expressions are validated only after the table and key. DynamoDB reports
	// a missing table as ResourceNotFoundException even when the request also
	// carries a malformed expression or an unsupported ReturnValues mode
	// (design spec §6.2 steps 2-4). Parsing still precedes the row read, so a
	// malformed expression fails whether or not the item exists.
	rv, err := validateReturnValuesUpdate(in.ReturnValues)
	if err != nil {
		return UpdateItemOutput{}, err
	}
	roc, err := validateReturnValuesOnConditionCheckFailure(in.ReturnValuesOnConditionCheckFailure)
	if err != nil {
		return UpdateItemOutput{}, err
	}
	ex, err := prepareExpressions(expressionRequest{
		Condition: in.ConditionExpression,
		Update:    in.UpdateExpression,
		Names:     in.ExpressionAttributeNames,
		Values:    in.ExpressionAttributeValues,
	})
	if err != nil {
		return UpdateItemOutput{}, err
	}
	if ex.Update != nil {
		// Key attributes are immutable; the check lives here because this is
		// where the TableDef is.
		if err := ex.Update.ValidateKeyAttrs(keyAttrs(def)); err != nil {
			return UpdateItemOutput{}, fmt.Errorf("%w: UpdateExpression: %v", ErrValidation, err)
		}
	}

	hashVal, err := keyValue(hv)
	if err != nil {
		return UpdateItemOutput{}, err
	}
	var rangeVal any
	if def.Range != "" {
		rangeVal, err = keyValue(rk)
		if err != nil {
			return UpdateItemOutput{}, err
		}
	}

	old, err := c.readItem(tx, in.TableName, hashVal, rangeVal)
	if err != nil {
		return UpdateItemOutput{}, err
	}
	if err := checkCondition(ex.Cond, old, roc); err != nil {
		return UpdateItemOutput{}, err
	}

	// Apply is documented to never mutate its input, but base is also what
	// projectReturnValues reads back as `old` and what ConditionalCheckFailedError
	// carries; shallow-copy so a future Apply refactor cannot silently corrupt it.
	var base Item
	if old != nil {
		base = make(Item, len(old))
		for k, v := range old {
			base[k] = v
		}
	} else {
		// Upsert: an absent key starts from the key attributes alone.
		base = Item{}
		for k, v := range in.Key {
			base[k] = v
		}
	}

	updated := base
	var touched []expr.TouchedAttribute
	if ex.Update != nil {
		res, tp, err := ex.Update.Apply(base)
		if err != nil {
			return UpdateItemOutput{}, fmt.Errorf("%w: UpdateExpression: %v", ErrValidation, err)
		}
		updated = res
		touched = tp
	}
	if err := ensureKeyIntact(def, in.Key, updated); err != nil {
		return UpdateItemOutput{}, err
	}

	wire, err := json.Marshal(updated)
	if err != nil {
		return UpdateItemOutput{}, fmt.Errorf("%w: marshal item: %v", ErrValidation, err)
	}
	if len(wire) > maxItemBytes {
		return UpdateItemOutput{}, fmt.Errorf("%w: item size %d exceeds %d bytes", ErrValidation, len(wire), maxItemBytes)
	}
	// GSI key validation on the post-write item (atomic rejection).
	if err := validateGsiKeys(updated, def.GSIs); err != nil {
		return UpdateItemOutput{}, err
	}

	dataID, err := c.store.PutItem(tx, in.TableName, hashVal, rangeVal, wire)
	if err != nil {
		return UpdateItemOutput{}, err
	}
	if err := c.maintainGsiRows(tx, in.TableName, def.GSIs, dataID, updated); err != nil {
		return UpdateItemOutput{}, err
	}
	if err := tx.Commit(); err != nil {
		return UpdateItemOutput{}, err
	}

	return UpdateItemOutput{Attributes: projectReturnValues(rv, old, updated, touched)}, nil
}

// ensureKeyIntact re-checks that the applied update left the key attributes
// exactly as supplied. ValidateKeyAttrs already rejects any action that targets
// a key attribute, so this is the write path's safety net rather than the
// primary check — the row's indexed key columns come from in.Key, and a
// mismatch between them and the stored blob would be a silent corruption.
func ensureKeyIntact(def storage.TableDef, key, updated Item) error {
	for _, name := range keyAttrs(def) {
		want, ok := key[name]
		if !ok {
			continue
		}
		got, ok := updated[name]
		if !ok || !got.Equal(want) {
			return fmt.Errorf("%w: update changed key attribute %q", ErrValidation, name)
		}
	}
	return nil
}
