# ddb-sqlite M2 — Expressions Design

**Date:** 2026-07-31
**Status:** Approved (brainstorming) → pending implementation plan
**Parent spec:** `docs/superpowers/specs/2026-07-31-ddb-sqlite-design.md` (§5, §11 M2 milestone)
**Prerequisite:** M1 complete — `internal/storage`, `ddb` table + key-based item ops, `awsdynamodb` adapter, and the dual-target conformance harness (both passes) merged.

## 1. Overview & goal

M2 adds the expression engine and wires it into the item operations. It delivers `internal/expr` — one lexer, two grammars (condition/filter and update), a bind phase, and two evaluators — plus the engine and adapter changes that expose conditional writes and `UpdateItem`.

M2 delivers:

- `internal/expr` — lexer, recursive-descent parsers, typed ASTs, bind/validation, condition-filter evaluator, update evaluator.
- `attrval` — two new methods, `SetPath` and `RemovePath`, so the update evaluator can write into nested document paths.
- `ddb` — `ConditionExpression` + `ReturnValues=ALL_OLD` on `PutItem`/`DeleteItem`; a new `UpdateItem` with the full `ReturnValues` set; a new `ConditionalCheckFailedError` carrying the pre-write item.
- `awsdynamodb` — expression pass-through, `UpdateItem`, legacy-parameter rejection, and `ConditionalCheckFailedException.Item` population.
- Conformance cases 1–12 (§7.3), gated on both targets.

The filter evaluator is built in M2 but has no public consumer until M3 wires `Query`/`Scan`. This is deliberate: condition and filter expressions share one grammar and one evaluator, so building only "the condition half" would mean writing the same code twice.

## 2. Approach: hand-written parser, three-phase pipeline

### 2.1 Parser strategy (decision)

A hand-written lexer plus recursive-descent parsers producing typed ASTs, evaluated by tree walkers.

Rejected alternatives:

- **`goyacc` / a parser-combinator library.** Adds either a codegen build step or an external dependency to a repo whose only deps are `modernc.org/sqlite` (root) and the AWS SDK (adapter), and yields worse error text. Expression error messages become `ValidationException` message bodies, so they matter.
- **Single-pass interpret-while-parsing (no AST).** Least code, but M3 must parse a `FilterExpression` once and evaluate it against every scanned row; a no-AST design forces a re-parse per item and cannot be validated independently of an item.

The DynamoDB expression grammar is small and frozen, which is what makes hand-writing it the low-risk option.

### 2.2 Three-phase pipeline (decision)

Substitution is **not** resolved during parsing. Three phases:

```
Parse(src)                → AST            item- and environment-independent; #x and :v are nodes
AST.Bind(names, values)   → bound form     resolves substitutions, validates undefined refs
bound.Eval(item)          → bool           condition / filter
bound.Apply(item)         → item, touched  update
```

Consequences that justify the split:

- Parsing stays pure and fuzzable — no maps required to parse.
- The undefined/unused substitution checks live in one well-defined place rather than being smeared through the parser.
- M3 can bind a filter once and call `Eval` per row.

## 3. Package layout

```
ddb-sqlite/
├─ attrval/
│  └─ path.go          # EXTENDED: SetPath, RemovePath alongside ParsePath/Lookup
├─ internal/
│  └─ expr/            # NEW
│     ├─ lex.go        # token kinds + scanner; shared by both grammars
│     ├─ ast.go        # condition/filter node types; update clause + action types
│     ├─ parse.go      # ParseCondition, ParseUpdate
│     ├─ bind.go       # Env, Bind, Refs, CheckUnused
│     ├─ eval.go       # (*BoundCondition).Eval
│     ├─ update.go     # (*BoundUpdate).Apply
│     └─ errors.go     # ErrSyntax, ErrUndefined, ErrUnused, ErrSemantic
├─ ddb/
│  ├─ items.go         # EXTENDED: conditions on Put/Delete, new UpdateItem
│  └─ errors.go        # EXTENDED: ConditionalCheckFailedError
└─ awsdynamodb/
   ├─ adapter.go       # EXTENDED: expression pass-through, UpdateItem, legacy rejection
   └─ conformance_test.go  # EXTENDED: cases 1-12
```

