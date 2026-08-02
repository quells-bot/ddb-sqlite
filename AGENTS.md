# Repository Guidelines

## Project Overview

`ddb-sqlite` is an **in-process mock of a subset of the AWS DynamoDB API**, implemented in Go and backed by a SQLite database. Its purpose: a drop-in replacement for the AWS SDK v2 DynamoDB client in unit tests — no external process, no network, no CGO. One engine instance models one AWS "region" backed by one SQLite database. See `IDEA.md` and `docs/superpowers/specs/2026-07-31-ddb-sqlite-design.md` for the full design.

Key design constraints (from the approved spec):
- The **core library must not depend on the AWS SDK**. A separate adapter package (`awsdynamodb/`) provides SDK integration and is its own Go module.
- Uses **CGO-free `modernc.org/sqlite`** to avoid cross-compilation issues.
- **Serialized single-writer** semantics (one connection). Not high-concurrency; eventual consistency is out of scope.
- Expressions (condition/filter/update) are **evaluated in Go**, not pushed to SQL. SQLite narrows only by key operations.

## Architecture & Data Flow

```
num.Decimal ──┐
              ├─→ attrval.Value ──┬─→ storage ──┐
              ┘                   │             ├─→ ddb (engine) ──→ awsdynamodb ──→ conformance suite
                                  └─→ expr ─────┘
```

**Layers (dependency order, inner must not import outer):**

| Package | Role | Importable? |
|---|---|---|
| `internal/num` | Exact decimal type for DynamoDB Numbers (no float64). `big.Int` coef × 10^(-scale). | internal only |
| `attrval` | Tagged-union value model mirroring DynamoDB `AttributeValue`; wire-JSON encode/decode; set dedup; document-path navigation. | **yes (public)** |
| `internal/storage` | SQLite plumbing: opens/configures `*sql.DB`, bootstraps catalog, generates per-table DDL, issues all SQL. Deals in `TableDef`, raw key values, opaque `[]byte` blobs — never imports `attrval`/`num`. | internal only |
| `internal/expr` | Expression parser/evaluator (condition, filter, update). Complete as of M2: lexer, both grammars, bind, condition/filter evaluator, and update evaluator. | internal only |
| `ddb` | The engine: validates inputs, marshals items to wire JSON via `attrval`, computes key values, delegates persistence to `storage`. **Never writes SQL.** | **yes (public)** |
| `awsdynamodb` | Adapter: implements the supported subset of SDK `DynamoDBAPI` by translating to `*ddb.Client`. Separate Go module so the SDK dep is isolated. | own module |

**Why `attrval` is public but `num`/`expr` aren't:** the adapter must translate SDK `types.AttributeValue` ↔ `attrval.Value`, and `attrval.Value` appears in core public signatures → must be importable post-split. `num` is an impl detail of `attrval`; `expr` is never named by the adapter (it passes raw expression strings through). No SDK types leak into core signatures.

**Request flow (example: `PutItem`):**
1. `ddb.Client.PutItem` (`ddb/items.go`) — validates the key against the table's `TableDef` (`validateKey`), computes the indexed column values (`keyValue`: S→string, N→float64, B→[]byte), marshals the `Item` to wire JSON.
2. `storage.Store.PutItem` (`internal/storage/tables.go`) — inserts/replaces the `data` BLOB row on a single `*sql.Tx`.
3. The `*sql.Tx` is begun by `ddb` via `store.BeginTx`, committed there. Every mutating op runs all its statements on one tx; the tx is the serialization unit.

## Key Directories

