package ddb

import (
	"encoding/json"
	"strings"
	"testing"
)

// FuzzItemSize asserts itemSize never panics on arbitrary items, always
// returns a non-negative byte count, and reports depth >= 1 for any
// non-empty item (M6c §9). Items are decoded from wire JSON so the fuzzer
// explores the full attrval tag space.
func FuzzItemSize(f *testing.F) {
	// 40 levels of nested M around a scalar leaf -> valueDepth 41, past
	// maxItemDepth (32); itemSize only measures, it does not enforce.
	deep := `{"S":"leaf"}`
	for i := 0; i < 40; i++ {
		deep = `{"M":{"d":` + deep + `}}`
	}
	seeds := []string{
		`{}`,
		`{"pk":{"S":"x"}}`,
		// All ten tags in one item.
		`{"s":{"S":"v"},"n":{"N":"12.5"},"b":{"B":"AAEC"},"bool":{"BOOL":true},` +
			`"null":{"NULL":true},"l":{"L":[{"S":"a"},{"N":"1"}]},"m":{"M":{"k":{"S":"v"}}},` +
			`"ss":{"SS":["b","a"]},"ns":{"NS":["1","2.0"]},"bs":{"BS":["AQ==","Ag=="]}}`,
		// Deep nesting.
		`{"a":` + deep + `}`,
		// Huge string.
		`{"big":{"S":"` + strings.Repeat("x", 100000) + `"}}`,
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		var item Item
		if err := json.Unmarshal([]byte(in), &item); err != nil {
			return
		}
		bytes, depth := itemSize(item)
		if bytes < 0 {
			t.Fatalf("negative size %d for %q", bytes, in)
		}
		if len(item) > 0 && depth < 1 {
			t.Fatalf("non-empty item with depth %d: %q", depth, in)
		}
	})
}
