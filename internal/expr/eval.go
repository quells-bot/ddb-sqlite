package expr

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"

	"github.com/quells-bot/ddb-sqlite-core/attrval"
)

// Eval evaluates the bound condition against one item. A nil item makes every
// path missing, which is how PutItem evaluates a condition against a key that
// does not yet exist.
//
// Type mismatches and missing attributes make a comparison false rather than an
// error, matching DynamoDB. Only genuine validation failures — a reversed
// BETWEEN range, a bad attribute_type code — return an error.
func (b *BoundCondition) Eval(item map[string]attrval.Value) (bool, error) {
	return evalCond(b.root, item)
}

// operandResult is a resolved operand. present distinguishes "attribute does
// not exist" from an existing NULL, which DynamoDB treats very differently.
type operandResult struct {
	v       attrval.Value
	present bool
}

func lookupItem(item map[string]attrval.Value, p attrval.Path) (attrval.Value, bool) {
	if len(p) == 0 || p[0].IsIndex {
		return attrval.Value{}, false
	}
	v, ok := item[p[0].Name]
	if !ok {
		return attrval.Value{}, false
	}
	if len(p) == 1 {
		return v, true
	}
	return v.Lookup(p[1:])
}

func evalCond(n condNode, item map[string]attrval.Value) (bool, error) {
	switch t := n.(type) {
	case *orNode:
		l, err := evalCond(t.left, item)
		if err != nil {
			return false, err
		}
		r, err := evalCond(t.right, item)
		if err != nil {
			return false, err
		}
		return l || r, nil
	case *andNode:
		l, err := evalCond(t.left, item)
		if err != nil {
			return false, err
		}
		r, err := evalCond(t.right, item)
		if err != nil {
			return false, err
		}
		return l && r, nil
	case *notNode:
		v, err := evalCond(t.inner, item)
		if err != nil {
			return false, err
		}
		return !v, nil
	case *cmpNode:
		return evalCmp(t, item)
	case *betweenNode:
		return evalBetween(t, item)
	case *inNode:
		return evalIn(t, item)
	case *funcNode:
		return evalFunc(t, item)
	}
	return false, fmt.Errorf("%w: unknown condition node %T", ErrSemantic, n)
}

func resolve(o operandNode, item map[string]attrval.Value) (operandResult, error) {
	switch t := o.(type) {
	case *pathOperand:
		v, ok := lookupItem(item, t.resolved)
		return operandResult{v: v, present: ok}, nil
	case *valueOperand:
		return operandResult{v: t.val, present: true}, nil
	case *sizeOperand:
		v, ok := lookupItem(item, t.path.resolved)
		if !ok {
			return operandResult{}, nil
		}
		sz, ok := sizeOf(v)
		if !ok {
			// size() is undefined for this type; the enclosing comparison is
			// false rather than an error. See spec §4.3(1) — the size()-on-N
			// case is settled by the dynamodb-local conformance probe.
			return operandResult{}, nil
		}
		n, err := attrval.NewNumberString(strconv.Itoa(sz))
		if err != nil {
			return operandResult{}, fmt.Errorf("%w: size %d: %v", ErrSemantic, sz, err)
		}
		return operandResult{v: n, present: true}, nil
	}
	return operandResult{}, fmt.Errorf("%w: unknown operand %T", ErrSemantic, o)
}

// sizeOf returns the DynamoDB size() of v. ok is false for types size() does
// not accept, which makes the enclosing comparison false.
func sizeOf(v attrval.Value) (int, bool) {
	switch v.Tag() {
	case attrval.TagString:
		return len(v.Str()), true
	case attrval.TagBinary:
		return len(v.Bin()), true
	case attrval.TagList:
		return len(v.List()), true
	case attrval.TagMap:
		return len(v.Map()), true
	case attrval.TagStringSet:
		return len(v.SS()), true
	case attrval.TagNumberSet:
		return len(v.NS()), true
	case attrval.TagBinarySet:
		return len(v.BS()), true
	}
	return 0, false
}

// compareValues orders two values. ok is false when the pair is not orderable —
// different types, or a type with no DynamoDB ordering (BOOL, NULL, L, M, sets).
func compareValues(a, b attrval.Value) (int, bool) {
	if a.Tag() != b.Tag() {
		return 0, false
	}
	switch a.Tag() {
	case attrval.TagString:
		return strings.Compare(a.Str(), b.Str()), true
	case attrval.TagNumber:
		return a.Num().Compare(b.Num()), true
	case attrval.TagBinary:
		return bytes.Compare(a.Bin(), b.Bin()), true
	}
	return 0, false
}

func evalCmp(n *cmpNode, item map[string]attrval.Value) (bool, error) {
	l, err := resolve(n.left, item)
	if err != nil {
		return false, err
	}
	r, err := resolve(n.right, item)
	if err != nil {
		return false, err
	}
	if !l.present || !r.present {
		// A missing operand makes most comparisons false, but `<>` is the
		// documented exception: real DynamoDB evaluates it true, because a
		// missing attribute is by definition not equal to anything. Settled
		// against dynamodb-local (spec §4.2).
		if n.op == opNe {
			return true, nil
		}
		return false, nil
	}
	switch n.op {
	case opEq:
		return l.v.Equal(r.v), nil
	case opNe:
		return !l.v.Equal(r.v), nil
	}
	c, ok := compareValues(l.v, r.v)
	if !ok {
		return false, nil
	}
	switch n.op {
	case opLt:
		return c < 0, nil
	case opLe:
		return c <= 0, nil
	case opGt:
		return c > 0, nil
	case opGe:
		return c >= 0, nil
	}
	return false, fmt.Errorf("%w: unknown comparator", ErrSemantic)
}

