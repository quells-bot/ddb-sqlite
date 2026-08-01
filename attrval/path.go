package attrval

import (
	"errors"
	"fmt"
	"strconv"
)

// ErrEmptyPath is returned by ParsePath for an empty path string.
var ErrEmptyPath = errors.New("attrval: empty document path")

// Segment is one component of a document path: either a map attribute name
// or a list index.
type Segment struct {
	IsIndex bool
	Name    string // valid when !IsIndex
	Index   int    // valid when IsIndex
}

// Path is a sequence of Segments navigated left-to-right into a Value.
type Path []Segment

// ParsePath parses a DynamoDB document path such as "a.b[2].c" into segments.
// Syntax: a leading attribute name followed by zero or more selectors, where
// a selector is ".name" (map nesting) or "[index]" (list indexing). Names are
// literal attribute names; substitution of "#name" tokens is the caller's
// responsibility (the expression engine resolves those before lookup).
func ParsePath(s string) (Path, error) {
	if s == "" {
		return nil, ErrEmptyPath
	}
	var segs Path
	i := 0
	// The first segment is always a name.
	j := i
	for j < len(s) && s[j] != '.' && s[j] != '[' && s[j] != ']' {
		j++
	}
	if j == i {
		return nil, fmt.Errorf("attrval: path %q: expected name at start", s)
	}
	segs = append(segs, Segment{Name: s[i:j]})
	i = j
	for i < len(s) {
		switch s[i] {
		case '.':
			i++
			j = i
			for j < len(s) && s[j] != '.' && s[j] != '[' && s[j] != ']' {
				j++
			}
			if j == i {
				return nil, fmt.Errorf("attrval: path %q: expected name after '.'", s)
			}
			segs = append(segs, Segment{Name: s[i:j]})
			i = j
		case '[':
			i++
			j = i
			for j < len(s) && s[j] >= '0' && s[j] <= '9' {
				j++
			}
			if j == i {
				return nil, fmt.Errorf("attrval: path %q: expected index inside '[]'", s)
			}
			if j >= len(s) || s[j] != ']' {
				return nil, fmt.Errorf("attrval: path %q: missing closing ']'", s)
			}
			idx, err := strconv.Atoi(s[i:j])
			if err != nil {
				return nil, fmt.Errorf("attrval: path %q: bad index: %w", s, err)
			}
			if j-i > 1 && s[i] == '0' {
				return nil, fmt.Errorf("attrval: path %q: list index must not have leading zeros", s)
			}
			segs = append(segs, Segment{IsIndex: true, Index: idx})
			i = j + 1
		default:
			return nil, fmt.Errorf("attrval: path %q: unexpected %q at %d", s, string(s[i]), i)
		}
	}
	return segs, nil
}

// Lookup navigates v along p, returning the found Value and true, or false if
// any segment is absent. A present Null is found (true), so callers can
// distinguish "attribute exists and is Null" from "attribute does not exist".
func (v Value) Lookup(p Path) (Value, bool) {
	cur := v
	for _, seg := range p {
		if seg.IsIndex {
			if cur.tag != TagList || seg.Index < 0 || seg.Index >= len(cur.list) {
				return Value{}, false
			}
			cur = cur.list[seg.Index]
		} else {
			if cur.tag != TagMap {
				return Value{}, false
			}
			val, ok := cur.m[seg.Name]
			if !ok {
				return Value{}, false
			}
			cur = val
		}
	}
	return cur, true
}

// ErrPathType is returned when a path segment cannot be applied to the value
// found there — descending into a non-container, or indexing a map.
var ErrPathType = errors.New("attrval: path segment type mismatch")

// ErrPathIndex is returned when a list index is out of range for a write.
// DynamoDB clamps any index >= len(list) to an append; a negative index is
// rejected (though the expression parser never produces one).
var ErrPathIndex = errors.New("attrval: list index out of range")

// ErrPathMissingParent is returned when a SetPath target's parent segment does
// not exist. DynamoDB creates only the final segment of a document path:
// `SET a.b.c = :v` is a ValidationException unless `a.b` already exists.
var ErrPathMissingParent = errors.New("attrval: document path parent does not exist")

