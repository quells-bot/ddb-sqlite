package expr

import (
	"errors"
	"testing"

	"github.com/quells-bot/ddb-sqlite-core/attrval"
)

func mustParse(t *testing.T, src string) *Condition {
	t.Helper()
	c, err := ParseCondition(src)
	if err != nil {
		t.Fatalf("ParseCondition(%q): %v", src, err)
	}
	return c
}

func TestBindResolvesNamesAndValues(t *testing.T) {
	c := mustParse(t, "#n.b[1] = :v")
	env := Env{
		Names:  map[string]string{"#n": "Actual Name"},
		Values: map[string]attrval.Value{":v": attrval.NewString("x")},
	}
	b, err := c.Bind(env)
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	cmp, ok := b.root.(*cmpNode)
	if !ok {
		t.Fatalf("root = %T, want *cmpNode", b.root)
	}
	p := cmp.left.(*pathOperand).resolved
	want := attrval.Path{
		{Name: "Actual Name"},
		{Name: "b"},
		{IsIndex: true, Index: 1},
	}
	if len(p) != len(want) {
		t.Fatalf("resolved = %v, want %v", p, want)
	}
	for i := range want {
		if p[i] != want[i] {
			t.Errorf("segment %d = %+v, want %+v", i, p[i], want[i])
		}
	}
	if got := cmp.right.(*valueOperand).val; got.Str() != "x" {
		t.Errorf("bound value = %v, want x", got)
	}
}

func TestBindUndefined(t *testing.T) {
	cases := []struct {
		name string
		src  string
		env  Env
	}{
		{
			name: "undefined name",
			src:  "#n = :v",
			env:  Env{Values: map[string]attrval.Value{":v": attrval.NewString("x")}},
		},
		{
			name: "undefined value",
			src:  "a = :v",
			env:  Env{},
		},
		{
			name: "undefined name in a nested segment",
			src:  "a.#deep = :v",
			env:  Env{Values: map[string]attrval.Value{":v": attrval.NewString("x")}},
		},
		{
			name: "undefined value inside IN",
			src:  "a IN (:x, :y)",
			env:  Env{Values: map[string]attrval.Value{":x": attrval.NewString("x")}},
		},
		{
			name: "undefined value inside a function",
			src:  "contains(a, :v)",
			env:  Env{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := mustParse(t, tc.src).Bind(tc.env); !errors.Is(err, ErrUndefined) {
				t.Errorf("Bind err = %v, want ErrUndefined", err)
			}
		})
	}
}

func TestBindDoesNotMutateCondition(t *testing.T) {
	c := mustParse(t, "#n = :v")
	env1 := Env{
		Names:  map[string]string{"#n": "one"},
		Values: map[string]attrval.Value{":v": attrval.NewString("a")},
	}
	env2 := Env{
		Names:  map[string]string{"#n": "two"},
		Values: map[string]attrval.Value{":v": attrval.NewString("b")},
	}
	b1, err := c.Bind(env1)
	if err != nil {
		t.Fatalf("Bind 1: %v", err)
	}
	b2, err := c.Bind(env2)
	if err != nil {
		t.Fatalf("Bind 2: %v", err)
	}
	n1 := b1.root.(*cmpNode).left.(*pathOperand).resolved[0].Name
	n2 := b2.root.(*cmpNode).left.(*pathOperand).resolved[0].Name
	if n1 != "one" || n2 != "two" {
		t.Errorf("bindings interfered: %q, %q", n1, n2)
	}
}

