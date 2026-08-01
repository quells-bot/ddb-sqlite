# ddb-sqlite M3 — Query/Scan & Pagination Design

**Date:** 2026-08-01
**Status:** Approved (brainstorming) → pending implementation plan
**Parent spec:** `docs/superpowers/specs/2026-07-31-ddb-sqlite-design.md` (§6.3–6.5, §11 M3 milestone)
**Prerequisite:** M2 complete — `internal/expr` (lexer, parser, bind, condition/filter evaluator, update evaluator), `ConditionExpression` + `ReturnValues` on `PutItem`/`DeleteItem`/`UpdateItem`, `ConditionalCheckFailedError`, the filter evaluator and `ValidateFilterKeys` built and unit-tested with no caller, all M2 conformance cases green against both targets.

## 1. Overview & goal

M3 adds `Query` and `Scan` to the engine, adapter, and conformance suite. Base-table only — GSI queries arrive in M4 (`IndexName` is rejected with `ValidationException` in M3).

M3 delivers:

- `internal/expr` — `ExtractKeyCondition`: validates and extracts a `KeyCondition` from a bound condition AST. The `KeyConditionExpression` is parsed with the existing `ParseCondition` (same grammar, already fuzzed); extraction is a semantic-validation walk.
- `internal/storage` — `Query` and `Scan` methods that generate `SELECT` statements with key-narrowing predicates on the indexed columns. Storage deals in opaque `[]byte` blobs and raw column values, never importing `attrval`/`num` — same as today.
- `ddb` — `Query` and `Scan` operations with `QueryInput`/`QueryOutput` and `ScanInput`/`ScanOutput`; `KeyConditionExpression` extraction and translation to storage space; `FilterExpression` wiring (bind once, eval per row); faithful pagination (`Limit` as read budget, `ExclusiveStartKey`/`LastEvaluatedKey` with stop-reason LEK rule, `Limit=0` rejected); `ScanIndexForward`; parallel scan (`Segment`/`TotalSegments`); `Select=COUNT`; `ErrGsiNotFound` for `IndexName`.
- `awsdynamodb` — `Query` and `Scan` with exact SDK signatures; expression pass-through; legacy-parameter rejection; present-but-empty-expression rejection; `ScanIndexForward` nil-default normalization; `Select` translation; `ErrGsiNotFound` → `ValidationException`.
- Conformance cases 16–37 (§8), gated on both targets.

The filter evaluator (`(*BoundCondition).Eval`) and the filter-only key-attribute rule (`(*BoundCondition).ValidateFilterKeys`) are built and unit-tested in M2 with no caller. M3 wires them. This is the M2 payoff: condition and filter expressions share one grammar and one evaluator, so M3 pays no new parser/evaluator cost for filtering.

## 2. Approach: reuse `ParseCondition` + AST extraction

### 2.1 KeyConditionExpression handling (decision)

The `KeyConditionExpression` uses the condition grammar (§5.1 of the parent spec) but with a restricted shape. It is parsed with the existing `expr.ParseCondition` — the same lexer, parser, and bind phase already built and fuzzed in M2. A new semantic step, `ExtractKeyCondition`, walks the bound AST to validate the key-condition shape and return a structured `KeyCondition`.

Rejected alternatives:

- **A dedicated `ParseKeyCondition` in `expr`.** A separate recursive-descent parser reusing `lex.go` but with its own smaller grammar. Tighter grammar (invalid constructs fail at parse time), but duplicates parsing logic for a strict subset of what's already built. The M2 spec explicitly says the key-condition grammar "shares this grammar," signaling reuse was the intent. More code to maintain and fuzz for a marginal error-message improvement.
- **Parse + extract in `ddb` (no `expr` changes).** Puts expression-walking logic in `ddb`, violating the separation where `expr` owns all expression logic. `ddb` would need to understand AST internals (`cmpNode`, `funcNode`, `andNode`). Rejected on architectural grounds.

The chosen approach reuses the fuzzed parser. The key-condition grammar restrictions are enforced as semantic validation (like `ValidateFilterKeys` already does), not parse-time rejection — consistent with how M2 handles other restrictions. Error messages say "KeyConditionExpression may not contain OR" rather than failing at the `OR` token, but both map to `ValidationException`.

### 2.2 KeyConditionExpression grammar & shape

```
keyCond := pkCond (AND sortCond)?
         | sortCond AND pkCond
pkCond  := path '=' :value            // exactly one: partition key = single value
sortCond := path op :value
          | path BETWEEN :lo AND :hi
          | begins_with '(' path ',' :value ')'
op      := '=' | '<' | '<=' | '>' | '>='
```

Either ordering of `pkCond` and `sortCond` in the top-level `AND` is accepted (`pk = :v AND sk > :x` and `sk > :x AND pk = :v` are both legal). This is a conformance-determined assumption — case 19 (§8) probes it against `dynamodb-local` and encodes the expectation for both targets.

### 2.3 Extraction in `internal/expr`

A new exported function on the bound condition:

```go
// KeyCondition is the validated, extracted form of a KeyConditionExpression.
// Sort is nil when only the partition-key equality was supplied.
type KeyCondition struct {
    Partition struct {
        Name  string         // resolved attribute name (matches table PK)
        Value attrval.Value  // the :value substitution
    }
    Sort *SortKeyCond // nil if absent
}

// SortKeyCond is one sort-key predicate. The structured form carries
// attrval.Value operands; ddb translates them to column-space Go values
// before passing to storage.
type SortKeyCond struct {
    Name       string
    Op         string        // "=", "<", "<=", ">", ">=", "BETWEEN", "BEGINS_WITH"
    Lo, Hi     attrval.Value // both set for BETWEEN; Lo set for all others
    BeginsWith attrval.Value // set for BEGINS_WITH only
}

// ExtractKeyCondition validates that b has the KeyConditionExpression shape,
// matching the partition and (optional) sort attribute names from the table
// schema. Either arm of the top-level AND may be the partition condition.
// pkName is required; skName is "" for a partition-only table.
func (b *BoundCondition) ExtractKeyCondition(pkName, skName string) (KeyCondition, error)
```

The extractor takes the key attribute names as parameters so it can classify which `AND` arm is the partition key. `expr` stays free of any `ddb` import — just two strings.

### 2.4 Validation rules enforced during extraction

The walk accepts exactly:

1. A single `cmpNode` with `op = =`, left = path, right = `:value` → partition condition, no sort condition. The path must match `pkName`.
2. An `andNode` whose left and right are each one of the allowed key conditions. Exactly one arm must be the partition equality (`path == pkName`, `op == =`); the other must be a sort condition (`path == skName`). Either ordering in the `AND` is accepted.
3. A sort-key condition is one of: `cmpNode` with `op ∈ {=,<,<=,>,>=}`, `betweenNode`, or `funcNode` with `begins_with`. Its path must match `skName`.

Everything else is rejected with `ErrSemantic` (→ `ValidationException`):

- `orNode`, `notNode` — "KeyConditionExpression may not contain OR/NOT".
- A comparator other than `=` on the partition key.
- More than one partition-key condition, or more than one sort-key condition.
- `IN`, `attribute_exists`, `attribute_not_exists`, `attribute_type`, `contains`, `size()` — none are legal in a key condition.
- A sort condition when `skName == ""` (table has no sort key).
- `begins_with` with a path operand instead of `:value`.
- A path that matches neither `pkName` nor `skName`.

### 2.5 `begins_with` on Number sort keys

`begins_with` is defined for S and B only. On an N sort key it is a `ValidationException`. This check happens in `ddb` (which knows the sort key type from `TableDef`), not in `expr` — `expr` doesn't know types. The `ddb` layer: if `KeyCondition.Sort.Op == "BEGINS_WITH"` and `def.RangeType == "N"`, reject.

### 2.6 Sort-key value type validation

The extracted sort-key operand values (`:lo`, `:hi`, `:v`) must match the sort key's DynamoDB type. This is checked in `ddb` after extraction: each value's `Tag()` must equal `tagForKeyType(def.RangeType)`. A mismatch is `ValidationException`, matching DynamoDB. This mirrors how `validateKey` checks key attribute types today. The partition value type is validated the same way against `def.HashType`.

## 3. Storage interface

Storage gains `Query` and `Scan` methods that generate `SELECT` statements with key-narrowing predicates. Both follow the existing pattern: opaque `[]byte` blobs out, raw column values in, no `attrval`/`num` imports.

### 3.1 Storage-level sort-key condition

`ddb` translates the `expr.SortKeyCond` (which carries `attrval.Value` operands) to column-space Go values via the existing `keyValue()` function (S→string, N→float64, B→[]byte) before passing to storage. `begins_with` on S/B is passed through as a `BEGINS_WITH` op: `ddb` computes the lexicographic successor of the prefix (it knows the key type from `TableDef`) and passes both `Lo=prefix` and `Hi=successor` to storage, which generates a half-open range `range >= ? AND range < ?`. Storage maps the op to a SQL fragment — same as `BETWEEN` → `range >= ? AND range <= ?` — with no DynamoDB semantics. Storage receives seven ops:

```go
// SortKeyCond is the storage-level sort-key predicate. ddb translates
// attrval.Value operands to Go values (string/float64/[]byte) before passing
// them here, matching the key column affinities. BEGINS_WITH carries Lo=prefix
// and Hi=successor (computed by ddb); storage emits a half-open range.
// Op == "" is legal: ddb builds such a condition when resuming a
// partition-equality-only Query from an ExclusiveStartKey, so that only the
// ResumeAfter clause is emitted (§5.2).
type SortKeyCond struct {
    Op          string // "", "=", "<", "<=", ">", ">=", "BETWEEN", "BEGINS_WITH"
    Lo          any    // set for every non-"" Op
    Hi          any    // BETWEEN: always set. BEGINS_WITH: nil means no
                        // successor exists (empty or all-0xFF prefix) and
                        // storage emits only "range >= ?".
    ResumeAfter any    // appended as "AND range > ?" (ASC) / "range < ?" (DESC); nil = no resume
}
```

`ResumeAfter` is the sort-key value of the last scanned item from a prior page, used to resume a `Query` after `ExclusiveStartKey`. It is appended to the range predicate: `range > ?` for `ScanIndexForward=true` (ASC), `range < ?` for `ScanIndexForward=false` (DESC). nil means no resume (first page).

### 3.2 Query — partition seek + optional sort-key range

```go
// Query selects rows for one partition key value, ordered by the sort key,
// and returns their item blobs. sortCond is nil for a partition-only seek
// with no resume; when resuming a partition-equality-only Query, sortCond is
// non-nil with Op == "" and only ResumeAfter set (§5.2). sortCond may carry a
// ResumeAfter bound from a prior page's LastEvaluatedKey.
// scanForward controls ASC (true) vs DESC (false) ordering of the range column.
// limit <= 0 means unlimited; otherwise at most limit rows are returned.
// Returns blobs in key order.
func (s *Store) Query(tx *sql.Tx, table string, hashVal any, sortCond *SortKeyCond, scanForward bool, limit int) ([][]byte, error)
```

Generated SQL shape (sort-key table example):

```sql
SELECT data FROM ddb_<hash>
WHERE hash = ?
  AND range < ?          -- sortCond.Op predicate; omitted when sortCond is nil
  AND range > ?          -- ResumeAfter; omitted when nil
ORDER BY range ASC       -- DESC when !scanForward
LIMIT ?                  -- omitted when limit <= 0
```

`BETWEEN` emits `range >= ? AND range <= ?`. `BEGINS_WITH` emits `range >= ? AND range < ?` (half-open). Partition-only tables omit the `AND range ...` clauses entirely and have no `ORDER BY range`. For partition-only tables, a `Query` returns at most one row; `ResumeAfter` is nil.

### 3.3 Scan — full or parallel scan

```go
// Scan selects rows in rowid order and returns their item blobs. segment and
// totalSegments implement parallel scan: when totalSegments > 1, only rows
// with (id % totalSegments) == segment are returned. afterID > 0 resumes the
// scan after that rowid (from a prior page's LastEvaluatedKey). limit <= 0
// means unlimited.
func (s *Store) Scan(tx *sql.Tx, table string, segment, totalSegments int, afterID int64, limit int) ([][]byte, error)
```

Generated SQL shape:

```sql
SELECT data FROM ddb_<hash>
WHERE (id % ?) = ?     -- only when totalSegments > 1
  AND id > ?            -- only when afterID > 0
ORDER BY id             -- rowid order; see last-write note below
LIMIT ?                 -- omitted when limit <= 0
```

`id` is the rowid (`INTEGER PRIMARY KEY`), so `id % N` partitions the rowid range without overlap — parallel scans cover disjoint sets. `ORDER BY id` makes the scan order deterministic and stable across pages.

