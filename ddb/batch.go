// Package ddb is the importable engine surface. This file adds the M5b batch
// operations: BatchWriteItem (multi-table puts/deletes on one atomic tx) and
// BatchGetItem (multi-table key fetch on one read tx). Both pre-validate the
// entire request, then apply — any validation failure rejects the whole batch
// (no partial processing, probe-confirmed against dynamodb-local; see
// docs/superpowers/specs/2026-08-02-m5b-batch-design.md §3). v1 has no
// throttling, so BatchWriteItem's UnprocessedItems is always empty;
// BatchGetItem enforces the 16MiB response cap (M6c W6), spilling overflow
// keys to UnprocessedKeys.
package ddb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/quells-bot/ddb-sqlite-core/attrval"
	"github.com/quells-bot/ddb-sqlite-core/internal/expr"
	"github.com/quells-bot/ddb-sqlite-core/internal/storage"
)

// WriteRequest is one item-level action in a batch write: either a Put or a
// Delete. Exactly one field must be non-nil — the engine rejects any other
// shape (both nil, both set) with ErrValidation.
type WriteRequest struct {
	Put    *PutRequest
	Delete *DeleteRequest
}

// PutRequest carries the item to put (must include all key attributes).
type PutRequest struct {
	Item Item
}

// DeleteRequest carries the exact key to delete.
type DeleteRequest struct {
	Key Item
}

// KeysAndAttributes carries the per-table key list for a batch get.
// ConsistentRead is accepted and ignored (the engine is always consistent).
// ProjectionExpression (+ ExpressionAttributeNames) limits each returned item
// to the named document paths. The legacy AttributesToGet parameter is
// rejected with ErrValidation (deprecated; deliberate divergence — the
// reference honors it alone).
type KeysAndAttributes struct {
	Keys                     []Item
	ConsistentRead           bool
	ProjectionExpression     string
	ExpressionAttributeNames map[string]string
	AttributesToGet          []string
}

// BatchWriteItemInput carries per-table write request lists.
type BatchWriteItemInput struct {
	RequestItems map[string][]WriteRequest
}

// BatchWriteItemOutput carries unprocessed requests. In v1 (no throttling),
// UnprocessedItems is always empty (nil).
type BatchWriteItemOutput struct {
	UnprocessedItems map[string][]WriteRequest
}

// BatchGetItemInput carries per-table key lists.
type BatchGetItemInput struct {
	RequestItems map[string]KeysAndAttributes
}

// BatchGetItemOutput carries the per-table found items and unprocessed keys.
// Responses is non-nil and contains an entry for every requested table; a
// table whose keys all miss has an empty slice (matching dynamodb-local).
// UnprocessedKeys is nil unless the 16MiB response cap trips (M6c W6); each
// spilled entry then echoes the request's ConsistentRead,
// ProjectionExpression, and ExpressionAttributeNames, and its Keys are the
// request keys whose items were not returned, sorted by key ascending.
type BatchGetItemOutput struct {
	Responses       map[string][]Item
	UnprocessedKeys map[string]KeysAndAttributes
}

const (
	// maxBatchWriteRequests is DynamoDB's per-call BatchWriteItem cap.
	// The 16MB aggregate cap is unreachable (25 × 400KB item cap = 10MB)
	// and is deliberately not enforced (spec §1.1.1).
	maxBatchWriteRequests = 25
	// maxBatchGetKeys is DynamoDB's per-call BatchGetItem key cap.
	maxBatchGetKeys = 100
	// maxBatchGetResponseBytes is DynamoDB's per-call BatchGetItem response
	// cap: exactly 16 MiB (M6c W6, boundary probe-pinned by P-batch). Found
	// items are measured by W1 accounting (itemSize) PRE-projection, and the
	// budget is whole-response — one accumulator across all tables.
	maxBatchGetResponseBytes int64 = 16 * 1024 * 1024
)

