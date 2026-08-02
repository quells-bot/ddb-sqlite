// Package storage owns the SQLite database for the ddb engine: it opens and
// configures the *sql.DB, bootstraps the catalog, generates per-table DDL, and
// issues all SQL. It deals in TableDef, raw key values, and opaque []byte item
// blobs; it never imports attrval or num.
package storage

import (
	"crypto/sha256"
	"encoding/hex"
)

// TableName maps a DynamoDB table name to a safe SQLite identifier:
// "ddb_" + the first 16 hex characters of SHA-256(name). Hashing yields a
// stable identifier regardless of characters illegal as SQLite identifiers.
func TableName(name string) string {
	sum := sha256.Sum256([]byte(name))
	return "ddb_" + hex.EncodeToString(sum[:8]) // 8 bytes = 16 hex chars
}

// GsiTableName maps a (table, gsi) pair to a safe SQLite identifier:
// the data table's hashed name + "_" + 16 hex of SHA-256(gsi).
func GsiTableName(table, gsi string) string {
	return TableName(table) + "_" + gsiHexFor(gsi)
}

// gsiHexFor returns the 16-hex-char suffix for a GSI name.
func gsiHexFor(name string) string {
	sum := sha256.Sum256([]byte(name))
	return hex.EncodeToString(sum[:8])
}
