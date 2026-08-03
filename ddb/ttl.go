// Package ddb is the importable engine surface. This file adds the TTL
// lifecycle: UpdateTimeToLive/DescribeTimeToLive configuration and the
// ExpireExpired deletion lever. Read paths never filter on TTL (Faithful).
package ddb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/quells-bot/ddb-sqlite-core/attrval"
	"github.com/quells-bot/ddb-sqlite-core/internal/num"
	"github.com/quells-bot/ddb-sqlite-core/internal/storage"
)

// TimeToLiveSpecification is the engine's TTL configuration.
type TimeToLiveSpecification struct {
	Enabled       bool
	AttributeName string
}

// UpdateTimeToLiveInput carries the table name and TTL spec to apply.
type UpdateTimeToLiveInput struct {
	TableName               string
	TimeToLiveSpecification TimeToLiveSpecification
}

// UpdateTimeToLiveOutput echoes the applied spec.
type UpdateTimeToLiveOutput struct {
	TimeToLiveSpecification TimeToLiveSpecification
}

// DescribeTimeToLiveInput names the table to describe.
type DescribeTimeToLiveInput struct {
	TableName string
}

// DescribeTimeToLiveOutput reports the TTL status and attribute name.
type DescribeTimeToLiveOutput struct {
	TimeToLiveStatus string // "ENABLED" | "DISABLED"
	AttributeName    string
}

// maxTTLAttrName is DynamoDB's attribute-name length cap (1-255 chars; no
// charset restriction, unlike table/GSI names).
const maxTTLAttrName = 255

// UpdateTimeToLive enables or disables TTL by recording the TTL attribute name
// in the catalog. Validation order: table-exists before attribute-name
// validation (ResourceNotFoundException takes precedence). The AttributeName is
// validated unconditionally (1-255 chars) whether enabling or disabling; when
// disabling the name is validated but otherwise ignored.
func (c *Client) UpdateTimeToLive(ctx context.Context, in UpdateTimeToLiveInput) (UpdateTimeToLiveOutput, error) {
	tx, err := c.store.BeginTx(ctx)
	if err != nil {
		return UpdateTimeToLiveOutput{}, err
	}
	defer tx.Rollback()

	def, err := c.store.GetTableDef(tx, in.TableName)
	if errors.Is(err, storage.ErrNotFound) {
		return UpdateTimeToLiveOutput{}, fmt.Errorf("%w: table %q not found", ErrTableNotFound, in.TableName)
	}
	if err != nil {
		return UpdateTimeToLiveOutput{}, err
	}

	name := in.TimeToLiveSpecification.AttributeName
	if len(name) < 1 || len(name) > maxTTLAttrName {
		return UpdateTimeToLiveOutput{}, fmt.Errorf("%w: TTL AttributeName must be 1-%d chars", ErrValidation, maxTTLAttrName)
	}

	attr := ""
	if in.TimeToLiveSpecification.Enabled {
		attr = name
	}
	if err := c.store.UpdateTableTTL(tx, def.ID, attr); err != nil {
		return UpdateTimeToLiveOutput{}, err
	}
	if err := tx.Commit(); err != nil {
		return UpdateTimeToLiveOutput{}, err
	}
	return UpdateTimeToLiveOutput{TimeToLiveSpecification: in.TimeToLiveSpecification}, nil
}

// DescribeTimeToLive reports the configured TTL status: "ENABLED" when a TTL
// attribute name is set, "DISABLED" otherwise.
func (c *Client) DescribeTimeToLive(ctx context.Context, in DescribeTimeToLiveInput) (DescribeTimeToLiveOutput, error) {
	tx, err := c.store.BeginTx(ctx)
	if err != nil {
		return DescribeTimeToLiveOutput{}, err
	}
	defer tx.Rollback()

	def, err := c.store.GetTableDef(tx, in.TableName)
	if errors.Is(err, storage.ErrNotFound) {
		return DescribeTimeToLiveOutput{}, fmt.Errorf("%w: table %q not found", ErrTableNotFound, in.TableName)
	}
	if err != nil {
		return DescribeTimeToLiveOutput{}, err
	}
	// Read-only: no commit, matching DescribeTable; defer Rollback releases the tx.

	out := DescribeTimeToLiveOutput{TimeToLiveStatus: "DISABLED"}
	if def.TTL != "" {
		out.TimeToLiveStatus = "ENABLED"
		out.AttributeName = def.TTL
	}
	return out, nil
}

// ExpireExpired scans the table, deletes items whose TTL attribute (configured
// via UpdateTimeToLive) is a Number <= now, and returns the count deleted. It
// is an engine extension with no SDK equivalent: tests call it explicitly for
// deterministic cleanup (real DynamoDB deletes asynchronously). Expired items
// remain visible on every read until this runs (Faithful model). Returns 0 when
// TTL is disabled. GSI index rows are cascade-deleted by storage.
func (c *Client) ExpireExpired(ctx context.Context, tableName string) (int, error) {
	tx, err := c.store.BeginTx(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	def, err := c.store.GetTableDef(tx, tableName)
	if errors.Is(err, storage.ErrNotFound) {
		return 0, fmt.Errorf("%w: table %q not found", ErrTableNotFound, tableName)
	}
	if err != nil {
		return 0, err
	}
	if def.TTL == "" {
		return 0, nil // TTL disabled — nothing to expire
	}

	nowEpoch := epochDecimal(c.now().Unix())
	ttlAttr := def.TTL
	expired := func(data []byte) (bool, error) {
		item := Item{}
		if err := json.Unmarshal(data, &item); err != nil {
			return false, fmt.Errorf("ddb: unmarshal item for TTL: %w", err)
		}
		v, ok := item[ttlAttr]
		if !ok || v.Tag() != attrval.TagNumber {
			return false, nil // absent or non-Number TTL attr -> kept
		}
		return v.Num().Compare(nowEpoch) <= 0, nil // <= boundary (spec §2.5)
	}

	n, err := c.store.ExpireExpired(tx, tableName, expired)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int(n), nil
}

// epochDecimal converts a Unix epoch-seconds value to an exact decimal for
// scale-insensitive comparison against a TTL attribute. strconv.FormatInt on an
// int64 always yields a valid decimal literal, so num.Parse cannot fail here.
func epochDecimal(n int64) num.Decimal {
	d, err := num.Parse(strconv.FormatInt(n, 10))
	if err != nil {
		panic(fmt.Sprintf("ddb: unreachable: bad epoch %d: %v", n, err))
	}
	return d
}
