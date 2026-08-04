package ddb

import (
	"strings"
	"testing"

	"github.com/quells-bot/ddb-sqlite-core/attrval"
	"github.com/quells-bot/ddb-sqlite-core/internal/num"
)

func TestItemSize(t *testing.T) {
	zero := mustNum("0")

	cases := []struct {
		name  string
		item  Item
		bytes int64
		depth int
	}{
		{
			name:  "single string",
			item:  Item{"pk": attrval.NewString("k")},
			bytes: 3, // "pk"=2 + "k"=1
			depth: 1,
		},
		{
			name:  "binary raw bytes not base64",
			item:  Item{"b": attrval.NewBinary([]byte{0x00, 0xff})},
			bytes: 3, // "b"=1 + 2 raw bytes
			depth: 1,
		},
		{
			name:  "number 1 sig digit",
			item:  Item{"n": attrval.NewNumber(mustNum("1"))},
			bytes: 3, // "n"=1 + ceil(1/2)+1 = 2
			depth: 1,
		},
		{
			name:  "number zero is size 1",
			item:  Item{"n": attrval.NewNumber(zero)},
			bytes: 2, // "n"=1 + ceil(0/2)+1 = 1
			depth: 1,
		},
		{
			name:  "number 38 sig digits",
			item:  Item{"n": attrval.NewNumber(mustNum("99999999999999999999999999999999999999"))},
			bytes: 21, // "n"=1 + ceil(38/2)+1 = 20
			depth: 1,
		},
		{
			name:  "number trailing zeros trimmed 100 -> 1 sig digit",
			item:  Item{"n": attrval.NewNumber(mustNum("100"))},
			bytes: 3, // "n"=1 + ceil(1/2)+1 = 2
			depth: 1,
		},
		{
			name:  "bool",
			item:  Item{"b": attrval.NewBool(true)},
			bytes: 2, // "b"=1 + 1
			depth: 1,
		},
		{
			name:  "null",
			item:  Item{"x": attrval.NewNull()},
			bytes: 2, // "x"=1 + 1
			depth: 1,
		},
		{
			name:  "empty list",
			item:  Item{"l": attrval.NewList(nil)},
			bytes: 4, // "l"=1 + 3
			depth: 1,
		},
		{
			name: "list one string element",
			item: Item{"l": attrval.NewList([]attrval.Value{attrval.NewString("a")})},
			// "l"=1 + 3 + (1 overhead + 1 string byte) = 6
			bytes: 6,
			depth: 2,
		},
		{
			name:  "empty map",
			item:  Item{"m": attrval.NewMap(nil)},
			bytes: 4, // "m"=1 + 3
			depth: 1,
		},
		{
			name: "map one string entry",
			item: Item{"m": attrval.NewMap(map[string]attrval.Value{"k": attrval.NewString("v")})},
			// "m"=1 + 3 + (1 overhead + "k"=1 + "v"=1) = 7
			bytes: 7,
			depth: 2,
		},
		{
			name: "string set no container overhead",
			item: Item{"ss": attrval.NewStringSet([]string{"ab", "c"})},
			// "ss"=2 + 2 + 1 = 5
			bytes: 5,
			depth: 1,
		},
		{
			name: "number set zero element",
			item: Item{"ns": attrval.NewNumberSet([]num.Decimal{zero})},
			// "ns"=2 + 1 (zero number size) = 3
			bytes: 3,
			depth: 1,
		},
		{
			name: "binary set",
			item: Item{"bs": attrval.NewBinarySet([][]byte{{1}, {2}})},
			// "bs"=2 + 1 + 1 = 4
			bytes: 4,
			depth: 1,
		},
		{
			name: "nested map depth 3",
			item: Item{"a": attrval.NewMap(map[string]attrval.Value{
				"b": attrval.NewMap(map[string]attrval.Value{
					"c": attrval.NewString("x"),
				}),
			})},
			bytes: 12, // "a"=1 + 3 + (1+"b"=1 + 3 + (1+"c"=1 + 1)) = 12
			depth: 3,
		},
		{
			name: "nested list depth 3",
			item: Item{"l": attrval.NewList([]attrval.Value{
				attrval.NewList([]attrval.Value{
					attrval.NewString("x"),
				}),
			})},
			bytes: 10, // "l"=1 + 3 + (1 + 3 + (1 + 1)) = 10
			depth: 3,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotBytes, gotDepth := itemSize(tc.item)
			if gotBytes != tc.bytes {
				t.Errorf("bytes = %d, want %d", gotBytes, tc.bytes)
			}
			if gotDepth != tc.depth {
				t.Errorf("depth = %d, want %d", gotDepth, tc.depth)
			}
		})
	}
}

func TestItemSizeExactBoundary(t *testing.T) {
	// Probe-verified: {pk:"k", big:S(n)} where 2+1+3+n = 409600 -> n=409594 accepted.
	ok := Item{"pk": attrval.NewString("k"), "big": attrval.NewString(strings.Repeat("x", 409594))}
	if b, _ := itemSize(ok); b != maxItemSize {
		t.Fatalf("boundary item = %d bytes, want %d", b, maxItemSize)
	}
	over := Item{"pk": attrval.NewString("k"), "big": attrval.NewString(strings.Repeat("x", 409595))}
	if b, _ := itemSize(over); b != maxItemSize+1 {
		t.Fatalf("over item = %d bytes, want %d", b, maxItemSize+1)
	}
}
