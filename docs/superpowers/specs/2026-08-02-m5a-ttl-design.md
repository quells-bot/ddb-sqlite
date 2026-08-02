# ddb-sqlite M5a — TTL Design

**Date:** 2026-08-02
**Status:** Approved (brainstorming) → pending implementation plan
**Parent spec:** `docs/superpowers/specs/2026-07-31-ddb-sqlite-design.md` (§3.4(5), §6.1, §6.2, §11 M5 milestone)
**Prerequisite:** M4 complete — `PutItem`/`UpdateItem`/`DeleteItem` with GSI maintenance, `CreateTable`/`DescribeTable` with GSI defs, `TableDef.TTL` field wired to catalog `ddb_table_defs.ttl TEXT` column, `Options{DSN string}` on `ddb.Client`, all M4 conformance cases green against both targets.

## 1. Overview & goal

M5a adds the **TTL (Time to Live) lifecycle**: `UpdateTimeToLive`/`DescribeTimeToLive` (configure), and `ExpireExpired` (the only deletion lever). An injectable clock enables deterministic TTL tests without `time.Sleep`.

M5a delivers:

- `ddb` — `UpdateTimeToLive`/`DescribeTimeToLive` operations; `ExpireExpired(ctx, tableName) (int, error)` engine extension; `TimeToLiveSpecification`/`DescribeTimeToLiveOutput` types; `Options.Now func() time.Time` injectable clock; `*Client.now()` accessor.
- `internal/storage` — `UpdateTableTTL` catalog update method; `ExpireExpired` scan-with-callback delete; **removal of the `ttl INTEGER` column** from the per-table data-table DDL (`CreateDataTable`).
- `awsdynamodb` — `UpdateTimeToLive`/`DescribeTimeToLive` adapter methods translating SDK `TimeToLiveSpecification`/`TimeToLiveDescription`.
- Conformance cases for `UpdateTimeToLive`/`DescribeTimeToLive` (dual-target).

**Scope boundary:** TTL only. `BatchWriteItem`/`BatchGetItem` are deferred to M5b (a follow-up discussion and spec). No `UpdateTable` (GSI add/remove — M6), no background reaper process, no TTL Streams events.

### 1.1 Correction to parent spec §3.4(5)

The parent spec chose **synchronous read-path filtering** ("On any read path (`Get`/`Query`/`Scan`/`BatchGet`), rows with `ttl <= now` are filtered out as if expired"). M5a **reverses this decision**:

- Real DynamoDB deletes expired items **asynchronously** (up to 48h after expiration); expired items remain visible on reads until the background reaper deletes them.
- `dynamodb-local` has **no TTL reaper at all** — expired items stay visible forever.
- Synchronous read-path filtering would diverge from **both** reference targets and break the dual-target conformance suite.

M5a adopts the **Faithful** model: expired items remain visible on every read until explicitly reaped via `ExpireExpired`. The parent spec's `ttl INTEGER` column on the data table (§3.2) is **removed** — under the parse-at-reap approach (§2.3) the column serves no purpose.

## 2. TTL model

### 2.1 Read visibility (Faithful)

Expired items are **never filtered** from read paths. `GetItem`, `Query`, `Scan`, and (future) `BatchGetItem` return expired items exactly like unexpired ones. No SQL `WHERE ttl > now` predicate is added to any read query. This is the deliberate divergence from parent spec §3.4(5), documented here as a correction.

Deletion of expired items is **opt-in** via `ExpireExpired(ctx, tableName)`, an engine extension with no SDK equivalent. Tests call it after advancing the injectable clock to assert "item gone."

### 2.2 Configuration

`UpdateTimeToLive` enables or disables TTL by recording the TTL attribute name in `ddb_table_defs.ttl` (the catalog `TEXT` column, already present and wired to `TableDef.TTL`).

