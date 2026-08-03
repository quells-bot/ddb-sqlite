package ddb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/quells-bot/ddb-sqlite-core/internal/storage"
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

// Projection describes which attributes a GSI projects.
type Projection struct {
	Type             string   // "ALL" | "KEYS_ONLY" | "INCLUDE"
	NonKeyAttributes []string // INCLUDE only; 1–100 attrs, no key attrs
}

// GlobalSecondaryIndex names one GSI declared at CreateTable.
type GlobalSecondaryIndex struct {
	IndexName  string
	KeySchema  []KeySchemaElement
	Projection Projection
}

// GlobalSecondaryIndexDescription is DescribeTable's view of a GSI.
type GlobalSecondaryIndexDescription struct {
	IndexName   string
	KeySchema   []KeySchemaElement
	Projection  Projection
	IndexStatus string // always "ACTIVE" in this engine (synchronous add)
}

// TableDescription is the engine's view of a table (GSIs/billing arrive later).
type TableDescription struct {
	Name, Hash, Range, HashType, RangeType, TTL string
	CreationTime                                time.Time
	GlobalSecondaryIndexes                      []GlobalSecondaryIndexDescription
	AttributeDefinitions                        []AttributeDefinition
}

// CreateTableInput carries the table name, key schema, and attribute defs.
type CreateTableInput struct {
	TableName              string
	KeySchema              []KeySchemaElement
	AttributeDefinitions   []AttributeDefinition
	GlobalSecondaryIndexes []GlobalSecondaryIndex
}

// DescribeTableInput names the table to describe.
type DescribeTableInput struct {
	TableName string
}

// GlobalSecondaryIndexUpdate is one action in an UpdateTable call. Exactly one
// of Create or Delete must be non-nil (enforced by validation rule 4).
type GlobalSecondaryIndexUpdate struct {
	Create *GlobalSecondaryIndex // non-nil for a Create action
	Delete *string               // non-nil GSI name for a Delete action
}

// UpdateTableInput carries a table name and at most one GSI create/delete action.
// Non-GSI fields (billing mode, throughput, SSE, streams, replicas) have no
// representation in core; the adapter sets NonGsiFieldsPresent so the engine can
// distinguish a truly empty update (rejected, rule 2) from a throughput-only
// no-op, and reject a GSI action combined with other operations (rule 5).
type UpdateTableInput struct {
	TableName                   string
	AttributeDefinitions        []AttributeDefinition
	GlobalSecondaryIndexUpdates []GlobalSecondaryIndexUpdate
	NonGsiFieldsPresent         bool
}

// UpdateTableOutput carries the table description after the update.
type UpdateTableOutput struct {
	TableDescription TableDescription
}

func validKeyType(t string) bool { return t == "S" || t == "N" || t == "B" }

