package expr

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// shape renders a parsed condition as a compact prefix string so precedence and
// associativity can be asserted without exporting the AST.
func shape(n condNode) string {
	switch t := n.(type) {
	case *orNode:
		return "OR(" + shape(t.left) + "," + shape(t.right) + ")"
	case *andNode:
		return "AND(" + shape(t.left) + "," + shape(t.right) + ")"
	case *notNode:
		return "NOT(" + shape(t.inner) + ")"
	case *cmpNode:
		return "CMP" + opNames[t.op] + "(" + operandShape(t.left) + "," + operandShape(t.right) + ")"
	case *betweenNode:
		return "BETWEEN(" + operandShape(t.operand) + "," + operandShape(t.lo) + "," + operandShape(t.hi) + ")"
	case *inNode:
		s := "IN(" + operandShape(t.operand)
		for _, o := range t.set {
			s += "," + operandShape(o)
		}
		return s + ")"
	case *funcNode:
		s := "FN" + fnNames[t.name] + "(" + operandShape(t.path)
		if t.arg != nil {
			s += "," + operandShape(t.arg)
		}
		return s + ")"
	}
	return "?"
}

func operandShape(o operandNode) string {
	switch t := o.(type) {
	case *pathOperand:
		s := ""
		for i, seg := range t.segs {
			if seg.isIndex {
				s += "[" + itoa(seg.index) + "]"
				continue
			}
			if i > 0 {
				s += "."
			}
			if seg.nameRef != "" {
				s += seg.nameRef
			} else {
				s += seg.name
			}
		}
		return s
	case *valueOperand:
		return t.ref
	case *sizeOperand:
		return "size(" + operandShape(t.path) + ")"
	}
	return "?"
}

