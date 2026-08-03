package expr

import (
	"fmt"
	"sort"
	"strings"

	"github.com/quells-bot/ddb-sqlite-core/attrval"
)

// Env carries the request's substitution maps. Keys include their sigil, as the
// AWS SDK supplies them: Names is keyed "#n", Values ":v".
type Env struct {
	Names  map[string]string
	Values map[string]attrval.Value
}

// BoundCondition is a condition/filter expression with every substitution
// resolved and every document path materialized as an attrval.Path. It is
// independent of the *Condition it came from, so one parse may be bound
// against several environments.
type BoundCondition struct {
	root condNode
}

// Bind resolves #name and :value references against env, returning a bound copy
// of the expression. A reference absent from env is ErrUndefined.
func (c *Condition) Bind(env Env) (*BoundCondition, error) {
	b := binder{env: env}
	root, err := b.cond(c.root)
	if err != nil {
		return nil, err
	}
	return &BoundCondition{root: root}, nil
}

type binder struct{ env Env }

func (b binder) cond(n condNode) (condNode, error) {
	switch t := n.(type) {
	case *orNode:
		l, err := b.cond(t.left)
		if err != nil {
			return nil, err
		}
		r, err := b.cond(t.right)
		if err != nil {
			return nil, err
		}
		return &orNode{left: l, right: r}, nil
	case *andNode:
		l, err := b.cond(t.left)
		if err != nil {
			return nil, err
		}
		r, err := b.cond(t.right)
		if err != nil {
			return nil, err
		}
		return &andNode{left: l, right: r}, nil
	case *notNode:
		in, err := b.cond(t.inner)
		if err != nil {
			return nil, err
		}
		return &notNode{inner: in}, nil
	case *cmpNode:
		l, err := b.operand(t.left)
		if err != nil {
			return nil, err
		}
		r, err := b.operand(t.right)
		if err != nil {
			return nil, err
		}
		return &cmpNode{op: t.op, left: l, right: r}, nil
	case *betweenNode:
		o, err := b.operand(t.operand)
		if err != nil {
			return nil, err
		}
		lo, err := b.operand(t.lo)
		if err != nil {
			return nil, err
		}
		hi, err := b.operand(t.hi)
		if err != nil {
			return nil, err
		}
		return &betweenNode{operand: o, lo: lo, hi: hi}, nil
	case *inNode:
		o, err := b.operand(t.operand)
		if err != nil {
			return nil, err
		}
		set := make([]operandNode, 0, len(t.set))
		for _, e := range t.set {
			be, err := b.operand(e)
			if err != nil {
				return nil, err
			}
			set = append(set, be)
		}
		return &inNode{operand: o, set: set}, nil
	case *funcNode:
		p, err := b.path(t.path)
		if err != nil {
			return nil, err
		}
		fn := &funcNode{name: t.name, path: p}
		if t.arg != nil {
			a, err := b.operand(t.arg)
			if err != nil {
				return nil, err
			}
			fn.arg = a
		}
		return fn, nil
	}
	return nil, fmt.Errorf("%w: unknown condition node %T", ErrSemantic, n)
}

func (b binder) operand(o operandNode) (operandNode, error) {
	switch t := o.(type) {
	case *pathOperand:
		return b.path(t)
	case *valueOperand:
		return b.value(t)
	case *sizeOperand:
		p, err := b.path(t.path)
		if err != nil {
			return nil, err
		}
		return &sizeOperand{path: p}, nil
	}
	return nil, fmt.Errorf("%w: unknown operand %T", ErrSemantic, o)
}

// value resolves one ":v" reference against the environment. Shared by the
// condition and update binders so the ErrUndefined message is identical.
func (b binder) value(t *valueOperand) (*valueOperand, error) {
	v, ok := b.env.Values[t.ref]
	if !ok {
		return nil, fmt.Errorf("%w: %s is not defined in ExpressionAttributeValues", ErrUndefined, t.ref)
	}
	return &valueOperand{ref: t.ref, val: v}, nil
}

