package expr

import (
	"fmt"

	"github.com/quells-bot/ddb-sqlite-core/attrval"
)

// KeyCondition is the validated, extracted form of a
// KeyConditionExpression. Sort is nil when only the partition-key equality
// was supplied.
type KeyCondition struct {
	Partition struct {
		Name  string        // resolved attribute name (matches table PK)
		Value attrval.Value // the :value substitution
	}
	Sort *SortKeyCond // nil if absent
}

// SortKeyCond is one sort-key predicate. The structured form carries
// attrval.Value operands; ddb translates them to column-space Go values
// before passing to storage.
type SortKeyCond struct {
	Name       string
	Op         string        // "=", "<", "<=", ">", ">=", "BETWEEN", "BEGINS_WITH"
	Lo, Hi     attrval.Value // both set for BETWEEN; Lo set for all others
	BeginsWith attrval.Value // set for BEGINS_WITH only
}

// ExtractKeyCondition validates that b has the KeyConditionExpression shape,
// matching the partition and (optional) sort attribute names from the table
// schema. Either arm of the top-level AND may be the partition condition.
// pkName is required; skName is "" for a partition-only table.
func (b *BoundCondition) ExtractKeyCondition(pkName, skName string) (KeyCondition, error) {
	kc, err := extract(b.root, pkName, skName)
	if err != nil {
		return KeyCondition{}, err
	}
	// Must have exactly one partition condition.
	if kc.Partition.Name == "" {
		return KeyCondition{}, fmt.Errorf("%w: KeyConditionExpression must contain exactly one partition key condition", ErrSemantic)
	}
	// Sort condition when the table has no sort key.
	if kc.Sort != nil && skName == "" {
		return KeyCondition{}, fmt.Errorf("%w: sort key condition on a table with no sort key", ErrSemantic)
	}
	return kc, nil
}

// extract walks the bound AST and classifies each arm as partition or sort.
func extract(n condNode, pkName, skName string) (KeyCondition, error) {
	switch t := n.(type) {
	case *andNode:
		l, err := extract(t.left, pkName, skName)
		if err != nil {
			return KeyCondition{}, err
		}
		r, err := extract(t.right, pkName, skName)
		if err != nil {
			return KeyCondition{}, err
		}
		return mergeKeyConditions(l, r)
	case *cmpNode:
		return extractCmp(t, pkName, skName)
	case *betweenNode:
		return extractBetween(t, skName)
	case *funcNode:
		return extractFunc(t, skName)
	case *orNode:
		return KeyCondition{}, fmt.Errorf("%w: KeyConditionExpression may not contain OR", ErrSemantic)
	case *notNode:
		return KeyCondition{}, fmt.Errorf("%w: KeyConditionExpression may not contain NOT", ErrSemantic)
	case *inNode:
		return KeyCondition{}, fmt.Errorf("%w: KeyConditionExpression may not contain IN", ErrSemantic)
	}
	return KeyCondition{}, fmt.Errorf("%w: KeyConditionExpression has invalid node %T", ErrSemantic, n)
}

func extractCmp(n *cmpNode, pkName, skName string) (KeyCondition, error) {
	// The left side must be a path; the right must be a :value.
	path, ok := n.left.(*pathOperand)
	if !ok {
		return KeyCondition{}, fmt.Errorf("%w: KeyConditionExpression comparator must have a path on the left", ErrSemantic)
	}
	val, ok := n.right.(*valueOperand)
	if !ok {
		return KeyCondition{}, fmt.Errorf("%w: KeyConditionExpression comparator must have a :value on the right", ErrSemantic)
	}
	attrName := topAttrName(path)
	if attrName == "" {
		return KeyCondition{}, fmt.Errorf("%w: KeyConditionExpression path must reference a top-level attribute", ErrSemantic)
	}
	if attrName == pkName {
		if n.op != opEq {
			return KeyCondition{}, fmt.Errorf("%w: partition key must use equality", ErrSemantic)
		}
		return KeyCondition{Partition: struct {
			Name  string
			Value attrval.Value
		}{Name: pkName, Value: val.val}}, nil
	}
	if attrName == skName {
		op, ok := cmpOpString(n.op)
		if !ok {
			return KeyCondition{}, fmt.Errorf("%w: invalid sort key comparator", ErrSemantic)
		}
		return KeyCondition{Sort: &SortKeyCond{Name: skName, Op: op, Lo: val.val}}, nil
	}
	return KeyCondition{}, fmt.Errorf("%w: attribute %q is neither the partition nor sort key", ErrSemantic, attrName)
}

