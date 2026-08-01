package expr

import (
	"bytes"
	"fmt"
	"sort"

	"github.com/quells-bot/ddb-sqlite/attrval"
	"github.com/quells-bot/ddb-sqlite/internal/num"
)

// TouchedAttribute describes one top-level attribute's contribution to the
// UPDATED_OLD and UPDATED_NEW ReturnValues projections.
//
//   - OldExisted: the specific document path this action touched existed in the
//     original item. UPDATED_OLD projects only attributes where this is true.
//   - Modified: the attribute was touched by a non-REMOVE action (SET, ADD, or
//     DELETE). UPDATED_NEW projects only attributes where this is true — a
//     REMOVE contributes nothing to UPDATED_NEW, even though the attribute may
//     survive in the updated item.
type TouchedAttribute struct {
	Name       string
	OldExisted bool
	Modified   bool
}

// Apply applies the bound update to item and returns the resulting item plus
// a TouchedAttribute per top-level attribute the update modified, in
// first-touch order.
//
// item is never mutated: Apply copies the top-level map and rewrites only the
// touched spine of each nested value (attrval.SetPath / RemovePath preserve the
// rest). Every operand resolves against the ORIGINAL item, never the
// partially-updated result, so "SET a = b, b = a" swaps the two attributes.
//
// REMOVE actions that target list indices on the same list are applied in
// descending index order so an earlier removal does not shift a later index
// — real DynamoDB resolves every index against the original list.
//
// A nil or empty item is the upsert case: ddb passes the key-only item it
// intends to create.
func (b *BoundUpdate) Apply(item map[string]attrval.Value) (map[string]attrval.Value, []TouchedAttribute, error) {
	out := make(map[string]attrval.Value, len(item)+len(b.sets))
	for k, v := range item {
		out[k] = v
	}

	type attrState struct {
		oldExisted bool
		modified   bool
	}
	states := map[string]*attrState{}
	var order []string
	touch := func(p attrval.Path, oldExisted, modified bool) {
		if len(p) == 0 || p[0].IsIndex {
			return
		}
		name := p[0].Name
		s, ok := states[name]
		if !ok {
			s = &attrState{}
			states[name] = s
			order = append(order, name)
		}
		if oldExisted {
			s.oldExisted = true
		}
		if modified {
			s.modified = true
		}
	}

	for _, a := range b.sets {
		nv, err := setValueOf(a.value, item)
		if err != nil {
			return nil, nil, err
		}
		_, existed := lookupItem(item, a.path.resolved)
		if err := setAt(out, a.path.resolved, nv); err != nil {
			return nil, nil, err
		}
		touch(a.path.resolved, existed, true)
	}

	// Sort removes so list-index removes on the same list apply in descending
	// index order, preventing an earlier removal from shifting a later index.
	removes := make([]*pathOperand, len(b.removes))
	copy(removes, b.removes)
	sort.SliceStable(removes, func(i, j int) bool {
		return removeSortKey(removes[i].resolved) > removeSortKey(removes[j].resolved)
	})
	for _, r := range removes {
		_, existed := lookupItem(item, r.resolved)
		if err := removeAt(out, r.resolved); err != nil {
			return nil, nil, err
		}
		touch(r.resolved, existed, false)
	}

	for _, a := range b.adds {
		_, existed := lookupItem(item, a.path.resolved)
		if err := applyAdd(out, item, a); err != nil {
			return nil, nil, err
		}
		touch(a.path.resolved, existed, true)
	}
	for _, a := range b.deletes {
		_, existed := lookupItem(item, a.path.resolved)
		if err := applyDelete(out, item, a); err != nil {
			return nil, nil, err
		}
		touch(a.path.resolved, existed, true)
	}

	touched := make([]TouchedAttribute, len(order))
	for i, name := range order {
		s := states[name]
		touched[i] = TouchedAttribute{Name: name, OldExisted: s.oldExisted, Modified: s.modified}
	}
	return out, touched, nil
}

// removeSortKey returns the last segment's list index for descending-order
// sorting, or -1 for non-index segments (which sort before all index removes).
func removeSortKey(p attrval.Path) int {
	if len(p) == 0 {
		return -1
	}
	last := p[len(p)-1]
	if last.IsIndex {
		return last.Index
	}
	return -1
}

