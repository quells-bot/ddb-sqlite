# ddb-sqlite M5b — Batch APIs Design

**Date:** 2026-08-02
**Status:** Implemented (2026-08-02) — see docs/superpowers/plans/2026-08-02-m5b-batch-ops.md
**Parent spec:** `docs/superpowers/specs/2026-07-31-ddb-sqlite-design.md` (§6.2, §11 M5 milestone)
**Prerequisite:** M5a complete — `PutItem`/`UpdateItem`/`DeleteItem`/`GetItem` with GSI maintenance and TTL (Faithful read model), `Options.Now` injectable clock, all M5a conformance cases green against both targets.

## 1. Overview & goal

M5b adds the two batch operations: `BatchWriteItem` (up to 25 puts/deletes across multiple tables in one call) and `BatchGetItem` (up to 100 key lookups across multiple tables in one call). Both are unconditional — no condition expressions, no `ReturnValues`, no `UpdateItem` in a batch.

M5b delivers:

- `ddb` — `BatchWriteItem`/`BatchGetItem` engine methods; `WriteRequest`/`PutRequest`/`DeleteRequest`/`KeysAndAttributes`/`BatchWriteItemInput`/`BatchWriteItemOutput`/`BatchGetItemInput`/`BatchGetItemOutput` types. New file `ddb/batch.go`.
- `awsdynamodb` — `BatchWriteItem`/`BatchGetItem` adapter methods translating SDK types ↔ engine types.
- Conformance cases for both operations (dual-target).
- `BatchWriteItem`/`BatchGetItem` added to the `api` conformance interface.

**Scope boundary:** Batch APIs only. No `TransactWriteItems`/`TransactGetItems` (v1 non-goal). No `UpdateTable` (M6). No `ProjectionExpression`/`ExpressionAttributeNames`/`AttributesToGet` on `BatchGetItem` (v1 non-goal — rejected rather than silently ignored).

### 1.1 Corrections & refinements to parent spec §6.2

The parent spec §6.2 states:

> **`BatchWriteItem`:** up to 25 requests / 16MB per DynamoDB; each request runs in the shared tx. With no throttling in v1, all valid requests are processed and `UnprocessedItems` is always empty; batches exceeding 25 requests or 16MB raise `ErrValidation` → `ValidationException` (no partial processing).
>
> **`BatchGetItem`:** up to 100 items / 16MB; per-table key lists; TTL filtering; `ProjectionExpression` out of scope (full items returned, faithful to "no projection in v1").

M5b refines three points:

1. **16MB size limits.** For BatchWriteItem, 25 items at the 400KB item cap = 10MB — the count limit is the only practical guardrail; the 16MB aggregate cap is structurally unreachable and remains unenforced. BatchGetItem's response-side 16MB limit (reachable: 100 keys × 400KB = 40MB) was **resolved by M6c W6 (2026-08-03)**: the engine enforces exactly 16MiB of W1 item accounting (pre-projection, whole-response accumulator) and spills overflow keys into `UnprocessedKeys` — the "UnprocessedKeys always empty" invariant below is broken. See `2026-08-03-m6c-hardening-design.md` §6.3.

2. **"TTL filtering" on BatchGetItem is reversed by M5a.** M5a §2.1 reversed the parent spec's synchronous read-path filtering: expired items are never filtered from any read path. BatchGetItem follows the Faithful model — expired items are returned exactly like unexpired ones. No SQL `WHERE ttl <= now` predicate.

3. **`ProjectionExpression` is rejected, not silently ignored.** The parent spec says "out of scope." M5b makes this explicit: a non-empty `ProjectionExpression`, `ExpressionAttributeNames`, **or legacy `AttributesToGet`** on `BatchGetItem` raises `ErrValidation` → `ValidationException`, so tests don't believe a projection was applied when it wasn't. All three are the same projection class; rejecting two while silently ignoring the third would reintroduce the exact hazard the rejection exists to prevent. This is a deliberate divergence from dynamodb-local (which supports all three) and is therefore covered by adapter-only tests, not dual-target conformance cases (§6.3).

**Resolved by M6B** (2026-08-02): the rejection is removed; `ProjectionExpression`/`ExpressionAttributeNames` are honored per-table. `AttributesToGet` remains rejected as a deliberate divergence.

## 2. Probe findings (dynamodb-local 3.3.1)

A throwaway probe — `awsdynamodb/batch_probe_test.go` — drove `dynamodb-local:3.3.1` via the conformance harness's `TestMain`-managed container. The probe is env-gated (`DDBSQLITE_CONF_TARGET=dynamodb-local|all`); it skips under the default `go test ./...`. It is deleted once the §2.4 probes have been added and run, and the M5b conformance cases are ported.

