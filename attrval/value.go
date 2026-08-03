// Package attrval provides DynamoDB's typed-value model, independent of the
// AWS SDK. A Value is a tagged union mirroring DynamoDB's AttributeValue
// types (S, N, B, BOOL, NULL, L, M, SS, NS, BS). Numbers carry an exact
// decimal (internal/num.Decimal); sets are deduplicated and canonically
// sorted at construction. Value is immutable by convention: construct via
// the New* functions and do not mutate returned slices/maps.
package attrval

import (
	"fmt"

	"github.com/quells-bot/ddb-sqlite-core/internal/num"
)

// Tag identifies the DynamoDB type of a Value.
type Tag uint8

const (
	TagNull Tag = iota
	TagString
	TagNumber
	TagBinary
	TagBoolean
	TagList
	TagMap
	TagStringSet
	TagNumberSet
	TagBinarySet
)

// String returns the DynamoDB type code for the tag: "S", "N", "B", "BOOL",
// "NULL", "L", "M", "SS", "NS", or "BS". This is the code used by the
// attribute_type condition function.
func (t Tag) String() string {
	switch t {
	case TagNull:
		return "NULL"
	case TagString:
		return "S"
	case TagNumber:
		return "N"
	case TagBinary:
		return "B"
	case TagBoolean:
		return "BOOL"
	case TagList:
		return "L"
	case TagMap:
		return "M"
	case TagStringSet:
		return "SS"
	case TagNumberSet:
		return "NS"
	case TagBinarySet:
		return "BS"
	}
	return "UNKNOWN"
}

// Value is a single DynamoDB typed value. The zero Value has tag TagNull.
// Obtain non-zero Values via the New* constructors; access the payload via
// the typed accessors (only the accessor matching the tag is meaningful).
type Value struct {
	tag  Tag
	str  string           // TagString
	num  num.Decimal      // TagNumber
	bin  []byte           // TagBinary
	b    bool             // TagBoolean
	list []Value          // TagList
	m    map[string]Value // TagMap
	ss   []string         // TagStringSet (deduped, sorted)
	ns   []num.Decimal    // TagNumberSet (deduped, sorted)
	bs   [][]byte         // TagBinarySet (deduped, sorted)
}

// Tag returns the Value's type tag.
func (v Value) Tag() Tag { return v.tag }

// Type returns the DynamoDB type code (same as v.Tag().String()).
func (v Value) Type() string { return v.tag.String() }

// NewString constructs a String value.
func NewString(s string) Value { return Value{tag: TagString, str: s} }

// NewNumber constructs a Number value from an already-built decimal. It does
// NOT validate DynamoDB limits; use NewNumberString for wire-boundary input.
func NewNumber(d num.Decimal) Value { return Value{tag: TagNumber, num: d} }

// NewNumberString parses a DynamoDB Number wire string and validates its
// precision/range. Use this at the wire boundary (JSON decode, adapter input).
func NewNumberString(s string) (Value, error) {
	d, err := num.Parse(s)
	if err != nil {
		return Value{}, fmt.Errorf("attrval: bad number %q: %w", s, err)
	}
	if err := d.Validate(); err != nil {
		return Value{}, fmt.Errorf("attrval: number %q out of range: %w", s, err)
	}
	return Value{tag: TagNumber, num: d}, nil
}

// NewBinary constructs a Binary value, copying the input bytes.
func NewBinary(b []byte) Value {
	cp := make([]byte, len(b))
	copy(cp, b)
	return Value{tag: TagBinary, bin: cp}
}

// NewBool constructs a Boolean value.
func NewBool(b bool) Value { return Value{tag: TagBoolean, b: b} }

// NewNull constructs a Null value.
func NewNull() Value { return Value{tag: TagNull} }

// Str returns the String value. Valid only when Tag()==TagString.
func (v Value) Str() string { return v.str }

// Num returns the Number's exact decimal. Valid only when Tag()==TagNumber.
func (v Value) Num() num.Decimal { return v.num }

// Bin returns the Binary bytes. Valid only when Tag()==TagBinary.
func (v Value) Bin() []byte { return v.bin }

// Bool returns the Boolean value. Valid only when Tag()==TagBoolean.
func (v Value) Bool() bool { return v.b }

// NewList constructs a List value, copying the input items.
func NewList(items []Value) Value {
	cp := make([]Value, len(items))
	copy(cp, items)
	return Value{tag: TagList, list: cp}
}

// NewMap constructs a Map value, copying the input map.
func NewMap(m map[string]Value) Value {
	cp := make(map[string]Value, len(m))
	for k, v := range m {
		cp[k] = v
	}
	return Value{tag: TagMap, m: cp}
}

// List returns the List elements. Valid only when Tag()==TagList.
func (v Value) List() []Value { return v.list }

// Map returns the Map entries. Valid only when Tag()==TagMap.
func (v Value) Map() map[string]Value { return v.m }
