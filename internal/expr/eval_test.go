package expr

import (
	"errors"
	"testing"

	"github.com/quells-bot/ddb-sqlite-core/attrval"
)

func mustNumber(t *testing.T, s string) attrval.Value {
	t.Helper()
	v, err := attrval.NewNumberString(s)
	if err != nil {
		t.Fatalf("NewNumberString(%q): %v", s, err)
	}
	return v
}

// fixtureItem exercises all ten attrval tags plus a nested document.
func fixtureItem(t *testing.T) map[string]attrval.Value {
	t.Helper()
	return map[string]attrval.Value{
		"s":    attrval.NewString("hello"),
		"n":    mustNumber(t, "42"),
		"b":    attrval.NewBinary([]byte{1, 2, 3}),
		"bool": attrval.NewBool(true),
		"null": attrval.NewNull(),
		"l":    attrval.NewList([]attrval.Value{attrval.NewString("x"), mustNumber(t, "7")}),
		"m": attrval.NewMap(map[string]attrval.Value{
			"inner": attrval.NewString("deep"),
			"list":  attrval.NewList([]attrval.Value{attrval.NewString("q")}),
		}),
		"ss": attrval.NewStringSet([]string{"a", "b"}),
		"ns": func() attrval.Value {
			v, err := attrval.NewNumberSetFromStrings([]string{"1", "2"})
			if err != nil {
				t.Fatalf("NewNumberSetFromStrings: %v", err)
			}
			return v
		}(),
		"bs": attrval.NewBinarySet([][]byte{{9}, {8}}),
	}
}

func evalWith(t *testing.T, src string, item map[string]attrval.Value, values map[string]attrval.Value) (bool, error) {
	t.Helper()
	return evalWithEnv(t, src, item, nil, values)
}

func evalWithEnv(t *testing.T, src string, item map[string]attrval.Value, names map[string]string, values map[string]attrval.Value) (bool, error) {
	t.Helper()
	c, err := ParseCondition(src)
	if err != nil {
		return false, err
	}
	b, err := c.Bind(Env{Names: names, Values: values})
	if err != nil {
		return false, err
	}
	return b.Eval(item)
}

func TestEvalComparisons(t *testing.T) {
	item := fixtureItem(t)
	vals := map[string]attrval.Value{
		":s":      attrval.NewString("hello"),
		":s2":     attrval.NewString("world"),
		":n":      mustNumber(t, "42"),
		":n0":     mustNumber(t, "42.0"),
		":nsmall": mustNumber(t, "7"),
		":nbig":   mustNumber(t, "100"),
		":b":      attrval.NewBinary([]byte{1, 2, 3}),
		":bool":   attrval.NewBool(true),
		":null":   attrval.NewNull(),
	}

	cases := []struct {
		name  string
		src   string
		names map[string]string
		want  bool
	}{
		{"string equality", "s = :s", nil, true},
		{"string inequality", "s <> :s2", nil, true},
		{"number equality ignores trailing zero", "n = :n0", nil, true},
		{"number less than", "n < :nbig", nil, true},
		{"number greater than", "n > :nsmall", nil, true},
		{"number le boundary", "n <= :n", nil, true},
		{"number ge boundary", "n >= :n", nil, true},
		{"binary equality", "b = :b", nil, true},
		{"bool equality", "bool = :bool", nil, true},
		{"null equality", "#n = :null", map[string]string{"#n": "null"}, true},
		{"string ordering", "s < :s2", nil, true},

		{"cross-type equality is false", "s = :n", nil, false},
		{"cross-type inequality is true", "s <> :n", nil, true},
		{"ordered comparison across types is false", "s < :n", nil, false},
		{"ordered comparison on BOOL is false", "bool < :bool", nil, false},
		{"ordered comparison on a list is false", "l < :s", nil, false},
		{"ordered comparison on a set is false", "ss > :s", nil, false},

		{"missing attribute equality is false", "nope = :s", nil, false},
		{"missing attribute inequality is true", "nope <> :s", nil, true},
		{"missing attribute ordering is false", "nope < :s", nil, false},
		{"present NULL is not missing", "#n = :null", map[string]string{"#n": "null"}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := evalWithEnv(t, tc.src, item, tc.names, vals)
			if err != nil {
				t.Fatalf("eval(%q): %v", tc.src, err)
			}
			if got != tc.want {
				t.Errorf("eval(%q) = %v, want %v", tc.src, got, tc.want)
			}
		})
	}
}

