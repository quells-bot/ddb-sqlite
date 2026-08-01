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
