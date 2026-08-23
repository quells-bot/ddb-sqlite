# Repository Guidelines

## Project Overview

`ddb-sqlite` is an in-process implementation of the AWS DynamoDB SDK V2 API backed by an in-memory SQLite database. It is built for unit tests: unlike dynalite, dynamodb-local, or LocalStack, it runs in-process — no separate process to manage. Behavior parity with real DynamoDB is verified by an extensive conformance suite that runs the same tests against a dockerized `dynamodb-local` container.

This repository (`github.com/quells-bot/ddb-sqlite`) is the **adapter shim** between [`ddb-sqlite-core`](https://github.com/quells-bot/ddb-sqlite-core) (the storage + business-logic engine) and AWS SDK V2 types. It does not contain SQL or expression logic itself. MIT licensed.

Modern DynamoDB semantics only; legacy/deprecated features are intentionally rejected (not silently emulated). No HTTP server, no Kinesis streams, no recovery.

## Architecture & Data Flow

```
aws-sdk-go-v2 types  ──►  Adapter (this repo)  ──►  *ddb.Client (ddb-sqlite-core)
 (DynamoDBAPI)            adapter.go / marshal.go      storage + business logic
                                                             │
                                                             ▼
                                                  modernc.org/sqlite (CGO-free, in-memory)
```

**Layers** (explicit separation enforced in core):

1. **Adapter** (`pkg/ddb-sqlite/adapter.go`) — translates SDK V2 request/response types to/from core types, validates SDK-level params, maps errors. Contains zero SQL or item introspection.
2. **Marshal** (`pkg/ddb-sqlite/marshal.go`) — converts `types.AttributeValue` ↔ core `attrval.Value`, including number-precision and empty-set validation.
3. **Core** (`ddb-sqlite-core`, separate module) — `*ddb.Client` owns all storage (SQLite data tables, GSI sparse index tables) and business logic (custom expression parser + tree-walking interpreter for Condition/Filter/Projection expressions).

**Per-call data flow:** SDK input params → `rejectLegacy*` validation → `FromSDKMap`/`exprValues` conversion → `*ddb.Client` method → `ToSDKMap`/`toSDKTableDescription` conversion → SDK output. Fully synchronous, `context.Context`-aware, no goroutines in this package.

## Key Directories

```
.
├── examples/catalog/      # End-to-end example app (REST API over the adapter)
│   ├── main.go            # Composition root: wires storage→bus→app, serves HTTP
│   ├── routes.go          # HTTP routing + method dispatch
│   ├── wires.go           # Dependency injection (in-memory mock or real AWS)
│   ├── storage/           # DynamoDB repository layer (single-table PK/SK design)
│   ├── bus/               # Business logic + validation, maps storage errors
│   └── app/               # HTTP handlers, request/response types, error→status mapping
├── pkg/ddb/               # Minimal DynamoDBAPI subset interface used by the example
├── pkg/ddb-sqlite/        # The entire package (package name: ddbsqlite)
│   ├── adapter.go         # SDK V2 DynamoDBAPI surface (16 ops + helpers)
│   ├── adapter_test.go    # Adapter-specific unit tests
│   ├── marshal.go         # AttributeValue ↔ core attrval.Value conversion
│   ├── marshal_test.go    # Marshalling roundtrip/validation tests
│   └── conformance_test.go# Dual-target parity tests vs dynamodb-local (~7.4k lines)
├── go.mod / go.sum        # Module github.com/quells-bot/ddb-sqlite, Go 1.25.5
├── README.md             # Architecture overview, supported features, disclosures
└── LICENSE               # MIT, Copyright (c) 2026 Kai Wells
```

No `cmd/`, `scripts/`, `internal/`, or CI config.

## Development Commands

```sh
# Run adapter-only tests (default — no Docker required)
go test ./...

# Run full dual-target conformance (requires Docker/Podman)
DDBSQLITE_CONF_TARGET=all go test -v -count=1 ./...

# Run conformance against dynamodb-local only
DDBSQLITE_CONF_TARGET=dynamodb-local go test -v -count=1 ./...

# Use an external dynamodb-local endpoint instead of spinning up a container
DDBSQLITE_CONF_LOCAL_ENDPOINT=http://localhost:8000 go test -v -count=1 ./...

# Run a specific test
go test ./pkg/ddb-sqlite/ -run TestAdapterCreateDescribePutGet -v

# Tidy / verify deps
go mod tidy && go vet ./...
```

There is no Makefile, Taskfile, linter config, or CI workflow. Use standard `go` tooling. No build tags on any file.

## Code Conventions & Common Patterns

- **Package:** `ddbsqlite`; tests are black-box package `ddbsqlite_test`.
- **Receiver:** consistently `(a *Adapter)`.
- **AWS types only:** all exported input/output types come from `github.com/aws/aws-sdk-go-v2/service/dynamodb` and its `types` subpackage. Never reimplement SDK types.
- **Type conversion:** `FromSDK`/`ToSDK` (single value), `FromSDKMap`/`ToSDKMap` (item maps). Every SDK↔core boundary goes through these — never hand-rolled conversion.
- **Error mapping:** central `mapError()` (`adapter.go:57`) uses `errors.As` for `ConditionalCheckFailedError` (returns `types.ConditionalCheckFailedException` with item payload) and `errors.Is` for core sentinels (`ErrResourceNotFound`, `ErrTableNotFound`, `ErrTableInUse`, `ErrGsiNotFound`, `ErrLimitExceeded`, `ErrValidation`). Validation errors wrap with `fmt.Errorf("...: %w", ddb.ErrValidation)`.
- **Legacy rejection:** deprecated SDK V1-style params (`Expected`, `AttributeUpdates`, `KeyConditions`, `QueryFilter`, `ScanFilter`, `ConditionalOperator`, `AttributesToGet`) are explicitly rejected with `ValidationException` rather than emulated. Add new rejection checks in the `rejectLegacy*` helpers.
- **SDK value accessors:** always use `aws.ToString`, `aws.ToBool`, `aws.ToInt32`, `aws.Int64`, `aws.String` — never direct pointer derefs on SDK string/bool fields.
- **No CGO:** SQLite comes from `modernc.org/sqlite` (pure Go) via `ddb-sqlite-core`. Do not introduce CGO or `mattn/go-sqlite3`.
- **No async:** fully synchronous. Goroutine-safety is the responsibility of `*ddb.Client` (core), not this adapter.
- **No clock injection in this package:** TTL clock injection lives in `ddb-sqlite-core`'s constructor `Options`, not here.
- **Numbers as keys:** stored as SQLite `REAL` (float64) — precision loss possible above 2^53. This is a documented limitation, not a bug.

## Important Files

| File | Role |
|------|------|
| `pkg/ddb-sqlite/adapter.go` | Entry point: `Adapter` struct (L26), `New(client)` (L34), `Open(ctx, dsn)` (L40); 16 SDK methods (CreateTable L122 → Scan L712); `mapError` (L57) |
| `pkg/ddb-sqlite/marshal.go` | `FromSDK` (L17), `ToSDK` (L61), `FromSDKMap` (L99), `ToSDKMap` (L111); empty-set and number-precision validation |
| `pkg/ddb-sqlite/conformance_test.go` | `TestMain` (L123) manages dockertest lifecycle; `api` interface (L18) mirrors SDK signatures; `runConformance` (L75) fans out to adapter + dynamodb-local targets |
| `go.mod` | Module path, Go 1.25.5, direct deps: `aws-sdk-go-v2`, `ddb-sqlite-core`, `dockertest/v4` |
| `README.md` | Authoritative architecture + supported-features + TTL semantics reference |
| `pkg/ddb/ddb.go` | Minimal `API` interface — DynamoDBAPI subset satisfied by both `*ddbsqlite.Adapter` and `*dynamodb.Client`; used by the example |
| `examples/catalog/` | End-to-end REST example: `storage/` (repository, single-table PK/SK), `bus/` (validation + error mapping), `app/` (HTTP handlers); see `examples/catalog/README.md` |

## Runtime/Tooling Preferences

- **Runtime:** Go 1.25.5 (per `go.mod` directive). No Node/Bun involvement.
- **Package manager:** `go mod` (committed `go.sum`). No vendoring.
- **CGO:** Disabled by design — `modernc.org/sqlite` is pure Go. `CGO_ENABLED=0` is the intended build mode.
- **Docker:** only needed for `DDBSQLITE_CONF_TARGET=all` conformance runs; `dockertest/v4` auto-detects Podman socket as fallback (`ensureDockerHost`, conformance_test.go L178).
- **Container image:** `amazon/dynamodb-local:3.3.1`, port 8000, 60s readiness retry on `ListTables`.

## Testing & QA

- **Framework:** Go stdlib `testing` only — no `testify`, no `gomock`. Table-driven where appropriate (`cases := []struct{...}`); otherwise line-driven subtests via `t.Run`.
- **Naming:** `TestXxx` (unit) / `TestConfXxx` (conformance); subtest names are descriptive strings.
- **Black-box:** all test files are package `ddbsqlite_test` — they exercise only exported API, never internals.
- **Three test files:**
  - `adapter_test.go` — adapter-only behavior (error mapping, legacy rejection, UpdateItem/UpdateTable rejections, TTL, batch). ~35 test functions.
  - `marshal_test.go` — 11-type AttributeValue roundtrip + number canonicalization (`'1.50' → '1.5'`).
  - `conformance_test.go` — ~120 dual-target parity tests covering tables, items, query/scan, GSIs, TTL, batch, projections, and expression limits (path depth, token count, substitution value count, key/value length).
- **Conformance harness:** same test body runs against both the in-memory adapter and real `dynamodb-local`; divergences surface as test failures. Default runs adapter-only (no Docker); set `DDBSQLITE_CONF_TARGET=all` for full parity.
- **TTL semantics:** `TestConfTTLExpiredItemVisible` confirms expired items remain visible on reads (no automatic read-side filtering), matching real DynamoDB. Automatic deletion is **not** implemented — `ExpireExpired` (a core extension) must be called manually.
- **Coverage:** no coverage thresholds or directives defined.

## Commit Conventions

Conventional-commits style with types: `milestone:`, `fix:`, `docs:`, `chore:`. Present-tense imperative mood, no scope prefixes. Example: `fix: reject empty ExpressionAttributeValues map`.

## Notes for AI Assistants

- This project was primarily written by LLMs (DeepSeek V4, GLM 5.2, Kimi K3, Claude Opus 5) via the Superpowers brainstorm/plan/execute loop. Code style reflects that: consistent, explicit, minimal.
- When adding an operation: add the method on `*Adapter` in `adapter.go`, add SDK↔core conversion via existing `FromSDKMap`/`ToSDKMap` helpers, route errors through `mapError`, add `rejectLegacy*` guards if the SDK input has deprecated fields, and add a conformance test (`TestConfXxx`) that exercises both targets.
- When modifying marshalling: update `marshal.go` and `marshal_test.go` (`TestRoundTripAllTags` enumerates all 11 attribute types).
- Prefer extending the conformance suite over adapter-only tests for behavior changes — that is where parity is enforced.