```
ddb-sqlite/                      # module github.com/quells-bot/ddb-sqlite (SDK-free, go 1.25)
├── go.mod / go.sum
├── IDEA.md                      # design overview / intent
├── ddb/                         # IMPORTABLE engine: Client, table ops, item ops, exported errors
│   ├── client.go                # *Client wraps *storage.Store; Open/Close
│   ├── tables.go                # CreateTable/DescribeTable/ListTables/DeleteTable
│   ├── items.go                 # PutItem/GetItem/DeleteItem (+ Item type = map[string]attrval.Value)
│   ├── batch.go                 # BatchWriteItem/BatchGetItem (M5b): pre-validate-then-apply on one tx
│   ├── update.go                # UpdateItem
│   └── errors.go                # sentinel errors: ErrTableNotFound, ErrValidation, ...
├── attrval/                     # IMPORTABLE value model + wire JSON
│   ├── value.go                 # Value tagged union, New* constructors, typed accessors
│   ├── wire.go                  # MarshalJSON/UnmarshalJSON (DynamoDB wire shape)
│   ├── set.go                   # SS/NS/BS construction (deduped, canonically sorted)
│   ├── path.go                  # ParsePath/Lookup document paths (a.b[2].c)
│   ├── equal.go                 # Value.Equal (numeric equality, set element-wise)
│   └── fuzz_test.go             # wire round-trip, path, set-dedup fuzz
├── internal/
│   ├── storage/                 # SQLite plumbing
│   │   ├── store.go             # *Store, Open (driver+pragmas+MaxOpenConns=1), BeginTx, bootstrap
│   │   ├── catalog.go           # TableDef CRUD against ddb_table_defs/ddb_gsi_defs
│   │   ├── tables.go            # per-table DDL generation, PutItem/GetItem/DeleteItem
│   │   └── naming.go            # TableName(name) = "ddb_" + 16 hex of SHA-256(name)
│   ├── num/                     # exact decimal
│   │   ├── decimal.go           # Parse/String/Equal/Compare/Validate (38 sig digits, AWS range)
│   │   └── fuzz_test.go
│   └── expr/                    # expression lexer/parser/bind/evaluator (condition, filter, update)
│       ├── errors.go            # sentinel errors (lex/parse/bind/eval)
│       ├── lex.go               # tokenizer for expression strings
│       ├── ast.go               # expression AST node types
│       ├── parse.go             # recursive-descent parser
│       ├── bind.go              # expression attribute name/value binding
│       ├── eval.go              # condition/filter evaluator
│       ├── update.go            # update expression evaluator (SET/REMOVE/ADD/DELETE)
│       ├── reserved.go          # reserved words
│       └── *_test.go            # lex/parse/bind/eval unit + fuzz tests
├── awsdynamodb/                 # SEPARATE MODULE (go 1.26.5) — depends on AWS SDK v2
│   ├── go.mod                   # replace github.com/quells-bot/ddb-sqlite => ..
│   ├── adapter.go               # Adapter implements SDK DynamoDBAPI subset; mapError → SDK exceptions
│   ├── marshal.go               # FromSDK/ToSDK AttributeValue ↔ attrval.Value, FromSDKMap/ToSDKMap
│   ├── conformance_test.go      # parameterized conformance harness (adapter + dynamodb-local)
│   ├── adapter_test.go
│   └── marshal_test.go
└── docs/superpowers/specs/          # design specs (authoritative for behavior)
```

## Development Commands

No Makefile, CI configs, or lint configs exist. Use standard Go tooling.

```bash
# Build the SDK-free root module
go build ./...

# Test the root module (unit tests; in-memory SQLite)
go test ./...

# Test the adapter module (conformance suite against the in-process adapter only)
cd awsdynamodb && go test ./...

# Conformance against dynamodb-local (requires Docker/podman socket; auto-skips if unavailable)
cd awsdynamodb && DDBSQLITE_CONF_TARGET=dynamodb-local go test ./...
# Both targets:
cd awsdynamodb && DDBSQLITE_CONF_TARGET=all go test -count=1 ./...
# Or point at a pre-existing dynamodb-local endpoint:
cd awsdynamodb && \
  DDBSQLITE_CONF_TARGET=dynamodb-local \
  DDBSQLITE_CONF_LOCAL_ENDPOINT=http://localhost:8000 \
  go test ./...

# Fuzzing
go test ./internal/num/ -fuzz=FuzzParseRoundTrip -fuzztime=30s
go test ./attrval/      -fuzz=FuzzWireRoundTrip -fuzztime=30s

# Tidy both modules
go mod tidy && (cd awsdynamodb && go mod tidy)
```

