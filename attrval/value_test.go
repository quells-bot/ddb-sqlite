package attrval

import (
	"errors"
	"strings"
	"testing"

	"github.com/quells-bot/ddb-sqlite/internal/num"
)

func TestScalars(t *testing.T) {
	s := NewString("hello")
	if s.Tag() != TagString || s.Type() != "S" || s.Str() != "hello" {
		t.Errorf("NewString: tag=%v type=%q str=%q", s.Tag(), s.Type(), s.Str())
	}

	n := NewNumber(mustDec("12.5"))
	if n.Tag() != TagNumber || n.Type() != "N" || n.Num().String() != "12.5" {
		t.Errorf("NewNumber: tag=%v type=%q num=%q", n.Tag(), n.Type(), n.Num().String())
	}

	b := NewBinary([]byte{1, 2, 3})
	if b.Tag() != TagBinary || b.Type() != "B" || len(b.Bin()) != 3 {
		t.Errorf("NewBinary: tag=%v type=%q len=%d", b.Tag(), b.Type(), len(b.Bin()))
	}

	bl := NewBool(true)
	if bl.Tag() != TagBoolean || bl.Type() != "BOOL" || !bl.Bool() {
		t.Errorf("NewBool: tag=%v type=%q bool=%v", bl.Tag(), bl.Type(), bl.Bool())
	}

	nl := NewNull()
	if nl.Tag() != TagNull || nl.Type() != "NULL" {
		t.Errorf("NewNull: tag=%v type=%q", nl.Tag(), nl.Type())
	}
}

// TestTagString covers the container/set type codes and the UNKNOWN fallback
// for an unregistered tag.
func TestTagString(t *testing.T) {
	cases := []struct {
		tag  Tag
		want string
	}{
		{TagNull, "NULL"},
		{TagString, "S"},
		{TagNumber, "N"},
		{TagBinary, "B"},
		{TagBoolean, "BOOL"},
		{TagList, "L"},
		{TagMap, "M"},
		{TagStringSet, "SS"},
		{TagNumberSet, "NS"},
		{TagBinarySet, "BS"},
		{Tag(255), "UNKNOWN"},
	}
	for _, c := range cases {
		if got := c.tag.String(); got != c.want {
			t.Errorf("Tag(%d).String() = %q, want %q", c.tag, got, c.want)
		}
	}
}

func TestNewNumberString(t *testing.T) {
	// valid: canonicalizes and validates.
	v, err := NewNumberString("1.50")
	if err != nil || v.Tag() != TagNumber || v.Num().String() != "1.5" {
		t.Errorf("NewNumberString(1.50): num=%q err=%v", v.Num().String(), err)
	}

	// malformed wraps num.ErrInvalidNumber.
	if _, err := NewNumberString("abc"); err == nil {
		t.Errorf("NewNumberString(abc) expected error")
	} else if !errors.Is(err, num.ErrInvalidNumber) {
		t.Errorf("NewNumberString(abc) error = %v, want wrap of ErrInvalidNumber", err)
	}

	// out-of-range precision wraps num.ErrTooManyDigits.
	big := strings.Repeat("9", 39)
	if _, err := NewNumberString(big); err == nil {
		t.Errorf("NewNumberString(39 nines) expected error")
	} else if !errors.Is(err, num.ErrTooManyDigits) {
		t.Errorf("NewNumberString(39 nines) error = %v, want wrap of ErrTooManyDigits", err)
	}
}

// mustDec parses a number string, panicking on error. Shared across test files.
func mustDec(s string) num.Decimal {
	d, err := num.Parse(s)
	if err != nil {
		panic(err)
	}
	return d
}

func TestContainers(t *testing.T) {
	l := NewList([]Value{NewString("a"), NewNull()})
	if l.Tag() != TagList || l.Type() != "L" || len(l.List()) != 2 {
		t.Errorf("NewList: tag=%v len=%d", l.Tag(), len(l.List()))
	}

	// constructor copies; mutating the input must not affect the value.
	in := []Value{NewString("x")}
	l2 := NewList(in)
	in[0] = NewString("y")
	if l2.List()[0].Str() != "x" {
		t.Errorf("NewList did not copy input")
	}

	m := NewMap(map[string]Value{"k": NewString("v")})
	if m.Tag() != TagMap || m.Type() != "M" || m.Map()["k"].Str() != "v" {
		t.Errorf("NewMap: tag=%v", m.Tag())
	}

	// constructor copies the map.
	src := map[string]Value{"a": NewString("b")}
	m2 := NewMap(src)
	src["a"] = NewNull()
	if m2.Map()["a"].Str() != "b" {
		t.Errorf("NewMap did not copy input")
	}
}
