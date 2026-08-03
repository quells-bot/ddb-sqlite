# ddb-sqlite M6c — Hardening Sweep (Core & Adapter) Design

**Date:** 2026-08-03
**Status:** Approved (brainstorming); probe battery complete — findings incorporated (§2.8) → pending implementation plan
**Parent spec:** `docs/superpowers/specs/2026-07-31-ddb-sqlite-design.md` (§5, §6, §10.4, §11 M6)
**Scope:** This spec is **M6c**, the final M6 installment — the hardening sweep. M6a shipped `UpdateTable`; M6B shipped projection expressions. M6c closes every remaining "deferred to M6" item from the M1–M6B specs plus the original-M6 golden-corpus and fuzz line items.

## 1. Overview & approach

M6c is sequenced **probe battery → enforcement → audit/fuzz** (Approach A, chosen over per-workstream probes and audit-first). One throwaway probe file settles every open behavioral question against dynamodb-local 3.3.1 up front; findings are recorded in this spec as a results table; implementation then proceeds workstream-by-workstream with zero open questions. This is the established project methodology (M2 §probes, M3 §5, M4 §2, M6a §8).

Nine workstreams:

| # | Workstream | Type | Decided policy |
|---|---|---|---|
| W1 | Item-size accounting (replaces M1 JSON proxy) | enforcement | AWS-faithful, probe-verified constants |
| W2 | Expression limits (4KB string, 300 operators, IN≤100, 255B tokens, 2MB substitutions, 32-level depth) | enforcement | probe-verified boundaries |
| W3 | Naming & key validation (table names, key-value lengths, 255B index names, cross-index projection sum, empty-name guard on all ops) | enforcement | probe-verified rules |
| W4 | Nested `INCLUDE` NonKeyAttributes | resolved (P-include): matches reference — no change | — |
| W5 | `DescribeTable` ItemCount/size reporting | implement (P-desc): real immediate counts, W1-sum sizes | — |
| W6 | BatchGetItem 16MB response cap | implement (P-batch): 16MiB cap, W1-accounting basis (pre-projection, whole-response), `UnprocessedKeys` spill | — |
| W7 | Legacy/deprecated field rejection audit | audit + fix gaps | reject all legacy fields (policy) |
| W8 | Golden-corpus edge-case audit | audit → triage | fix small/isolated, document structural |
| W9 | Fuzz pass beyond existing targets | coverage | — |

**Finding policies (settled in brainstorming):**

1. *Probe-then-match* for every behavioral question. dynamodb-local 3.3.1 is the reference; AWS docs supply the defaults when a probe is inconclusive.
2. *Audit mismatches are triaged*: small/isolated fixes land in M6c via the revived `conformance_divergence_test.go` RED-case flow (encode RED → fix → migrate green cases into `conformance_test.go`); structural work is documented in the §11 divergence table and deferred.
3. *All legacy/deprecated API fields are rejected* with `ValidationException`. Honoring them (as the reference does for e.g. `KeyConditions`) remains a documented deliberate divergence.
4. *Item-size accounting is AWS-faithful*, with constants verified by probe rather than guessed — P-size pinned the exact number-size function the AWS docs give only as "approximately" (§3.1).

### 1.1 Corrections & refinements to prior specs

- **Parent spec §5 ("Faithful limits enforced")** and **M2 §9** (expression caps deferred to M6): resolved by W2.
- **M1 §8 / parent spec** (400KB item-size via JSON byte-length proxy, full accounting deferred to M6): resolved by W1. The proxy and `maxItemBytes` are deleted.
- **M3 §9.2 risk 6** (`ListTables` may share the dropped `limit+1` divergence): verified by W8 (P-misc probe + conformance case).
- **M4 §11** (table-name charset/length validation deferred to M6; nested `INCLUDE` gap): resolved by W3 and W4 respectively.
- **M5b §8** (16MB limits not enforced): BatchWriteItem's 16MB is structurally unreachable (25 × 400KB = 10MB; M5b's analysis stands, no work). BatchGetItem's response-side 16MB is reachable (100 × 400KB = 40MB) → W6.
- **M6a §9** (ItemCount/IndexSizeBytes, error precedence, empty-table-name on other ops, same-name GSI): resolved by W5, W8, W3, W8 respectively.
- **M6B §8** (nested `INCLUDE` deferral): resolved by W4.

### 1.2 Newly surfaced documented limits (AWS Constraints page)

Research for this spec (AWS *Constraints in Amazon DynamoDB* and *DynamoDB item sizes and formats* docs) surfaced limits not named in any prior spec; they join the probe battery and, where confirmed, the enforcement workstreams:

- Partition-key values: 1–2048 bytes; sort-key values: 1–1024 bytes (W3).
- Secondary-index key attribute names and `INCLUDE` `NonKeyAttributes` names ≤255 bytes (W3).
- Total user-specified projected attributes across all of a table's secondary indexes ≤100 (W3).
- Operators/functions per expression ≤300, with a documented error-message quirk (reported count is always 301) (W2).
- Single expression attribute *name* token (`#name` key in `ExpressionAttributeNames`) ≤255 bytes — P-expr confirmed the limit applies to the token key string only, NOT to the actual attribute name it maps to; `:value` tokens have no 255B limit (individual value cap ~1MB serialized) (W2).
- Substitution values summed: dynamodb-local enforces ~1MB *serialized* `ExpressionAttributeValues`, not the AWS-documented 2MB (P-expr; docs-vs-local discrepancy recorded in §11) (W2).
- Nested attribute depth ≤32 levels — an item-structure constraint as well as a path constraint (W1 walk + W2).

## 2. Probe battery

One throwaway `awsdynamodb/probe_m6c_test.go` (deleted after findings are ported, per M4/M6a precedent), run against the live dynamodb-local 3.3.1 container via `DDBSQLITE_CONF_TARGET=dynamodb-local`. Every probe has a documented default (AWS docs or current engine behavior) so the battery degrades gracefully if the container is unreachable — same structure as M6a §8.

