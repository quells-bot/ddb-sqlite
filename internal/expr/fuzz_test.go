package expr

import (
	"strings"
	"testing"

	"github.com/quells-bot/ddb-sqlite-core/attrval"
)

// FuzzParseCondition asserts the parser never panics: arbitrary input must
// yield either an error or an AST.
func FuzzParseCondition(f *testing.F) {
	seeds := []string{
		"a = :v",
		"#n.b[2] <> :v",
		"attribute_exists(a) AND NOT b = :v",
		"a BETWEEN :lo AND :hi",
		"a IN (:x, :y)",
		"size(a) > :n",
		"contains(a, :v) OR begins_with(b, :p)",
		"attribute_type(a, :t)",
		"((a = :v))",
		"",
		"(",
		"a = ",
		"AND",
		"a[",
		"#",
		":",
		"a..b = :v",
	}
	// Boundary-biased seeds (M6c §9): at and just over the W2 limits.
	// Limits must produce errors, never panics.
	seeds = append(seeds,
		// 4KB expression-string limit: 4096 bytes accepted, 4097 ErrLimit.
		"a = a "+strings.Repeat(" ", 4081)+"AND b = b",
		"a = a "+strings.Repeat(" ", 4082)+"AND b = b",
		// Operator-count limit: 299 operators accepted, 301 ErrLimit.
		strings.Join(repeatStr("a=a", 150), " OR "),
		strings.Join(repeatStr("a=a", 151), " OR "),
		// Path depth at/over the 32-segment bind limit (parse accepts both).
		"a"+strings.Repeat(".b", 31)+" = :v",
		"a"+strings.Repeat(".b", 32)+" = :v",
		// IN operand limit: 100 accepted, 101 ErrLimit.
		"a IN ("+strings.Join(repeatStr(":v", 100), ", ")+")",
		"a IN ("+strings.Join(repeatStr(":v", 101), ", ")+")",
	)
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, src string) {
		c, err := ParseCondition(src)
		if err != nil {
			return
		}
		if c == nil {
			t.Fatalf("ParseCondition(%q) returned nil with no error", src)
		}
		names, values := c.Refs()
		for _, n := range names {
			if len(n) < 2 || n[0] != '#' {
				t.Fatalf("bad name ref %q from %q", n, src)
			}
		}
		for _, v := range values {
			if len(v) < 2 || v[0] != ':' {
				t.Fatalf("bad value ref %q from %q", v, src)
			}
		}
	})
}

// FuzzBindEval asserts that parse -> bind -> eval never panics. Every ref the
// parser reports is supplied, so binding succeeds whenever parsing did.
func FuzzBindEval(f *testing.F) {
	seeds := []string{
		"a = :v",
		"attribute_not_exists(a)",
		"size(a) = :v",
		"a IN (:v)",
		"a BETWEEN :v AND :v",
		"contains(a, :v)",
		"attribute_type(a, :v)",
		"NOT a <> :v",
	}
	for _, s := range seeds {
		f.Add(s, "hello")
	}
	f.Fuzz(func(t *testing.T, src, sval string) {
		c, err := ParseCondition(src)
		if err != nil {
			return
		}
		names, values := c.Refs()
		env := Env{Names: map[string]string{}, Values: map[string]attrval.Value{}}
		for _, n := range names {
			env.Names[n] = n[1:]
		}
		for _, v := range values {
			env.Values[v] = attrval.NewString(sval)
		}
		b, err := c.Bind(env)
		if err != nil {
			// Bind errors are legitimate: since Task 10, a bare reserved-word
			// path (e.g. "select = :v") fails with ErrSemantic even when every
			// ref is supplied. A panic is what we fuzz for; an error is not.
			return
		}
		item := map[string]attrval.Value{
			"a":      attrval.NewString(sval),
			"nested": attrval.NewMap(map[string]attrval.Value{"x": attrval.NewNull()}),
			"list":   attrval.NewList([]attrval.Value{attrval.NewString(sval)}),
		}
		// Errors are legitimate (reversed BETWEEN, bad attribute_type code);
		// a panic is not.
		_, _ = b.Eval(item)
		_, _ = b.Eval(nil)
	})
}