func TestCheckUnused(t *testing.T) {
	cases := []struct {
		name    string
		env     Env
		names   []string
		values  []string
		wantErr bool
	}{
		{
			name:   "all used",
			env:    Env{Names: map[string]string{"#a": "A"}, Values: map[string]attrval.Value{":x": attrval.NewNull()}},
			names:  []string{"#a"},
			values: []string{":x"},
		},
		{
			name:    "unused name",
			env:     Env{Names: map[string]string{"#a": "A", "#b": "B"}},
			names:   []string{"#a"},
			wantErr: true,
		},
		{
			name:    "unused value",
			env:     Env{Values: map[string]attrval.Value{":x": attrval.NewNull(), ":y": attrval.NewNull()}},
			values:  []string{":x"},
			wantErr: true,
		},
		{
			name: "union across two expressions satisfies both maps",
			env: Env{
				Names:  map[string]string{"#a": "A", "#b": "B"},
				Values: map[string]attrval.Value{":x": attrval.NewNull(), ":y": attrval.NewNull()},
			},
			// #a/:x come from one expression, #b/:y from another.
			names:  []string{"#a", "#b"},
			values: []string{":x", ":y"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckUnused(tc.env, tc.names, tc.values)
			if tc.wantErr {
				if !errors.Is(err, ErrUnused) {
					t.Errorf("err = %v, want ErrUnused", err)
				}
				return
			}
			if err != nil {
				t.Errorf("err = %v, want nil", err)
			}
		})
	}
}

func TestBindReservedWords(t *testing.T) {
	cases := []struct {
		name    string
		src     string
		env     Env
		wantErr bool
	}{
		{name: "bare reserved word rejected", src: "null = :v", env: Env{Values: map[string]attrval.Value{":v": attrval.NewNull()}}, wantErr: true},
		{name: "reserved word is case-insensitive", src: "NULL = :v", env: Env{Values: map[string]attrval.Value{":v": attrval.NewNull()}}, wantErr: true},
		{name: "reserved word in a nested segment rejected", src: "m.inner = :v", env: Env{Values: map[string]attrval.Value{":v": attrval.NewString("x")}}, wantErr: true},
		{name: "reserved word inside a function rejected", src: "attribute_exists(null)", wantErr: true},
		{name: "bare non-reserved word is fine", src: "marker = :v", env: Env{Values: map[string]attrval.Value{":v": attrval.NewString("x")}}},
		{name: "#name alias may resolve to a reserved word", src: "#z = :v", env: Env{
			Names:  map[string]string{"#z": "null"},
			Values: map[string]attrval.Value{":v": attrval.NewNull()},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := mustParse(t, tc.src).Bind(tc.env)
			if tc.wantErr {
				if !errors.Is(err, ErrSemantic) {
					t.Errorf("Bind err = %v, want ErrSemantic", err)
				}
				return
			}
			if err != nil {
				t.Errorf("Bind err = %v, want nil", err)
			}
		})
	}
}

func TestValidateFilterKeys(t *testing.T) {
	env := Env{
		Names:  map[string]string{"#p": "pk"},
		Values: map[string]attrval.Value{":v": attrval.NewString("x")},
	}
	cases := []struct {
		name    string
		src     string
		wantErr bool
	}{
		{name: "non-key attribute is fine", src: "marker = :v"},
		{name: "literal key attribute rejected", src: "pk = :v", wantErr: true},
		{name: "key attribute via name ref rejected", src: "#p = :v", wantErr: true},
		{name: "key attribute inside a function rejected", src: "attribute_exists(pk)", wantErr: true},
		{name: "nested path under a non-key root is fine", src: "marker.pk = :v"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := mustParse(t, tc.src)
			// #p stays defined for every case even when unreferenced: Bind
			// only rejects UNDEFINED refs, and the unused check is
			// CheckUnused's job, not Bind's.
			b, err := c.Bind(env)
			if err != nil {
				t.Fatalf("Bind: %v", err)
			}
			err = b.ValidateFilterKeys([]string{"pk", "sk"})
			if tc.wantErr {
				if !errors.Is(err, ErrSemantic) {
					t.Errorf("err = %v, want ErrSemantic", err)
				}
				return
			}
			if err != nil {
				t.Errorf("err = %v, want nil", err)
			}
		})
	}
}

func mustParseUpdate(t *testing.T, src string) *Update {
	t.Helper()
	u, err := ParseUpdate(src)
	if err != nil {
		t.Fatalf("ParseUpdate(%q): %v", src, err)
	}
	return u
}