Scan order is last-write order, not first-insert order: `PutItem` uses `INSERT OR REPLACE`, and SQLite `REPLACE` deletes and re-inserts the row, moving an overwritten item to the newest rowid. A consequence: an item overwritten *between* pages of a paginated scan can be returned twice, and an item whose rowid moves ahead of the resume cursor can be skipped. This is acceptable for a mock — DynamoDB's Scan order is unspecified and its concurrent-modification guarantees are equivalently weak — but tests must not assume first-insert order.

### 3.4 Why storage returns only blobs, and the `GetItem` extension

`Query` and `Scan` return item blobs and nothing else. `ddb` builds `LastEvaluatedKey` by decoding the last scanned item's blob (which always contains the key attributes) — never from the condition values, since with a range condition like `sk > :x` the actual key of the last scanned item is not necessarily `:x`, and `Scan` has no key condition at all. The row's `id` and `range` column values are storage internals and never cross the boundary: `Scan` resume passes `afterID` *into* storage and `Query` resume passes `ResumeAfter` *into* storage; neither appears in any output. (An earlier draft returned them on a `QueryRow` struct; nothing in `ddb` consumed them, so they were cut.)

To obtain that `afterID` for a resumed `Scan` (§5.3), storage's existing point lookup is extended to also return the rowid:

```go
// GetItem returns the rowid and item blob for the key. found is false (no
// error) if absent. The id is a storage internal; only ddb's Scan resume
// path uses it.
func (s *Store) GetItem(tx *sql.Tx, table string, hashVal, rangeVal any) (id int64, data []byte, found bool, err error)
```

Existing callers (`ddb.GetItem`, `ddb.UpdateItem`) ignore the `id`.

## 4. `ddb` engine API surface

### 4.1 Input/Output structs

```go
// QueryInput queries one partition by key condition, optionally filtered.
type QueryInput struct {
    TableName                 string
    KeyConditionExpression    string
    FilterExpression          string
    ExpressionAttributeNames  map[string]string
    ExpressionAttributeValues map[string]attrval.Value

    ExclusiveStartKey Item
    Limit              int32
    ScanIndexForward   bool // default true (ASC)
    ConsistentRead     bool // accepted, ignored (engine is always consistent)
    Select             string // "" (default ALL_ATTRIBUTES) or "COUNT"
}

type QueryOutput struct {
    Items            []Item
    Count            int32  // items passing the filter
    ScannedCount     int32  // items scanned (before filter)
    LastEvaluatedKey Item
}

// ScanInput scans a full table (or one parallel segment).
type ScanInput struct {
    TableName                 string
    FilterExpression          string
    ExpressionAttributeNames  map[string]string
    ExpressionAttributeValues map[string]attrval.Value

    ExclusiveStartKey Item
    Limit              int32
    Segment            int32  // 0 when TotalSegments is 0
    TotalSegments      int32  // 0 = non-parallel scan
    ConsistentRead     bool
    Select             string
}

type ScanOutput struct {
    Items            []Item
    Count            int32
    ScannedCount     int32
    LastEvaluatedKey Item
}
```

### 4.2 New sentinel error

```go
var ErrGsiNotFound = errors.New("ddb: global secondary index not found")
```

`IndexName` is rejected in M3 (GSI is M4). A non-empty `IndexName` on `Query` or `Scan` yields `ErrGsiNotFound` → `ValidationException` ("Index ... not found on table ..."). This sentinel exists now so the adapter maps it correctly from M3 onward.

### 4.3 `Select` validation

Shared helper:

```go
func validateSelect(s string) (string, error) // normalizes "" → "ALL_ATTRIBUTES"
```

Accepts `""` / `"ALL_ATTRIBUTES"` / `"COUNT"`. Rejects `"SPECIFIC_ATTRIBUTES"` and `"ALL_PROJECTED_ATTRIBUTES"` as `ErrValidation` (they need `ProjectionExpression` / GSI respectively — both v1 non-goals). Case-sensitive, matching DynamoDB's enum.

When `Select == "COUNT"`: `Items` is returned as `nil` (the adapter omits the field); `Count`/`ScannedCount` are still populated; `LastEvaluatedKey` is still computed (DynamoDB returns `LEK` on `COUNT` queries that stop early).

### 4.4 `Limit` semantics