### 3.1 Dependency direction

`expr` imports `attrval` (and, transitively, `internal/num`). `expr` **must not** import `ddb` — that is the cycle direction, since `ddb` imports `expr`. Items therefore cross the boundary as `map[string]attrval.Value`, not as `ddb.Item`.

`expr` stays internal: the adapter never names an `expr` type, because SDK inputs already carry expressions as raw strings plus `ExpressionAttributeNames`/`ExpressionAttributeValues` maps, which the adapter passes straight through.

### 3.2 `attrval` additions

```go
func (v Value) SetPath(p Path, nv Value) (Value, error)
func (v Value) RemovePath(p Path) Value
```

Both return a new `Value`, copying only along the touched spine — the immutable-by-construction invariant in `attrval`'s package doc is preserved.

Semantics:

- `SetPath` creates **only the final segment** of the path. Every parent segment must already exist: setting `a.b.c` when `a.b` is absent is an error, and so is setting `a.b` when `a` is absent. DynamoDB reports these as `ValidationException: The document path provided in the update expression is invalid for update`. This is the rule behind the familiar `SET #m = if_not_exists(#m, :emptyMap)` idiom — a parent map has to be materialized by its own action before a child can be written. Note that `if_not_exists` on the *target* path does not excuse a missing parent: `SET a.b = if_not_exists(a.b, :v)` with no `a` is still an error.
- `SetPath` **clamps an out-of-range list index to an append**. Setting `a[3]` when `a` has two elements appends at index 2, as does `a[99]`; DynamoDB neither pads the list nor rejects the index. Setting an in-range index overwrites that element.
- `SetPath` through a segment whose existing value has the wrong container type (e.g. `.b` where `a` is a String) is an error.
- `RemovePath` on a **list index removes and shifts** subsequent elements. On a missing path it is a no-op returning the receiver unchanged.
- Neither prunes containers that become empty — DynamoDB does not auto-prune empty maps or lists. (Empty *sets* are a separate case; see §5.2 `DELETE`.)

### 3.3 Item lookup without copying

Evaluation must not wrap each item in `attrval.NewMap`, which copies the top-level map — at M3 that cost lands on every scanned row. A parsed `Path`'s first segment is always a name, so `expr` resolves segment 0 against the `map[string]attrval.Value` directly and delegates the remainder to the existing `Value.Lookup`. Missing segment 0 yields *missing*, identical to a missing nested segment.

## 4. Condition / filter expressions

### 4.1 Grammar

Precedence, lowest to highest: `OR` → `AND` → `NOT` → comparator / `BETWEEN` / `IN` / function. Parentheses override.

```
cond    := cond OR cond
         | cond AND cond
         | NOT cond
         | '(' cond ')'
         | operand comparator operand
         | operand BETWEEN operand AND operand
         | operand IN '(' operand {',' operand} ')'
         | func

comparator := '=' | '<>' | '<' | '<=' | '>' | '>='

operand := path | ':' name | 'size' '(' path ')'

func    := 'attribute_exists' '(' path ')'
         | 'attribute_not_exists' '(' path ')'
         | 'attribute_type' '(' path ',' operand ')'
         | 'contains' '(' path ',' operand ')'
         | 'begins_with' '(' path ',' operand ')'

path    := (name | '#' name) { '.' (name | '#' name) | '[' index ']' }
```

`#name` tokens are resolved at bind time, including in nested position (`#a.b`, `a.#b`). Keywords (`AND`, `OR`, `NOT`, `IN`, `BETWEEN`) are matched case-insensitively, as are function names, matching DynamoDB.

A bare (non-`#`) attribute name that is a DynamoDB reserved word is a `ValidationException`; the full 573-word list from the AWS docs is enforced at bind time, case-insensitively. A `#name` alias may resolve to any stored name, reserved or not — that is the escape hatch. Observed against `dynamodb-local`, which rejects e.g. bare `null` and `inner`.

Condition and filter expressions use this grammar identically. They differ in exactly one validation rule (§4.4).

### 4.2 Evaluation semantics