func (b binder) path(p *pathOperand) (*pathOperand, error) {
	resolved := make(attrval.Path, 0, len(p.segs))
	for _, seg := range p.segs {
		if seg.isIndex {
			resolved = append(resolved, attrval.Segment{IsIndex: true, Index: seg.index})
			continue
		}
		// A bare name must not be a DynamoDB reserved word; only a #name
		// alias may escape the list. Matches real DynamoDB, which rejects
		// the expression at validation time.
		if seg.name != "" && reservedWords[strings.ToUpper(seg.name)] {
			return nil, fmt.Errorf("%w: attribute name %q is a DynamoDB reserved word; use a #name alias", ErrSemantic, seg.name)
		}
		name := seg.name
		if seg.nameRef != "" {
			actual, ok := b.env.Names[seg.nameRef]
			if !ok {
				return nil, fmt.Errorf("%w: %s is not defined in ExpressionAttributeNames", ErrUndefined, seg.nameRef)
			}
			name = actual
		}
		resolved = append(resolved, attrval.Segment{Name: name})
	}
	return &pathOperand{segs: p.segs, resolved: resolved}, nil
}

// CheckUnused reports an entry in env's maps that no expression references.
// DynamoDB validates this across ALL expressions on a request jointly, so the
// caller must pass the union of every expression's Refs — never one
// expression's refs at a time.
func CheckUnused(env Env, names, values []string) error {
	used := func(refs []string, ref string) bool {
		for _, r := range refs {
			if r == ref {
				return true
			}
		}
		return false
	}
	var unusedNames []string
	for k := range env.Names {
		if !used(names, k) {
			unusedNames = append(unusedNames, k)
		}
	}
	if len(unusedNames) > 0 {
		sort.Strings(unusedNames)
		return fmt.Errorf("%w: ExpressionAttributeNames entries not used in any expression: %v", ErrUnused, unusedNames)
	}
	var unusedValues []string
	for k := range env.Values {
		if !used(values, k) {
			unusedValues = append(unusedValues, k)
		}
	}
	if len(unusedValues) > 0 {
		sort.Strings(unusedValues)
		return fmt.Errorf("%w: ExpressionAttributeValues entries not used in any expression: %v", ErrUnused, unusedValues)
	}
	return nil
}

// ValidateFilterKeys enforces the filter-only rule from spec §4.4: a
// FilterExpression may not reference the key attributes of the table or index
// being scanned. Built and tested in M2; called from Query/Scan in M3.
func (b *BoundCondition) ValidateFilterKeys(keyAttrs []string) error {
	var bad string
	walkPaths(b.root, func(p attrval.Path) {
		if bad != "" || len(p) == 0 || p[0].IsIndex {
			return
		}
		for _, k := range keyAttrs {
			if p[0].Name == k {
				bad = k
				return
			}
		}
	})
	if bad != "" {
		return fmt.Errorf("%w: filter expression may not reference key attribute %q", ErrSemantic, bad)
	}
	return nil
}

// walkPaths calls fn for every resolved document path in a bound tree.
func walkPaths(n condNode, fn func(attrval.Path)) {
	var operand func(operandNode)
	operand = func(o operandNode) {
		switch t := o.(type) {
		case *pathOperand:
			fn(t.resolved)
		case *sizeOperand:
			fn(t.path.resolved)
		}
	}
	switch t := n.(type) {
	case *orNode:
		walkPaths(t.left, fn)
		walkPaths(t.right, fn)
	case *andNode:
		walkPaths(t.left, fn)
		walkPaths(t.right, fn)
	case *notNode:
		walkPaths(t.inner, fn)
	case *cmpNode:
		operand(t.left)
		operand(t.right)
	case *betweenNode:
		operand(t.operand)
		operand(t.lo)
		operand(t.hi)
	case *inNode:
		operand(t.operand)
		for _, e := range t.set {
			operand(e)
		}
	case *funcNode:
		fn(t.path.resolved)
		if t.arg != nil {
			operand(t.arg)
		}
	}
}

// BoundUpdate is an update expression with every substitution resolved and
// every action target materialized as an attrval.Path. It is independent of the
// *Update it came from, so one parse may be bound against several environments.
type BoundUpdate struct {
	sets    []setAction
	removes []*pathOperand
	adds    []modAction
	deletes []modAction
}

