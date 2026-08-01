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

// QueryInput queries one partition by key condition, optionally filtered.
type QueryInput struct {
	TableName                 string
	KeyConditionExpression    string
	FilterExpression          string
	ExpressionAttributeNames  map[string]string
	ExpressionAttributeValues map[string]attrval.Value

	ExclusiveStartKey Item
	Limit             int32
	ScanIndexForward  bool   // default true (ASC)
	ConsistentRead    bool   // accepted, ignored (engine is always consistent)
	Select            string // "" (default ALL_ATTRIBUTES) or "COUNT"
}

// QueryOutput carries the matching items, counts, and optional resume key.
type QueryOutput struct {
	Items            []Item
	Count            int32 // items passing the filter
	ScannedCount     int32 // items scanned (before filter)
	LastEvaluatedKey Item
}

// ScanInput scans a full table (or one parallel segment).
type ScanInput struct {
	TableName                 string
	FilterExpression          string
	ExpressionAttributeNames  map[string]string
	ExpressionAttributeValues map[string]attrval.Value

	ExclusiveStartKey Item
	Limit             int32
	Segment           int32 // 0 when TotalSegments is 0
	TotalSegments     int32 // 0 = non-parallel scan
	ConsistentRead    bool
	Select            string
}

// ScanOutput carries the scanned items, counts, and optional resume key.
type ScanOutput struct {
	Items            []Item
	Count            int32
	ScannedCount     int32
	LastEvaluatedKey Item
}

// validateSelect normalizes "" to "ALL_ATTRIBUTES". Accepts "ALL_ATTRIBUTES"
// and "COUNT". Rejects "SPECIFIC_ATTRIBUTES" and "ALL_PROJECTED_ATTRIBUTES"
// (need ProjectionExpression / GSI — both v1 non-goals). Case-sensitive.
func validateSelect(s string) (string, error) {
	switch s {
	case "", "ALL_ATTRIBUTES":
		return "ALL_ATTRIBUTES", nil
	case "COUNT":
		return "COUNT", nil
	case "SPECIFIC_ATTRIBUTES", "ALL_PROJECTED_ATTRIBUTES":
		return "", fmt.Errorf("%w: Select %q requires ProjectionExpression or a GSI", ErrValidation, s)
	default:
		return "", fmt.Errorf("%w: Select %q is not a valid value", ErrValidation, s)
	}
}

// beginsWithSuccessor computes the lexicographic successor of a byte prefix.
// The successor is the smallest byte string strictly greater than the prefix,
// used as the upper bound for begins_with's half-open range. When every byte
// is 0xFF (or the prefix is empty), no successor exists and nil is returned
// so storage emits only "range >= ?".
func beginsWithSuccessor(prefix []byte) any {
	if len(prefix) == 0 {
		return nil
	}
	succ := make([]byte, len(prefix))
	copy(succ, prefix)
	// Walk back from the last byte; if it's < 0xFF, increment it and return.
	// If it's 0xFF, drop it and carry left.
	for i := len(succ) - 1; i >= 0; i-- {
		if succ[i] < 0xFF {
			succ[i]++
			return succ[:i+1]
		}
		// succ[i] == 0xFF — carry left (truncate this byte).
	}
	// All bytes were 0xFF.
	return nil
}