Two Go modules must be built/tested independently:
- Root: `github.com/quells-bot/ddb-sqlite` (go 1.25, no AWS SDK)
- Adapter: `github.com/quells-bot/ddb-sqlite/awsdynamodb` (go 1.26.5, AWS SDK v2 + `ory/dockertest` for the dynamodb-local target; uses a `replace` directive pointing the root module at `..`)

## Code Conventions & Common Patterns

- **Package doc comments** open every file with `// Package <name> ...` describing role and invariants. Follow this for new packages.
- **Immutable-by-construction values.** `attrval.Value` is constructed via `New*` functions; do not mutate returned slices/maps. `num.Decimal` methods return new values; the zero `Decimal` is not valid — obtain via `Parse`.
- **Sentinel errors via `errors.New`.** `ddb/errors.go` exports typed errors (`ErrTableNotFound`, `ErrValidation`, `ErrResourceNotFound`, `ErrTableInUse`, `ErrConditionalCheck`) returned by value so `errors.Is` works. The adapter maps each 1:1 to an SDK exception type in `awsdynamodb/adapter.go` `mapError`. Add new engine errors here, not ad-hoc.
- **Error wrapping** with `fmt.Errorf("pkg: context: %w", err)` and package-prefixed messages (e.g. `"storage: ..."`, `"attrval: ..."`).
- **Validation at the wire boundary.** `attrval.NewNumberString` (and set variants) parse+validate DynamoDB limits; plain `NewNumber` trusts a pre-built decimal. Numbers parsed as strings at boundaries, decimals internally — never float64 for value semantics.
- **Sets are deduped + canonically sorted at construction** (`dedupStrings`/`dedupNumbers`/`dedupBytes` in `attrval/set.go`). NS dedups by numeric value (`1` == `1.0`). This invariant is relied on by `Equal`, `MarshalJSON`, and condition functions — preserve it.
- **Storage deals in opaque `[]byte` blobs**, not `attrval`/`num`. The `ddb` layer marshals items to wire JSON; `storage` only stores/retrieves the blob + key columns. Do not import `attrval` or `num` from `internal/storage`.
- **Single-writer transaction discipline.** `storage.Store` opens one `*sql.DB` with `MaxOpenConns(1)` (serialized pool). Every mutating op does `BeginTx` → all statements on that `*sql.Tx` → `Commit`. Never issue statements through the parent `*sql.DB` inside a tx (deadlock — the tx already holds the single conn).
- **SQLite tables are `STRICT`** and named `ddb_<16-hex-of-SHA256(name)>` (see `internal/storage/naming.go`). Number key columns are `REAL` (float64 index ordering — acceptable for a test mock; exact value preserved in the JSON blob). Per-table DDL is generated dynamically in `internal/storage/tables.go` `CreateDataTable`.
- **Input structs + result structs** mirror a subset of the DynamoDB API (e.g. `CreateTableInput`/`TableDescription` in `ddb/tables.go`, `PutItemInput`/`GetItemOutput` in `ddb/items.go`). Add new operations following this pattern.
- **Context-first signatures** throughout (`ctx context.Context, in ...Input`).
- **Pagination is faithful**: `ListTables` (`ddb/tables.go`) honors `ExclusiveStartTableName`/`Limit` with `LastEvaluatedTableName`; default/cap is 100. Preserve DynamoDB pagination semantics for any new list/query ops.

## Important Files