func evalBetween(n *betweenNode, item map[string]attrval.Value) (bool, error) {
	lo, err := resolve(n.lo, item)
	if err != nil {
		return false, err
	}
	hi, err := resolve(n.hi, item)
	if err != nil {
		return false, err
	}
	if lo.present && hi.present {
		if lo.v.Tag() != hi.v.Tag() {
			return false, fmt.Errorf("%w: BETWEEN bounds must have the same type, got %s and %s", ErrSemantic, lo.v.Type(), hi.v.Type())
		}
		if c, ok := compareValues(lo.v, hi.v); ok && c > 0 {
			return false, fmt.Errorf("%w: BETWEEN requires the upper bound to be >= the lower bound", ErrSemantic)
		}
	}
	o, err := resolve(n.operand, item)
	if err != nil {
		return false, err
	}
	if !o.present || !lo.present || !hi.present {
		return false, nil
	}
	cl, ok := compareValues(o.v, lo.v)
	if !ok {
		return false, nil
	}
	ch, ok := compareValues(o.v, hi.v)
	if !ok {
		return false, nil
	}
	return cl >= 0 && ch <= 0, nil
}

func evalIn(n *inNode, item map[string]attrval.Value) (bool, error) {
	o, err := resolve(n.operand, item)
	if err != nil {
		return false, err
	}
	if !o.present {
		return false, nil
	}
	for _, e := range n.set {
		r, err := resolve(e, item)
		if err != nil {
			return false, err
		}
		if r.present && o.v.Equal(r.v) {
			return true, nil
		}
	}
	return false, nil
}

// validTypeCodes are the attribute_type() second-argument codes.
var validTypeCodes = map[string]bool{
	"S": true, "N": true, "B": true, "BOOL": true, "NULL": true,
	"L": true, "M": true, "SS": true, "NS": true, "BS": true,
}

func evalFunc(n *funcNode, item map[string]attrval.Value) (bool, error) {
	v, present := lookupItem(item, n.path.resolved)

	switch n.name {
	case fnAttributeExists:
		return present, nil
	case fnAttributeNotExists:
		return !present, nil
	}

	arg, err := resolve(n.arg, item)
	if err != nil {
		return false, err
	}

	if n.name == fnAttributeType {
		// DynamoDB requires the second operand to be a :v value substitution; a
		// literal path/size operand (e.g. attribute_type(s, someAttr)) is a
		// validation error rather than a false comparison.
		if _, ok := n.arg.(*valueOperand); !ok {
			return false, fmt.Errorf("%w: attribute_type requires a :v value substitution as its second argument", ErrSemantic)
		}
		if !arg.present || arg.v.Tag() != attrval.TagString || !validTypeCodes[arg.v.Str()] {
			return false, fmt.Errorf("%w: attribute_type requires a string type code, got %v", ErrSemantic, arg.v)
		}
		if !present {
			return false, nil
		}
		return v.Type() == arg.v.Str(), nil
	}

	if !present || !arg.present {
		return false, nil
	}

	switch n.name {
	case fnContains:
		return evalContains(v, arg.v), nil
	case fnBeginsWith:
		if v.Tag() == attrval.TagString && arg.v.Tag() == attrval.TagString {
			return strings.HasPrefix(v.Str(), arg.v.Str()), nil
		}
		if v.Tag() == attrval.TagBinary && arg.v.Tag() == attrval.TagBinary {
			return bytes.HasPrefix(v.Bin(), arg.v.Bin()), nil
		}
		return false, nil
	}
	return false, fmt.Errorf("%w: unknown function", ErrSemantic)
}

func evalContains(v, arg attrval.Value) bool {
	switch v.Tag() {
	case attrval.TagString:
		return arg.Tag() == attrval.TagString && strings.Contains(v.Str(), arg.Str())
	case attrval.TagBinary:
		return arg.Tag() == attrval.TagBinary && bytes.Contains(v.Bin(), arg.Bin())
	case attrval.TagStringSet:
		if arg.Tag() != attrval.TagString {
			return false
		}
		for _, s := range v.SS() {
			if s == arg.Str() {
				return true
			}
		}
		return false
	case attrval.TagNumberSet:
		if arg.Tag() != attrval.TagNumber {
			return false
		}
		for _, d := range v.NS() {
			if d.Equal(arg.Num()) {
				return true
			}
		}
		return false
	case attrval.TagBinarySet:
		if arg.Tag() != attrval.TagBinary {
			return false
		}
		for _, bb := range v.BS() {
			if bytes.Equal(bb, arg.Bin()) {
				return true
			}
		}
		return false
	case attrval.TagList:
		for _, e := range v.List() {
			if e.Equal(arg) {
				return true
			}
		}
		return false
	}
	return false
}
