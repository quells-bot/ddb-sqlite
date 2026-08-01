package attrval

import (
	"bytes"
	"fmt"
	"sort"

	"github.com/quells-bot/ddb-sqlite/internal/num"
)

// dedupStrings returns the unique strings from in, sorted lexicographically.
func dedupStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// dedupNumbers returns the unique decimals from in (1 and 1.0 collide),
// sorted numerically. Dedup keys on the canonical string, where both "1"
// and "1.0" canonicalize to "1".
func dedupNumbers(in []num.Decimal) []num.Decimal {
	seen := make(map[string]struct{}, len(in))
	out := make([]num.Decimal, 0, len(in))
	for _, d := range in {
		key := d.String()
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Less(out[j]) })
	return out
}

// dedupBytes returns the unique byte slices from in, sorted by byte order.
// Returned slices are copies of the inputs.
func dedupBytes(in [][]byte) [][]byte {
	seen := make(map[string]struct{}, len(in))
	out := make([][]byte, 0, len(in))
	for _, b := range in {
		key := string(b)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		cp := make([]byte, len(b))
		copy(cp, b)
		out = append(out, cp)
	}
	sort.Slice(out, func(i, j int) bool { return bytes.Compare(out[i], out[j]) < 0 })
	return out
}

// NewStringSet constructs a StringSet, deduplicating and sorting the items.
func NewStringSet(items []string) Value {
	return Value{tag: TagStringSet, ss: dedupStrings(items)}
}

// NewNumberSet constructs a NumberSet, deduplicating by numeric value and
// sorting numerically.
func NewNumberSet(items []num.Decimal) Value {
	return Value{tag: TagNumberSet, ns: dedupNumbers(items)}
}

// NewBinarySet constructs a BinarySet, deduplicating and sorting the items.
func NewBinarySet(items [][]byte) Value {
	return Value{tag: TagBinarySet, bs: dedupBytes(items)}
}

// SS returns the StringSet elements (deduped, sorted). Valid only when
// Tag()==TagStringSet.
func (v Value) SS() []string { return v.ss }

// NS returns the NumberSet elements (deduped, sorted). Valid only when
// Tag()==TagNumberSet.
func (v Value) NS() []num.Decimal { return v.ns }

// NewNumberSetFromStrings constructs a NumberSet from DynamoDB wire number
// strings, parsing and validating each via num.Parse and deduplicating
// numerically (so "1" and "1.0" collide). It is the string-based entry point
// used by the adapter, which must not import internal/num.
func NewNumberSetFromStrings(items []string) (Value, error) {
	decs := make([]num.Decimal, 0, len(items))
	for _, s := range items {
		d, err := num.Parse(s)
		if err != nil {
			return Value{}, fmt.Errorf("attrval: bad number set member %q: %w", s, err)
		}
		decs = append(decs, d)
	}
	return NewNumberSet(decs), nil
}

// BS returns the BinarySet elements (deduped, sorted). Valid only when
// Tag()==TagBinarySet.
func (v Value) BS() [][]byte { return v.bs }
