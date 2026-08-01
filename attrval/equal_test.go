package attrval

import (
	"testing"

	"github.com/quells-bot/ddb-sqlite/internal/num"
)

func TestEqual(t *testing.T) {
	cases := []struct {
		name string
		a, b Value
		want bool
	}{
		{"null==null", NewNull(), NewNull(), true},
		{"null!=string", NewNull(), NewString(""), false},
		{"string eq", NewString("a"), NewString("a"), true},
		{"string neq", NewString("a"), NewString("b"), false},
		{"number numeric eq", NewNumber(mustDec("1")), NewNumber(mustDec("1.0")), true},
		{"number neq", NewNumber(mustDec("1")), NewNumber(mustDec("2")), false},
		{"binary eq", NewBinary([]byte{1}), NewBinary([]byte{1}), true},
		{"binary neq", NewBinary([]byte{1}), NewBinary([]byte{2}), false},
		{"bool eq", NewBool(true), NewBool(true), true},
		{"bool neq", NewBool(true), NewBool(false), false},
		{"list eq", NewList([]Value{NewString("a")}), NewList([]Value{NewString("a")}), true},
		{"list neq content", NewList([]Value{NewString("a")}), NewList([]Value{NewString("b")}), false},
		{"list neq len", NewList([]Value{NewString("a")}), NewList([]Value{NewString("a"), NewNull()}), false},
		{"map eq", NewMap(map[string]Value{"k": NewString("v")}), NewMap(map[string]Value{"k": NewString("v")}), true},
		{"map neq value", NewMap(map[string]Value{"k": NewString("v")}), NewMap(map[string]Value{"k": NewString("x")}), false},
		{"map neq key", NewMap(map[string]Value{"k": NewString("v")}), NewMap(map[string]Value{"j": NewString("v")}), false},
		{"ss order-independent", NewStringSet([]string{"a", "b"}), NewStringSet([]string{"b", "a"}), true},
		{"ss neq", NewStringSet([]string{"a"}), NewStringSet([]string{"a", "b"}), false},
		{"empty ss eq", NewStringSet(nil), NewStringSet(nil), true},
		{"empty ss != non-empty", NewStringSet(nil), NewStringSet([]string{"a"}), false},
		{"ns order-independent + numeric", NewNumberSet([]num.Decimal{mustDec("1"), mustDec("2")}), NewNumberSet([]num.Decimal{mustDec("2"), mustDec("1.0")}), true},
		{"bs order-independent", NewBinarySet([][]byte{{1}, {2}}), NewBinarySet([][]byte{{2}, {1}}), true},
		{"bs neq", NewBinarySet([][]byte{{1}}), NewBinarySet([][]byte{{1}, {2}}), false},
	}
	for _, c := range cases {
		if got := c.a.Equal(c.b); got != c.want {
			t.Errorf("Equal(%s) = %v, want %v", c.name, got, c.want)
		}
		// Equality must be reflexive and symmetric.
		if !c.a.Equal(c.a) {
			t.Errorf("Equal(%s) not reflexive", c.name)
		}
		if c.a.Equal(c.b) != c.b.Equal(c.a) {
			t.Errorf("Equal(%s) not symmetric", c.name)
		}
	}
}
