package ddb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/quells-bot/ddb-sqlite/internal/storage"
)

// KeySchemaElement names one key attribute as HASH or RANGE.
type KeySchemaElement struct {
	AttributeName string
	KeyType       string // "HASH" | "RANGE"
}

// AttributeDefinition declares one attribute's DynamoDB type (S/N/B).
type AttributeDefinition struct {
	AttributeName string
	AttributeType string // "S" | "N" | "B"
}

// TableDescription is the engine's view of a table (GSIs/billing arrive later).
type TableDescription struct {
	Name, Hash, Range, HashType, RangeType, TTL string
	CreationTime                                time.Time
}

// CreateTableInput carries the table name, key schema, and attribute defs.
type CreateTableInput struct {
	TableName            string
	KeySchema            []KeySchemaElement
	AttributeDefinitions []AttributeDefinition
}

// DescribeTableInput names the table to describe.
type DescribeTableInput struct {
	TableName string
}

func validKeyType(t string) bool { return t == "S" || t == "N" || t == "B" }

func (c *Client) CreateTable(ctx context.Context, in CreateTableInput) (TableDescription, error) {
	// Validate first (without mutating in).
	hashName, hashType, rangeName, rangeType, err := analyzeCreateTable(in)
	if err != nil {
		return TableDescription{}, err
	}
	tx, err := c.store.BeginTx(ctx)
	if err != nil {
		return TableDescription{}, err
	}
	defer tx.Rollback()

	exists, err := c.store.TableExists(tx, in.TableName)
	if err != nil {
		return TableDescription{}, err
	}
	if exists {
		return TableDescription{}, fmt.Errorf("%w: table %q already exists", ErrTableInUse, in.TableName)
	}

	now := time.Now().UTC()
	meta, err := json.Marshal(map[string]any{"class": "STANDARD", "creationTime": now.Format(time.RFC3339Nano)})
	if err != nil {
		return TableDescription{}, fmt.Errorf("%w: marshal table meta: %v", ErrValidation, err)
	}
	def := storage.TableDef{
		Name:      in.TableName,
		Hash:      hashName,
		HashType:  hashType,
		Range:     rangeName,
		RangeType: rangeType,
		Meta:      meta,
	}
	if _, err := c.store.InsertTableDef(tx, def); err != nil {
		return TableDescription{}, err
	}
	if err := c.store.CreateDataTable(tx, def); err != nil {
		return TableDescription{}, err
	}
	if err := tx.Commit(); err != nil {
		return TableDescription{}, err
	}
	return describeFromDef(in.TableName, def, now), nil
}

func (c *Client) DescribeTable(ctx context.Context, in DescribeTableInput) (TableDescription, error) {
	tx, err := c.store.BeginTx(ctx)
	if err != nil {
		return TableDescription{}, err
	}
	defer tx.Rollback()

	def, err := c.store.GetTableDef(tx, in.TableName)
	if errors.Is(err, storage.ErrNotFound) {
		return TableDescription{}, fmt.Errorf("%w: table %q not found", ErrTableNotFound, in.TableName)
	}
	if err != nil {
		return TableDescription{}, err
	}
	return describeFromDef(in.TableName, def, creationTimeFromMeta(def.Meta)), nil
}