`Limit` caps *scanned* items (before filter), not *returned* items — faithful to DynamoDB, which counts items scanned, not returned. There is no upper cap on `Limit` (DynamoDB's 1MB limit is a capacity concern, out of scope for this mock).

In the engine, `Limit` is an `int32` where `0` means *unset* (unlimited) — the adapter maps SDK nil to `0` (§7.2). A negative `Limit` is rejected with `ErrValidation` (DynamoDB rejects `Limit < 0`). An SDK-present `Limit` of `0` cannot be represented in this encoding, so the adapter rejects it with `ValidationException` (§7.3), matching AWS's documented minimum of 1. **Confirmed by probe (§5.6):** `dynamodb-local:3.3.1` rejects `Limit=0` on both `Query` and `Scan` with `ValidationException: Limit must be greater than or equal to 1`. The open question is settled — the `int32` encoding stands.

### 4.5 Operation flow — `Query`

```
1.  BeginTx
2.  GetTableDef → ErrTableNotFound if absent
3.  Validate Select (shared helper)
4.  If IndexName != "" → ErrGsiNotFound (M4)
5.  Parse KeyConditionExpression; union Refs with FilterExpression's Refs; CheckUnused once; Bind both. An empty KeyConditionExpression fails here as a parse error → ErrValidation — the same exception type DynamoDB raises for a missing key condition, so no separate presence check is needed
6.  ExtractKeyCondition(boundKeyCond, def.Hash, def.Range) → KeyCondition
7.  Validate: partition value type matches def.HashType; sort value type matches def.RangeType
8.  If begins_with on N sort key → ErrValidation
9.  ValidateFilterKeys(boundFilter, [def.Hash, def.Range]) if filter present
10. Translate KeyCondition to storage space (keyValue on partition value; sort condition values via keyValue)
11. Build resume cursor from ExclusiveStartKey (§5.2)
12. store.Query(tx, table, hashVal, sortCond, scanForward, limit) — ddb passes limit as-is (no +1; see the note below and §5.1 for why the limit+1 trick was dropped)
13. For each returned blob: unmarshal → Item; this is one scanned item (increment ScannedCount). Storage returns at most `limit` rows (or all rows when fewer than `limit` exist)
14. Apply filter (if present): Eval(item) → keep (increment Count) / discard
15. Build LastEvaluatedKey from the last *scanned* item iff ScannedCount == Limit (§5.1); nil iff ScannedCount < Limit (exhausted) or Limit is unset
16. If Select == COUNT: drop Items, keep counts + LEK
17. Commit
```

The "fetch `limit+1`" trick from `ListTables` was **dropped** for Query/Scan. The probe (§5.6) proved that DynamoDB sets `LEK` whenever `ScannedCount == Limit`, *even when the Limit-th item is the last item in the scope* — resuming from that LEK returns an empty trailing page. The limit+1 trick cannot reproduce this: with 10 items and `Limit=10`, storage returns 10 (not 11), and the old "returned ≤ limit → no LEK" rule would wrongly omit the LEK. The correct rule is simpler: fetch `limit` rows; set `LEK` iff `ScannedCount == Limit`; nil iff `ScannedCount < Limit` (or no Limit). No surplus row, no second query. (`ListTables` may have the same divergence — noted as a follow-up in §9.2.)

### 4.6 Operation flow — `Scan`

Same structure, minus the key-condition steps:

- No `KeyConditionExpression`.
- `Segment`/`TotalSegments` validation: if `TotalSegments > 0`, `Segment` must be in `[0, TotalSegments)` and `TotalSegments > 0`. `TotalSegments == 0` means non-parallel; `Segment` must then be `0`.
- Resume cursor is `id > lekID`: `ddb` resolves `ExclusiveStartKey` to a rowid via `store.GetItem` (one indexed point lookup), then passes the recovered `id` as `afterID` to `store.Scan`.
- `FilterExpression` validation: `ValidateFilterKeys` with `[def.Hash, def.Range]` — `Scan` also forbids key attributes in the filter, matching DynamoDB.

### 4.7 Parse/bind before row read

Parsing and binding happen **before** any row is read so that a malformed expression fails with `ValidationException` regardless of whether the table is empty, matching DynamoDB's validate-then-execute ordering. This is consistent with the M2 pattern for condition expressions.

## 5. Pagination mechanics

Pagination is the subtlest part — DynamoDB's `LastEvaluatedKey` semantics are exact, and getting them wrong breaks resume.

### 5.1 `LastEvaluatedKey` construction

`LEK` is the key of the **last scanned item** — the last item that counted toward `Limit`, whether or not it passed the filter. It contains the table's full key (partition + sort, or partition only), extracted by decoding the scanned item's blob (which always contains the key attributes).

```
scanned items:  [A, B, C, D, E]   Limit=3
                 ↑     ↑
                 scanned cap → LEK = key of C
Items returned:  only those passing filter, from A,B,C
```

**`LEK` is governed by the stop reason, not by item count** (confirmed by probe, §5.6):

- **Set** when `ScannedCount == Limit` — the stop reason is "Limit reached." This holds *even when the Limit-th item is the last item in the scope*: `Limit` exactly equal to the available item count still yields a non-nil LEK, and resuming from it returns an empty trailing page (`ScannedCount=0`, `LEK=nil`).
- **Nil** when `ScannedCount < Limit` (fewer items than the budget → exhausted), or when `Limit` is unset (unlimited) and the whole scope was read.

The only reliable pagination terminator is `LEK == nil`. A caller cannot conclude "done" from `Limit ≥ item count`; only the absence of LEK proves exhaustion. The engine sets `LEK` iff `ScannedCount == Limit` — no "more remain" probe, no surplus row, no second query.

**Trailing empty pages.** A pagination walk emits a final round with `Count=0`/`ScannedCount=0`/`LEK=nil` iff the last non-empty round hit `ScannedCount == Limit` exactly at the final item. Pagination loops must terminate on `LEK == nil` and tolerate this empty round. Conformance cases 20 and 26 (§8.4) pin this for Query and Scan respectively.

**Limit caps at available items (no padding).** `Limit` larger than the scope reads exactly the available count; `ScannedCount` never exceeds the item count and never exceeds `Limit`. When `Limit > available`, `ScannedCount < Limit` → exhausted → no LEK.

There is no `LEK` design for a page that scanned zero items: an `LEK` is the key of the last *scanned* item, and any value chosen without scanning would either skip an unreturned item on resume or fail to advance the caller. Accordingly, an SDK-present `Limit=0` is rejected by the adapter as `ValidationException` (§4.4). **Confirmed by probe** (§5.6): `dynamodb-local:3.3.1` rejects `Limit=0` with `ValidationException: Limit must be greater than or equal to 1` on both `Query` and `Scan`.

### 5.2 `Query` resume from `ExclusiveStartKey`

The caller passes the previous `LEK` as `ExclusiveStartKey`. `ddb`:

1. Validates `ExclusiveStartKey` against the table's key schema (same `validateKey` used by `GetItem` — which also rejects a key carrying *extra* attributes, matching DynamoDB's "The provided starting key is invalid" behavior; probed in case 30). Its partition key must match the `KeyConditionExpression`'s partition value — a mismatch is `ValidationException`.
2. Extracts the sort-key value from `ExclusiveStartKey` (for sort-key tables).
3. Sets `sortCond.ResumeAfter` to the `LEK`'s sort-key value (translated to column space via `keyValue`). If the `KeyConditionExpression` had no sort condition, `ddb` constructs a `SortKeyCond{Op: "", ResumeAfter: ...}` — the `Op == ""` case from §3.1 — so the resume bound is still emitted. Storage appends `AND range > ?` (ASC) or `AND range < ?` (DESC) to the range predicate, combining it with the original sort condition. This preserves the original bounds: a `BETWEEN :a AND :b` resumed after `lekSortVal` becomes `range >= :a AND range <= :b AND range > lekSortVal`.

For partition-only tables (no sort key), `ExclusiveStartKey` still validates and matches the partition — but a partition-only table has at most one row per partition, so `LEK` is never set on a `Query` to a partition-only table (one row, scan ends). Resume is a no-op there. `ResumeAfter` is nil.

### 5.3 `Scan` resume from `ExclusiveStartKey`

`Scan` has no key condition, so resume is by rowid. `ddb`:

1. Validates `ExclusiveStartKey` against the table's key schema (reuse `validateKey`). No partition constraint — any valid key in the table is a legal resume point.
2. Decodes `ExclusiveStartKey` to get the key values, calls `store.GetItem` to recover the row's `id`. (One indexed point lookup — cheap, only on the first page of a resumed scan.)
3. Passes the recovered `id` as `afterID` to `store.Scan`.

A stale `ExclusiveStartKey` (the row was deleted since the prior page) is handled gracefully: `store.GetItem` returns "not found," and the scan starts from the beginning. This matches DynamoDB, which accepts a stale `ExclusiveStartKey` and resumes from the nearest row. (If the row exists, `id > afterID` resumes exactly after it.)

