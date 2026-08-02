package ddb

import (
	"database/sql"
	"fmt"

	"github.com/quells-bot/ddb-sqlite/attrval"
	"github.com/quells-bot/ddb-sqlite/internal/expr"
	"github.com/quells-bot/ddb-sqlite/internal/storage"
)

// validateGsiKeys checks every present GSI key attribute on the post-write item
// against the declared index key types. A present key attr must be a scalar
// (S/N/B) matching the declared type and non-empty for S/B. SQLite STRICT
// affinity coercion silently accepts lossless mismatches, so this check must
// live in Go (spec §2.3). A composite GSI indexes an item only when both key
// attrs are present; an absent key attr is not an error here (sparsity).
func validateGsiKeys(item Item, gsis []storage.GsiDef) error {
	for _, g := range gsis {
		if err := validateOneGsiKey(item, g.Hash, g.HashType); err != nil {
			return fmt.Errorf("%w: index %q: %v", ErrValidation, g.Name, err)
		}
		if g.Range != "" {
			if err := validateOneGsiKey(item, g.Range, g.RangeType); err != nil {
				return fmt.Errorf("%w: index %q: %v", ErrValidation, g.Name, err)
			}
		}
	}
	return nil
}

func validateOneGsiKey(item Item, name, keyType string) error {
	v, ok := item[name]
	if !ok {
		return nil // absent = sparse, not an error
	}
	wantTag := tagForKeyType(keyType)
	if v.Tag() != wantTag {
		return fmt.Errorf("Type mismatch for Index Key %q", name)
	}
	// Non-scalar types can't reach here (tagForKeyType only returns S/N/B tags),
	// but a non-scalar VALUE assigned to a key attr would have the wrong tag and
	// is caught above. Empty S/B key values are rejected.
	switch v.Tag() {
	case attrval.TagString:
		if v.Str() == "" {
			return fmt.Errorf("The AttributeValue for a key attribute %q cannot contain an empty string value", name)
		}
	case attrval.TagBinary:
		if len(v.Bin()) == 0 {
			return fmt.Errorf("The AttributeValue for a key attribute %q cannot contain an empty binary value", name)
		}
	}
	return nil
}

// maintainGsiRows upserts GSI index rows for each GSI whose key attrs are all
// present on the post-write item. Runs after the data write (which returned
// dataID) so the ON DELETE CASCADE from the data-row REPLACE has already
// cleaned old GSI rows. Items missing a GSI partition key are sparse (no row).
func (c *Client) maintainGsiRows(tx *sql.Tx, table string, gsis []storage.GsiDef, dataID int64, item Item) error {
	for _, g := range gsis {
		hv, ok := item[g.Hash]
		if !ok {
			continue // sparse: no partition key
		}
		hashVal, err := keyValue(hv)
		if err != nil {
			return err
		}
		var rangeVal any
		if g.Range != "" {
			rv, ok := item[g.Range]
			if !ok {
				continue // composite GSI, sort absent: not indexed (probe G20)
			}
			rangeVal, err = keyValue(rv)
			if err != nil {
				return err
			}
		}
		if err := c.store.UpsertGsiRow(tx, table, g.Name, dataID, hashVal, rangeVal); err != nil {
			return err
		}
	}
	return nil
}

// lookupGsi finds a GSI by name on the table def.
func lookupGsi(def storage.TableDef, name string) (storage.GsiDef, error) {
	for _, g := range def.GSIs {
		if g.Name == name {
			return g, nil
		}
	}
	return storage.GsiDef{}, fmt.Errorf("%w: index %q not found on table %q", ErrGsiNotFound, name, def.Name)
}

// gsiKeyAttrsForFilter returns table keys + GSI keys for ValidateFilterKeys.
func gsiKeyAttrsForFilter(def storage.TableDef, gsi storage.GsiDef) []string {
	out := keyAttrs(def)
	out = append(out, gsi.Hash)
	if gsi.Range != "" {
		out = append(out, gsi.Range)
	}
	return out
}

// gsiProjectionAttrs returns the set of attributes to keep for a GSI read.
func gsiProjectionAttrs(def storage.TableDef, gsi storage.GsiDef) map[string]bool {
	keep := map[string]bool{}
	// Table keys.
	keep[def.Hash] = true
	if def.Range != "" {
		keep[def.Range] = true
	}
	// GSI keys.
	keep[gsi.Hash] = true
	if gsi.Range != "" {
		keep[gsi.Range] = true
	}
	switch gsi.ProjectionType {
	case "ALL":
		return nil // nil = keep everything
	case "KEYS_ONLY":
		return keep
	case "INCLUDE":
		for _, a := range gsi.Projected {
			keep[a] = true
		}
		return keep
	}
	return keep
}

// projectItem copies only the attributes in keep that are present in item.
// keep == nil means keep everything.
func projectItem(item Item, keep map[string]bool) Item {
	if keep == nil {
		return item
	}
	out := make(Item, len(item))
	for k, v := range item {
		if keep[k] {
			out[k] = v
		}
	}
	return out
}