- **Enable:** `UPDATE ddb_table_defs SET ttl = ? WHERE id = ?` with the attribute name. The attribute name must be 1–255 chars; DynamoDB imposes **no charset restriction** on attribute names (unlike table/GSI names). The name is required — and validated — whether enabling or disabling, matching the AWS API model where `AttributeName` is a required member of `TimeToLiveSpecification`. Enabling on an already-enabled table with the same attribute is idempotent. Re-specifying a different attribute name while enabled is allowed (DynamoDB permits re-specification).
- **Disable:** `UPDATE ddb_table_defs SET ttl = NULL WHERE id = ?`. The catalog attr name is cleared. The supplied attribute name is validated (1–255 chars) but need not match the currently-configured name — it is ignored beyond validation. **Existing items are untouched** — `ExpireExpired` only runs when `def.TTL != ""`, so disabling stops further expiration. This matches DynamoDB: disable stops the reaper but does not delete already-expired data.
- **No backfill on enable.** `UpdateTimeToLive` does not scan existing items. Existing items with the TTL attribute are eligible for expiration by the next `ExpireExpired` call (parse-at-reap, §2.3).

`DescribeTimeToLive` reads `def.TTL` and returns `TimeToLiveStatus` as `"ENABLED"` (when `def.TTL != ""`) or `"DISABLED"` (when `def.TTL == ""`), plus the attribute name.

### 2.3 Expiration: parse-at-reap (no `ttl` column)

The per-table data table's `ttl INTEGER` column (parent spec §3.2) is **removed**. `ExpireExpired` scans all data rows, unmarshals each blob, extracts the named TTL attribute, and deletes expired items. Storage does the SQL (scan + delete by rowid); `ddb` provides the expiry predicate (blob → bool) so storage stays blob-agnostic.

This approach:
- **Simplifies the write path** — `PutItem`/`UpdateItem` keep their current signatures; no `ttl` parameter, no write-path population.
- **Eliminates backfill** — enabling TTL requires no scan of existing items.
- **Trades reap cost for write simplicity** — every `ExpireExpired` call is a full table scan. Acceptable for a test mock where tables are small and `ExpireExpired` is called explicitly and rarely.
- **No `ttl` column** — the data-table DDL (`CreateDataTable`) drops `, ttl INTEGER`. No migration needed (the column was always NULL; no existing code reads or writes it).

### 2.4 TTL attribute extraction rules

When `ExpireExpired` parses a blob for the TTL attribute (`def.TTL`):

| Item state | Behavior |
|---|---|
| TTL attr present, type `Number`, value `<= now` | **Expired** — deleted. |
| TTL attr present, type `Number`, value `> now` | Not expired — kept. |
| TTL attr **absent** | Not expired — kept (no TTL → never expires). |
| TTL attr present, type **not `Number`** | Not expired — kept (DynamoDB ignores non-conforming TTL attrs). |
| TTL attr present, type `Number`, value **non-integer / negative / zero** | Parsed as-is (numeric value). `ttl=0` expires at epoch; negative expires "already." No validation rejection — DynamoDB doesn't reject these either. |

The comparison uses `num.Decimal` (the exact decimal carried by `attrval.Number`) compared against the epoch-seconds of `now`. This preserves numeric equality semantics (`1` == `1.0`) consistent with the rest of the engine.

### 2.5 Expiry boundary: `<=` vs `<`

The parent spec (§3.4(5)) used `ttl <= now`. AWS documentation is ambiguous on the exact boundary. This is a **probe question** (§6): a throwaway probe against `dynamodb-local:3.3.1` settles whether an item with `ttl == now` is expired. If the probe is inconclusive (dynamodb-local has no reaper), `<=` is retained as the documented choice — it is the more intuitive boundary ("expired at or before now").

## 3. Storage changes

### 3.1 Remove `ttl INTEGER` from data-table DDL

`CreateDataTable` (`internal/storage/tables.go`) currently emits:

```sql
CREATE TABLE ddb_<hash> (
  id INTEGER NOT NULL PRIMARY KEY,
  hash <TYPE> NOT NULL,
  range <TYPE>,          -- if sort key
  data BLOB NOT NULL, ttl INTEGER,
  UNIQUE (hash, range)
) STRICT
```

M5a changes the `data` line to `data BLOB NOT NULL` (dropping `, ttl INTEGER`):

```go
b.WriteString(`, data BLOB NOT NULL`)
```

The stale doc comment on `CreateDataTable` (`ttl is NULL for now (populated M5)`) is updated in the same change.

