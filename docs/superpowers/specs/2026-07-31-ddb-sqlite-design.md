# ddb-sqlite — Design

**Date:** 2026-07-31
**Status:** Approved (brainstorming) → pending implementation plan
**Module (today):** `github.com/quells-bot/ddb-sqlite` (single Go module; adapter splits to its own module before public release)

## 1. Overview & Goals

`ddb-sqlite` is an **in-process mock of a subset of the DynamoDB API**, implemented in Go and backed by a SQLite database, for use in unit tests of web applications that use DynamoDB as their primary database. Existing alternatives (dynalite, the `amazon/dynamodb-local` Docker container, scylladb alternator) require an external process and a local network; an in-process DynamoDB-compatible library greatly simplifies and speeds unit tests.

Goals:

- Provide a **drop-in replacement for the AWS SDK v2 DynamoDB client** for the supported API subset.
- The **core library must not depend on the AWS SDK**. A child adapter package that *does* depend on the SDK provides the integration layer; it lives in this repo during development and is pulled into its own Go module before public release.
- Support **multiple mocked tables in a single "AWS region"** (one engine instance = one region = one SQLite DB).
- **Parse and execute condition, filter, and update expressions.** Projection expressions are out of scope for v1.
- Use the **CGO-free `modernc.org/sqlite`** package to avoid cross-compilation headaches.
- Provide **Global Secondary Index (GSI)** features.

Non-goals (explicitly out of scope for v1):

- An HTTP API equivalent to the real DynamoDB service (a wrapper could be built separately).
- High concurrency. A single SQLite database is effectively single-writer; access is serialized.
- Eventual consistency. SQLite transactions + serialized access approximate the consistent read/write feature of real DynamoDB.
- Automatic streaming events (`StreamSpecification`).
- Provisioned-capacity throttling and consumed-capacity accounting.
- `TransactWriteItems` / `TransactGetItems`, PartiQL / `ExecuteStatement` / `ExecuteTransaction` / `BatchExecuteStatement`.
- Backups, point-in-time recovery, autoscaling, a background TTL reaper, and `ProjectionExpression`.

## 2. Architecture: core / adapter split

```
ddb-sqlite/                      # module github.com/quells-bot/ddb-sqlite  (SDK-free)
├─ go.mod
├─ attrval/                       # IMPORTABLE: DynamoDB typed-value model + wire encode/decode
├─ ddb/                           # IMPORTABLE: the engine (Client, operations, exported errors)
│   ├─ client.go                  # *Client holds the *sql.DB + table registry
│   ├─ tables.go                  # CreateTable/DescribeTable/UpdateTable/ListTables/DeleteTable/UpdateTimeToLive
│   ├─ items.go                   # PutItem/GetItem/UpdateItem/DeleteItem/BatchWriteItem/BatchGetItem/Query/Scan
│   └─ errors.go                  # exported: ErrConditionalCheck, ErrResourceNotFound, ...
├─ internal/                      # importable by ddb/ (same module) but NOT by the split adapter
│   ├─ storage/                   # SQLite plumbing: schema bootstrap, prepared statements, table-name hashing
│   ├─ expr/                      # expression parser + evaluator (condition / filter / update)
│   └─ num/                       # decimal type for exact N comparison & sort
├─ awsdynamodb/                  # adapter — depends on AWS SDK v2; splits to own module pre-release
│   └─ adapter.go                 # implements the supported subset of SDK DynamoDBAPI; translates Input/Output <-> core
└─ examples/, *_test.go
```

**Importable surface:** only `attrval` and `ddb`. The post-split adapter module imports exactly those two paths and nothing else from this module.

**Why `attrval` is importable but `expr`/`num` aren't:**

- The adapter must translate SDK `types.AttributeValue` ↔ the core value model, so `attrval.Value` appears in the core's public operation signatures → it must be importable post-split → top-level, not `internal/`.
- Expressions arrive from SDK inputs as **raw strings + `ExpressionAttributeNames`/`ExpressionAttributeValues` maps** (the SDK already carries them in that form). The adapter hands those strings to core; core parses them with `internal/expr`. The adapter never names `expr` types → `expr` stays internal.
- `num` is an implementation detail of `attrval`/`expr` → internal.

**No SDK types leak into core signatures.** During development, `awsdynamodb/` uses a `replace` directive in `go.mod`; the pre-release split is a `go.mod` move with no signature changes.

