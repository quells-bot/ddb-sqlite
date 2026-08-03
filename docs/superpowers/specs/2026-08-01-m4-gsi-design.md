# ddb-sqlite M4 — Global Secondary Index Design

**Date:** 2026-08-01
**Status:** Approved (brainstorming) → pending implementation plan
**Revision:** 2026-08-01 (same day) — supplementary probe round G20–G29 after spec review: ExclusiveStartKey shape validation, write-time GSI key type validation, composite-GSI sort-key absence, empty GSI key values, CreateTable validation edges. Sections 2.1–2.3, 4.1, 6.2, 7.2, 8.1, 10.3, 11.1 updated.
**Parent spec:** `docs/superpowers/specs/2026-07-31-ddb-sqlite-design.md` (§3.3, §3.4, §6.3, §11 M4 milestone)
**Prerequisite:** M3 complete — `Query`/`Scan` with faithful pagination (`Limit` as read budget, `ExclusiveStartKey`/`LastEvaluatedKey` stop-reason rule, `ScannedCount`/`Count`), `FilterExpression` wiring, `ExtractKeyCondition`, `translateSortCond`, `storage.Query`/`storage.Scan`, `ErrGsiNotFound` sentinel (M3 rejects `IndexName`), all M3 conformance cases green against both targets.

## 1. Overview & goal

M4 adds **Global Secondary Index (GSI)** support: create-time GSI definition, write-triggered GSI maintenance, and GSI `Query`/`Scan` with read-time projection. `IndexName` — rejected with `ValidationException` in M3 — becomes valid.

M4 delivers:

- `ddb` — `GlobalSecondaryIndex`/`Projection` input types on `CreateTableInput`; GSI defs on `TableDescription`; `IndexName` on `QueryInput`/`ScanInput`; GSI `Query`/`Scan` (partition seek + sort-key range on the GSI index table, composite keyset resume cursor); read-time projection trimming (`ALL`/`KEYS_ONLY`/`INCLUDE`); `ConsistentRead` rejection on GSI queries and scans; updated `validateSelect` (`ALL_PROJECTED_ATTRIBUTES`, `ALL_ATTRIBUTES` on non-ALL GSIs); write-triggered GSI maintenance on `PutItem`/`UpdateItem`/`DeleteItem` with write-time GSI key type validation and GSI-aware `ExclusiveStartKey` validation.
- `internal/storage` — GSI index table DDL (`CreateGsiTable`); GSI catalog CRUD (`InsertGsiDef`/`GetGsiDefs`/`DeleteGsiDefs`); `QueryGSI`/`ScanGSI` (JOIN + keyset cursor); `UpsertGsiRow`; `PutItem` extended to return the new `data_id`.
- `awsdynamodb` — `CreateTable` accepts `GlobalSecondaryIndexes`; `Query`/`Scan` accept `IndexName`; `DescribeTable` returns GSIs; M3 `IndexName`/GSI rejections removed.
- Conformance cases 38–68 (§10.3), gated on both targets.

**Scope boundary:** create-time GSIs only. `UpdateTable` (GSI add/remove on an existing table) stays in M6, per the parent spec's milestone list. LSI (local secondary indexes) is a v1 non-goal.

## 2. Probe-verified semantics & methodology

GSI semantics are under-documented in the AWS reference. Following the M3 precedent (`limit_probe_test.go`), a throwaway probe — `awsdynamodb/gsi_probe_test.go` — drives `dynamodb-local:3.3.1` directly via the conformance harness's `TestMain`-managed `localClient` (a real AWS SDK v2 `*dynamodb.Client`). The probe is env-gated (`DDBSQLITE_CONF_TARGET=dynamodb-local|all`); it skips under the default `go test ./...`. It is deleted once the M4 conformance cases are ported.

**Table & seed.** Composite primary key: `pk` HASH (type S), `sk` RANGE (type S). Three GSIs: `gsi-all` (`gsi_pk` HASH S, `gsi_sk` RANGE S, projection ALL), `gsi-keys` (`gsi_pk` HASH S, no sort, KEYS_ONLY), `gsi-incl` (`gsi_pk` HASH S, `gsi_sk` RANGE S, INCLUDE `[proj1, proj2]`). Five items:

| pk | sk | gsi_pk | gsi_sk | other |
|---|---|---|---|---|
| A | a | G1 | s1 | proj1=foo, proj2=bar, extra=baz |
| B | b | G1 | s2 | proj1=qux, extra=quux |
| C | c | G1 | s1 | (none — shares gsi_pk/gsi_sk with A) |
| D | d | (absent) | — | (sparse: absent from all GSIs) |
| E | e | G2 | s3 | proj1=alpha |

Each finding below cites the probe subtest that established it. The probe ran in two rounds — G0–G19 (initial) and G20–G29 (supplementary, after spec review) — both in `awsdynamodb/gsi_probe_test.go`. The probe output (all 30 subtests green) is the ground truth the spec encodes.

### 2.1 Findings

