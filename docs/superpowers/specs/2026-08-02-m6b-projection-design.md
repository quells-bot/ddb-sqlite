# M6B — Projection Expressions

**Date:** 2026-08-02
**Status:** Implemented
**Parent spec:** `docs/superpowers/specs/2026-07-31-ddb-sqlite-design.md`

## 1. Overview

M6B brings `ProjectionExpression` into scope for v1, ahead of the M6 hardening sweep. The parent spec listed `ProjectionExpression` as a v1 non-goal (§1, §8); M6B supersedes that decision. The M5b BatchGetItem projection rejection (§1.1.3 of the M5b spec) is **resolved** — the fields are now honored, and the adapter-only rejection tests flip to dual-target conformance cases asserting honoring.

**Surface delivered:** `ProjectionExpression` + `ExpressionAttributeNames` honored on **GetItem, Query, Scan, BatchGetItem** (per-table on the last). `TransactGetItems` remains a v1 non-goal.

**Faithful scope:**
- Full `Select` model: `SPECIFIC_ATTRIBUTES` valid when a projection is present; `COUNT` + projection rejected; `ALL_PROJECTED_ATTRIBUTES` + projection rejected; `ALL_ATTRIBUTES` + projection on a non-ALL GSI rejected.
- GSI restriction: on a GSI Query/Scan, a `ProjectionExpression` may name only attributes the index projects (table keys ∪ GSI keys ∪ `INCLUDE`-projected attrs). Base-table queries unrestricted.
- Legacy `AttributesToGet` stays rejected: adapter-side on Query/Scan (existing `rejectLegacyQuery`/`rejectLegacyScan`), engine-side on BatchGetItem (existing), and **newly** adapter-side on GetItem — where it is currently *silently ignored*, not rejected (see §5). The reference honors `AttributesToGet` alone; rejecting it everywhere is the same deliberate divergence class as Query/Scan.

## 2. Probe findings (dynamodb-local 3.3.1)

All core semantics were measured against `dynamodb-local:3.3.1` via the AWS SDK v2 before being asserted. The probe file (`awsdynamodb/projection_probe_test.go`) is dropped after findings are ported into conformance cases.

### 2.1 Corrected assumptions

Three assumptions from the initial design draft were corrected by the probe:

1. **Key attributes are NOT auto-returned.** Projecting `top` returns only `{"top": …}` — no `pk`/`sk`. `LastEvaluatedKey` is still computed internally from the full item; it simply does not appear in the returned attributes. (The initial draft assumed keys were always included.)
2. **Duplicate paths are REJECTED, not deduped.** `top, top` → `ValidationException: Invalid ProjectionExpression: Two document paths overlap with each other; must remove or rewrite one of these paths; path one: [top], path two: [top]`.
3. **Overlapping parent/child paths are REJECTED, not merged.** `obj, obj.a` → same `ValidationException` with `path one: [obj], path two: [obj, a]`.

### 2.2 Confirmed semantics

| Behavior | Probe result |
|---|---|
| Projected path not present in item | Omitted, no error |
| Descend into a non-container (`top.nested` where `top` is String) | Accepted, path absent from result (does not resolve) |
| Nested paths preserve spine | `obj.nested.x` → `{"obj":{"nested":{"x":…}}}` |
| Sibling nested merge | `obj.a, obj.b` → both under one `obj` map |
| List elements projectable | `arr[1]` → `{"arr":[{"S":"e1"}]}` (single-element list) |
| `#name` substitution | Works identically to condition/filter expressions |
| Reserved word bare use | `ValidationException: Attribute name is a reserved keyword` |
| Reserved word via `#name` alias | Accepted |
| Empty `ProjectionExpression` | `ValidationException: Invalid ProjectionExpression: The expression can not be empty` |
| `ProjectionExpression` + `AttributesToGet` both set | `ValidationException: Can not use both expression and non-expression parameters in the same request` |
| `Count`/`ScannedCount` unaffected by projection | Identical counts with and without projection |
| Projection applied after filter | `Count=1, ScannedCount=2` when filter discards one of two scanned items |

### 2.3 `Select` + projection interaction

