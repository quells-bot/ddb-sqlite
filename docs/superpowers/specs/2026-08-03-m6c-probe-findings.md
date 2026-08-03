# M6c Probe Findings — Spec Changes Required

**Date:** 2026-08-03
**Source:** `awsdynamodb/probe_m6c_test.go` run against dynamodb-local 3.3.1
**Scope:** This document records probe findings from the M6c §2 probe battery and enumerates every change required to the M6c spec (`2026-08-03-m6c-hardening-design.md`) based on what the probes revealed. It is the deliverable for the probe-battery phase; implementation proceeds from the amended spec.

---

## 1. Probe results table (fills §2.8)

### §2.1 P-size — item-size accounting

| Probe question | Default | Observed | Action |
|---|---|---|---|
| 100-byte storage overhead toward 400KB? | not counted | **Not counted.** Baseline item `{pk:"k", big:S(n)}`: max n=409594 → item=409600=exactly 400KB. Formula: 2(pk) + 1(k) + 3(big) + n = 6+n = 409600. | §3.1: state definitively — no per-item overhead; limit is exactly 409600 bytes. |
| S value bytes | name + UTF-8 bytes | delta=2 for 1-byte S (name 1 + value 1) ✓ | no change |
| B value bytes | name + raw bytes | delta=2 for 1-byte B ✓ (not base64) | no change |
| N size function | ~1 byte/2 sig digits + 1 | **Exact formula confirmed: N_size = ceil(sig_digits / 2) + 1**, where "0" has 0 significant digits. See §2 below for full table. | §3.1: remove "approximately"; state exact formula. |
| BOOL/NULL | name + 1 | delta=2 for both ✓ | no change |
| L/M overhead | name + 3 + elements (each +1) | L_empty=4, M_empty=4, L1=6, L2=8, M1=7, M2=10, L_nested=10 — all match formula ✓ | no change |
| SS/NS/BS | name + elements (no container overhead) | SS1=2, SS2=3, NS1=3, NS2=5, BS1=2 ✓ | no change |
| Error precedence: oversize + failing condition | size first | **ValidationException** (size) beats ConditionalCheckFailedException. | §3.2: confirm current engine behavior is correct. |

### §2.2 P-expr — expression limits

| Probe question | Default | Observed | Action |
|---|---|---|---|
| 4KB expression string | 4096 accept, 4097 reject | ProjectionExpression: 4096 accepted, 4097 → ValidationException "Expression size has exceeded" ✓ | no change |
| Operator count ≤300 | 301 reject | 299 ops accepted, 301 → ValidationException "operator count: **301**" ✓ (quirk confirmed) | no change |
| IN ≤100 operands | 100 accept, 101 reject | 100 accepted (condition false), 101 → ValidationException "number of operands: 101" ✓ | no change |
| IN × operator-count interaction | open | **IN counts as 1 operator** regardless of operand count. 3×IN(100) + 2 AND = 5 ops → accepted. If IN counted as 1+N (305 ops) it would reject. | §4 #2: add clarification — IN is 1 operator; IN-heavy expressions cannot trip the 300 cap. |
| #name token 255B | 255 accept, 256 reject | **Token string (#name in expression): 255 accepted, 256 → "key too long; size of key: 256".** Actual attribute name (what #n maps to): 256 accepted — no limit. | §4 #4: **correct** — the 255B limit applies only to ExpressionAttributeNames keys (#name tokens), NOT to the actual attribute names they map to. |
| :value token 255B | 255 accept, 256 reject | **No 255B limit on individual :value.** 256B accepted, 4087B accepted. Single :value cap binary-searched to **1,048,567 bytes (~1MB)**; 1,048,568 rejected with "Expression size has exceeded." | §4 #4: **correct** — remove 255B claim for :value tokens. Add: individual :value limit is ~1MB serialized. |
| 2MB substitution sum | 2MB sum | **dynamodb-local enforces ~1MB serialized** ExpressionAttributeValues (not 2MB). Error: "Expression size has exceeded" (same as 4KB error). A separate "ExpressionAttributeValues exceeds max size" error exists at 2MB raw but is unreachable for string values (the ~1MB serialized limit trips first). | §4 #4: **correct** — the 2MB documented limit is not what dynamodb-local enforces. Record as a dynamodb-local divergence. The engine should enforce the ~1MB serialized cap to match the reference. |
| Path nesting 32 vs 33 | 32 accept, 33 reject | 32 accepted, 33 → "nesting levels: 33" ✓ | no change |
| Item-structure depth >32 | rejected independently | 32 accepted, 33 → "Nesting Levels have exceeded supported limits" ✓ (independent of expressions) | no change |

