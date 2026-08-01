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

func mustPath(t *testing.T, s string) Path {
	t.Helper()
	p, err := ParsePath(s)
	if err != nil {
		t.Fatalf("ParsePath(%q): %v", s, err)
	}
	return p
}

func TestSetPath(t *testing.T) {
	base := NewMap(map[string]Value{
		"a":        NewMap(map[string]Value{"b": NewString("old")}),
		"l":        NewList([]Value{NewString("x"), NewString("y")}),
		"nullattr": NewNull(),
	})

	cases := []struct {
		name    string
		path    string
		nv      Value
		wantErr error
		check   func(t *testing.T, got Value)
	}{
		{
			name: "overwrite nested map value",
			path: "a.b",
			nv:   NewString("new"),
			check: func(t *testing.T, got Value) {
				v, ok := got.Lookup(mustPath(t, "a.b"))
				if !ok || v.Str() != "new" {
					t.Errorf("a.b = %v, %v; want new", v, ok)
				}
			},
		},
		// Only the FINAL segment of a path may be created. Every parent must
		// already exist, matching DynamoDB's "The document path provided in the
		// update expression is invalid for update".
		{
			name:    "missing top-level parent rejected",
			path:    "x.y",
			nv:      NewString("no"),
			wantErr: ErrPathMissingParent,
		},
		{
			name:    "missing intermediate parents rejected",
			path:    "x.y.z",
			nv:      NewString("no"),
			wantErr: ErrPathMissingParent,
		},
		{
			name:    "missing parent under an existing map rejected",
			path:    "a.z.c",
			nv:      NewString("no"),
			wantErr: ErrPathMissingParent,
		},
		{
			name: "final segment under an existing map is created",
			path: "a.fresh",
			nv:   NewString("leaf"),
			check: func(t *testing.T, got Value) {
				v, ok := got.Lookup(mustPath(t, "a.fresh"))
				if !ok || v.Str() != "leaf" {
					t.Errorf("a.fresh = %v, %v; want leaf", v, ok)
				}
				if v, ok := got.Lookup(mustPath(t, "a.b")); !ok || v.Str() != "old" {
					t.Errorf("a.b = %v, %v; want the sibling preserved", v, ok)
				}
			},
		},
		{
			name: "new top-level attribute is created",
			path: "fresh",
			nv:   NewString("top"),
			check: func(t *testing.T, got Value) {
				v, ok := got.Lookup(mustPath(t, "fresh"))
				if !ok || v.Str() != "top" {
					t.Errorf("fresh = %v, %v; want top", v, ok)
				}
			},
		},
		{
			name: "overwrite existing list element",
			path: "l[0]",
			nv:   NewString("z"),
			check: func(t *testing.T, got Value) {
				v, _ := got.Lookup(mustPath(t, "l[0]"))
				if v.Str() != "z" {
					t.Errorf("l[0] = %q, want z", v.Str())
				}
			},
		},
		{
			name: "append at len(list)",
			path: "l[2]",
			nv:   NewString("app"),
			check: func(t *testing.T, got Value) {
				v, ok := got.Lookup(mustPath(t, "l[2]"))
				if !ok || v.Str() != "app" {
					t.Errorf("l[2] = %v, %v; want app", v, ok)
				}
			},
		},
		{
			// DynamoDB clamps any out-of-range list index to an append; it
			// neither pads the list nor rejects the index.
			name: "index beyond the end appends",
			path: "l[5]",
			nv:   NewString("app"),
			check: func(t *testing.T, got Value) {
				l, ok := got.Lookup(mustPath(t, "l"))
				if !ok || l.Tag() != TagList {
					t.Fatalf("l = %v, %v; want a list", l, ok)
				}
				if len(l.List()) != 3 {
					t.Fatalf("len(l) = %d, want 3 (appended, not padded)", len(l.List()))
				}
				if l.List()[0].Str() != "x" || l.List()[1].Str() != "y" || l.List()[2].Str() != "app" {
					t.Errorf("l = %v, want [x y app]", l.List())
				}
			},
		},
		{
			name:    "descend through non-container rejected",
			path:    "a.b.c",
			nv:      NewString("no"),
			wantErr: ErrPathType,
		},
		{
			name:    "index into a map rejected",
			path:    "a[0]",
			nv:      NewString("no"),
			wantErr: ErrPathType,
		},
		{
			name:    "descend through an explicit NULL rejected",
			path:    "nullattr.b",
			nv:      NewString("no"),
			wantErr: ErrPathType,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := base.SetPath(mustPath(t, tc.path), tc.nv)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("SetPath: %v", err)
			}
			tc.check(t, got)
		})
	}
}

func TestSetPathDoesNotMutateReceiver(t *testing.T) {
	base := NewMap(map[string]Value{"a": NewMap(map[string]Value{"b": NewString("old")})})
	if _, err := base.SetPath(mustPath(t, "a.b"), NewString("new")); err != nil {
		t.Fatalf("SetPath: %v", err)
	}
	v, _ := base.Lookup(mustPath(t, "a.b"))
	if v.Str() != "old" {
		t.Errorf("receiver mutated: a.b = %q, want old", v.Str())
	}
}

func TestRemovePath(t *testing.T) {
	base := NewMap(map[string]Value{
		"a": NewMap(map[string]Value{"b": NewString("v"), "c": NewString("w")}),
		"l": NewList([]Value{NewString("x"), NewString("y"), NewString("z")}),
	})

	t.Run("removes map key", func(t *testing.T) {
		got := base.RemovePath(mustPath(t, "a.b"))
		if _, ok := got.Lookup(mustPath(t, "a.b")); ok {
			t.Error("a.b still present")
		}
		if _, ok := got.Lookup(mustPath(t, "a.c")); !ok {
			t.Error("a.c was removed too")
		}
	})

	t.Run("list index removes and shifts", func(t *testing.T) {
		got := base.RemovePath(mustPath(t, "l[0]"))
		l, _ := got.Lookup(mustPath(t, "l"))
		if len(l.List()) != 2 {
			t.Fatalf("len = %d, want 2", len(l.List()))
		}
		if l.List()[0].Str() != "y" || l.List()[1].Str() != "z" {
			t.Errorf("list = %v, want [y z]", l.List())
		}
	})

	t.Run("missing path is a no-op", func(t *testing.T) {
		got := base.RemovePath(mustPath(t, "nope.deep"))
		if !got.Equal(base) {
			t.Error("value changed on no-op remove")
		}
	})

	t.Run("does not prune emptied container", func(t *testing.T) {
		one := NewMap(map[string]Value{"a": NewMap(map[string]Value{"b": NewString("v")})})
		got := one.RemovePath(mustPath(t, "a.b"))
		a, ok := got.Lookup(mustPath(t, "a"))
		if !ok {
			t.Fatal("a was pruned")
		}
		if a.Tag() != TagMap || len(a.Map()) != 0 {
			t.Errorf("a = %v, want empty map", a)
		}
	})
}
