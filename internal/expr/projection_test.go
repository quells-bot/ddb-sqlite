package expr

import (
	"errors"
	"testing"

	"github.com/quells-bot/ddb-sqlite-core/attrval"
)

func pathsEq(a, b []attrval.Path) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if len(a[i]) != len(b[i]) {
			return false
		}
		for j := range a[i] {
			if a[i][j] != b[i][j] {
				return false
			}
		}
	}
	return true
}

func mustParsePath(t *testing.T, s string) attrval.Path {
	t.Helper()
	p, err := attrval.ParsePath(s)
	if err != nil {
		t.Fatalf("attrval.ParsePath(%q): %v", s, err)
	}
	return p
}

func TestParseProjection(t *testing.T) {
	cases := []struct {
		src   string
		want  []attrval.Path
		names []string
	}{
		{"top", []attrval.Path{mustParsePath(t, "top")}, nil},
		{"a, b, c", []attrval.Path{mustParsePath(t, "a"), mustParsePath(t, "b"), mustParsePath(t, "c")}, nil},
		{"obj.nested.x", []attrval.Path{mustParsePath(t, "obj.nested.x")}, nil},
		{"arr[1]", []attrval.Path{mustParsePath(t, "arr[1]")}, nil},
		{"#t, obj.a", []attrval.Path{mustParsePath(t, "#t"), mustParsePath(t, "obj.a")}, []string{"#t"}},
		{"arr[0][2].x", []attrval.Path{mustParsePath(t, "arr[0][2].x")}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.src, func(t *testing.T) {
			p, err := ParseProjection(tc.src)
			if err != nil {
				t.Fatalf("ParseProjection(%q): %v", tc.src, err)
			}
			names, values := p.Refs()
			if len(values) != 0 {
				t.Errorf("Refs values = %v, want nil", values)
			}
			if len(names) != len(tc.names) || (len(names) > 0 && names[0] != tc.names[0]) {
				t.Errorf("Refs names = %v, want %v", names, tc.names)
			}
			// Structural check: parsePath results bind to the wanted paths
			// (aliases resolve to themselves here only to verify shape).
			env := Env{Names: map[string]string{}}
			for _, n := range names {
				env.Names[n] = n
			}
			b, err := p.Bind(env)
			if err != nil {
				t.Fatalf("Bind: %v", err)
			}
			var want []attrval.Path
			for _, wp := range tc.want {
				r := make(attrval.Path, len(wp))
				copy(r, wp)
				want = append(want, r)
			}
			if !pathsEq(b.Paths(), want) {
				t.Errorf("Paths() = %v, want %v", b.Paths(), want)
			}
		})
	}
}

func TestParseProjectionRejects(t *testing.T) {
	for _, src := range []string{
		"",        // empty
		":v",      // value ref
		"size(a)", // function
		"a = :v",  // comparator
		"a, ",     // trailing comma
		",a",      // leading comma
		"a b",     // missing comma
		"a, a[",   // unterminated index
	} {
		if _, err := ParseProjection(src); err == nil {
			t.Errorf("ParseProjection(%q): want error", src)
		}
	}
	if _, err := ParseProjection(""); !errors.Is(err, ErrSyntax) {
		t.Errorf("empty: err = %v, want ErrSyntax", err)
	}
}

func TestProjectionBind(t *testing.T) {
	p, err := ParseProjection("#t, obj.#n")
	if err != nil {
		t.Fatalf("ParseProjection: %v", err)
	}
	b, err := p.Bind(Env{Names: map[string]string{"#t": "top", "#n": "nested"}})
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	want := []attrval.Path{mustParsePath(t, "top"), mustParsePath(t, "obj.nested")}
	if !pathsEq(b.Paths(), want) {
		t.Errorf("Paths() = %v, want %v", b.Paths(), want)
	}

	// Undefined #name -> ErrUndefined.
	if _, err := p.Bind(Env{Names: map[string]string{"#t": "top"}}); !errors.Is(err, ErrUndefined) {
		t.Errorf("undefined: err = %v, want ErrUndefined", err)
	}

	// Bare reserved word -> ErrSemantic; via alias -> OK.
	if _, err := ParseProjection("name"); err != nil {
		t.Fatalf("ParseProjection(name): %v", err)
	}
	rw, _ := ParseProjection("name")
	if _, err := rw.Bind(Env{}); !errors.Is(err, ErrSemantic) {
		t.Errorf("reserved bare: err = %v, want ErrSemantic", err)
	}
	alias, _ := ParseProjection("#n")
	if _, err := alias.Bind(Env{Names: map[string]string{"#n": "name"}}); err != nil {
		t.Errorf("reserved via alias: %v", err)
	}
}

func TestProjectionBindOverlap(t *testing.T) {
	for _, src := range []string{
		"top, top",       // duplicate
		"obj, obj.a",     // parent + child
		"arr[1], arr[1]", // duplicate index
		"#a, #b",         // aliases to the same name (bind-time only)
	} {
		p, err := ParseProjection(src)
		if err != nil {
			t.Fatalf("ParseProjection(%q): %v", src, err)
		}
		env := Env{Names: map[string]string{"#a": "same", "#b": "same"}}
		if _, err := p.Bind(env); !errors.Is(err, ErrSemantic) {
			t.Errorf("Bind(%q): err = %v, want ErrSemantic (overlap)", src, err)
		}
	}
	// Non-overlapping siblings are fine.
	p, _ := ParseProjection("obj.a, obj.b, arr[0], arr[1]")
	if _, err := p.Bind(Env{}); err != nil {
		t.Errorf("siblings: %v", err)
	}
}