These rules are the faithfulness surface — the place a "mostly working" implementation silently diverges.

**Missing vs NULL.** A path that fails to resolve yields *missing*, which is distinct from a present `NULL` value. `attribute_exists` is **true** for a present `NULL`; `attribute_not_exists` is false. A comparison with a missing operand evaluates **false**, never an error — with one exception, settled against `dynamodb-local`: `<>` with a missing operand evaluates **true** (a missing attribute is by definition not equal to anything). This is real DynamoDB's documented quirk; the conformance case `missing inequality is true` pins it.

**Equality.** `=` and `<>` are defined across all ten types via `attrval.Equal`: numeric equality for N (`1` equals `1.0`), element-wise for sets (which are already deduped and canonically sorted at construction), structural for L and M. Across different types, `=` is false and `<>` is true.

**Ordering.** `<`, `<=`, `>`, `>=`, and `BETWEEN` are defined only for:

| Type | Order |
|---|---|
| S | UTF-8 byte order |
| N | exact decimal (`num.Decimal.Compare`) |
| B | unsigned byte order |

Applied to any other type, or to two operands of different types, an ordered comparison evaluates **false**. It is not an error — this matches DynamoDB and is the rule most implementations get wrong.

**`BETWEEN` with a lower bound greater than the upper bound is a `ValidationException`**, not false. Real DynamoDB rejects it during validation rather than evaluating it.

**`IN`** compares the left operand for equality against each listed operand, using the same `Equal` semantics. (The 100-operand cap is deferred to M6; see §9.)

**`size(path)`** is a value operand, not a boolean. Defined for S (UTF-8 byte length), B (byte count), L / M (element count), and SS / NS / BS (distinct element count — the dedup invariant makes this the stored length). Applied to a missing path it yields *missing*, so the enclosing comparison is false. `N` and `BOOL` are **not** supported: `size()` on them yields *missing*, so the enclosing comparison is false (settled against `dynamodb-local`; see §4.3(1)).

**`contains(path, operand)`:**

| Path type | Behavior |
|---|---|
| SS / NS / BS | set membership |
| S | substring; the operand must be S, else false |
| B | contiguous sub-sequence; the operand must be B, else false |
| L | element equality via `Equal` against any element; a type-mismatched element is **skipped**, not a scan-stopper (settled against `dynamodb-local`; see §4.3(2)) |
| other | false |

**`begins_with(path, operand)`** is a prefix test for S/S and B/B; every other combination is false.

**`attribute_type(path, operand)`** requires the operand to be a `:v` substitution of type S carrying one of the ten type codes (`S`, `N`, `B`, `BOOL`, `NULL`, `L`, `M`, `SS`, `NS`, `BS`). A non-S operand, a literal path operand, or an unrecognized code is a `ValidationException`. Otherwise it is a direct tag check against `Value.Type()`, and false for a missing path.

### 4.3 Conformance-determined behaviors

Two behaviors were not settled from documentation and were resolved by running the case against `dynamodb-local` during implementation. The observed results are encoded both in the conformance tests and in this section:

1. **`size()` applied to an `N` attribute.** `dynamodb-local` returns `ConditionalCheckFailedException` for `size(n) = :n` regardless of the digit count supplied (`5` and `6` for `n = 12345` both fail), i.e. `size(N)` is undefined and the comparison is simply **false** — not a digit count and not a `ValidationException`. The engine's `sizeOf` returning `ok = false` for `TagNumber` is therefore correct, and **parent spec §5.1 was amended** (it had asserted "Number → digit count per AWS spec") in the same commit. The probe lives in `TestConfSize/size of a number`.
2. **`contains(path, :v)` on an `L` whose elements are of mixed types.** `dynamodb-local` matches `contains(l, :seven)` on `[S "x", N 7]` with `:seven = N 7` (the write succeeds), so a type-mismatched element is **skipped** — the scan continues. The engine's `evalContains` `TagList` branch (element-wise `Equal`) is correct; the probe lives in `TestConfConditionSemantics/contains on a mixed-type list`.

Both are single conformance cases whose assertions were filled in from the reference run.

