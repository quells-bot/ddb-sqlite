package attrval

import (
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/quells-bot/ddb-sqlite-core/internal/num"
)

// MarshalJSON encodes the Value as DynamoDB wire JSON: a single-key object
// whose key is the type code and value is the payload. Sets emit their
// canonically-sorted elements; this relies on the sets-being-canonically-
// sorted invariant established at construction. Numbers emit their canonical
// decimal string.
func (v Value) MarshalJSON() ([]byte, error) {
	switch v.tag {
	case TagNull:
		return []byte(`{"NULL":true}`), nil
	case TagString:
		return marshalSingleton("S", v.str)
	case TagNumber:
		return marshalSingleton("N", v.num.String())
	case TagBinary:
		return marshalSingleton("B", base64.StdEncoding.EncodeToString(v.bin))
	case TagBoolean:
		return marshalSingleton("BOOL", v.b)
	case TagList:
		return marshalSingleton("L", v.list)
	case TagMap:
		return marshalSingleton("M", v.m)
	case TagStringSet:
		return marshalSingleton("SS", v.ss)
	case TagNumberSet:
		strs := make([]string, len(v.ns))
		for i, d := range v.ns {
			strs[i] = d.String()
		}
		return marshalSingleton("NS", strs)
	case TagBinarySet:
		strs := make([]string, len(v.bs))
		for i, b := range v.bs {
			strs[i] = base64.StdEncoding.EncodeToString(b)
		}
		return marshalSingleton("BS", strs)
	}
	return nil, fmt.Errorf("attrval: cannot marshal unknown tag %d", v.tag)
}

// marshalSingleton encodes {"key": value} via the standard encoder so JSON
// escaping and ordering are correct.
func marshalSingleton(key string, value any) ([]byte, error) {
	return json.Marshal(map[string]any{key: value})
}

// UnmarshalJSON decodes DynamoDB wire JSON into a Value. Exactly one type key
// must be present. Numbers are parsed and validated at this boundary.
func (v *Value) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("attrval: invalid AttributeValue JSON: %w", err)
	}
	if len(raw) != 1 {
		return fmt.Errorf("attrval: AttributeValue must have exactly one key, got %d", len(raw))
	}
	for key, val := range raw {
		switch key {
		case "S":
			var s string
			if err := json.Unmarshal(val, &s); err != nil {
				return fmt.Errorf("attrval: bad S value: %w", err)
			}
			*v = NewString(s)
		case "N":
			var s string
			if err := json.Unmarshal(val, &s); err != nil {
				return fmt.Errorf("attrval: bad N value: %w", err)
			}
			nv, err := NewNumberString(s)
			if err != nil {
				return err
			}
			*v = nv
		case "B":
			var s string
			if err := json.Unmarshal(val, &s); err != nil {
				return fmt.Errorf("attrval: bad B value: %w", err)
			}
			b, err := base64.StdEncoding.DecodeString(s)
			if err != nil {
				return fmt.Errorf("attrval: bad B base64: %w", err)
			}
			*v = NewBinary(b)
		case "BOOL":
			var b bool
			if err := json.Unmarshal(val, &b); err != nil {
				return fmt.Errorf("attrval: bad BOOL value: %w", err)
			}
			*v = NewBool(b)
		case "NULL":
			*v = NewNull()
		case "L":
			var items []Value
			if err := json.Unmarshal(val, &items); err != nil {
				return fmt.Errorf("attrval: bad L value: %w", err)
			}
			*v = NewList(items)
		case "M":
			var m map[string]Value
			if err := json.Unmarshal(val, &m); err != nil {
				return fmt.Errorf("attrval: bad M value: %w", err)
			}
			*v = NewMap(m)
		case "SS":
			var items []string
			if err := json.Unmarshal(val, &items); err != nil {
				return fmt.Errorf("attrval: bad SS value: %w", err)
			}
			*v = NewStringSet(items)
		case "NS":
			var items []string
			if err := json.Unmarshal(val, &items); err != nil {
				return fmt.Errorf("attrval: bad NS value: %w", err)
			}
			ds := make([]num.Decimal, 0, len(items))
			for _, s := range items {
				d, err := num.Parse(s)
				if err != nil {
					return fmt.Errorf("attrval: bad NS element %q: %w", s, err)
				}
				if err := d.Validate(); err != nil {
					return fmt.Errorf("attrval: NS element %q out of range: %w", s, err)
				}
				ds = append(ds, d)
			}
			*v = NewNumberSet(ds)
		case "BS":
			var items []string
			if err := json.Unmarshal(val, &items); err != nil {
				return fmt.Errorf("attrval: bad BS value: %w", err)
			}
			bs := make([][]byte, 0, len(items))
			for _, s := range items {
				b, err := base64.StdEncoding.DecodeString(s)
				if err != nil {
					return fmt.Errorf("attrval: bad BS base64 %q: %w", s, err)
				}
				bs = append(bs, b)
			}
			*v = NewBinarySet(bs)
		default:
			return fmt.Errorf("attrval: unknown AttributeValue key %q", key)
		}
	}
	return nil
}
