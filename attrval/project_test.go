package attrval

import "testing"

func projTestItem() map[string]Value {
	return map[string]Value{
		"pk":   NewString("P1"),
		"top":  NewString("topval"),
		"num":  mustNumVal("42"),
		"flag": NewBool(true),
		"obj": NewMap(map[string]Value{
			"a": NewString("aval"),
			"b": NewString("bval"),
			"nested": NewMap(map[string]Value{
				"x": NewString("xval"),
				"y": NewString("yval"),
			}),
		}),
		"arr":  NewList([]Value{NewString("e0"), NewString("e1"), NewString("e2")}),
		"marr": NewList([]Value{
			NewMap(map[string]Value{"x": NewString("x0"), "y": NewString("y0")}),
			NewMap(map[string]Value{"x": NewString("x1"), "y": NewString("y1")}),
		}),
	}
}

func mustNumVal(s string) Value {
	v, err := NewNumberString(s)
	if err != nil {
		panic(err)
	}
	return v
}

func pp(t *testing.T, ss ...string) []Path {
	t.Helper()
	out := make([]Path, 0, len(ss))
	for _, s := range ss {
		p, err := ParsePath(s)
		if err != nil {
			t.Fatalf("ParsePath(%q): %v", s, err)
		}
		out = append(out, p)
	}
	return out
}

func TestProject(t *testing.T) {
	item := projTestItem()
	cases := []struct {
		name  string
		paths []Path
		check func(t *testing.T, got map[string]Value)
	}{
		{"single top-level", pp(t, "top"), func(t *testing.T, got map[string]Value) {
			if len(got) != 1 || got["top"].Str() != "topval" {
				t.Errorf("got %v", got)
			}
		}},
		{"multiple top-level", pp(t, "top", "num"), func(t *testing.T, got map[string]Value) {
			if len(got) != 2 || got["top"].Str() != "topval" || got["num"].Num().String() != "42" {
				t.Errorf("got %v", got)
			}
		}},
		{"missing attr omitted", pp(t, "nonexistent"), func(t *testing.T, got map[string]Value) {
			if len(got) != 0 {
				t.Errorf("got %v, want empty", got)
			}
		}},
		{"nested spine preserved", pp(t, "obj.nested.x"), func(t *testing.T, got map[string]Value) {
			obj := got["obj"].Map()
			if len(got) != 1 || len(obj) != 1 || obj["nested"].Map()["x"].Str() != "xval" {
				t.Errorf("got %v", got)
			}
		}},
		{"sibling nested merge", pp(t, "obj.a", "obj.b"), func(t *testing.T, got map[string]Value) {
			obj := got["obj"].Map()
			if len(obj) != 2 || obj["a"].Str() != "aval" || obj["b"].Str() != "bval" {
				t.Errorf("got %v", got)
			}
		}},
		{"list element compacted", pp(t, "arr[1]"), func(t *testing.T, got map[string]Value) {
			l := got["arr"].List()
			if len(l) != 1 || l[0].Str() != "e1" {
				t.Errorf("got %v", got)
			}
		}},
		{"multi-index path order", pp(t, "arr[0]", "arr[2]"), func(t *testing.T, got map[string]Value) {
			l := got["arr"].List()
			if len(l) != 2 || l[0].Str() != "e0" || l[1].Str() != "e2" {
				t.Errorf("got %v", got)
			}
		}},
		{"multi-index reversed paths", pp(t, "arr[2]", "arr[0]"), func(t *testing.T, got map[string]Value) {
			l := got["arr"].List()
			// source-index order (probe-verified, §2.5 probe 24): [e0, e2]
			if len(l) != 2 || l[0].Str() != "e0" || l[1].Str() != "e2" {
				t.Errorf("got %v", got)
			}
		}},
		{"convergent paths one element", pp(t, "marr[1].x", "marr[1].y"), func(t *testing.T, got map[string]Value) {
			l := got["marr"].List()
			if len(l) != 1 {
				t.Fatalf("got %v, want one merged element", got)
			}
			m := l[0].Map()
			if len(m) != 2 || m["x"].Str() != "x1" || m["y"].Str() != "y1" {
				t.Errorf("got %v", got)
			}
		}},
		{"descend into scalar omitted", pp(t, "top.nested"), func(t *testing.T, got map[string]Value) {
			if len(got) != 0 {
				t.Errorf("got %v, want empty (path does not resolve)", got)
			}
		}},
		{"partial miss pruned", pp(t, "obj.missing"), func(t *testing.T, got map[string]Value) {
			if len(got) != 0 {
				t.Errorf("got %v, want empty (empty spine pruned)", got)
			}
		}},
		{"index out of range omitted", pp(t, "arr[9]"), func(t *testing.T, got map[string]Value) {
			if len(got) != 0 {
				t.Errorf("got %v, want empty", got)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.check(t, Project(item, tc.paths))
		})
	}
}

func TestProjectDoesNotMutateReceiver(t *testing.T) {
	item := projTestItem()
	before := NewMap(item)
	_ = Project(item, pp(t, "obj.a", "arr[1]", "marr[1].x"))
	if !NewMap(item).Equal(before) {
		t.Error("Project mutated the receiver item")
	}
}