// Bind resolves #name and :value references against env. It also rejects two
// actions whose targets overlap (one path a prefix of the other, or equal),
// which DynamoDB reports as a validation error rather than applying in some
// order. Overlap can only be computed after binding, because two different
// aliases may resolve to the same attribute name.
func (u *Update) Bind(env Env) (*BoundUpdate, error) {
	b := binder{env: env}
	out := &BoundUpdate{}

	for _, a := range u.sets {
		p, err := b.path(a.path)
		if err != nil {
			return nil, err
		}
		v, err := b.setValue(a.value)
		if err != nil {
			return nil, err
		}
		out.sets = append(out.sets, setAction{path: p, value: v})
	}
	for _, r := range u.removes {
		p, err := b.path(r)
		if err != nil {
			return nil, err
		}
		out.removes = append(out.removes, p)
	}
	for _, list := range []struct {
		in  []modAction
		out *[]modAction
	}{{u.adds, &out.adds}, {u.deletes, &out.deletes}} {
		for _, a := range list.in {
			p, err := b.path(a.path)
			if err != nil {
				return nil, err
			}
			v, err := b.value(a.value)
			if err != nil {
				return nil, err
			}
			*list.out = append(*list.out, modAction{path: p, value: v})
		}
	}

	if err := out.checkOverlap(); err != nil {
		return nil, err
	}
	return out, nil
}

func (b binder) setValue(v setValueNode) (setValueNode, error) {
	switch t := v.(type) {
	case *pathOperand:
		return b.path(t)
	case *valueOperand:
		return b.value(t)
	case *arithNode:
		l, err := b.operand(t.left)
		if err != nil {
			return nil, err
		}
		r, err := b.operand(t.right)
		if err != nil {
			return nil, err
		}
		return &arithNode{plus: t.plus, left: l, right: r}, nil
	case *ifNotExistsNode:
		p, err := b.path(t.path)
		if err != nil {
			return nil, err
		}
		a, err := b.operand(t.alt)
		if err != nil {
			return nil, err
		}
		return &ifNotExistsNode{path: p, alt: a}, nil
	case *listAppendNode:
		l, err := b.operand(t.left)
		if err != nil {
			return nil, err
		}
		r, err := b.operand(t.right)
		if err != nil {
			return nil, err
		}
		return &listAppendNode{left: l, right: r}, nil
	}
	return nil, fmt.Errorf("%w: unknown SET value %T", ErrSemantic, v)
}

// targets returns every action's resolved target path, in expression order.
func (b *BoundUpdate) targets() []attrval.Path {
	out := make([]attrval.Path, 0, len(b.sets)+len(b.removes)+len(b.adds)+len(b.deletes))
	for _, a := range b.sets {
		out = append(out, a.path.resolved)
	}
	for _, r := range b.removes {
		out = append(out, r.resolved)
	}
	for _, a := range b.adds {
		out = append(out, a.path.resolved)
	}
	for _, a := range b.deletes {
		out = append(out, a.path.resolved)
	}
	return out
}

func (b *BoundUpdate) checkOverlap() error {
	ps := b.targets()
	for i := 0; i < len(ps); i++ {
		for j := i + 1; j < len(ps); j++ {
			if pathOverlaps(ps[i], ps[j]) {
				return fmt.Errorf("%w: two update actions modify overlapping document paths", ErrSemantic)
			}
		}
	}
	return nil
}

// pathOverlaps reports whether one path is a prefix of the other, equal paths
// included. attrval.Segment is comparable, so segments compare directly.
func pathOverlaps(a, b attrval.Path) bool {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ValidateKeyAttrs enforces spec §5.2: no update action may target a key
// attribute. ddb calls it with the table's key attribute names, since that is
// where the TableDef lives.
func (b *BoundUpdate) ValidateKeyAttrs(keyAttrs []string) error {
	for _, p := range b.targets() {
		if len(p) == 0 || p[0].IsIndex {
			continue
		}
		for _, k := range keyAttrs {
			if p[0].Name == k {
				return fmt.Errorf("%w: update expression may not modify key attribute %q", ErrSemantic, k)
			}
		}
	}
	return nil
}