// Query selects items from one partition by key condition, optionally filtered.
// See M3 spec §4.5 for the operation flow.
func (c *Client) Query(ctx context.Context, in QueryInput) (QueryOutput, error) {
	tx, err := c.store.BeginTx(ctx)
	if err != nil {
		return QueryOutput{}, err
	}
	defer tx.Rollback()

	def, err := c.store.GetTableDef(tx, in.TableName)
	if errors.Is(err, storage.ErrNotFound) {
		return QueryOutput{}, fmt.Errorf("%w: table %q not found", ErrTableNotFound, in.TableName)
	}
	if err != nil {
		return QueryOutput{}, err
	}

	selectMode, err := validateSelect(in.Select)
	if err != nil {
		return QueryOutput{}, err
	}

	// IndexName is rejected in M3 (GSI is M4).
	// (QueryInput has no IndexName field yet — this check is a no-op until M4
	// adds the field. The adapter checks IndexName before calling the engine.)

	// Parse/bind expressions. An empty KeyConditionExpression fails as a parse
	// error → ErrValidation, the same exception type DynamoDB raises.
	ex, err := prepareExpressions(expressionRequest{
		Condition: in.KeyConditionExpression,
		Filter:    in.FilterExpression,
		Names:     in.ExpressionAttributeNames,
		Values:    in.ExpressionAttributeValues,
	})
	if err != nil {
		return QueryOutput{}, err
	}
	if ex.Cond == nil {
		return QueryOutput{}, fmt.Errorf("%w: KeyConditionExpression is required", ErrValidation)
	}

	// Extract the key condition from the bound AST.
	kc, err := ex.Cond.ExtractKeyCondition(def.Hash, def.Range)
	if err != nil {
		return QueryOutput{}, fmt.Errorf("%w: %v", ErrValidation, err)
	}

	// Validate partition value type.
	if kc.Partition.Value.Tag() != tagForKeyType(def.HashType) {
		return QueryOutput{}, fmt.Errorf("%w: partition key type mismatch", ErrValidation)
	}

	// Validate sort key: begins_with on N is rejected; sort value types match.
	if kc.Sort != nil {
		if kc.Sort.Op == "BEGINS_WITH" && def.RangeType == "N" {
			return QueryOutput{}, fmt.Errorf("%w: begins_with is not supported on Number sort keys", ErrValidation)
		}
		sortVal := kc.Sort.Lo
		if kc.Sort.Op == "BEGINS_WITH" {
			sortVal = kc.Sort.BeginsWith
		}
		if sortVal.Tag() != tagForKeyType(def.RangeType) {
			return QueryOutput{}, fmt.Errorf("%w: sort key type mismatch", ErrValidation)
		}
		if kc.Sort.Op == "BETWEEN" && kc.Sort.Hi.Tag() != tagForKeyType(def.RangeType) {
			return QueryOutput{}, fmt.Errorf("%w: sort key BETWEEN hi type mismatch", ErrValidation)
		}
	}

	// ValidateFilterKeys if a filter is present.
	if ex.Filter != nil {
		if err := ex.Filter.ValidateFilterKeys(keyAttrs(def)); err != nil {
			return QueryOutput{}, fmt.Errorf("%w: %v", ErrValidation, err)
		}
	}

	// Translate to storage space.
	hashVal, err := keyValue(kc.Partition.Value)
	if err != nil {
		return QueryOutput{}, err
	}

	var sortCond *storage.SortKeyCond
	if def.Range != "" {
		sortCond, err = translateSortCond(kc.Sort, def, in.ScanIndexForward)
		if err != nil {
			return QueryOutput{}, err
		}
	}

	// Build resume cursor from ExclusiveStartKey.
	if len(in.ExclusiveStartKey) > 0 {
		lekHash, lekRange, err := validateKey(def, in.ExclusiveStartKey)
		if err != nil {
			return QueryOutput{}, fmt.Errorf("%w: invalid ExclusiveStartKey: %v", ErrValidation, err)
		}
		// Partition must match the key condition's partition.
		if !lekHash.Equal(kc.Partition.Value) {
			return QueryOutput{}, fmt.Errorf("%w: ExclusiveStartKey partition does not match KeyConditionExpression", ErrValidation)
		}
		if def.Range != "" {
			resumeVal, err := keyValue(lekRange)
			if err != nil {
				return QueryOutput{}, err
			}
			if sortCond == nil {
				sortCond = &storage.SortKeyCond{Op: "", ResumeAfter: resumeVal}
			} else {
				sortCond.ResumeAfter = resumeVal
			}
		}
	}

	// Limit: 0 = unset (unlimited); negative is rejected.
	if in.Limit < 0 {
		return QueryOutput{}, fmt.Errorf("%w: Limit must be non-negative", ErrValidation)
	}
	limit := int(in.Limit)

	// Execute.
	blobs, err := c.store.Query(tx, in.TableName, hashVal, sortCond, in.ScanIndexForward, limit)
	if err != nil {
		return QueryOutput{}, err
	}

	// Process scanned items.
	var items []Item
	var scanned, count int32
	for _, blob := range blobs {
		var item Item
		if err := json.Unmarshal(blob, &item); err != nil {
			return QueryOutput{}, fmt.Errorf("%w: unmarshal scanned item: %v", ErrValidation, err)
		}
		scanned++
		keep := true
		if ex.Filter != nil {
			ok, err := ex.Filter.Eval(item)
			if err != nil {
				return QueryOutput{}, fmt.Errorf("%w: filter eval: %v", ErrValidation, err)
			}
			keep = ok
		}
		if keep {
			count++
			items = append(items, item)
		}
	}

	// Build LEK: set iff ScannedCount == Limit (Limit reached, not exhausted).
	var lek Item
	if limit > 0 && scanned == int32(limit) && len(blobs) > 0 {
		// LEK is the key of the last scanned item (may have been filtered out).
		var lastItem Item
		if err := json.Unmarshal(blobs[len(blobs)-1], &lastItem); err != nil {
			return QueryOutput{}, fmt.Errorf("%w: unmarshal LEK item: %v", ErrValidation, err)
		}
		lek = Item{def.Hash: lastItem[def.Hash]}
		if def.Range != "" {
			lek[def.Range] = lastItem[def.Range]
		}
	}

	if selectMode == "COUNT" {
		items = nil
	}

	if err := tx.Commit(); err != nil {
		return QueryOutput{}, err
	}

	return QueryOutput{
		Items:            items,
		Count:            count,
		ScannedCount:     scanned,
		LastEvaluatedKey: lek,
	}, nil
}

