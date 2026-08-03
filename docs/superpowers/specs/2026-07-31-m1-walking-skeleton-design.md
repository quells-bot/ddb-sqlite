# ddb-sqlite M1 — Walking Skeleton Design

**Date:** 2026-07-31
**Status:** Approved (brainstorming) → pending implementation plan
**Parent spec:** `docs/superpowers/specs/2026-07-31-ddb-sqlite-design.md` (§11, M1 milestone)
**Prerequisite:** M0 complete — `internal/num.Decimal` and `attrval.Value` (wire round-trip, sets, document paths) merged on `main`.

## 1. Overview & goal

M1 is the walking skeleton: a thin vertical slice that runs end-to-end from the SDK-facing adapter down to SQLite. It proves the wire-JSON contract, SDK type marshaling, the storage layer, and the conformance harness against the in-memory adapter — all before expressions (M2), Query/Scan (M3), and GSI (M4) widen the system.

M1 delivers:

- `internal/storage` — opens/configures the `*sql.DB`, bootstraps the catalog, generates per-table DDL, and owns all SQL.
- `ddb` — the engine: table ops (`CreateTable`/`DescribeTable`/`ListTables`/`DeleteTable`) and key-based item ops (`PutItem`/`GetItem`/`DeleteItem`) with validation, item-size limit, and typed errors.
- `awsdynamodb` — a **separate Go module** that depends on the AWS SDK v2 and implements the supported `DynamoDBAPI` subset via type marshaling and error mapping.
- A **dual-target conformance harness** (designed in full, implemented in two passes) parameterized by `dynamodb.API`.

## 2. Package layout & module structure

```
ddb-sqlite/                         # module github.com/quells-bot/ddb-sqlite-core (SDK-free)
├─ go.mod                           # adds modernc.org/sqlite in M1
├─ attrval/                         # (done in M0) Value, wire, sets, paths
├─ internal/
│  ├─ num/                          # (done in M0)
│  ├─ storage/                      # NEW: owns *sql.DB, schema bootstrap, DDL, all SQL
│  │  ├─ store.go                   # *Store: Open, pragmas, BeginTx, Close
│  │  ├─ catalog.go                 # ddb_table_defs / ddb_gsi_defs CRUD
│  │  ├─ tables.go                  # per-table data-table DDL gen + exec
│  │  └─ naming.go                  # table-name hashing (16-hex SHA-256)
│  └─ (expr/ arrives M2)
├─ ddb/                             # NEW: the engine — orchestration + policy, no SQL
│  ├─ client.go                     # *Client wraps *storage.Store; Open/Close
│  ├─ tables.go                     # CreateTable/DescribeTable/ListTables/DeleteTable
│  ├─ items.go                      # PutItem/GetItem/DeleteItem (key-based)
│  └─ errors.go                     # exported typed errors
└─ awsdynamodb/                     # SEPARATE MODULE (new go.mod)
   ├─ go.mod                        # requires AWS SDK v2 + this repo (replace => ..)
   ├─ adapter.go                    # *Adapter implements supported DynamoDBAPI subset
   ├─ marshal.go                    # types.AttributeValue <-> attrval.Value
   └─ conformance_test.go           # parameterized DynamoDBAPI suite (dual-target)
```

### Two Go modules in the repo

1. **Root module** (`github.com/quells-bot/ddb-sqlite-core`) — SDK-free. Adds `modernc.org/sqlite` as its only new dependency. Houses `attrval`, `internal/num`, `internal/storage`, and `ddb`.
2. **`awsdynamodb/` module** (`github.com/quells-bot/ddb-sqlite-core/awsdynamodb`) — its own `go.mod` that `require`s the AWS SDK v2 (`github.com/aws/aws-sdk-go-v2/service/dynamodb` + `types`) and `require`s + `replace`s the root module to `..`. The SDK dependency is isolated here; the root never imports it.

### Dependency direction

`attrval` ← `storage`/`ddb`; `storage` ← `ddb`; `ddb` ← `awsdynamodb`. No edge points inward toward `attrval`/`num`. `ddb` imports `storage` and `attrval` but never writes SQL.

