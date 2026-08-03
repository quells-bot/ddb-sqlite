package ddb

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/quells-bot/ddb-sqlite-core/internal/storage"
)

// validateUpdateTableShape enforces request-shape rules 2–6 (rule 1, table
// exists, is checked by the caller after loading the def). It does not touch
// the table state. Returns nil for a valid Create, a valid Delete, or a
// throughput-only no-op (NonGsiFieldsPresent true, no GSI entries, no
// AttributeDefinitions).
func validateUpdateTableShape(in UpdateTableInput) error {
	n := len(in.GlobalSecondaryIndexUpdates)
	// Rule 3: more than one entry.
	if n > 1 {
		return fmt.Errorf("%w: at most one GlobalSecondaryIndexUpdates entry per UpdateTable", ErrValidation)
	}
	// Rule 2: truly empty (no GSI entries, no ignored fields).
	if n == 0 && !in.NonGsiFieldsPresent {
		return fmt.Errorf("%w: UpdateTable must specify at least one change", ErrValidation)
	}
	createAction := false
	if n == 1 {
		u := in.GlobalSecondaryIndexUpdates[0]
		// Rule 4: exactly one of Create/Delete (both nil or both non-nil rejected).
		if (u.Create == nil) == (u.Delete == nil) {
			return fmt.Errorf("%w: exactly one of Create or Delete must be set", ErrValidation)
		}
		createAction = u.Create != nil
		// Rule 5: a GSI action combined with ignored fields.
		if in.NonGsiFieldsPresent {
			return fmt.Errorf("%w: cannot combine a GlobalSecondaryIndexUpdate with other UpdateTable operations", ErrValidation)
		}
	}
	// Rule 6: AttributeDefinitions without a Create action.
	if len(in.AttributeDefinitions) > 0 && !createAction {
		return fmt.Errorf("%w: AttributeDefinitions require a Create action", ErrValidation)
	}
	return nil
}

// validateCreateGsi enforces rule 7 for a Create action against the loaded table
// def: GSI name format, the merged attribute-types map (table keys + existing
// GSI keys + input AttributeDefinitions), key schema, projection, surplus-attr
// rejection, name-in-use (ErrGsiInUse), and the 20-GSI cap (ErrLimitExceeded).
// Returns the resolved GsiDef ready to persist. Assumes validateUpdateTableShape
// already passed (so there is exactly one entry and it is a Create).
func validateCreateGsi(def storage.TableDef, in UpdateTableInput) (storage.GsiDef, error) {
	g := in.GlobalSecondaryIndexUpdates[0].Create

	// Valid GSI name.
	if !validGsiName(g.IndexName) {
		return storage.GsiDef{}, fmt.Errorf("%w: invalid index name %q: must be 3-255 chars of [a-zA-Z0-9_.-]", ErrValidation, g.IndexName)
	}

	// Merged types map: already-declared attrs (table keys + existing GSI keys).
	types := map[string]string{def.Hash: def.HashType}
	if def.Range != "" {
		types[def.Range] = def.RangeType
	}
	for _, ex := range def.GSIs {
		if _, ok := types[ex.Hash]; !ok {
			types[ex.Hash] = ex.HashType
		}
		if ex.Range != "" {
			if _, ok := types[ex.Range]; !ok {
				types[ex.Range] = ex.RangeType
			}
		}
	}
	declaredBeforeInput := map[string]bool{def.Hash: true}
	if def.Range != "" {
		declaredBeforeInput[def.Range] = true
	}
	for _, ex := range def.GSIs {
		declaredBeforeInput[ex.Hash] = true
		if ex.Range != "" {
			declaredBeforeInput[ex.Range] = true
		}
	}

	// Input AttributeDefinitions: validate type, reject duplicate names, reject
	// conflicting-type redeclarations, accept same-type redeclarations.
	for _, ad := range in.AttributeDefinitions {
		if !validKeyType(ad.AttributeType) {
			return storage.GsiDef{}, fmt.Errorf("%w: attribute %q has invalid type %q", ErrValidation, ad.AttributeName, ad.AttributeType)
		}
		if existing, exists := types[ad.AttributeName]; exists {
			if existing != ad.AttributeType {
				return storage.GsiDef{}, fmt.Errorf("%w: attribute %q already declared with a different type", ErrValidation, ad.AttributeName)
			}
			continue // same type: accepted and ignored
		}
		types[ad.AttributeName] = ad.AttributeType
	}

	// Key schema + projection reuse the existing validators.
	gh, gr, ght, grt, err := validateGsiKeySchema(g.KeySchema, types)
	if err != nil {
		return storage.GsiDef{}, err
	}
	if err := validateProjection(g.Projection, gh, gr, def.Hash, def.Range); err != nil {
		return storage.GsiDef{}, err
	}

	// Surplus attrs: every input AttributeDefinition not already declared must be
	// referenced by the new GSI's key schema (mirrors analyzeCreateTable's check,
	// scoped to the new GSI).
	newKeyAttrs := map[string]bool{gh: true}
	if gr != "" {
		newKeyAttrs[gr] = true
	}
	for _, ad := range in.AttributeDefinitions {
		if declaredBeforeInput[ad.AttributeName] {
			continue
		}
		if !newKeyAttrs[ad.AttributeName] {
			return storage.GsiDef{}, fmt.Errorf("%w: AttributeDefinition %q is not used by the new index key schema", ErrValidation, ad.AttributeName)
		}
	}

	// Name already in use.
	for _, ex := range def.GSIs {
		if ex.Name == g.IndexName {
			return storage.GsiDef{}, fmt.Errorf("%w: index %q already exists", ErrGsiInUse, g.IndexName)
		}
	}

	// 20-GSI cap (21st via UpdateTable → ErrLimitExceeded).
	if len(def.GSIs) >= 20 {
		return storage.GsiDef{}, fmt.Errorf("%w: at most 20 global secondary indexes per table", ErrLimitExceeded)
	}

	return storage.GsiDef{
		Name:           g.IndexName,
		Hash:           gh,
		Range:          gr,
		HashType:       ght,
		RangeType:      grt,
		ProjectionType: g.Projection.Type,
		Projected:      g.Projection.NonKeyAttributes,
	}, nil
}