### 4.4 Filter-only rule

A `FilterExpression` may not reference the key attributes of the table (or, for an index query, of the index) → `ValidationException`. The rule is implemented and unit-tested in M2 against the parsed AST's `Refs`, but has no caller until M3 wires `Query`/`Scan`. No `ddb`-level filter API is exposed in M2.

### 4.5 Substitution binding and validation

```go
type Env struct {
    Names  map[string]string
    Values map[string]attrval.Value
}
```

`Bind` resolves every `#name` against `Env.Names` and every `:value` against `Env.Values`. An unresolved reference is a `ValidationException`.

**The unused check cannot live inside `Bind`.** DynamoDB validates unused `ExpressionAttributeNames`/`Values` entries across *all* expressions in a request jointly: a `#n` referenced only by the `UpdateExpression` must not be reported unused merely because the `ConditionExpression` does not mention it. Therefore:

- each parsed expression exposes `Refs() (names, values []string)`;
- `ddb` unions the refs from every expression present on the request and calls `expr.CheckUnused(env, names, values)` exactly once;
- an entry in either map that no expression references is a `ValidationException`.

Undefined references remain a per-expression check inside `Bind`.

### 4.6 Empty vs absent expression strings

`ddb`'s input structs carry expressions as plain `string`, so `""` means *absent*. The SDK carries them as `*string`, so a caller can pass a present-but-empty expression, which real DynamoDB rejects. **The adapter** performs that check: a non-nil pointer to an empty string yields `ValidationException` before the call reaches `ddb`. This keeps the engine's input structs free of pointer fields.

## 5. Update expressions

### 5.1 Grammar

```
update  := clause { clause }
clause  := 'SET'    setAction    { ',' setAction }
         | 'REMOVE' path         { ',' path }
         | 'ADD'    path operand { ',' path operand }
         | 'DELETE' path operand { ',' path operand }

setAction := path '=' setValue
setValue  := operand
           | operand '+' operand
           | operand '-' operand
           | 'if_not_exists' '(' path ',' operand ')'
           | 'list_append' '(' operand ',' operand ')'
operand   := path | ':' name
```

Clauses may appear in any order; each keyword may appear **at most once** (a second `SET` clause is a `ValidationException`). Keywords and function names are case-insensitive.

### 5.2 Semantics

**All actions read from the original item.** Every operand resolves against the pre-update item state, not against the partially-updated result, so `SET a = b, b = a` swaps the two attributes. `Apply` builds the new item from the original and applies all actions against that original snapshot.

**Overlapping paths are rejected.** Two actions in one expression whose paths overlap (`SET a.b = :x, a = :y`; `SET a = :x REMOVE a`) → `ValidationException`. Overlap is computed on the bound paths: one path is a prefix of the other, or they are equal.

**Key attributes are immutable.** Any action whose path targets the table's partition or sort key attribute → `ValidationException`. This is checked in `ddb`, which is where the `TableDef` lives.

**`SET`:**

- `path = :v` or `path = otherPath` — creates or overwrites, via `attrval.SetPath` (§3.2 rules for intermediate segments and list indices apply).
- `a + b` / `a - b` — both operands must resolve to N, else `ValidationException`. A missing operand is a `ValidationException`, not a zero.
- `if_not_exists(path, operand)` — the operand's value if `path` is missing, the existing value otherwise. A present `NULL` counts as existing.
- `list_append(a, b)` — both operands must resolve to L, else `ValidationException`; produces the concatenation.

**`REMOVE`:** deletes the attribute or element at the path. On a list index, the element is removed and subsequent elements **shift** down. A missing path is a silent no-op. Containers left empty are not pruned.

**`ADD` and `DELETE` are top-level only.** A nested path (`ADD a.b :v`) is a `ValidationException` — DynamoDB restricts both actions to top-level attributes.

