package attrval

import (
	"encoding/json"
	"testing"

	"github.com/quells-bot/ddb-sqlite-core/internal/num"
)

// FuzzWireRoundTrip asserts any JSON that decodes to a Value round-trips
// through encode/decode to an Equal value and re-encodes to identical bytes.
func FuzzWireRoundTrip(f *testing.F) {
	seeds := []Value{
		NewNull(),
		NewString("hello"),
		NewNumber(mustDec("12.5")),
		NewBinary([]byte{0, 1, 2}),
		NewBool(true),
		NewList([]Value{NewString("a"), NewNumber(mustDec("1"))}),
		NewMap(map[string]Value{"k": NewString("v")}),
		NewStringSet([]string{"b", "a", "b"}),
		NewNumberSet([]num.Decimal{mustDec("1"), mustDec("1.0"), mustDec("2")}),
		NewBinarySet([][]byte{{1}, {2}, {1}}),
	}
	for _, s := range seeds {
		b, _ := json.Marshal(s)
		f.Add(string(b))
	}
	f.Fuzz(func(t *testing.T, in string) {
		var v Value
		if err := json.Unmarshal([]byte(in), &v); err != nil {
			return
		}
		b1, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal after unmarshal failed: %v", err)
		}
		var v2 Value
		if err := json.Unmarshal(b1, &v2); err != nil {
			t.Fatalf("re-unmarshal failed: %v", err)
		}
		if !v.Equal(v2) {
			t.Fatalf("round-trip not equal: %s", in)
		}
		b2, err := json.Marshal(v2)
		if err != nil {
			t.Fatalf("second marshal failed: %v", err)
		}
		if string(b1) != string(b2) {
			t.Fatalf("not idempotent: %s != %s", b1, b2)
		}
	})
}

// FuzzParsePath asserts ParsePath never panics and Lookup never panics for
// any path and any value shape.
func FuzzParsePath(f *testing.F) {
	for _, seed := range []string{"a", "a.b", "a[0]", "a.b[2].c", "a[0][1]", "x..y", "[0]"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		p, err := ParsePath(s)
		if err != nil {
			return
		}
		item := NewMap(map[string]Value{
			"a": NewList([]Value{NewMap(map[string]Value{"c": NewString("v")})}),
		})
		_, _ = item.Lookup(p)
		_, _ = NewNull().Lookup(p)
	})
}

// FuzzSetDedup asserts set construction is idempotent: building a set from
// an already-built set's elements yields an Equal value.
func FuzzSetDedup(f *testing.F) {
	f.Add("a", "b", "a", "c")
	f.Fuzz(func(t *testing.T, a, b, c, d string) {
		s1 := NewStringSet([]string{a, b, c, d})
		s2 := NewStringSet(s1.SS())
		if !s1.Equal(s2) {
			t.Fatalf("SS not idempotent: %v vs %v", s1.SS(), s2.SS())
		}
	})
}

// FuzzProject asserts Project never panics for arbitrary path lists over
// arbitrary items, that the result's top-level attributes are always a
// subset of the paths' root names, and that the receiver is unchanged
// (M6c §9).
func FuzzProject(f *testing.F) {
	doc := `{"a":{"M":{"b":{"L":[{"S":"x"},{"M":{"c":{"N":"1"}}}]}}},` +
		`"arr":{"L":[{"S":"p"},{"S":"q"},{"S":"r"}]},"s":{"S":"v"},"n":{"N":"3.14"}}`
	for _, seed := range [][3]string{
		{"a.b[1].c", "arr[1]", doc},
		{"a", "missing.deep[9]", doc},
		{"arr[0]", "arr[2]", doc},
		{"s", "n", doc},
		{"a.b[0]", "a.b[1]", doc},
		{"", "[0]", doc},
	} {
		f.Add(seed[0], seed[1], seed[2])
	}
	f.Fuzz(func(t *testing.T, p1, p2, doc string) {
		var m map[string]Value
		if err := json.Unmarshal([]byte(doc), &m); err != nil {
			return
		}
		var paths []Path
		roots := map[string]bool{}
		for _, ps := range []string{p1, p2} {
			p, err := ParsePath(ps)
			if err != nil {
				continue
			}
			paths = append(paths, p)
			roots[p[0].Name] = true
		}
		before, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("marshal item: %v", err)
		}
		out := Project(m, paths)
		for name := range out {
			if !roots[name] {
				t.Fatalf("Project emitted %q, not a path root (paths %q, %q)", name, p1, p2)
			}
		}
		after, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("re-marshal item: %v", err)
		}
		if string(before) != string(after) {
			t.Fatalf("Project mutated its receiver: %s -> %s", before, after)
		}
	})
}