**What `ddb.Client` holds:** a `*storage.Store` (which owns the `*sql.DB` and the driver registration). `ddb.Open` delegates to `storage.Open`; `Client.Close` delegates to `Store.Close`. The engine is a thin orchestration layer — it validates inputs, marshals items to wire JSON, computes hashes via `storage`, and begins/commits transactions, while `storage` issues every SQL statement on the provided `*sql.Tx`.

## 3. `internal/storage`: SQL ownership & schema

`storage` is the only package that touches SQL. It owns the `*sql.DB`, driver registration, pragmas, catalog bootstrap, DDL generation, and all data-table statements. It imports only `database/sql`, stdlib, and `modernc.org/sqlite` (for the driver import). It **never** imports `attrval` or `num` — it deals in `TableDef`, raw key values, and opaque `[]byte` item blobs.

### `*Store` lifecycle (`store.go`)

```go
package storage

type Store struct { db *sql.DB }

// Open registers the modernc driver (import for side effect), opens the DSN,
// sets MaxOpenConns=1 / MaxIdleConns=1 / ConnMaxLifetime=0 (serialized single
// writer), runs pragmas (journal_mode, foreign_keys=ON), and bootstraps the
// catalog tables if absent. One Store = one SQLite DB = one region.
func Open(ctx context.Context, dsn string) (*Store, error)

// BeginTx wraps db.BeginTx; every mutating op runs all statements on one tx.
func (s *Store) BeginTx(ctx context.Context) (*sql.Tx, error)
func (s *Store) Close() error
```

Driver: `modernc.org/sqlite` imported for side-effect registration; opened as `sql.Open("sqlite", dsn)`. Pragmas run once via a single `db.Exec` on open: `PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON;`.

`SetMaxOpenConns(1)` / `SetMaxIdleConns(1)` / `SetConnMaxLifetime(0)` serialize access: at most one logical connection in flight at a time; concurrent ops queue on the pool — the desired single-writer / serialized-access behavior, with no custom locking. Within a transaction, every statement is issued through the `*sql.Tx`, never through the parent `*sql.DB` (otherwise it would deadlock waiting for the conn the tx already holds).

### Catalog tables (bootstrapped once on Open)