| `Select` | Projection? | GSI context | Result |
|---|---|---|---|
| `""` | no | base table | `ALL_ATTRIBUTES` (default) |
| `""` | no | non-ALL GSI | `ALL_PROJECTED_ATTRIBUTES` (default) |
| `""` | yes | any | `ALL_ATTRIBUTES` (projection governs returned attrs) |
| `ALL_ATTRIBUTES` | no | non-ALL GSI | `ValidationException` |
| `ALL_ATTRIBUTES` | yes | non-ALL GSI | `ValidationException`: *"Cannot specify the ProjectionExpression when choosing to get ALL_ATTRIBUTES"* |
| `ALL_PROJECTED_ATTRIBUTES` | yes | GSI | `ValidationException`: *"Cannot specify the ProjectionExpression when choosing to get ALL_PROJECTED_ATTRIBUTES"* |
| `COUNT` | yes | — | `ValidationException`: *"Cannot specify the ProjectionExpression when choosing to get only the Count"* |
| `SPECIFIC_ATTRIBUTES` | yes | — | Accepted; equivalent to projection alone |
| `SPECIFIC_ATTRIBUTES` | no | — | `ValidationException` (needs projection) |

### 2.4 GSI restriction

On a GSI Query/Scan, a `ProjectionExpression` may name only attributes the index projects. Probe-verified:

- KEYS_ONLY GSI: projecting `top` (non-projected) → `ValidationException: One or more parameter values were invalid: Global secondary index gkeys does not project [top]`
- KEYS_ONLY GSI: projecting `pk, sk` (table keys) → accepted, returns `{"pk":…, "sk":…}`
- INCLUDE GSI: projecting `top` (included attr) → accepted
- INCLUDE GSI: projecting `num` (not included) → `ValidationException: Global secondary index gincl does not project [num]`

Probed on **Query and Scan** — probe 26 confirmed the Scan rejection holds identically (see §2.5). The projected-attr set is defined over **top-level attribute names** (first path segment); nested `INCLUDE` entries are excluded — see §4.7.

### 2.5 Second probe round — findings (probes 24–28)

A second probe round (probes 24–28 in the probe file) measured the five remaining semantics against `dynamodb-local:3.3.1`:

| # | Probe | Measured result | Verdict |
|---|---|---|---|
| 24 | Multiple indices of one list | `arr[0], arr[2]` → `[e0 e2]`; `arr[2], arr[0]` → `[e0 e2]` | Reference emits **source-index order**: `arr[2], arr[0]` returns `[e0, e2]`. §3.2's provisional path-order rule is replaced by "slots sorted by source index before emitting". |
| 25 | Convergent paths through one index | `marr[1].x, marr[1].y` → one result element `{"x":…,"y":…}` | **Confirmed** — converges into a single list element, not two. |
| 26 | GSI restriction on Scan | KEYS_ONLY GSI Scan projecting `top` → `ValidationException: … Global secondary index gkeys does not project [top]` | **Confirmed** — same rejection as Query; §2.4's Scan extrapolation is now probe-verified. |
| 27 | BatchGetItem empty projection | per-table `ProjectionExpression: ""` → `ValidationException: Invalid ProjectionExpression: The expression can not be empty` | **Confirmed** — parity with the GetItem empty-expression rejection. |
| 28 | GetItem `AttributesToGet` alone | Returns `["top"]` | **Confirmed (documentation only)** — the reference honors `AttributesToGet` alone; rejecting it in the adapter remains the deliberate divergence of §1. |

## 3. Engine internals

Three new units, each with one clear purpose, communicating through well-defined interfaces. All follow existing patterns in the codebase.

### 3.1 `expr.Projection` — parse + bind (`internal/expr`)

A new type symmetric with `Condition` and `Update`:

```go
// Projection is a parsed projection expression. Independent of substitution
// maps: call Bind to resolve #name refs, then the resolved paths feed
// attrval.Project.
type Projection struct {
    paths []*pathOperand // reuses the existing pathOperand from ast.go
    names []string        // #name refs, feeds the joint CheckUnused
}

func ParseProjection(src string) (*Projection, error)
func (p *Projection) Refs() (names, values []string)  // values always nil
func (p *Projection) Bind(env Env) (*BoundProjection, error)

// BoundProjection is a projection with every #name resolved to an
// attrval.Path. Independent of the *Projection it came from.
type BoundProjection struct {
    paths []attrval.Path
}

func (b *BoundProjection) Paths() []attrval.Path
```