### 2.1 BatchWriteItem findings

| Probe | Result |
|---|---|
| Multi-table (2 tables, 1 put each) | Succeeds; `UnprocessedItems` empty. |
| 25 requests | Succeeds. |
| 26 requests | `ValidationException: Too many items requested for the BatchWriteItem call`. |
| Duplicate key (put+put same key, same table) | `ValidationException: Provided list of item keys contains duplicates`. |
| Put + Delete same key, same table | `ValidationException: Provided list of item keys contains duplicates`. |
| Unknown table in multi-table batch | `ResourceNotFoundException` — valid table's item NOT written (no partial processing). |
| Bad key (missing partition attr) | `ValidationException` — good item NOT written (no partial processing). |
| Empty `RequestItems` | `ValidationException: BatchWriteItem cannot have a null or no requests set`. |
| Mixed put+delete across tables | Succeeds. |
| Same key in different tables | NOT a duplicate — succeeds. |

### 2.2 BatchGetItem findings

| Probe | Result |
|---|---|
| Multi-table read | Succeeds; `UnprocessedKeys` empty. |
| 100 items | Succeeds. |
| 101 items | `ValidationException: Too many items requested for the BatchGetItem call`. |
| Nonexistent key | Simply omitted from `Responses` — not in `UnprocessedKeys`, not an error. |
| Duplicate keys (same key twice) | `ValidationException: Provided list of item keys contains duplicates` — NOT deduplicated. |
| Response ordering | Items returned in an **arbitrary, non-sorted** internal order (empirically confirmed 2026-08-02, 3.3.1): neither request order nor sorted by key, and not reproducible from key values. The mock therefore imposes its own deterministic sort (§3) as a documented divergence where the mock is *more deterministic* than the reference. |
| Unknown table in multi-table batch | `ResourceNotFoundException`. |
| ConsistentRead per-table | Accepted; both tables return items. |
| Composite keys | Works; missing (pk, sk) pair omitted. |
| Empty `RequestItems` | `ValidationException: BatchGetItem must have some requests set`. |

### 2.3 Key findings that shape the design

1. **Duplicate keys are rejected** in both operations — not deduplicated. Same error message for both.
2. **No partial processing on validation errors** — if any request is invalid (unknown table, bad key), the entire batch is rejected and nothing is applied.
3. **BatchGetItem response order is arbitrary in dynamodb-local.** The engine deterministically **sorts each table's items by primary key ascending** (hash, then range) as a mock contract — a documented divergence where the mock is *more deterministic* than dynamodb-local (empirically confirmed: local returns a non-reproducible internal order, not sorted and not request order).
4. **Unknown table → `ResourceNotFoundException`**; count / bad-key / duplicate / empty → `ValidationException`.

### 2.4 Additional probes required before implementation

Design review identified four degenerate inputs the original probe matrix did not cover. The probe file still exists; these rows MUST be added and run before `batch_probe_test.go` is deleted. All five rows were probed against dynamodb-local 3.3.1 on 2026-08-02 and are confirmed below (observed code/message). The empty-table-name probe returned `ValidationException` (NOT `ResourceNotFoundException`), so the engine adds an explicit empty/invalid-name check → `ErrValidation` (§3, §4.2) rather than relying on `ErrTableNotFound`.

| Probe | Observed result (dynamodb-local 3.3.1, 2026-08-02) |
|---|---|
| BatchWriteItem: a `WriteRequest` with neither `PutRequest` nor `DeleteRequest` set | `ValidationException`: "Supplied AttributeValue has more than one datatypes set, must contain exactly one of the supported datatypes" |
| BatchWriteItem: a `WriteRequest` with both `PutRequest` and `DeleteRequest` set | `ValidationException`: "Supplied AttributeValue has more than one datatypes set, must contain exactly one of the supported datatypes" |
| BatchWriteItem: a table mapping to an empty request slice (`RequestItems{"T": {}}`) | `ValidationException`: "The batch write request list for a table cannot be null or empty: BWGap3" |
| BatchGetItem: a table mapping to an empty key list (`{"T": {Keys: []}}`) | `ValidationException`: "The list of keys in RequestItems for BatchGetItem is required: BWGap4 has empty list" |
| Empty-string table name in `RequestItems` | `ValidationException`: "Invalid table/index name.  Table/index names must be between 3 and 255 characters long, and may contain only the characters a-z, A-Z, 0-9, '_', '-', and '.'" |

All five rows confirmed (confirmed 2026-08-02, dynamodb-local 3.3.1). The empty-table-name row is the reconciliation point: expected assumption was `ValidationException`, but the engine as originally specified would yield `ResourceNotFoundException` via `ErrTableNotFound`; because dynamodb-local returned `ValidationException`, the engine must add an explicit empty-name check (§3, §4.2).