The catalog `ddb_table_defs.ttl TEXT` column (the TTL attribute name) is **unaffected** — it stores the configured attribute name, not an item-level timestamp. No migration: existing in-memory test DBs are ephemeral; the column was never written or read.

### 3.2 `UpdateTableTTL` — catalog update (new)

```go
// UpdateTableTTL sets the TTL attribute name for a table. An empty ttlAttr
// disables TTL (sets the catalog column to NULL). This is the first
// catalog-mutate method beyond insert/get/delete.
func (s *Store) UpdateTableTTL(tx *sql.Tx, tableID int64, ttlAttr string) error
```

SQL: `UPDATE ddb_table_defs SET ttl = ? WHERE id = ?` (empty string → NULL via `nilIfEmpty`).

### 3.3 `ExpireExpired` — scan-with-callback delete (new)

```go
// ExpireExpired scans all rows in the table's data table, calls expired(data)
// for each blob, and deletes the rows for which expired returns true. Returns
// the count of deleted rows. GSI index rows are cleaned by the ON DELETE
// CASCADE foreign key on GSI tables. The expired callback is provided by ddb
// and handles blob unmarshalling + TTL attribute extraction; storage stays
// blob-agnostic.
func (s *Store) ExpireExpired(tx *sql.Tx, table string, expired func([]byte) (bool, error)) (int64, error)
```

Flow:
1. `SELECT id, data FROM ddb_<hash>` — full table scan.
2. For each row, call `expired(data)`. On error, abort the scan and return the wrapped error (a blob that fails to unmarshal must fail loud, not be silently kept). Collect ids where it returns true.
3. Close the `sql.Rows` cursor (cannot issue DELETE while iterating on the same tx).
4. `DELETE FROM ddb_<hash> WHERE id = ?` per collected id (rowid delete — efficient, no key re-parsing).
5. Return total deleted count.

GSI index rows are removed automatically by the `ON DELETE CASCADE` on `data_id REFERENCES ddb_<hash> (id)`.

## 4. `ddb` engine API surface

### 4.1 Injectable clock

```go
// Options configures a Client. DSN is a file path, ":memory:", or a
// "file:...?..." URI. Now, when non-nil, overrides time.Now for TTL expiration
// and table creation timestamps, enabling deterministic tests without sleeping.
type Options struct {
    DSN string
    Now func() time.Time
}
```

`Open` stores `opts.Now` on `*Client`, defaulting to `time.Now` when nil. The clock is **immutable after construction** — no `SetClock` mutator (avoids synchronization; the client is goroutine-safe). Tests advance time by capturing a variable in the closure:

```go
now := time.Now()
c, _ := ddb.Open(ctx, ddb.Options{
    DSN: ":memory:",
    Now: func() time.Time { return now },
})
c.PutItem(ctx, PutItemInput{...})  // item with TTL attr = now + 60s
c.ExpireExpired(ctx, "MyTable")    // 0 deleted (60s ahead)
now = now.Add(2 * time.Minute)     // closure sees the rebind
c.ExpireExpired(ctx, "MyTable")    // 1 deleted
```

Go closures capture variables **by reference** — reassigning the captured variable after closure creation is visible to the closure. `time.Time` being a value type does not change this; the *binding* is what's captured. (Verified: reassigning a captured `time.Time` variable updates the closure's return value.)

The clock drives:
- `ExpireExpired` — "now" for the expiry comparison.
- `CreateTable` — `CreationTime` (replaces the current `time.Now().UTC()` call in `ddb/tables.go`).

### 4.2 New types

```go
// TimeToLiveSpecification is the engine's TTL configuration.
type TimeToLiveSpecification struct {
    Enabled       bool
    AttributeName string
}

// UpdateTimeToLiveInput carries the table name and TTL spec to apply.
type UpdateTimeToLiveInput struct {
    TableName                string
    TimeToLiveSpecification  TimeToLiveSpecification
}

// UpdateTimeToLiveOutput echoes the applied spec.
type UpdateTimeToLiveOutput struct {
    TimeToLiveSpecification TimeToLiveSpecification
}

// DescribeTimeToLiveInput names the table to describe.
type DescribeTimeToLiveInput struct {
    TableName string
}

// DescribeTimeToLiveOutput reports the TTL status and attribute name.
type DescribeTimeToLiveOutput struct {
    TimeToLiveStatus string // "ENABLED" | "DISABLED"
    AttributeName    string
}
```

