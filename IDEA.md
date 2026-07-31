# Overview

ddb-sqlite is a mock implementation of a subset of the DynamoDB API implemented in Go and backed by a sqlite database.

The primary use case is for unit tests in web applications that use DynamoDB as their primary database.
While there are existing projects for running some version of the DynamoDB API for unit tests and local development
(dynalite, the amazon/dynamodb-local Docker container, scylladb alternator, etc) these all require an external process
and a local network. Having an in-process DynamoDB-compatible API would greatly simplify and speed up unit tests.

# Goals

ddb-sqlite should provide a drop-in replacement for the AWS SDK v2 DynamoDB client (although it must not take the SDK as a dependency).
It will support multiple mocked DynamoDB tables in a single "aws region".

As a consequence of providing the same API, ddb-sqlite must be able to parse and execute condition and filter expressions. Projection expressions may be supported but are not a primary goal.

ddb-sqlite should use the CGO-free modernc.org sqlite package to avoid cross-compilation headaches.

ddb-sqlite should provide Global Secondary Index features.

# Non-Goals

Providing an HTTP API equivalent to the real DynamoDB service is out of scope for this project (although it should be straightforward to build a wrapper to provide one).

High concurrency is out of scope for this project. A single sqlite database is effectively single writer.

Eventual consistency is out of scope for this project. Although a real DynamoDB table might return stale data immediately after a write, we will use sqlite transactions and a single database connection to ensure serialized access. This should approximate the consistent read/write feature available in the real DynamoDB.

Automatic streaming events are out of scope for this project.

# Implementation Ideas

A sketch of the database schema:

```sql
CREATE TABLE ddb_table_defs (
  id INTEGER NOT NULL PRIMARY KEY,
  name TEXT NOT NULL, -- name of the table, required
  hash TEXT NOT NULL, -- name of the partition key attribute, required
  range TEXT, -- name of the range/sort key attribute for composite primary keys, optional
  hash_type TEXT NOT NULL, -- type of the partition key attribute, required (S|N|B)
  range_type TEXT, -- type of the range key attribute, required if range is set (S|N|B)
  ttl TEXT, -- name of the ttl/expiration attribute, optional
  meta TEXT NOT NULL -- JSON payload carrying additional metadata about the table
) STRICT;

CREATE TABLE ddb_gsi_defs (
  table_id INTEGER NOT NULL REFERENCES ddb_table_defs (id),
  name TEXT NOT NULL, -- name of the GSI, required
  hash TEXT NOT NULL, -- name of the partition key attribute, required
  range TEXT, -- name of the range/sort key attribute for composite primary keys, optional
  hash_type TEXT NOT NULL, -- type of the partition key attribute, required (S|N|B)
  range_type TEXT, -- type of the range key attribute, required if range is set (S|N|B)
  projection_type TEXT NOT NULL, -- KEYS|INCLUDE|ALL
  projected TEXT, -- JSON payload representing the projected attributes (if project_type is not ALL)
  PRIMARY KEY (table_id, name)
) STRICT;

CREATE TABLE ddb_<table-name-hash> (
  id INTEGER NOT NULL PRIMARY KEY,
  hash (TEXT|NUMERIC|BLOB) NOT NULL, -- partition attribute value, required
  range? (TEXT|NUMERIC|BLOB NOT NULL), -- range/sort attribute value, optional depending on table schema
  data BLOB NOT NULL, -- JSON payload representing the item attributes
  ttl? INTEGER NOT NULL, -- epoch seconds for expiration time, if a TTL attribute is set for the table
  UNIQUE (hash, range)
) STRICT;

CREATE TABLE ddb_<table-name-hash>_<gsi-name-hash> (
  data_id REFERENCES ddb_<table-name-hash> (id),
  hash (TEXT|NUMERIC|BLOB) NOT NULL, -- partition attribute value, required
  range? (TEXT|NUMERIC|BLOB NOT NULL) -- range/sort attribute value, optional depending on gsi schema
) STRICT;
```

## API Surface

### Tables

- Create
- Describe
- Delete
- List
- Update
- UpdateTimeToLive

### Items

- Put
- Delete
- BatchWrite
- Query
- Scan
- Update
- Get
- BatchGet

### Condition Expressions

- attribute_exists
- attribute_not_exists
- attribute_type
- contains
- begins_with
- size
- = <> < > <= >= BETWEEN IN
- AND OR NOT

## Example Flows

### Create Table

```sql
INSERT INTO ddb_table_defs (name, hash, hash_type, range, range_type, meta) VALUES
('Music', 'Artist', 'S', 'SongTitle', 'S', '{"class":"STANDARD","readCapacity":5,"writeCapacity":5}');

CREATE TABLE ddb_6eb00b4b2614a144 (
  id INTEGER NOT NULL PRIMARY KEY,
  hash TEXT NOT NULL,
  range TEXT NOT NULL,
  data BLOB NOT NULL,
  UNIQUE (hash, range)
) STRICT;
```

### Put Item

```sql
INSERT INTO ddb_6eb00b4b2614a144 (hash, range, data) VALUES
('Acme Band', 'Happy Day', '{"Artist":{"S":"Acme Band"},"SongTitle":{"S":"Happy Day},"Album":{"S":"Songs About Life"},"Awards":{"N":10}}');
```