**`ParseProjection`** reuses the existing lexer (`lex.go`), then parses comma-separated document paths via the existing `parsePath()`/`parseNameSeg()`. It rejects `:value` refs, `size()`, comparators, and any token that isn't a path or comma — a projection expression is *only* paths (`:v` and `size(` already fail `parseNameSeg`, so rejection falls out of the grammar). Empty string → error (probe-verified), enforced here for direct `expr` users. Note the engine wiring never passes `""` — every op gates on `ProjectionExpression != ""`, and the engine's plain-string API cannot distinguish absent from present-but-empty — so the SDK-visible empty rejection lives in the **adapter** (`exprString`, §5). Tracks `#name` refs via the existing `addName` mechanism.

**`Bind`** reuses the existing `binder.path()` (`bind.go`) to resolve every `#name` → `attrval.Path`, producing `[]attrval.Path`. Undefined `#name` → `ErrUndefined` (same error path as condition/update bind).

**Overlap check** runs at bind time (before any item is read), reusing the existing `pathOverlaps(a, b)` function from `bind.go` — already used by the update evaluator. Any pair of resolved paths where one is a prefix of the other (equal paths included) → `ValidationException: Two document paths overlap with each other`. This matches the probe-verified rejection of both `top, top` and `obj, obj.a`.

### 3.2 `attrval.Project` — extraction (public, `attrval/`)

```go
// Project returns a copy of item containing only the values at the given
// paths, preserving document spines. Paths that do not resolve (missing
// attribute, or descending into a non-container) are omitted — no error.
// Overlapping paths must be rejected by the caller before calling Project.
func Project(item map[string]Value, paths []Path) map[string]Value
```

Parallels `Lookup`/`SetPath`/`RemovePath` — the fourth document-path operation on `attrval.Value`. For each path: navigate via the existing `Lookup`; if found, insert into the result spine; if not found, skip. Only the touched spine is copied; the receiver is unchanged.

Spine construction merges by segment kind, and list merging is the subtle part:

- **Map spines merge by name.** Sibling paths under the same parent merge naturally (both `obj.a` and `obj.b` land under one `obj` map).
- **List spines merge by source index, not append order.** Paths converging on the same index of the same list land in ONE result element: `arr[1].x, arr[1].y` → `{"arr":[{"x":…,"y":…}]}`, not two elements. Construction therefore tracks source indices alongside result slots (parallel slices or a small slot struct) while building.
- **Result lists are compacted.** Probe-verified: `arr[1]` on a 3-element list → `{"arr":[elem]}` — the element lands at result index 0; gaps are not preserved.
- **Multiple indices of one list:** slots are sorted by **source index** before emitting (probe-verified, §2.5 probe 24). Both `arr[0], arr[2]` and `arr[2], arr[0]` return `[e0, e2]` — the reference emits in source-index order, not path order. Project therefore sorts result slots by source index during construction, so `arr[2], arr[0]` → `[v0, v2]`.

Public because `ddb` (a separate package) calls it at read time.

### 3.3 `ddb` wiring — `prepareExpressions` extension

Two field additions to the existing structures in `ddb/expressions.go`:

```go
type expressionRequest struct {
    Condition  string
    Update     string
    Filter     string
    Projection string          // NEW
    Names      map[string]string
    Values     map[string]attrval.Value
}

type preparedExpressions struct {
    Cond   *expr.BoundCondition
    Update *expr.BoundUpdate
    Filter *expr.BoundCondition
    Proj   *expr.BoundProjection  // NEW
}
```

`prepareExpressions` gains a projection parse+bind block after the existing filter block. The projection's name refs are unioned into the existing joint `CheckUnused` — a `#name` referenced only by the projection must not be reported unused, and a `#name` in the map that no expression references is still rejected. The projection's value refs are always nil (projections have no `:value` refs).

**Scan wiring subtlety:** Scan currently gates its `prepareExpressions` call on `FilterExpression != ""`. With projection added, the call must also fire when `ProjectionExpression != ""`, `len(ExpressionAttributeNames) > 0`, **or `len(ExpressionAttributeValues) > 0`**, so the unused-check runs — names-only and values-only requests are rejected symmetrically (today values-only silently escapes the check). Query already calls unconditionally (it always has a `KeyConditionExpression`).

## 4. Operation integration

