package attrval

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/quells-bot/ddb-sqlite/internal/num"
)

func TestMarshalJSON(t *testing.T) {
	cases := []struct {
		name string
		v    Value
		want string
	}{
		{"null", NewNull(), `{"NULL":true}`},
		{"zero value", Value{}, `{"NULL":true}`},
		{"string", NewString("hi"), `{"S":"hi"}`},
		{"empty string", NewString(""), `{"S":""}`},
		{"number", NewNumber(mustDec("12.5")), `{"N":"12.5"}`},
		{"binary", NewBinary([]byte{1, 2}), `{"B":"AQI="}`},
		{"bool", NewBool(true), `{"BOOL":true}`},
		{"bool false", NewBool(false), `{"BOOL":false}`},
		{"list", NewList([]Value{NewString("a")}), `{"L":[{"S":"a"}]}`},
		{"empty list", NewList(nil), `{"L":[]}`},
		{"map", NewMap(map[string]Value{"k": NewString("v")}), `{"M":{"k":{"S":"v"}}}`},
		{"empty map", NewMap(nil), `{"M":{}}`},
		{"ss", NewStringSet([]string{"a", "b"}), `{"SS":["a","b"]}`},
		{"empty string set", NewStringSet(nil), `{"SS":[]}`},
		{"ns", NewNumberSet([]num.Decimal{mustDec("12.5")}), `{"NS":["12.5"]}`},
		{"empty number set", NewNumberSet(nil), `{"NS":[]}`},
		{"bs", NewBinarySet([][]byte{{1}}), `{"BS":["AQ=="]}`},
		{"empty binary set", NewBinarySet(nil), `{"BS":[]}`},
	}
	for _, c := range cases {
		got, err := json.Marshal(c.v)
		if err != nil {
			t.Errorf("Marshal(%s): %v", c.name, err)
			continue
		}
		if string(got) != c.want {
			t.Errorf("Marshal(%s) = %s, want %s", c.name, got, c.want)
		}
	}
}

// TestMarshalEscaping ensures JSON-special characters in strings are escaped.
func TestMarshalEscaping(t *testing.T) {
	got, err := json.Marshal(NewString(`a"b\c`))
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(got) != `{"S":"a\"b\\c"}` {
		t.Errorf("Marshal escaped = %s", got)
	}
}

// TestMarshalJSONUnknownTag ensures marshaling a Value with an unregistered
// tag fails instead of silently emitting a malformed object.
func TestMarshalJSONUnknownTag(t *testing.T) {
	if _, err := json.Marshal(Value{tag: Tag(255)}); err == nil {
		t.Errorf("Marshal of unknown tag expected error")
	}
}

// TestNewBinaryCopyIsolation ensures NewBinary copies its input bytes, so
// mutating the caller's slice after construction does not alter the value.
func TestNewBinaryCopyIsolation(t *testing.T) {
	input := []byte{1, 2, 3}
	v := NewBinary(input)
	input[0] = 99
	if !bytes.Equal(v.Bin(), []byte{1, 2, 3}) {
		t.Errorf("NewBinary did not copy input: got %v, want [1 2 3]", v.Bin())
	}
}

// TestNewBinarySetCopyIsolation ensures NewBinarySet copies each input element
// slice, so mutating an element after construction does not alter the value.
func TestNewBinarySetCopyIsolation(t *testing.T) {
	elem := []byte{1, 2, 3}
	v := NewBinarySet([][]byte{elem})
	elem[0] = 99
	stored := v.BS()
	if len(stored) != 1 || !bytes.Equal(stored[0], []byte{1, 2, 3}) {
		t.Errorf("NewBinarySet did not copy element: got %v, want [1 2 3]", stored)
	}
}

func TestUnmarshalJSON(t *testing.T) {
	cases := []struct {
		in      string
		wantTag Tag
	}{
		{`{"NULL":true}`, TagNull},
		{`{"S":"x"}`, TagString},
		{`{"N":"1.5"}`, TagNumber},
		{`{"B":"AQ=="}`, TagBinary},
		{`{"BOOL":false}`, TagBoolean},
		{`{"L":[{"S":"a"}]}`, TagList},
		{`{"M":{"k":{"S":"v"}}}`, TagMap},
		{`{"SS":["a","b"]}`, TagStringSet},
		{`{"NS":["1","2"]}`, TagNumberSet},
		{`{"BS":["AQ=="]}`, TagBinarySet},
	}
	for _, c := range cases {
		var v Value
		if err := json.Unmarshal([]byte(c.in), &v); err != nil {
			t.Errorf("Unmarshal(%s): %v", c.in, err)
			continue
		}
		if v.Tag() != c.wantTag {
			t.Errorf("Unmarshal(%s) tag = %v, want %v", c.in, v.Tag(), c.wantTag)
		}
	}

	// Number validated at the decode boundary (39 significant digits).
	tooBig := `{"N":"` + strings.Repeat("9", 39) + `"}`
	if err := json.Unmarshal([]byte(tooBig), &Value{}); err == nil {
		t.Errorf("Unmarshal 39-nine N expected validation error")
	}

	// NS element validated individually at decode (39-digit element rejected).
	tooBigNS := `{"NS":["1","` + strings.Repeat("9", 39) + `"]}`
	if err := json.Unmarshal([]byte(tooBigNS), &Value{}); err == nil {
		t.Errorf("Unmarshal NS with 39-nine element expected validation error")
	}

	// Exactly one key required.
	if err := json.Unmarshal([]byte(`{"S":"a","N":"1"}`), &Value{}); err == nil {
		t.Errorf("Unmarshal two-key object expected error")
	}

	// Unknown key rejected.
	if err := json.Unmarshal([]byte(`{"X":"a"}`), &Value{}); err == nil {
		t.Errorf("Unmarshal unknown key expected error")
	}

	// Empty object rejected.
	if err := json.Unmarshal([]byte(`{}`), &Value{}); err == nil {
		t.Errorf("Unmarshal empty object expected error")
	}
}

func TestWireRoundTrip(t *testing.T) {
	values := []Value{
		NewNull(),
		NewString("hi"),
		NewNumber(mustDec("12.5")),
		NewBinary([]byte{1, 2}),
		NewBool(true),
		NewList([]Value{NewString("a"), NewNull()}),
		NewMap(map[string]Value{"k": NewString("v"), "n": NewNumber(mustDec("1"))}),
		NewStringSet([]string{"b", "a"}),
		NewNumberSet([]num.Decimal{mustDec("1"), mustDec("1.0"), mustDec("2")}),
		NewBinarySet([][]byte{{1}, {2}}),
	}
	for _, v := range values {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		var v2 Value
		if err := json.Unmarshal(b, &v2); err != nil {
			t.Fatalf("Unmarshal %s: %v", b, err)
		}
		b2, err := json.Marshal(v2)
		if err != nil {
			t.Fatalf("Marshal v2: %v", err)
		}
		if string(b) != string(b2) {
			t.Errorf("round-trip mismatch: %s != %s", b, b2)
		}
	}
}