// FuzzParseUpdate asserts the update parser never panics: arbitrary input must
// yield either an error or an AST whose refs are well-formed.
func FuzzParseUpdate(f *testing.F) {
	seeds := []string{
		"SET a = :v",
		"SET a = b + :v, c = list_append(c, :v)",
		"SET a = if_not_exists(b, :v)",
		"REMOVE a, b[0]",
		"ADD a :v",
		"DELETE a :v",
		"SET a = :v REMOVE b ADD c :v DELETE d :v",
		"set a = :v remove b",
		"SET #n.x[1] = :v",
		"",
		"SET",
		"SET a",
		"SET a =",
		"SET a = :v,",
		"ADD a.b :v",
		"ADD a b",
		"SET a = size(b)",
		"SET a = :v SET b = :v",
		"NOPE a = :v",
		"SET a = :v + :v - :v",
	}
	// Boundary-biased seeds (M6c §9): at and just over the W2 limits.
	seeds = append(seeds,
		// 4KB expression-string limit: 4096 bytes accepted, 4097 ErrLimit.
		"SET a = :v"+strings.Repeat(" ", 4086),
		"SET a = :v"+strings.Repeat(" ", 4087),
		// Operator-count limit: each "a = a + :v" SET action is 1 operator
		// (the arithNode; SET '=' is not counted): 300 accepted, 301 ErrLimit.
		"SET "+strings.Join(repeatStr("a = a + :v", 300), ", "),
		"SET "+strings.Join(repeatStr("a = a + :v", 301), ", "),
		// Path depth at/over the 32-segment bind limit (parse accepts both).
		"SET a"+strings.Repeat(".b", 31)+" = :v",
		"SET a"+strings.Repeat(".b", 32)+" = :v",
	)
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, src string) {
		u, err := ParseUpdate(src)
		if err != nil {
			return
		}
		if u == nil {
			t.Fatalf("ParseUpdate(%q) returned nil with no error", src)
		}
		names, values := u.Refs()
		for _, n := range names {
			if len(n) < 2 || n[0] != '#' {
				t.Fatalf("bad name ref %q from %q", n, src)
			}
		}
		for _, v := range values {
			if len(v) < 2 || v[0] != ':' {
				t.Fatalf("bad value ref %q from %q", v, src)
			}
		}
	})
}

// FuzzBindApply asserts that parse -> bind -> apply never panics. Every ref the
// parser reports is supplied, so binding fails only on a legitimate semantic
// rule (reserved word, overlapping paths). Apply errors are legitimate too — a
// panic is what this fuzzes for.
func FuzzBindApply(f *testing.F) {
	seeds := []string{
		"SET a = :v",
		"SET a = a + :v",
		"SET a = list_append(l, :v)",
		"SET a = if_not_exists(nope, :v)",
		"REMOVE l[0]",
		"ADD a :v",
		"DELETE ss :v",
		"SET nested.x = :v",
	}
	for _, s := range seeds {
		f.Add(s, "hello")
	}
	f.Fuzz(func(t *testing.T, src, sval string) {
		u, err := ParseUpdate(src)
		if err != nil {
			return
		}
		names, values := u.Refs()
		env := Env{Names: map[string]string{}, Values: map[string]attrval.Value{}}
		for _, n := range names {
			env.Names[n] = n[1:]
		}
		for _, v := range values {
			env.Values[v] = attrval.NewString(sval)
		}
		b, err := u.Bind(env)
		if err != nil {
			return
		}
		item := map[string]attrval.Value{
			"a":      attrval.NewString(sval),
			"l":      attrval.NewList([]attrval.Value{attrval.NewString(sval)}),
			"ss":     attrval.NewStringSet([]string{sval}),
			"nested": attrval.NewMap(map[string]attrval.Value{"x": attrval.NewNull()}),
		}
		_, _, _ = b.Apply(item)
		_, _, _ = b.Apply(nil)
	})
}