### 4.1 Input struct changes

**GetItemInput** gains two fields (it currently has no expression fields at all):

```go
type GetItemInput struct {
    TableName                 string
    Key                       Item
    ConsistentRead            bool
    ProjectionExpression      string            // NEW
    ExpressionAttributeNames  map[string]string // NEW
}
```

**QueryInput / ScanInput** already have `ExpressionAttributeNames`; each gains `ProjectionExpression string`. (Their stale `Select` doc comments — `"" (default ALL_ATTRIBUTES) or "COUNT"` — are corrected as part of §4.6/§7.)

`AttributesToGet` gets **no engine field** on GetItem: it is rejected adapter-side (§5), matching the existing Query/Scan pattern.

**KeysAndAttributes** (BatchGetItem) already has `ProjectionExpression`/`ExpressionAttributeNames`/`AttributesToGet` fields — no struct change needed, just the rejection removed.

### 4.2 GetItem

Currently does not call `prepareExpressions`. After: calls it when `ProjectionExpression != ""` or `ExpressionAttributeNames` is non-empty. The adapter rejects a present-but-empty `ProjectionExpression`, any `AttributesToGet`, and a present-but-empty `ExpressionAttributeNames` map accompanying a projection **before** the engine is called (§5), so the engine never sees those shapes.

Validation order (table → key → expression). This precedence is a **chosen order, not probe-verified** — no probe tested a doubly-invalid request (e.g. bad key + bad projection); it matches the engine's existing write-op order, and conformance does not assert double-fault precedence:

1. Table lookup → `ErrTableNotFound`
2. `validateKey` → typed key error
3. `prepareExpressions({Projection, Names})` → `ErrValidation` (parse/bind/overlap/unused failures)
4. Storage `GetItem`
5. If found and `ex.Proj != nil`: `attrval.Project(item, ex.Proj.Paths())` applied to the unmarshaled item before return. If not found: empty `Item{}` (unchanged).

**Acknowledged divergence (found-but-nothing-projected):** when the item exists but no projected path resolves, `Project` returns an empty map and the adapter's existing `len(out.Item) == 0` check omits the `Item` field — the same wire shape as NOT FOUND. The reference returns `"Item": {}` for this case. The distinction is invisible to the conformance suite's length-based assertions and is accepted.

### 4.3 Query

Already calls `prepareExpressions` unconditionally. Pass `Projection` into the request. Validation order — **the existing order is kept**: `validateSelect` currently runs before `prepareExpressions`, and `hasProjection = in.ProjectionExpression != ""` needs no parse, so nothing forces a swap. The relative precedence of Select vs. expression errors is unprobed; keeping the current order avoids churn:

1. Table lookup
2. `validateSelect(in.Select, gsiProjection, hasProjection)` — **revised** (see §4.6)
3. `prepareExpressions({Condition: KeyCondition, Filter, Projection, Names, Values})` — parse/bind all, joint unused-check once
4. Key-condition extraction + filter-key-attr validation (existing)
5. **GSI projection restriction** (when `IndexName` is set and `ex.Proj != nil`): check every resolved path's first segment (always a name — `parseNameSeg` never produces a leading index) against the GSI's projected-attr set: table keys ∪ GSI keys ∪ the **top-level** `INCLUDE`-projected attrs (nested entries excluded — see §4.7). Reject with the probe-verified message if any path names a non-projected attr.
6. Storage query → scan loop. **Projection applied per-item after filter**: if `ex.Proj != nil`, `attrval.Project(item, paths)` on each kept item. If `Select == "COUNT"`: items set to nil (existing behavior), projection irrelevant (already rejected at step 2).

**LEK invariant:** `LastEvaluatedKey` continues to be built from the **raw blobs**, never from projected items (the current code unmarshals `blobs[len(blobs)-1]` separately — keep it that way). A projected item may omit every key attribute (§2.1.1); an LEK derived from it would be empty and pagination would silently break.

### 4.4 Scan

Same as Query minus the key-condition extraction, with two points spelled out so they are not lost in the elision:

- **The GSI projection restriction (§4.3 step 5) applies to GSI Scan identically.** It was probed on Query only; the §6.5 Scan conformance case verifies the extrapolation dual-target.
- The existing `FilterExpression != ""` gate on `prepareExpressions` is **broadened** to `FilterExpression != "" || ProjectionExpression != "" || len(ExpressionAttributeNames) > 0 || len(ExpressionAttributeValues) > 0` so the unused-check fires when only projection + substitution maps are present (names-only and values-only symmetrically; §3.3).

