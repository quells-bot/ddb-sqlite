package attrval

import (
	"errors"
	"testing"
)

func TestParsePath(t *testing.T) {
	cases := []struct {
		in   string
		want Path
	}{
		{"a", Path{{Name: "a"}}},
		{"a.b", Path{{Name: "a"}, {Name: "b"}}},
		{"a[0]", Path{{Name: "a"}, {IsIndex: true, Index: 0}}},
		{"a.b[2].c", Path{{Name: "a"}, {Name: "b"}, {IsIndex: true, Index: 2}, {Name: "c"}}},
		{"a[0][1]", Path{{Name: "a"}, {IsIndex: true, Index: 0}, {IsIndex: true, Index: 1}}},
	}
	for _, c := range cases {
		got, err := ParsePath(c.in)
		if err != nil {
			t.Errorf("ParsePath(%q) error: %v", c.in, err)
			continue
		}
		if !pathEqual(got, c.want) {
			t.Errorf("ParsePath(%q) = %v, want %v", c.in, got, c.want)
		}
	}

	for _, in := range []string{".a", "a.", "a[]", "a[", "a[x]", "a.b[", "a]", "a.[0]"} {
		if _, err := ParsePath(in); err == nil {
			t.Errorf("ParsePath(%q) expected error", in)
		}
	}

	if _, err := ParsePath(""); !errors.Is(err, ErrEmptyPath) {
		t.Errorf("ParsePath(\"\") err = %v, want ErrEmptyPath", err)
	}
}

func pathEqual(a, b Path) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].IsIndex != b[i].IsIndex || a[i].Name != b[i].Name || a[i].Index != b[i].Index {
			return false
		}
	}
	return true
}

func TestLookup(t *testing.T) {
	item := NewMap(map[string]Value{
		"a": NewMap(map[string]Value{
			"b": NewList([]Value{
				NewString("x"),
				NewMap(map[string]Value{"c": NewString("v")}),
			}),
		}),
		"present": NewNull(),
	})
	cases := []struct {
		path  string
		found bool
		tag   Tag
	}{
		{"a", true, TagMap},
		{"a.b", true, TagList},
		{"a.b[0]", true, TagString},
		{"a.b[1].c", true, TagString},
		{"a.b[2]", false, TagNull},   // index out of range
		{"a.b[1].d", false, TagNull}, // missing map key mid-path
		{"missing", false, TagNull},  // missing top-level attribute
		{"present", true, TagNull},   // present Null IS found
		{"a.c", false, TagNull},      // map missing key mid-path
		{"a[0]", false, TagNull},     // index segment on a Map is not found
	}
	for _, c := range cases {
		p, err := ParsePath(c.path)
		if err != nil {
			t.Fatalf("ParsePath(%q): %v", c.path, err)
		}
		v, found := item.Lookup(p)
		if found != c.found {
			t.Errorf("Lookup(%q) found = %v, want %v", c.path, found, c.found)
		}
		if found && v.Tag() != c.tag {
			t.Errorf("Lookup(%q) tag = %v, want %v", c.path, v.Tag(), c.tag)
		}
	}
}