| Finding | Result | Probe test |
|---|---|---|
| **GSI key uniqueness** | GSI keys are **not unique**. Items A and C share `gsi_pk=G1, gsi_sk=s1`; both are returned. | G3 `NonUniqueGSIKey`: `Query gsi-all WHERE gsi_pk=G1 AND gsi_sk=s1` returned pks `[A C]`. |
| **LEK composition (Query)** | `LastEvaluatedKey` carries **both** the GSI index keys and the table keys. | G4 `LEKComposition`: `Query gsi-all, gsi_pk=G1, Limit=1` → `LEK = {gsi_pk="G1", gsi_sk="s1", pk="C", sk="c"}`. |
| **LEK composition (Scan)** | GSI `Scan` LEK carries the same four keys. | G19 `ScanLEK`: `Scan gsi-all, Limit=1` → `LEK = {gsi_pk="G1", gsi_sk="s1", pk="C", sk="c"}`. |
| **Sparse GSIs** | An item missing the GSI partition key is absent from that GSI's index. | G2 `Sparse`: `Query gsi-all, gsi_pk=G2` returned `[E]` only (D absent). G17 `UpdateRemovesGSIKey`: after `REMOVE gsi_pk` on B, `Query gsi_pk=G1` returned `[A C]` (B gone). |
| **ConsistentRead rejection** | `ConsistentRead=true` on a GSI `Query` is rejected. | G6 `ConsistentReadRejected`: `Query gsi-all, ConsistentRead=true` → `ValidationException: Consistent reads are not supported on global secondary indexes`. |
| **Projection KEYS_ONLY** | Returns GSI key attrs + table key attrs only. | G7 `ProjectionKeysOnly`: `Query gsi-keys, gsi_pk=G1` → every item attrs `[gsi_pk, pk, sk]`. |
| **Projection INCLUDE** | Returns GSI keys + table keys + projected attrs **if present**; absent projected attrs are omitted (not null). | G8 `ProjectionInclude`: item A attrs `[gsi_pk, gsi_sk, pk, sk, proj1, proj2]`; item C (no proj1/proj2) attrs `[gsi_pk, gsi_sk, pk, sk]`. |
| **Projection ALL** | Returns all item attributes. | G9 `ProjectionAll`: `Query gsi-all, gsi_pk=G1, gsi_sk=s1` → item A attrs `[extra, gsi_pk, gsi_sk, pk, proj1, proj2, sk]`. |
| **Select=ALL_PROJECTED_ATTRIBUTES** | Valid on a GSI; returns the GSI's projected attrs (same as the projection). | G10 `SelectAllProjectedAttributes`: `Query gsi-incl, Select=ALL_PROJECTED_ATTRIBUTES` → item attrs match INCLUDE projection. |
| **Select=ALL_ATTRIBUTES on non-ALL GSI** | **Rejected.** | G11 `SelectAllAttributesOnGSI`: `Query gsi-keys, Select=ALL_ATTRIBUTES` → `ValidationException: Select type ALL_ATTRIBUTES is not supported for global secondary index gsi-keys because its projection type is not ALL`. |
| **Non-GSI attr in KeyConditionExpression** | Rejected. | G14 `NonGSIAttrInKeyCond`: `Query gsi-all, KeyConditionExpression="pk = :v"` → `ValidationException: Query condition missed key schema element`. |
| **UpdateItem changes GSI key** | Item moves to the new GSI partition. | G16 `UpdateMovesGSIKey`: `SET gsi_pk=G3` on E → `Query gsi_pk=G2` empty, `Query gsi_pk=G3` returned `[E]`. |
| **UpdateItem removes GSI key** | Item becomes sparse (absent from GSI). | G17 `UpdateRemovesGSIKey`: `REMOVE gsi_pk, gsi_sk` on B → `Query gsi_pk=G1` returned `[A C]` (B excluded). |
| **DeleteItem removes from GSI** | Item disappears from the GSI. | G18 `DeleteRemovesFromGSI`: `DeleteItem A` → `Query gsi_pk=G1, gsi_sk=s1` returned `[C]` only. |
| **GSI Scan** | Returns all indexed items (sparse excluded). | G12 `Scan`: `Scan gsi-all` returned pks `[A B C E]` (D excluded); LEK nil (exhausted). |
| **Tied sort-key order** | Unspecified — items sharing a GSI sort key return in no guaranteed order. | G1 `BasicQuery`: `Query gsi_pk=G1` returned `[A B C]`; A and C share `gsi_sk=s1` and their relative order is not caller-controllable. Any stable tiebreak is faithful. |
| **begins_with on GSI sort key** | Works like base-table begins_with. | G13 `BeginsWith`: `Query gsi_pk=G1 AND begins_with(gsi_sk, "s")` returned `[A B C]`. |
| **Pagination resume** | LEK with four keys resumes correctly. | G5 `Pagination`: `Query gsi_pk=G1, Limit=2` walked 2 rounds, recovered `[A B C]`, terminated on LEK nil. Round 1 LEK `{gsi_pk="G1", gsi_sk="s1", pk="A", sk="a"}`; round 2 LEK nil. |
| **DescribeTable GSI order** | Unspecified — not creation order, not alphabetical. | G0 `DescribeTable`: GSIs returned as `[gsi-incl, gsi-keys, gsi-all]` (creation order was all, keys, incl). |
| **Composite GSI sort-key absence** | An item with the GSI partition key but **missing the GSI sort key** is write-accepted but absent from the index — from both GSI `Query` and `Scan`. | G20 `MissingGsiSortKey`: PutItem X (`gsi_pk=G1`, no `gsi_sk`) accepted; `Query gsi_pk=G1` and `Scan gsi-all` both exclude X. |
| **Write-time GSI key type validation** | A GSI key attr whose scalar type differs from the declared index key type → `ValidationException`; the write fails **atomically** (nothing is stored). | G21 `GsiKeyTypeMismatchPut`: `gsi_pk=N` (declared S) → `Type mismatch for Index Key`; subsequent `GetItem` finds nothing. G23 `GsiKeyTypeMismatchUpdate`: `SET gsi_pk=:n` → same rejection. |
| **Non-scalar GSI key attr** | L/M/BOOL/SS/NULL as a GSI key attr → `ValidationException: Invalid attribute value type`. | G22 `GsiKeyNonScalar` (L, BOOL, SS, NULL all rejected). |
| **Empty S/B GSI key value** | Empty string as a GSI key attr → `ValidationException` (key attrs must be non-empty). | G29 `EmptyGsiKeyValue`: `The AttributeValue for a key attribute cannot contain an empty string value`. |
| **ConsistentRead on GSI Scan** | Rejected, same as Query. | G24 `ConsistentReadGsiScan`: `ValidationException: Consistent reads are not supported on global secondary indexes`. |
| **ESK shape (GSI Query)** | `ExclusiveStartKey` must carry **exactly** the union of the table key attrs and the GSI key attrs — no more, no fewer. | G25 `EskShapes`: table-keys-only, GSI-keys-only, and union-plus-extra all rejected (`The provided starting key is invalid`); the full 4-key union accepted. |
| **ESK GSI partition mismatch** | Rejected — the resume must stay within the queried GSI partition. | G25: `The provided starting key does not match the range key predicate`. |
| **Stale GSI ESK** | Accepted; resume proceeds. | G26 `StaleEskGsiQuery`: resuming with a deleted row's LEK returned all remaining partition items — consistent with restart-from-beginning. |
| **Duplicate AttributeDefinition** | Rejected. | G27 `CreateTableValidation`: `Cannot have two attributes with the same name`. (M1's `analyzeCreateTable` silently dedups — an M1 gap this milestone fixes.) |
| **Projection validation** | INCLUDE naming any key attr (table or GSI) → rejected; ALL with `NonKeyAttributes` → rejected. | G27. |
| **Index name validation** | 3–255 chars, `[a-zA-Z0-9_.-]` only. | G27: `"bad name"` and `"ab"` (too short) rejected. |
| **DESC order with tied GSI sort keys** | Unspecified, same as ASC ties. | G28 `DescOrderTiedSortKeys`: tie broken deterministically but not caller-controllable. Any stable tiebreak is faithful. |

### 2.2 Corrections to the parent spec

The probe disproved or filled in four claims in the parent design spec (§3.3, §3.4):

1. **GSI index non-uniqueness (parent §3.3 said UNIQUE).** The parent spec's GSI index table DDL specified a `UNIQUE` index on `(hash, range)`. GSI keys are not unique — two items can share a GSI key (finding above, probe G3). The M4 DDL (§4) uses a non-unique index.
2. **`ON DELETE CASCADE` on the GSI FK (parent §3.3 omitted it).** `PutItem`/`UpdateItem` use `INSERT OR REPLACE`, which deletes the old data row before inserting the new one. Without `ON DELETE CASCADE`, the FK from the GSI table to the data table would block the delete. `DeleteItem` also relies on it. The M4 DDL (§4) adds `ON DELETE CASCADE`.
3. **`Select=ALL_ATTRIBUTES` rejected on non-ALL GSIs (parent spec did not specify).** The probe (G11) proved `ALL_ATTRIBUTES` is rejected on a `KEYS_ONLY` GSI. M4's `validateSelect` (§6.4) encodes this.
4. **LEK carries both index keys and table keys (parent spec did not detail).** The probe (G4/G19) proved the LEK for a GSI `Query`/`Scan` carries the GSI keys plus the table keys — four attributes for a composite-PK + composite-GSI-key table. M4's LEK construction (§7) encodes this.

The supplementary round (G20–G29) corrected or filled in four claims in the first draft of *this* spec:

5. **GSI `ExclusiveStartKey` validation must be GSI-aware (the draft contradicted itself).** §7.1's LEK carries table + GSI keys, but the draft's §7.2 validated `ExclusiveStartKey` with the base-table `validateKey`, which requires *exactly* the table key attrs — it would have rejected the spec's own LEK on resume. The probe (G25) shows DynamoDB requires exactly the union of table and GSI key attrs. §7.2 is rewritten accordingly.
6. **Write-time GSI key type validation (the draft omitted it).** The probe (G21–G23, G29) shows type-mismatched, non-scalar, or empty GSI key attrs are rejected at write time, atomically — on `PutItem` and `UpdateItem` alike. §8.1 adds this validation; §2.3 shows why it cannot be delegated to SQLite.
7. **A composite GSI indexes an item only when both key attrs are present (the draft sparsified on the partition key only).** The probe (G20) shows an item with the partition key but no sort key is write-accepted yet absent from the index — Query and Scan alike. §8.1 encodes the both-present rule; the §4.1 `range` column becomes NOT NULL.
8. **Duplicate `AttributeDefinition` rejected (M1 gap).** The probe (G27) shows DynamoDB rejects duplicate attribute definitions (`Cannot have two attributes with the same name`); M1's `analyzeCreateTable` silently dedups via map. §6.2 adds the rejection.

### 2.3 Why storage cannot enforce GSI key types: SQLite STRICT affinity coercion

A scratch check against the project's SQLite build (`modernc.org/sqlite`) settled where write-time GSI key type validation must live. Per the SQLite docs ([stricttables.html §2(3)](https://sqlite.org/stricttables.html)), a STRICT table applies **column-affinity coercion before type checking** and only rejects *non-lossless* conversions. Measured on this build:

| Insert | Result |
|---|---|
| REAL `1.5` → TEXT column | accepted, silently stored as text `'1.5'` |
| INTEGER `42` → TEXT column | accepted, stored as `'42'` |
| TEXT `'123'` → REAL column | accepted, silently stored as `123.0` |
| TEXT `'abc'` → REAL column | `SQLITE_CONSTRAINT_DATATYPE` (3091) |
| TEXT → BLOB, BLOB → TEXT, BLOB → REAL | `SQLITE_CONSTRAINT_DATATYPE` (3091) |

So without Go-side validation, an N-typed value written into an S-typed GSI key column (or the string `'123'` into an N-typed one) would **silently corrupt the index**, and the remaining mismatches would surface as opaque internal errors — where real DynamoDB rejects the write with `ValidationException` (§2.1, G21–G23). There is no PRAGMA that tightens this (the only STRICT-related pragma, `writable_schema`, *disables* enforcement). `ddb` validates GSI key attr types on the write path (§8.1).

The same scratch check verified the §4.1 FK design: `INSERT OR REPLACE` on the data table fires `ON DELETE CASCADE` on GSI tables under `foreign_keys=ON`, and REPLACE assigns a new rowid — which is why §8.1 upserts GSI rows *after* the data write, using the newly returned `data_id`.

## 3. Architecture approach: JOIN + keyset cursor

Selected approach (over two-step seek-and-fetch / denormalized columns): storage executes GSI `Query`/`Scan` as a single `SELECT … JOIN` (GSI index table JOINed to the data table on `data_id = id`) with a composite keyset resume cursor.

The decisive factor is GSI key non-uniqueness (§2.1). Base-table sort keys are `UNIQUE`, so M3's `range > lek_range` resume never skips a row. GSI sort keys are not unique — two items can share `range = s1` — so `range > lek_range` alone would skip the second. The composite cursor `(range, data_id)` with the predicate `(range > ? OR (range = ? AND data_id > ?))` resumes exactly after the LEK item. `data_id` is `INTEGER PRIMARY KEY` (= the rowid), so a non-unique index on `(hash, range)` implicitly orders by `(hash, range, data_id)` — a stable, deterministic tiebreak for equal sort keys at no extra cost.

Rejected alternatives:

- **Two-step (index seek → batch fetch).** Step 1: `SELECT data_id FROM gsi_tbl WHERE … ORDER BY range, data_id LIMIT ?`; step 2: `SELECT id, data FROM data_tbl WHERE id IN (…)`; re-sort blobs in Go by the step-1 order. Simpler SQL, but two round-trips, Go-side re-sort, and storage must return `[]int64` ids — breaking the "storage returns blobs" contract M3 established. It pays the same `(range, data_id)` ordering cost with an extra round-trip and a broken contract.
- **Denormalize GSI keys into data-table columns + SQLite indexes.** Add GSI key columns to the data table; SQLite auto-maintains indexes. No separate GSI tables, no JOIN, no manual maintenance. But it deviates from the parent spec's separate-table architecture, `ALTER TABLE ADD COLUMN` on a STRICT table is messy, and it complicates M6 `UpdateTable` (GSI add/remove would require column add/drop on a populated table). The parent spec already rejected this shape.

The chosen approach honors the parent spec's separate-table design, keeps storage returning blobs, and makes the non-unique-sort-key tiebreak almost free.

## 4. Storage: GSI index tables & catalog

### 4.1 GSI index table DDL (corrected)

The SQLite table name is `ddb_<16hex-SHA256(table)>_<16hex-SHA256(gsi)>`. Key column affinity depends on the GSI key's DynamoDB type (same `sqliteType` mapping as the data table: S→TEXT, N→REAL, B→BLOB).

```sql
CREATE TABLE ddb_<tablehash>_<gsihash> (
  data_id INTEGER NOT NULL PRIMARY KEY REFERENCES ddb_<tablehash>(id) ON DELETE CASCADE,
  hash    <TYPE> NOT NULL,                 -- TYPE = TEXT (S) | REAL (N) | BLOB (B)
  range   <TYPE> NOT NULL                  -- present iff the GSI has a sort key
) STRICT;
CREATE INDEX ddb_<tablehash>_<gsihash>_idx ON ddb_<tablehash>_<gsihash>(hash, range);
-- partition-only GSI: no range column; index on (hash) only
```

Key design points:

1. **`data_id INTEGER PRIMARY KEY`** aliases the rowid. A non-unique index on `(hash, range)` then implicitly orders by `(hash, range, rowid)` = `(hash, range, data_id)` — the stable tiebreak for equal sort keys. No separate tiebreak column needed.
2. **Non-unique index** — no `UNIQUE` keyword. GSI keys are not unique (§2.1, probe G3).
3. **`ON DELETE CASCADE`** (correction to parent §3.3). `PutItem`/`UpdateItem` use `INSERT OR REPLACE`, which deletes the old data row; CASCADE auto-cleans the old GSI rows. `DeleteItem` also relies on it. `foreign_keys=ON` is already set as a pragma (M1 `store.go`).
4. The GSI table stores only keys + the FK to the data row. Projection is applied in Go at read time (parent §3.4(4)).
5. **`range` is NOT NULL.** An item is indexed only when *all* GSI key attrs are present (§8.1, probe G20), so a NULL range is unreachable. Column types alone cannot enforce DynamoDB's key-type rules — STRICT affinity coercion silently accepts lossless mismatches (§2.3); `ddb` validates key attr types in Go before any storage write (§8.1).

`GsiTableName(table, gsi)` = `TableName(table) + "_" + hex.EncodeToString(SHA256(gsi)[:8])`.

### 4.2 GSI catalog CRUD

The `ddb_gsi_defs` catalog table is already bootstrapped (M1 `store.go`). M4 adds CRUD methods:

```go
// GsiDef mirrors a ddb_gsi_defs catalog row. Projected is the raw JSON
// attr list (INCLUDE); nil otherwise.
type GsiDef struct {
    Name            string
    Hash            string
    Range           string
    HashType        string
    RangeType       string
    ProjectionType  string        // "ALL" | "KEYS_ONLY" | "INCLUDE"
    Projected       []string      // INCLUDE only; nil otherwise
}

func (s *Store) InsertGsiDef(tx *sql.Tx, tableID int64, def GsiDef) error
func (s *Store) GetGsiDefs(tx *sql.Tx, tableID int64) ([]GsiDef, error)
func (s *Store) GetGsiDef(tx *sql.Tx, tableID int64, name string) (GsiDef, error)
func (s *Store) DeleteGsiDefs(tx *sql.Tx, tableID int64) error
```

`TableDef` gains a `GSIs []GsiDef` field. `GetTableDef` loads GSI defs alongside the table def (one extra query — GSI defs are few). `DeleteTable` cascade-deletes GSI catalog rows via `DeleteGsiDefs` (the FK on `table_id` also enforces this, but the explicit delete keeps the catalog clean even if CASCADE behavior varies).

`InsertGsiDef` stores `Projected` as a JSON array (matching the catalog's `projected TEXT` column).

### 4.3 GSI table DDL generation

```go
// CreateGsiTable generates and executes the per-GSI index table DDL. The
// data_id column is INTEGER PRIMARY KEY (aliases rowid) with ON DELETE
// CASCADE to the data table. hash/range columns use the GSI key types.
func (s *Store) CreateGsiTable(tx *sql.Tx, tableDef TableDef, gsi GsiDef) error
```

Reuses `sqliteType` (M1) for column affinity. Partition-only GSI omits the `range` column.

## 5. Storage: GSI Query/Scan

Both methods follow the M3 contract: opaque `[]byte` blobs out, raw column values in, no `attrval`/`num` imports. `ddb` translates `attrval.Value` operands to column-space Go values via the existing `keyValue()` (S→string, N→float64, B→[]byte).

### 5.1 Storage-level GSI sort-key condition & resume cursor

`ddb` translates the `expr.SortKeyCond` to the same `storage.SortKeyCond` M3 uses, with one addition — a **composite resume cursor**:

```go
// GsiResume carries the (range, data_id) keyset cursor for GSI resume. For a
// partition-only GSI (no sort key), Range is nil and only DataID is used.
type GsiResume struct {
    Range  any    // the LEK's GSI sort-key value; nil for partition-only GSIs
    DataID int64  // the LEK item's data_id (rowid)
}
```

Base-table sort keys are `UNIQUE`, so M3's `ResumeAfter` (a single sort-key value) suffices. GSI sort keys are not unique (§2.1), so resume needs the `data_id` tiebreak. `ddb` resolves the LEK's table key → `data_id` via the existing `storage.GetItem` (which already returns the rowid — M3 extension), same pattern as M3 Scan resume.

`QueryGSI` takes a `*GsiResume` (nil on the first page) instead of M3's `ResumeAfter` field on `SortKeyCond`:

```go
// QueryGSI selects rows for one GSI partition key value, ordered by the GSI
// sort key (then data_id for stable tiebreak), and returns their item blobs
// via a JOIN to the data table. sortCond is nil for a partition-only seek
// with no resume; when resuming a partition-equality-only GSI Query, sortCond
// is non-nil with Op == "" and only the resume cursor set. scanForward
// controls ASC vs DESC. limit <= 0 means unlimited.
func (s *Store) QueryGSI(tx *sql.Tx, table, gsi string, hashVal any,
    sortCond *SortKeyCond, resume *GsiResume, scanForward bool, limit int) ([][]byte, error)
```

Generated SQL shape (sort-key GSI, with resume):

```sql
SELECT d.data
FROM ddb_<th>_<gh> g JOIN ddb_<th> d ON g.data_id = d.id
WHERE g.hash = ?
  AND g.range < ?                             -- sortCond.Op predicate; omitted when nil
  AND (g.range > ? OR (g.range = ? AND g.data_id > ?))  -- resume; omitted when nil
ORDER BY g.range ASC, g.data_id ASC           -- DESC, data_id DESC when !scanForward
LIMIT ?                                        -- omitted when limit <= 0
```

`BETWEEN` emits `g.range >= ? AND g.range <= ?`. `BEGINS_WITH` emits `g.range >= ? AND g.range < ?` (half-open; `ddb` computes the lexicographic successor, same as M3). The resume predicate is appended to whatever the sort condition generated, combining bounds like M3's `ResumeAfter`.

For partition-only GSIs (no `range` column), resume is simply `g.data_id > ?`; there is no `ORDER BY range`, only `ORDER BY g.data_id`.

`ScanGSI` — full GSI scan by `data_id` order, JOINed:

```go
// ScanGSI selects all rows in the GSI in data_id order and returns their item
// blobs. afterID > 0 resumes after that data_id. limit <= 0 means unlimited.
func (s *Store) ScanGSI(tx *sql.Tx, table, gsi string, afterID int64, limit int) ([][]byte, error)
```

```sql
SELECT d.data FROM ddb_<th>_<gh> g JOIN ddb_<th> d ON g.data_id = d.id
WHERE g.data_id > ?        -- only when afterID > 0
ORDER BY g.data_id
LIMIT ?                    -- omitted when limit <= 0
```

`ScanGSI` does not support parallel segments in M4 (GSI `Scan` with `Segment`/`TotalSegments` is rarely used in tests; the probe did not cover it). When `IndexName` is set on a `Scan`, `TotalSegments > 1` is rejected with `ErrValidation` in M4 — a deliberate scope cut, not a reference-faithful behavior. `TotalSegments == 0` (non-parallel) is the only supported GSI scan. Conformance does not assert GSI parallel scan.

### 5.2 Why storage returns only blobs

Same rationale as M3 §3.4: `ddb` builds `LastEvaluatedKey` by decoding the last scanned item's blob (which always contains the GSI keys and the table keys — probe-verified G4/G19). The `data_id` is a storage internal; it never crosses the boundary in the output. `ddb` passes `GsiResume`/`afterID` *into* storage; neither appears in any output.

## 6. `ddb` engine API surface

### 6.1 New / changed input types

```go
// Projection describes which attributes a GSI projects.
type Projection struct {
    Type             string   // "ALL" | "KEYS_ONLY" | "INCLUDE"
    NonKeyAttributes []string // INCLUDE only; 1–100 attrs, no key attrs
}

// GlobalSecondaryIndex names one GSI declared at CreateTable.
type GlobalSecondaryIndex struct {
    IndexName  string
    KeySchema  []KeySchemaElement // 1 HASH, 0–1 RANGE
    Projection Projection
}

// GlobalSecondaryIndexDescription is DescribeTable's view of a GSI.
type GlobalSecondaryIndexDescription struct {
    IndexName  string
    KeySchema  []KeySchemaElement
    Projection Projection
}
```

`CreateTableInput` gains `GlobalSecondaryIndexes []GlobalSecondaryIndex`. `TableDescription` gains `GlobalSecondaryIndexes []GlobalSecondaryIndexDescription`. `QueryInput`/`ScanInput` gain `IndexName string`.

### 6.2 GSI validation in `analyzeCreateTable`

The M1 rule "AttributeDefinitions must match the key schema attributes" is **lifted**. `AttributeDefinitions` may now include attrs beyond the table's own key attrs (GSI key attrs need definitions). The new rules:

1. Every key attr (table HASH/RANGE + every GSI HASH/RANGE) must have an `AttributeDefinition` with a valid type (S/N/B).
2. Unreferenced `AttributeDefinitions` are still rejected (DynamoDB rejects surplus definitions).
3. Each GSI: `IndexName` 3–255 chars from `[a-zA-Z0-9_.-]` (probe G27 — the same rule DynamoDB applies to table names; M1 validates table names only for non-empty, a pre-existing divergence deferred to M6, but GSI names are validated from the start); ≤20 GSIs per table (AWS limit); unique name within the table; `KeySchema` = exactly 1 HASH + 0–1 RANGE; HASH and RANGE can't be the same attr.
4. `Projection.Type=ALL` → `NonKeyAttributes` empty (probe G27). `KEYS_ONLY` → empty. `INCLUDE` → 1–100 non-key attrs, none of which are key attrs — table keys *or* this GSI's keys (probe G27). `NonKeyAttributes` deduped (same canonical dedup as sets, though these are plain strings).
5. A GSI key attr may overlap with the table's key attrs (e.g., GSI partition key = table partition key). Valid.
6. `ProvisionedThroughput` on a GSI is accepted-and-ignored (billing out of scope).
7. Duplicate `AttributeDefinitions` (same attribute name twice) are rejected (probe G27: `Cannot have two attributes with the same name`). This also fixes an M1 gap: M1's `analyzeCreateTable` builds the types map silently, deduping duplicates. The check applies with or without GSIs.

### 6.3 New sentinel error

`ErrGsiNotFound` already exists (M3). It is returned when `IndexName` is non-empty but the table has no such GSI. The adapter maps it to `ValidationException` (already wired in M3 `mapError`).

### 6.4 `Select` validation (updated `validateSelect`)

M3's `validateSelect` accepts `""`/`ALL_ATTRIBUTES`/`COUNT` and rejects `SPECIFIC_ATTRIBUTES`/`ALL_PROJECTED_ATTRIBUTES`. M4 extends it with GSI awareness — the function now takes the GSI's projection type (empty for a base table):

```go
// validateSelect normalizes "" to "ALL_ATTRIBUTES". GSI-aware: on a non-ALL
// GSI, ALL_ATTRIBUTES is rejected; ALL_PROJECTED_ATTRIBUTES is valid only on
// a GSI. COUNT is always valid. SPECIFIC_ATTRIBUTES needs ProjectionExpression
// (v1 non-goal).
func validateSelect(s string, gsiProjection string) (string, error)
```

- `""` / `ALL_ATTRIBUTES`: valid on a base table OR a GSI with `Projection=ALL`. On a `KEYS_ONLY`/`INCLUDE` GSI → `ErrValidation` (probe G11).
- `ALL_PROJECTED_ATTRIBUTES`: valid only when `gsiProjection != ""` (GSI-only). On a base table → `ErrValidation`.
- `COUNT`: always valid.
- `SPECIFIC_ATTRIBUTES`: rejected (needs `ProjectionExpression`, v1 non-goal).

### 6.5 Operation flow — GSI `Query` (delta from M3 §4.5)

```
1.  BeginTx
2.  GetTableDef → ErrTableNotFound if absent
3.  If IndexName != "":
      a. Load GSI def (from def.GSIs) → ErrGsiNotFound if absent
      b. If ConsistentRead → ErrValidation ("consistent reads not supported on GSIs")
      c. gsiHash = GSI.Hash; gsiRange = GSI.Range; gsiProjection = GSI.ProjectionType
    else:
      gsiHash = def.Hash; gsiRange = def.Range; gsiProjection = ""
4.  Validate Select (validateSelect with gsiProjection)
5.  Parse KeyConditionExpression; union Refs with FilterExpression's Refs; CheckUnused once; Bind both
6.  ExtractKeyCondition(boundKeyCond, gsiHash, gsiRange) → KeyCondition
      (uses the GSI's key names, not the table's — probe G14)
7.  Validate: partition value type matches GSI.HashType; sort value type matches GSI.RangeType
8.  If begins_with on N GSI sort key → ErrValidation
9.  ValidateFilterKeys(boundFilter, keyAttrsForFilter) where keyAttrsForFilter =
      [def.Hash, def.Range, gsiHash, gsiRange] (filter can't reference table OR GSI keys)
10. Translate KeyCondition to storage space (keyValue on GSI key values)
11. Build resume cursor from ExclusiveStartKey (§7.2)
12. store.QueryGSI(tx, table, gsiName, hashVal, sortCond, resume, scanForward, limit)
13. For each blob: unmarshal → Item; ScannedCount++
14. Apply filter: Eval(item) → keep (Count++) / discard
15. Build LastEvaluatedKey from the last scanned item (GSI keys + table keys) iff ScannedCount == Limit
16. Apply projection trimming (§6.6) to each returned Item
17. If Select == COUNT: drop Items, keep counts + LEK
18. Commit
```

Base-table `Query`/`Scan` (IndexName == "") follow the M3 flow unchanged, except step 4 calls the updated `validateSelect(s, "")`.

### 6.6 Projection trimming

Applied in Go after fetching the full blob, per parent spec §3.4(4). The set of attributes to keep:

- `ALL`: all attributes (no trimming).
- `KEYS_ONLY`: GSI key attrs + table key attrs.
- `INCLUDE`: GSI keys + table keys + `NonKeyAttributes` (attrs absent from the item are omitted — probe G8; no null padding).
- `Select=COUNT`: items dropped (counts/LEK retained).
- `Select=ALL_PROJECTED_ATTRIBUTES`: trims to the GSI's projection (same as the default GSI behavior for ALL/KEYS_ONLY/INCLUDE).

Implemented as a `projectItem(item Item, keep map[string]bool) Item` helper that copies only `keep` attributes present in the item.

### 6.7 GSI `Scan` flow

Same delta as GSI `Query` minus the key-condition steps: load GSI def; `ConsistentRead` check (probe-verified on Scan — G24, same exception as Query); `validateSelect` with GSI projection; `store.ScanGSI`; projection trimming; LEK = GSI keys + table keys. `FilterExpression` validation excludes table and GSI key attrs.

## 7. Pagination mechanics

M3's pagination contract (§6.5 of the parent spec, §5 of the M3 spec) carries over unchanged: `Limit` is a read budget applied before `FilterExpression`; `ScannedCount`/`Count` reported separately; `LEK` set iff `ScannedCount == Limit` (including the `Limit == available` trailing-empty-page case); `Limit=0` rejected by the adapter; `Limit < 0` rejected by the engine. M4 adds GSI-specific resume and LEK composition.

### 7.1 LEK construction for GSI Query/Scan

`LEK` is the key of the last *scanned* item, built by decoding the last scanned item's blob. For a GSI, it carries **both** the GSI index keys and the table keys (probe G4/G19):

```
LEK = { gsiHash: <val>, gsiRange: <val>, tableHash: <val>, tableRange: <val> }
```

For a partition-only GSI (no GSI sort key), `gsiRange` is omitted. For a partition-only table, `tableRange` is omitted. The LEK is exactly the attributes a subsequent `ExclusiveStartKey` needs to resume.

### 7.2 GSI Query resume from ExclusiveStartKey

The caller passes the previous LEK (four keys) as `ExclusiveStartKey`. `ddb`:

1. Validates `ExclusiveStartKey` against the **union of the table's key attrs and the GSI's key attrs** (deduped when they overlap — the case-58 shape where a GSI key is also a table key). The ESK must carry exactly that union: every attr present and type-correct, no extras (probe G25 — table-keys-only, GSI-keys-only, and union-plus-extra are all rejected with "The provided starting key is invalid"). This is GSI-aware validation, **not** the base-table `validateKey` (which requires exactly the table key attrs and would reject the four-key LEK §7.1 constructs — the first draft's self-contradiction, §2.2(5)). The GSI partition is implied by the `KeyConditionExpression`; a mismatch between the `ExclusiveStartKey`'s GSI key and the `KeyConditionExpression`'s GSI partition is a `ValidationException` (probe G25 — the resume must be within the same GSI partition).
2. Resolves the table key → `data_id` via `store.GetItem` (one indexed point lookup, same as M3 Scan resume).
3. Extracts the GSI sort-key value from `ExclusiveStartKey` (for sort-key GSIs).
4. Builds `GsiResume{Range: gsiSortVal, DataID: dataID}` and passes it to `store.QueryGSI`.

A stale `ExclusiveStartKey` (row deleted since the prior page): `store.GetItem` returns "not found," and the GSI Query starts from the beginning of the GSI partition (no resume bound). Probe-verified (G26): dynamodb-local accepts a stale GSI resume key; the observed output is consistent with restart-from-beginning.

### 7.3 GSI Scan resume

`Scan` has no key condition, so resume is by `data_id`. `ddb` resolves `ExclusiveStartKey` (table key) → `data_id` via `store.GetItem`, passes `afterID` to `store.ScanGSI`. Stale key → scan from the beginning.

## 8. Write-triggered GSI maintenance

For `PutItem`/`UpdateItem`, `ddb` recomputes each GSI's key attributes from the post-write item, validates them (type/scalar/non-empty), and only then issues the storage writes. All maintenance runs in the same `*sql.Tx` as the data write (the serialization unit); a validation failure aborts before any write, so rejection is atomic.

### 8.1 PutItem / UpdateItem

Both use `storage.PutItem` (`INSERT OR REPLACE`). M4 extends `storage.PutItem` to return the new `data_id` (via `res.LastInsertId()`, same mechanism already used in `InsertTableDef`):

```go
func (s *Store) PutItem(tx *sql.Tx, table string, hashVal, rangeVal any, data []byte) (int64, error)
```

Existing callers (`ddb.GetItem`, `ddb.UpdateItem`'s read path) don't use the return; only the write paths adopt it.

Maintenance runs for each GSI on the table, driven by the post-write item. **Validation happens before any storage write** so a rejected write is atomic (probe G21: nothing is stored, including the data row):

1. Extract the GSI partition (+ sort) key attributes from the post-write item (top-level attribute lookup — GSI keys are always top-level attributes, never document paths).
2. **Validate present key attrs** (probe G21–G23, G29): each present GSI key attr must be a scalar whose tag matches the declared GSI key type (`tagForKeyType`), and non-empty for S/B. A wrong scalar type → `ErrValidation` (`Type mismatch for Index Key`); a non-scalar (L/M/BOOL/NULL/set) → `ErrValidation` (`Invalid attribute value type`); empty S/B → `ErrValidation`. This check **cannot be delegated to SQLite** — STRICT affinity coercion silently accepts lossless mismatches (§2.3).
3. **Index iff all key attrs are present**: partition key present AND (for a composite GSI) sort key present → `storage.UpsertGsiRow(tx, gsiTable, dataID, hashVal, rangeVal)`. `INSERT OR REPLACE INTO gsi_tbl (data_id, hash, range) VALUES (?, ?, ?)` — `data_id` is the PK, so this upserts the GSI row.
4. **Partition present, sort absent** (composite GSI) → the item is **not indexed**; the write itself is accepted (probe G20). No GSI row is written; CASCADE already removed any old row.
5. **Partition absent** → no action (sparse: probe G2/G17). The `ON DELETE CASCADE` from the data-row REPLACE already deleted the old GSI row.

Because `INSERT OR REPLACE` deletes the old data row (and CASCADE cleans the old GSI rows) before inserting the new one, `ddb` only ever *upserts* GSI rows — it never explicitly deletes them on the write path. This keeps the maintenance path uniform across Put and Update (both REPLACE → cascade cleans old → upsert new).

### 8.2 DeleteItem

No GSI logic in `ddb`. `storage.DeleteItem` deletes the data row; `ON DELETE CASCADE` removes all GSI rows for that `data_id` (probe G18).

### 8.3 Key immutability

GSI key attributes are **not immutable** (unlike table keys). `UpdateItem` can change a GSI key — the item moves in the index (probe G16). The update evaluator's `ValidateKeyAttrs` continues to protect only the *table* key attributes; GSI key attrs are writable. The maintenance step (§8.1) recomputes GSI rows from the post-write item, so a changed GSI key naturally upserts the new row and the old row is cascade-deleted.

### 8.4 Cost note

`INSERT OR REPLACE` + CASCADE re-inserts all GSI rows on every overwrite, even when the GSI keys are unchanged. This is acceptable for a mock (unit-test tables are small); correctness is unaffected. Optimizing to skip unchanged GSI keys is out of scope (YAGNI for a test double).

## 9. Adapter changes

The adapter (`awsdynamodb/adapter.go`) gains GSI translation; removes M3/M1 GSI rejections.

### 9.1 CreateTable

Remove the M1 rejection (`if len(params.GlobalSecondaryIndexes) > 0 → ValidationException`). Translate SDK `[]types.GlobalSecondaryIndex` → `[]ddb.GlobalSecondaryIndex`:

| SDK field | Engine field |
|---|---|
| `IndexName` (`*string`) | `IndexName` |
| `KeySchema` (`[]types.KeySchemaElement`) | `KeySchema` |
| `Projection.ProjectionType` | `Projection.Type` |
| `Projection.NonKeyAttributes` | `Projection.NonKeyAttributes` |
| `ProvisionedThroughput` | ignored |

`AttributeDefinitions` passed through (now may include GSI key attrs).

### 9.2 Query / Scan

Remove the M3 `IndexName` rejection. Pass `IndexName` through to the engine. `ConsistentRead` + `IndexName` → the engine rejects (§6.5 step 3b); the adapter does not need a separate check.

### 9.3 DescribeTable

Translate `TableDescription.GlobalSecondaryIndexes` → `[]types.GlobalSecondaryIndexDescription` (key schema + projection + best-effort `ItemCount`/`IndexSizeBytes` as 0 — matching what dynamodb-local returns immediately after create).

### 9.4 Removed tests

M3 conformance case 31 (`IndexName` → `ValidationException`) and the adapter test `TestAdapterRejectsGSI` / `TestAdapterQueryIndexName` are removed or flipped: `IndexName` and `GlobalSecondaryIndexes` are now valid. The flipped cases assert successful GSI creation and query.

## 10. Testing strategy & conformance cases

Four layers, following the M3 precedent.

### 10.1 Layer 1 — `internal/storage` unit tests

- GSI table DDL: with and without sort key; verify `ON DELETE CASCADE` (delete a data row → GSI rows gone).
- `UpsertGsiRow`: insert then update (same `data_id`, new key) → row updated.
- `QueryGSI`: partition seek; each sort op (`=`, `<`, `<=`, `>`, `>=`, `BETWEEN`, `BEGINS_WITH`); ASC vs DESC; composite resume cursor `(range, data_id)` (including the non-unique-sort-key case: two items share `range`, resume after the first returns the second); partition-only GSI (resume by `data_id`).
- `ScanGSI`: full scan order (`data_id`); resume by `afterID`.
- All wrapped in one `BeginTx` with `defer tx.Rollback()`, matching existing storage test style.

### 10.2 Layer 2 — `ddb` unit tests

- GSI `Query` end-to-end: key condition → results in GSI sort order.
- GSI `Scan` end-to-end.
- Projection: `KEYS_ONLY` (GSI keys + table keys only), `INCLUDE` (keys + projected, absent attrs omitted), `ALL` (everything).
- Sparse: item missing GSI partition key absent from GSI Query/Scan.
- Non-unique GSI key: two items sharing a GSI key both returned.
- `ConsistentRead=true` on GSI Query → `ErrValidation`.
- `Select=ALL_ATTRIBUTES` on non-ALL GSI → `ErrValidation`; `Select=ALL_PROJECTED_ATTRIBUTES` on base table → `ErrValidation`.
- Non-GSI attr in `KeyConditionExpression` → `ErrValidation`.
- `begins_with` on GSI sort key; on N GSI sort key → `ErrValidation`.
- LEK composition: four keys (GSI + table) for composite case; partition-only GSI LEK.
- Pagination: `Limit` + LEK resume (including trailing empty page); non-unique-sort-key resume.
- Write maintenance: `PutItem` (new item appears in GSI); `UpdateItem` changes GSI key (item moves); `UpdateItem` removes GSI key (sparse); `DeleteItem` (gone from GSI).
- `IndexName` → `ErrGsiNotFound` when the GSI doesn't exist.
- `ExclusiveStartKey` validation on GSI Query — exact table∪GSI key-attr union required: table-only, GSI-only, and union-plus-extra all → `ErrValidation`; overlapping table/GSI key attrs deduped in the union.
- Write-time GSI key type validation: wrong scalar type / non-scalar (L/M/BOOL/NULL/set) / empty S-B key on `PutItem` and `UpdateItem` → `ErrValidation`; rejection is atomic (no data row stored).
- Composite GSI sort-key absence: item with partition key but no sort key is write-accepted but absent from GSI Query/Scan.
- `analyzeCreateTable`: duplicate `AttributeDefinition` → `ErrValidation`; GSI IndexName charset/length → `ErrValidation`; INCLUDE naming a table or GSI key attr → `ErrValidation`; ALL with `NonKeyAttributes` → `ErrValidation`.

### 10.3 Layer 3 — conformance cases (dual-target)

Added to `awsdynamodb/conformance_test.go`, continuing the M3 numbering (37). Each runs against both the adapter and `dynamodb-local`:

38. **Basic GSI Query** — `IndexName` + `gsi_pk = :v` returns the right items in GSI sort order.
39. **Sparse GSI** — item missing the GSI partition key absent from GSI Query/Scan.
40. **Non-unique GSI key** — two items sharing a GSI key both returned.
41. **GSI sort-key conditions** — each of `=`, `<`, `<=`, `>`, `>=`, `BETWEEN`, `begins_with` on an S GSI sort key.
42. **`ScanIndexForward=false` on GSI** — DESC ordering by GSI sort key.
43. **GSI pagination** — `Limit` + resume via LEK (four keys) to exhaustion; trailing empty page when `Limit == available`.
44. **`ConsistentRead=true` on GSI Query and Scan** → `ValidationException`. (Probe G6/G24.)
45. **Projection KEYS_ONLY** — returned attrs = GSI keys + table keys.
46. **Projection INCLUDE** — returned attrs = GSI keys + table keys + projected (absent projected attrs omitted).
47. **Projection ALL** — all attrs returned.
48. **`Select=ALL_PROJECTED_ATTRIBUTES`** on a GSI — returns projected attrs.
49. **`Select=ALL_ATTRIBUTES` on non-ALL GSI** → `ValidationException`.
50. **Non-GSI attr in `KeyConditionExpression`** → `ValidationException`.
51. **GSI Scan** — all indexed items (sparse excluded); LEK nil when exhausted.
52. **GSI Scan pagination** — `Limit` + resume.
53. **`begins_with` on GSI sort key** — prefix match.
54. **`UpdateItem` changes GSI key** — item moves to the new GSI partition.
55. **`UpdateItem` removes GSI key** — item becomes sparse (absent from GSI).
56. **`DeleteItem` removes from GSI** — item gone from GSI Query.
57. **Partition-only GSI** — no sort key; Query returns all items in the GSI partition.
58. **GSI key overlapping table key** — GSI partition key = table partition key; valid.
59. **`ExclusiveStartKey` validation on GSI Query** — GSI partition mismatch with `KeyConditionExpression` → `ValidationException`. (Probe G25.)
60. **Composite GSI sort-key absence** — item with the GSI partition key but no sort key: write accepted; absent from GSI Query and Scan. (Probe G20.)
61. **GSI key type mismatch on `PutItem`** — GSI key attr of the wrong scalar type → `ValidationException`; the write is atomic (a subsequent `GetItem` finds nothing). (Probe G21.)
62. **Non-scalar GSI key attr** — L/BOOL/SS/NULL as a GSI key attr on `PutItem` → `ValidationException`. (Probe G22.)
63. **GSI key type mismatch on `UpdateItem`** — `SET` a GSI key to a mismatched type → `ValidationException`. (Probe G23.)
64. **Empty S GSI key value** — empty string as a GSI key attr → `ValidationException`. (Probe G29.)
65. **ESK shape validation on GSI Query** — table-keys-only, GSI-keys-only, and union-plus-extra `ExclusiveStartKey` → `ValidationException`; the exact table∪GSI key union is accepted. (Probe G25.)
66. **Duplicate `AttributeDefinition`** → `ValidationException` (fixes an M1 gap; applies with and without GSIs). (Probe G27.)
67. **GSI IndexName validation** — illegal characters / too short → `ValidationException`. (Probe G27.)
68. **DescribeTable returns GSI defs** — key schema + projection round-trip, compared as a set (order unspecified; probe G0).

### 10.4 Fuzzing

No new fuzz targets. The existing `FuzzParseCondition`/`FuzzBindEval` cover the grammar `KeyConditionExpression` reuses. `ExtractKeyCondition` is unchanged from M3 (it takes the GSI key names as parameters — same function, different names). GSI maintenance and projection trimming are exercised through the conformance and unit tests.

### 10.5 Verification gate

M4 is complete when all of the following pass, with the cache buster on every `go test`:

```bash
go test -count=1 ./...                                        # root module
cd awsdynamodb && go test -count=1 ./...                      # adapter module, adapter target
cd awsdynamodb && DDBSQLITE_CONF_TARGET=all go test -count=1 ./...  # both conformance targets
go vet ./... && (cd awsdynamodb && go vet ./...)              # both module directories
```

**Every M4 conformance case (38–68) must be green against both the adapter and `dynamodb-local.** GSI semantics — non-uniqueness, LEK composition, projection trimming, sparse behavior — are precisely where a faithful mock silently diverges, and the reference is available, so divergence is a blocker.

## 11. Decisions, risks & out of scope

### 11.1 Decisions captured

| Decision | Choice |
|---|---|
| Milestone shape | One spec, single implementation pass |
| GSI definition | Create-time only (`UpdateTable` GSI add/remove is M6) |
| Storage approach | JOIN + composite keyset cursor `(range, data_id)` |
| GSI index uniqueness | Non-unique (probe G3 disproved parent spec's UNIQUE) |
| GSI FK | `ON DELETE CASCADE` (required for `INSERT OR REPLACE` + `DeleteItem`) |
| Stable tiebreak | `data_id` (= rowid) via `INTEGER PRIMARY KEY`; non-unique index orders by `(hash, range, data_id)` |
| LEK composition | GSI keys + table keys (probe G4/G19) |
| Projection | Applied in Go at read time (parent §3.4(4)) |
| `Select=ALL_ATTRIBUTES` on non-ALL GSI | Rejected (probe G11) |
| `Select=ALL_PROJECTED_ATTRIBUTES` | GSI-only; returns projected attrs (probe G10) |
| `ConsistentRead` on GSI | Rejected → `ErrValidation` (probe G6) |
| Write maintenance | Upsert GSI rows after data write; CASCADE cleans old rows; never explicit delete on write path |
| Write-time GSI key type validation | In `ddb`, on the post-write item, before any storage write; wrong scalar / non-scalar / empty → `ErrValidation`; rejection is atomic (probe G21–G23, G29). Cannot be delegated to SQLite — STRICT affinity coercion (§2.3) |
| Composite GSI indexing rule | Index iff partition AND sort key attrs are both present; partition-present-sort-absent is write-accepted but unindexed (probe G20) |
| GSI `ExclusiveStartKey` validation | Exactly the table∪GSI key-attr union (deduped on overlap); missing/extra → `ErrValidation` (probe G25). Not the base-table `validateKey` |
| GSI `range` column | NOT NULL when present (both-present rule makes NULL unreachable) |
| Duplicate `AttributeDefinition` | Rejected (probe G27); fixes M1's silent dedup |
| GSI IndexName validation | 3–255 chars, `[a-zA-Z0-9_.-]` (probe G27); the equivalent table-name rule stays M6 |
| GSI key immutability | Not immutable (unlike table keys); UpdateItem can change GSI keys (probe G16) |
| GSI Scan parallel segments | `TotalSegments > 1` on GSI Scan rejected in M4 (scope cut) |
| Probe disposal | `gsi_probe_test.go` deleted once conformance cases ported |

### 11.2 Risks & mitigations

1. **Composite resume cursor correctness.** The `OR (range = ? AND data_id > ?)` tiebreak must not skip or repeat items across pages. *Mitigation:* storage unit tests for `QueryGSI` with a non-unique sort key + resume; conformance case 43 exercises multi-page GSI pagination with tied sort keys.
2. **`ON DELETE CASCADE` + `INSERT OR REPLACE` interaction.** The REPLACE deletes the old row (triggering CASCADE on GSI rows) before inserting the new one. If the GSI upsert runs *before* the data write, the CASCADE would delete the just-upserted GSI row. *Mitigation:* maintenance order is strictly data-write-first (§8.1); the upsert uses the new `data_id` returned by `PutItem`. Storage unit test: Put → upsert → re-Get confirms the GSI row survives.
3. **`ValidateFilterKeys` for GSI queries.** A filter referencing a GSI key attr must be rejected, same as table key attrs. An easy omission. *Mitigation:* `keyAttrsForFilter` includes both table and GSI key attrs (§6.5 step 9); conformance case asserts a filter on a GSI key attr is rejected (covered by extending the M3 filter-key-attr case, case 23, to GSI queries).
4. **`DescribeTable` GSI order is unspecified.** The probe (G0) showed GSIs return in no guaranteed order. *Mitigation:* conformance cases compare GSI contents as sets, not order-sensitive; `DescribeTable` tests sort GSI defs before comparing.
5. **`INSERT OR REPLACE` re-inserts all GSI rows on every overwrite.** Acceptable for a mock (small tables); correctness unaffected. No mitigation needed; documented as a cost note (§8.4).

### 11.3 Explicitly out of scope for M4

- `UpdateTable` (GSI add/remove on an existing table) — M6.
- LSI (local secondary indexes) — v1 non-goal.
- GSI backfill on `UpdateTable` add — M6.
- GSI `Scan` with `Segment`/`TotalSegments` (parallel scan on a GSI) — rejected in M4; rarely used in tests.
- `ProjectionExpression` — v1 non-goal.
- Consumed-capacity accounting, throttling — v1 non-goals.
- GSI auto-scaling, GSI replica (global tables) — v1 non-goals.
- Table-name charset/length validation (the same 3–255, `[a-zA-Z0-9_.-]` rule GSI names get in M4) — M6 hardening; M1 predates the finding.
- GSI backfill violation handling: when `UpdateTable` (M6) adds a GSI to a populated table, items whose key attrs violate the index key schema are excluded from the index during backfill rather than blocking creation — while post-backfill writes to such items are rejected (AWS-documented; distinct from M4's create-time-only GSIs, where no pre-existing items exist).
