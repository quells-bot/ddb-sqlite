// Package ddb is the importable engine surface: it validates inputs, marshals
// items to wire JSON via attrval, computes key values, and delegates all
// persistence to internal/storage. It never writes SQL.
package ddb

import (
	"context"

	"github.com/quells-bot/ddb-sqlite/internal/storage"
)

// Options configures a Client. DSN is a file path, ":memory:", or a
// "file:...?..." URI.
type Options struct {
	DSN string
}

// Client wraps a *storage.Store. One Client = one SQLite DB = one region.
type Client struct {
	store *storage.Store
}

// Open delegates to storage.Open (driver, pragmas, catalog bootstrap).
func Open(ctx context.Context, opts Options) (*Client, error) {
	s, err := storage.Open(ctx, opts.DSN)
	if err != nil {
		return nil, err
	}
	return &Client{store: s}, nil
}

// Close closes the underlying store.
func (c *Client) Close() error { return c.store.Close() }