### §2.3 P-names — naming & key validation

| Probe question | Default | Observed | Action |
|---|---|---|---|
| Table name charset/length | 3–255 [A-Za-z0-9_.-] | 2-char and 256-char rejected, space/!/slash/non-ASCII rejected, . and _ accepted, 255 accepted ✓ | §5 #1: confirmed, same rule as GSI names. |
| Empty TableName on all ops | ValidationException | **All 12 operations** return ValidationException for empty string. Nil TableName caught SDK-side (not a server error). | §5 #2: confirmed. Engine must validate empty string on every op. |
| pk S/B 2048 boundary | ≤2048 | 2048 accepted, 2049 rejected ✓ (error says "under 2048" but 2048 is accepted — inclusive) | §5 #3: note boundary is inclusive (≤2048), error message is misleading. |
| sk S/B 1024 boundary | ≤1024 | 1024 accepted, 1025 rejected ✓ (same inclusive behavior) | §5 #3: same. |
| Empty primary key values | rejected | S empty → "empty string value"; B empty → "AttributeValue is empty" ✓ | §5 #3: confirmed. Different error messages for S vs B. |
| GSI key attr name 255B | ≤255 | 255 accepted, 256 → "must be between 1 and 255 characters, inclusive" ✓ | §5 #4: confirmed. |
| INCLUDE NonKeyAttributes name 255B | ≤255 | 255 accepted, 256 → same error ✓ | §5 #4: confirmed. |
| Cross-index sum ≤100 | ≤100 | Single GSI 100 accepted, 101 rejected. Two GSIs sum=101 rejected. 99 accepted (key attrs don't count). | §5 #5: confirmed. Key attrs do NOT count. **Error message is misleading** — says "No more than 20 attributes per table can be projected into Local Secondary Indices" but the actual limit is 100 for GSIs. |

### §2.4 P-include — nested INCLUDE

| Probe question | Default | Observed | Action |
|---|---|---|---|
| `NonKeyAttributes: ["obj.a"]` | accepted, never projected | **Accepted at CreateTable. PutItem accepted. GSI Query returns only `[gk, pk, sk]` — obj.a NOT projected.** | §6.1: confirmed — current behavior matches reference. Update M4/M6B notes from "divergence deferred" to "matches reference". No code change. |

### §2.5 P-desc — DescribeTable reporting

| Probe question | Default | Observed | Action |
|---|---|---|---|
| ItemCount/TableSizeBytes present? | 0 (engine); zeros (adapter) | **Real, immediate values.** Empty: ItemCount=0, Size=0. After 3 puts: ItemCount=3, TableSizeBytes=3033, GSI ItemCount=3, IndexSizeBytes=3033. Immediate (not eventually consistent). | §6.2: **change from "probe → match" to "implement"** — see §6.2 below. |
| Size = W1 accounting or wire bytes? | open | **W1 item accounting.** 3 items × {pk:"kN", gk:"G1", big:S(1000)} = 3 × 1011 = 3033. Wire JSON would be 3 × ~1045 = 3135 ≠ 3033. | §6.2: pin TableSizeBytes = W1 item-accounting sum. |

### §2.6 P-batch — BatchGetItem 16MB

| Probe question | Default | Observed | Action |
|---|---|---|---|
| 16MB response cap enforced? | no enforcement | **Enforced.** 100 items × 170KB (~17MB): 96 returned, 4 spilled to UnprocessedKeys. | §6.3: **change from "probe → match" to "implement"** — see §6.3 below. |
| Measurement basis | open | **Both W1 and wire bytes give the same spill boundary** for this probe (the per-item JSON overhead is small enough that the boundary doesn't shift). Wire bytes is the natural choice since the cap is on the serialized response and `LENGTH(data)` is nearly free. | §6.3: use wire bytes (LENGTH(data)). |

### §2.7 P-misc

| Probe question | Default | Observed | Action |
|---|---|---|---|
| ListTables Limit == table count: LEK set? | open | **LEK NOT set.** 3 tables, Limit=3: returns all 3, LastEvaluatedTableName=nil. | W8 #7: no divergence. M3 §9.2 risk 6 resolved — no change needed. |
| UpdateTable error precedence | open | **Existing-index check beats schema validation.** Create GSI with existing name + invalid key schema → "Attempting to create an index which already exists" (not the schema error). | W8 #8: confirmed. |
| Same-name GSI | open | **Accepted.** GSI named "SameName" on table "SameName" — no error. | W8 #9: confirmed. No validation needed for same-name GSI. |

---

## 2. N-size formula — exact values

The probe binary-searched each number's contribution to the 400KB limit. Deltas = name("t"=1 byte) + N_size. So N_size = delta − 1.

| Input | Sig digits (after trimming) | N_size (observed) | Formula: ceil(sig/2)+1 |
|---|---|---|---|
| `"0"` | 0 | 1 | ceil(0/2)+1 = 1 ✓ |
| `"1"` | 1 | 2 | ceil(1/2)+1 = 2 ✓ |
| `"9"` | 1 | 2 | 2 ✓ |
| `"10"` | 2 | 2 | ceil(2/2)+1 = 2 ✓ |
| `"99"` | 2 | 2 | 2 ✓ |
| `"100"` | 1 (trailing zeros trimmed → "1") | 2 | 2 ✓ |
| `"123"` | 3 | 3 | ceil(3/2)+1 = 3 ✓ |
| `"999"` | 3 | 3 | 3 ✓ |
| `"1000"` | 1 | 2 | 2 ✓ |
| `"00100"` | 1 (→ "100" → "1") | 2 | 2 ✓ |
| `"100.00"` | 1 (→ "100") | 2 | 2 ✓ |
| `"0.0001"` | 1 (→ "0001" → "1") | 2 | 2 ✓ |
| `"12345678901234567890"` | 20 | 11 | ceil(20/2)+1 = 11 ✓ |
| `"999...9"` (38 digits) | 38 | 20 | ceil(38/2)+1 = 20 ✓ |

**Confirmed formula:** `N_size = ceil(sig_digits / 2) + 1`, where significant digits are counted from the canonical `num.Decimal` coefficient after trimming leading/trailing zeros, and "0" has 0 significant digits.

---

## 3. Spec changes by workstream

### W1 — Item-size accounting (§3)

**§3.1 changes:**
1. **Remove "approximately"** from the N-size description. The exact formula is `N_size = ceil(sig_digits / 2) + 1` (see §2 above).
2. **State definitively:** no per-item storage overhead (the AWS-documented 100-byte storage overhead) counts toward the 400KB write rejection. The limit is **exactly 409,600 bytes**.
3. All other formula terms (S, B, BOOL, NULL, L/M, SS/NS/BS) are confirmed exactly as written — no changes.

**§3.2 changes:**
4. **Confirm error precedence:** size validation (ValidationException) beats condition evaluation (ConditionalCheckFailedException). The current engine behavior (size checked first in `ddb/items.go`) matches the reference. No code change needed; add a note to §3.2 documenting this is probe-verified.

### W2 — Expression limits (§4)

**§4 #2 (operator count) — add:**
5. **IN counts as 1 operator** regardless of operand count. This means IN-heavy expressions cannot trip the 300-operator cap (each IN is 1 operator, same as `=` or `AND`). The spec's open question "does IN count as 1 + operands?" is answered: **1 operator only**.

**§4 #4 (token limits) — correct:**
6. **The 255B limit applies only to ExpressionAttributeNames keys** (`#name` token strings in the expression), NOT to the actual attribute names they map to. Actual attribute names of any length are accepted via substitution (256B actual name was accepted).
7. **Individual :value tokens have no 255B limit.** The individual :value cap is ~1,048,567 bytes (~1MB serialized). The spec's §1.2 claim "Single expression attribute name/value token ≤255 bytes" is **partially wrong** — the 255B limit is names only, not values.
8. **The 2MB substitution sum is not what dynamodb-local enforces.** dynamodb-local enforces a ~1MB serialized ExpressionAttributeValues cap (the serialized JSON map, including keys and `{"S":"..."}` wrappers). The error is "Expression size has exceeded the maximum allowed size" — the same error as the 4KB string limit. A separate "ExpressionAttributeValues exceeds max size" error exists at 2MB raw but is unreachable for string values (the ~1MB serialized limit trips first). **The engine should enforce the ~1MB serialized cap to match the reference.** Record the 2MB-vs-1MB discrepancy as a dynamodb-local divergence from AWS docs.

**§4 #5 (path depth) — no change:**
9. Path nesting ≤32 and item-structure depth ≤32 both confirmed. No changes needed.

### W3 — Naming & key validation (§5)

**§5 #1 (table names) — confirmed, no change:**
10. Table names: 3–255 chars, `[A-Za-z0-9_.-]+`. Same rule as GSI names (M4 G27).

**§5 #2 (validateTableName) — confirmed, no change:**
11. Empty string TableName → ValidationException on all 12 operations. Nil TableName is caught SDK-side (the adapter doesn't need to handle it, but the engine must reject empty string).

**§5 #3 (key-value lengths) — add notes:**
12. **Boundaries are inclusive:** pk ≤2048 (2048 accepted), sk ≤1024 (1024 accepted). The dynamodb-local error message says "under 2048" / "under 1024" but the actual limits are ≤2048 / ≤1024. The engine should enforce ≤2048 / ≤1024.
13. **Empty key values produce different errors for S vs B:** empty S → "AttributeValue for a key attribute cannot contain an empty string value"; empty B → "Supplied AttributeValue is empty, must contain exactly one of the supported datatypes". Both are ValidationException.

**§5 #5 (cross-index sum) — add note:**
14. **Key attributes do NOT count** toward the 100-attribute sum. Confirmed: 99 NonKeyAttributes + 2 key attrs = accepted.
15. **The dynamodb-local error message is misleading:** it says "No more than 20 attributes per table can be projected into Local Secondary Indices" but the actual limit is 100 for GSIs. The engine should use a correct error message.

### W4 — Nested INCLUDE (§6.1)

16. **No change.** Current behavior (accepted, never projected) matches dynamodb-local. Update M4/M6B notes from "divergence deferred" to "matches reference".

### W5 — DescribeTable reporting (§6.2)

17. **Change from "probe → match reference" to "implement":** dynamodb-local reports **real, immediate** ItemCount and TableSizeBytes. The engine must implement these.
18. **TableSizeBytes = W1 item-accounting sum** (not wire bytes). Confirmed: 3 items × 1011 bytes = 3033 = reported value. Wire JSON would give 3135 ≠ 3033.
19. **ItemCount = exact count** (not eventually consistent in dynamodb-local). Pin both ItemCount and TableSizeBytes exactly in conformance.
20. **Per-GSI reporting:** GSI ItemCount and IndexSizeBytes also report real values (equal to table values for an ALL-projection GSI where every item is indexed). Implement these too.
21. **§11 divergence table:** remove "DescribeTable reports 0 counts/sizes" entry (resolved — implement real values).

### W6 — BatchGetItem 16MB (§6.3)

22. **Change from "probe → match reference" to "implement":** dynamodb-local enforces a 16MB response cap. 96 of 100 items returned, 4 spilled to UnprocessedKeys.
23. **Measurement basis:** wire bytes (`LENGTH(data)`). Both W1 and wire give the same spill boundary, but wire bytes is the natural choice since the cap is on the serialized response and `LENGTH(data)` is nearly free.
24. **UnprocessedKeys contains the spilled keys** — confirmed. The spec's description of the spill mechanism (echo full KeysAndAttributes, deterministic sorted order) stands.
25. **§11 divergence table:** remove "BatchGetItem 16MB / UnprocessedKeys" pending entry (resolved — implement the cap). Add new entry: "BatchGetItem enforces 16MB response cap with UnprocessedKeys spill" as implemented behavior. Update M5b §8's "UnprocessedKeys always empty" invariant — it is now broken by W6.

### W7 — Legacy-field rejection (§7)

26. No probe findings (audit workstream, not probe-decided). No spec changes from probes.

### W8 — Golden-corpus (§8)

27. **ListTables Limit==count:** LEK NOT set. No divergence. M3 §9.2 risk 6 resolved.
28. **UpdateTable error precedence:** existing-index check beats schema validation. Add conformance case.
29. **Same-name GSI:** accepted at CreateTable. Add conformance case.
30. **Fixture ripple confirmed:** the table-name min-3 rule means ddb unit tests using 1–2 char names (`"T"`, `"Num"`, etc.) must be renamed. `"Num"` is 3 chars — OK. `"T"` is 1 char — must rename. `"Tbl"` is already 3 chars — OK. Check all test fixtures.

### W9 — Fuzz (§9)

31. No probe findings (coverage workstream). No spec changes from probes.

---

## 4. New divergences discovered

These were not in the §11 divergence table before and should be added:

| Divergence | Status |
|---|---|
| dynamodb-local enforces ~1MB serialized ExpressionAttributeValues (not the AWS-documented 2MB); error is "Expression size has exceeded" not "exceeds max size" | deliberate (match reference); record the AWS-docs-vs-local discrepancy |
| Cross-index sum error message says "20 attributes / Local Secondary Indices" but the actual GSI limit is 100 | dynamodb-local error message bug; engine uses correct message |
| Key-value length error says "under 2048/1024" but the limit is inclusive (≤2048/≤1024) | dynamodb-local error message quirk; engine uses inclusive boundary |

## 5. Resolved divergences (remove from §11)

| Divergence | Resolution |
|---|---|
| BatchGetItem 16MB / UnprocessedKeys | Implement the cap (W6). |
| Nested INCLUDE accepted, never projected | Matches reference (W4). No divergence. |
| DescribeTable reports 0 counts/sizes | Implement real values (W5). |

---

## 6. Summary of all spec edits required

| Spec section | Edit | Severity |
|---|---|---|
| §1.2 | Correct "Single expression attribute name/value token ≤255 bytes" → 255B applies to #name keys only, not :value tokens | **correction** |
| §1.2 | Add: 2MB substitution sum is ~1MB serialized in dynamodb-local; record discrepancy | **correction** |
| §2.8 | Fill in probe results table (all rows from §1 above) | fill-in |
| §3.1 | Remove "approximately" from N-size; state exact formula | **correction** |
| §3.1 | State definitively: no per-item overhead; limit is exactly 409600 | **correction** |
| §3.2 | Add: size-validation-beats-condition is probe-verified | note |
| §4 #2 | Add: IN counts as 1 operator regardless of operands | **addition** |
| §4 #4 | Correct 255B scope (names only) and 2MB sum basis (~1MB serialized) | **correction** |
| §5 #3 | Add: boundaries are inclusive; different error messages for empty S vs B | note |
| §5 #5 | Add: key attrs don't count; error message is misleading | note |
| §6.1 | No change (matches reference) | none |
| §6.2 | Change from "probe → match" to "implement": real immediate ItemCount + W1-sum TableSizeBytes, per-GSI too | **change** |
| §6.3 | Change from "probe → match" to "implement": 16MB cap with wire-byte measurement, UnprocessedKeys spill | **change** |
| §8 #7–9 | Add resolved findings (ListTables LEK, error precedence, same-name GSI) | fill-in |
| §11 | Remove 3 resolved entries; add 3 new divergence entries | update |

---

## 7. Supplemental probe pass (W5/W6) — 2026-08-03

A post-port review found six questions the first pass missed. Four were closed as AWS-doc defaults (recorded inline in the spec, not probed): per-kind 4KB expression-string limits (only `ProjectionExpression` was verified), operator-count composition beyond `=`/`AND`/`IN`, the N-key length confirmation, and list-nested item depth. The remaining two affected W5/W6 implementation contracts and were probed with five new probes appended to `probe_m6c_test.go`.

### W6 — BatchGetItem 16MB (supersedes the first-pass measurement-basis conclusion)

| Probe | Observed | Action |
|---|---|---|
| Exact boundary, wire vs W1 (binary search on per-item size, 100 items) | **W1 item accounting.** s*=167,763: 100×(9+s)=16,777,200 ≤ 16,777,216 exactly; wire model 100×(34+s)=16,779,700 exceeds the cap. **Cap = 16MiB = 16,777,216 bytes** | §6.3 **corrected**: measurement basis is W1 accounting, not wire bytes; W6 reuses W1's `itemSize` walker |
| Pre/post-projection (`ProjectionExpression="pk"` on 100×200KB) | **PRE-projection.** 81 returned (= ⌊16,777,216/204,809⌋), 19 spilled despite the tiny projected response | §6.3: measure the stored item; projection ignored |
| Per-table vs whole-response (2 tables × 50×300KB, ~15MB each, 100 keys) | **Whole-response.** Only 54 items returned total (4 from A + 50 from B) — one global accumulator. Cross-table order arbitrary (B fully served before A despite A sorting first) | §6.3: single accumulator; engine keeps deterministic sorted order — new §11 divergence |
| `UnprocessedKeys` echo shape (request with projection + EAN + ConsistentRead) | **Confirmed.** Spilled `KeysAndAttributes` echoes `ProjectionExpression="#b"`, `ExpressionAttributeNames={#b:big}`, `ConsistentRead=true` | §6.3: echo full `KeysAndAttributes` (first-pass assertion now observed) |

Probe-design note: the first multi-table attempt (60+60 keys) tripped the 100-key request limit (`ValidationException: Too many items requested for the BatchGetItem call`) — the fixed version uses 50+50.

### W5 — DescribeTable reporting (supplemental)

| Probe | Observed | Action |
|---|---|---|
| `IndexSizeBytes` for KEYS_ONLY / INCLUDE / ALL (same 3 items) | **Projection-independent**: all three GSIs report ItemCount=3, IndexSizeBytes=3033 — full-item W1 sum over indexed items | §6.2: GSI size = W1 sum over indexed items regardless of projection |
| Sparse item (no `gk`) | Table count/size grow; all GSI counts/sizes unchanged | §6.2: GSI sums cover indexed items only |
| Overwrite (1011→511) / delete / sparse put | Immediate, exact: 2533, 1522, 1629 — all exact W1 sums | §6.2: stored sizes maintained on every write path; conformance pins overwrite/delete/sparse cases |

### Spec edits from the supplemental pass

| Spec section | Edit | Severity |
|---|---|---|
| §1 table W6 row / §6.3 | Measurement basis corrected from wire bytes to **W1 item accounting** (pre-projection, whole-response, exact 16MiB); W6 depends on W1's walker | **correction** |
| §2.8 | 4 new W6 rows (basis, projection, accumulation, echo) + 3 new W5 rows (projection-independence, sparse, overwrite/delete) | fill-in |
| §6.2 | GSI sizes projection-independent; sparse excluded; overwrite/delete exact | **addition** |
| §3.2 / §4 #1 / §4 #2 / §5 #3 | AWS-doc defaults recorded for the four unprobed low-risk questions | note |
| §11 | New divergence: 16MB spill accumulation order across tables (engine sorted, reference arbitrary) | **addition** |

---

## 8. W8 golden-corpus probe pass — 2026-08-03

Ad-hoc probe (scaffolding not committed) against dynamodb-local 3.3.1 to ground
the §8 #1/#2/#4/#5 conformance expectations. Fixture: the case-4 seed item
(s/n/b/bool/null/ss/ns/bs/l/m).

| Question | Observed |
|---|---|
| BETWEEN with attr type ≠ both bounds' type | **FALSE**, not error (3 shapes) |
| IN with all-mismatched operands | **FALSE**; mismatched operands are skipped, so one matching operand → TRUE |
| `ss IN (:ss)` / `bool IN (:true)` | TRUE |
| Set equality | order-insensitive TRUE; subset FALSE |
| `l = :l` / `m = :m` deep equality | TRUE |
| set/list vs scalar equality; `bool = :n` | FALSE; `<>` forms TRUE |
| Ordering vs non-scalar **attribute** (scalar operand) | FALSE for `< <= > >=` |
| Ordering/BETWEEN with non-scalar **operand** (BOOL/NULL/L/SS) | **ValidationException** "Incorrect operand type for operator or function" — item-independent (fires with missing attr) ⇒ request-time validation; engine fix landed in `expr.Bind` |
| Nested missing paths (`m.nope`, `nope.deep.nest`), scalar descent (`s.deeper`), list past end (`l[5]`) | `=` FALSE, `<>` TRUE |
| `size(nope) = 0`, `contains(nope, :v)` | FALSE |
| Scan/Query filter on missing path | `=` → Count 0, ScannedCount consumed; `<>` → all scanned returned |
| Scan Limit == total | LEK set; resume → empty page, LEK nil (stop-reason contract holds for Scan) |
| Scan Limit=5 over 2 partitions × 5 | pages 5, 5, then empty terminator; no loss/dup across the boundary |

Out-of-scope observation (not W8 work): an NS expression value with
numerically duplicate members (`["1","2","2.0"]`) is rejected by the reference
("Input collection contains duplicates") while the engine dedups at set
construction (deliberate M0 design, parent spec §5).

**Spec changes from this pass:** none beyond §8 itself — the two mismatches it
surfaced (UpdateTable precedence, ordering-operand validation) were fixed, not
documented as divergences. The UpdateTable precedence fix (§8 #8) landed and is
pinned dual-target at the message level (the case asserts the existing-index
error via "already exists") because dynamodb-local reports that error as
ValidationException while the engine uses ResourceInUseException (real-AWS
behavior).
