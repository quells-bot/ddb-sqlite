package ddbsqlite

import (
	"fmt"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/quells-bot/ddb-sqlite-core/attrval"
	"github.com/quells-bot/ddb-sqlite-core/ddb"
)

// nilErr reports a typed-nil AttributeValue member so FromSDK/FromSDKMap return
// a ValidationException-mapped error instead of panicking at a nil deref.
func nilErr(name string) error {
	return fmt.Errorf("ddbsqlite: %s is nil", name)
}

// FromSDK converts a single SDK AttributeValue to an attrval.Value. A Number or
// NumberSet member that fails precision/range validation returns an error (the
// adapter maps it to ValidationException).
func FromSDK(av types.AttributeValue) (attrval.Value, error) {
	switch v := av.(type) {
	case *types.AttributeValueMemberS:
		if v == nil {
			return attrval.Value{}, nilErr("AttributeValueMemberS")
		}
		return attrval.NewString(v.Value), nil
	case *types.AttributeValueMemberN:
		if v == nil {
			return attrval.Value{}, nilErr("AttributeValueMemberN")
		}
		n, err := attrval.NewNumberString(v.Value)
		if err != nil {
			return attrval.Value{}, err
		}
		return n, nil
	case *types.AttributeValueMemberB:
		if v == nil {
			return attrval.Value{}, nilErr("AttributeValueMemberB")
		}
		return attrval.NewBinary(v.Value), nil
	case *types.AttributeValueMemberBOOL:
		if v == nil {
			return attrval.Value{}, nilErr("AttributeValueMemberBOOL")
		}
		return attrval.NewBool(v.Value), nil
	case *types.AttributeValueMemberNULL:
		if v == nil {
			return attrval.Value{}, nilErr("AttributeValueMemberNULL")
		}
		return attrval.NewNull(), nil
	case *types.AttributeValueMemberL:
		if v == nil {
			return attrval.Value{}, nilErr("AttributeValueMemberL")
		}
		elems := make([]attrval.Value, 0, len(v.Value))
		for _, e := range v.Value {
			ev, err := FromSDK(e)
			if err != nil {
				return attrval.Value{}, err
			}
			elems = append(elems, ev)
		}
		return attrval.NewList(elems), nil
	case *types.AttributeValueMemberM:
		if v == nil {
			return attrval.Value{}, nilErr("AttributeValueMemberM")
		}
		m := make(map[string]attrval.Value, len(v.Value))
		for k, e := range v.Value {
			ev, err := FromSDK(e)
			if err != nil {
				return attrval.Value{}, err
			}
			m[k] = ev
		}
		return attrval.NewMap(m), nil
	case *types.AttributeValueMemberSS:
		if v == nil {
			return attrval.Value{}, nilErr("AttributeValueMemberSS")
		}
		if len(v.Value) == 0 {
			return attrval.Value{}, fmt.Errorf("ddbsqlite: a StringSet must not be empty")
		}
		return attrval.NewStringSet(v.Value), nil
	case *types.AttributeValueMemberNS:
		if v == nil {
			return attrval.Value{}, nilErr("AttributeValueMemberNS")
		}
		if len(v.Value) == 0 {
			return attrval.Value{}, fmt.Errorf("ddbsqlite: a NumberSet must not be empty")
		}
		return attrval.NewNumberSetFromStrings(v.Value)
	case *types.AttributeValueMemberBS:
		if v == nil {
			return attrval.Value{}, nilErr("AttributeValueMemberBS")
		}
		if len(v.Value) == 0 {
			return attrval.Value{}, fmt.Errorf("ddbsqlite: a BinarySet must not be empty")
		}
		return attrval.NewBinarySet(v.Value), nil
	default:
		return attrval.Value{}, fmt.Errorf("ddbsqlite: unsupported AttributeValue type %T", av)
	}
}

// ToSDK converts an attrval.Value to the SDK discriminated-union member.
func ToSDK(v attrval.Value) types.AttributeValue {
	switch v.Tag() {
	case attrval.TagString:
		return &types.AttributeValueMemberS{Value: v.Str()}
	case attrval.TagNumber:
		return &types.AttributeValueMemberN{Value: v.Num().String()}
	case attrval.TagBinary:
		return &types.AttributeValueMemberB{Value: v.Bin()}
	case attrval.TagBoolean:
		return &types.AttributeValueMemberBOOL{Value: v.Bool()}
	case attrval.TagNull:
		return &types.AttributeValueMemberNULL{Value: true}
	case attrval.TagList:
		elems := make([]types.AttributeValue, 0, len(v.List()))
		for _, e := range v.List() {
			elems = append(elems, ToSDK(e))
		}
		return &types.AttributeValueMemberL{Value: elems}
	case attrval.TagMap:
		m := make(map[string]types.AttributeValue, len(v.Map()))
		for k, e := range v.Map() {
			m[k] = ToSDK(e)
		}
		return &types.AttributeValueMemberM{Value: m}
	case attrval.TagStringSet:
		return &types.AttributeValueMemberSS{Value: v.SS()}
	case attrval.TagNumberSet:
		ns := v.NS()
		out := make([]string, 0, len(ns))
		for _, d := range ns {
			out = append(out, d.String())
		}
		return &types.AttributeValueMemberNS{Value: out}
	case attrval.TagBinarySet:
		return &types.AttributeValueMemberBS{Value: v.BS()}
	default:
		// Unreachable for values built via attrval constructors.
		return &types.AttributeValueMemberNULL{Value: true}
	}
}

// FromSDKMap converts an SDK item map to a ddb.Item.
func FromSDKMap(m map[string]types.AttributeValue) (ddb.Item, error) {
	item := ddb.Item{}
	for k, av := range m {
		v, err := FromSDK(av)
		if err != nil {
			return nil, err
		}
		item[k] = v
	}
	return item, nil
}

// ToSDKMap converts a ddb.Item to an SDK item map.
func ToSDKMap(item ddb.Item) map[string]types.AttributeValue {
	out := make(map[string]types.AttributeValue, len(item))
	for k, v := range item {
		out[k] = ToSDK(v)
	}
	return out
}
