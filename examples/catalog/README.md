# catalog

An end-to-end example REST API built on `ddb-sqlite`. It demonstrates how to
switch between the in-memory adapter in tests and the real AWS DynamoDB client
in production — without changing any application code.

## Layers

This application uses a 3-layer architecture:

```
HTTP request
    │
    ▼
app/        HTTP handlers, request/response JSON, error→status mapping
    │
    ▼
bus/        Business logic, input validation, storage error→bus error mapping
    │
    ▼
storage/    DynamoDB repository (single-table PK/SK design), item marshalling
```

### storage/

DynamoDB repository. All items live in a single `catalog` table with a
composite primary key:

| Entity  | PK              | SK        | EntityType | EntityID |
|---------|-----------------|-----------|------------|----------|
| Author  | `AUTHOR#{id}`   | `PROFILE` | `AUTHOR`   | author ID |
| Book    | `AUTHOR#{id}`   | `BOOK#{bookID}` | `BOOK` | book ID |

A Global Secondary Index (`EntityTypeIndex`) on `(EntityType, EntityID)`
enables efficient listing by entity type: `ListAuthors` queries
`EntityType = AUTHOR` and `ListAllBooks` queries `EntityType = BOOK`,
replacing the previous full-table Scan for authors.

Deletes are TTL-based soft deletes: `DeleteAuthor`/`DeleteBook` set an
`Expires` attribute (Unix epoch seconds, `now + 1h`) via `UpdateItem`
instead of hard-deleting the item. TTL is enabled on the `Expires`
attribute (on real DynamoDB, the background TTL process eventually
removes expired items; the in-memory adapter does not auto-expire —
soft-deleted items are filtered out by reads and remain in the table).
All reads filter soft-deleted items with `attribute_not_exists(Expires)`.

Manual TTL expiry is exposed at the storage layer via
`repo.ExpireExpired(ctx)`, which calls the `ddb.ExpireExpired` helper on the
`ddb.API` the repo holds. The helper asserts the `Expirer` capability on the
underlying client — satisfied by `*ddbsqlite.Adapter`, not by real AWS — so the
repo reaches the engine extension without importing the concrete adapter or
performing a cast. Returns the count of deleted items. Real DynamoDB removes
expired items asynchronously, so there is no counterpart to call in production.

`DeleteAuthor` cascades: it soft-deletes the author, then batch
soft-deletes all live books under that author via `BatchWriteItem`
(chunked by 25, the DynamoDB batch limit).
The cascade is not transactional: the author is soft-deleted before its
books, so if the book step fails the author is gone while its books remain
live and visible in `GET /books`. A `TransactWriteItems` version would
close this gap; the example keeps the simpler two-step flow for clarity.

Create/update operations use `ConditionExpression` to enforce
existence invariants: `Put` rejects duplicates
(`attribute_not_exists`), `Update`/`Delete` reject missing or
already-soft-deleted items (`attribute_exists(PK) AND
attribute_not_exists(Expires)`). `ConditionalCheckFailedException` is
mapped to `ErrConflict` (Put) or `ErrNotFound` (Update/Delete).

### bus/

Business logic. Generates random IDs (`crypto/rand`), validates input, and
maps `storage.ErrNotFound`/`storage.ErrConflict` to its own `ErrNotFound`/
`ErrConflict`/`ErrValidation` sentinels.

### app/

HTTP handlers. Decodes JSON requests, calls the bus service, and maps bus
errors to HTTP statuses: 404 not found, 409 conflict, 400 validation,
500 internal. Uses Go 1.22+ method routing via `http.ServeMux` patterns.

## Running

```sh
# In-memory adapter (default for local development)
DDB_MOCK=1 go run ./examples/catalog

# Real AWS DynamoDB (uses default AWS credential chain)
go run ./examples/catalog
```

The server listens on `:8080` by default; override with `HTTP_ADDR`.

## API

| Method   | Path                                     | Description          |
|----------|------------------------------------------|----------------------|
| `GET`    | `/authors`                               | List authors         |
| `POST`   | `/authors`                               | Create author        |
| `GET`    | `/authors/{authorID}`                    | Get author           |
| `PUT`    | `/authors/{authorID}`                    | Update author        |
| `DELETE` | `/authors/{authorID}`                    | Delete author        |
| `GET`    | `/authors/{authorID}/books`              | List books by author |
| `GET`    | `/books`                                 | List all books       |
| `POST`   | `/authors/{authorID}/books`              | Create book          |
| `GET`    | `/authors/{authorID}/books/{bookID}`     | Get book             |
| `PUT`    | `/authors/{authorID}/books/{bookID}`     | Update book          |
| `DELETE` | `/authors/{authorID}/books/{bookID}`     | Delete book          |

## Testing

Every test layer wires the real layers below it against the in-memory
adapter — no mocks except to force otherwise-unreachable error paths
(conflict, internal error).

```sh
go test ./examples/catalog/...
```
The example demonstrates GSI queries (entity-type listing), TTL soft
deletes (Expires attribute), and BatchWriteItem (cascading deletes).