// UpdateTable adds or removes one Global Secondary Index, or accepts and ignores
// non-GSI field changes (billing mode, throughput, ...). GSI add/delete is
// synchronous: the index is ACTIVE when the call returns (a single transaction
// creates the catalog row + index table, backfills existing rows, and commits).
// At most one GlobalSecondaryIndexUpdates entry is allowed (AWS: one operation
// per call). All work runs on one *sql.Tx; a failure rolls the whole call back.
func (c *Client) UpdateTable(ctx context.Context, in UpdateTableInput) (UpdateTableOutput, error) {
	tx, err := c.store.BeginTx(ctx)
	if err != nil {
		return UpdateTableOutput{}, err
	}
	defer tx.Rollback()

	// Rule 1: table exists.
	def, err := c.store.GetTableDef(tx, in.TableName)
	if errors.Is(err, storage.ErrNotFound) {
		return UpdateTableOutput{}, fmt.Errorf("%w: table %q not found", ErrTableNotFound, in.TableName)
	}
	if err != nil {
		return UpdateTableOutput{}, err
	}

	// Rules 2–6: request shape.
	if err := validateUpdateTableShape(in); err != nil {
		return UpdateTableOutput{}, err
	}

	// Rule 7 (create) / rule 8 (delete): the single action, if any.
	if len(in.GlobalSecondaryIndexUpdates) == 1 {
		u := in.GlobalSecondaryIndexUpdates[0]
		if u.Create != nil {
			gd, err := validateCreateGsi(def, in)
			if err != nil {
				return UpdateTableOutput{}, err
			}
			if err := c.store.CreateGsiTable(tx, def, gd); err != nil {
				return UpdateTableOutput{}, err
			}
			if err := c.store.InsertGsiDef(tx, def.ID, gd); err != nil {
				return UpdateTableOutput{}, err
			}
			if err := c.backfillGsi(tx, def, gd); err != nil {
				return UpdateTableOutput{}, err
			}
		} else {
			name := *u.Delete
			found := false
			for _, g := range def.GSIs {
				if g.Name == name {
					found = true
					break
				}
			}
			if !found {
				return UpdateTableOutput{}, fmt.Errorf("%w: index %q not found", ErrGsiNotFoundForDelete, name)
			}
			if err := c.store.DropGsiTable(tx, def.Name, name); err != nil {
				return UpdateTableOutput{}, err
			}
			if err := c.store.DeleteGsiDef(tx, def.ID, name); err != nil {
				return UpdateTableOutput{}, err
			}
		}
	}

	// Reload the def so the returned description reflects the add/remove.
	reloaded, err := c.store.GetTableDef(tx, in.TableName)
	if err != nil {
		return UpdateTableOutput{}, err
	}
	if err := tx.Commit(); err != nil {
		return UpdateTableOutput{}, err
	}
	return UpdateTableOutput{TableDescription: describeFromDef(in.TableName, reloaded, creationTimeFromMeta(reloaded.Meta))}, nil
}

// backfillGsi scans every data row, decodes the item, and upserts a GSI index
// row for each indexable item (one whose GSI key attrs are all present and
// valid). Non-indexable items (sparse, wrong-typed, non-scalar, empty S/B,
// composite-missing-range) are skipped — matching real DynamoDB's backfill over
// items written before the GSI existed. Runs on the UpdateTable tx; a decode or
// upsert error aborts and the enclosing transaction rolls back.
func (c *Client) backfillGsi(tx *sql.Tx, def storage.TableDef, gd storage.GsiDef) error {
	next, err := c.store.ScanAllData(tx, def.Name)
	if err != nil {
		return err
	}
	for {
		id, data, err := next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("ddb: backfill scan index %q: %w", gd.Name, err)
		}
		var item Item
		if err := json.Unmarshal(data, &item); err != nil {
			return fmt.Errorf("ddb: backfill decode index %q: %w", gd.Name, err)
		}
		hv, rv, ok := gsiIndexKey(item, gd)
		if !ok {
			continue
		}
		if err := c.store.UpsertGsiRow(tx, def.Name, gd.Name, id, hv, rv); err != nil {
			return err
		}
	}
}