**Battery complete (2026-08-03).** The full findings and per-workstream rationale live in `2026-08-03-m6c-probe-findings.md`; the results are ported into §2.8 below and §3–§6/§8/§11 are amended accordingly. The probe file is deleted.

### 2.1 P-size — item-size accounting

Binary-search the 400KB acceptance boundary with crafted items to isolate each term of the formula:

- **Baseline:** single S attribute, vary value length → confirms name bytes + value bytes, and reveals any fixed per-item overhead inside the 400KB (AWS documents a 100-byte *storage billing* overhead; whether it counts toward the 400KB write rejection is the key question).
- **B:** raw byte length (not base64) — confirm.
- **N:** vary significant digits (1, 2, 3, 38 digits; leading/trailing zeros trimmed) → fit the actual number-size function against the documented "1 byte per two significant digits + 1, approximately".
- **L/M:** empty containers (3-byte overhead?), nested containers, per-element 1-byte overhead → confirm recursively.
- **SS/NS/BS:** no container overhead, element sums → confirm.
- **BOOL/NULL:** 1 byte → confirm.
- **Error precedence:** an oversize item combined with a failing `ConditionExpression` — `ValidationException` or `ConditionalCheckFailedException`? The engine sizes before evaluating the condition today (`ddb/items.go`); the probe pins whether that order matches.

Default: the AWS formula as documented (§3.1), 100-byte storage overhead *not* counted toward 400KB.

### 2.2 P-expr — expression limits

- 4KB expression-string boundary (4096 vs 4097 bytes) per expression kind: Condition, Filter, Update, Projection, KeyCondition.
- Operator count: 300 vs 301 comparators/keywords/functions; whether the error message reports exactly 301 (AWS-documented quirk); whether `=` in `SET` is excluded and `+`/`-` counted.
- `IN` operand count: 100 vs 101.
- `IN` × operator-count interaction: does `IN (:a, :b, …)` count as 1 operator or 1 + operands toward the 300 cap? Determines whether IN-heavy expressions can ever trip the limit at all.
- Substitution tokens: single `#name` value >255 bytes; single `:value` payload >255 bytes (measurement basis: token string bytes vs serialized value); 2MB substitution sum (measurement basis: names only, values only, or both; how values are measured).
- Path nesting: 32 vs 33 levels in a document path; whether item-structure depth >32 is rejected at write time independently of expressions.

Defaults: AWS-documented values (§1.2), local enforcing all of them.

### 2.3 P-names — naming & key validation

- Table names: 2-char and 256-char names; names containing `!`, ` `, `/`, non-ASCII. (M4 probe G27 confirmed 3–255 `[A-Za-z0-9_.-]` for *GSI* names; this extends the probe to table names.)
- Empty/missing `TableName` on every operation. Current state is patchwork: `CreateTable` (`analyzeCreateTable`) and both batch ops (per-table, at the request-map key loop) already reject `""` engine-side; `UpdateTable`'s guard is adapter-side only — the engine op returns `ErrTableNotFound` for `""`, as do `PutItem`/`GetItem`/`DeleteItem`/`UpdateItem`/`Query`/`Scan`/`DescribeTable`/`DeleteTable`/TTL ops, where the reference returns `ValidationException`. This is a live divergence W3 rule 2 closes.
- Partition-key value 2048 vs 2049 bytes (S and B); sort-key 1024 vs 1025 bytes. N keys need no boundary probe: the 38-significant-digit limit bounds any number to ~21 bytes by the W1 formula and to well under 200 bytes even as a maximally padded wire string — always under 1024. A single confirmation suffices.
- Empty primary-key values (0-byte S, 0-byte B): the engine accepts them today — only GSI key values reject empty S/B (`validateOneGsiKey`/`gsiKeyValue`); `validatePutKey`/`validateKey` check presence and type only. Confirm the reference rejects, and with which error, so W3's min-1-byte enforcement pins the right behavior.
- GSI key attribute name and `INCLUDE` name 255 vs 256 bytes.
- Cross-index projected-attribute sum: two GSIs whose `NonKeyAttributes` sum to 101. Plus a counting-rule probe the sum probe cannot answer alone: one GSI with 99 `INCLUDE` attrs — if table/index key attributes count toward the 100 this is 101+ and must reject; if only user-specified `NonKeyAttributes` count it accepts.

Defaults: AWS-documented rules, enforced as `ValidationException`.

### 2.4 P-include — nested INCLUDE

`CreateTable` with `NonKeyAttributes: ["obj.a"]`: rejected, accepted-but-never-projected, or accepted-and-projected? If projected, verify GSI query returns the nested spine.

Default: current engine behavior (accepted, never projected; M6B §4.7 made the restriction check consistent with that).

### 2.5 P-desc — DescribeTable reporting