## 3. Architecture & validation model

**New file `ddb/batch.go`** houses `BatchWriteItem` and `BatchGetItem` engine methods, keeping batch logic separate from single-item ops in `items.go`. No new `internal/storage` methods — both operations reuse `storage.PutItem`/`DeleteItem`/`GetItem` within a single `BeginTx` → `Commit` (or rollback for reads). No changes to `internal/storage`.

**Transaction model:**
- **BatchWriteItem:** one write tx for the entire batch. All puts/deletes execute on the same `*sql.Tx`. The tx is the serialization unit — the entire batch is atomic: either all writes apply or none do. This matches the parent spec ("shared tx") and the probe finding that no partial processing occurs on validation errors.
- **BatchGetItem:** one read-only tx for the entire batch. Released by `defer tx.Rollback()` — no writes to commit (matching `DescribeTable`/`DescribeTimeToLive`). All key lookups execute on the same tx.

**Validation flow (pre-write, all-or-nothing):**

1. `RequestItems` empty → `ErrValidation`.
2. Total request count > 25 (BatchWriteItem) / 100 (BatchGetItem) → `ErrValidation`.
3. For each table name: reject an **empty/invalid name** → `ErrValidation` (confirmed §2.4); then `GetTableDef` → `ErrTableNotFound` if absent. A table mapping to an **empty request/key list** → `ErrValidation` (confirmed §2.4).
4. For each request in each table: BatchWriteItem first requires **exactly one of `Put`/`Delete` non-nil** → `ErrValidation` otherwise (confirmed §2.4); then validate key attributes (present, correct type, exact key-attribute set for deletes/gets).
5. For each table: detect duplicate keys → `ErrValidation: Provided list of item keys contains duplicates`.
6. Only if all validation passes: apply all requests in the tx, commit.

Steps 3–5 happen before any write. A validation failure at any step rejects the entire batch — nothing is written (probe-confirmed: unknown-table and bad-key batches left the valid table untouched).

**Validation order** (structural before semantic, consistent with existing ops):

1. Empty `RequestItems` → `ErrValidation`
2. Count > 25/100 → `ErrValidation`
3. Per-table empty/invalid table name → `ErrValidation`; `GetTableDef` → `ErrTableNotFound` (`ResourceNotFoundException`); empty per-table request/key list → `ErrValidation`
4. Per-request shape validation (BatchWriteItem: exactly one of Put/Delete) → `ErrValidation`; per-request key validation → `ErrValidation`
5. Per-table duplicate detection → `ErrValidation`

This ordering is a documented assumption — the probes tested each error in isolation, not combinations. If a dual-target conformance case reveals a different precedence, the engine's order adjusts to match.