func (c *Client) CreateTable(ctx context.Context, in CreateTableInput) (TableDescription, error) {
	// Validate first (without mutating in).
	hashName, hashType, rangeName, rangeType, gsis, err := analyzeCreateTable(in)
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

	now := c.now().UTC()
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
		GSIs:      gsis,
	}
	id, err := c.store.InsertTableDef(tx, def)
	if err != nil {
		return TableDescription{}, err
	}
	if err := c.store.CreateDataTable(tx, def); err != nil {
		return TableDescription{}, err
	}
	for _, g := range gsis {
		if err := c.store.InsertGsiDef(tx, id, g); err != nil {
			return TableDescription{}, err
		}
		if err := c.store.CreateGsiTable(tx, def, g); err != nil {
			return TableDescription{}, err
		}
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

func analyzeCreateTable(in CreateTableInput) (hashName, hashType, rangeName, rangeType string, gsis []storage.GsiDef, err error) {
	if in.TableName == "" {
		return "", "", "", "", nil, fmt.Errorf("%w: table name is empty", ErrValidation)
	}
	if len(in.KeySchema) > 2 {
		return "", "", "", "", nil, fmt.Errorf("%w: key schema must have at most two elements", ErrValidation)
	}
	hashCount := 0
	rangeCount := 0
	seen := map[string]bool{}
	for _, k := range in.KeySchema {
		if k.KeyType != "HASH" && k.KeyType != "RANGE" {
			return "", "", "", "", nil, fmt.Errorf("%w: key type %q must be HASH or RANGE", ErrValidation, k.KeyType)
		}
		if seen[k.AttributeName+k.KeyType] {
			return "", "", "", "", nil, fmt.Errorf("%w: duplicate key element %q %q", ErrValidation, k.AttributeName, k.KeyType)
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
		return "", "", "", "", nil, fmt.Errorf("%w: key schema must have exactly one HASH element", ErrValidation)
	}
	if rangeCount > 1 {
		return "", "", "", "", nil, fmt.Errorf("%w: key schema must have at most one RANGE element", ErrValidation)
	}
	if hashName != "" && hashName == rangeName {
		return "", "", "", "", nil, fmt.Errorf("%w: attribute %q cannot be both HASH and RANGE", ErrValidation, hashName)
	}

	// AttributeDefinitions: reject duplicates (probe G27), build types map.
	types := map[string]string{}
	for _, ad := range in.AttributeDefinitions {
		if !validKeyType(ad.AttributeType) {
			return "", "", "", "", nil, fmt.Errorf("%w: attribute %q has invalid type %q", ErrValidation, ad.AttributeName, ad.AttributeType)
		}
		if _, exists := types[ad.AttributeName]; exists {
			return "", "", "", "", nil, fmt.Errorf("%w: Cannot have two attributes with the same name: %q", ErrValidation, ad.AttributeName)
		}
		types[ad.AttributeName] = ad.AttributeType
	}
	hashType, ok := types[hashName]
	if !ok {
		return "", "", "", "", nil, fmt.Errorf("%w: no AttributeDefinition for hash key %q", ErrValidation, hashName)
	}
	if rangeName != "" {
		rangeType, ok = types[rangeName]
		if !ok {
			return "", "", "", "", nil, fmt.Errorf("%w: no AttributeDefinition for range key %q", ErrValidation, rangeName)
		}
	}

	// Collect all key attr names (table + GSIs) to validate surplus defs.
	keyAttrs := map[string]bool{hashName: true}
	if rangeName != "" {
		keyAttrs[rangeName] = true
	}

	// Validate GSIs.
	gsiNames := map[string]bool{}
	if len(in.GlobalSecondaryIndexes) > 20 {
		return "", "", "", "", nil, fmt.Errorf("%w: at most 20 global secondary indexes per table", ErrValidation)
	}
	for _, g := range in.GlobalSecondaryIndexes {
		if !validGsiName(g.IndexName) {
			return "", "", "", "", nil, fmt.Errorf("%w: invalid index name %q: must be 3-255 chars of [a-zA-Z0-9_.-]", ErrValidation, g.IndexName)
		}
		if gsiNames[g.IndexName] {
			return "", "", "", "", nil, fmt.Errorf("%w: duplicate index name %q", ErrValidation, g.IndexName)
		}
		gsiNames[g.IndexName] = true

		gh, gr, ght, grt, gerr := validateGsiKeySchema(g.KeySchema, types)
		if gerr != nil {
			return "", "", "", "", nil, gerr
		}

		if err := validateProjection(g.Projection, gh, gr, hashName, rangeName); err != nil {
			return "", "", "", "", nil, err
		}

		gsis = append(gsis, storage.GsiDef{
			Name:           g.IndexName,
			Hash:           gh,
			Range:          gr,
			HashType:       ght,
			RangeType:      grt,
			ProjectionType: g.Projection.Type,
			Projected:      g.Projection.NonKeyAttributes,
		})
		keyAttrs[gh] = true
		if gr != "" {
			keyAttrs[gr] = true
		}
	}

	// Reject AttributeDefinitions not referenced by any key (table or GSI).
	if len(types) != len(keyAttrs) {
		return "", "", "", "", nil, fmt.Errorf("%w: AttributeDefinitions must match the key schema attributes (and GSI key attributes)", ErrValidation)
	}
	return hashName, hashType, rangeName, rangeType, gsis, nil
}

// validGsiName checks 3–255 chars from [a-zA-Z0-9_.-].
func validGsiName(name string) bool {
	if len(name) < 3 || len(name) > 255 {
		return false
	}
	for _, c := range name {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', (c == '_' || c == '.' || c == '-'):
		default:
			return false
		}
	}
	return true
}

// validateGsiKeySchema checks exactly 1 HASH + 0-1 RANGE, each with a definition,
// and HASH != RANGE. Returns the resolved names/types.
func validateGsiKeySchema(ks []KeySchemaElement, types map[string]string) (hash, range_, hashType, rangeType string, err error) {
	if len(ks) > 2 {
		return "", "", "", "", fmt.Errorf("%w: GSI key schema must have at most two elements", ErrValidation)
	}
	hc, rc := 0, 0
	for _, k := range ks {
		if k.KeyType != "HASH" && k.KeyType != "RANGE" {
			return "", "", "", "", fmt.Errorf("%w: GSI key type %q must be HASH or RANGE", ErrValidation, k.KeyType)
		}
		switch k.KeyType {
		case "HASH":
			hc++
			hash = k.AttributeName
		case "RANGE":
			rc++
			range_ = k.AttributeName
		}
	}
	if hc != 1 {
		return "", "", "", "", fmt.Errorf("%w: GSI key schema must have exactly one HASH element", ErrValidation)
	}
	if rc > 1 {
		return "", "", "", "", fmt.Errorf("%w: GSI key schema must have at most one RANGE element", ErrValidation)
	}
	ht, ok := types[hash]
	if !ok {
		return "", "", "", "", fmt.Errorf("%w: no AttributeDefinition for GSI hash key %q", ErrValidation, hash)
	}
	hashType = ht
	if range_ != "" {
		rt, ok := types[range_]
		if !ok {
			return "", "", "", "", fmt.Errorf("%w: no AttributeDefinition for GSI range key %q", ErrValidation, range_)
		}
		rangeType = rt
		if hash == range_ {
			return "", "", "", "", fmt.Errorf("%w: attribute %q cannot be both GSI HASH and RANGE", ErrValidation, hash)
		}
	}
	return hash, range_, hashType, rangeType, nil
}

// validateProjection checks the projection type and NonKeyAttributes against
// the table and GSI key attrs (probe G27).
func validateProjection(p Projection, gsiHash, gsiRange, tblHash, tblRange string) error {
	switch p.Type {
	case "ALL":
		if len(p.NonKeyAttributes) > 0 {
			return fmt.Errorf("%w: ALL projection must not specify NonKeyAttributes", ErrValidation)
		}
	case "KEYS_ONLY":
		if len(p.NonKeyAttributes) > 0 {
			return fmt.Errorf("%w: KEYS_ONLY projection must not specify NonKeyAttributes", ErrValidation)
		}
	case "INCLUDE":
		if len(p.NonKeyAttributes) == 0 {
			return fmt.Errorf("%w: INCLUDE projection requires NonKeyAttributes", ErrValidation)
		}
		if len(p.NonKeyAttributes) > 100 {
			return fmt.Errorf("%w: INCLUDE NonKeyAttributes exceeds 100", ErrValidation)
		}
		keySet := map[string]bool{gsiHash: true, tblHash: true}
		if gsiRange != "" {
			keySet[gsiRange] = true
		}
		if tblRange != "" {
			keySet[tblRange] = true
		}
		seen := map[string]bool{}
		for _, a := range p.NonKeyAttributes {
			if keySet[a] {
				return fmt.Errorf("%w: INCLUDE NonKeyAttributes must not include key attribute %q", ErrValidation, a)
			}
			if seen[a] {
				return fmt.Errorf("%w: duplicate NonKeyAttribute %q", ErrValidation, a)
			}
			seen[a] = true
		}
	default:
		return fmt.Errorf("%w: invalid projection type %q", ErrValidation, p.Type)
	}
	return nil
}

func describeFromDef(name string, def storage.TableDef, created time.Time) TableDescription {
	td := TableDescription{
		Name:         name,
		Hash:         def.Hash,
		Range:        def.Range,
		HashType:     def.HashType,
		RangeType:    def.RangeType,
		TTL:          def.TTL,
		CreationTime: created,
	}
	for _, g := range def.GSIs {
		ks := []KeySchemaElement{{AttributeName: g.Hash, KeyType: "HASH"}}
		if g.Range != "" {
			ks = append(ks, KeySchemaElement{AttributeName: g.Range, KeyType: "RANGE"})
		}
		td.GlobalSecondaryIndexes = append(td.GlobalSecondaryIndexes, GlobalSecondaryIndexDescription{
			IndexName:   g.Name,
			KeySchema:   ks,
			Projection:  Projection{Type: g.ProjectionType, NonKeyAttributes: g.Projected},
			IndexStatus: "ACTIVE",
		})
	}
	td.AttributeDefinitions = append(td.AttributeDefinitions, AttributeDefinition{AttributeName: def.Hash, AttributeType: def.HashType})
	if def.Range != "" {
		td.AttributeDefinitions = append(td.AttributeDefinitions, AttributeDefinition{AttributeName: def.Range, AttributeType: def.RangeType})
	}
	seen := map[string]bool{def.Hash: true}
	if def.Range != "" {
		seen[def.Range] = true
	}
	for _, g := range def.GSIs {
		if !seen[g.Hash] {
			td.AttributeDefinitions = append(td.AttributeDefinitions, AttributeDefinition{AttributeName: g.Hash, AttributeType: g.HashType})
			seen[g.Hash] = true
		}
		if g.Range != "" && !seen[g.Range] {
			td.AttributeDefinitions = append(td.AttributeDefinitions, AttributeDefinition{AttributeName: g.Range, AttributeType: g.RangeType})
			seen[g.Range] = true
		}
	}
	return td
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
	for _, g := range def.GSIs {
		if err := c.store.DropGsiTable(tx, in.TableName, g.Name); err != nil {
			return err
		}
	}
	if err := c.store.DeleteGsiDefs(tx, def.ID); err != nil {
		return err
	}
	if err := c.store.DeleteTableDef(tx, def.ID); err != nil {
		return err
	}
	return tx.Commit()
}