| File | Why it matters |
|---|---|
| `ddb/client.go` | Engine entry point: `Open(ctx, Options{DSN})` → `*Client`. One Client = one SQLite DB = one region. |
| `ddb/errors.go` | All exported engine errors. The adapter's `mapError` depends on this set. |
| `ddb/tables.go` | Table ops + `KeySchemaElement`/`AttributeDefinition`/`TableDescription` types. `analyzeCreateTable` validates key schemas. |
| `ddb/items.go` | `Item = map[string]attrval.Value`; `PutItem`/`GetItem`/`DeleteItem`; `validateKey`; 400KB limit (`maxItemBytes`). |
| `ddb/batch.go` | Batch ops: two-phase validate→apply, duplicate-key detection via canonical wire JSON, `validatePutKey` shared with `PutItem`. |
| `ddb/update.go` | `UpdateItem`: read-modify-write on one tx, upsert, key immutability, `ReturnValues` projection. |
| `internal/expr/update.go` | The update evaluator — `Apply` and the SET/REMOVE/ADD/DELETE action semantics. |
| `attrval/value.go` | The tagged-union `Value` and `Tag` enum — the core value model. |
| `attrval/wire.go` | `MarshalJSON`/`UnmarshalJSON` — the DynamoDB wire-JSON contract stored in the `data` BLOB. |
| `internal/storage/store.go` | `Open` + pragmas (`journal_mode=WAL`, `foreign_keys=ON`) + catalog bootstrap DDL. |
| `internal/storage/naming.go` | `TableName` hash scheme — the SQLite↔DynamoDB table-name mapping. |
| `awsdynamodb/adapter.go` | `Adapter` implementing SDK `DynamoDBAPI` subset; `mapError` engine-error→SDK-exception. |
| `awsdynamodb/marshal.go` | `FromSDK`/`ToSDK` ↔ `attrval.Value`; the only place SDK types touch core types. |
| `awsdynamodb/conformance_test.go` | The conformance harness — parameterized `api` interface, both adapter and dynamodb-local targets. |
| `docs/superpowers/specs/2026-07-31-ddb-sqlite-design.md` | The approved design spec — authoritative for behavior, schema, milestones. |
| `IDEA.md` | Original overview, schema sketch, API surface, goals/non-goals. |

## Runtime/Tooling Preferences

- **Go toolchain**: root module `go 1.25`; adapter module `go 1.26.5`. Both must build.
- **SQLite driver**: `modernc.org/sqlite` (CGO-free), imported for side-effect registration in `internal/storage/store.go`. **`CGO_ENABLED=0` is the intent** — do not introduce CGO deps.
- **No AWS SDK in the root module.** The SDK (`aws-sdk-go-v2`) lives only in `awsdynamodb/go.mod`. If adding a package that needs SDK types, it belongs in `awsdynamodb/`, not the root.
- **Two modules**: run `go` commands in the right directory. The adapter module uses `replace github.com/quells-bot/ddb-sqlite => ..`.
- **dynamodb-local via podman**: the conformance suite's local target expects a rootless podman socket (`systemctl --user start podman.socket`); `DOCKER_HOST` is auto-set to the podman socket if unset. Image `amazon/dynamodb-local:3.3.1`. Falls back to `DDBSQLITE_CONF_LOCAL_ENDPOINT`.
- **No package manager / no Bun / no Node.** Pure Go project.

## Testing & QA

**Framework**: standard `testing` package only — no testify, no external assertion libs. Assertions use `t.Errorf`/`t.Fatalf` directly.

