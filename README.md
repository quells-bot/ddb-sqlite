# ddb-sqlite

An implementation of the DynamoDB SDK V2 API designed to be used in unit tests.
While several other options exist (dynalite, dynamodb-local, LocalStack),
those must all be run as separate processes. `ddb-sqlite` is built to run
in-process with an in-memory sqlite database to simplify running unit tests.
The increased testing speed is a nice side effect.

This project aims to match the semantics and features of modern DynamoDB
(as of 2026). Legacy and deprecated features are intentionally not supported.
Writing changes to a Kinesis stream, recovery, and other non-database features
are intentionally not supported. Running an HTTP server with the DynamoDB REST
API is intentionally not supported.

Conformance with those semantics is ensured via extensive behavior parity
testing against the dynamodb-local container image.

### Supported Features

- Tables: CreateTable, DescribeTable, DeleteTable, ListTables, UpdateTable
- Items: GetItem, PutItem, DeleteItem, UpdateItem, Query, Scan
- Batch: BatchGetItem, BatchWriteItem
- TTL: UpdateTimeToLive, DescribeTimeToLive, ExpireExpired (extension, see below)

DynamoDB auto-delete via TTL is not immediate; the behavior is carried into
this implementation. However, this package **never automatically deletes items**.
Whereas the real DynamoDB will periodically remove expired items (usually
within 48 hours), users of this package must explicitly call `ExpireExpired`.
Expired items remain visible on reads until they are deleted — matching
DynamoDB's read semantics — but deletion here is manual, not automatic.
A clock can be injected in constructor `Options` to give unit tests more
precise control over when an item is considered "expired".

## Example

See `examples/catalog` for a complete example of an application which uses
DynamoDB for its database and ddb-sqlite for unit tests throughout.

## Architecture

This package is just the adapter shim between
[ddb-sqlite-core](https://github.com/quells-bot/ddb-sqlite-core) and the SDK V2
API types.

### Core

The foundation is sqlite; or, rather, the CGO-free port from modernc.org.
One sqlite table keeps track of metadata for the DynamoDB tables that have been
created. Another table keeps track of metadata for the Global Secondary Indices
(GSIs) which have been created for those tables. Each DynamoDB table also gets
its own data table in sqlite to hold its Items. Each GSI gets its own index
table, which holds rows only for items that carry the GSI's key attributes
(sparse indexing). These data sqlite tables are created and dropped dynamically
according to the lifecycle of their corresponding DynamoDB table/GSI.

Items are stored in the data tables in their JSON attribute value
representation. Correctness and reusing the existing un/marshalling functions
was prioritized over saving a few bytes for ephemeral tables that only live for
the duration of a unit test.

Condition, Filter, and Projection Expressions are implemented using a custom
parser and are executed using a tree-walking interpreter.

There is an explicit separation between the sqlite storage layer and the
business logic of managing and querying data. The storage layer does not
introspect item contents; the business layer does not write SQL queries.

When Number (N) attributes are used for key attributes, they are stored with a
sqlite REAL column. This permits native sorting in SQL but could cause
incorrect behavior when integers exceed the precision limits of float64 (2^53).

## Disclosures

### AI/LLM Use

These packages are primarily written by LLMs using the Superpowers
brainstorm/plan/execute loop.

This README was written by a human, but may be proofread by LLMs.

This project and the corresponding core package were written using
DeepSeek V4, GLM 5.2, Kimi K3, and Claude Opus 5.

### Legal

DynamoDB is a trademark of Amazon Technologies, Inc.

ddb-sqlite is not affiliated with or endorsed by Amazon or AWS.