func analyzeCreateTable(in CreateTableInput) (hashName, hashType, rangeName, rangeType string, err error) {
	if in.TableName == "" {
		return "", "", "", "", fmt.Errorf("%w: table name is empty", ErrValidation)
	}
	if len(in.KeySchema) > 2 {
		return "", "", "", "", fmt.Errorf("%w: key schema must have at most two elements", ErrValidation)
	}
	hashCount := 0
	rangeCount := 0
	seen := map[string]bool{}
	for _, k := range in.KeySchema {
		if k.KeyType != "HASH" && k.KeyType != "RANGE" {
			return "", "", "", "", fmt.Errorf("%w: key type %q must be HASH or RANGE", ErrValidation, k.KeyType)
		}
		if seen[k.AttributeName+k.KeyType] {
			return "", "", "", "", fmt.Errorf("%w: duplicate key element %q %q", ErrValidation, k.AttributeName, k.KeyType)
		}
		seen[k.AttributeName+k.KeyType] = true
		switch k.KeyType {
		case "HASH":
			hashCount++
			hashName = k.AttributeName
		case "RANGE":
			rangeCount++
			rangeName = k.AttributeName
		}
	}
	if hashCount != 1 {
		return "", "", "", "", fmt.Errorf("%w: key schema must have exactly one HASH element", ErrValidation)
	}
	if rangeCount > 1 {
		return "", "", "", "", fmt.Errorf("%w: key schema must have at most one RANGE element", ErrValidation)
	}
	if hashName != "" && hashName == rangeName {
		return "", "", "", "", fmt.Errorf("%w: attribute %q cannot be both HASH and RANGE", ErrValidation, hashName)
	}
	types := map[string]string{}
	for _, ad := range in.AttributeDefinitions {
		if !validKeyType(ad.AttributeType) {
			return "", "", "", "", fmt.Errorf("%w: attribute %q has invalid type %q", ErrValidation, ad.AttributeName, ad.AttributeType)
		}
		types[ad.AttributeName] = ad.AttributeType
	}
	hashType, ok := types[hashName]
	if !ok {
		return "", "", "", "", fmt.Errorf("%w: no AttributeDefinition for hash key %q", ErrValidation, hashName)
	}
	if rangeName != "" {
		rangeType, ok = types[rangeName]
		if !ok {
			return "", "", "", "", fmt.Errorf("%w: no AttributeDefinition for range key %q", ErrValidation, rangeName)
		}
	}
	// Reject AttributeDefinitions not referenced by the key schema.
	keyAttrs := map[string]bool{hashName: true}
	if rangeName != "" {
		keyAttrs[rangeName] = true
	}
	if len(types) != len(keyAttrs) {
		return "", "", "", "", fmt.Errorf("%w: AttributeDefinitions must match the key schema attributes", ErrValidation)
	}
	return hashName, hashType, rangeName, rangeType, nil
}

func describeFromDef(name string, def storage.TableDef, created time.Time) TableDescription {
	return TableDescription{
		Name:         name,
		Hash:         def.Hash,
		Range:        def.Range,
		HashType:     def.HashType,
		RangeType:    def.RangeType,
		TTL:          def.TTL,
		CreationTime: created,
	}
}

func creationTimeFromMeta(meta []byte) time.Time {
	var m struct {
		CreationTime string `json:"creationTime"`
	}
	if json.Unmarshal(meta, &m) == nil && m.CreationTime != "" {
		if t, err := time.Parse(time.RFC3339Nano, m.CreationTime); err == nil {
			return t
		}
	}
	return time.Time{}
}

// ListTablesInput paginates the table list. Limit <= 0 defaults to 100 (the
// AWS max); values above 100 are capped to 100.
type ListTablesInput struct {
	ExclusiveStartTableName string
	Limit                   int32
}

// ListTablesOutput returns table names and a LastEvaluatedTableName when more
// remain (faithful pagination).
type ListTablesOutput struct {
	TableNames             []string
	LastEvaluatedTableName string
}

// DeleteTableInput names the table to drop.
type DeleteTableInput struct {
	TableName string
}

func (c *Client) ListTables(ctx context.Context, in ListTablesInput) (ListTablesOutput, error) {
	limit := int(in.Limit)
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	// Fetch limit+1 to detect "more remain" without a second query.
	tx, err := c.store.BeginTx(ctx)
	if err != nil {
		return ListTablesOutput{}, err
	}
	defer tx.Rollback()

	defs, err := c.store.ListTableDefsPage(tx, in.ExclusiveStartTableName, limit+1)
	if err != nil {
		return ListTablesOutput{}, err
	}
	if err := tx.Commit(); err != nil {
		return ListTablesOutput{}, err
	}

	out := ListTablesOutput{}
	names := make([]string, 0, len(defs))
	for _, d := range defs {
		names = append(names, d.Name)
	}
	if len(names) > limit {
		// More remain: LastEvaluated is the last *returned* name.
		out.LastEvaluatedTableName = names[limit-1]
		names = names[:limit]
	}
	out.TableNames = names
	return out, nil
}

func (c *Client) DeleteTable(ctx context.Context, in DeleteTableInput) error {
	tx, err := c.store.BeginTx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	def, err := c.store.GetTableDef(tx, in.TableName)
	if errors.Is(err, storage.ErrNotFound) {
		return fmt.Errorf("%w: table %q not found", ErrTableNotFound, in.TableName)
	}
	if err != nil {
		return err
	}
	if err := c.store.DropDataTable(tx, in.TableName); err != nil {
		return err
	}
	if err := c.store.DeleteTableDef(tx, def.ID); err != nil {
		return err
	}
	return tx.Commit()
}
