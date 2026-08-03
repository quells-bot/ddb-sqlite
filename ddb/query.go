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
	IndexName                 string
	KeyConditionExpression    string
	FilterExpression          string
	ProjectionExpression      string
	ExpressionAttributeNames  map[string]string
	ExpressionAttributeValues map[string]attrval.Value

	ExclusiveStartKey Item
	Limit             int32
	ScanIndexForward  bool   // true = forward ASC; false = reverse DESC (zero value false). Adapter defaults nil->true.
	ConsistentRead    bool   // accepted, ignored (engine is always consistent)
	Select            string // "" (default: ALL_ATTRIBUTES; ALL_PROJECTED_ATTRIBUTES on a non-ALL GSI without projection), "ALL_ATTRIBUTES", "ALL_PROJECTED_ATTRIBUTES" (GSI only), "COUNT", or "SPECIFIC_ATTRIBUTES" (requires ProjectionExpression)
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
	IndexName                 string
	FilterExpression          string
	ProjectionExpression      string
	ExpressionAttributeNames  map[string]string
	ExpressionAttributeValues map[string]attrval.Value

	ExclusiveStartKey Item
	Limit             int32
	Segment           int32 // 0 when TotalSegments is 0
	TotalSegments     int32 // 0 = non-parallel scan
	ConsistentRead    bool
	Select            string // "" (default: ALL_ATTRIBUTES; ALL_PROJECTED_ATTRIBUTES on a non-ALL GSI without projection), "ALL_ATTRIBUTES", "ALL_PROJECTED_ATTRIBUTES" (GSI only), "COUNT", or "SPECIFIC_ATTRIBUTES" (requires ProjectionExpression)
}

// ScanOutput carries the scanned items, counts, and optional resume key.
type ScanOutput struct {
	Items            []Item
	Count            int32
	ScannedCount     int32
	LastEvaluatedKey Item
}