After known writes: `ItemCount`, `TableSizeBytes`; per-GSI `ItemCount`, `IndexSizeBytes`. Exact values or 0? Eventually-consistent approximations or immediate? If nonzero: is the reported size a function the engine can reproduce — the W1 item-accounting sum, wire-blob bytes (`SUM(LENGTH(data))`), or neither? Probe with items of known accounting size (per W1's formula) to fit the relationship before W5 commits to exact-match assertions.

Default: current engine behavior (no table-level count/size fields exist; the adapter emits per-GSI zeros only — M6a §4 left unchanged).

### 2.6 P-batch — BatchGetItem 16MB

Request 100 keys resolving to items totaling >16MB (e.g. 100 × 400KB-max items): truncated response with `UnprocessedKeys` overflow, or full response? Size measurement basis if enforced: item accounting (W1) vs wire bytes, and pre- or post-projection — does a `ProjectionExpression` shrinking the response change which keys spill? Note the stored `data` blob *is* wire JSON, so pre-projection wire-byte measurement is `LENGTH(data)`, nearly free; item accounting requires decoding each item and makes W6 depend on W1 landing first.

Default: current engine behavior (no enforcement; `UnprocessedKeys` always empty — M5b documented divergence).

### 2.7 P-misc

- `ListTables` with `Limit == table count`: is `LastEvaluatedTableName` set (M3 §9.2 risk 6)?
- `UpdateTable` error precedence: GSI `Create` with an existing index name *and* an invalid key schema in one call — which error surfaces?
- GSI named identically to its table: accepted or rejected at `CreateTable`?

### 2.8 Probe results

Run against dynamodb-local 3.3.1 on 2026-08-03 (`awsdynamodb/probe_m6c_test.go`, since deleted). Full detail: `2026-08-03-m6c-probe-findings.md`.

**P-size — item-size accounting:**

| Probe | Default | Observed | Action |
|---|---|---|---|
| 100-byte storage overhead toward 400KB? | not counted | Not counted. Baseline `{pk:"k", big:S(n)}`: max n=409594 → item = 6+n = exactly 409600 accepted | §3.1: no per-item overhead; limit is exactly 409600 bytes |
| S value bytes | name + UTF-8 bytes | delta=2 for 1-byte S ✓ | no change |
| B value bytes | name + raw bytes | delta=2 for 1-byte B ✓ (not base64) | no change |
| N size function | ~1 byte/2 sig digits + 1 | **Exact: `N_size = ceil(sig_digits/2) + 1`**; `"0"` has 0 sig digits → size 1. Verified across 14 inputs (1–38 sig digits, leading/trailing-zero trimming) | §3.1: remove "approximately"; state exact formula |
| BOOL/NULL | name + 1 | delta=2 for both ✓ | no change |
| L/M overhead | name + 3 + elements (each +1) | L_empty=4, M_empty=4, L1=6, L2=8, M1=7, M2=10, L_nested=10 — all match ✓ | no change |
| SS/NS/BS | name + elements (no container overhead) | SS1=2, SS2=3, NS1=3, NS2=5, BS1=2 ✓ | no change |
| Error precedence: oversize + failing condition | size first | `ValidationException` (size) beats `ConditionalCheckFailedException` | §3.2: engine's existing order is correct — probe-verified |

**P-expr — expression limits:**

| Probe | Default | Observed | Action |
|---|---|---|---|
| 4KB expression string | 4096 accept, 4097 reject | 4096 accepted, 4097 → "Expression size has exceeded" ✓ | no change |
| Operator count ≤300 | 301 reject | 299 accepted, 301 → "operator count: 301" ✓ (reported-count quirk confirmed) | no change |
| IN ≤100 operands | 100 accept, 101 reject | 100 accepted, 101 → "number of operands: 101" ✓ | no change |
| IN × operator-count interaction | open | **IN counts as 1 operator** regardless of operand count (3×IN(100) + 2 AND = 5 ops, accepted) | §4 #2: IN-heavy expressions cannot trip the 300 cap |
| `#name` token 255B | 255 accept, 256 reject | Token key string: 255 accepted, 256 → "key too long; size of key: 256". **Actual attribute name (what `#n` maps to): 256 accepted — no limit** | §4 #4: 255B applies to `ExpressionAttributeNames` keys only |
| `:value` token 255B | 255 accept, 256 reject | **No 255B limit.** 256B and 4087B accepted; individual `:value` cap binary-searched to **1,048,567 bytes (~1MB)**; 1,048,568 → "Expression size has exceeded" | §4 #4: remove 255B claim for values |
| 2MB substitution sum | 2MB | **Local enforces ~1MB *serialized* `ExpressionAttributeValues`** (same "Expression size has exceeded" error as the 4KB limit). The 2MB raw "exceeds max size" error exists but is unreachable for string values | §4 #4: enforce ~1MB serialized; record docs-vs-local discrepancy (§11) |
| Path nesting 32 vs 33 | 32 accept, 33 reject | 32 accepted, 33 → "nesting levels: 33" ✓ | no change |
| Item-structure depth >32 | rejected independently | 32 accepted, 33 → "Nesting Levels have exceeded supported limits" ✓ (independent of expressions) | no change |

**P-names — naming & key validation:**

| Probe | Default | Observed | Action |
|---|---|---|---|
| Table name charset/length | 3–255 `[A-Za-z0-9_.-]` | 2-char and 256-char rejected; space/`!`/`/`/non-ASCII rejected; `.`/`_` accepted; 255 accepted ✓ | §5 #1: confirmed — same rule as GSI names; fixture ripple is real |
| Empty `TableName` on all ops | `ValidationException` | All 12 operations return `ValidationException` for `""`. Nil `TableName` is caught SDK-side | §5 #2: confirmed — engine validates empty string on every op |
| pk S/B 2048 boundary | ≤2048 | 2048 accepted, 2049 rejected ✓ — **inclusive** (error text says "under 2048", misleading) | §5 #3: enforce ≤2048; message quirk recorded (§11) |
| sk S/B 1024 boundary | ≤1024 | 1024 accepted, 1025 rejected ✓ — inclusive | §5 #3: enforce ≤1024 |
| Empty primary key values | rejected | Empty S → "empty string value"; empty B → "AttributeValue is empty" — **different messages per type** | §5 #3: confirmed |
| GSI key attr name 255B | ≤255 | 255 accepted, 256 → "must be between 1 and 255 characters, inclusive" ✓ | §5 #4: confirmed |
| INCLUDE `NonKeyAttributes` name 255B | ≤255 | 255 accepted, 256 → same error ✓ | §5 #4: confirmed |
| Cross-index sum ≤100 | ≤100 | Single GSI 100 accepted / 101 rejected; two GSIs summing to 101 rejected; 99 + key attrs accepted — **key attrs do NOT count**. Error message is misleading ("No more than 20 attributes … Local Secondary Indices") | §5 #5: confirmed; engine uses a correct message (§11) |

**P-include / P-desc / P-batch / P-misc:**

| Probe | Default | Observed | Action |
|---|---|---|---|
| `NonKeyAttributes: ["obj.a"]` | accepted, never projected | Accepted at `CreateTable` and `PutItem`; GSI query returns only `[gk, pk, sk]` — `obj.a` NOT projected | §6.1: current behavior **matches reference**; no code change |
| DescribeTable ItemCount/size | 0 / zeros | **Real, immediate values.** Empty: 0/0. After 3 puts: ItemCount=3, TableSizeBytes=3033; per-GSI ItemCount=3, IndexSizeBytes=3033. Not eventually consistent | §6.2: **implement** |
| Size = W1 accounting or wire bytes? | open | **W1 item accounting** (3 × 1011 = 3033; wire JSON ≈ 3135 ≠ 3033) | §6.2: pin TableSizeBytes = W1 sum |
| IndexSizeBytes per projection type | open | **Projection-independent.** KEYS_ONLY, INCLUDE, and ALL GSIs all report ItemCount=3, IndexSizeBytes=3033 for the same 3 items — index size = full-item W1 sum over indexed items, regardless of projection | §6.2: GSI size = W1 sum over indexed items |
| Sparse items in GSI counts | open | Sparse item (no `gk`) raises table count/size only; all GSI counts/sizes unchanged | §6.2: GSI sums cover indexed items only |
| Overwrite/delete size behavior | open | Immediate and exact: overwrite 1011→511 gives 2533; delete gives 1522; sparse put (+107) gives 1629 — all exact W1 sums | §6.2: maintain stored sizes on every write path |
| BatchGetItem 16MB cap | no enforcement | **Enforced.** 100 × 170KB (~17MB): 96 returned, 4 spilled to `UnprocessedKeys` | §6.3: **implement** |
| 16MB measurement basis | open | First pass inconclusive (both models gave the same spill boundary). **Supplemental probe (binary search): W1 ITEM ACCOUNTING** — max fully-returned s*=167763 fits 100×(9+s)=16,777,200 ≤ 16,777,216 exactly; the wire model (100×(34+s)=16,779,700) exceeds the cap. Exact cap = **16MiB = 16,777,216 bytes** | §6.3: W1 accounting — **corrects the first-pass wire-bytes choice** |
| 16MB pre/post-projection | open | **PRE-projection.** 100×200KB with `ProjectionExpression="pk"` still spilled 19; 81 returned = ⌊16,777,216 / 204,809⌋ | §6.3: measure the stored item, ignore projection |
| 16MB per-table or whole-response | open | **Whole-response.** 2 tables × 50×300KB (~15MB each, ~30MB total, 100 keys): only 54 items returned total (4 from A + 50 from B) — one global accumulator. Reference's cross-table order is arbitrary (B fully served before A despite A sorting first) | §6.3: single accumulator across tables; engine keeps deterministic sorted order (divergence, §11) |
| 16MB `UnprocessedKeys` echo shape | asserted, unobserved | **Confirmed.** Spilled `KeysAndAttributes` echoes `ProjectionExpression`, `ExpressionAttributeNames`, and `ConsistentRead` | §6.3: echo full `KeysAndAttributes` |
| ListTables Limit == count: LEK set? | open | **LEK NOT set** (3 tables, Limit=3 → all 3, `LastEvaluatedTableName`=nil) | §8 #7: no divergence; M3 §9.2 risk 6 closed |
| UpdateTable error precedence | open | **Existing-index check beats schema validation** ("Attempting to create an index which already exists") | §8 #8: confirmed |
| Same-name GSI | open | **Accepted** — GSI named identically to its table, no error | §8 #9: confirmed; no validation |

## 3. W1 — Item-size accounting

**Status: implemented 2026-08-03.**

### 3.1 The formula

Per AWS *DynamoDB item sizes and formats*, per attribute:

- **S:** UTF-8 bytes of name + UTF-8 bytes of value.
- **N:** UTF-8 bytes of name + ⌈significant digits / 2⌉ + 1 byte — **exact, P-size-verified** (AWS documents this only as "approximately"). Significant digits are counted from the canonical `num.Decimal` coefficient after trimming leading/trailing zeros; `"0"` has 0 significant digits → size 1. Verified across 14 inputs from 1 to 38 significant digits (see the findings doc §2 for the full table).
- **B:** UTF-8 bytes of name + raw byte count.
- **BOOL / NULL:** UTF-8 bytes of name + 1 byte.
- **L / M:** UTF-8 bytes of name + 3 bytes + sum of element sizes; each element adds 1 byte of overhead. Map elements: key UTF-8 bytes count as the element's "name". Recurses.
- **SS / NS / BS:** UTF-8 bytes of name + sum of element sizes (no container overhead; elements sized as the scalar without a name).

Item size = sum over attributes. Limit: **exactly 400KB (409600 bytes)** — P-size confirmed **no per-item fixed overhead counts** toward the write rejection (the AWS-documented 100-byte storage-billing overhead is NOT counted; a baseline item of exactly 409600 bytes by the formula is accepted).

### 3.2 Engine changes

**Walker lives in `ddb`, unexported — no new `attrval` API.** `attrval` is a cross-module public contract (the adapter module imports it); limit-enforcement policy does not belong on its surface, the same reason `num`/`expr` are internal. The existing exported accessors (`Tag`, `Str`, `Bin`, `Bool`, `List`, `Map`, `SS`, `NS`, `BS`) suffice for a complete walk, and numbers use `(*Value).Num().Digits()` — `ddb` may import `internal/num` (same module). So: `itemSize(item Item) (bytes int64, depth int)` in `ddb/items.go` — one recursive, allocation-free traversal returning size and max nesting depth together (accessors return internal references; immutable-by-construction means no copies). The only consumer is `ddb`; the adapter maps `ErrValidation` and never calls it.

**Depth counting base — pinned here, shared with §4's path check:** a value's depth equals the number of document-path segments that address it: top-level attribute values are depth 1; map entries' values and list elements are one deeper than their container; scalars add no levels below themselves. Item max depth is the max over all values. The item-side check and the expression path-depth check (§4 #5) use this same base, so the two limits agree by construction rather than by parallel implementation; P-expr pins the boundary (32 vs 33) against the reference.

**Enforcement points** (all → `ErrValidation` → adapter `ValidationException`):

1. `PutItem` (`ddb/items.go`) — replaces the JSON-proxy check at the existing site; `maxItemBytes` and the proxy are deleted.
2. `UpdateItem` (`ddb/update.go`) — the merged post-update item is sized before write; oversize → `ErrValidation`, no write.
3. `BatchWriteItem` (`ddb/batch.go`) — each put request sized during pre-validation (pre-validate-then-apply is unchanged; oversize anywhere fails the whole batch, matching existing all-or-nothing semantics).

Depth >32 levels in the item structure → `ErrValidation` at the same three points (P-expr confirmed: 32 accepted, 33 rejected with "Nesting Levels have exceeded supported limits", independently of expressions). The probe used nested maps; list-nested depth shares the same depth base by construction (not separately probed).

**Error precedence (P-size-verified):** size validation (`ValidationException`) is checked before condition evaluation (`ConditionalCheckFailedException`) — an oversize item with a failing `ConditionExpression` returns the size error. The engine's existing order in `ddb/items.go` matches the reference; no reordering.

**Not enforced** (structurally unreachable, M5b analysis): BatchWriteItem 16MB request cap.

## 4. W2 — Expression limits

**Status: implemented 2026-08-03.**

Enforced in `internal/expr`, all → `ErrValidation` (adapter maps to `ValidationException`), boundaries per P-expr findings:

1. **Expression string ≤4KB** (byte length), checked at parse entry for every expression kind (Condition, Filter, Update, Projection, KeyCondition). P-expr verified the 4096/4097 boundary on `ProjectionExpression` only; the other kinds share the limit per AWS-doc default (not separately probed).
2. **Operator count ≤300**, counted during parse: comparators (`= <> < <= > >=`), `AND`/`OR`/`NOT`, `BETWEEN`, `IN`, functions (`attribute_exists`, `begins_with`, `contains`, `size`, `if_not_exists`, `list_append`), `+`/`-` in update expressions. `=` in `SET` actions is assignment syntax, not counted (AWS-documented). P-expr confirmed the quirk: the error message reports the count as 301 regardless of actual count. **`IN` counts as exactly 1 operator** regardless of operand count (P-expr: 3×IN(100) + 2 AND = 5 operators, accepted), so IN-heavy expressions cannot trip the 300 cap. Only `=`, `AND`, and `IN` counting are probe-verified; SET `=` exclusion, `+`/`-`/function/`BETWEEN`/`NOT` counting, and the cap's applicability to Update/Filter/KeyCondition expressions are AWS-doc defaults (not probed).
3. **`IN` ≤100 operands**, at parse.
4. **Token limits at bind (P-expr-verified):** single substitution *name* token (the `#name` key string in `ExpressionAttributeNames`) ≤255 bytes — the limit applies to the token key string only, NOT to the actual attribute name it maps to (a 256-byte actual name is accepted via substitution). `:value` tokens have **no 255B limit**; an individual `:value` is capped at 1,048,567 bytes serialized (~1MB), error "Expression size has exceeded". The substitution sum is capped at **~1MB *serialized* `ExpressionAttributeValues`** (the serialized JSON map, including keys and `{"S":…}` wrappers) — NOT the AWS-documented 2MB; local's 2MB "exceeds max size" error is unreachable for string values because the ~1MB serialized cap trips first (same error text as the 4KB string limit). The engine enforces the ~1MB serialized cap to match the reference; the docs-vs-local discrepancy is recorded in §11.
5. **Path nesting ≤32 levels** in expression document paths, at parse/bind, measured as path-segment count — the depth base pinned in §3.2 — so the path check and the item-side check agree by construction.

These slot into the existing lex/parse/bind pipeline as validation passes; no grammar changes. Placement is the `Parse*`/`Bind*` entry points in `internal/expr`, **not** `ddb.prepareExpressions` — BatchGetItem parses projections on its own path (`ddb/batch.go`) and would escape checks placed only in `prepareExpressions`.

## 5. W3 — Naming & key validation

**Status: implemented 2026-08-03 (plan: docs/superpowers/plans/2026-08-03-m6c-w3-naming-key-validation.md).**

All rules P-names-confirmed; all → `ErrValidation`:

1. **Table names** at `CreateTable`: 3–255 chars, `[A-Za-z0-9_.-]+` (same rule M4 applied to GSI names via `validGsiName`; the helper generalizes).
2. **`validateTableName` on every operation.** Consolidates the §2.3 patchwork: a package-private `ddb` helper validates presence of `TableName` (P-names confirmed: empty string → `ValidationException` on all 12 operations; nil `TableName` is caught SDK-side), called at the top of every public op — and per table entry in the batch ops' request maps, replacing their inline `""` checks. This moves `UpdateTable`'s adapter-side guard into the engine and fixes the live `ErrTableNotFound`-for-`""` divergence on the remaining ops.
3. **Key-value lengths** in a shared key-value validator called from **both** `validateKey` (GetItem/DeleteItem/UpdateItem key paths) **and** `validatePutKey` (PutItem/BatchWriteItem) — checks in `validateKey` alone would leave the write paths unenforced. Partition key ≤2048 bytes, sort key ≤1024 bytes — P-names confirmed the boundaries are **inclusive** (2048/1025-style: 2048 and 1024 accepted, 2049 and 1025 rejected; local's error text says "under 2048/1024" but the limit is ≤ — message quirk recorded in §11). Minimum 1 byte is **new enforcement, not existing behavior**: only GSI key values reject empty S/B today; primary keys accept them (a live divergence — verified against the engine: `PutItem`/`GetItem` with an empty-string partition key succeed). P-names confirmed the reference rejects, with **different messages per type**: empty S → "AttributeValue for a key attribute cannot contain an empty string value"; empty B → "Supplied AttributeValue is empty, must contain exactly one of the supported datatypes" (both `ValidationException`). A conformance case pins the boundary. N keys need no boundary enforcement: the 38-significant-digit limit bounds any number to ~21 bytes by the W1 formula and well under 200 bytes as a maximally padded wire string — always under 1024 (the planned single confirmation probe was not run; AWS-doc default).
4. **Index-adjacent name lengths** at `CreateTable`/`UpdateTable`: GSI key attribute names ≤255 bytes; `INCLUDE` `NonKeyAttributes` names ≤255 bytes.
5. **Cross-index projected-attribute sum ≤100** user-specified projected attributes across all of a table's GSIs (`KEYS_ONLY`/`ALL` exempt per AWS docs). P-names confirmed the counting rule: **index/table key attributes do NOT count** (99 `INCLUDE` attrs + 2 key attrs accepted; single GSI with 101 rejected; two GSIs summing to 101 rejected). Local's error message is misleading ("No more than 20 attributes per table can be projected into Local Secondary Indices" — the actual GSI limit is 100); the engine uses a correct message (§11).

**Fixture ripple:** P-names confirmed min-3 for table names, so the ripple is real: ddb unit tests use 1–2 char table names (`"T"` etc.) pervasively. Same for the adapter module's unit tests (`awsdynamodb/adapter_test.go`, `query_test.go`), which reach engine validation through the adapter. Both are mechanically renamed in the same commit as rule 1 (e.g. `T` → `Tbl`). Conformance fixtures already use ≥3-char names (they run against dynamodb-local).

## 6. W4 / W5 / W6 — Probe-resolved workstreams

### 6.1 W4 — Nested INCLUDE NonKeyAttributes

**Status: implemented 2026-08-03 (plan: docs/superpowers/plans/2026-08-03-m6c-w4-nested-include.md).**

**Resolved by P-include: accepted-but-never-projected — current behavior matches the reference.** Local accepts `NonKeyAttributes: ["obj.a"]` at `CreateTable`, accepts the write, and GSI query returns only the key attributes (`obj.a` is NOT projected). No code change; the M4/M6B notes are updated from "divergence deferred" to "matches reference" (§10 docs task), and the §11 entry is removed.

### 6.2 W5 — DescribeTable ItemCount/size reporting

**Status: implemented 2026-08-03 (plan: docs/superpowers/plans/2026-08-03-m6c-w5-describetable-reporting.md).**

**Resolved by P-desc: implement real, immediate values.** dynamodb-local reports exact `ItemCount`/`TableSizeBytes` immediately (empty table: 0/0; after 3 puts of 1011-byte items: ItemCount=3, TableSizeBytes=3033) — no eventual consistency. `TableSizeBytes` equals the **W1 item-accounting sum** (3 × 1011 = 3033; wire-JSON bytes would be ≈3135 ≠ 3033), so W5 reuses W1's `itemSize` walker.

Supplemental probe findings:

- **Per-GSI sizes are projection-independent.** KEYS_ONLY, INCLUDE, and ALL GSIs all report the same `ItemCount`/`IndexSizeBytes` for the same indexed items — `IndexSizeBytes` is the **full-item W1 sum over indexed items**, not a function of what the index projects.
- **Sparse items are excluded** from GSI counts/sizes (an item without the GSI key raises only the table's count/size).
- **Overwrites and deletes adjust immediately and exactly** (overwrite 1011→511 → 2533; delete → 1522; sparse put +107 → 1629 — all exact W1 sums), so stored sizes must be maintained on every write path, not just inserts.

Implementation: `internal/storage` gains count/size queries against data and GSI tables — `COUNT(*)` plus `SUM` over a per-item W1-accounting size stored at write time (the walker already runs at write for W1 enforcement; GSI rows carry the item's full W1 size at index-maintenance time). `TableDescription` gains `ItemCount`/`TableSizeBytes` and per-GSI `ItemCount`/`IndexSizeBytes`; the adapter maps them. Conformance pins both `ItemCount` and `TableSizeBytes` **exactly** after known writes, including overwrite/delete/sparse cases. The §11 "reports 0 counts/sizes" entry is removed.

### 6.3 W6 — BatchGetItem 16MB response cap

**Status: implemented 2026-08-03 (plan: docs/superpowers/plans/2026-08-03-m6c-w6-batchgetitem-16mb-cap.md).**

**Resolved by P-batch: implement the cap.** dynamodb-local enforces a response cap of exactly **16MiB (16,777,216 bytes)**: 100 items × 170KB (~17MB) returned 96 items and spilled 4 keys to `UnprocessedKeys`. Supplemental-probe findings (the first-pass items couldn't distinguish the measurement models; a binary search on per-item size could):

- **Measurement basis is W1 item accounting, NOT wire bytes.** Max fully-returned per-item size s*=167,763 fits 100×(9+s)=16,777,200 ≤ 16,777,216 exactly; the wire-byte model (100×(34+s)=16,779,700) exceeds the cap. W6 therefore **reuses W1's `itemSize` walker** — the two workstreams share the accounting function.
- **Measurement is PRE-projection.** `ProjectionExpression="pk"` on 100×200KB items still spilled 19 keys (81 returned = ⌊16,777,216 / 204,809⌋) — the stored item is measured, projection is ignored.
- **The cap is whole-response**, one accumulator across all tables in the request: 2 tables × 50×300KB (~15MB each) returned only 54 items total. The reference's cross-table accumulation order is arbitrary (B was fully served before A despite A sorting first); the engine keeps M5b's deterministic sorted order (divergence, §11).
- **Echo shape confirmed:** spilled `UnprocessedKeys` entries echo the full `KeysAndAttributes` — `ProjectionExpression`, `ExpressionAttributeNames`, `ConsistentRead`, and the remaining keys.

The engine accumulates W1 item size during the per-table fetch loop and spills remaining keys into `UnprocessedKeys`. Accumulation order is deterministic engine-internally: iterate tables and keys in the same sorted order M5b's responses already use. **This breaks M5b's "UnprocessedKeys always empty" invariant**; M5b §8's divergence note is updated. BatchWriteItem's 16MB stays unenforced (unreachable).

**W6 conformance asserts counts, not bodies.** The reference's spill distribution is not deterministic — across tables its accumulation order is arbitrary (P-batch: B fully served before A), and even within one table the exact key set that spills is not a stable contract. Dual-target conformance cases therefore assert only: total items returned per table, total `UnprocessedKeys` counts per table, returned + unprocessed = requested, and that spilled entries carry the request's projection/EAN/ConsistentRead shape. Exact response bodies and exact spill composition are never compared against the reference.

## 7. W7 — Legacy-field rejection audit

**Status: implemented 2026-08-03 (audit complete — rejection matrix fully pinned with adapter-only unit tests).**

Policy (settled): **all legacy/deprecated fields are rejected** with `ErrValidation` → `ValidationException`; honoring them remains a documented deliberate divergence (parent spec §7.5; conformance "reference honors it" notes stay).

The audit makes rejection *complete*. Matrix — every op × every legacy field:

| Op | Legacy fields |
|---|---|
| PutItem / DeleteItem | `Expected`, `ConditionalOperator` |
| UpdateItem | `Expected`, `ConditionalOperator`, `AttributeUpdates` |
| GetItem / Query / Scan / BatchGetItem | `AttributesToGet` |
| Query | `KeyConditions`, `QueryFilter`, `ConditionalOperator` |
| Scan | `ScanFilter`, `ConditionalOperator` |

For each populated cell: rejection exists → pin with a unit test; gap (silently ignored) → add the rejection + test. Adapter-side: confirm the SDK-level rejections (`rejectLegacy`, `rejectLegacyGetItem`, `rejectLegacyQuery`, `rejectLegacyScan`, and the existing `AttributeUpdates` rejection) cover the same matrix; conformance's adapter-only rejection tests stay adapter-only (the reference honors these fields).

## 8. W8 — Golden-corpus edge-case audit

**Status: implemented 2026-08-03.** New conformance cases in `conformance_test.go`, dual-target, each expectation measured against dynamodb-local first (extending the probe battery where a question isn't already covered):

1. **Null vs missing attributes** in conditions, filters, projections (`attribute_exists` on Null-valued attrs, comparisons against missing paths).
2. **Type-mismatched comparisons** across every comparator (`= <> < <= > >=`, `BETWEEN`, `IN`) and operand-type pair — false-not-error semantics.
3. **Sparse GSIs** — items missing GSI key attrs or with wrong-typed ones: absent from index on write, absent from GSI query/scan results, updatable.
4. **Trailing empty pages** — `Limit` lands exactly on a partition/result boundary; LEK presence per the M3 stop-reason contract.
5. **`LastEvaluatedKey` resume at partition end.**
6. **`Limit=0` rejection** — verified: pinned adapter-side (`awsdynamodb/query.go`) and dual-target (`TestConfQueryLimitZero`/`TestConfScanLimitZero`). The engine itself treats `Limit=0` as unset/unlimited (its `Limit` is `int32`, 0 = absent) — a deliberate engine-API simplification, recorded in §11.
7. **`ListTables` `Limit == table-count` edge** — P-misc resolved: 3 tables with Limit=3 returns all 3 and `LastEvaluatedTableName` is NOT set. No divergence; M3 §9.2 risk 6 is closed. A conformance case pins it.
8. **`UpdateTable` error precedence** — P-misc resolved: the existing-index check beats schema validation (GSI `Create` with an existing index name + invalid key schema → "Attempting to create an index which already exists"). A conformance case pins it.
9. **Same-name GSI collision** — P-misc resolved: a GSI named identically to its table is **accepted** at `CreateTable`; no validation is added. A conformance case pins it.

Triage per §1 policy: small/isolated mismatches → `conformance_divergence_test.go` revived, RED case → fix → migrate to `conformance_test.go`, file retired again when empty; structural mismatches → §11 divergence table.

**Resolution (2026-08-03):** all nine items pinned dual-target in
`conformance_test.go` (`TestConfNullVsMissing`, `TestConfTypeMismatchComparisons`,
`TestConfOrderingOperandTypeValidation`, `TestConfGSISparseUpdate`,
`TestConfScanTrailingEmptyPage`, `TestConfScanResumePartitionBoundary`,
`TestConfListTablesLimitEqualsCount`, `TestConfUpdateTableCreateExistingPrecedence`,
`TestConfGsiSameNameAsTable`; #6 was already pinned). Two items were real
divergences and were fixed via the RED flow: #8 (engine checked key schema
before existing-index in `validateCreateGsi` — reordered) and the #2
operand-type half (ordering comparators/BETWEEN now reject non-scalar `:value`
operands at `expr.Bind`, matching the reference's request-time
ValidationException). The #8 existing-index case is pinned at the message level
(`TestConfUpdateTableCreateExistingPrecedence` asserts the error contains
"already exists") because dynamodb-local reports it as ValidationException while
the engine uses ResourceInUseException (real-AWS behavior); the precedence
divergence (existing-index error surfaces before schema error) is fixed for both
targets. No new §11 divergences; `conformance_divergence_test.go` was revived
and retired again empty.

## 9. W9 — Fuzz pass

**Status: implemented 2026-08-04.** New targets: `FuzzItemSize` (`ddb/fuzz_test.go`), `FuzzProject` (`attrval/fuzz_test.go`), `FuzzAddSub` (`internal/num/fuzz_test.go`); boundary-biased seeds added to `FuzzParseCondition`/`FuzzParseUpdate` (`internal/expr/fuzz_test.go`). No crashers found in 30s spot runs.

New `go test -fuzz` targets with `f.Add` seed corpora, matching existing conventions (`internal/num/fuzz_test.go`, `attrval/fuzz_test.go`):

1. **`ddb.itemSize`** — no panic, non-negative size, depth ≥1 on non-empty items, on arbitrary nested items (seed corpus: deep nesting, huge strings, all tags). Fuzzed from an in-package `ddb/fuzz_test.go` (same-package tests reach the unexported walker; items built via `attrval.New*`).
2. **`attrval.Project`** — no panic on arbitrary path lists × arbitrary items (M6B added `Project` unfuzzed).
3. **Expression parser at boundaries** — extends the existing expression fuzz seeds with boundary-biased inputs: ~4KB strings, ~300 operators, deep paths, 100/101-operand `IN` lists. Asserts no panic (limits produce errors, not crashes).
4. **`num.Add`/`Sub`** — range/precision behavior and no panic (M2 added them unfuzzed).

Existing targets unchanged. Fuzz runs stay manual (`-fuzztime`); no CI exists to wire them into.

## 10. Testing & verification

- **Probe battery complete.** §2.8's results table is filled and §3–§6/§8/§11 amended from the findings (`2026-08-03-m6c-probe-findings.md`); implementation proceeds with zero open probe questions (same flow as M6a).
- **Engine unit tests** at every new boundary, table-driven, existing conventions (`t.Helper()` constructors, `:memory:` clients, one-`BeginTx` storage tests).
- **Conformance:** each workstream lands dual-target cases; W8's audit cases are the golden corpus. Adapter-only tests for the legacy-rejection matrix (W7) and any probe-confirmed engine/reference error-code disagreements.
- **Verification:** `go test ./...` (root) and `cd awsdynamodb && go test ./...` (adapter, default target) green; one `DDBSQLITE_CONF_TARGET=all go test -count=1 ./...` dual-target run; spot fuzz runs (`-fuzztime=30s`) on the new targets.
- **Docs:** parent spec §5/§6 limit claims updated to implemented; resolution notes added to M1 (proxy), M2 (limits), M3 (ListTables risk note), M4 (table-name deferral, nested INCLUDE), M5b (16MB note — W6 lands; the "UnprocessedKeys always empty" invariant is broken), M6a (§9 leftovers), M6B (nested INCLUDE deferral); AGENTS.md milestone status → M6 complete.

## 11. Out of scope & standing divergences

**Out of scope for M6c:**

- 1MB Query/Scan read budget — M3 §6 decision stands (capacity concern, out of scope for the mock).
- Honoring legacy fields (`AttributesToGet`, `KeyConditions`, `QueryFilter`/`ScanFilter`, `Expected`, `ConditionalOperator`) — rejection is policy (§7); the reference-honors-it divergence is documented, not closed.
- `TransactWriteItems`/`TransactGetItems`, PartiQL, LSI — v1 non-goals (parent spec §8).
- Throttling / `UnprocessedItems` simulation on BatchWriteItem; BatchWriteItem 16MB (structurally unreachable, §1.1).
- Storage-billing overheads (the 100-byte per-item billing overhead) — P-size confirmed it does NOT count toward the 400KB write rejection (§3.1).
- `ReturnConsumedCapacity`/`ReturnItemCollectionMetrics` accounting (accepted-and-ignored, unchanged).

**Divergence table** (probe-resolved entries removed; current entries):

| Divergence | Status |
|---|---|
| Legacy fields rejected, reference honors them | deliberate, standing (W7 makes rejection complete) |
| dynamodb-local enforces a ~1MB *serialized* `ExpressionAttributeValues` cap (error "Expression size has exceeded"), not the AWS-documented 2MB substitution sum; local's 2MB "exceeds max size" error is unreachable for string values | engine matches the reference (~1MB serialized, W2); AWS-docs-vs-local discrepancy recorded |
| Cross-index sum error message says "No more than 20 attributes per table can be projected into Local Secondary Indices" but the actual GSI limit is 100 | local error-message bug (P-names); engine uses a correct message (W3) |
| Key-value length error text says "under 2048/1024" but the limits are inclusive (≤2048/≤1024) | local error-message quirk (P-names); engine enforces the inclusive boundary (W3) |
| BatchGetItem response ordering (sorted by primary key; reference unordered) | deliberate, standing (M5b) |
| BatchGetItem 16MB spill accumulation order across tables (engine: deterministic sorted table/key order; reference: arbitrary — P-batch observed table B fully served before A) | deliberate (W6; consistent with M5b ordering) |
| No TTL read filtering (expired items returned) | deliberate, standing (M5a Faithful model) |
| UpdateTable multi-action error code (`ValidationException` vs local's `LimitExceededException`) | standing (M6a probe P4) |
| Engine `Limit=0` treated as unset/unlimited; the adapter rejects to match the reference | deliberate, standing (engine `int32` Limit cannot distinguish absent from 0) |
| N key columns stored as `REAL` — float64 index ordering; exact value preserved in the JSON blob | deliberate, standing (parent spec; acceptable for a test mock) |
| BatchGetItem returns an empty per-table `Responses` entry for all-miss tables | deliberate, standing (M5b; matches dynamodb-local, differs from AWS doc behavior) |