func TestUpdateBindResolves(t *testing.T) {
	u := mustParseUpdate(t, "SET #a = :v, x = if_not_exists(#a.b, :v) REMOVE #c ADD d :n DELETE e :s")
	env := Env{
		Names: map[string]string{"#a": "alpha", "#c": "gamma"},
		Values: map[string]attrval.Value{
			":v": attrval.NewString("val"),
			":n": mustNumber(t, "1"),
			":s": attrval.NewStringSet([]string{"q"}),
		},
	}
	b, err := u.Bind(env)
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if got := b.sets[0].path.resolved[0].Name; got != "alpha" {
		t.Errorf("SET target = %q, want alpha", got)
	}
	ine, ok := b.sets[1].value.(*ifNotExistsNode)
	if !ok {
		t.Fatalf("sets[1].value = %T, want *ifNotExistsNode", b.sets[1].value)
	}
	if got := ine.path.resolved[0].Name; got != "alpha" {
		t.Errorf("if_not_exists path = %q, want alpha", got)
	}
	if got := b.removes[0].resolved[0].Name; got != "gamma" {
		t.Errorf("REMOVE target = %q, want gamma", got)
	}
	if got := b.adds[0].value.val; got.Type() != "N" {
		t.Errorf("ADD value type = %s, want N", got.Type())
	}
	if got := b.deletes[0].value.val; got.Type() != "SS" {
		t.Errorf("DELETE value type = %s, want SS", got.Type())
	}
}

func TestUpdateBindUndefined(t *testing.T) {
	cases := []struct {
		name string
		src  string
		env  Env
	}{
		{"undefined name in a SET target", "SET #missing = :v", Env{Values: map[string]attrval.Value{":v": attrval.NewString("x")}}},
		{"undefined value in a SET action", "SET a = :missing", Env{}},
		{"undefined value in arithmetic", "SET a = a + :missing", Env{}},
		{"undefined name in REMOVE", "REMOVE #missing", Env{}},
		{"undefined value in ADD", "ADD a :missing", Env{}},
		{"undefined value in DELETE", "DELETE a :missing", Env{}},
		{"undefined value in list_append", "SET a = list_append(a, :missing)", Env{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := mustParseUpdate(t, tc.src)
			if _, err := u.Bind(tc.env); !errors.Is(err, ErrUndefined) {
				t.Errorf("Bind(%q) err = %v, want ErrUndefined", tc.src, err)
			}
		})
	}
}

func TestUpdateBindReservedWord(t *testing.T) {
	u := mustParseUpdate(t, "SET total = :v")
	env := Env{Values: map[string]attrval.Value{":v": attrval.NewString("x")}}
	if _, err := u.Bind(env); !errors.Is(err, ErrSemantic) {
		t.Errorf("Bind err = %v, want ErrSemantic for a bare reserved word", err)
	}
}

func TestUpdateOverlappingPaths(t *testing.T) {
	env := Env{
		Names:  map[string]string{"#a": "a"},
		Values: map[string]attrval.Value{":v": attrval.NewString("x"), ":w": attrval.NewString("y"), ":s": attrval.NewStringSet([]string{"q"})},
	}
	cases := []struct {
		name    string
		src     string
		overlap bool
	}{
		{"identical paths across clauses", "SET a = :v REMOVE a", true},
		{"prefix path", "SET a.b = :v, a = :w", true},
		{"prefix path reversed", "SET a = :v, a.b = :w", true},
		{"alias resolving to the same name", "SET #a = :v REMOVE a", true},
		{"list index prefix", "SET a[0] = :v REMOVE a", true},
		{"identical within one clause", "SET a = :v, a = :w", true},
		{"ADD and SET on the same attribute", "SET a = :v ADD a :s", true},
		{"sibling map entries", "SET a.b = :v, a.c = :w", false},
		{"different list indices", "SET a[0] = :v, a[1] = :w", false},
		{"different attributes", "SET a = :v REMOVE b", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := mustParseUpdate(t, tc.src)
			_, err := u.Bind(env)
			if tc.overlap {
				if !errors.Is(err, ErrSemantic) {
					t.Errorf("Bind(%q) err = %v, want ErrSemantic", tc.src, err)
				}
				return
			}
			if err != nil {
				t.Errorf("Bind(%q) err = %v, want nil", tc.src, err)
			}
		})
	}
}