// setAt writes nv at p in the top-level map. The first segment is always a
// name (the parser guarantees it), so segment 0 is resolved against the map
// directly and the remainder delegates to attrval.SetPath.
func setAt(m map[string]attrval.Value, p attrval.Path, nv attrval.Value) error {
	if len(p) == 0 {
		return fmt.Errorf("%w: empty update path", ErrSemantic)
	}
	name := p[0].Name
	if len(p) == 1 {
		m[name] = nv
		return nil
	}
	cur, ok := m[name]
	if !ok {
		// Only the final segment may be created; a missing parent is a
		// validation error, not an implicit empty map.
		return fmt.Errorf("%w: %w: %q", ErrSemantic, attrval.ErrPathMissingParent, name)
	}
	sub, err := cur.SetPath(p[1:], nv)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrSemantic, err)
	}
	m[name] = sub
	return nil
}

// removeAt deletes the value at p. A path that does not resolve is a no-op.
func removeAt(m map[string]attrval.Value, p attrval.Path) error {
	if len(p) == 0 {
		return fmt.Errorf("%w: empty update path", ErrSemantic)
	}
	name := p[0].Name
	if len(p) == 1 {
		delete(m, name)
		return nil
	}
	cur, ok := m[name]
	if !ok {
		return nil
	}
	m[name] = cur.RemovePath(p[1:])
	return nil
}

// operandValue resolves an update operand — a document path or a :value —
// against the original item. A path that does not resolve is a validation
// error: DynamoDB rejects an update whose operand names a missing attribute
// rather than substituting a default.
func operandValue(o operandNode, item map[string]attrval.Value) (attrval.Value, error) {
	switch t := o.(type) {
	case *pathOperand:
		v, ok := lookupItem(item, t.resolved)
		if !ok {
			return attrval.Value{}, fmt.Errorf("%w: update operand refers to an attribute that does not exist in the item", ErrSemantic)
		}
		return v, nil
	case *valueOperand:
		return t.val, nil
	}
	return attrval.Value{}, fmt.Errorf("%w: unsupported update operand %T", ErrSemantic, o)
}

// numOperand resolves an operand that must be a Number.
func numOperand(o operandNode, item map[string]attrval.Value) (num.Decimal, error) {
	v, err := operandValue(o, item)
	if err != nil {
		return num.Decimal{}, err
	}
	if v.Tag() != attrval.TagNumber {
		return num.Decimal{}, fmt.Errorf("%w: arithmetic requires Number operands, got %s", ErrSemantic, v.Type())
	}
	return v.Num(), nil
}

func setValueOf(v setValueNode, item map[string]attrval.Value) (attrval.Value, error) {
	switch t := v.(type) {
	case *pathOperand:
		return operandValue(t, item)
	case *valueOperand:
		return operandValue(t, item)
	case *arithNode:
		l, err := numOperand(t.left, item)
		if err != nil {
			return attrval.Value{}, err
		}
		r, err := numOperand(t.right, item)
		if err != nil {
			return attrval.Value{}, err
		}
		d := l.Add(r)
		if !t.plus {
			d = l.Sub(r)
		}
		if err := d.Validate(); err != nil {
			return attrval.Value{}, fmt.Errorf("%w: arithmetic result is out of DynamoDB's Number range: %v", ErrSemantic, err)
		}
		return attrval.NewNumber(d), nil
	case *ifNotExistsNode:
		// A present NULL counts as existing.
		if val, ok := lookupItem(item, t.path.resolved); ok {
			return val, nil
		}
		return operandValue(t.alt, item)
	case *listAppendNode:
		l, err := operandValue(t.left, item)
		if err != nil {
			return attrval.Value{}, err
		}
		r, err := operandValue(t.right, item)
		if err != nil {
			return attrval.Value{}, err
		}
		if l.Tag() != attrval.TagList || r.Tag() != attrval.TagList {
			return attrval.Value{}, fmt.Errorf("%w: list_append requires two List operands, got %s and %s", ErrSemantic, l.Type(), r.Type())
		}
		joined := make([]attrval.Value, 0, len(l.List())+len(r.List()))
		joined = append(joined, l.List()...)
		joined = append(joined, r.List()...)
		return attrval.NewList(joined), nil
	}
	return attrval.Value{}, fmt.Errorf("%w: unknown SET value %T", ErrSemantic, v)
}