### 4.3 Operation flow — `UpdateTimeToLive`

1. Begin tx.
2. `GetTableDef(tx, in.TableName)` → `ErrTableNotFound` if absent.
3. Validate `AttributeName` (1–255 chars) — **unconditionally**, whether enabling or disabling (the AWS API model marks it required in `TimeToLiveSpecification`). Attribute names are broader than table/GSI names — any non-empty string ≤255 chars, no charset restriction; do **not** reuse `validGsiName` (which enforces a 3-char minimum and a restricted charset for GSI names). When disabling, the name is validated but otherwise ignored (it need not match the configured name).
4. `UpdateTableTTL(tx, def.ID, attrName)` — when `Enabled`, set attr name; when disabled, set empty (NULL).
5. Commit.
6. Return `UpdateTimeToLiveOutput{TimeToLiveSpecification: in.TimeToLiveSpecification}`.

**Validation order:** table-exists before attribute-name validation (`ResourceNotFoundException` takes precedence over `ValidationException`). Probe T5 (§6) verifies this against dynamodb-local; if local disagrees, the engine's order becomes a documented adapter-only divergence.

**Idempotency:** enabling with the same attr name as currently configured is a no-op (returns the same spec). Changing the attr name while enabled is allowed (overwrites).

### 4.4 Operation flow — `DescribeTimeToLive`

1. Begin tx.
2. `GetTableDef(tx, in.TableName)` → `ErrTableNotFound` if absent.
3. If `def.TTL != ""`: status = `"ENABLED"`, attr = `def.TTL`.
   If `def.TTL == ""`: status = `"DISABLED"`, attr = `""`.
4. No commit — read-only tx released by the deferred rollback, matching `DescribeTable`.
5. Return `DescribeTimeToLiveOutput`.

### 4.5 Operation flow — `ExpireExpired`

```go
func (c *Client) ExpireExpired(ctx context.Context, tableName string) (int, error)
```

1. Begin tx.
2. `GetTableDef(tx, tableName)` → `ErrTableNotFound` if absent.
3. If `def.TTL == ""`: return `0, nil` (TTL disabled — nothing to expire).
4. Build the `expired` callback:
   - Unmarshal the blob into `Item` (wire JSON, same pattern as `readItem` in `ddb/items.go`). On unmarshal failure, return `(false, err)` — storage aborts the scan and propagates the error.
   - Look up `item[def.TTL]`. If absent or `Tag() != TagNumber`: not expired.
   - Take the number's exact decimal via `attrval.Value.Num()` (`num.Decimal`).
   - Compare the TTL decimal against the epoch-seconds of `c.now()`. Convert `c.now().Unix()` to a `num.Decimal` (e.g. via `num.Parse(strconv.FormatInt(..., 10))` — infallible for this input) and use `num.Decimal.Compare` (scale-insensitive: `1` == `1.0`). Expired when `ttlValue <= nowEpoch` (boundary per §2.5).
5. `store.ExpireExpired(tx, tableName, expired)` — scans, deletes, returns count.
6. Commit.
7. Return count.

**Why the callback pattern:** storage iterates `SELECT id, data` and deletes by rowid; `ddb` owns the blob-parsing logic (unmarshal, TTL attr extraction, numeric comparison). This keeps storage blob-agnostic (no import of `attrval`/`num`/`json`) while `ddb` owns all item semantics. The callback is the seam.

**GSI cleanup:** automatic via `ON DELETE CASCADE`. No explicit GSI maintenance call needed (unlike `PutItem`/`UpdateItem` which call `maintainGsiRows`).

### 4.6 No changes to read paths

`GetItem`, `Query`, `Scan`, and `readItem` (shared by conditional-write paths) are **unchanged**. They do not filter on TTL. This is the core of the Faithful model (§2.1).

### 4.7 No changes to write paths

`PutItem` and `UpdateItem` are **unchanged**. They do not populate a `ttl` column (there is none). The TTL attribute lives inside the `data` JSON blob alongside every other attribute; `ExpireExpired` parses it at reap time.

