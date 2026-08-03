package expr

import (
	"errors"
	"strings"
	"testing"

	"github.com/quells-bot/ddb-sqlite-core/attrval"
)

func TestNameTokenLimit(t *testing.T) {
	// 255-byte #name key: accepted.
	short := "#" + strings.Repeat("a", 254)
	c, _ := ParseCondition(short + " = :v")
	if _, err := c.Bind(Env{
		Names:  map[string]string{short: "attr"},
		Values: map[string]attrval.Value{":v": attrval.NewString("x")},
	}); err != nil {
		t.Fatalf("255-byte name key: err = %v", err)
	}

	// 256-byte #name key: rejected.
	long := "#" + strings.Repeat("a", 255)
	c, _ = ParseCondition(long + " = :v")
	_, err := c.Bind(Env{
		Names:  map[string]string{long: "attr"},
		Values: map[string]attrval.Value{":v": attrval.NewString("x")},
	})
	if !errors.Is(err, ErrLimit) {
		t.Errorf("256-byte name key: err = %v, want ErrLimit", err)
	}
}

func TestSubstitutionValueLimit(t *testing.T) {
	// A single ~2MB string value: serialized map exceeds 1MB -> rejected.
	big := attrval.NewString(strings.Repeat("x", 2<<20))
	c, _ := ParseCondition("a = :v")
	_, err := c.Bind(Env{
		Values: map[string]attrval.Value{":v": big},
	})
	if !errors.Is(err, ErrLimit) {
		t.Errorf("2MB value: err = %v, want ErrLimit", err)
	}

	// Normal-sized value: accepted.
	c, _ = ParseCondition("a = :v")
	if _, err := c.Bind(Env{
		Values: map[string]attrval.Value{":v": attrval.NewString("ok")},
	}); err != nil {
		t.Fatalf("normal value: err = %v", err)
	}
}

func TestExprStringLimit(t *testing.T) {
	// 4096 bytes: accepted (parses or fails on grammar, not on size).
	// Fixture base "a = a " + "AND b = b" is 15 bytes; 15 + 4081 = 4096.
	_, err := ParseCondition("a = a " + strings.Repeat(" ", 4081) + "AND b = b")
	if err != nil && errors.Is(err, ErrLimit) {
		t.Errorf("4096-byte expression rejected with ErrLimit: %v", err)
	}

	// 4097 bytes: rejected with ErrLimit. Fixture base is 15 bytes;
	// 15 + 4082 = 4097.
	longExpr := "a = a " + strings.Repeat(" ", 4082) + "AND b = b"
	if len(longExpr) != 4097 {
		t.Fatalf("test fixture length = %d, want 4097", len(longExpr))
	}
	_, err = ParseCondition(longExpr)
	if !errors.Is(err, ErrLimit) {
		t.Errorf("4097-byte expression: err = %v, want ErrLimit", err)
	}
}

func TestOperatorCountLimit(t *testing.T) {
	// 299 operators: 150 "a=a" joined by " OR " = 150 comparators + 149 OR.
	ok := strings.Join(repeatStr("a=a", 150), " OR ")
	if n := countCondOperators(mustCond(t, ok).root); n > maxOperators {
		t.Fatalf("299-op expression has %d operators", n)
	}
	if _, err := ParseCondition(ok); err != nil {
		t.Fatalf("299 operators: err = %v", err)
	}

	// 301 operators: 151 "a=a" joined by " OR " = 151 + 150.
	over := strings.Join(repeatStr("a=a", 151), " OR ")
	_, err := ParseCondition(over)
	if !errors.Is(err, ErrLimit) {
		t.Errorf("301 operators: err = %v, want ErrLimit", err)
	}
}

func TestOperatorCountInUpdate(t *testing.T) {
	// Each SET action "a = a + :v" has 1 operator (+). 300 actions = 300 ops.
	actions := make([]string, 300)
	for i := range actions {
		actions[i] = "a = a + :v"
	}
	ok := "SET " + strings.Join(actions, ", ")
	if _, err := ParseUpdate(ok); err != nil {
		t.Fatalf("300 update operators: err = %v", err)
	}

	actions = append(actions, "b = b + :v")
	over := "SET " + strings.Join(actions, ", ")
	_, err := ParseUpdate(over)
	if !errors.Is(err, ErrLimit) {
		t.Errorf("301 update operators: err = %v, want ErrLimit", err)
	}
}

func TestInOperandLimit(t *testing.T) {
	// 100 operands: accepted.
	ops := make([]string, 100)
	for i := range ops {
		ops[i] = ":v"
	}
	ok := "a IN (" + strings.Join(ops, ",") + ")"
	if _, err := ParseCondition(ok); err != nil {
		t.Fatalf("100 IN operands: err = %v", err)
	}

	// 101 operands: rejected.
	ops = append(ops, ":v")
	over := "a IN (" + strings.Join(ops, ",") + ")"
	_, err := ParseCondition(over)
	if !errors.Is(err, ErrLimit) {
		t.Errorf("101 IN operands: err = %v, want ErrLimit", err)
	}
}

func TestPathDepthLimit(t *testing.T) {
	// 32 segments: accepted.
	segs32 := strings.Join(repeatStr("a", 32), ".")
	_, err := ParseCondition("attribute_exists(" + segs32 + ")")
	if err != nil {
		t.Fatalf("32-segment path: err = %v", err)
	}
	c, _ := ParseCondition("attribute_exists(" + segs32 + ")")
	if _, err := c.Bind(Env{}); err != nil {
		t.Fatalf("32-segment path bind: err = %v", err)
	}

	// 33 segments: rejected at bind.
	segs33 := strings.Join(repeatStr("a", 33), ".")
	c, err = ParseCondition("attribute_exists(" + segs33 + ")")
	if err != nil {
		t.Fatalf("parse 33-segment path: err = %v", err)
	}
	_, err = c.Bind(Env{})
	if !errors.Is(err, ErrLimit) {
		t.Errorf("33-segment path bind: err = %v, want ErrLimit", err)
	}
}

func mustCond(t *testing.T, src string) *Condition {
	t.Helper()
	c, err := ParseCondition(src)
	if err != nil {
		t.Fatalf("ParseCondition(%q): %v", src, err)
	}
	return c
}

func repeatStr(s string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = s
	}
	return out
}