// SetPath returns a copy of v with the value at p replaced by nv. Only the
// FINAL segment of p may be created — a missing intermediate parent is an
// error (ErrPathMissingParent), matching DynamoDB's "document path provided
// in the update expression is invalid for update". A list index >= len
// clamps to an append. Only the touched spine is copied; the receiver is
// unchanged.
func (v Value) SetPath(p Path, nv Value) (Value, error) {
	if len(p) == 0 {
		return nv, nil
	}
	seg := p[0]
	if seg.IsIndex {
		if v.tag != TagList {
			return Value{}, fmt.Errorf("%w: index into %s", ErrPathType, v.Type())
		}
		if seg.Index < 0 {
			return Value{}, fmt.Errorf("%w: index %d, len %d", ErrPathIndex, seg.Index, len(v.list))
		}
		// DynamoDB clamps any out-of-range index to an append: it neither
		// pads the list nor rejects the index.
		appending := seg.Index >= len(v.list)
		var child Value
		if !appending {
			child = v.list[seg.Index]
		} else if len(p) > 1 {
			// Creating a new element implied by a deeper path: start from a map
			// unless the next segment indexes, which would be out of range.
			if p[1].IsIndex {
				return Value{}, fmt.Errorf("%w: cannot create list under append", ErrPathIndex)
			}
			child = Value{tag: TagMap, m: map[string]Value{}}
		}
		sub, err := child.SetPath(p[1:], nv)
		if err != nil {
			return Value{}, err
		}
		cp := make([]Value, len(v.list), len(v.list)+1)
		copy(cp, v.list)
		if appending {
			cp = append(cp, sub)
		} else {
			cp[seg.Index] = sub
		}
		return Value{tag: TagList, list: cp}, nil
	}

	// A name segment requires a map. This rejects SET a.b = :v when a is a
	// scalar OR an explicit NULL, matching DynamoDB. Recursion never lands
	// here with a non-map: missing intermediates are rejected below before
	// the recursion can descend.
	if v.tag != TagMap {
		return Value{}, fmt.Errorf("%w: name segment on %s", ErrPathType, v.Type())
	}
	child, ok := v.m[seg.Name]
	if !ok {
		if len(p) > 1 {
			// Only the final segment may be created; a missing intermediate
			// parent is a validation error.
			return Value{}, fmt.Errorf("%w: %q", ErrPathMissingParent, seg.Name)
		}
		// Final segment: child stays zero; SetPath([], nv) returns nv.
	}
	sub, err := child.SetPath(p[1:], nv)
	if err != nil {
		return Value{}, err
	}
	cp := make(map[string]Value, len(v.m)+1)
	for k, val := range v.m {
		cp[k] = val
	}
	cp[seg.Name] = sub
	return Value{tag: TagMap, m: cp}, nil
}

// RemovePath returns a copy of v with the value at p deleted. Removing a list
// element shifts subsequent elements down. A path that does not resolve is a
// no-op returning v unchanged. Containers left empty are not pruned — DynamoDB
// does not auto-prune empty maps or lists.
func (v Value) RemovePath(p Path) Value {
	if len(p) == 0 {
		return v
	}
	seg := p[0]
	if seg.IsIndex {
		if v.tag != TagList || seg.Index < 0 || seg.Index >= len(v.list) {
			return v // missing -> no-op
		}
		if len(p) == 1 {
			cp := make([]Value, 0, len(v.list)-1)
			cp = append(cp, v.list[:seg.Index]...)
			cp = append(cp, v.list[seg.Index+1:]...)
			return Value{tag: TagList, list: cp}
		}
		cp := make([]Value, len(v.list))
		copy(cp, v.list)
		cp[seg.Index] = v.list[seg.Index].RemovePath(p[1:])
		return Value{tag: TagList, list: cp}
	}

	if v.tag != TagMap {
		return v // missing -> no-op
	}
	child, ok := v.m[seg.Name]
	if !ok {
		return v
	}
	cp := make(map[string]Value, len(v.m))
	for k, val := range v.m {
		cp[k] = val
	}
	if len(p) == 1 {
		delete(cp, seg.Name)
	} else {
		cp[seg.Name] = child.RemovePath(p[1:])
	}
	return Value{tag: TagMap, m: cp}
}