var opNames = map[cmpOp]string{opEq: "=", opNe: "<>", opLt: "<", opLe: "<=", opGt: ">", opGe: ">="}
var fnNames = map[funcKind]string{
	fnAttributeExists:    "_exists",
	fnAttributeNotExists: "_not_exists",
	fnAttributeType:      "_type",
	fnContains:           "_contains",
	fnBeginsWith:         "_begins_with",
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

func TestParseConditionShape(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"simple equality", "a = :v", "CMP=(a,:v)"},
		{"and binds tighter than or", "a = :x OR b = :y AND c = :z",
			"OR(CMP=(a,:x),AND(CMP=(b,:y),CMP=(c,:z)))"},
		{"parens override", "(a = :x OR b = :y) AND c = :z",
			"AND(OR(CMP=(a,:x),CMP=(b,:y)),CMP=(c,:z))"},
		{"not binds tighter than and", "NOT a = :x AND b = :y",
			"AND(NOT(CMP=(a,:x)),CMP=(b,:y))"},
		{"or is left associative", "a = :x OR b = :y OR c = :z",
			"OR(OR(CMP=(a,:x),CMP=(b,:y)),CMP=(c,:z))"},
		{"keywords are case insensitive", "a = :x and b = :y",
			"AND(CMP=(a,:x),CMP=(b,:y))"},
		{"between", "a BETWEEN :lo AND :hi", "BETWEEN(a,:lo,:hi)"},
		{"in", "a IN (:x, :y, :z)", "IN(a,:x,:y,:z)"},
		{"nested path with name refs", "#a.b[2].#c = :v", "CMP=(#a.b[2].#c,:v)"},
		{"size operand", "size(a.b) > :n", "CMP>(size(a.b),:n)"},
		{"attribute_exists", "attribute_exists(a)", "FN_exists(a)"},
		{"attribute_not_exists", "attribute_not_exists(#p)", "FN_not_exists(#p)"},
		{"attribute_type", "attribute_type(a, :t)", "FN_type(a,:t)"},
		{"contains", "contains(a, :v)", "FN_contains(a,:v)"},
		{"begins_with", "begins_with(a.b, :p)", "FN_begins_with(a.b,:p)"},
		{"all comparators parse", "a <> :v", "CMP<>(a,:v)"},
		{"le", "a <= :v", "CMP<=(a,:v)"},
		{"ge", "a >= :v", "CMP>=(a,:v)"},
		{"lt", "a < :v", "CMP<(a,:v)"},
		{"value on the left", ":v = a", "CMP=(:v,a)"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := ParseCondition(tc.src)
			if err != nil {
				t.Fatalf("ParseCondition(%q): %v", tc.src, err)
			}
			if got := shape(c.root); got != tc.want {
				t.Errorf("shape = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestParseConditionErrors(t *testing.T) {
	cases := []struct{ name, src string }{
		{"empty", ""},
		{"trailing operator", "a ="},
		{"missing operand", "= :v"},
		{"unclosed paren", "(a = :v"},
		{"unexpected trailing token", "a = :v b"},
		{"unknown function", "no_such_fn(a)"},
		{"between missing AND", "a BETWEEN :lo :hi"},
		{"empty IN list", "a IN ()"},
		{"size of a value", "size(:v) > :n"},
		{"function on a value operand", "attribute_exists(:v)"},
		{"index without close", "a[0 = :v"},
		{"non numeric index", "a[x] = :v"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseCondition(tc.src); !errors.Is(err, ErrSyntax) {
				t.Errorf("ParseCondition(%q) err = %v, want ErrSyntax", tc.src, err)
			}
		})
	}
}

func TestParseConditionRefs(t *testing.T) {
	c, err := ParseCondition("#a.#b = :x AND contains(#a, :y) AND size(c) > :x")
	if err != nil {
		t.Fatalf("ParseCondition: %v", err)
	}
	names, values := c.Refs()
	wantNames := []string{"#a", "#b"}
	wantValues := []string{":x", ":y"}
	if len(names) != len(wantNames) {
		t.Fatalf("names = %v, want %v", names, wantNames)
	}
	for i := range wantNames {
		if names[i] != wantNames[i] {
			t.Errorf("names = %v, want %v", names, wantNames)
			break
		}
	}
	if len(values) != len(wantValues) {
		t.Fatalf("values = %v, want %v", values, wantValues)
	}
	for i := range wantValues {
		if values[i] != wantValues[i] {
			t.Errorf("values = %v, want %v", values, wantValues)
			break
		}
	}
}

func TestParseUpdateClauses(t *testing.T) {
	u, err := ParseUpdate("SET a = :v, b.c[0] = :w REMOVE d, e[1] ADD f :n DELETE g :s")
	if err != nil {
		t.Fatalf("ParseUpdate: %v", err)
	}
	if len(u.sets) != 2 {
		t.Errorf("sets = %d, want 2", len(u.sets))
	}
	if len(u.removes) != 2 {
		t.Errorf("removes = %d, want 2", len(u.removes))
	}
	if len(u.adds) != 1 {
		t.Errorf("adds = %d, want 1", len(u.adds))
	}
	if len(u.deletes) != 1 {
		t.Errorf("deletes = %d, want 1", len(u.deletes))
	}
	// Clause keywords are case-insensitive and may appear in any order.
	if _, err := ParseUpdate("delete g :s add f :n remove d set a = :v"); err != nil {
		t.Errorf("lowercase, reordered clauses: %v", err)
	}
}

func TestParseUpdateSetValues(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string // %T of the single SET action's value node
	}{
		{"value substitution", "SET a = :v", "*expr.valueOperand"},
		{"path copy", "SET a = b", "*expr.pathOperand"},
		{"addition", "SET a = b + :v", "*expr.arithNode"},
		{"subtraction", "SET a = b - :v", "*expr.arithNode"},
		{"if_not_exists", "SET a = if_not_exists(b, :v)", "*expr.ifNotExistsNode"},
		{"list_append", "SET a = list_append(b, :v)", "*expr.listAppendNode"},
		{"IF_NOT_EXISTS is case-insensitive", "SET a = IF_NOT_EXISTS(b, :v)", "*expr.ifNotExistsNode"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u, err := ParseUpdate(tc.src)
			if err != nil {
				t.Fatalf("ParseUpdate(%q): %v", tc.src, err)
			}
			if len(u.sets) != 1 {
				t.Fatalf("sets = %d, want 1", len(u.sets))
			}
			if got := fmt.Sprintf("%T", u.sets[0].value); got != tc.want {
				t.Errorf("value node = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestParseUpdateRefs(t *testing.T) {
	u, err := ParseUpdate("SET #a = :v, #a.b = :v REMOVE #c ADD d :n")
	if err != nil {
		t.Fatalf("ParseUpdate: %v", err)
	}
	names, values := u.Refs()
	wantNames := []string{"#a", "#c"}
	wantValues := []string{":v", ":n"}
	if len(names) != len(wantNames) {
		t.Fatalf("names = %v, want %v", names, wantNames)
	}
	for i, n := range wantNames {
		if names[i] != n {
			t.Errorf("names[%d] = %q, want %q", i, names[i], n)
		}
	}
	if len(values) != len(wantValues) {
		t.Fatalf("values = %v, want %v", values, wantValues)
	}
	for i, v := range wantValues {
		if values[i] != v {
			t.Errorf("values[%d] = %q, want %q", i, values[i], v)
		}
	}
}

func TestParseUpdateErrors(t *testing.T) {
	cases := []struct {
		name    string
		src     string
		wantErr error
	}{
		{"empty", "", ErrSyntax},
		{"duplicate SET clause", "SET a = :v SET b = :w", ErrSyntax},
		{"duplicate REMOVE clause", "REMOVE a REMOVE b", ErrSyntax},
		{"unknown clause keyword", "PUT a = :v", ErrSyntax},
		{"missing equals", "SET a :v", ErrSyntax},
		{"missing value", "SET a =", ErrSyntax},
		{"trailing comma", "SET a = :v,", ErrSyntax},
		{"dangling arithmetic", "SET a = :v +", ErrSyntax},
		{"chained arithmetic", "SET a = :v + :w - :x", ErrSyntax},
		{"unknown SET function", "SET a = frobnicate(b, :v)", ErrSyntax},
		{"size is condition-only", "SET a = size(b)", ErrSyntax},
		{"ADD without an operand", "ADD a", ErrSyntax},
		{"ADD with a path operand", "ADD a b", ErrSyntax},
		{"DELETE with a path operand", "DELETE a b", ErrSyntax},
		{"ADD on a nested path", "ADD a.b :v", ErrSemantic},
		{"ADD on a list index", "ADD a[0] :v", ErrSemantic},
		{"DELETE on a nested path", "DELETE a.b :v", ErrSemantic},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseUpdate(tc.src)
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("ParseUpdate(%q) err = %v, want %v", tc.src, err, tc.wantErr)
			}
		})
	}
}

// Nesting recurses parsePrimary -> parseOr -> parseAnd -> parseNot ->
// parsePrimary, once per level. Without a cap, a deep enough expression
// overflows the goroutine stack — a fatal error that no recover can catch and
// that takes the whole process down. The cap turns that into a syntax error.
func TestParseConditionRejectsPathologicalNesting(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{
			// Structurally meaningful nesting: one operator per level, so this
			// is not merely redundant parentheses.
			name: "nested parens",
			src:  strings.Repeat("a = :v OR (", 100000) + "a = :v" + strings.Repeat(")", 100000),
		},
		{
			name: "chained NOT",
			src:  strings.Repeat("NOT ", 100000) + "a = :v",
		},
		{
			name: "bare parens",
			src:  strings.Repeat("(", 100000) + "a = :v" + strings.Repeat(")", 100000),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseCondition(tc.src)
			if !errors.Is(err, ErrSyntax) {
				t.Errorf("ParseCondition(%s, 100000 levels) err = %v, want ErrSyntax", tc.name, err)
			}
		})
	}
}

// The cap must not reject anything a caller could legitimately send. DynamoDB
// caps expressions at 300 operators and 4KB, so real expressions nest far
// shallower than maxParseDepth.
func TestParseConditionAcceptsModestNesting(t *testing.T) {
	const levels = 100
	src := strings.Repeat("a = :v OR (", levels) + "a = :v" + strings.Repeat(")", levels)
	if _, err := ParseCondition(src); err != nil {
		t.Errorf("ParseCondition(%d levels) = %v, want success", levels, err)
	}
	if _, err := ParseCondition(strings.Repeat("NOT ", levels) + "a = :v"); err != nil {
		t.Errorf("ParseCondition(%d NOTs) = %v, want success", levels, err)
	}
}