## 5. Adapter changes

### 5.1 `UpdateTimeToLive`

```go
func (a *Adapter) UpdateTimeToLive(ctx context.Context, params *dynamodb.UpdateTimeToLiveInput,
    optFns ...func(*dynamodb.Options)) (*dynamodb.UpdateTimeToLiveOutput, error)
```

Translates SDK `types.TimeToLiveSpecification{Enabled, AttributeName}` → engine `TimeToLiveSpecification`. **The adapter validates nothing about content** — all attribute-name validation lives in the engine (§4.3), so the engine's error precedence (table-exists before spec validation) is preserved and `mapError` surfaces `ValidationException`/`ResourceNotFoundException` exactly as for other operations. The only adapter-level checks are the structural nil rejections required to translate at all:

- `params.TimeToLiveSpecification == nil` → `ValidationException` (unavoidable: there is nothing to translate). This structural check necessarily precedes the engine's table lookup; document it as such.
- `Enabled == nil` → treated as `false` (documented adapter choice; the SDK field is `*bool`).
- `AttributeName == nil` → translated as `""`, which the engine then rejects with `ErrValidation` (1–255 required unconditionally) — keeping this content error in the engine.

Returns SDK `UpdateTimeToLiveOutput{TimeToLiveSpecification}` echoing the input spec.

### 5.2 `DescribeTimeToLive`

```go
func (a *Adapter) DescribeTimeToLive(ctx context.Context, params *dynamodb.DescribeTimeToLiveInput,
    optFns ...func(*dynamodb.Options)) (*dynamodb.DescribeTimeToLiveOutput, error)
```

Maps engine `DescribeTimeToLiveOutput` → SDK `types.TimeToLiveDescription{TimeToLiveStatus, AttributeName}`. `TimeToLiveStatus` maps `"ENABLED"` → `types.TimeToLiveStatusEnabled`, `"DISABLED"` → `types.TimeToLiveStatusDisabled`. When the engine reports `DISABLED` (empty `AttributeName`), the adapter maps the empty string to a **nil `*string`** so the SDK description omits the attribute name — DynamoDB omits `AttributeName` when TTL is disabled, and a pointer-to-empty-string would diverge in dual-target conformance.

### 5.3 No adapter method for `ExpireExpired`