- **`ADD path :v`** — if `:v` is N: a missing attribute is treated as `0` and the value added; an existing non-N attribute is a `ValidationException`. If `:v` is a set (SS/NS/BS): a missing attribute is created as that set; an existing set of the **same** set type is unioned; an existing attribute of any other type, or a set of a different set type, is a `ValidationException`. `:v` of any other type is a `ValidationException`.
- **`DELETE path :v`** — `:v` must be a set matching the attribute's set type, else `ValidationException`. Removes the listed elements. **If the result is empty, the attribute is removed entirely**, because DynamoDB has no empty-set representation. A missing attribute is a no-op.

**Upsert.** `UpdateItem` against an absent key creates the item: the key attributes from `Key`, plus the update's effects applied to that key-only item. A call with no `UpdateExpression` at all creates a key-only item. (`ADD :n` on an absent item therefore yields the addend; `REMOVE` yields just the key.)

### 5.3 Touched paths and `ReturnValues`

`Apply` returns the new item plus, for each touched path, the top-level attribute name, which action kind touched it, and whether the path existed beforehand. A flat set of touched attribute names is **not** sufficient — the two `UPDATED_*` modes filter that set differently, and neither filter can be recovered from the names alone.

| `ReturnValues` | Result |
|---|---|
| `NONE` (default) | no `Attributes` |
| `ALL_OLD` | the complete item as it was before the update; absent if it did not exist |
| `ALL_NEW` | the complete item after the update |
| `UPDATED_OLD` | attributes whose touched **path existed before** the update, at their pre-update value |
| `UPDATED_NEW` | attributes touched by a **value-producing action** (`SET`/`ADD`/`DELETE`) that **still exist after** the update, at their post-update value |

Both modes project the **whole top-level attribute**, not the sub-document at the touched path: `REMOVE m.a` reports all of `m` under `UPDATED_OLD`, not just `m.a`.

The two filters are independent, and each has a case the other does not:

- **`REMOVE` never contributes to `UPDATED_NEW`,** even when the top-level attribute survives. `REMOVE m.a` on `m = {a,b}` returns *no* attributes, though `m` still exists — a flat touched-set implementation gets this wrong by reporting the surviving `m`. `SET m.c = :v REMOVE m.a` does report `m`, but on the strength of the `SET` alone.
- **`DELETE` contributes only if the attribute survives.** `DELETE ss :one` reports `ss`; a `DELETE` that removes the set's last element removes the attribute, and reports nothing.
- **`SET` on a previously absent path contributes nothing to `UPDATED_OLD`.** `SET m.c = :v` where `m` exists but `m.c` does not returns no attributes, because the *path* is new even though the attribute is not.

These rules were measured against `dynamodb-local` 3.3.1; the cases are pinned in `awsdynamodb/conformance_divergence_test.go`.

`PutItem` and `DeleteItem` accept `NONE` and `ALL_OLD` only; any other value is a `ValidationException` (matching DynamoDB, which restricts those two ops to that pair). The comparison is **case-sensitive**: `all_old` is a `ValidationException`, not a synonym for `ALL_OLD`.

## 6. Engine and adapter integration

### 6.1 `ddb` operation surface

`PutItemInput` and `DeleteItemInput` gain:

```go
ConditionExpression       string
ExpressionAttributeNames  map[string]string
ExpressionAttributeValues map[string]attrval.Value
ReturnValues              string   // "" == NONE
ReturnValuesOnConditionCheckFailure string
```

`ReturnValuesOnConditionCheckFailure` appears on all three conditional operations rather than on `UpdateItem` alone: the mechanism is a single field on `ConditionalCheckFailedError` (§6.3), and Put/Delete already read the pre-write item to evaluate their condition, so restricting it to `UpdateItem` would be an arbitrary gap rather than a saving.

Both operations change signature to return an output struct:

```go
func (c *Client) PutItem(ctx context.Context, in PutItemInput) (PutItemOutput, error)
func (c *Client) DeleteItem(ctx context.Context, in DeleteItemInput) (DeleteItemOutput, error)
```

where each output carries `Attributes Item`. This is a breaking change to `ddb` and its adapter call sites, taken now rather than deferred — the package is pre-release and the M1 signatures have no external consumers.

`UpdateItem` is new:

```go
type UpdateItemInput struct {
    TableName                 string
    Key                       Item
    UpdateExpression          string
    ConditionExpression       string
    ExpressionAttributeNames  map[string]string
    ExpressionAttributeValues map[string]attrval.Value
    ReturnValues              string
    ReturnValuesOnConditionCheckFailure string
}

type UpdateItemOutput struct{ Attributes Item }

func (c *Client) UpdateItem(ctx context.Context, in UpdateItemInput) (UpdateItemOutput, error)
```

### 6.2 Read-modify-write inside one transaction

Every conditional or updating operation becomes a single read-modify-write on one `*sql.Tx`:

1. `store.BeginTx`
2. `store.GetTableDef` → `ErrTableNotFound` if absent
3. validate `Key` / `Item` against the key schema (existing `validateKey`)
4. parse each expression present; union `Refs` and run `CheckUnused` once; `Bind` each
5. `store.GetItem` for the existing item (needed by the condition, by `ALL_OLD`, and by `UpdateItem`'s read-modify-write)
6. evaluate the condition → `ConditionalCheckFailedError` on false
7. for `UpdateItem`, `Apply` the update; re-validate that key attributes are unchanged and that the result is under the 400KB proxy limit
8. `store.PutItem` / `store.DeleteItem`
9. `Commit`

Atomicity needs no new machinery: `MaxOpenConns(1)` means the transaction holds the single connection for its whole duration, so the read and the write cannot interleave with another operation. Every statement issues through the `*sql.Tx`, never the parent `*sql.DB` — the existing rule from the parent spec §7.

Parse/bind happens **before** the row read so that a malformed expression fails with `ValidationException` regardless of whether the item exists, matching DynamoDB's validate-then-execute ordering.

### 6.3 New error type

```go
type ConditionalCheckFailedError struct{ Item Item }

func (e *ConditionalCheckFailedError) Error() string { ... }
func (e *ConditionalCheckFailedError) Is(target error) bool  // matches ErrConditionalCheck
```

`Is` reporting true for the existing `ErrConditionalCheck` sentinel keeps every current `errors.Is` call site working, including the adapter's `mapError`, while letting the adapter recover the pre-write item via `errors.As` for `ReturnValuesOnConditionCheckFailure`. `Item` is populated only when the request asked for it; otherwise it is nil.

All expression failures — syntax, undefined/unused substitutions, semantic violations — map to the existing `ErrValidation`, which the adapter already renders as `ValidationException`. No new sentinel is needed for them.

### 6.4 Adapter changes

- `PutItem`, `DeleteItem`: pass `ConditionExpression`, `ExpressionAttributeNames`, and `FromSDKMap`-converted `ExpressionAttributeValues` through; translate `types.ReturnValue`; return `Attributes` via `ToSDKMap`.
- `UpdateItem`: new method with the exact SDK signature.
- **Legacy parameters are rejected, not ignored:** `Expected` and `ConditionalOperator` (Put/Delete/Update) and `AttributeUpdates` (Update) yield `ValidationException` when non-empty. Silently ignoring them would let a test appear to pass a conditional write that was never evaluated.
- **Present-but-empty expression strings** yield `ValidationException` (§4.6).
- `mapError` gains one branch: `errors.As` to `*ddb.ConditionalCheckFailedError` produces a `types.ConditionalCheckFailedException` with `Item` set from the error when present.

The adapter still names no `expr` type — it passes strings and maps.

## 7. Testing strategy & verification

### 7.1 Layer 1 — `internal/expr` unit tests

Table-driven, in the established style (inline `cases := []struct{...}`, `t.Run`, stdlib `testing` only):

- **Lexer:** token sequences for each grammar construct; unterminated `#`/`:` tokens; case-insensitive keywords.
- **Parser:** precedence trees (`a = :x OR b = :y AND c = :z` binds as `OR(a, AND(b, c))`); parenthesization; each function; each update clause; duplicate-clause rejection; malformed input producing `ErrSyntax` with the offending token named.
- **Bind:** undefined `#n`/`:v`; `Refs` completeness; `CheckUnused` across a union of two expressions.
- **Condition eval:** every comparator and function against a fixture item exercising all ten tags; the missing-vs-`NULL` matrix; type-mismatch→false matrix; `BETWEEN` reversed bounds → error.
- **Update apply:** each action, `if_not_exists`, `list_append`, arithmetic, nested `SET`, list-index `REMOVE` shift, `DELETE` emptying a set, overlapping-path rejection, original-item read semantics (the swap case).

### 7.2 Layer 2 — `attrval` and `ddb` unit tests

- `attrval`: `SetPath`/`RemovePath` — final-segment creation, rejection of a missing parent segment, out-of-range list index clamping to an append, wrong-container-type rejection, remove-and-shift, no-prune-on-empty, and non-mutation of the receiver.
- `ddb`: conditional Put/Delete both outcomes; `ALL_OLD` on both; `UpdateItem` upsert; key-attribute immutability; `ReturnValues` projection for all five modes; expression errors surfacing as `ErrValidation`.

### 7.3 Layer 3 — conformance cases (dual-target)

Added to the existing `awsdynamodb/conformance_test.go` harness, driven through the SDK interface so both targets run them:

1. Conditional put — `attribute_not_exists(pk)` succeeds on insert, fails on overwrite → `ConditionalCheckFailedException`.
2. Conditional delete with `#n = :v`, both outcomes.
3. `ReturnValues=ALL_OLD` on Put and Delete; the absent-item case returns no `Attributes`.
4. Every comparator and function against an item exercising all ten types, including missing-vs-`NULL` and type-mismatch→false.
5. `BETWEEN` with reversed bounds → `ValidationException`.
6. `size()` on S / B / L / M / SS, plus the `N` probe that settles §4.3(1).
7. Undefined `#n` and `:v`; unused entries in each map; the cross-expression union case from §4.5.
8. `UpdateItem` upsert on an absent key; key-only creation with no `UpdateExpression`.
9. Each of SET / REMOVE / ADD / DELETE, including `if_not_exists`, `list_append`, arithmetic, nested-path SET, list-index REMOVE shift, and `DELETE` emptying a set → attribute removed.
10. `ADD` on a nested path, and any action targeting a key attribute → `ValidationException`.
11. All five `ReturnValues` modes on `UpdateItem`.
12. `ReturnValuesOnConditionCheckFailure=ALL_OLD` populates the exception's `Item`, on `UpdateItem` and on a conditional `PutItem`.

### 7.4 Fuzzing

- `FuzzParseCondition` / `FuzzParseUpdate` — arbitrary input must return an error or an AST, never panic.
- `FuzzBindEval` — arbitrary expression source plus a generated item; parse, bind against a fixed env, evaluate; must never panic. Seeded from the unit-test corpus.

### 7.5 Verification gate

M2 is complete when all of the following pass, with the cache buster on every `go test` so a cached result cannot stand in for an actual run — this matters most for the `dynamodb-local` target, whose outcome depends on container state that Go's content-based cache cannot see:

```bash
go test -count=1 ./...                                        # root module
cd awsdynamodb && go test -count=1 ./...                      # adapter module, adapter target
DDBSQLITE_CONF_TARGET=all go test -count=1 ./awsdynamodb/...  # both conformance targets
go vet ./...                                                  # run in both module directories
```

**Every M2 conformance case must be green against both the adapter and `dynamodb-local`.** Expression semantics is precisely where a faithful mock silently diverges, and the reference is available as of M1 pass 2, so divergence is a blocker rather than a logged follow-up.

## 8. Implementation sequencing (two passes)

Following M1's precedent of designing in full and implementing in passes.

**Pass 1 — condition/filter.**

- `attrval.SetPath` / `RemovePath` (needed by pass 2, but self-contained and cheap to land early).
- `internal/expr`: `lex.go`, `ast.go`, `parse.go` (condition grammar), `bind.go`, `eval.go`, `errors.go`.
- The filter-only key-attribute rule (§4.4), unit-tested, unwired.
- `ddb`: `ConditionExpression` + `ReturnValues=ALL_OLD` + `ReturnValuesOnConditionCheckFailure` on `PutItem`/`DeleteItem`; `ConditionalCheckFailedError`; signature changes.
- `awsdynamodb`: expression pass-through, legacy rejection, empty-string rejection, `mapError` branch.
- Conformance cases 1–7. `FuzzParseCondition`, `FuzzBindEval`.

Pass 1 implements `ReturnValuesOnConditionCheckFailure` for Put/Delete; conformance case 12 exercises it for both those ops and `UpdateItem` once pass 2 lands.

**Pass 2 — update.**

- `internal/expr`: update grammar in `parse.go`, `update.go`.
- `ddb`: `UpdateItem`, full `ReturnValues`, key-immutability check, upsert.
- `awsdynamodb`: `UpdateItem`, `ReturnValuesOnConditionCheckFailure`.
- Conformance cases 8–12. `FuzzParseUpdate`.

Pass 1 is independently shippable and green under the §7.5 gate for cases 1–7.

## 9. Decisions, risks & out of scope

### 9.1 Decisions captured

| Decision | Choice |
|---|---|
| Milestone shape | One spec, two implementation passes |
| Parser strategy | Hand-written lexer + recursive descent + typed AST |
| Substitution timing | Parse → Bind → Eval, three phases |
| Filter evaluator in M2 | Built and unit-tested; not wired until M3 |
| Expression limits enforced | Undefined **and** unused `#name`/`:value` only |
| Length / `IN`-count / nesting-depth caps | Deferred to M6 |
| Path writes | Public `attrval.SetPath` / `RemovePath` |
| `UpdateItem` upsert on absent key | In scope |
| Legacy SDK params | Rejected with `ValidationException` |
| `ReturnValuesOnConditionCheckFailure` | In scope |
| Key attributes immutable | In scope |
| `ReturnValues=ALL_OLD` on Put/Delete | In scope |
| Verification gate | Dual-target green required, `-count=1` |

### 9.2 Risks & mitigations

1. **`dynamodb-local` diverges from documentation on undocumented edges.** Two are already identified (§4.3); more may surface. *Mitigation:* the dual-target gate turns each into a decision point during implementation rather than a latent bug. Where `dynamodb-local` contradicts this spec, the reference wins and the spec is amended in the same commit — including parent spec §5.1 if the `size()`-on-N probe requires it.
2. **The cross-expression unused check is easy to implement per-expression by mistake**, which would reject a valid request that splits its substitutions across `ConditionExpression` and `UpdateExpression`. *Mitigation:* the check lives in `ddb` (not `Bind`) by construction, and conformance case 7 covers the union case specifically.
3. **Breaking `PutItem`/`DeleteItem` signatures** ripples through the adapter and every existing test. *Mitigation:* mechanical, compiler-caught, and done in pass 1 while the call-site count is small.
4. **`SetPath` is the subtlest new `attrval` code** — missing-parent rejection, out-of-range index clamping, and shifting on remove. *Mitigation:* dedicated unit tests (§7.2) plus conformance case 9's list-index REMOVE. **This risk materialized:** the original §3.2 asserted that `SetPath` creates intermediate maps and rejects an out-of-range list index. Both were wrong about DynamoDB, the unit tests pinned the wrong behavior, and no conformance case sent either shape to the reference. §3.2 above is the corrected rule; the divergences are captured as RED cases in `awsdynamodb/conformance_divergence_test.go`. The lesson for M3+: a semantic claim that has never been run against `dynamodb-local` is a hypothesis, not a spec.
5. **Update actions reading from a partially-updated item** would silently break the swap case and any expression referencing an attribute it also writes. *Mitigation:* `Apply` snapshots the original explicitly; a unit test asserts the swap.

### 9.3 Explicitly out of scope for M2

- `Query` / `Scan` and the wiring of `FilterExpression` (M3); GSI (M4); `BatchWriteItem` / `BatchGetItem` / TTL (M5); `UpdateTable` (M6).
- Expression length (4KB), `IN` operand count (100), and document-path nesting-depth caps — M6 hardening, per §9.1.
- Full item-size accounting; M2 continues to use M1's JSON byte-length proxy.
- `ProjectionExpression`, `TransactWriteItems`, PartiQL — v1 non-goals.
- `KeyConditionExpression` — it shares this grammar but belongs to `Query`, so it arrives in M3.