The LEK invariant from §4.3 applies unchanged.

### 4.5 BatchGetItem

The rejection block in phase 1 (`ka.ProjectionExpression != "" || len(ka.ExpressionAttributeNames) > 0 || len(ka.AttributesToGet) > 0` → single `ErrValidation`) is removed. Each per-table `KeysAndAttributes` now carries its projection through validation and read:

1. Existing validation (table, empty keys, duplicate keys, key types)
2. **Per-table projection parse/bind**: if `ka.ProjectionExpression != ""`, parse+bind via `expr.ParseProjection`/`Bind` with `ka.ExpressionAttributeNames`; the unused-check runs per-table via the exported `expr.CheckUnused`. The overlap check runs inside `Bind` (§3.1), so it needs no separate wiring. (The adapter has already rejected a present-but-empty projection, and a present-but-empty names map accompanying one — §5 — so `""` here means absent.)
3. **`AttributesToGet` rejection stays** — rejected with `ErrValidation` (deprecated; cannot mix with expression parameters). This subsumes the mixed case: any `AttributesToGet` rejects regardless of whether a projection is also present.
4. Read phase: read all of the table's items, then **sort first, project after**. The per-table `sort.Slice` compares via `compareItems`, which reads key attributes **out of the items** — and keys are usually absent from a projection (§2.1.1: not auto-returned). Projecting before sorting would compare zero `Value`s (`""` strings, invalid zero `Decimal`) and — since `sort.Slice` is unstable — silently scramble the M5b deterministic key-ordering. So: sort the full found items, then apply `attrval.Project(item, paths)` to each before placing them in `Responses`.

The `KeysAndAttributes` doc comment is updated: fields are now honored, no longer "v1 non-goals."

### 4.6 `validateSelect` revision

Current signature: `validateSelect(s string, gsiProjection string) (string, error)`. Revised to take the projection-present flag:

```go
func validateSelect(s string, gsiProjection string, hasProjection bool) (string, error)
```

New rules (all probe-verified, table in §2.3):

| `Select` | Projection? | GSI context | Result |
|---|---|---|---|
| `""` | no | base | `ALL_ATTRIBUTES` |
| `""` | no | non-ALL GSI | `ALL_PROJECTED_ATTRIBUTES` |
| `""` | yes | any | `ALL_ATTRIBUTES` (projection governs) |
| `ALL_ATTRIBUTES` | no | non-ALL GSI | rejected |
| `ALL_ATTRIBUTES` | yes | non-ALL GSI | rejected |
| `ALL_PROJECTED_ATTRIBUTES` | yes | GSI | rejected |
| `COUNT` | yes | — | rejected |
| `SPECIFIC_ATTRIBUTES` | yes | — | accepted |
| `SPECIFIC_ATTRIBUTES` | no | — | rejected (needs projection) |

### 4.7 Nested `INCLUDE` attributes (accepted, never projected — matches reference)