`ExpireExpired` is an engine extension with no SDK equivalent. Tests that need it use the `*ddb.Client` directly (the conformance harness's adapter target wraps a `*ddb.Client`, so the test can recover it via a type assertion or a dedicated accessor if needed). The `api` conformance interface does **not** include `ExpireExpired`.

## 6. Probe questions

Following the M3/M4 precedent, a throwaway probe — `awsdynamodb/ttl_probe_test.go` — drives `dynamodb-local:3.3.1` directly via the conformance harness's `TestMain`-managed `localClient`. The probe is env-gated (`DDBSQLITE_CONF_TARGET=dynamodb-local|all`); it skips under the default `go test ./...`. It is deleted once the M5a conformance cases are ported.

**Probe scenarios:**

| # | Scenario | Question | Expected / fallback |
|---|---|---|---|
| T1 | `UpdateTimeToLive(Enabled=true, AttributeName="ttl")` then `DescribeTimeToLive` | Does dynamodb-local accept `UpdateTimeToLive` and reflect it in `DescribeTimeToLive`? | **Likely yes** (accepts, reflects). Web research indicates local accepts the API call. |
| T2 | Put item with `ttl` attr = past epoch; `GetItem` immediately | Does dynamodb-local auto-delete or filter expired items on read? | **No** — expired items remain visible (no reaper). |
| T3 | Put item with `ttl` attr = exactly `now`; `GetItem` | Expiry boundary: is `ttl == now` expired? | If inconclusive (no reaper), retain `<=` as documented choice. |
| T4 | Put item with `ttl` attr = non-Number (e.g. String); `GetItem` | Does dynamodb-local reject the item at write time, or store it? (Configuration never inspects items, so a config-time rejection is structurally impossible — the question is write-time validation.) | **Likely stores it** (no write-time TTL validation; the attr is just blob data). |
| T5 | `UpdateTimeToLive` on a nonexistent table with an invalid (empty) `AttributeName` | Error precedence: `ResourceNotFoundException` or `ValidationException`? | **Likely ResourceNotFound** (table lookup precedes spec validation, matching §4.3). If local returns Validation instead, the engine's order becomes a documented adapter-only divergence. |

**Probe output drives the conformance cases.** If T1 confirms local accepts `UpdateTimeToLive`/`DescribeTimeToLive`, those become dual-target conformance cases. If local rejects them, the cases become adapter-only (in `conformance_divergence_test.go`). T2/T3 establish that no auto-deletion occurs (settling the Faithful model's conformance stance).

## 7. Testing strategy

### 7.1 Layer 1 — `internal/storage` unit tests

| Test | What it verifies |
|---|---|
| `TestUpdateTableTTL` | Enable sets catalog `ttl` to the attr name; disable sets it to NULL; re-enable with a different name overwrites. |
| `TestExpireExpired` | Seed items with various blobs; verify the callback is called per row, matching ids are deleted, non-matching survive, count is correct. GSI rows are cascade-deleted (seed a GSI, verify index rows gone after base delete). |
| `TestCreateDataTableNoTTLColumn` | The data-table DDL no longer has a `ttl` column (verify via `PRAGMA table_info`). |

### 7.2 Layer 2 — `ddb` unit tests

| Test | What it verifies |
|---|---|
| `TestUpdateTimeToLive` | Enable/disable/idempotent/re-specify. `ErrTableNotFound` for unknown table. `ErrValidation` for empty/oversized attr name — **including when disabling** (name required unconditionally). Table-exists error takes precedence over attr-name validation. |
| `TestDescribeTimeToLive` | ENABLED/DISABLED status, attr name round-trip. `ErrTableNotFound` for unknown table. |
| `TestExpireExpired` | With injectable clock: put items with TTL attr at various offsets, advance clock, call `ExpireExpired`, verify correct items deleted and survivors remain. Verify expired items were **visible on reads before** `ExpireExpired` (Faithful). Verify `ExpireExpired` returns 0 when TTL disabled. |
| `TestExpireExpiredEdgeCases` | Absent TTL attr (kept), non-Number TTL attr (kept), zero/negative TTL (expired), non-integer epoch. |
| `TestInjectableClock` | `Options.Now` drives `ExpireExpired` and `CreateTable`'s `CreationTime`. Default (`nil`) uses `time.Now`. |

### 7.3 Layer 3 — conformance cases (dual-target)

Added to `awsdynamodb/conformance_test.go`. `ExpireExpired` is engine-only (not on the `api` interface), so it has no conformance target.

| Case | Scenario |
|---|---|
| `TestConfUpdateTimeToLive` | Enable TTL, verify via `DescribeTimeToLive`. Disable, verify DISABLED. Re-enable with different attr. |
| `TestConfUpdateTimeToLiveErrors` | `UpdateTimeToLive` on a nonexistent table → `ResourceNotFoundException`. Empty `AttributeName` → `ValidationException` (both when enabling and when disabling — the name is required unconditionally). Precedence per probe T5. |
| `TestConfDescribeTimeToLive` | Describe on a table with TTL enabled → ENABLED + attr name. Describe on a table with TTL never set → DISABLED **and the SDK description's `AttributeName` is nil** (omitted, not empty string — §5.2). Also describe after disable → DISABLED, nil attr name. |
| `TestConfTTLExpiredItemVisible` | Put item with TTL attr = past epoch. `GetItem` returns the item (not filtered). `Query`/`Scan` include it. (Faithful: no read filtering.) |

If the probe (§6) finds dynamodb-local rejects `UpdateTimeToLive`, the first two cases move to `conformance_divergence_test.go` (adapter-only). The third case (`TestConfTTLExpiredItemVisible`) is valid on both targets regardless — it asserts that expired items are visible, which holds under both "no reaper" (local) and "reaper hasn't run yet" (real DynamoDB).

### 7.4 Verification gate

- `go test ./...` green (root module, default target).
- `cd awsdynamodb && go test ./...` green (adapter target).
- `cd awsdynamodb && DDBSQLITE_CONF_TARGET=all go test -count=1 ./...` green (both targets) — requires podman socket.
- No existing tests broken by the `ttl INTEGER` column removal.

## 8. Decisions, risks & out of scope

### 8.1 Decisions captured

1. **Faithful read visibility** (no filtering). Reverses parent spec §3.4(5). Expired items visible until `ExpireExpired`. Rationale: diverges from both reference targets otherwise; breaks dual-target conformance.
2. **Parse-at-reap, no `ttl` column.** `ExpireExpired` scans blobs and parses the TTL attr. Simplifies write path (no signature change, no backfill). Trade-off: every reap is a full scan — acceptable for a test mock.
3. **`ExpireExpired(ctx, tableName)`** — engine-only, per-table, no SDK equivalent. Tests call it explicitly for deterministic cleanup.
4. **Injectable clock via `Options.Now func() time.Time`** — immutable after construction, default `time.Now`. No `SetClock` mutator (avoids synchronization). Drives `ExpireExpired` + `CreationTime`.
5. **`DescribeTimeToLive` added** — not in the parent spec's milestone list, but part of the faithful TTL API. Lets tests verify configuration via the SDK interface.
6. **Disable does not clear data** — `UpdateTimeToLive(Enabled=false)` clears the catalog attr name; `ExpireExpired` becomes a no-op. Existing items untouched. Matches DynamoDB.
7. **`AttributeName` validated unconditionally** (1–255 chars, no charset restriction), enabling or disabling — the AWS API model marks it required in `TimeToLiveSpecification`. When disabling, the name is validated but otherwise ignored.
8. **Adapter translates only; the engine validates.** All content validation lives in `ddb` so the §4.3 error precedence (table-exists before spec validation) holds and `mapError` is the single error-mapping point. The only adapter-level checks are structural nil rejections required for translation (nil `TimeToLiveSpecification` → `ValidationException`; nil `Enabled` → false; nil `AttributeName` → `""` → engine rejects). Disabled `DescribeTimeToLive` maps the empty attr name to a nil `*string` (DynamoDB omits it).
9. **`expired` callback carries an error channel** (`func([]byte) (bool, error)`) — a blob that fails to unmarshal aborts the reap with a wrapped error rather than being silently kept.

### 8.2 Risks & mitigations

1. **dynamodb-local TTL behavior unknown.** Web research indicates local accepts `UpdateTimeToLive` but has no reaper (expired items stay visible). *Mitigation:* the probe (§6) settles T1–T5 before conformance cases are written. If local rejects `UpdateTimeToLive`, the config cases become adapter-only divergences; the "expired item visible" case is valid on both targets regardless.
2. **Removing `ttl INTEGER` column changes `CreateDataTable`.** The column is in the current DDL but always NULL; no code reads or writes it. *Mitigation:* no migration needed (ephemeral DBs); verify no test depends on the column's existence.
3. **`Options.Now` adds a public API field.** Existing callers use `Options{DSN: ...}` without `Now` — the zero value (`nil`) defaults to `time.Now`. *Mitigation:* backward-compatible; no existing caller breaks.
4. **`ExpireExpired` full-scan cost.** Every call scans all rows in the table. *Mitigation:* test-mock tables are small; the cost is O(n) per table per call, called explicitly and rarely. If profiling ever shows this is slow, a `ttl` column + indexed `DELETE WHERE ttl <= ?` (option A from the design dialogue) can be reintroduced without changing the public API.
5. **Callback closure captures `def.TTL` and `c.now()`.** The `expired` callback is constructed fresh per `ExpireExpired` call (not stored long-term), so there's no stale-capture risk. *Mitigation:* the callback is call-scoped; no lifecycle concern.

### 8.3 Explicitly out of scope for M5a

- `BatchWriteItem` / `BatchGetItem` — deferred to M5b (follow-up discussion and spec).
- `UpdateTable` (GSI add/remove) — M6.
- Background TTL reaper process / TTL Streams events — v1 non-goals.
- TTL attribute validation at write time (rejecting non-Number TTL attrs on `PutItem`) — DynamoDB doesn't do this; the attr is just stored in the blob.
- `TimeToLiveSpecification` on `CreateTable` (DynamoDB doesn't support specifying TTL at table creation; TTL is always a separate `UpdateTimeToLive` call).