// validateSelect normalizes Select against the GSI projection type and
// whether a ProjectionExpression is present (spec §4.6). "" defaults to
// ALL_ATTRIBUTES — on a non-ALL GSI without a projection it defaults to
// ALL_PROJECTED_ATTRIBUTES instead. COUNT and ALL_PROJECTED_ATTRIBUTES
// reject an accompanying projection; SPECIFIC_ATTRIBUTES requires one.
// gsiProjection is "" for a base table.
func validateSelect(s string, gsiProjection string, hasProjection bool) (string, error) {
	switch s {
	case "":
		if hasProjection {
			return "ALL_ATTRIBUTES", nil // projection governs returned attrs
		}
		if gsiProjection != "" && gsiProjection != "ALL" {
			return "ALL_PROJECTED_ATTRIBUTES", nil
		}
		return "ALL_ATTRIBUTES", nil
	case "ALL_ATTRIBUTES":
		if hasProjection && gsiProjection != "" && gsiProjection != "ALL" {
			return "", fmt.Errorf("%w: Cannot specify the ProjectionExpression when choosing to get ALL_ATTRIBUTES", ErrValidation)
		}
		if gsiProjection != "" && gsiProjection != "ALL" {
			return "", fmt.Errorf("%w: Select type ALL_ATTRIBUTES is not supported for global secondary index because its projection type is not ALL", ErrValidation)
		}
		return "ALL_ATTRIBUTES", nil
	case "ALL_PROJECTED_ATTRIBUTES":
		if hasProjection {
			return "", fmt.Errorf("%w: Cannot specify the ProjectionExpression when choosing to get ALL_PROJECTED_ATTRIBUTES", ErrValidation)
		}
		if gsiProjection == "" {
			return "", fmt.Errorf("%w: Select type ALL_PROJECTED_ATTRIBUTES is valid only on a global secondary index", ErrValidation)
		}
		return "ALL_PROJECTED_ATTRIBUTES", nil
	case "COUNT":
		if hasProjection {
			return "", fmt.Errorf("%w: Cannot specify the ProjectionExpression when choosing to get only the Count", ErrValidation)
		}
		return "COUNT", nil
	case "SPECIFIC_ATTRIBUTES":
		if !hasProjection {
			return "", fmt.Errorf("%w: Select SPECIFIC_ATTRIBUTES requires ProjectionExpression", ErrValidation)
		}
		return "SPECIFIC_ATTRIBUTES", nil
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

	// Resolve index: base table or GSI.
	var gsiDef storage.GsiDef
	var gsiHash, gsiRange, gsiProjection string
	if in.IndexName != "" {
		gsiDef, err = lookupGsi(def, in.IndexName)
		if err != nil {
			return QueryOutput{}, err
		}
		if in.ConsistentRead {
			return QueryOutput{}, fmt.Errorf("%w: Consistent reads are not supported on global secondary indexes", ErrValidation)
		}
		gsiHash = gsiDef.Hash
		gsiRange = gsiDef.Range
		gsiProjection = gsiDef.ProjectionType
	}

	selectMode, err := validateSelect(in.Select, gsiProjection, in.ProjectionExpression != "")
	if err != nil {
		return QueryOutput{}, err
	}

	// Parse/bind expressions. An empty KeyConditionExpression fails as a parse
	// error → ErrValidation, the same exception type DynamoDB raises.
	ex, err := prepareExpressions(expressionRequest{
		Condition:  in.KeyConditionExpression,
		Filter:     in.FilterExpression,
		Projection: in.ProjectionExpression,
		Names:      in.ExpressionAttributeNames,
		Values:     in.ExpressionAttributeValues,
	})
	if err != nil {
		return QueryOutput{}, err
	}
	if ex.Cond == nil {
		return QueryOutput{}, fmt.Errorf("%w: KeyConditionExpression is required", ErrValidation)
	}

	pkName := def.Hash
	skName := def.Range
	keyType := def.HashType
	rangeType := def.RangeType
	if in.IndexName != "" {
		pkName = gsiHash
		skName = gsiRange
		keyType = gsiDef.HashType
		rangeType = gsiDef.RangeType
	}

	// Extract the key condition from the bound AST.
	kc, err := ex.Cond.ExtractKeyCondition(pkName, skName)
	if err != nil {
		return QueryOutput{}, fmt.Errorf("%w: %v", ErrValidation, err)
	}

	// Validate partition value type.
	if kc.Partition.Value.Tag() != tagForKeyType(keyType) {
		return QueryOutput{}, fmt.Errorf("%w: partition key type mismatch", ErrValidation)
	}

	// Validate sort key: begins_with on N is rejected; sort value types match.
	if kc.Sort != nil {
		if kc.Sort.Op == "BEGINS_WITH" && rangeType == "N" {
			return QueryOutput{}, fmt.Errorf("%w: begins_with is not supported on Number sort keys", ErrValidation)
		}
		sortVal := kc.Sort.Lo
		if kc.Sort.Op == "BEGINS_WITH" {
			sortVal = kc.Sort.BeginsWith
		}
		if sortVal.Tag() != tagForKeyType(rangeType) {
			return QueryOutput{}, fmt.Errorf("%w: sort key type mismatch", ErrValidation)
		}
		if kc.Sort.Op == "BETWEEN" && kc.Sort.Hi.Tag() != tagForKeyType(rangeType) {
			return QueryOutput{}, fmt.Errorf("%w: sort key BETWEEN hi type mismatch", ErrValidation)
		}
	}

	// Filter keys: exclude table keys AND (for GSI) GSI keys.
	filterKeyAttrs := keyAttrs(def)
	if in.IndexName != "" {
		filterKeyAttrs = gsiKeyAttrsForFilter(def, gsiDef)
	}
	if ex.Filter != nil {
		if err := ex.Filter.ValidateFilterKeys(filterKeyAttrs); err != nil {
			return QueryOutput{}, fmt.Errorf("%w: %v", ErrValidation, err)
		}
	}

	// GSI projection restriction: on a GSI read, a ProjectionExpression may
	// name only attributes the index projects (table keys ∪ GSI keys ∪
	// top-level INCLUDE attrs; nested INCLUDE entries contribute nothing —
	// spec §4.7). gsiProjectionAttrs returns nil for ALL (no restriction).
	if in.IndexName != "" && ex.Proj != nil {
		if keep := gsiProjectionAttrs(def, gsiDef); keep != nil {
			for _, p := range ex.Proj.Paths() {
				if !keep[p[0].Name] {
					return QueryOutput{}, fmt.Errorf("%w: One or more parameter values were invalid: Global secondary index %s does not project [%s]", ErrValidation, in.IndexName, p[0].Name)
				}
			}
		}
	}

	// Translate to storage space.
	hashVal, err := keyValue(kc.Partition.Value)
	if err != nil {
		return QueryOutput{}, err
	}

	if in.Limit < 0 {
		return QueryOutput{}, fmt.Errorf("%w: Limit must be non-negative", ErrValidation)
	}
	limit := int(in.Limit)

	var blobs [][]byte
	if in.IndexName != "" {
		// GSI Query path.
		var sortCond *storage.SortKeyCond
		if gsiRange != "" {
			sortCond, err = translateSortCond(kc.Sort, storage.TableDef{RangeType: rangeType}, in.ScanIndexForward)
			if err != nil {
				return QueryOutput{}, err
			}
		}
		var resume *storage.GsiResume
		if len(in.ExclusiveStartKey) > 0 {
			resume, err = c.validateGsiExclusiveStartKey(tx, def, gsiDef, in.ExclusiveStartKey, kc)
			if err != nil {
				return QueryOutput{}, fmt.Errorf("%w: invalid ExclusiveStartKey: %v", ErrValidation, err)
			}
		}
		blobs, err = c.store.QueryGSI(tx, in.TableName, in.IndexName, hashVal, sortCond, resume, in.ScanIndexForward, limit)
		if err != nil {
			return QueryOutput{}, err
		}
	} else {
		var sortCond *storage.SortKeyCond
		if def.Range != "" {
			sortCond, err = translateSortCond(kc.Sort, def, in.ScanIndexForward)
			if err != nil {
				return QueryOutput{}, err
			}
		}
		if len(in.ExclusiveStartKey) > 0 {
			lekHash, lekRange, eerr := validateKey(def, in.ExclusiveStartKey)
			if eerr != nil {
				return QueryOutput{}, fmt.Errorf("%w: invalid ExclusiveStartKey: %v", ErrValidation, eerr)
			}
			if !lekHash.Equal(kc.Partition.Value) {
				return QueryOutput{}, fmt.Errorf("%w: ExclusiveStartKey partition does not match KeyConditionExpression", ErrValidation)
			}
			if def.Range != "" {
				resumeVal, rerr := keyValue(lekRange)
				if rerr != nil {
					return QueryOutput{}, rerr
				}
				if sortCond == nil {
					sortCond = &storage.SortKeyCond{Op: "", ResumeAfter: resumeVal}
				} else {
					sortCond.ResumeAfter = resumeVal
				}
			}
		}
		blobs, err = c.store.Query(tx, in.TableName, hashVal, sortCond, in.ScanIndexForward, limit)
		if err != nil {
			return QueryOutput{}, err
		}
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
			ok, ferr := ex.Filter.Eval(item)
			if ferr != nil {
				return QueryOutput{}, fmt.Errorf("%w: filter eval: %v", ErrValidation, ferr)
			}
			keep = ok
		}
		if keep {
			count++
			if ex.Proj != nil {
				item = attrval.Project(item, ex.Proj.Paths())
			}
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
		if in.IndexName != "" {
			lek = gsiLastEvaluatedKey(def, gsiDef, lastItem)
		} else {
			lek = Item{def.Hash: lastItem[def.Hash]}
			if def.Range != "" {
				lek[def.Range] = lastItem[def.Range]
			}
		}
	}

	// Projection trimming for GSI reads.
	if in.IndexName != "" {
		keep := gsiProjectionAttrs(def, gsiDef)
		for i := range items {
			items[i] = projectItem(items[i], keep)
		}
	}

	if selectMode == "COUNT" || selectMode == "ALL_PROJECTED_ATTRIBUTES" {
		// ALL_PROJECTED_ATTRIBUTES behaves like the default GSI projection (already trimmed).
		// COUNT drops items.
		if selectMode == "COUNT" {
			items = nil
		}
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

	var gsiDef storage.GsiDef
	var gsiProjection string
	if in.IndexName != "" {
		gsiDef, err = lookupGsi(def, in.IndexName)
		if err != nil {
			return ScanOutput{}, err
		}
		if in.ConsistentRead {
			return ScanOutput{}, fmt.Errorf("%w: Consistent reads are not supported on global secondary indexes", ErrValidation)
		}
		gsiProjection = gsiDef.ProjectionType
		// GSI parallel scan is a deliberate M4 scope cut.
		if in.TotalSegments > 1 {
			return ScanOutput{}, fmt.Errorf("%w: parallel scan on a global secondary index is not supported", ErrValidation)
		}
	}

	selectMode, err := validateSelect(in.Select, gsiProjection, in.ProjectionExpression != "")
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

	// Parse/bind expressions. The gate fires on ANY expression or substitution
	// map — not just the filter — so the unused-check rejects names-only and
	// values-only requests symmetrically (spec §3.3).
	var ex preparedExpressions
	if in.FilterExpression != "" || in.ProjectionExpression != "" || len(in.ExpressionAttributeNames) > 0 || len(in.ExpressionAttributeValues) > 0 {
		ex, err = prepareExpressions(expressionRequest{
			Filter:     in.FilterExpression,
			Projection: in.ProjectionExpression,
			Names:      in.ExpressionAttributeNames,
			Values:     in.ExpressionAttributeValues,
		})
		if err != nil {
			return ScanOutput{}, err
		}
		filterKeyAttrs := keyAttrs(def)
		if in.IndexName != "" {
			filterKeyAttrs = gsiKeyAttrsForFilter(def, gsiDef)
		}
		if ex.Filter != nil {
			if err := ex.Filter.ValidateFilterKeys(filterKeyAttrs); err != nil {
				return ScanOutput{}, fmt.Errorf("%w: %v", ErrValidation, err)
			}
		}
		// GSI projection restriction — identical to Query (spec §4.4).
		if in.IndexName != "" && ex.Proj != nil {
			if keep := gsiProjectionAttrs(def, gsiDef); keep != nil {
				for _, p := range ex.Proj.Paths() {
					if !keep[p[0].Name] {
						return ScanOutput{}, fmt.Errorf("%w: One or more parameter values were invalid: Global secondary index %s does not project [%s]", ErrValidation, in.IndexName, p[0].Name)
					}
				}
			}
		}
	}

	// Build resume cursor: resolve ExclusiveStartKey to a rowid.
	var afterID int64
	if len(in.ExclusiveStartKey) > 0 {
		if in.IndexName != "" {
			// GSI Scan ESK: validate the union of table+GSI key attrs and resolve
			// the table key to a data_id (see resolveGsiScanAfterID).
			afterID, err = c.resolveGsiScanAfterID(tx, def, gsiDef, in.ExclusiveStartKey)
			if err != nil {
				return ScanOutput{}, fmt.Errorf("%w: invalid ExclusiveStartKey: %v", ErrValidation, err)
			}
		} else {
			lekHash, lekRange, eerr := validateKey(def, in.ExclusiveStartKey)
			if eerr != nil {
				return ScanOutput{}, fmt.Errorf("%w: invalid ExclusiveStartKey: %v", ErrValidation, eerr)
			}
			hashVal, herr := keyValue(lekHash)
			if herr != nil {
				return ScanOutput{}, herr
			}
			var rangeVal any
			if def.Range != "" && lekRange.Tag() != attrval.TagNull {
				rangeVal, herr = keyValue(lekRange)
				if herr != nil {
					return ScanOutput{}, herr
				}
			}
			id, _, found, gerr := c.store.GetItem(tx, in.TableName, hashVal, rangeVal)
			if gerr != nil {
				return ScanOutput{}, gerr
			}
			if found {
				afterID = id
			}
			// Stale key (not found): scan starts from beginning. Matches DynamoDB.
		}
	}

	// Limit: 0 = unset (unlimited); negative is rejected.
	if in.Limit < 0 {
		return ScanOutput{}, fmt.Errorf("%w: Limit must be non-negative", ErrValidation)
	}
	limit := int(in.Limit)

	var blobs [][]byte
	if in.IndexName != "" {
		blobs, err = c.store.ScanGSI(tx, in.TableName, in.IndexName, afterID, limit)
	} else {
		blobs, err = c.store.Scan(tx, in.TableName, segment, totalSegments, afterID, limit)
	}
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
			ok, ferr := ex.Filter.Eval(item)
			if ferr != nil {
				return ScanOutput{}, fmt.Errorf("%w: filter eval: %v", ErrValidation, ferr)
			}
			keep = ok
		}
		if keep {
			count++
			if ex.Proj != nil {
				item = attrval.Project(item, ex.Proj.Paths())
			}
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
		if in.IndexName != "" {
			lek = gsiLastEvaluatedKey(def, gsiDef, lastItem)
		} else {
			lek = Item{def.Hash: lastItem[def.Hash]}
			if def.Range != "" {
				lek[def.Range] = lastItem[def.Range]
			}
		}
	}

	// Projection trimming for GSI reads.
	if in.IndexName != "" {
		keep := gsiProjectionAttrs(def, gsiDef)
		for i := range items {
			items[i] = projectItem(items[i], keep)
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