// normalizedKeyJSON renders an item's key attributes (hash plus optional
// range) as canonical wire JSON for per-table duplicate detection. Map keys
// sort under encoding/json and num.Parse canonicalizes at parse time, so
// equal keys — including numeric spelling variants like 1 vs 1.0 — marshal
// to identical bytes (spec §3).
func normalizedKeyJSON(def storage.TableDef, item Item) ([]byte, error) {
	norm := make(Item, mapSizeForKeys(def))
	norm[def.Hash] = item[def.Hash]
	if def.Range != "" {
		norm[def.Range] = item[def.Range]
	}
	b, err := json.Marshal(norm)
	if err != nil {
		return nil, fmt.Errorf("%w: marshal key: %v", ErrValidation, err)
	}
	return b, nil
}

// compareItems orders two items by their primary key ascending (hash, then
// range when present), reading key values from the item using the table's
// declared key types: S compares strings lexically, N compares numerics via
// num.Decimal.Compare, B compares bytes via bytes.Compare. Returns -1, 0, or 1.
func compareItems(def storage.TableDef, a, b Item) int {
	if c := compareKeyValues(def.HashType, a[def.Hash], b[def.Hash]); c != 0 {
		return c
	}
	if def.Range != "" {
		return compareKeyValues(def.RangeType, a[def.Range], b[def.Range])
	}
	return 0
}

// compareKeyValues compares two attrval.Values of a declared key type.
func compareKeyValues(typ string, x, y attrval.Value) int {
	switch typ {
	case "S":
		return strings.Compare(x.Str(), y.Str())
	case "N":
		return x.Num().Compare(y.Num())
	case "B":
		return bytes.Compare(x.Bin(), y.Bin())
	}
	return 0
}