```sql
CREATE TABLE IF NOT EXISTS ddb_table_defs (
  id INTEGER NOT NULL PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,          -- DynamoDB table name
  hash TEXT NOT NULL,                 -- partition key attribute name
  range TEXT,                         -- sort key attribute name (NULL if none)
  hash_type TEXT NOT NULL,            -- S | N | B
  range_type TEXT,                    -- required iff range is set (S | N | B)
  ttl TEXT,                           -- TTL attribute name (NULL = none)
  meta TEXT NOT NULL                  -- JSON: class, creationTime, ...
) STRICT;

CREATE TABLE IF NOT EXISTS ddb_gsi_defs (
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

`ddb_gsi_defs` is created now but unused until M4; its FK (`table_id REFERENCES ddb_table_defs(id)`) is why `foreign_keys=ON` matters from M1.

### Table-name hashing (`naming.go`)

`func TableName(name string) string` → `"ddb_" + 16 hex of SHA-256(name)`. GSI tables append `_` + 16 hex of SHA-256(gsiName), but that's M4; M1 exposes only the data-table hashing.

### DDL generation (`tables.go`)

`CreateDataTable(tx, def TableDef) error` generates and executes the per-table DDL, picking the key-column type from the key's DynamoDB type:

| DynamoDB key type | SQLite column type |
|---|---|
| S (String) | `TEXT` |
| N (Number) | `REAL` |
| B (Binary) | `BLOB` |

```sql
CREATE TABLE ddb_<hash> (
  id INTEGER NOT NULL PRIMARY KEY,
  hash <TYPE> NOT NULL,
  range <TYPE>,            -- present + NOT NULL iff table has a sort key
  data BLOB NOT NULL,
  ttl INTEGER,             -- NULL when no TTL attribute; populated M5
  UNIQUE (hash, range)      -- UNIQUE(hash) when no sort key
) STRICT;
```

`DropDataTable(tx, name string) error` drops `ddb_<hash>`.

**Caveat (parent spec §3.4):** N-typed keys rely on float64 ordering in the SQLite index — correct for normal-precision keys, theoretically divergent for keys beyond float64 precision. Acceptable for a test mock; the exact value is preserved in the JSON blob and Go-side comparison uses `num.Decimal`. Documented via a code comment at the REAL column generation.

### Catalog CRUD (`catalog.go`)

Typed methods that take the `*sql.Tx` so the engine keeps them in one transaction with the DDL:

```go
InsertTableDef(tx, def TableDef) (int64 id, error)
GetTableDef(tx, name string) (TableDef, error)        // storage.ErrNotFound if absent; ddb maps to ErrTableNotFound
ListTableDefs(tx) ([]TableDef, error)
ListTableDefsPage(tx, start string, limit int) ([]TableDef, error)  // pagination
DeleteTableDef(tx, id int64) error
TableExists(tx, name string) (bool, error)
```

`TableDef` is a `storage` struct mirroring a catalog row: `{ID int64, Name, Hash, Range, HashType, RangeType, TTL string, Meta json.RawMessage}`. M1 populates `Meta` with `{class, creationTime}`; GSIs are M4.

### Data-table statements

```go
PutItem(tx, table string, hashVal, rangeVal any, data []byte) error   // INSERT OR REPLACE
GetItem(tx, table string, hashVal any, rangeVal any) (data []byte, found bool, error)
DeleteItem(tx, table string, hashVal any, rangeVal any) (found bool, error)
```

Key values are passed as Go `any` (string for S, float64 for N, []byte for B) so SQLite's affinity binding matches the column type. The `data` BLOB is the wire-JSON item bytes (produced by `ddb` via `attrval`); `storage` treats it as opaque bytes.

### Boundary invariant

`storage` imports only `database/sql` + stdlib + `modernc.org/sqlite` (for the driver import). It never imports `attrval` or `num` — it deals in `TableDef`, raw key values, and opaque `[]byte` item blobs. All `attrval` marshaling and DynamoDB-policy decisions live in `ddb`.

## 4. `ddb`: engine orchestration & policy

`ddb` is the importable engine surface. It validates inputs, marshals items to wire JSON via `attrval`, computes key values for storage, and wraps typed errors. It never writes SQL — every persistence call goes through `*storage.Store`.

### `*Client` lifecycle (`client.go`)

```go
package ddb

type Options struct {
    DSN string   // file path, ":memory:", or "file:...?..." URI
}

type Client struct { store *storage.Store }

// Open delegates to storage.Open (driver, pragmas, catalog bootstrap).
func Open(ctx context.Context, opts Options) (*Client, error)
func (c *Client) Close() error
```

### Error types (`errors.go`)

Typed sentinel errors matching parent spec §6.6, returned by value so `errors.Is` works and the adapter can map each to an SDK exception:

```go
var (
    ErrResourceNotFound   = errors.New("ddb: resource not found")
    ErrTableNotFound      = errors.New("ddb: table not found")
    ErrTableInUse         = errors.New("ddb: table already exists")
    ErrValidation         = errors.New("ddb: validation error")
    ErrConditionalCheck   = errors.New("ddb: conditional check failed")  // used from M2
)
```

M1 emits `ErrTableNotFound`, `ErrTableInUse`, `ErrValidation`. `ErrConditionalCheck` is defined now (adapter maps it to `ConditionalCheckFailedException`) but no M1 op fails it — condition expressions arrive M2.

### Table ops (`tables.go`)

```go
type KeySchemaElement struct { AttributeName string; KeyType string }  // "HASH" | "RANGE"
type AttributeDefinition  struct { AttributeName string; AttributeType string }  // "S"|"N"|"B"

type TableDescription struct {
    Name, Hash, Range, HashType, RangeType, TTL string
    CreationTime time.Time
    // GSIs, billing, etc. filled in later milestones
}