// gsiLastEvaluatedKey builds the LEK (GSI keys + table keys) from the last scanned item.
func gsiLastEvaluatedKey(def storage.TableDef, gsi storage.GsiDef, lastItem Item) Item {
	lek := Item{}
	if v, ok := lastItem[gsi.Hash]; ok {
		lek[gsi.Hash] = v
	}
	if gsi.Range != "" {
		if v, ok := lastItem[gsi.Range]; ok {
			lek[gsi.Range] = v
		}
	}
	if v, ok := lastItem[def.Hash]; ok {
		lek[def.Hash] = v
	}
	if def.Range != "" {
		if v, ok := lastItem[def.Range]; ok {
			lek[def.Range] = v
		}
	}
	return lek
}

// validateGsiEskShape validates that an ExclusiveStartKey carries exactly the
// union of table key attrs and GSI key attrs (deduped), and that each present
// attr carries the type tag matching its declared key type. Shared by GSI Query
// (validateGsiExclusiveStartKey) and GSI Scan (resolveGsiScanAfterID) resume.
func validateGsiEskShape(def storage.TableDef, gsi storage.GsiDef, key Item) error {
	want := map[string]bool{def.Hash: true}
	if def.Range != "" {
		want[def.Range] = true
	}
	want[gsi.Hash] = true
	if gsi.Range != "" {
		want[gsi.Range] = true
	}
	if len(key) != len(want) {
		return fmt.Errorf("The provided starting key is invalid")
	}
	for attr := range key {
		if !want[attr] {
			return fmt.Errorf("The provided starting key is invalid")
		}
	}
	if v, ok := key[def.Hash]; ok && v.Tag() != tagForKeyType(def.HashType) {
		return fmt.Errorf("The provided starting key is invalid")
	}
	if def.Range != "" {
		if v, ok := key[def.Range]; ok && v.Tag() != tagForKeyType(def.RangeType) {
			return fmt.Errorf("The provided starting key is invalid")
		}
	}
	if v, ok := key[gsi.Hash]; ok && v.Tag() != tagForKeyType(gsi.HashType) {
		return fmt.Errorf("The provided starting key is invalid")
	}
	if gsi.Range != "" {
		if v, ok := key[gsi.Range]; ok && v.Tag() != tagForKeyType(gsi.RangeType) {
			return fmt.Errorf("The provided starting key is invalid")
		}
	}
	return nil
}

// validateGsiExclusiveStartKey validates the Query ESK carries exactly the union
// of table key attrs and GSI key attrs (deduped), resolves the table key to a
// data_id, and builds a GsiResume. It is a *Client method because it must
// resolve the data_id via store.GetItem. Returns (nil, nil) for a stale key
// (row deleted) so the query resumes from the beginning of the GSI partition.
func (c *Client) validateGsiExclusiveStartKey(tx *sql.Tx, def storage.TableDef, gsi storage.GsiDef, key Item, kc expr.KeyCondition) (*storage.GsiResume, error) {
	if err := validateGsiEskShape(def, gsi, key); err != nil {
		return nil, err
	}
	// GSI partition must match the key condition's partition.
	if v, ok := key[gsi.Hash]; ok && !v.Equal(kc.Partition.Value) {
		return nil, fmt.Errorf("The provided starting key does not match the range key predicate")
	}
	// Resolve table key -> data_id via the data table (one indexed lookup).
	hv := key[def.Hash]
	hashVal, err := keyValue(hv)
	if err != nil {
		return nil, err
	}
	var rangeVal any
	if def.Range != "" {
		rv := key[def.Range]
		rangeVal, err = keyValue(rv)
		if err != nil {
			return nil, err
		}
	}
	id, _, found, err := c.store.GetItem(tx, def.Name, hashVal, rangeVal)
	if err != nil {
		return nil, err
	}
	if !found {
		// Stale key (row deleted since the prior page): resume from the beginning.
		return nil, nil
	}
	resume := &storage.GsiResume{DataID: id}
	if gsi.Range != "" {
		if v, ok := key[gsi.Range]; ok {
			rv, err := keyValue(v)
			if err != nil {
				return nil, err
			}
			resume.Range = rv
		}
	}
	return resume, nil
}

// resolveGsiScanAfterID validates a GSI Scan ExclusiveStartKey — which carries
// the union of table and GSI key attrs (from gsiLastEvaluatedKey) — and resolves
// the TABLE key to the data_id used as the scan resume position. Returns 0
// (scan from the beginning) for a stale key (row deleted since the prior page).
// GSI Scan has no key condition, so there is no partition-vs-key-condition check.
func (c *Client) resolveGsiScanAfterID(tx *sql.Tx, def storage.TableDef, gsi storage.GsiDef, key Item) (int64, error) {
	if err := validateGsiEskShape(def, gsi, key); err != nil {
		return 0, err
	}
	hv := key[def.Hash]
	hashVal, err := keyValue(hv)
	if err != nil {
		return 0, err
	}
	var rangeVal any
	if def.Range != "" {
		rv := key[def.Range]
		rangeVal, err = keyValue(rv)
		if err != nil {
			return 0, err
		}
	}
	id, _, found, err := c.store.GetItem(tx, def.Name, hashVal, rangeVal)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, nil // stale key: scan from beginning
	}
	return id, nil
}