func extractBetween(n *betweenNode, skName string) (KeyCondition, error) {
	path, ok := n.operand.(*pathOperand)
	if !ok {
		return KeyCondition{}, fmt.Errorf("%w: BETWEEN must have a path operand", ErrSemantic)
	}
	lo, ok1 := n.lo.(*valueOperand)
	hi, ok2 := n.hi.(*valueOperand)
	if !ok1 || !ok2 {
		return KeyCondition{}, fmt.Errorf("%w: BETWEEN bounds must be :value substitutions", ErrSemantic)
	}
	name := topAttrName(path)
	if name != skName {
		return KeyCondition{}, fmt.Errorf("%w: BETWEEN on %q is not the sort key", ErrSemantic, name)
	}
	return KeyCondition{Sort: &SortKeyCond{Name: skName, Op: "BETWEEN", Lo: lo.val, Hi: hi.val}}, nil
}

func extractFunc(n *funcNode, skName string) (KeyCondition, error) {
	if n.name != fnBeginsWith {
		return KeyCondition{}, fmt.Errorf("%w: function %s is not allowed in KeyConditionExpression", ErrSemantic, funcName(n.name))
	}
	path := n.path // funcNode.path is already *pathOperand
	arg, ok := n.arg.(*valueOperand)
	if !ok {
		return KeyCondition{}, fmt.Errorf("%w: begins_with prefix must be a :value, not a path", ErrSemantic)
	}
	name := topAttrName(path)
	if name != skName {
		return KeyCondition{}, fmt.Errorf("%w: begins_with on %q is not the sort key", ErrSemantic, name)
	}
	return KeyCondition{Sort: &SortKeyCond{Name: skName, Op: "BEGINS_WITH", BeginsWith: arg.val}}, nil
}

// mergeKeyConditions combines the partition and sort conditions from the two
// AND arms. Exactly one arm must carry the partition; the other the sort.
func mergeKeyConditions(l, r KeyCondition) (KeyCondition, error) {
	var kc KeyCondition
	// Collect partition from whichever arm has it.
	if l.Partition.Name != "" && r.Partition.Name != "" {
		return KeyCondition{}, fmt.Errorf("%w: more than one partition key condition", ErrSemantic)
	}
	if l.Partition.Name != "" {
		kc.Partition = l.Partition
	} else if r.Partition.Name != "" {
		kc.Partition = r.Partition
	}
	// Collect sort from whichever arm has it.
	if l.Sort != nil {
		kc.Sort = l.Sort
	}
	if r.Sort != nil {
		if kc.Sort != nil {
			return KeyCondition{}, fmt.Errorf("%w: more than one sort key condition", ErrSemantic)
		}
		kc.Sort = r.Sort
	}
	if kc.Partition.Name == "" {
		return KeyCondition{}, fmt.Errorf("%w: AND must contain exactly one partition key condition", ErrSemantic)
	}
	return kc, nil
}

// topAttrName returns the top-level attribute name of a resolved path, or ""
// if the path is empty or starts with an index.
func topAttrName(p *pathOperand) string {
	if p == nil || len(p.resolved) == 0 || p.resolved[0].IsIndex {
		return ""
	}
	return p.resolved[0].Name
}

// cmpOpString maps a cmpOp to its string form for the sort-key condition.
func cmpOpString(op cmpOp) (string, bool) {
	switch op {
	case opEq:
		return "=", true
	case opLt:
		return "<", true
	case opLe:
		return "<=", true
	case opGt:
		return ">", true
	case opGe:
		return ">=", true
	}
	return "", false
}

// funcName returns the human-readable function name for error messages.
func funcName(k funcKind) string {
	switch k {
	case fnAttributeExists:
		return "attribute_exists"
	case fnAttributeNotExists:
		return "attribute_not_exists"
	case fnAttributeType:
		return "attribute_type"
	case fnContains:
		return "contains"
	case fnBeginsWith:
		return "begins_with"
	}
	return "unknown"
}