// translateSortCond converts an expr.SortKeyCond (attrval.Value operands) to a
// storage.SortKeyCond (Go-typed operands). When sort is nil — a
// partition-equality-only query — it returns a non-nil empty-op cond so
// storage takes the sort-key-table path (all rows in the partition, ordered by
// range) and the ExclusiveStartKey resume code below can attach ResumeAfter.
// It is only called when def.Range != "", so this is never a partition-only
// table.
func translateSortCond(sort *expr.SortKeyCond, def storage.TableDef, scanForward bool) (*storage.SortKeyCond, error) {
	if sort == nil {
		return &storage.SortKeyCond{Op: ""}, nil
	}
	sc := &storage.SortKeyCond{Op: sort.Op}
	switch sort.Op {
	case "BEGINS_WITH":
		lo, err := keyValue(sort.BeginsWith)
		if err != nil {
			return nil, err
		}
		sc.Lo = lo
		// Compute the successor for the upper bound. The successor must be bound
		// with the same Go type as the sort-key value so SQLite binds it with the
		// same column affinity: an S key is a TEXT column, so the (string)
		// successor must stay a string, not a []byte (a []byte binds as BLOB and
		// SQLite's type comparison always ranks BLOB > TEXT, silently dropping
		// the upper bound). failsafe: if there is no successor (nil), leave
		// sc.Hi nil so storage emits only "range >= ?".
		switch v := lo.(type) {
		case string:
			if succ := beginsWithSuccessor([]byte(v)); succ != nil {
				sc.Hi = string(succ.([]byte))
			}
		case []byte:
			sc.Hi = beginsWithSuccessor(v) // B column is BLOB — correct as-is
		default:
			// Number sort key — begins_with on N is rejected earlier.
			return nil, fmt.Errorf("%w: begins_with on non-string/binary sort key", ErrValidation)
		}
	case "BETWEEN":
		lo, err := keyValue(sort.Lo)
		if err != nil {
			return nil, err
		}
		hi, err := keyValue(sort.Hi)
		if err != nil {
			return nil, err
		}
		sc.Lo, sc.Hi = lo, hi
	default: // "=", "<", "<=", ">", ">="
		lo, err := keyValue(sort.Lo)
		if err != nil {
			return nil, err
		}
		sc.Lo = lo
	}
	return sc, nil
}