**Conventions** (follow exactly in new tests):
- **Table-driven tests** with inline `cases := []struct{ name string; ... }{...}` and `t.Run(tc.name, ...)`. See `ddb/tables_test.go` `TestCreateTableValidation`.
- **Test helpers** marked `t.Helper()`, prefixed `new...`/`must...`/`as...`. Examples: `ddb.newClient(t)` (opens `:memory:`, registers `t.Cleanup`), storage `newTestStore(t)`, conformance `mustCreate`/`strVal`/`asResourceNotFound`/`asValidation`.
- **In-memory SQLite** is the default: `Open(ctx, ":memory:")`. For persistence-across-reopen cases use `t.TempDir()` + a file DSN (`internal/storage/store_test.go` `TestOpenReopenFileIdempotent`).
- **Storage tests** wrap each test in one `BeginTx` with `defer tx.Rollback()` (`internal/storage/items_test.go`).
- **Fuzz tests** use `go test -fuzz` with seed corpora added via `f.Add(...)`. Cover: `num` parse round-trip + compare antisymmetry (`internal/num/fuzz_test.go`), `attrval` wire round-trip + path parse + set-dedup idempotence (`attrval/fuzz_test.go`).
- **Conformance suite** (`awsdynamodb/conformance_test.go`): scenarios written against a minimal `api` interface (exact SDK signatures) that both `*awsdynamodb.Adapter` and `*dynamodb.Client` (vs dynamodb-local) satisfy. `runConformance(t, fn)` iterates active targets. Default runs the adapter target only; `DDBSQLITE_CONF_TARGET` env var selects `dynamodb-local` or `all`. The local container is started once in `TestMain`, shared across the binary; per-test cleanup purges tables. New conformance cases go here and run against both targets.

**Coverage expectations**: the conformance suite is the primary regression net and living documentation of supported behavior — add cases for edge cases (null vs missing attributes, type-mismatched comparisons, sparse GSIs, `Limit=0`, `LastEvaluatedKey` resume). Engine unit tests cover parser/evaluator/storage internals; fuzz tests guard against panics and round-trip instability.

## Implementation Status (Milestones)

The project follows a walking-skeleton-then-widen plan (see the spec §11). Currently implemented: **M0** (`num`, `attrval`), **M1** (`storage`, `ddb` table ops + basic item ops, `awsdynamodb` adapter, conformance harness), **M2** (`internal/expr` in full — condition/filter and update expressions; `ConditionExpression` + `ReturnValues` on `PutItem`/`DeleteItem`; `UpdateItem` with upsert, key immutability, and all five `ReturnValues` modes; `attrval.SetPath`/`RemovePath`; `num.Add`/`Sub`), **M3** (`Query`/`Scan` with faithful pagination — SQLite partition-seek + sort-key range narrowing, `ScanIndexForward`, `Limit` as a read budget applied before `FilterExpression` with `ScannedCount`/`Count`, `ExclusiveStartKey`/`LastEvaluatedKey`, parallel-scan `Segment`/`TotalSegments`), and **M4** (GSI support — write-triggered sparse index maintenance, GSI `Query`/`Scan`, read-time projection for `KEYS_ONLY`/`INCLUDE`/`ALL`, `Select`), and **M5** (complete) — **M5a** TTL (`UpdateTimeToLive`/`DescribeTimeToLive`; `ExpireExpired` engine extension; injectable `Options.Now` clock; removal of the `ttl INTEGER` data-table column; `storage.UpdateTableTTL`/`storage.ExpireExpired`) and **M5b** batch ops (`BatchWriteItem`/`BatchGetItem` — one atomic tx per batch, pre-validate-then-apply, 25/100 count limits, duplicate-key rejection via canonical wire JSON, no 16MB enforcement, no TTL read filtering, projection on BatchGetItem rejected as a deliberate divergence; BatchGetItem deterministically sorts each table's responses by primary key ascending and includes an empty per-table `Responses` entry for all-miss tables — both documented divergences). **Not yet present**: hardening (M6). When extending, check the spec's milestone list and prefer the conformance harness as the validation target.

**Known divergences from real DynamoDB** are captured as RED conformance cases — while any are open, keep them in a dedicated `awsdynamodb/conformance_divergence_test.go` where they pass against `dynamodb-local` and fail against the adapter until fixed. Once a case goes green on both targets, move it into `conformance_test.go`. All M2–M4 divergences are resolved and migrated (`conformance_divergence_test.go` has been retired), so `go test ./...` in `awsdynamodb/` is green on the default target.
