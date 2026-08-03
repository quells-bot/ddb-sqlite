package expr

import "github.com/quells-bot/ddb-sqlite/attrval"

// Condition is a parsed condition or filter expression. It is independent of
// substitution maps and of any item: call Bind to resolve #name/:value against
// an Env, then Eval against an item. A *Condition may be bound more than once.
type Condition struct {
	root   condNode
	names  []string // "#n" refs, deduped, in first-appearance order
	values []string // ":v" refs, deduped, in first-appearance order
}

// Refs returns the substitution tokens this expression references. The engine
// unions these across every expression on a request before calling CheckUnused,
// because DynamoDB validates unused entries across all expressions jointly.
func (c *Condition) Refs() (names, values []string) {
	return c.names, c.values
}

type condNode interface{ condNode() }

type orNode struct{ left, right condNode }
type andNode struct{ left, right condNode }
type notNode struct{ inner condNode }

type cmpOp uint8

const (
	opEq cmpOp = iota
	opNe
	opLt
	opLe
	opGt
	opGe
)

type cmpNode struct {
	op          cmpOp
	left, right operandNode
}

type betweenNode struct{ operand, lo, hi operandNode }

type inNode struct {
	operand operandNode
	set     []operandNode
}

type funcKind uint8

const (
	fnAttributeExists funcKind = iota
	fnAttributeNotExists
	fnAttributeType
	fnContains
	fnBeginsWith
)

type funcNode struct {
	name funcKind
	path *pathOperand // every function's first argument is a document path
	arg  operandNode  // second argument; nil for attribute_(not_)exists
}

func (*orNode) condNode()      {}
func (*andNode) condNode()     {}
func (*notNode) condNode()     {}
func (*cmpNode) condNode()     {}
func (*betweenNode) condNode() {}
func (*inNode) condNode()      {}
func (*funcNode) condNode()    {}

type operandNode interface{ operandNode() }

// pathSeg is one document-path component before binding. A segment is either a
// list index, a literal name, or a #name reference resolved at bind time.
type pathSeg struct {
	isIndex bool
	index   int
	name    string // literal name; empty when nameRef is set
	nameRef string // "#n"; empty when name is set
}

// pathOperand carries the unbound segments from the parser; resolved is filled
// in by Bind, which substitutes every #name and yields the attrval.Path the
// evaluator navigates.
type pathOperand struct {
	segs     []pathSeg
	resolved attrval.Path
}

// valueOperand carries the ":v" token; val is filled in by Bind.
type valueOperand struct {
	ref string
	val attrval.Value
}
type sizeOperand struct{ path *pathOperand }

func (*pathOperand) operandNode()  {}
func (*valueOperand) operandNode() {}
func (*sizeOperand) operandNode()  {}

// Update is a parsed update expression. Like Condition it is independent of
// substitution maps and of any item: call Bind to resolve #name/:value, then
// Apply against an item. Each clause keyword may appear at most once, so the
// four action slices together hold every action in the expression.
type Update struct {
	sets    []setAction
	removes []*pathOperand
	adds    []modAction
	deletes []modAction

	names  []string // "#n" refs, deduped, in first-appearance order
	values []string // ":v" refs, deduped, in first-appearance order
}

// Refs returns the substitution tokens this expression references. The engine
// unions these with every other expression's refs before calling CheckUnused.
func (u *Update) Refs() (names, values []string) {
	return u.names, u.values
}

// setAction is one "path = value" action of a SET clause.
type setAction struct {
	path  *pathOperand
	value setValueNode
}

// modAction is one action of an ADD or DELETE clause. Both are restricted to
// top-level attributes and to a :value operand, so path always has exactly one
// name segment and value is always a substitution.
type modAction struct {
	path  *pathOperand
	value *valueOperand
}

// setValueNode is the right-hand side of a SET action: a bare operand, an
// arithmetic pair, or one of the two SET-only functions.
type setValueNode interface{ setValueNode() }

// arithNode is "left + right" (plus) or "left - right" (!plus). Both operands
// must resolve to Numbers at Apply time.
type arithNode struct {
	plus        bool
	left, right operandNode
}

// ifNotExistsNode yields path's value when it exists, alt's value otherwise.
type ifNotExistsNode struct {
	path *pathOperand
	alt  operandNode
}

// listAppendNode concatenates two operands that must both resolve to Lists.
type listAppendNode struct{ left, right operandNode }

func (*pathOperand) setValueNode()     {}
func (*valueOperand) setValueNode()    {}
func (*arithNode) setValueNode()       {}
func (*ifNotExistsNode) setValueNode() {}
func (*listAppendNode) setValueNode()  {}