CreateTable(ctx, CreateTableInput) (TableDescription, error)
DescribeTable(ctx, DescribeTableInput) (TableDescription, error)
ListTables(ctx, ListTablesInput) (ListTablesOutput, error)
DeleteTable(ctx, DeleteTableInput) error
```

`CreateTable` flow (one transaction):
1. Validate name non-empty; key schema has exactly one HASH; RANGE optional; key types ∈ {S,N,B}; at least one `AttributeDefinition` covers each key attribute. → `ErrValidation` on any failure.
2. `store.TableExists(tx, name)` → `ErrTableInUse` if present.
3. Build `storage.TableDef` (hash the table name, map key types → column types, set `Meta={class:"STANDARD", creationTime:now}`). `UpdateTable` is **not** in M1; `UpdateTimeToLive` is M5.
4. `store.InsertTableDef(tx, def)` then `store.CreateDataTable(tx, def)` — both on the same tx; commit atomically. Rollback on any error.
5. Return the `TableDescription`.

`DescribeTable` → `store.GetTableDef` → `ErrTableNotFound` if absent; map to `TableDescription`.

`ListTables` → `store.ListTableDefsPage(tx, ExclusiveStartTableName, Limit)` (default limit 100, capped). Returns names + `LastEvaluatedTableName` when more remain. Faithful pagination.

`DeleteTable` → `GetTableDef` (ErrTableNotFound if absent) → `DropDataTable` + `DeleteTableDef` on one tx.

### Item ops (`items.go`) — key-based only, no expressions

```go
type Item map[string]attrval.Value

PutItem(ctx, PutItemInput) error
GetItem(ctx, GetItemInput) (Item, error)        // empty Item if not found
DeleteItem(ctx, DeleteItemInput) error
```

**`PutItem`** (one transaction):
1. `GetTableDef` → resolve table, key schema, column types. `ErrTableNotFound` if absent.
2. Validate the `Item` carries the partition key attribute (and sort key if the table has one); their `attrval.Value` tags match the declared key types. → `ErrValidation` otherwise.
3. Item-size limit: marshal the item to wire JSON (`json.Marshal(item)`) and reject if `len(bytes) > 400*1024` → `ErrValidation`. JSON-byte-length proxy — faithful enough for a test mock; full accounting deferred to M6.
4. Extract key column values: S→`string`, N→`float64` (via `num.Decimal` → `float64` for the indexed column; exact string preserved inside the JSON blob), B→`[]byte`.
5. `store.PutItem(tx, table, hashVal, rangeVal, wireBytes)` → commit.

**`GetItem`**:
1. Resolve table. Validate the supplied key has exactly the table's key attributes with matching types. → `ErrValidation` otherwise.
2. `store.GetItem(tx, table, hashVal, rangeVal)` → if `!found`, return empty `Item` (DynamoDB returns no item, no error — the adapter maps to no `Item` field). If found, `json.Unmarshal` the blob into `Item` and return.

**`DeleteItem`**:
1. Resolve table, validate key.
2. `store.DeleteItem(tx, table, hashVal, rangeVal)`. No error if the key didn't exist (DynamoDB idempotency). M2 adds the condition-expression pre-check here.

**No condition/filter/update expressions in M1.** `PutItemInput`/`DeleteItemInput` carry no expression fields. They arrive in M2. `ConsistentRead` on `GetItem` is accepted and ignored (always consistent). `ReturnValues` is irrelevant for Put/Delete and omitted.

### Boundary invariant

`ddb` imports `attrval` (for `Item`/`Value`), `internal/num` (for N→float64 conversion), and `internal/storage`. It never imports `database/sql` directly, never builds SQL, and never sees `[]byte` as anything but an item blob to hand to storage. All DynamoDB semantic policy — type validation, item-size, key resolution — is here.

## 5. `awsdynamodb`: adapter, marshaling & error mapping

The adapter is a separate Go module that depends on the AWS SDK v2 and implements the supported subset of `DynamoDBAPI`, translating SDK types ↔ core. It is goroutine-safe because `*ddb.Client` is; no extra locking.

### Module wiring (`awsdynamodb/go.mod`)

```go
module github.com/quells-bot/ddb-sqlite-core/awsdynamodb