func TestEvalNestedPaths(t *testing.T) {
	item := fixtureItem(t)
	vals := map[string]attrval.Value{
		":deep": attrval.NewString("deep"),
		":q":    attrval.NewString("q"),
		":x":    attrval.NewString("x"),
	}
	cases := []struct {
		name  string
		src   string
		names map[string]string
		want  bool
	}{
		{"map attribute", "m.#i = :deep", map[string]string{"#i": "inner"}, true},
		{"list index", "l[0] = :x", nil, true},
		{"map then list index", "m.#lst[0] = :q", map[string]string{"#lst": "list"}, true},
		{"out of range index is missing", "l[9] = :x", nil, false},
		{"index into a non-list is missing", "s[0] = :x", nil, false},
		{"name under a non-map is missing", "s.#i = :deep", map[string]string{"#i": "inner"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := evalWithEnv(t, tc.src, item, tc.names, vals)
			if err != nil {
				t.Fatalf("eval(%q): %v", tc.src, err)
			}
			if got != tc.want {
				t.Errorf("eval(%q) = %v, want %v", tc.src, got, tc.want)
			}
		})
	}
}

func TestEvalLogical(t *testing.T) {
	item := fixtureItem(t)
	vals := map[string]attrval.Value{":s": attrval.NewString("hello"), ":no": attrval.NewString("nope")}
	cases := []struct {
		name string
		src  string
		want bool
	}{
		{"and both true", "s = :s AND attribute_exists(n)", true},
		{"and one false", "s = :no AND attribute_exists(n)", false},
		{"or one true", "s = :no OR attribute_exists(n)", true},
		{"or both false", "s = :no OR attribute_exists(nope)", false},
		{"not", "NOT s = :no", true},
		{"precedence: or of and", "s = :no AND s = :s OR attribute_exists(n)", true},
		{"parens change grouping", "s = :no AND (s = :s OR attribute_exists(n))", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := evalWith(t, tc.src, item, vals)
			if err != nil {
				t.Fatalf("eval(%q): %v", tc.src, err)
			}
			if got != tc.want {
				t.Errorf("eval(%q) = %v, want %v", tc.src, got, tc.want)
			}
		})
	}
}

func TestEvalBetweenAndIn(t *testing.T) {
	item := fixtureItem(t)
	vals := map[string]attrval.Value{
		":lo": mustNumber(t, "10"),
		":hi": mustNumber(t, "100"),
		":eq": mustNumber(t, "42"),
		":s":  attrval.NewString("hello"),
		":s2": attrval.NewString("world"),
		":n":  mustNumber(t, "42"),
	}
	t.Run("between inside range", func(t *testing.T) {
		got, err := evalWith(t, "n BETWEEN :lo AND :hi", item, vals)
		if err != nil || !got {
			t.Errorf("got %v, %v; want true, nil", got, err)
		}
	})
	t.Run("between inclusive at bounds", func(t *testing.T) {
		got, err := evalWith(t, "n BETWEEN :eq AND :hi", item, vals)
		if err != nil || !got {
			t.Errorf("got %v, %v; want true, nil", got, err)
		}
	})
	t.Run("between missing attribute is false", func(t *testing.T) {
		got, err := evalWith(t, "nope BETWEEN :lo AND :hi", item, vals)
		if err != nil || got {
			t.Errorf("got %v, %v; want false, nil", got, err)
		}
	})
	t.Run("between reversed bounds is an error", func(t *testing.T) {
		_, err := evalWith(t, "n BETWEEN :hi AND :lo", item, vals)
		if !errors.Is(err, ErrSemantic) {
			t.Errorf("err = %v, want ErrSemantic", err)
		}
	})
	t.Run("in matches", func(t *testing.T) {
		got, err := evalWith(t, "s IN (:s2, :s)", item, vals)
		if err != nil || !got {
			t.Errorf("got %v, %v; want true, nil", got, err)
		}
	})
	t.Run("in does not match", func(t *testing.T) {
		got, err := evalWith(t, "s IN (:s2, :n)", item, vals)
		if err != nil || got {
			t.Errorf("got %v, %v; want false, nil", got, err)
		}
	})
	t.Run("in with a missing attribute is false", func(t *testing.T) {
		got, err := evalWith(t, "nope IN (:s, :s2)", item, vals)
		if err != nil || got {
			t.Errorf("got %v, %v; want false, nil", got, err)
		}
	})
}