// applyAdd implements ADD. The existing value is read from the ORIGINAL item;
// the result is written to out.
func applyAdd(out, item map[string]attrval.Value, a modAction) error {
	name := a.path.resolved[0].Name
	v := a.value.val
	cur, exists := item[name]

	switch v.Tag() {
	case attrval.TagNumber:
		base := num.Zero()
		if exists {
			if cur.Tag() != attrval.TagNumber {
				return fmt.Errorf("%w: ADD with a Number requires attribute %q to be a Number, got %s", ErrSemantic, name, cur.Type())
			}
			base = cur.Num()
		}
		sum := base.Add(v.Num())
		if err := sum.Validate(); err != nil {
			return fmt.Errorf("%w: ADD result is out of DynamoDB's Number range: %v", ErrSemantic, err)
		}
		out[name] = attrval.NewNumber(sum)
		return nil

	case attrval.TagStringSet, attrval.TagNumberSet, attrval.TagBinarySet:
		if !exists {
			out[name] = v
			return nil
		}
		if cur.Tag() != v.Tag() {
			return fmt.Errorf("%w: ADD with a %s requires attribute %q to be a %s, got %s", ErrSemantic, v.Type(), name, v.Type(), cur.Type())
		}
		switch v.Tag() {
		case attrval.TagStringSet:
			out[name] = attrval.NewStringSet(append(append([]string{}, cur.SS()...), v.SS()...))
		case attrval.TagNumberSet:
			out[name] = attrval.NewNumberSet(append(append([]num.Decimal{}, cur.NS()...), v.NS()...))
		case attrval.TagBinarySet:
			out[name] = attrval.NewBinarySet(append(append([][]byte{}, cur.BS()...), v.BS()...))
		}
		return nil
	}
	return fmt.Errorf("%w: ADD requires a Number or set value, got %s", ErrSemantic, v.Type())
}

// applyDelete implements DELETE. An emptied set is removed entirely: DynamoDB
// has no empty-set representation.
func applyDelete(out, item map[string]attrval.Value, a modAction) error {
	name := a.path.resolved[0].Name
	v := a.value.val
	switch v.Tag() {
	case attrval.TagStringSet, attrval.TagNumberSet, attrval.TagBinarySet:
	default:
		return fmt.Errorf("%w: DELETE requires a set value, got %s", ErrSemantic, v.Type())
	}

	cur, exists := item[name]
	if !exists {
		return nil
	}
	if cur.Tag() != v.Tag() {
		return fmt.Errorf("%w: DELETE with a %s requires attribute %q to be a %s, got %s", ErrSemantic, v.Type(), name, v.Type(), cur.Type())
	}

	switch v.Tag() {
	case attrval.TagStringSet:
		var keep []string
		for _, s := range cur.SS() {
			drop := false
			for _, x := range v.SS() {
				if s == x {
					drop = true
					break
				}
			}
			if !drop {
				keep = append(keep, s)
			}
		}
		if len(keep) == 0 {
			delete(out, name)
			return nil
		}
		out[name] = attrval.NewStringSet(keep)
	case attrval.TagNumberSet:
		var keep []num.Decimal
		for _, d := range cur.NS() {
			drop := false
			for _, x := range v.NS() {
				if d.Equal(x) {
					drop = true
					break
				}
			}
			if !drop {
				keep = append(keep, d)
			}
		}
		if len(keep) == 0 {
			delete(out, name)
			return nil
		}
		out[name] = attrval.NewNumberSet(keep)
	case attrval.TagBinarySet:
		var keep [][]byte
		for _, bb := range cur.BS() {
			drop := false
			for _, x := range v.BS() {
				if bytes.Equal(bb, x) {
					drop = true
					break
				}
			}
			if !drop {
				keep = append(keep, bb)
			}
		}
		if len(keep) == 0 {
			delete(out, name)
			return nil
		}
		out[name] = attrval.NewBinarySet(keep)
	}
	return nil
}