### 5.4 `LastEvaluatedKey` for `Scan`

Built from the last scanned item's key attributes (decoded from its blob), same as `Query`. The `id` is not included in `LEK` — it is a storage internal, invisible to the caller. The caller passes the key back as `ExclusiveStartKey`, and `ddb` re-resolves it to an `id` on resume.

### 5.5 `LEK` for `Select=COUNT`

`Select=COUNT` does not change LEK construction. `LEK` is still the key of the last scanned item, set iff `ScannedCount == Limit`. The `Items` slice is dropped but the scan still reads and counts items — `Count` and `ScannedCount` are populated, and `LEK` is set when the Limit bound the stop. Resuming from that LEK continues the scan identically.

### 5.6 Probe-verified semantics & methodology

The `Limit`/`LEK`/`FilterExpression` semantics in §4.4–§5.5 were measured against `dynamodb-local:3.3.1` via the AWS SDK v2 to settle open questions and correct two assumptions in the earlier draft. The probe is `awsdynamodb/limit_probe_test.go` (throwaway — delete once the M3 conformance cases are ported). It reuses the conformance harness's `TestMain`-managed dockertest container, type-asserts the shared `*dynamodb.Client` for real `Query`/`Scan`, and is env-gated (`DDBSQLITE_CONF_TARGET=dynamodb-local`); it skips under the default `go test ./...`.

**Table & seed.** Composite primary key: `pk` HASH (type S), `sk` RANGE (type N), plus a `flag` attribute (`"yes"`/`"no"`). 12 items: partition `P1` with `sk` 0–9 (`flag="yes"` on even `sk` → 5 yes), partition `P2` with `sk` 0–1 (1 yes). Total: 12 items, 6 `"yes"`.

**What each scenario establishes** (these are the basis for the §8.4 conformance cases):

| ID | Scenario | Finding |
|---|---|---|
| Q1 | Query all P1, no Limit | `ScannedCount == Count == 10`, LEK nil (no Limit, all read) |
| Q2 | Query P1, Limit=3, no filter | `ScannedCount == Count == 3 == Limit`, LEK set (Limit < scope) |
| Q3 | Query P1, Limit=3, filter=yes | `ScannedCount=3, Count=2` — Limit counts reads, filter keeps a subset |
| Q4 | Query P1, Limit=10, filter=yes | `ScannedCount=10 == Limit` → LEK **still set** (to sk=9) even though all 10 items were read |
| Q4b | Resume from Q4's LEK | `ScannedCount=0, Count=0, LEK=nil` — trailing empty page proves the prior LEK meant "Limit reached", not "more data" |
| Q5 | Query P1, Limit=11, filter=yes | `ScannedCount=10 < Limit=11` → exhausted → LEK nil (contrast with Q4) |
| Q6 | Query P1, Limit=0 | `ValidationException: Limit must be greater than or equal to 1` |
| Q7 | Query P1, Limit=1, reverse | `ScanIndexForward=false` reads from high end → sk=9, LEK set |
| Q8 | Query P1, Limit=1, filter=no | `Count=0` (no match in window) but `ScannedCount=1 == Limit` → LEK set (a read was consumed) |
| Q9 | Query pagination walk, Limit=3, filter=yes | 4 rounds; recovers all 5 yes while scanning all 10; terminates on LEK nil |
| Q10 | Query P1, sk BETWEEN 2 AND 7, Limit=3 | Limit applied after key-condition narrowing → reads sk 2,3,4, LEK set |
| S1 | Scan all, no Limit | `ScannedCount == Count == 12`, LEK nil; cross-partition order: P2 items before P1 |
| S2 | Scan, Limit=4, no filter | `ScannedCount == Count == 4 == Limit`, LEK set |
| S3 | Scan, Limit=4, filter=yes | `ScannedCount=4, Count=2` — same read-budget invariant as Query |
| S4 | Scan, Limit=0 | `ValidationException: Limit must be greater than or equal to 1` |
| S5 | Scan pagination walk, Limit=2 | 7 rounds for 12 items; round 6 hit `ScannedCount==Limit` at last item → LEK set; round 7 = empty trailing page |
| S6 | Scan pagination walk, Limit=4, filter=yes | 4 rounds; recovers all 6 yes while scanning all 12; round 4 = empty trailing page |

**Key corrections to the earlier draft:**

1. **`Limit=0` is rejected** (was: "returns no items but sets LEK"). Both Query and Scan return `ValidationException`.
2. **LEK is set iff `ScannedCount == Limit`**, not iff "more items remain" (was: "fetch limit+1; LEK set iff surplus row returned"). The `Limit == available` case sets LEK; resuming yields an empty trailing page. The limit+1 trick was dropped — it cannot reproduce this behavior.

**Consumed capacity** (informational, not implemented): the probe confirmed `ConsumedCapacity` scales with `ScannedCount`, not `Count` — every scanned item costs a read regardless of filter outcome. This invariant informs the `ScannedCount`/`Count` split but is otherwise out of scope (§9.3).

## 6. FilterExpression wiring

This is the M2 payoff — the filter evaluator is built, unit-tested, and has no caller. M3 wires it.

### 6.1 Bind-once, eval-per-row

`FilterExpression` is parsed and bound **once** at the start of the operation (step 5 in the §4.5 flow), before any row is read. A malformed filter fails with `ValidationException` regardless of whether the table is empty — matching DynamoDB's validate-then-execute ordering, consistent with how M2 handles condition expressions.

The bound filter is then evaluated against each scanned item via `(*BoundCondition).Eval(item)`:

- `true` → item kept (increment `Count`).
- `false` → item discarded (does not increment `Count`, but **does** increment `ScannedCount`).

### 6.2 Key-attribute restriction

`(*BoundCondition).ValidateFilterKeys(keyAttrs)` is called once after binding, with the table's key attribute names (`[def.Hash]` or `[def.Hash, def.Range]`). A filter referencing a key attribute → `ValidationException`. This applies to both `Query` and `Scan` — DynamoDB forbids key attributes in `FilterExpression` for both, since the key is already specified in the `KeyConditionExpression` (`Query`) or irrelevant to a full scan (`Scan`).

### 6.3 Empty vs absent filter

`FilterExpression` is a plain `string` in `ddb`'s input structs; `""` means absent. The adapter performs the present-but-empty-string check (a non-nil pointer to `""` → `ValidationException`), matching the M2 pattern for condition/update expressions.