func TestEvalFunctions(t *testing.T) {
	item := fixtureItem(t)
	vals := map[string]attrval.Value{
		":sub":     attrval.NewString("ell"),
		":pre":     attrval.NewString("he"),
		":nomatch": attrval.NewString("zz"),
		":a":       attrval.NewString("a"),
		":one":     mustNumber(t, "1"),
		":nine":    attrval.NewBinary([]byte{9}),
		":bpre":    attrval.NewBinary([]byte{1, 2}),
		":bmid":    attrval.NewBinary([]byte{2, 3}),
		":x":       attrval.NewString("x"),
		":tS":      attrval.NewString("S"),
		":tN":      attrval.NewString("N"),
		":tNULL":   attrval.NewString("NULL"),
		":tSS":     attrval.NewString("SS"),
		":tBad":    attrval.NewString("NOPE"),
		":tNum":    mustNumber(t, "1"),
		":size5":   mustNumber(t, "5"),
		":size2":   mustNumber(t, "2"),
		":size3":   mustNumber(t, "3"),
	}

	cases := []struct {
		name    string
		src     string
		names   map[string]string
		want    bool
		wantErr bool
	}{
		{name: "attribute_exists present", src: "attribute_exists(s)", want: true},
		{name: "attribute_exists on a present NULL", src: "attribute_exists(#n)", names: map[string]string{"#n": "null"}, want: true},
		{name: "attribute_exists missing", src: "attribute_exists(nope)", want: false},
		{name: "attribute_not_exists missing", src: "attribute_not_exists(nope)", want: true},
		{name: "attribute_not_exists on a present NULL", src: "attribute_not_exists(#n)", names: map[string]string{"#n": "null"}, want: false},
		{name: "attribute_exists nested", src: "attribute_exists(m.#i)", names: map[string]string{"#i": "inner"}, want: true},

		{name: "attribute_type match", src: "attribute_type(s, :tS)", want: true},
		{name: "attribute_type mismatch", src: "attribute_type(s, :tN)", want: false},
		{name: "attribute_type NULL", src: "attribute_type(#n, :tNULL)", names: map[string]string{"#n": "null"}, want: true},
		{name: "attribute_type set", src: "attribute_type(ss, :tSS)", want: true},
		{name: "attribute_type missing attribute", src: "attribute_type(nope, :tS)", want: false},
		{name: "attribute_type bad code", src: "attribute_type(s, :tBad)", wantErr: true},
		{name: "attribute_type non-string code", src: "attribute_type(s, :tNum)", wantErr: true},
		{name: "attribute_type path operand is a validation error", src: "attribute_type(s, s)", wantErr: true},

		{name: "contains substring", src: "contains(s, :sub)", want: true},
		{name: "contains substring absent", src: "contains(s, :nomatch)", want: false},
		{name: "contains string set member", src: "contains(ss, :a)", want: true},
		{name: "contains number set member", src: "contains(ns, :one)", want: true},
		{name: "contains binary set member", src: "contains(bs, :nine)", want: true},
		{name: "contains binary subsequence", src: "contains(b, :bmid)", want: true},
		{name: "contains list element", src: "contains(l, :x)", want: true},
		{name: "contains type mismatch is false", src: "contains(s, :one)", want: false},
		{name: "contains on a missing attribute is false", src: "contains(nope, :sub)", want: false},

		{name: "begins_with string", src: "begins_with(s, :pre)", want: true},
		{name: "begins_with string absent", src: "begins_with(s, :nomatch)", want: false},
		{name: "begins_with binary", src: "begins_with(b, :bpre)", want: true},
		{name: "begins_with type mismatch is false", src: "begins_with(s, :one)", want: false},
		{name: "begins_with on a missing attribute is false", src: "begins_with(nope, :pre)", want: false},

		{name: "size of a string", src: "size(s) = :size5", want: true},
		{name: "size of binary", src: "size(b) = :size3", want: true},
		{name: "size of a list", src: "size(l) = :size2", want: true},
		{name: "size of a map", src: "size(m) = :size2", want: true},
		{name: "size of a string set", src: "size(ss) = :size2", want: true},
		{name: "size of a missing attribute is false", src: "size(nope) = :size2", want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := evalWithEnv(t, tc.src, item, tc.names, vals)
			if tc.wantErr {
				if !errors.Is(err, ErrSemantic) {
					t.Errorf("err = %v, want ErrSemantic", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("eval(%q): %v", tc.src, err)
			}
			if got != tc.want {
				t.Errorf("eval(%q) = %v, want %v", tc.src, got, tc.want)
			}
		})
	}
}

func TestEvalNilItem(t *testing.T) {
	vals := map[string]attrval.Value{":v": attrval.NewString("x")}
	got, err := evalWith(t, "attribute_not_exists(pk)", nil, vals)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if !got {
		t.Error("attribute_not_exists(pk) on a nil item = false, want true")
	}
	got, err = evalWith(t, "pk = :v", nil, vals)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if got {
		t.Error("pk = :v on a nil item = true, want false")
	}
}