## 3. Storage model & SQL schema

### 3.1 Catalog tables (created once per database, STRICT)

```sql
CREATE TABLE ddb_table_defs (
  id INTEGER NOT NULL PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,          -- DynamoDB table name
  hash TEXT NOT NULL,                 -- partition key attribute name
  range TEXT,                         -- sort key attribute name (NULL if none)
  hash_type TEXT NOT NULL,            -- S | N | B
  range_type TEXT,                    -- required iff range is set (S | N | B)
  ttl TEXT,                           -- TTL attribute name (NULL = none)
  meta TEXT NOT NULL                  -- JSON: class, billingMode (ignored), creationTime, gsi snapshot, ...
) STRICT;

CREATE TABLE ddb_gsi_defs (
  table_id INTEGER NOT NULL REFERENCES ddb_table_defs (id),
  name TEXT NOT NULL,
  hash TEXT NOT NULL,
  range TEXT,
  hash_type TEXT NOT NULL,
  range_type TEXT,
  projection_type TEXT NOT NULL,      -- KEYS | INCLUDE | ALL
  projected TEXT,                     -- JSON attr list (INCLUDE); NULL otherwise
  PRIMARY KEY (table_id, name)
) STRICT;
```

### 3.2 Per-table data table (DDL generated dynamically)

The SQLite table name is `ddb_<16-hex-of-sha256(name)>`. Key column affinity depends on the key's DynamoDB type:

```sql
CREATE TABLE ddb_<hash> (
  id INTEGER NOT NULL PRIMARY KEY,
  hash <TYPE> NOT NULL,               -- TYPE = TEXT (S) | REAL (N) | BLOB (B)
  range <TYPE>,                       -- present iff the table has a sort key; NOT NULL when present
  data BLOB NOT NULL,                 -- item as AttributeValue wire JSON
  ttl INTEGER,                        -- epoch seconds; NULL when no TTL attribute set
  UNIQUE (hash, range)                -- UNIQUE(hash) when no sort key
) STRICT;
```

### 3.3 Per-GSI index table

`ddb_<tablehash>_<gsihash>` stores only keys + a foreign key back to the data row; projection is applied in Go at read time.

```sql
CREATE TABLE ddb_<tablehash>_<gsihash> (
  data_id INTEGER NOT NULL REFERENCES ddb_<tablehash> (id),
  hash <TYPE> NOT NULL,
  range <TYPE>,
  PRIMARY KEY (data_id)               -- each data row contributes at most one GSI row
) STRICT;
-- plus a UNIQUE index on (hash, range) so GSI Query can range-seek; PK on data_id for fast item maintenance
```

### 3.4 Key design decisions