func TestUpdateValidateKeyAttrs(t *testing.T) {
	env := Env{Values: map[string]attrval.Value{":v": attrval.NewString("x"), ":s": attrval.NewStringSet([]string{"q"})}}
	cases := []struct {
		name string
		src  string
		bad  bool
	}{
		{"SET on the partition key", "SET pk = :v", true},
		{"SET into the sort key document", "SET sk.x = :v", true},
		{"REMOVE the sort key", "REMOVE sk", true},
		{"ADD to the partition key", "ADD pk :s", true},
		{"non-key attribute", "SET marker = :v", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := mustParseUpdate(t, tc.src)
			b, err := u.Bind(env)
			if err != nil {
				t.Fatalf("Bind(%q): %v", tc.src, err)
			}
			err = b.ValidateKeyAttrs([]string{"pk", "sk"})
			if tc.bad {
				if !errors.Is(err, ErrSemantic) {
					t.Errorf("ValidateKeyAttrs(%q) err = %v, want ErrSemantic", tc.src, err)
				}
				return
			}
			if err != nil {
				t.Errorf("ValidateKeyAttrs(%q) err = %v, want nil", tc.src, err)
			}
		})
	}
}

// W8 #2: ordering comparators and BETWEEN reject non-scalar :value operands
// (BOOL/NULL/L/M/sets) at Bind time; S/N/B operands, equality, and IN accept
// any type. Measured against dynamodb-local 3.3.1.
func TestBindOrderingOperandType(t *testing.T) {
	boolV := attrval.NewBool(true)
	nullV := attrval.NewNull()
	listV := attrval.NewList([]attrval.Value{attrval.NewString("x")})
	ssV := attrval.NewStringSet([]string{"a"})
	strV := attrval.NewString("x")
	numV, err := attrval.NewNumberString("1")
	if err != nil {
		t.Fatalf("NewNumberString: %v", err)
	}
	binV := attrval.NewBinary([]byte{1})

	cases := []struct {
		name   string
		src    string
		values map[string]attrval.Value
		want   error
	}{
		{"less than bool operand rejected", "s < :v", map[string]attrval.Value{":v": boolV}, ErrSemantic},
		{"less-equal null operand rejected", "s <= :v", map[string]attrval.Value{":v": nullV}, ErrSemantic},
		{"greater than list operand rejected", "s > :v", map[string]attrval.Value{":v": listV}, ErrSemantic},
		{"greater-equal set operand rejected", "s >= :v", map[string]attrval.Value{":v": ssV}, ErrSemantic},
		{"between null bounds rejected", "n BETWEEN :lo AND :hi", map[string]attrval.Value{":lo": nullV, ":hi": nullV}, ErrSemantic},
		{"between bool bounds rejected", "n BETWEEN :lo AND :hi", map[string]attrval.Value{":lo": boolV, ":hi": boolV}, ErrSemantic},
		{"rejected inside AND", "attribute_exists(pk) AND s < :v", map[string]attrval.Value{":v": boolV}, ErrSemantic},
		{"string operand accepted", "s < :v", map[string]attrval.Value{":v": strV}, nil},
		{"number operand accepted", "n >= :v", map[string]attrval.Value{":v": numV}, nil},
		{"binary operand accepted", "b < :v", map[string]attrval.Value{":v": binV}, nil},
		{"between string bounds accepted", "s BETWEEN :lo AND :hi", map[string]attrval.Value{":lo": strV, ":hi": strV}, nil},
		{"equality bool operand accepted", "s = :v", map[string]attrval.Value{":v": boolV}, nil},
		{"inequality null operand accepted", "s <> :v", map[string]attrval.Value{":v": nullV}, nil},
		{"in null operand accepted", "s IN (:v)", map[string]attrval.Value{":v": nullV}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := mustParse(t, tc.src)
			_, err := c.Bind(Env{Values: tc.values})
			if !errors.Is(err, tc.want) {
				t.Errorf("Bind err = %v, want %v", err, tc.want)
			}
		})
	}
}