// BatchWriteItem applies put/delete requests across tables on a single tx.
// Phase 1 validates the entire request; phase 2 applies. Any failure rolls
// the whole batch back — no partial processing.
func (c *Client) BatchWriteItem(ctx context.Context, in BatchWriteItemInput) (BatchWriteItemOutput, error) {
	total := 0
	for _, reqs := range in.RequestItems {
		total += len(reqs)
	}
	if total == 0 {
		return BatchWriteItemOutput{}, fmt.Errorf("%w: BatchWriteItem cannot have a null or no requests set", ErrValidation)
	}
	if total > maxBatchWriteRequests {
		return BatchWriteItemOutput{}, fmt.Errorf("%w: Too many items requested for the BatchWriteItem call", ErrValidation)
	}

	tx, err := c.store.BeginTx(ctx)
	if err != nil {
		return BatchWriteItemOutput{}, err
	}
	defer tx.Rollback()

	// Phase 1: validate every request. Table defs are cached for phase 2.
	defs := make(map[string]storage.TableDef, len(in.RequestItems))
	for table, reqs := range in.RequestItems {
		if err := validateTableName(table); err != nil {
			return BatchWriteItemOutput{}, err
		}
		def, err := c.store.GetTableDef(tx, table)
		if errors.Is(err, storage.ErrNotFound) {
			return BatchWriteItemOutput{}, fmt.Errorf("%w: table %q not found", ErrTableNotFound, table)
		}
		if err != nil {
			return BatchWriteItemOutput{}, err
		}
		if len(reqs) == 0 {
			return BatchWriteItemOutput{}, fmt.Errorf("%w: BatchWriteItem cannot have a null or no requests set", ErrValidation)
		}
		defs[table] = def

		seen := make(map[string]struct{}, len(reqs))
		for _, wr := range reqs {
			if (wr.Put == nil) == (wr.Delete == nil) {
				return BatchWriteItemOutput{}, fmt.Errorf("%w: WriteRequest must set exactly one of PutRequest or DeleteRequest", ErrValidation)
			}
			var kj []byte
			if wr.Put != nil {
				if err := validatePutKey(def, wr.Put.Item); err != nil {
					return BatchWriteItemOutput{}, err
				}
				size, depth := itemSize(wr.Put.Item)
				if size > maxItemSize {
					return BatchWriteItemOutput{}, fmt.Errorf("%w: item size %d exceeds %d bytes", ErrValidation, size, maxItemSize)
				}
				if depth > maxItemDepth {
					return BatchWriteItemOutput{}, fmt.Errorf("%w: item nesting depth %d exceeds %d levels", ErrValidation, depth, maxItemDepth)
				}
				kj, err = normalizedKeyJSON(def, wr.Put.Item)
			} else {
				if _, _, err := validateKey(def, wr.Delete.Key); err != nil {
					return BatchWriteItemOutput{}, err
				}
				kj, err = normalizedKeyJSON(def, wr.Delete.Key)
			}
			if err != nil {
				return BatchWriteItemOutput{}, err
			}
			if _, dup := seen[string(kj)]; dup {
				return BatchWriteItemOutput{}, fmt.Errorf("%w: Provided list of item keys contains duplicates", ErrValidation)
			}
			seen[string(kj)] = struct{}{}
		}
	}

	// Phase 2: apply. The tx rolls back on any failure.
	for table, reqs := range in.RequestItems {
		def := defs[table]
		for _, wr := range reqs {
			if wr.Put != nil {
				wire, err := json.Marshal(wr.Put.Item)
				if err != nil {
					return BatchWriteItemOutput{}, fmt.Errorf("%w: marshal item: %v", ErrValidation, err)
				}
				size, _ := itemSize(wr.Put.Item)
				// GSI key validation BEFORE the storage write (atomic reject).
				if err := validateGsiKeys(wr.Put.Item, def.GSIs); err != nil {
					return BatchWriteItemOutput{}, err
				}
				hashVal, err := keyValue(wr.Put.Item[def.Hash])
				if err != nil {
					return BatchWriteItemOutput{}, err
				}
				var rangeVal any
				if def.Range != "" {
					rangeVal, err = keyValue(wr.Put.Item[def.Range])
					if err != nil {
						return BatchWriteItemOutput{}, err
					}
				}
				dataID, err := c.store.PutItem(tx, table, hashVal, rangeVal, wire, size)
				if err != nil {
					return BatchWriteItemOutput{}, err
				}
				if err := c.maintainGsiRows(tx, table, def.GSIs, dataID, wr.Put.Item, size); err != nil {
					return BatchWriteItemOutput{}, err
				}
			} else {
				hashVal, err := keyValue(wr.Delete.Key[def.Hash])
				if err != nil {
					return BatchWriteItemOutput{}, err
				}
				var rangeVal any
				if def.Range != "" {
					rangeVal, err = keyValue(wr.Delete.Key[def.Range])
					if err != nil {
						return BatchWriteItemOutput{}, err
					}
				}
				// GSI index rows are cascade-deleted by storage (FK).
				if _, err := c.store.DeleteItem(tx, table, hashVal, rangeVal); err != nil {
					return BatchWriteItemOutput{}, err
				}
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return BatchWriteItemOutput{}, err
	}
	return BatchWriteItemOutput{}, nil
}

// BatchGetItem fetches items by exact key across tables on a single read-only
// tx (released by rollback, never committed). Phase 1 validates everything;
// phase 2 reads each table and returns its found items sorted by primary key
// ascending; every requested table is present in Responses (empty slice when
// all its keys miss), matching dynamodb-local. Nonexistent keys are omitted;
// expired-but-unreaped TTL items are returned, matching GetItem (M5a
// Faithful model — reads never filter on TTL).
//
// The response is capped at exactly 16MiB of W1 item accounting measured
// PRE-projection, accumulated across all tables (M6c W6, probe P-batch).
// Tables are processed in sorted-name order and keys in ascending order, so
// the spill is deterministic (unlike the reference). Once an item does not
// fit, every request key whose item is not returned — including misses —
// spills into UnprocessedKeys.
func (c *Client) BatchGetItem(ctx context.Context, in BatchGetItemInput) (BatchGetItemOutput, error) {
	total := 0
	for _, ka := range in.RequestItems {
		total += len(ka.Keys)
	}
	if total == 0 {
		return BatchGetItemOutput{}, fmt.Errorf("%w: BatchGetItem must have some requests set", ErrValidation)
	}
	if total > maxBatchGetKeys {
		return BatchGetItemOutput{}, fmt.Errorf("%w: Too many items requested for the BatchGetItem call", ErrValidation)
	}

	tx, err := c.store.BeginTx(ctx)
	if err != nil {
		return BatchGetItemOutput{}, err
	}
	defer tx.Rollback() // read-only tx: released by rollback, never committed

	// Phase 1: validate everything; prepare storage key values for phase 2.
	type keyVals struct{ hashVal, rangeVal any }
	type tablePlan struct {
		def  storage.TableDef
		vals []keyVals
		proj *expr.BoundProjection
	}
	prepared := make(map[string]tablePlan, len(in.RequestItems))
	for table, ka := range in.RequestItems {
		if err := validateTableName(table); err != nil {
			return BatchGetItemOutput{}, err
		}
		def, err := c.store.GetTableDef(tx, table)
		if errors.Is(err, storage.ErrNotFound) {
			return BatchGetItemOutput{}, fmt.Errorf("%w: table %q not found", ErrTableNotFound, table)
		}
		if err != nil {
			return BatchGetItemOutput{}, err
		}
		if len(ka.Keys) == 0 {
			return BatchGetItemOutput{}, fmt.Errorf("%w: BatchGetItem must have some requests set", ErrValidation)
		}
		// AttributesToGet is the legacy pre-expression projection parameter —
		// rejected, not silently ignored (deliberate divergence; the reference
		// honors it alone). This subsumes the mixed projection+AttributesToGet
		// case.
		if len(ka.AttributesToGet) > 0 {
			return BatchGetItemOutput{}, fmt.Errorf("%w: the legacy AttributesToGet parameter is not supported; use ProjectionExpression", ErrValidation)
		}
		// Per-table projection parse/bind. The adapter has already rejected a
		// present-but-empty projection, so "" here means absent.
		var proj *expr.BoundProjection
		if ka.ProjectionExpression != "" || len(ka.ExpressionAttributeNames) > 0 {
			env := expr.Env{Names: ka.ExpressionAttributeNames}
			var names []string
			var p *expr.Projection
			if ka.ProjectionExpression != "" {
				var err error
				p, err = expr.ParseProjection(ka.ProjectionExpression)
				if err != nil {
					return BatchGetItemOutput{}, fmt.Errorf("%w: ProjectionExpression: %v", ErrValidation, err)
				}
				names, _ = p.Refs()
			}
			if err := expr.CheckUnused(env, names, nil); err != nil {
				return BatchGetItemOutput{}, fmt.Errorf("%w: %v", ErrValidation, err)
			}
			if p != nil {
				proj, err = p.Bind(env) // overlap check runs inside Bind
				if err != nil {
					return BatchGetItemOutput{}, fmt.Errorf("%w: ProjectionExpression: %v", ErrValidation, err)
				}
			}
		}

		seen := make(map[string]struct{}, len(ka.Keys))
		vals := make([]keyVals, 0, len(ka.Keys))
		for _, key := range ka.Keys {
			hv, rv, err := validateKey(def, key)
			if err != nil {
				return BatchGetItemOutput{}, err
			}
			kj, err := normalizedKeyJSON(def, key)
			if err != nil {
				return BatchGetItemOutput{}, err
			}
			if _, dup := seen[string(kj)]; dup {
				return BatchGetItemOutput{}, fmt.Errorf("%w: Provided list of item keys contains duplicates", ErrValidation)
			}
			seen[string(kj)] = struct{}{}

			hashVal, err := keyValue(hv)
			if err != nil {
				return BatchGetItemOutput{}, err
			}
			var rangeVal any
			if def.Range != "" {
				rangeVal, err = keyValue(rv)
				if err != nil {
					return BatchGetItemOutput{}, err
				}
			}
			vals = append(vals, keyVals{hashVal, rangeVal})
		}
		prepared[table] = tablePlan{def: def, vals: vals, proj: proj}
	}

	// Phase 2: read. Every requested table gets a Responses entry (empty when
	// all its keys miss or all spill), and found items are sorted by key
	// ascending per table — both matching dynamodb-local.
	//
	// 16MiB whole-response cap (M6c W6): tables are processed in sorted-name
	// order so the accumulator is deterministic (the reference's spill order
	// is arbitrary — documented divergence, M6c spec §11). Each found item is
	// measured by W1 accounting (itemSize) PRE-projection and added to a
	// single cross-table budget. The first item that would exceed the cap
	// trips the budget: every request key whose item is not returned —
	// including misses and every key of later tables — spills into
	// UnprocessedKeys, echoing the request's ConsistentRead,
	// ProjectionExpression, and ExpressionAttributeNames.
	tables := make([]string, 0, len(prepared))
	for table := range prepared {
		tables = append(tables, table)
	}
	sort.Strings(tables)

	var used int64
	tripped := false
	responses := make(map[string][]Item, len(prepared))
	var unprocessed map[string]KeysAndAttributes
	for _, table := range tables {
		plan := prepared[table]
		items := make([]Item, 0, len(plan.vals))
		for _, kv := range plan.vals {
			item, err := c.readItem(tx, table, kv.hashVal, kv.rangeVal)
			if err != nil {
				return BatchGetItemOutput{}, err
			}
			if item == nil {
				continue
			}
			items = append(items, item)
		}
		sort.Slice(items, func(i, j int) bool {
			return compareItems(plan.def, items[i], items[j]) < 0
		})

		returned := items
		if tripped {
			returned = items[:0]
		} else {
			for i, item := range items {
				size, _ := itemSize(item)
				if used+size > maxBatchGetResponseBytes {
					returned = items[:i]
					tripped = true
					break
				}
				used += size
			}
		}

		if tripped {
			// Spill every request key whose item was not returned.
			keep := make(map[string]struct{}, len(returned))
			for _, item := range returned {
				kj, err := normalizedKeyJSON(plan.def, item)
				if err != nil {
					return BatchGetItemOutput{}, err
				}
				keep[string(kj)] = struct{}{}
			}
			req := in.RequestItems[table]
			spilled := make([]Item, 0, len(req.Keys)-len(returned))
			for _, key := range req.Keys {
				kj, err := normalizedKeyJSON(plan.def, key)
				if err != nil {
					return BatchGetItemOutput{}, err
				}
				if _, ok := keep[string(kj)]; !ok {
					spilled = append(spilled, key)
				}
			}
			sort.Slice(spilled, func(i, j int) bool {
				return compareItems(plan.def, spilled[i], spilled[j]) < 0
			})
			if unprocessed == nil {
				unprocessed = make(map[string]KeysAndAttributes, len(prepared))
			}
			unprocessed[table] = KeysAndAttributes{
				Keys:                     spilled,
				ConsistentRead:           req.ConsistentRead,
				ProjectionExpression:     req.ProjectionExpression,
				ExpressionAttributeNames: req.ExpressionAttributeNames,
			}
		}

		if plan.proj != nil {
			for i := range returned {
				returned[i] = attrval.Project(returned[i], plan.proj.Paths())
			}
		}
		responses[table] = returned
	}
	return BatchGetItemOutput{Responses: responses, UnprocessedKeys: unprocessed}, nil
}