1. **Table-name hashing → 16 hex of SHA-256.** DynamoDB table names can contain characters illegal as SQLite identifiers; the hash yields a stable, collision-resistant identifier. GSI tables append the GSI name's hash.
2. **Number sort keys stored as `REAL`** so SQLite's index gives correct *numeric* ordering for sort-key range conditions. The exact decimal value is preserved as a string inside the `data` JSON blob, so Go-side expression evaluation uses exact decimals, not float64. **Caveat:** a partition/sort key of type N relies on float64 ordering in the SQLite index — correct for normal-precision keys, theoretically divergent for keys beyond float64 precision. Acceptable for a test mock; documented.
3. **GSI maintenance is write-triggered.** On every `PutItem`/`UpdateItem`/`DeleteItem`, recompute each GSI's key attributes from the new item and upsert/delete the GSI index rows in the same transaction. A GSI item with a missing key attribute is simply absent from that GSI's index (matches DynamoDB: items missing a GSI key attribute don't appear in that GSI) — GSIs are sparse by construction.
4. **Projection applied at read time, not storage.** GSI index tables store only keys + FK; on GSI `Query`/`Scan` we fetch the full row from the data table, then trim to `KEYS_ONLY` (GSI keys + table keys), `INCLUDE` (those + projected attrs), or `ALL` (everything) in Go.
5. **TTL is lazy.** `UpdateTimeToLive` records the TTL attribute name in `ddb_table_defs.ttl`. On write, if that attribute exists in the item and is a Number, its value (epoch seconds) is copied into the `ttl` column. On any read path (`Get`/`Query`/`Scan`/`BatchGet`), rows with `ttl <= now` are filtered out as if expired — no background reaper. DynamoDB's real TTL is also best-effort/lazy, so this is faithful enough; a manual `ExpireExpired(ctx)` may be offered for tests that want deterministic cleanup.

## 4. Data model: `attrval` + `num`

`attrval.Value` is a tagged union mirroring DynamoDB's typed values, independent of the AWS SDK. Tags: `String`, `Number`, `Binary`, `Boolean`, `Null`, `List`, `Map`, `StringSet`, `NumberSet`, `BinarySet`.

- **Wire round-trip:** `attrval` encodes/decodes the DynamoDB wire JSON shape (`{"S":"…"}`, `{"N":"12.5"}`, `{"B":"base64…"}`, `{"BOOL":true}`, `{"NULL":true}`, `{"L":[…]}`, `{"M":{…}}`, `{"SS":[…]}`, `{"NS":[…]}`, `{"BS":[…]}`). This is exactly what's stored in the `data` BLOB and what the adapter marshals to/from SDK `types.AttributeValue`.
- **Numbers are strings at the boundary, decimals inside.** A `Number` carries an exact decimal (`internal/num.Decimal`, built on `big.Float` or a decimal library) so comparisons, `size()`, and ordering match DynamoDB — never float64. The wire form is the canonical string (leading/trailing zeros trimmed per DynamoDB). Equality is numeric (`1` == `1.0`).
- **Sets are unordered, deduplicated, with type-specific equality.** `SS`/`NS`/`BS` store a Go set keyed by the value's canonical form (NS keyed by decimal value so `1` and `1.0` collide). `contains`, `IN`, and set-overlap semantics derive from this. `size()` of a set = distinct count.
- **Document paths** (`a.b[2].c`) navigate `Map`/`List`; missing segments yield "attribute does not exist," which condition expressions must distinguish from an *existing* `Null`. `attrval` exposes path lookup helpers used by `expr`.
- **Type-matching for `attribute_type`:** each tag maps to one DynamoDB type code (`S`/`N`/`B`/`BOOL`/`NULL`/`L`/`M`/`SS`/`NS`/`BS`), so `attribute_type(path, "N")` is a direct tag check.

`num.Decimal` (`internal/`): exact decimal with comparison and ordering; canonical-string normalization. Used by `attrval` for Number equality/size and by `expr` for numeric comparisons. Stays internal — the adapter never sees it; it only passes Number values as strings.

## 5. Expression engine (`internal/expr`)

One lexer + parser produces an AST; one evaluator serves condition **and** filter expressions (identical grammar); a separate update evaluator applies update expressions. The adapter passes raw strings + `ExpressionAttributeNames`/`ExpressionAttributeValues` maps; `expr` does all parsing.

### 5.1 Condition / filter grammar & evaluation (per AWS DynamoDB reference)

- **Functions:** `attribute_exists`, `attribute_not_exists`, `attribute_type`, `contains`, `begins_with`, `size`.
- **Comparisons:** `= <> < > <= >=`, `BETWEEN`, `IN`. **Logical:** `AND OR NOT`, parentheses.
- **Operands:** a document `name.path[2]`, a `:value` substitution, or `size(path)`.
- **Type discipline (faithfulness):** comparisons require operand types to be comparable; mismatched types make a comparison evaluate **false** (not an error), matching DynamoDB. `=` across types is false, `<>` is true. Number comparisons use exact decimals.
- **`size()` semantics:** String→UTF-8 byte length, Binary→byte count, Set/List/Map→element count. Number and BOOL are **unsupported**: `size()` on them yields *missing*, so the enclosing comparison is false. (An earlier draft asserted "Number → digit count per AWS spec"; `dynamodb-local` returns `ConditionalCheckFailedException` for `size(n) = :n` at any digit count, so the digit-count claim was wrong and this line is the correction, see M2 expressions spec §4.3(1).)
- **`contains` / `begins_with`:** substring/byte-prefix for String/Binary; membership for Set; element-equality for List.
- **`attribute_exists` vs Null:** distinguishes *present* (including `Null`) from *missing* — the evaluator must honor this subtle DynamoDB distinction.

### 5.2 Update expression evaluator

- **`SET`** `path = value`: create/overwrite at a nested path; `if_not_exists(path, operand)` and `list_append(a, b)` functions.
- **`REMOVE`** `path`: delete the attribute/element; empty containers left as-is (DynamoDB doesn't auto-prune).
- **`ADD`** `path value`: Number→increment; Set (SS/NS/BS)→union in the elements (creating the set if absent).
- **`DELETE`** `path value`: sets only — remove the listed elements.
- Clauses appear in any order, each at most once. `ReturnValues` (`NONE|ALL_OLD|ALL_NEW|UPDATED_OLD|UPDATED_NEW`) computed by diffing before/after items in Go.

### 5.3 Substitution & limits

- `#name`→actual attribute name; `:value`→`attrval.Value`. The parser treats `#x` as a name token and resolves it (including nested `#x.sub`) against the names map.
- **Faithful limits enforced:** condition-expression length, `IN` value count, attribute-name/value count and nesting-depth caps (per AWS docs) — rejecting oversized inputs so tests can rely on these boundaries.
- **Error mapping:** malformed expressions, undefined `#name`/`:value`, or type-invalid `ADD`/`DELETE` operands raise typed core errors the adapter maps to SDK validation exceptions (`ValidationException`).

## 6. Operations & behavior

### 6.1 Table operations

`CreateTable` (validates name/key-schema/GSIs, creates catalog rows + data/GSI tables in one tx), `DescribeTable`, `ListTables` (`ExclusiveStartTableName`/`Limit` pagination), `DeleteTable` (drops data + GSI tables, removes catalog rows), `UpdateTable` (GSI add/remove — throughput/stream changes accepted-and-ignored since provisioning is out of scope), `UpdateTimeToLive` (sets `ddb_table_defs.ttl`). All return typed errors for unknown table, name in use, etc.

### 6.2 Item operations

- **`PutItem`:** insert/overwrite by key; `ConditionExpression` evaluated pre-write → `ErrConditionalCheck` on failure. 400KB item-size limit enforced. On write, recompute all GSI key rows + `ttl` column in the same tx.
- **`GetItem`:** key lookup, skip if TTL-expired. `ConsistentRead` accepted (no behavioral change — always consistent here).
- **`UpdateItem`:** read-modify-write the `data` blob via the update evaluator; `ConditionExpression` pre-check; `ReturnValues` honored. Maintain GSI keys + TTL.
- **`DeleteItem`:** key delete with condition check; cascade-delete GSI index rows.
- **`BatchWriteItem`:** up to 25 requests / 16MB per DynamoDB; each request runs in the shared tx. With no throttling in v1, all valid requests are processed and `UnprocessedItems` is always empty; batches exceeding 25 requests or 16MB raise `ErrValidation` → `ValidationException` (no partial processing).
- **`BatchGetItem`:** up to 100 items / 16MB; per-table key lists; TTL filtering; `ProjectionExpression` out of scope (full items returned, faithful to "no projection in v1").

### 6.3 Query

- Scope by partition key equality **in SQLite** (index seek); apply the sort-key condition (`= < <= > >= BETWEEN begins_with`) as a SQLite range predicate on the indexed `range` column — this is the key-narrowing SQLite does.
- `ScanIndexForward` controls ASC/DESC order. `Limit` caps items **before** filter (DynamoDB counts items scanned, not returned). `ExclusiveStartKey` resumes from a key; `LastEvaluatedKey` emitted when stopped early.
- `FilterExpression` evaluated **in Go** after the key scan; filtered-out items still count toward `Limit`/`ScannedCount`. Both `ScannedCount` and `Count` reported.
- `IndexName` → query the GSI index table instead (sparse semantics); GSI projection applied at fetch.

### 6.4 Scan

Full table scan (or GSI scan via `IndexName`), ordered by rowid; `Limit`/`ExclusiveStartKey`/`LastEvaluatedKey`/`FilterExpression` as in Query. `Segment`/`TotalSegments` for parallel scan — honored by partitioning the rowid range so parallel scans don't overlap.

### 6.5 Pagination semantics (faithful)

`LastEvaluatedKey` is the key of the last *scanned* item (even if filtered out), so a subsequent request with it as `ExclusiveStartKey` resumes exactly. `Limit=0` returns no items but sets `LastEvaluatedKey` if the table is non-empty (DynamoDB behavior).

### 6.6 Error contracts

Typed core errors — `ErrConditionalCheck`, `ErrResourceNotFound`, `ErrTableNotFound`, `ErrGsiNotFound`, `ErrValidation`, `ErrItemCollectionSize`, `ErrLimitExceeded` — map 1:1 to SDK exception types in the adapter. `ConditionalCheckFailedException` is returned as that exact type so `errors.As` works in tests.

## 7. Lifecycle, concurrency, transactions

**Client lifecycle:** `ddb.Open(ctx, opts) (*Client, error)` where `opts` is an `Options` struct carrying a `DSN string` (a file path, `:memory:`, or a `file:…?…` URI) plus optional pragmas. **One `*Client` = one SQLite DB = one "region"** with its own set of tables. Tests typically `ddb.Open(ctx, Options{DSN: ":memory:"})` for an ephemeral per-test DB, or a temp-file path when persistence across reopen is needed. `Client.Close()` closes the DB.

**Concurrency via `database/sql`:** the engine uses the standard `database/sql` surface with the **`modernc.org/sqlite`** driver (imported for side-effect registration). One `*sql.DB` is opened from the DSN and configured:

```go
db, _ := sql.Open("sqlite", dsn)
db.SetMaxOpenConns(1)   // serialize: at most one connection in flight at a time
db.SetMaxIdleConns(1)
db.SetConnMaxLifetime(0)
```

- **Serialized single-writer semantics come from the pool.** `SetMaxOpenConns(1)` means at most one logical connection is checked out at a time; concurrent ops queue on the pool — the desired "single writer / serialized access" behavior, with no custom locking.
- **Idiomatic, less code.** We use `*sql.DB`/`*sql.Tx`/`*sql.Rows` and get prepared-statement caching, `context` cancellation, and conn lifecycle for free, instead of managing a modernc raw conn and a mutex ourselves.
- **Atomic multi-statement ops are natural:** each mutating op does `db.BeginTx` → all catalog/data/GSI/ttl statements on that `*sql.Tx` → `Commit`. The tx holds the single conn for its whole duration, so concurrent ops block until commit — the transaction *is* the serialization unit.
- **Rule the engine honors:** within a transaction, every statement is issued through the `*sql.Tx`, never through the parent `*sql.DB` (otherwise it would deadlock waiting for the conn the tx already holds). All write paths already do catalog+data+GSI+ttl on the tx.
- **Driver pragmas** set once on open (e.g. `journal_mode`, `foreign_keys=ON` for the GSI FKs) via a single `Exec` before serving.

`internal/storage` wraps the `*sql.DB`.

**Adapter construction:** `awsdynamodb.New(client *ddb.Client) *Adapter`, where `*Adapter` implements the supported subset of the SDK's `DynamoDBAPI`. It marshals `types.AttributeValue`↔`attrval.Value`, passes expression strings + names/values maps straight through, and converts typed core errors to SDK exception types. `*Adapter` is goroutine-safe because `*ddb.Client` is; no extra concurrency in the adapter.

## 8. Scope boundaries (out of scope for v1)

- `TransactWriteItems` / `TransactGetItems`, PartiQL, `ExecuteStatement`, `ExecuteTransaction`, `BatchExecuteStatement`.
- HTTP server, streaming/`StreamSpecification`, backups/PITR, autoscaling.
- Provisioned-capacity throttling and consumed-capacity accounting.
- Eventual-consistency simulation.
- Background TTL reaper (lazy only).
- `ProjectionExpression` (items returned in full).

## 9. Testing strategy

Since the library's purpose is to be a faithful test double, the highest-value tests are **conformance tests** — exercises of DynamoDB behavior run against both our adapter and a reference DynamoDB, asserting identical results.

1. **Engine unit tests** (`internal/`): table-driven tests for the expression parser/evaluator (condition/filter/update: expression string + item → expected bool or resulting item), storage round-trips, GSI maintenance, TTL filtering. Fast and hermetic.
2. **Adapter conformance suite** (`awsdynamodb/`): scenarios written against the SDK's `DynamoDBAPI` interface — CRUD, conditional writes, Query/Scan pagination, GSIs, batch ops, update expressions, error cases. **Parameterized by the interface**, so the same suite can be pointed at (a) `*awsdynamodb.Adapter` wrapping an in-memory `*ddb.Client`, and (b) a real `DynamoDB-local`/LocalStack instance when an env flag (`DDBSQLITE_CONF_TARGET=dynamodb-local`) is set. Continuous cross-validation against the reference during development without a hard CI dependency.
3. **Fuzzing** (`go test -fuzz`): the expression lexer/parser/evaluator for panics and for "parse → eval → no panic on arbitrary input"; valid-expression round-trip stability.
4. **Golden corpus** of real DynamoDB edge cases (null vs missing attributes, type-mismatched comparisons, sparse GSIs, empty containers, `Limit=0`, `LastEvaluatedKey` resume) encoded as conformance cases — exactly where "semantically faithful" is most likely to silently break.

The conformance suite doubles as living documentation of supported behavior and the regression net when the engine changes.

## 10. Architecture approach: in-Go expression evaluation

Selected approach (over SQL push-down / hybrid): SQLite narrows only by **exact key operations** that map cleanly to its indexes — partition-key equality and sort-key range conditions for `Query`, and GSI key tables for indexed access. All **condition, filter, and update expressions** are evaluated in Go against decoded `AttributeValue` maps.

Rationale: DynamoDB semantics diverge from SQL precisely where "faithful" implementations break — missing-vs-null attributes, exact type matching, arbitrary-precision number comparison, set operations, `size()`, nested document paths. Evaluating in Go gives full control over each; a single evaluator serves condition, filter, and update expressions. Filter expressions filtering in Go (not pushed into SQLite) is less efficient for very large tables but matches DynamoDB's own behavior (filters don't reduce read capacity), and unit-test tables are small.

## 11. Implementation milestones

Build strategy: **walking skeleton first, then widen.** A thin vertical slice runs end-to-end as early as Milestone 1, then each later milestone widens one concern. This de-risks SDK type marshaling and the wire-JSON contract early and gives a runnable system to validate against `dynamodb-local` from the first milestone.

### Dependency layers

```
num.Decimal ──┐
              ├─→ attrval.Value ──┬─→ storage ──┐
              ┘                   │             ├─→ ddb (engine) ──→ awsdynamodb ──→ conformance suite
                                  └─→ expr ─────┘
```

- `num` + `attrval`: zero internal deps — the foundation.
- `storage` and `expr`: both depend only on `attrval`/`num`; **independent of each other**, so they can be built in parallel.
- `ddb` (engine) integrates `storage` + `expr`; it is the critical path.
- `awsdynamodb` (adapter) sits on `ddb` + `attrval`; the conformance suite sits on the adapter.

### Milestones

- **M0 — Foundations (parallel).** `num.Decimal` (exact decimal, comparison/ordering, canonical string — pure, fuzzable) and `attrval.Value` (tagged union, wire JSON round-trip, set dedup, document-path navigation).
- **M1 — Walking skeleton.** `storage` (`*sql.DB` open/config/pragmas, catalog bootstrap, table-name hashing, DDL generation); `ddb` table ops (`CreateTable`/`DescribeTable`/`ListTables`/`DeleteTable`); `ddb` basic items (`PutItem`/`GetItem` key-based with 400KB limit, `DeleteItem`); `awsdynamodb` adapter (type marshaling + error mapping for the above); conformance harness skeleton, runnable vs the adapter and vs `dynamodb-local`.
- **M2 — Expressions wired in.** `expr` (lexer/parser/AST, condition+filter evaluator, update evaluator, substitution); condition expressions into `Put`/`Update`/`Delete`; `UpdateItem` with update expressions + `ReturnValues`; filter expressions into `Query`/`Scan` (which arrive in M3).
- **M3 — Query/Scan & pagination.** `Query` (partition seek + sort-key range narrowing in SQLite, `ScanIndexForward`, `Limit`, `ExclusiveStartKey`/`LastEvaluatedKey`); `Scan` (full/GSI scan, parallel-scan segments).
- **M4 — GSI support.** Write-triggered GSI maintenance; GSI `Query`/`Scan` with read-time projection.
- **M5 — Batch & TTL.** `BatchWriteItem`/`BatchGetItem`; lazy TTL filtering on reads, `ExpireExpired`.
- **M6 — Hardening.** `UpdateTable` (GSI add/remove); full conformance golden corpus; fuzz pass; edge cases (null vs missing, type-mismatched comparisons, sparse GSIs, `Limit=0`).

M0's two packages are genuinely parallel; M1 `storage` and the table-ops can also overlap. The critical path is `attrval → storage → ddb → adapter`, with `expr` joining at M2. Each milestone is independently testable against the conformance harness.