Two placement notes: **(a)** the 400KB per-item size check runs in the *apply* phase (matching `PutItem`'s placement in `items.go`), so its precedence against other validation errors is unprobed — atomicity is unaffected since the tx rolls back; **(b)** an empty-string table name in `RequestItems` is rejected with `ErrValidation` → `ValidationException` by an explicit empty/invalid-name check that runs **before** `GetTableDef` (confirmed §2.4: dynamodb-local returns `ValidationException: Invalid table/index name…`). This supersedes the earlier design where an empty name would fall through to `GetTableDef` → `ErrTableNotFound` → `ResourceNotFoundException`. (Micro-divergence note: the engine's empty/invalid-name check currently rejects only the empty string `""`; other non-empty invalid names — too short, invalid characters — fall through to `GetTableDef` → `ErrTableNotFound` → `ResourceNotFoundException`, whereas dynamodb-local returns `ValidationException` for them. This is a documented micro-divergence, out of scope for M5b: the engine has no general table-name validation anywhere.

**Duplicate key detection:** for each request, extract the key attributes into a normalized `Item` (just `hash` + optional `range`), marshal to wire JSON, and use the JSON bytes as a `map[string]struct{}` set key per table. Duplicate JSON bytes → duplicate key → `ErrValidation`. Because `num.Parse` canonicalizes at parse time (trailing fractional zeros stripped, leading integer zeros trimmed, exponent notation rejected), spelling variants of the same number (`1` vs `1.0` vs `01`) produce *identical* wire JSON and **are** caught. The true residual hole is narrower: two numerically *distinct* N keys that collide in the REAL float64 index column (e.g. `9007199254740992` vs `9007199254740993`) pass the duplicate check — correctly, since real DynamoDB treats them as two distinct items — but within one batch the second put silently REPLACEs the first, and a delete can target the wrong item. This is the pre-existing N-key float64 caveat (`internal/storage/tables.go` `sqliteType`) with a new batch-only manifestation; accepted and documented, not fixed. A numeric-aware duplicate check (`num.Equal` per hash+range pair) is the fallback if it ever matters.

**BatchGetItem ordering and presence:** dynamodb-local returns each table's items in an **arbitrary, non-reproducible internal order** — empirically confirmed 2026-08-02 against 3.3.1 to be neither request order nor sorted by key (§2.2). The engine therefore imposes a deterministic mock contract: for each table, returned items are **sorted by primary key ascending** (hash, then range when present; S lexicographic, N numeric via `num.Decimal.Compare`, B bytewise). This is a **documented divergence where the mock is more deterministic than the reference**: the mock's order is stable and testable, while dynamodb-local's is not. (Because order is not a dynamodb-local contract, the dual-target ordering conformance case asserts only the returned key SET — §6.2.) Every requested table is present in `Responses`; a table whose keys all miss yields an **empty slice** entry (this part matches dynamodb-local). Nonexistent keys are simply skipped (omitted from `Responses`, not in `UnprocessedKeys`).

## 4. `ddb` engine API surface

### 4.1 New types

```go
// WriteRequest is one item-level action in a batch write: either a Put or a
// Delete. Exactly one field must be non-nil — the engine rejects any other
// shape (both nil, both set) with ErrValidation (confirmed §2.4).
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
// ProjectionExpression, ExpressionAttributeNames, and the legacy
// AttributesToGet are v1 non-goals — a non-empty value raises ErrValidation
// so tests don't believe a projection was applied when it wasn't.
type KeysAndAttributes struct {
    Keys                    []Item
    ConsistentRead          bool
    ProjectionExpression    string
    ExpressionAttributeNames map[string]string
    AttributesToGet         []string
}
```

### 4.2 BatchWriteItem

```go
type BatchWriteItemInput struct {
    RequestItems map[string][]WriteRequest
}

// BatchWriteItemOutput carries unprocessed requests. In v1 (no throttling),
// UnprocessedItems is always empty (nil).
type BatchWriteItemOutput struct {
    UnprocessedItems map[string][]WriteRequest
}

func (c *Client) BatchWriteItem(ctx context.Context, in BatchWriteItemInput) (BatchWriteItemOutput, error)
```

**Flow:**

1. Begin tx. Defer rollback.
2. Pre-validate:
   - `RequestItems` empty → `ErrValidation`.
   - Total request count (sum across all tables) > 25 → `ErrValidation`.
   - For each table name: reject an empty name → `ErrValidation` (confirmed §2.4: dynamodb-local returns `ValidationException` for an empty table name rather than `ResourceNotFoundException`). Then `GetTableDef(tx, name)` → `ErrTableNotFound` if absent. An empty request slice for the table → `ErrValidation` (confirmed §2.4).
   - For each WriteRequest: exactly one of `Put`/`Delete` non-nil → `ErrValidation` otherwise (confirmed §2.4).
   - For each Put request: validate the partition key is present with the correct type; if the table has a sort key, validate it too (same inline logic as `PutItem` in `items.go`, extracted as a shared helper `validatePutKey`).
   - For each Delete request: `validateKey(def, key)`.
   - For each table: build a `map[string]struct{}` of wire-JSON key bytes; if a key is already present → `ErrValidation` ("Provided list of item keys contains duplicates").
3. Apply (only if all validation passed):
   - For each Put: marshal item to wire JSON, check `maxItemBytes` (400KB), `validateGsiKeys(item, def.GSIs)`, extract key values via `keyValue`, `storage.PutItem(tx, table, hashVal, rangeVal, wire)` → `dataID`, `maintainGsiRows(tx, table, def.GSIs, dataID, item)`.
   - For each Delete: extract key values via `keyValue`, `storage.DeleteItem(tx, table, hashVal, rangeVal)`.
4. Commit.
5. Return `BatchWriteItemOutput{}` (empty `UnprocessedItems`).

**Shared helper extraction:** `PutItem`'s inline key validation (lines 91–108 of `items.go`) is extracted into `validatePutKey(def storage.TableDef, item Item) error` in `items.go`, used by both `PutItem` and `BatchWriteItem`. This avoids duplicating the partition/sort key presence-and-type checks. `PutItem`'s behavior is unchanged.

### 4.3 BatchGetItem

```go
type BatchGetItemInput struct {
    RequestItems map[string]KeysAndAttributes
}

// BatchGetItemOutput carries the per-table found items and unprocessed keys.
// In v1 (no throttling), UnprocessedKeys is always empty (nil).
type BatchGetItemOutput struct {
    Responses       map[string][]Item
    UnprocessedKeys map[string]KeysAndAttributes
}

func (c *Client) BatchGetItem(ctx context.Context, in BatchGetItemInput) (BatchGetItemOutput, error)
```

**Flow:**

1. Begin tx. Defer rollback (read-only tx — no commit).
2. Pre-validate:
   - `RequestItems` empty → `ErrValidation`.
   - Total key count (sum across all tables) > 100 → `ErrValidation`.
   - For each table name: reject an empty name → `ErrValidation` (confirmed §2.4: dynamodb-local returns `ValidationException` for an invalid table name); then `GetTableDef(tx, name)` → `ErrTableNotFound` if absent. An empty `Keys` list for the table → `ErrValidation` (confirmed §2.4).
   - For each table: if `ProjectionExpression` non-empty, `ExpressionAttributeNames` non-empty, or `AttributesToGet` non-empty → `ErrValidation` (v1 non-goal, rejected rather than silently ignored).
   - For each key: `validateKey(def, key)`.
   - For each table: duplicate key detection (same wire-JSON approach as BatchWriteItem) → `ErrValidation`.
3. Read (only if all validation passed):
   - For each table (in `RequestItems` iteration order):
     - For each key: `readItem(tx, table, hashVal, rangeVal)` — reusing the existing `items.go` helper rather than re-implementing fetch+unmarshal; `rangeVal` is nil for hash-only tables, mirroring `GetItem`'s branch. Append non-nil results to the table's response slice. If not found, skip.
     - Sort the table's found items by primary key ascending (hash, then range) via a package-private comparator (§3), then assign the sorted slice to `Responses[table]`.
4. Rollback (deferred — releases the read-only tx).
5. Return `BatchGetItemOutput{Responses: ..., UnprocessedKeys: nil}`.

**TTL:** no filtering on reads (M5a Faithful model, §2.1). Expired items returned exactly like unexpired ones. No SQL `WHERE ttl <= now` predicate. No changes to `storage.GetItem`.

**Response map:** the engine initializes `Responses` as a non-nil `map[string][]Item{}` with an entry for **every requested table** — a table whose keys all miss gets an **empty slice** `[]Item{}` (matching dynamodb-local, which includes the empty-table entry). Found items per table are sorted by key ascending (§3). The adapter translates the engine map to the SDK `Responses` map via `ToSDKMap` per item; an all-miss table produces an empty SDK entry.

### 4.4 No changes to existing operations

`PutItem`, `GetItem`, `DeleteItem`, `UpdateItem`, `Query`, `Scan`, and `readItem` are unchanged. The only modification to existing code is extracting `validatePutKey` from `PutItem`'s inline validation (§4.2) — a refactor with no behavioral change.

## 5. Adapter changes

### 5.1 BatchWriteItem

```go
func (a *Adapter) BatchWriteItem(ctx context.Context, params *dynamodb.BatchWriteItemInput,
    optFns ...func(*dynamodb.Options)) (*dynamodb.BatchWriteItemOutput, error)
```

Translates `map[string][]types.WriteRequest` → `map[string][]ddb.WriteRequest`. Each `types.WriteRequest` has either `PutRequest{Item}` or `DeleteRequest{Key}`. The adapter marshals SDK `types.AttributeValue` maps to `attrval.Value` via the existing `FromSDKMap`. No expression strings or substitution maps to translate (batch ops are unconditional).

Returns `BatchWriteItemOutput{UnprocessedItems: nil}` on success (engine always returns empty). On error, `mapError` maps `ErrTableNotFound` → `ResourceNotFoundException`, `ErrValidation` → `ValidationException`.

**Structural nil rejection:** `params.RequestItems == nil` (empty map) is passed through to the engine, which rejects it with `ErrValidation` → `ValidationException` (matching dynamodb-local's "cannot have a null or no requests set"). No adapter-level check needed — the engine owns content validation.

### 5.2 BatchGetItem

```go
func (a *Adapter) BatchGetItem(ctx context.Context, params *dynamodb.BatchGetItemInput,
    optFns ...func(*dynamodb.Options)) (*dynamodb.BatchGetItemOutput, error)
```

Translates `map[string]types.KeysAndAttributes` → `map[string]ddb.KeysAndAttributes`. Each key map marshaled via `FromSDKMap`. `ConsistentRead` (`*bool`) → `aws.ToBool`. `ProjectionExpression` (`*string`) → `aws.ToString`. `ExpressionAttributeNames` passed through as a `map[string]string`. `AttributesToGet` passed through as a `[]string`. The engine rejects a non-empty `ProjectionExpression`, `ExpressionAttributeNames`, **or** `AttributesToGet` with `ErrValidation` → `ValidationException` (§4.3) — any field alone is rejected, matching the v1 non-goal.

Returns `BatchGetItemOutput{Responses: ..., UnprocessedKeys: nil}`. The engine's `Responses` is a non-nil map with an entry for **every requested table** (§4.3): found items sorted by key ascending, and an **empty slice** for a table whose keys all miss (matching dynamodb-local). The adapter translates each table's `[]Item` to `[]map[string]types.AttributeValue` via `ToSDKMap`, producing the SDK `Responses` map.

### 5.3 `api` conformance interface

`BatchWriteItem` and `BatchGetItem` are added to the `api` interface in `conformance_test.go`. Both `*awsdynamodb.Adapter` and `*dynamodb.Client` already satisfy these signatures (exact SDK method signatures).

### 5.4 `ReturnConsumedCapacity` / `ReturnItemCollectionMetrics`

The SDK inputs have `ReturnConsumedCapacity` (both operations) and `ReturnItemCollectionMetrics` (`BatchWriteItemInput`) fields. Consumed capacity and item-collection metrics are out of scope for v1 — accepted and ignored, same as `ConsistentRead` on `GetItem`. The adapter does not translate them.

## 6. Conformance cases

Dual-target cases go into `awsdynamodb/conformance_test.go` and run against both targets via `runConformance`. Deliberately divergent rejections (§6.3) cannot run dual-target and go into `awsdynamodb/adapter_test.go` as adapter-only unit tests. The probe tests in `batch_probe_test.go` are deleted once the §2.4 probes have run and the cases are ported (following the M3/M5a precedent).

### 6.1 BatchWriteItem cases

| Case | Scenario |
|---|---|
| `TestConfBatchWriteMultiTable` | Batch puts to 2 tables in one request. Verify both items land via GetItem. `UnprocessedItems` empty. |
| `TestConfBatchWriteCountLimit` | 25 requests succeed. 26 → `ValidationException`. |
| `TestConfBatchWriteDuplicateKey` | Two PutRequests for same key, same table → `ValidationException`. |
| `TestConfBatchWritePutDeleteSameKey` | Put + Delete for same key, same table → `ValidationException`. |
| `TestConfBatchWriteCrossTableSameKey` | Same key value in different tables → succeeds (not a duplicate). |
| `TestConfBatchWriteUnknownTable` | One table in a multi-table batch doesn't exist → `ResourceNotFoundException`. Verify the valid table's item was NOT written (no partial processing). |
| `TestConfBatchWriteBadKey` | One item missing the partition key attribute → `ValidationException`. Verify the good item was NOT written (no partial processing). |
| `TestConfBatchWriteEmptyRequestItems` | Empty `RequestItems` map → `ValidationException`. |
| `TestConfBatchWriteMixed` | Put + Delete across multiple tables in one batch. Verify all applied correctly. |
| `TestConfBatchWriteDelete` | Batch of DeleteRequests. Verify items removed. |
| `TestConfBatchWriteOverwrite` | Batch put overwrites an existing item. Verify new value via GetItem. |
| `TestConfBatchWriteGsiPut` | Batch puts to a GSI-backed table. Query the GSI — all items indexed. |
| `TestConfBatchWriteGsiDelete` | Batch delete from a GSI-backed table. Query the GSI — index rows gone (ON DELETE CASCADE). |
| `TestConfBatchWriteGsiOverwrite` | Batch put overwrites an existing item with *changed* GSI key attrs. Old GSI row gone (REPLACE+CASCADE), new row present — no phantom Query hits. |
| `TestConfBatchWriteItemTooLarge` | One put's item exceeds 400KB → `ValidationException`; the whole batch is rejected, nothing written (all-or-nothing). |
| `TestConfBatchWriteNeitherPutNorDelete` | A WriteRequest with neither field set → `ValidationException` (confirmed §2.4). |
| `TestConfBatchWriteEmptyTableRequests` | A table mapping to an empty request slice → `ValidationException` (confirmed §2.4). |
| `TestConfBatchWriteEmptyTableName` | An empty-string table name in `RequestItems` → `ValidationException` (confirmed §2.4; NOT `ResourceNotFoundException`). |

The §2.4 probe rows were confirmed against dynamodb-local 3.3.1 on 2026-08-02 and are included in the dual-target suite.

### 6.2 BatchGetItem cases

| Case | Scenario |
|---|---|
| `TestConfBatchGetMultiTable` | Batch reads from 2 tables. Both items returned. `UnprocessedKeys` empty. |
| `TestConfBatchGetCountLimit` | 100 items succeed. 101 → `ValidationException`. |
| `TestConfBatchGetNonexistentKey` | Mix of existing and nonexistent keys. Only existing keys in `Responses`; nonexistent omitted (not in `UnprocessedKeys`, not an error). |
| `TestConfBatchGetDuplicateKeys` | Same key twice in one table's key list → `ValidationException`. |
| `TestConfBatchGetOrdering` | Request keys in a shuffled order with an interleaved miss. Asserts the returned key **SET** is exactly `{k1,k2,k3,k5}` (ghost omitted) — **order-agnostic**, because dynamodb-local returns an arbitrary non-sorted order (§3); the mock deterministically sorts but order is not assertable against the reference. |
| `TestConfBatchGetUnknownTable` | One table in a multi-table batch doesn't exist → `ResourceNotFoundException`. |
| `TestConfBatchGetConsistentReadPerTable` | `ConsistentRead` set differently per table. Both return items (engine is always consistent). |
| `TestConfBatchGetCompositeKeys` | Composite primary keys (pk + sk). Missing (pk, sk) pair omitted. |
| `TestConfBatchGetEmptyRequestItems` | Empty `RequestItems` map → `ValidationException`. |
| `TestConfBatchGetAllMissTableOmitted` | Multi-table batch where one table's keys all miss → that table has an **empty** `Responses` entry (matching dynamodb-local); the other table's items returned. |
| `TestConfBatchGetEmptyTableKeys` | A table mapping to an empty `Keys` list → `ValidationException` (confirmed §2.4). |
| `TestConfBatchGetEmptyTableName` | An empty-string table name in `RequestItems` → `ValidationException` (confirmed §2.4; NOT `ResourceNotFoundException`). |

§2.4 probe rows were confirmed against dynamodb-local 3.3.1 on 2026-08-02 and are included in the dual-target suite.

### 6.3 Adapter-only cases (divergent rejections)

These behaviors deliberately diverge from dynamodb-local (which supports projection on `BatchGetItem`), so they cannot run dual-target. They live in `awsdynamodb/adapter_test.go` as adapter-only unit tests:

| Case | Scenario |
|---|---|
| `TestAdapterBatchGetProjectionRejected` | Non-empty `ProjectionExpression` on `BatchGetItem` → `ValidationException`. |
| `TestAdapterBatchGetExpressionNamesRejected` | Non-empty `ExpressionAttributeNames` → `ValidationException`. |
| `TestAdapterBatchGetAttributesToGetRejected` | Non-empty legacy `AttributesToGet` → `ValidationException` (same projection class; rejected, not silently ignored). |

### 6.4 TTL interaction case (M5a Faithful model)

| Case | Scenario |
|---|---|
| `TestConfBatchGetExpiredItemVisible` | Table with TTL enabled; put item with past-epoch TTL attr. `BatchGetItem` returns the expired item (no read filtering, per M5a §2.1). |

### 6.5 Verification gate

- `go test ./...` green (root module, default target).
- `cd awsdynamodb && go test ./...` green (adapter target).
- `cd awsdynamodb && DDBSQLITE_CONF_TARGET=all go test -count=1 ./...` green (both targets) — requires podman socket.
- No existing tests broken by the `validatePutKey` extraction.

## 7. Decisions, risks & out of scope

### 7.1 Decisions captured

1. **One tx per batch (shared tx).** Matches parent spec §6.2. Entire batch is atomic — all writes apply or none. No partial processing on validation errors (probe-confirmed).
2. **No new storage methods.** Reuse `storage.PutItem`/`DeleteItem`/`GetItem` in a loop within the shared tx. No performance concern under `MaxOpenConns(1)` with small test tables.
3. **Pre-validate, then apply.** All validation (count, table existence, key validation, duplicate detection) runs before any write. A failure at any step rejects the entire batch.
4. **Duplicate keys rejected, not deduplicated.** Both BatchWriteItem and BatchGetItem reject duplicate keys in the same table with `ValidationException: Provided list of item keys contains duplicates` (probe-confirmed).
5. **BatchGetItem deterministically sorts each table's items by key ascending.** Response items are sorted by primary key ascending (hash, then range). dynamodb-local's order is arbitrary and non-reproducible (§2.2), so this is a **documented divergence where the mock is more deterministic than the reference** — the dual-target ordering case asserts the key set only (§6.2).
6. **Nonexistent keys omitted from BatchGetItem responses.** Not in `Responses`, not in `UnprocessedKeys`, not an error (probe-confirmed).
7. **Count limits; BatchGetItem 16MiB response cap enforced by M6c W6.** 25 (BatchWriteItem) / 100 (BatchGetItem). BatchWriteItem's 16MB aggregate limit is unreachable in practice (25 × 400KB = 10MB) and remains unenforced; BatchGetItem's response-side cap is enforced (M6c W6): items measured by W1 accounting pre-projection, one whole-response accumulator, overflow spills to `UnprocessedKeys`.
8. **`UnprocessedItems` always empty; `UnprocessedKeys` empty unless the 16MiB cap trips.** No throttling in v1, so BatchWriteItem requests are never spilled. BatchGetItem spills only on response-cap overflow (M6c W6).
9. **No TTL read filtering on BatchGetItem.** Consistent with M5a's Faithful model — expired items returned like unexpired ones.
10. **`ProjectionExpression`/`ExpressionAttributeNames` rejected on BatchGetItem.** V1 non-goal; rejected with `ValidationException` rather than silently ignored, so tests don't believe a projection was applied.
11. **BatchGetItem tx released by rollback.** Read-only tx — no writes to commit (matching `DescribeTable`/`DescribeTimeToLive`).
12. **`ConsistentRead` per-table accepted and ignored.** Engine is always consistent.
13. **`ReturnConsumedCapacity` accepted and ignored.** Consumed capacity out of scope for v1.
14. **New file `ddb/batch.go`.** Keeps batch logic separate from single-item ops.
15. **`validatePutKey` extracted from `PutItem`.** Shared helper for key presence-and-type validation, used by both `PutItem` and `BatchWriteItem`. No behavioral change to `PutItem`.
16. **WriteRequest shape validated.** Exactly one of `Put`/`Delete` non-nil; any other shape (both nil, both set) → `ErrValidation` (confirmed §2.4).
17. **Empty per-table request lists rejected.** A table mapping to an empty request/key slice → `ErrValidation` (confirmed §2.4).
18. **`AttributesToGet` rejected on BatchGetItem.** Same projection class as `ProjectionExpression` — rejected, not silently ignored. Deliberate divergence from dynamodb-local; adapter-only tests (§6.3).
19. **Numeric duplicate detection is canonical-form-based.** `1` vs `1.0` vs `01` ARE caught (`num.Parse` canonicalizes); the accepted hole is float64-colliding distinct N keys (§3).
20. **`readItem` reused for BatchGetItem reads.** No duplicated fetch+unmarshal path.
21. **BatchGetItem includes an empty `Responses` entry for every requested table.** A table whose keys all miss yields `Responses[table] = []` (matching dynamodb-local); found items are sorted by key ascending (§3, decision #5).

### 7.2 Risks & mitigations

1. **Duplicate key detection via wire-JSON bytes.** Number spelling variants (`1` vs `1.0` vs `01`) canonicalize to identical wire JSON at parse time and ARE caught. The accepted hole: numerically distinct N keys colliding in the float64 REAL index column pass the check, and within one batch a second put silently REPLACEs the first (real DynamoDB keeps both items). *Mitigation:* pre-existing N-key float64 caveat, documented in §3; far outside test usage. A numeric-aware check (`num.Equal` per hash+range pair) is the fallback if it ever matters.
2. **Probe-pending rejections.** WriteRequest shape, empty per-table request lists, and empty table names were designed as rejections ahead of probing (§2.4). *Mitigation:* all five §2.4 probes ran against dynamodb-local 3.3.1 on 2026-08-02 and confirmed the four `ValidationException` rejections. The **empty-table-name** probe returned `ValidationException` (not `ResourceNotFoundException`), so the engine adds an explicit empty/invalid-name check → `ErrValidation` before `GetTableDef` (§3, §4.2); this supersedes the earlier `ErrTableNotFound` path for empty names.
3. **Validation order unprobed for combinations.** Probes tested each error in isolation, not combinations (e.g., a batch with both an unknown table and too many requests). *Mitigation:* document the assumed order; conformance cases test single-error scenarios; adjust if a combination case diverges.
4. **`validatePutKey` extraction changes `PutItem`.** The extraction is a pure refactor — the same checks, same error messages, just moved to a function. *Mitigation:* the M1–M5a conformance suite covers `PutItem` extensively; any regression surfaces immediately.
5. **No new errors needed.** `ErrTableNotFound` and `ErrValidation` already exist and map to the right SDK exceptions. No `ddb/errors.go` changes.

### 7.3 Explicitly out of scope for M5b

- ~~16MB request/response size limits~~ — resolved by M6c W6 (2026-08-03) for BatchGetItem (16MiB response cap enforced with `UnprocessedKeys` spill); BatchWriteItem's 16MB aggregate limit remains unenforced (structurally unreachable: 25 × 400KB = 10MB).
- `ProjectionExpression` / `ExpressionAttributeNames` / `AttributesToGet` on `BatchGetItem` (rejected, v1 non-goal — a deliberate divergence from dynamodb-local, covered by adapter-only tests §6.3).
- `ReturnConsumedCapacity` / `ReturnItemCollectionMetrics` accounting (accepted, ignored).
- `TransactWriteItems` / `TransactGetItems` (v1 non-goal, parent spec §8).
- `BatchExecuteStatement` / PartiQL (v1 non-goal).
- `UpdateTable` (GSI add/remove — M6).