### 6.4 Composition with `Select=COUNT`

`Select=COUNT` and `FilterExpression` compose: `COUNT` returns only `Count`/`ScannedCount`, but the filter still applies — `Count` is items passing the filter, `ScannedCount` is total scanned. The `Items` slice is dropped. No interaction beyond that.

### 6.5 Substitution union

`FilterExpression` and `KeyConditionExpression` both contribute `Refs`. `ddb` unions their refs and calls `expr.CheckUnused` once, so a `#n` referenced only by the filter is not reported unused because the key condition does not mention it — exactly the cross-expression rule from M2 §4.5.

## 7. Adapter changes

The adapter (`awsdynamodb/adapter.go`) gains `Query` and `Scan` methods with exact SDK signatures, plus the supporting validation that only the adapter can perform.

### 7.1 New methods

```go
func (a *Adapter) Query(ctx context.Context, params *dynamodb.QueryInput,
    optFns ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error)

func (a *Adapter) Scan(ctx context.Context, params *dynamodb.ScanInput,
    optFns ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error)
```

### 7.2 Translation

| SDK field | Engine field |
|---|---|
| `TableName` | `TableName` |
| `KeyConditionExpression` (`*string`) | `KeyConditionExpression` |
| `FilterExpression` (`*string`) | `FilterExpression` |
| `ExpressionAttributeNames` | `ExpressionAttributeNames` |
| `ExpressionAttributeValues` | `exprValues(FromSDKMap)` |
| `ExclusiveStartKey` | `FromSDKMap` → `Item` |
| `Limit` (`*int32`) | `Limit` (0 if nil) |
| `ScanIndexForward` (`*bool`) | `ScanIndexForward` (nil → `true`; see §7.4) |
| `ConsistentRead` (`*bool`) | `ConsistentRead` |
| `Select` (`*string`) | `Select` |
| `Segment` / `TotalSegments` (`*int32`) | `Segment` / `TotalSegments` |
| `IndexName` (`*string`) | rejected → `ValidationException` (M4) |

Output translation: `Items` → `ToSDKMap` per item; `Count`/`ScannedCount` → `int32`; `LastEvaluatedKey` → `ToSDKMap` (nil → omitted).

### 7.3 Adapter-only validation (present-but-empty checks)

Same pattern as M2: the SDK carries expression strings as `*string`, the engine as plain `string`. The adapter rejects:

- `KeyConditionExpression` present-but-empty → `ValidationException`.
- `FilterExpression` present-but-empty → `ValidationException`.
- Present-but-empty `ExpressionAttributeNames`/`ExpressionAttributeValues` maps alongside any expression → `ValidationException` (reuse `rejectEmptySubMaps`, passing `KeyConditionExpression` and `FilterExpression` as its two expression arguments).
- Present-but-zero `Limit` → `ValidationException` (§4.4). Only the adapter can distinguish SDK nil (unlimited) from a pointer to `0`.

### 7.4 `ScanIndexForward` default

The SDK sends `ScanIndexForward` as `*bool`; when nil, DynamoDB defaults to `true` (ASC). The adapter owns SDK-default normalization; the engine struct carries the resolved value:

```go
ScanIndexForward: params.ScanIndexForward == nil || *params.ScanIndexForward
```

nil or true both yield `true`; explicit false yields `false`.

### 7.5 Legacy parameter rejection

`Query` and `Scan` have deprecated pre-expression parameters: `KeyConditions` (the legacy `Query` key-condition map), `QueryFilter`, `ScanFilter`, `ConditionalOperator`, `AttributesToGet`. The M3 **adapter** rejects them with `ValidationException` when non-empty, matching the M2 precedent for `Expected`/`ConditionalOperator`/`AttributeUpdates`. Silently ignoring them would let a test believe a `KeyConditions` or `QueryFilter` constraint was applied when it never was.

**Reference divergence (measured):** `dynamodb-local:3.3.1` still *accepts* the deprecated `KeyConditions`/`QueryFilter`/`ScanFilter` and evaluates them (a probe returned items). So the adapter's rejection is a deliberate scope decision, not reference-faithful behavior. Per §9.2 the reference wins: this is a documented adapter-only divergence, and conformance case 32 asserts these behaviors separately per target (rejection on the adapter, acceptance on the reference). The engine is unchanged — it never sees legacy params.

### 7.6 Conformance `api` interface extension

The `api` interface in `conformance_test.go` gains:

```go
Query(ctx context.Context, params *dynamodb.QueryInput, optFns ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error)
Scan(ctx context.Context, params *dynamodb.ScanInput, optFns ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error)
```

Both `*awsdynamodb.Adapter` and `*dynamodb.Client` (the SDK client vs dynamodb-local) already satisfy these — they are standard SDK methods.

## 8. Testing strategy & conformance cases

Four layers, following the M2 precedent.

### 8.1 Layer 1 — `internal/expr` unit tests

`ExtractKeyCondition` is the one new `expr` function:

- Valid shapes: `pk = :v`; `pk = :v AND sk = :v`; `sk = :v AND pk = :v` (either order); `begins_with(sk, :v)`; `sk BETWEEN :lo AND :hi`; each comparator (`=`, `<`, `<=`, `>`, `>=`).
- Rejected shapes: `OR`, `NOT`, `IN`, `attribute_exists`, `attribute_not_exists`, `attribute_type`, `contains`, `size()`, non-equality on PK, two sort conditions, sort condition when `skName == ""`, `begins_with` with a path operand instead of `:value`, path matching neither `pkName` nor `skName`.
- Schema matching: PK arm matched against `pkName`, sort arm matched against `skName`; mismatched attribute name → `ErrSemantic`.

### 8.2 Layer 2 — `internal/storage` unit tests

`Query` and `Scan` against a seeded in-memory table:

- `Query` partition seek with no sort key; with each sort-key op; `BETWEEN` range; `ASC` vs `DESC` ordering.
- `Scan` full table order (rowid); parallel segments cover disjoint sets and union to the full set.
- `limit` truncation.
- Resume: `Query` with `ResumeAfter`; `Scan` with `afterID`.
- Wrapped in one `BeginTx` with `defer tx.Rollback()`, matching existing storage test style.

### 8.3 Layer 3 — `ddb` unit tests