// Scan selects all items from a table (or one parallel segment), optionally
// filtered. See M3 spec §4.6 for the operation flow.
func (c *Client) Scan(ctx context.Context, in ScanInput) (ScanOutput, error) {
	tx, err := c.store.BeginTx(ctx)
	if err != nil {
		return ScanOutput{}, err
	}
	defer tx.Rollback()

	def, err := c.store.GetTableDef(tx, in.TableName)
	if errors.Is(err, storage.ErrNotFound) {
		return ScanOutput{}, fmt.Errorf("%w: table %q not found", ErrTableNotFound, in.TableName)
	}
	if err != nil {
		return ScanOutput{}, err
	}

	selectMode, err := validateSelect(in.Select)
	if err != nil {
		return ScanOutput{}, err
	}

	// Parallel scan validation.
	segment, totalSegments := int(in.Segment), int(in.TotalSegments)
	if totalSegments < 0 {
		return ScanOutput{}, fmt.Errorf("%w: TotalSegments must be non-negative", ErrValidation)
	}
	if totalSegments > 0 {
		if segment < 0 || segment >= totalSegments {
			return ScanOutput{}, fmt.Errorf("%w: Segment must be in [0, TotalSegments)", ErrValidation)
		}
	} else if segment != 0 {
		return ScanOutput{}, fmt.Errorf("%w: Segment must be 0 when TotalSegments is 0", ErrValidation)
	}

	// Parse/bind filter.
	var ex preparedExpressions
	if in.FilterExpression != "" {
		ex, err = prepareExpressions(expressionRequest{
			Filter: in.FilterExpression,
			Names:  in.ExpressionAttributeNames,
			Values: in.ExpressionAttributeValues,
		})
		if err != nil {
			return ScanOutput{}, err
		}
		if ex.Filter != nil {
			if err := ex.Filter.ValidateFilterKeys(keyAttrs(def)); err != nil {
				return ScanOutput{}, fmt.Errorf("%w: %v", ErrValidation, err)
			}
		}
	}

	// Build resume cursor: resolve ExclusiveStartKey to a rowid.
	var afterID int64
	if len(in.ExclusiveStartKey) > 0 {
		lekHash, lekRange, err := validateKey(def, in.ExclusiveStartKey)
		if err != nil {
			return ScanOutput{}, fmt.Errorf("%w: invalid ExclusiveStartKey: %v", ErrValidation, err)
		}
		hashVal, err := keyValue(lekHash)
		if err != nil {
			return ScanOutput{}, err
		}
		var rangeVal any
		if def.Range != "" && lekRange.Tag() != attrval.TagNull {
			rangeVal, err = keyValue(lekRange)
			if err != nil {
				return ScanOutput{}, err
			}
		}
		id, _, found, err := c.store.GetItem(tx, in.TableName, hashVal, rangeVal)
		if err != nil {
			return ScanOutput{}, err
		}
		if found {
			afterID = id
		}
		// Stale key (not found): scan starts from beginning. Matches DynamoDB.
	}

	// Limit: 0 = unset (unlimited); negative is rejected.
	if in.Limit < 0 {
		return ScanOutput{}, fmt.Errorf("%w: Limit must be non-negative", ErrValidation)
	}
	limit := int(in.Limit)

	blobs, err := c.store.Scan(tx, in.TableName, segment, totalSegments, afterID, limit)
	if err != nil {
		return ScanOutput{}, err
	}

	var items []Item
	var scanned, count int32
	for _, blob := range blobs {
		var item Item
		if err := json.Unmarshal(blob, &item); err != nil {
			return ScanOutput{}, fmt.Errorf("%w: unmarshal scanned item: %v", ErrValidation, err)
		}
		scanned++
		keep := true
		if ex.Filter != nil {
			ok, err := ex.Filter.Eval(item)
			if err != nil {
				return ScanOutput{}, fmt.Errorf("%w: filter eval: %v", ErrValidation, err)
			}
			keep = ok
		}
		if keep {
			count++
			items = append(items, item)
		}
	}

	// Build LEK: set iff ScannedCount == Limit.
	var lek Item
	if limit > 0 && scanned == int32(limit) && len(blobs) > 0 {
		var lastItem Item
		if err := json.Unmarshal(blobs[len(blobs)-1], &lastItem); err != nil {
			return ScanOutput{}, fmt.Errorf("%w: unmarshal LEK item: %v", ErrValidation, err)
		}
		lek = Item{def.Hash: lastItem[def.Hash]}
		if def.Range != "" {
			lek[def.Range] = lastItem[def.Range]
		}
	}

	if selectMode == "COUNT" {
		items = nil
	}

	if err := tx.Commit(); err != nil {
		return ScanOutput{}, err
	}

	return ScanOutput{
		Items:            items,
		Count:            count,
		ScannedCount:     scanned,
		LastEvaluatedKey: lek,
	}, nil
}