The engine has accepted nested paths in `Projection.NonKeyAttributes` at `CreateTable` since M4 (mirroring dynamodb-local's laxness — the reference accepted `["top", "obj.a"]` in the probe fixture; real AWS documents top-level names only). They do not actually work: the M4 read-time trim (`gsiProjectionAttrs`) keeps a literal top-level `"obj.a"`, a name no item carries, so the attribute is silently dropped from GSI reads.

M6B keeps that behavior and makes the restriction check consistent with it: the §4.3-step-5 projected-attr set contains **top-level names only** — a nested entry like `obj.a` contributes nothing, so projecting `obj` or `obj.a` on such an index is rejected with "does not project". M6c W4 (probe P-include, 2026-08-03) confirmed dynamodb-local behaves identically — accepts `NonKeyAttributes: ["obj.a"]` at `CreateTable`, accepts the write, and never projects the nested entry — so the engine matches the reference and there is no divergence; `TestConfGSINestedInclude` pins this dual-target. Conformance fixtures otherwise use top-level `INCLUDE` attrs only.

## 5. Adapter (`awsdynamodb`)

The adapter owns every check that needs the nil-vs-empty distinction the engine's plain-string API cannot express. The codebase already has the pattern: `exprString` rejects a present-but-empty expression string ("the distinction can only be made here"), and `rejectEmptySubMaps` rejects a present-but-empty substitution map accompanying an expression. Projection threads through both.

**GetItem:**
- `exprString(params.ProjectionExpression, "ProjectionExpression")` → `ddb.GetItemInput.ProjectionExpression` — NOT `aws.ToString`, which would flatten `aws.String("")` to `""` = absent, letting the §6.1 empty-projection case silently succeed.
- `params.ExpressionAttributeNames` → `ddb.GetItemInput.ExpressionAttributeNames` (already a plain map, threaded as-is).
- New `rejectLegacyGetItem(params)`, symmetric with `rejectLegacyQuery`/`rejectLegacyScan`: rejects any `AttributesToGet` with `ValidationException`. **This closes a real gap: GetItem currently silently ignores `AttributesToGet`** — it neither honors nor rejects it — and the §6.1 mixed case can only pass dual-target if it rejects. `AttributesToGet`-alone is functional on the reference (§2.5 probe 5 documents this); rejecting it is the same deliberate divergence class as Query/Scan.
- `rejectEmptySubMaps` gains a GetItem call site (projection as the only expression, `nil` values — GetItem has no `ExpressionAttributeValues` field), with the projection string counted as an expression (below).

**Query/Scan:** `exprString(params.ProjectionExpression, "ProjectionExpression")` → `ddb.QueryInput`/`ddb.ScanInput.ProjectionExpression`. `ExpressionAttributeNames` already threaded for filter/key-condition.

**`rejectEmptySubMaps` extension:** the helper currently decides "an expression is present" from the condition/update/filter strings alone. It gains the projection string at every call site (mechanical: make the expression args variadic, pass projection wherever the op carries one — GetItem, Query, Scan), so a present-but-empty `ExpressionAttributeNames`/`ExpressionAttributeValues` map accompanying only a projection is rejected like any other expression.

**BatchGetItem (per-table):** the field threading already exists. Add two per-table pointer-level checks before calling the engine: `ka.ProjectionExpression != nil && *ka.ProjectionExpression == ""` → `ValidationException` (parity with `exprString`; §2.5 probe 4 confirms the reference rejects), and `ka.ExpressionAttributeNames` present-but-empty with a projection present → `ValidationException` (parity with `rejectEmptySubMaps`).

**`AttributesToGet`:** rejected everywhere, in three places: adapter-side on Query/Scan (existing `rejectLegacyQuery`/`rejectLegacyScan`) and now GetItem (`rejectLegacyGetItem`); engine-side on BatchGetItem (existing rejection, stays per §4.5 item 3). **Correction:** an earlier version of this section claimed it "continues passing through on all ops" — it does not pass through on GetItem today; it is silently ignored there.

**Error mapping:** No new error types. Projection failures (parse, bind, overlap, unused substitution, GSI restriction) all surface as `ErrValidation` → `ValidationException` via the existing `mapError`.

## 6. Conformance cases

All new cases are dual-target (adapter + dynamodb-local), following the existing `runConformance(t, fn)` pattern. Cases grouped by concern:

### 6.1 Basic projection semantics (GetItem)

- Project a single top-level attr → only that attr returned (no keys)
- Project multiple top-level attrs → exactly those attrs
- Project a missing attr → omitted, no error
- Project a nested path → spine preserved
- Project sibling nested paths (`obj.a, obj.b`) → both under one `obj`
- Project a list element (`arr[1]`) → single-element list
- `#name` substitution in projection
- Reserved word requires `#name` alias
- Empty `ProjectionExpression` → `ValidationException`
- `ProjectionExpression` + `AttributesToGet` → `ValidationException`

### 6.2 Overlap rejection

- Duplicate path (`top, top`) → `ValidationException`
- Parent + child (`obj, obj.a`) → `ValidationException`

### 6.3 Query/Scan projection

- Query with projection → projected attrs only, `Count`/`ScannedCount` unaffected
- Query projection applied after filter (`Count=1, ScannedCount=2`)
- Scan with projection → projected attrs only
- `Select=COUNT` + `ProjectionExpression` → `ValidationException`
- `Select=SPECIFIC_ATTRIBUTES` + `ProjectionExpression` → accepted, returns projected attrs

### 6.4 BatchGetItem (divergence resolution)

- BatchGetItem with `ProjectionExpression` → honored, projected attrs returned
- BatchGetItem with `ProjectionExpression` + `#name` substitution → honored (covers the names path)
- BatchGetItem with `ExpressionAttributeNames` but no projection → `ValidationException` (unused names) — replaces the coverage lost with the removed unit test
- The existing `TestAdapterBatchGetProjectionRejected` and `TestAdapterBatchGetExpressionNamesRejected` adapter-only unit tests are **removed** — superseded by the dual-target cases above
- `TestAdapterBatchGetAttributesToGetRejected` stays as an adapter-only rejection (deprecated parameter; the reference honors `AttributesToGet` alone — deliberate divergence, §1)

### 6.5 GSI restriction

- KEYS_ONLY GSI: project non-projected attr → `ValidationException`
- KEYS_ONLY GSI: project key attrs → accepted
- INCLUDE GSI: project included attr → accepted
- INCLUDE GSI: project non-included attr → `ValidationException`
- `Select=ALL_PROJECTED_ATTRIBUTES` + `ProjectionExpression` on GSI → `ValidationException`
- KEYS_ONLY GSI **Scan**: project non-projected attr → `ValidationException` (verifies the §2.4 extrapolation from Query; if the reference diverges, the case moves to a divergence file per repo convention)

### 6.6 Edge cases

- `top.nested` where `top` is a String → accepted, `top` absent from result (path doesn't resolve)
- Multiple indices of one list (`arr[0], arr[2]` and `arr[2], arr[0]`) → order/shape per the §2.5 probe; written after the probe lands (§3.2 provisional rule)
- Convergent paths through one index (`arr[1].x, arr[1].y`) → one merged element

### 6.7 Probe file disposal

`awsdynamodb/projection_probe_test.go` first gains the §2.5 probes, then is deleted after all findings are ported into conformance cases — same pattern as M3/M4/M5a/M5b probes. The `localDB` helper it defines is only used by probes; it goes with the file.

Note: three committed probes encode the superseded initial-draft assumptions and **fail against the reference by design** (`TestProbeProjKeyAttrsAlwaysReturned`, `TestProbeProjDuplicateDedup`, `TestProbeProjOverlapParentChild` — their failures were the §2.1 corrections). `DDBSQLITE_CONF_TARGET=all` stays red on them until the file is deleted; that is expected, not a regression.

## 7. Spec & doc updates

- **Parent spec §1, §8:** Remove `ProjectionExpression` from the out-of-scope lists.
- **Parent spec §6.2 `BatchGetItem`:** Update from "rejected" to "honored."
- **Parent spec §6.3/§6.4:** Add `ProjectionExpression` to Query/Scan descriptions.
- **M5b spec §1.1.3:** Mark the BatchGetItem projection rejection as **resolved by M6B**.
- **AGENTS.md:** Add M6B to the milestone table; update implementation status.
- **`ddb/query_test.go`:** the existing `validateSelect` table test calls the 2-arg signature — update for the 3-arg signature and add rows for the new projection-aware rules (§4.6).
- **`QueryInput`/`ScanInput` `Select` doc comments** (`ddb/query.go`): currently `"" (default ALL_ATTRIBUTES) or "COUNT"` — stale since M4; rewrite for the full §4.6 model.

## 8. Scope boundaries (out of scope for M6B)

- `TransactGetItems` (v1 non-goal, parent spec §8).
- Nested `INCLUDE` `NonKeyAttributes` — accepted at `CreateTable` but never projected (pre-existing M4 gap); M6B only makes the GSI restriction check consistent with that reality (§4.7). Rejecting them at `CreateTable` or implementing nested projection is deferred. — resolved by M6c W4 (2026-08-03): P-include confirmed dynamodb-local also accepts nested entries at CreateTable but never projects them; engine behavior matches the reference, no divergence, deferral closed.
- `ProjectionExpression` on write operations (`PutItem`/`UpdateItem`/`DeleteItem`/`BatchWriteItem`) — these do not return items (except via `ReturnValues`, which is a separate projection mechanism already implemented in M2).
- Expression length / path-count caps (M6 hardening).
- Full item-size accounting (M6 hardening).
