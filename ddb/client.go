// Package ddb is the importable engine surface: it validates inputs, marshals
// items to wire JSON via attrval, computes key values, and delegates all
// persistence to internal/storage. It never writes SQL.
package ddb

import (
	"context"
	"time"

	"github.com/quells-bot/ddb-sqlite/internal/storage"
)

// Options configures a Client. DSN is a file path, ":memory:", or a
// "file:...?..." URI. Now, when non-nil, overrides time.Now for TTL expiration
// and table creation timestamps, enabling deterministic tests without sleeping.
type Options struct {
	DSN string
	Now func() time.Time
}

// Client wraps a *storage.Store. One Client = one SQLite DB = one region.
type Client struct {
	store *storage.Store
	clock func() time.Time
}

// Open delegates to storage.Open (driver, pragmas, catalog bootstrap). A nil
// opts.Now defaults to time.Now; the clock is immutable after construction.
func Open(ctx context.Context, opts Options) (*Client, error) {
	s, err := storage.Open(ctx, opts.DSN)
	if err != nil {
		return nil, err
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Client{store: s, clock: now}, nil
}

// Close closes the underlying store.
func (c *Client) Close() error { return c.store.Close() }

// now returns the client's clock reading. Shared by ExpireExpired (TTL expiry)
// and CreateTable (CreationTime).
func (c *Client) now() time.Time { return c.clock() }