require (
    github.com/aws/aws-sdk-go-v2/service/dynamodb <ver>
    github.com/quells-bot/ddb-sqlite-core <pseudo-ver>
)
replace github.com/quells-bot/ddb-sqlite-core => ..
```

The root module stays SDK-free; the SDK is pulled only here.

### `*Adapter` (`adapter.go`)

```go
package awsdynamodb

type Adapter struct { client *ddb.Client }

func New(client *ddb.Client) *Adapter
```

Implements the `DynamoDBAPI` methods for M1's supported set: `CreateTable`, `DescribeTable`, `ListTables`, `DeleteTable`, `PutItem`, `GetItem`, `DeleteItem`. Each method:

1. Translates the SDK `*Input` struct → the corresponding `ddb.*Input` (key schema, attribute definitions, item).
2. Calls `client.<Op>(ctx, input)`.
3. Translates the `ddb` result → the SDK `*Output` struct.
4. Maps `ddb.Err*` → the matching SDK exception type.

### AttributeValue marshaling (`marshal.go`)

`types.AttributeValue` → `attrval.Value` and back, a 1:1 mapping over all ten tags:

| SDK `types.AttributeValue` member | `attrval.Tag` | Notes |
|---|---|---|
| `&types.AttributeValueMemberString{Value}` | String | direct |
| `&types.AttributeValueMemberNumber{Value}` | Number | parse via `attrval.NewNumberString` (validates precision/range) |
| `&types.AttributeValueMemberBinary{Value}` | Binary | copy bytes |
| `&types.AttributeValueMemberBoolean{Value}` | Boolean | direct |
| `&types.AttributeValueMemberNull{Value}` | Null | direct |
| `&types.AttributeValueMemberList{Value}` | List | recurse over elements |
| `&types.AttributeValueMemberMap{Value}` | Map | recurse over values |
| `&types.AttributeValueMemberStringSet{Value}` | StringSet | dedup via `attrval.NewStringSet` |
| `&types.AttributeValueMemberNumberSet{Value}` | NumberSet | each parsed via `NewNumberString`, dedup via `NewNumberSet` |
| `&types.AttributeValueMemberBinarySet{Value}` | BinarySet | dedup via `attrval.NewBinarySet` |

Reverse direction reconstructs the SDK discriminated-union members. A Number that fails `NewNumberString` validation → `ValidationException`. Unknown/unsupported member → `ValidationException`.

### Error mapping

| `ddb` error | SDK exception |
|---|---|
| `ErrTableNotFound` | `*types.ResourceNotFoundException` |
| `ErrTableInUse` | `*types.ResourceInUseException` |
| `ErrValidation` | `*types.ValidationException` |
| `ErrConditionalCheck` (M2) | `*types.ConditionalCheckFailedException` |

Returned as typed `errors` so `errors.As(out, &target)` works in tests — a hard requirement of the conformance suite.

### Input/output translation specifics

- **CreateTable:** SDK `KeySchema`, `AttributeDefinitions` → `ddb.KeySchemaElement`/`ddb.AttributeDefinition`. `BillingMode`/provisioned throughput accepted and ignored (provisioning out of scope). `SSESpecification`, `StreamSpecification` accepted and ignored. `GlobalSecondaryIndexes` → **rejected with `ValidationException` in M1** (GSIs are M4; creating a table with GSIs in M1 is out of scope, not silently dropped). Returns `TableDescription` populated into `CreateTableOutput.Table`.
- **DescribeTable/ListTables/DeleteTable:** straight pass-throughs returning SDK `*TableDescription`/`*ListTablesOutput`.
- **PutItem/GetItem/DeleteItem:** SDK `map[string]types.AttributeValue` ↔ `ddb.Item`. `ConditionExpression`/`UpdateExpression`/expression-attribute maps: **not present** in M1 input translation (no expression fields accepted); they land in M2. `ReturnValues` on Put/Delete ignored for M1.

### Boundary invariant

`awsdynamodb` imports `ddb`, `attrval`, and the AWS SDK v2. It never imports `internal/*`. All expression strings, names, and values maps pass through unchanged once M2 adds them — but M1 accepts none.

## 6. Conformance harness: dual-target, two-pass implementation

The parent spec (§9) calls for a conformance suite parameterized by the SDK's `DynamoDBAPI` interface, runnable against both our adapter and a real `dynamodb-local`. M1 **designs the full dual-target harness** but implements it in two passes: pass 1 is the adapter target wired and running; pass 2 is the `dynamodb-local` target via `github.com/ory/dockertest/v4`.

### Harness shape

```go
// awsdynamodb/conformance_test.go

type confTarget struct {
    name string
    ctor func(t *testing.T) (dynamodb.API, func())  // API + cleanup
}

var targets = []confTarget{
    {"adapter", newAdapterTarget},        // pass 1: implemented in M1
    {"dynamodb-local", newLocalTarget},   // pass 2: stubbed in M1
}
```

- **Parameterized by `dynamodb.API`** — the SDK's `DynamoDBAPI`-equivalent interface. Every case is written against the interface, not against `*Adapter`, so both targets run the identical assertions.
- **Target selection by env:** `DDBSQLITE_CONF_TARGET`. Default (unset) = run `adapter` only. `=all` = run every active target. `=dynamodb-local` = run only the local target. `t.Skip` when a target's prereqs are absent.
- **Each test does `t.Run(target.name, ...)`**, so `go test ./awsdynamodb` runs the adapter target by default and the local target only when explicitly enabled.

### Pass 1 (M1) — adapter target

`newAdapterTarget` constructs an in-memory `*ddb.Client` (`ddb.Open(ctx, ddb.Options{DSN: ":memory:"})`), wraps it in `*awsdynamodb.Adapter`, and returns the `dynamodb.API` plus a cleanup that closes the client. Fully hermetic, no Docker. This is the target that proves the M1 walking skeleton end-to-end.

### Pass 2 (M1 design, later activation) — dynamodb-local via `ory/dockertest`

`newLocalTarget` is a **designed stub** in M1 that, when activated, uses `github.com/ory/dockertest/v4`:

```go
// awsdynamodb/conformance_test.go (pass 2 activation)

func newLocalTarget(t *testing.T) (dynamodb.API, func()) {
    // 1. Honor an explicit external endpoint if provided (CI with a
    //    pre-existing dynamodb-local):
    if ep := os.Getenv("DDBSQLITE_CONF_LOCAL_ENDPOINT"); ep != "" {
        return newLocalClient(t, ep), func() {}
    }
    // 2. Otherwise spin up an ephemeral dynamodb-local container via dockertest.
    pool, err := dockertest.NewPool("")             // uses local docker socket
    // resource, err := pool.Run("amazon/dynamodb-local", "<tag>", nil)
    // ... pool.Retry waits for the port to accept connections ...
    endpoint := "http://localhost:" + resource.GetPort("8000/tcp")
    cleanup := func() { _ = pool.Purge(resource) }  // tear down the container
    return newLocalClient(t, endpoint), cleanup
}
```

- **Lifecycle:** `pool.Run` starts `amazon/dynamodb-local`, `cleanup` purges it — one container per test that selects this target, no leaked state between runs.
- **Readiness:** `pool.Retry` polls the port until dynamodb-local accepts the `ListTables` handshake before handing the client to the case.
- **Dependency:** `ory/dockertest/v4` is a **test-only** dependency in `awsdynamodb/go.mod`. It pulls Docker SDK bindings but never enters the shipped adapter code — only `conformance_test.go`.
- **Env escape hatch:** `DDBSQLITE_CONF_LOCAL_ENDPOINT` lets a pre-provisioned endpoint (CI-managed, or a developer's long-running local) bypass container creation — `dockertest` only runs when the env is unset and Docker is available. If Docker is absent → `t.Skip("dynamodb-local target requires Docker or DDBSQLITE_CONF_LOCAL_ENDPOINT")`.
- **Activation is pass 2:** the `dockertest` wiring, the `amazon/dynamodb-local` image tag pin, and the readiness/retry logic are designed now but implemented in the second pass. The committed M1 stub is the env check + `t.Skip` with a message naming the two activation paths.

### M1 conformance cases

All written against `dynamodb.API`, exercising the M1 supported set:

1. **CreateTable + DescribeTable** — create a table, describe it, assert name/key-schema/attribute-types match. Describe a missing table → `ResourceNotFoundException`.
2. **ListTables** — create several, list, assert presence; pagination via `ExclusiveStartTableName`/`Limit`.
3. **DeleteTable** — delete, then describe → `ResourceNotFoundException`. Delete a missing table → `ResourceInUseException`/`ResourceNotFoundException` per AWS.
4. **PutItem + GetItem** — put an item, get it back, assert deep equality of all attribute types (S, N, B, BOOL, NULL, L, M, SS, NS, BS). Get a missing key → empty result (no `Item`), no error.
5. **PutItem overwrite** — put the same key twice, assert the second value wins.
6. **DeleteItem** — delete an existing item, get → empty; delete a missing key → no error (idempotent).
7. **PutItem 400KB limit** — put a >400KB item → `ValidationException`.
8. **PutItem key validation** — missing partition key, or key type mismatch (declared N, supplied S) → `ValidationException`. Includes a malformed NumberSet member → `ValidationException`.
9. **Unknown table** — Put/Get/Delete on a nonexistent table → `ResourceNotFoundException`.

### Boundary invariant

`conformance_test.go` imports only the AWS SDK v2 (for `dynamodb.API` and `types`) and the adapter constructor. It never imports `ddb`/`attrval`/`internal` directly — it drives everything through the SDK interface, which is what makes the dual-target swap work. The suite is the same file for both targets; only the `confTarget.ctor` differs.

## 7. Testing strategy & verification

M1's test layers, each hermetic and independent:

### Layer 1 — `internal/storage` unit tests (`storage/*_test.go`)

Tests open an in-memory `*Store` (`Open(ctx, ":memory:")`) and exercise SQL directly, no `attrval`:

- **Catalog round-trips:** `InsertTableDef`/`GetTableDef`/`ListTableDefsPage`/`DeleteTableDef` — insert, fetch by name, paginate, delete; `GetTableDef` on a missing name returns a not-found sentinel the engine maps to `ErrTableNotFound`.
- **DDL generation:** `CreateDataTable` produces a table whose key columns have the right affinity (TEXT for S, REAL for N, BLOB for B); `UNIQUE(hash, range)` vs `UNIQUE(hash)` enforced by inserting duplicate keys and expecting a constraint error. `DropDataTable` drops it.
- **Data-table statements:** `PutItem`/`GetItem`/`DeleteItem` with raw `any` key values and opaque `[]byte` blobs — round-trip the blob bytes, `GetItem` on a missing key returns `found=false`, `DeleteItem` on a missing key is idempotent (`found=false`, no error).
- **Table-name hashing:** `TableName` is deterministic and collision-resistant across distinct names.
- **Bootstrap idempotency:** `Open` twice on the same file DSN does not error on catalog creation (`CREATE TABLE IF NOT EXISTS`).

### Layer 2 — `ddb` engine unit tests (`ddb/*_test.go`)

Tests drive `*Client` with `attrval.Item` inputs, no SQL, no SDK:

- **Table ops:** `CreateTable` validates name/key schema/key types (rejects empty name, missing HASH, type ∉ {S,N,B}, unmatched `AttributeDefinition`); `CreateTable` on an existing name → `ErrTableInUse`; `DescribeTable`/`DeleteTable` on unknown → `ErrTableNotFound`; `ListTables` pagination returns `LastEvaluatedTableName` correctly.
- **Item ops:** `PutItem`+`GetItem` round-trips an item exercising all ten `attrval` tags; `PutItem` rejects missing key attribute / type mismatch / >400KB with `ErrValidation`; `PutItem` overwrites on same key; `DeleteItem` makes a subsequent `GetItem` return empty; ops on unknown table → `ErrTableNotFound`.

### Layer 3 — `awsdynamodb` conformance suite (`awsdynamodb/conformance_test.go`)

The dual-target harness from Section 6, pass 1 active. The 9 M1 cases exercise the adapter end-to-end through `dynamodb.API`: marshaling all attribute types, error mapping, and the M1 op set. This is the proof that the walking skeleton runs.

### Verification gate for M1 completion

M1 is done when **all three layers pass hermetically** (`go test ./...` from the repo root for the SDK-free module, and `go test ./...` from `awsdynamodb/` for the adapter module) and the conformance suite's adapter target is green. The `dynamodb-local` target is `t.Skip` in M1; activating it is pass 2. A smoke run of `go test ./awsdynamodb` against the adapter target, plus `go vet ./...` across both modules, is the closing check.

### What M1 does NOT test

No condition/filter/update expressions (M2), no Query/Scan (M3), no GSI (M4), no batch/TTL (M5). The 400KB test is a boundary check, not a full item-size-accounting suite (deferred to M6 hardening, per the JSON-byte proxy decision).

## 8. Open questions, risks & out-of-scope recap

### Open questions resolved (captured for the record)

| Decision | Choice |
|---|---|
| Conformance harness scope | Full dual-target design; implement in two passes; Docker deferred to pass 2 |
| 400KB item-size limit | JSON byte-length proxy (`len(marshalWire(item)) > 400*1024`) |
| `UpdateTable` in M1 | Excluded (GSI add/remove is M6); `UpdateTimeToLive` is M5 |
| Adapter SDK dependency | Real AWS SDK v2 dependency now |
| Module layout | `awsdynamodb/` separate module + `replace => ..` |
| `dynamodb-local` target | `ory/dockertest/v4` in pass 2 |

### Risks & mitigations

1. **Number sort key stored as REAL diverges from exact decimal.** Parent spec §3.4 already documents this: N-typed keys rely on float64 ordering in the SQLite index. Correct for normal-precision keys; theoretically divergent beyond float64. **Mitigation:** acceptable for a test mock; the exact value is preserved in the JSON blob and Go-side comparison uses `num.Decimal`. Surfaced via a code comment at the REAL column generation; no test in M1 exercises beyond-float64 keys.

2. **`STRICT` tables + BLOB affinity.** modernc's `STRICT` mode enforces column types strictly; `data BLOB` must accept arbitrary bytes. **Mitigation:** verify in the storage unit tests that a `BLOB` column round-trips binary item blobs; the DDL uses `BLOB` not `TEXT` for `data` so no coercion surprises.

3. **Separate-module test resolution.** `go test ./awsdynamodb` must resolve the root module via `replace`. **Mitigation:** the conformance suite runs from the `awsdynamodb/` module dir; `go vet ./...` across both modules is the gate. CI must run `go test` in both the root and `awsdynamodb/` (or use `go work` — not introduced in M1 to avoid extra tooling).

4. **`CREATE TABLE IF NOT EXISTS` on reopen.** `Open` must be idempotent. **Mitigation:** catalog bootstrap uses `IF NOT EXISTS`; storage unit test reopens a file DSN.

5. **Adapter marshaling of NumberSet with invalid members.** A single bad number in an NS → `ValidationException`, not a partial parse. **Mitigation:** `marshal.go` validates each member via `NewNumberString`; the conformance case "PutItem key validation" covers the scalar Number path, and an added case covers a malformed NumberSet member.

### Explicitly out of scope for M1

- Condition, filter, update expressions (M2) — `PutItem`/`DeleteItem` accept no expression fields.
- `Query`, `Scan` (M3); GSI create/maintain/query (M4); `BatchWriteItem`/`BatchGetItem`/TTL (M5).
- `UpdateTable` (M6), `UpdateTimeToLive` (M5), `UpdateItem` with update expressions (M2).
- `ProjectionExpression` (v1 non-goal), `TransactWriteItems`/PartiQL (v1 non-goal).
- Full item-size accounting (deferred to M6 hardening; M1 uses the proxy).
- The `dynamodb-local` Docker launcher itself (pass 2 activation).
- `go.work` workspace file (not introduced in M1).