- `Query` end-to-end: key condition → filtered results; `ScannedCount` vs `Count`.
- `Scan` end-to-end: full scan; parallel scan segments.
- Pagination: `Limit` + `LastEvaluatedKey` resume (both ops), including resume of a partition-equality-only `Query` (`Op == ""` + `ResumeAfter`); negative `Limit` → `ErrValidation`. (`Limit=0` rejection is adapter-level — conformance case 21.) LEK stop-reason rule: `LEK` set iff `ScannedCount == Limit` (including `Limit == available` → trailing empty page on resume); `LEK` nil iff `ScannedCount < Limit` or unset.
- `Select=COUNT` drops `Items`, keeps counts.
- `ExclusiveStartKey` partition mismatch on `Query` → `ValidationException`.
- `begins_with` on N sort key → `ValidationException`.
- Prefix-successor edge cases: empty prefix and all-`0xFF` prefix → no upper bound (storage `SortKeyCond.Hi` is nil); `begins_with(sk, "")` matches the whole partition.
- `IndexName` → `ErrGsiNotFound` → `ValidationException`.
- Expression/validation failures surface as `ErrValidation`.

### 8.4 Layer 4 — conformance cases (dual-target)

Added to `awsdynamodb/conformance_test.go`, continuing the M2 numbering. Each runs against both the adapter and `dynamodb-local`:

16. **Basic Query** — partition-only table and sort-key table; `pk = :v` returns the right items in key order.
17. **Sort-key conditions** — each of `=`, `<`, `<=`, `>`, `>=`, `BETWEEN`, `begins_with` on an S sort key.
18. **`ScanIndexForward=false`** — same partition, DESC ordering.
19. **`KeyConditionExpression` PK/SK ordering** — `sk > :x AND pk = :v` and `pk = :v AND sk > :x` return identical results. (Probes the "either order accepted" assumption against `dynamodb-local`; the expectation is encoded for both targets.)
20. **Query pagination** — `Limit` smaller than result set; resume via `ExclusiveStartKey`/`LastEvaluatedKey` until `LEK` nil. Includes `Limit` exactly equal to the partition size: LEK is **still set** (to the last item's key), and resuming yields an empty trailing page (`ScannedCount=0`, `LEK=nil`). The only reliable terminator is `LEK == nil`. (Probe Q4/Q4b/Q9, §5.6.)
21. **`Limit=0`** — SDK-present `Limit` of `0` on `Query` and `Scan`. **Confirmed by probe (§5.6):** `dynamodb-local:3.3.1` rejects with `ValidationException: Limit must be greater than or equal to 1` on both operations. The `int32` encoding (0 = unset) stands; the adapter rejects SDK-present `0`.
22. **`FilterExpression` on Query** — filter narrows results; `ScannedCount` reflects all scanned, `Count` the filtered count; `LEK` is the last *scanned* item even if filtered out. Includes the zero-match-in-window case: `Limit=1` + a filter matching nothing in the 1-item window yields `Count=0` but `LEK` set (a read was consumed, `ScannedCount == Limit`). (Probe Q3/Q8, §5.6.)
23. **Filter referencing key attribute** → `ValidationException` (both `Query` and `Scan`).
24. **`Select=COUNT`** — `Items` omitted, `Count`/`ScannedCount` populated, `LEK` still set when stopped early.
25. **Basic Scan** — full table scan returns all items.
26. **Scan pagination** — `Limit` + resume to exhaustion. Includes the trailing empty page: when the last non-empty round hits `ScannedCount == Limit` at the final item, the walk emits a final round with `Count=0`/`ScannedCount=0`/`LEK=nil`. (Probe S5/S6, §5.6.)
27. **Parallel Scan** — `TotalSegments=N`, each `Segment` returns a disjoint partition; union equals a full scan.
28. **`begins_with` on N sort key** → `ValidationException`.
29. **KeyConditionExpression rejections** — `OR`, `NOT`, non-equality on PK, two sort conditions → `ValidationException`.
30. **`ExclusiveStartKey` validation** — partition mismatch with the `KeyConditionExpression` → `ValidationException`; a key carrying extra attributes → `ValidationException` (both targets).
31. **`IndexName`** on Query and Scan → `ValidationException` (GSI is M4).
32. **Legacy param rejection** — `KeyConditions` non-empty with a well-formed value → `ValidationException` on the **adapter** only. The reference (`dynamodb-local:3.3.1`) *accepts* the deprecated `KeyConditions` and returns items, so the case asserts each target's real behavior (see §7.5). The engine is unchanged.
33. **Present-but-empty expressions** — `KeyConditionExpression=""` and `FilterExpression=""` via `*string` → `ValidationException`.
34. **Limit > available items** — `Query` with `Limit` exceeding the partition size; `ScannedCount` caps at the available count (`ScannedCount < Limit`); `LEK` nil (exhausted, not Limit-bound). Contrasts with case 20's `Limit == available` where `LEK` is set. (Probe Q5, §5.6.)
35. **Limit applied after key-condition narrowing** — `Query` with `sk BETWEEN :a AND :b` (6-item range) + `Limit=3`; reads the first 3 items of the key-conditioned set in sort order, `LEK` set. Limit does not count items outside the sort-key range. (Probe Q10, §5.6.)
36. **`ScanIndexForward=false` with Limit** — `Query` with `Limit=1` and `ScanIndexForward=false` reads from the high sort-key end; the first returned item is the highest sort key, `LEK` set. Limit counts from the reverse-order start. (Probe Q7, §5.6.)
37. **`Select=COUNT` with Limit and filter** — `Select=COUNT` + `Limit` + `FilterExpression`: `Items` omitted, `Count`/`ScannedCount` populated, `LEK` set when `ScannedCount == Limit`. The filter and count interact but LEK construction is unchanged. (Composes probe Q4 with `Select=COUNT`; see §5.5.)

### 8.5 Fuzzing

No new fuzz targets. The existing `FuzzParseCondition`/`FuzzBindEval` already cover the grammar `KeyConditionExpression` and `FilterExpression` reuse. `ExtractKeyCondition` is a pure tree-walk over already-validated ASTs — exercised through the conformance and unit tests.

### 8.6 Verification gate

M3 is complete when all of the following pass, with the cache buster on every `go test` so a cached result cannot stand in for an actual run — this matters most for the `dynamodb-local` target, whose outcome depends on container state that Go's content-based cache cannot see:

```bash
go test -count=1 ./...                                        # root module
cd awsdynamodb && go test -count=1 ./...                      # adapter module, adapter target
cd awsdynamodb && DDBSQLITE_CONF_TARGET=all go test -count=1 ./...  # both conformance targets
go vet ./... && (cd awsdynamodb && go vet ./...)              # both module directories
```

**Every M3 conformance case (16–37) must be green against both the adapter and `dynamodb-local`.** Pagination and key-condition semantics are precisely where a faithful mock silently diverges, and the reference is available, so divergence is a blocker rather than a logged follow-up.

## 9. Decisions, risks & out of scope

### 9.1 Decisions captured

| Decision | Choice |
|---|---|
| Milestone shape | One spec, single implementation pass |
| `KeyConditionExpression` | Reuse `ParseCondition` + `ExtractKeyCondition` AST walk |
| PK/SK ordering in `AND` | Either order accepted; extractor takes `pkName`/`skName` to classify |
| `begins_with` on N sort key | `ValidationException` (checked in `ddb`, which knows the type) |
| `begins_with` on S/B | `ddb` computes lexicographic successor; storage `BEGINS_WITH` op emits half-open `>= prefix AND < successor` |
| Sort-key resume | `ResumeAfter` field on storage `SortKeyCond`; appended as `range > ?` / `range < ?` |
| Scan resume | `afterID int64` (rowid); `ddb` resolves `ExclusiveStartKey` → `id` via `GetItem` (extended to return the rowid) |
| Scan order | rowid order = last-write order (`INSERT OR REPLACE` moves overwritten rows); DynamoDB Scan order is unspecified |
| Storage row shape | `[][]byte` blobs only; `id`/`range` never cross the storage boundary |
| `LEK` source | Last *scanned* item's key attributes (decoded from blob), never the condition values |
| `Limit` semantics | Caps *scanned* items (before filter); `LEK` set iff `ScannedCount == Limit` (even at scope end → trailing empty page); nil iff `ScannedCount < Limit` or unset. No `limit+1` fetch — dropped after probe (§5.6) proved the limit+1 trick can't reproduce the `Limit == available` case |
| `Limit` encoding | Engine `int32`, `0` = unset; SDK-present `0` rejected by the adapter — confirmed by probe (§5.6); negative rejected by the engine |
| `Select=COUNT` | In scope; drops `Items`, keeps `Count`/`ScannedCount`/`LEK` |
| `IndexName` | Rejected → `ErrGsiNotFound` (M4) |
| `Segment`/`TotalSegments` | Parallel scan via `id % N == segment` |
| Legacy params | Adapter rejects non-empty with `ValidationException`; reference (`dynamodb-local`) accepts them — documented adapter-only divergence (case 32) |
| `ScanIndexForward` default | Adapter normalizes nil → `true` |
| Filter wiring | Bind once, `Eval` per row; `ValidateFilterKeys` on both `Query` and `Scan` |
| Substitution union | `KeyConditionExpression` + `FilterExpression` refs unioned, `CheckUnused` once |
| Fuzzing | No new targets; existing grammar fuzz covers the reused parser |

### 9.2 Risks & mitigations

1. **`dynamodb-local` diverges from documentation on pagination edges.** The rejection of `Limit=0` and the "either order of PK/SK in `AND`" assumption were documentation-light. **`Limit=0` is now settled** — the probe (§5.6) confirmed `dynamodb-local:3.3.1` rejects it with `ValidationException` on both `Query` and `Scan`, matching the spec. The "either order" assumption remains a hypothesis until case 19 runs. *Mitigation:* the dual-target gate turns each into a decision point during implementation. Where `dynamodb-local` contradicts this spec, the reference wins and the spec is amended in the same commit — the same lesson M2 recorded (§9.2 risk 4 of the M2 spec: "a semantic claim that has never been run against `dynamodb-local` is a hypothesis, not a spec").
2. **`Scan` resume by rowid is an internal leak.** `ddb` resolves `ExclusiveStartKey` to an `id` via `GetItem`, which is an extra round trip on the first page of a resumed scan. *Mitigation:* one indexed point lookup is negligible; the `id` never crosses the storage boundary in the output (`LEK` carries only key attributes). The round trip is the price of keeping `id` internal to storage.
3. **`begins_with` lexicographic-range translation in `ddb`.** Computing the successor of a string/byte prefix (for the `< successor` bound) must handle the last-byte-`0xFF` edge (carry left); when every byte is `0xFF` (or the prefix is empty) no successor exists, and `ddb` passes `Hi = nil` so storage emits only `range >= ?`. *Mitigation:* dedicated unit tests for the successor function including the empty and all-`0xFF` edges (§8.3); conformance case 17 exercises `begins_with` on an S sort key against the reference.
4. **`ResumeAfter` combined with the original sort condition.** A `BETWEEN` resumed after `lekSortVal` must keep both bounds: `range >= :a AND range <= :b AND range > lekSortVal`. Storage appends `ResumeAfter` as an additional `AND range > ?` to whatever predicate the original condition generated. *Mitigation:* storage unit test for `Query` with `BETWEEN` + `ResumeAfter`; conformance case 20 exercises multi-page `Query` with a `BETWEEN`.
5. **Parallel scan `id % N` evenness.** If rows are unevenly distributed, some segments may return more rows than others. *Mitigation:* this matches DynamoDB's own behavior (parallel segments are not size-balanced); conformance case 27 verifies disjointness and union-equals-full, not per-segment count equality.
6. **`ListTables` may share the `limit+1` divergence.** The `fetch limit+1` trick was dropped for Query/Scan (§4.5) because the probe proved `LEK` is set iff `ScannedCount == Limit`, not iff "more remain." `ListTables` (M1, already shipped) uses the same `limit+1` pattern and may diverge from `dynamodb-local` in the `Limit == table-count` edge. *Mitigation:* verify `ListTables` pagination against `dynamodb-local` in M6 hardening (or when an M3 conformance case exposes a mismatch); if it diverges, apply the same `ScannedCount == Limit` rule. Not an M3 blocker — `ListTables` is not in the M3 scope.

### 9.3 Explicitly out of scope for M3

- GSI support, `IndexName` queries/scans, read-time projection — M4.
- `BatchWriteItem` / `BatchGetItem` / TTL — M5.
- `UpdateTable` (GSI add/remove) — M6.
- Expression length (4KB), `IN` operand count (100), and document-path nesting-depth caps — M6 hardening.
- `ProjectionExpression`, `TransactWriteItems`, PartiQL — v1 non-goals.
- `Select=SPECIFIC_ATTRIBUTES` and `Select=ALL_PROJECTED_ATTRIBUTES` — require `ProjectionExpression` / GSI respectively, both v1 non-goals.
- Full item-size accounting; M3 continues to use M1's JSON byte-length proxy.
- Consumed-capacity accounting, throttling — v1 non-goals.
