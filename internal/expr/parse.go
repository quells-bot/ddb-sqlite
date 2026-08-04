package expr

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseCondition parses a condition or filter expression. The two share this
// grammar exactly; they differ only in the filter-only validation rule applied
// by ValidateFilterRefs. Precedence, lowest to highest: OR, AND, NOT, then
// comparator/BETWEEN/IN/function.
func ParseCondition(src string) (*Condition, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	if p.peek().kind == tokEOF {
		return nil, fmt.Errorf("%w: expression is empty", ErrSyntax)
	}
	root, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	if p.peek().kind != tokEOF {
		return nil, fmt.Errorf("%w: unexpected %q at %d", ErrSyntax, p.peek().text, p.peek().pos)
	}
	if n := countCondOperators(root); n > maxOperators {
		return nil, fmt.Errorf("%w: operator count: 301", ErrLimit)
	}
	return &Condition{root: root, names: p.names, values: p.values}, nil
}

type parser struct {
	toks   []token
	i      int
	names  []string
	values []string
	depth  int
}

// maxParseDepth caps nesting in the condition grammar. Every level of
// parentheses or NOT recurses through parsePrimary/parseNot, so without a cap a
// sufficiently nested expression overflows the goroutine stack — a fatal error
// no recover can catch, which takes down the caller's whole test process
// instead of returning a ValidationException.
//
// 500 cannot reject anything the real service accepts. DynamoDB caps a
// condition expression at 300 operators and 4KB, and rejects redundant
// parentheses outright; meaningful nesting costs at least one operator per
// level, so any expression reaching this depth is one the reference rejects
// too. (The operator and length limits themselves belong to M6 hardening.)
const maxParseDepth = 500

// enter records one level of grammar recursion, and errors past the cap. Every
// enter that returns nil must be paired with a leave.
func (p *parser) enter() error {
	p.depth++
	if p.depth > maxParseDepth {
		return fmt.Errorf("%w: expression nested deeper than %d levels", ErrSyntax, maxParseDepth)
	}
	return nil
}

func (p *parser) leave() { p.depth-- }

func (p *parser) peek() token { return p.toks[p.i] }

func (p *parser) next() token {
	t := p.toks[p.i]
	if t.kind != tokEOF {
		p.i++
	}
	return t
}

func (p *parser) expect(k kind, what string) (token, error) {
	t := p.peek()
	if t.kind != k {
		return t, fmt.Errorf("%w: expected %s at %d, got %q", ErrSyntax, what, t.pos, t.text)
	}
	return p.next(), nil
}

// isKeyword reports whether the current token is the given bare keyword,
// matched case-insensitively.
func (p *parser) isKeyword(kw string) bool {
	t := p.peek()
	return t.kind == tokName && strings.EqualFold(t.text, kw)
}

func (p *parser) addName(ref string) {
	for _, n := range p.names {
		if n == ref {
			return
		}
	}
	p.names = append(p.names, ref)
}

func (p *parser) addValue(ref string) {
	for _, v := range p.values {
		if v == ref {
			return
		}
	}
	p.values = append(p.values, ref)
}

func (p *parser) parseOr() (condNode, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.isKeyword("OR") {
		p.next()
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left = &orNode{left: left, right: right}
	}
	return left, nil
}

func (p *parser) parseAnd() (condNode, error) {
	left, err := p.parseNot()
	if err != nil {
		return nil, err
	}
	for p.isKeyword("AND") {
		p.next()
		right, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		left = &andNode{left: left, right: right}
	}
	return left, nil
}

func (p *parser) parseNot() (condNode, error) {
	if p.isKeyword("NOT") {
		p.next()
		if err := p.enter(); err != nil {
			return nil, err
		}
		inner, err := p.parseNot()
		p.leave()
		if err != nil {
			return nil, err
		}
		return &notNode{inner: inner}, nil
	}
	return p.parsePrimary()
}

// funcArity maps a recognized function name to its argument count.
var funcArity = map[string]struct {
	kind  funcKind
	nargs int
}{
	"attribute_exists":     {fnAttributeExists, 1},
	"attribute_not_exists": {fnAttributeNotExists, 1},
	"attribute_type":       {fnAttributeType, 2},
	"contains":             {fnContains, 2},
	"begins_with":          {fnBeginsWith, 2},
}

func (p *parser) parsePrimary() (condNode, error) {
	if p.peek().kind == tokLParen {
		p.next()
		if err := p.enter(); err != nil {
			return nil, err
		}
		inner, err := p.parseOr()
		p.leave()
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(tokRParen, "')'"); err != nil {
			return nil, err
		}
		return inner, nil
	}

	// A boolean function call: a name token followed immediately by '('.
	if t := p.peek(); t.kind == tokName && p.toks[p.i+1].kind == tokLParen {
		lower := strings.ToLower(t.text)
		if lower == "size" {
			// size() is a value operand, not a boolean — fall through to the
			// comparison path below.
		} else if spec, ok := funcArity[lower]; ok {
			return p.parseFunc(spec.kind, spec.nargs)
		} else {
			return nil, fmt.Errorf("%w: unknown function %q at %d", ErrSyntax, t.text, t.pos)
		}
	}

	left, err := p.parseOperand()
	if err != nil {
		return nil, err
	}

	if p.isKeyword("BETWEEN") {
		p.next()
		lo, err := p.parseOperand()
		if err != nil {
			return nil, err
		}
		if !p.isKeyword("AND") {
			return nil, fmt.Errorf("%w: expected AND in BETWEEN at %d", ErrSyntax, p.peek().pos)
		}
		p.next()
		hi, err := p.parseOperand()
		if err != nil {
			return nil, err
		}
		return &betweenNode{operand: left, lo: lo, hi: hi}, nil
	}

	if p.isKeyword("IN") {
		p.next()
		if _, err := p.expect(tokLParen, "'(' after IN"); err != nil {
			return nil, err
		}
		var set []operandNode
		for {
			o, err := p.parseOperand()
			if err != nil {
				return nil, err
			}
			set = append(set, o)
			if p.peek().kind == tokComma {
				p.next()
				continue
			}
			break
		}
		if len(set) > maxInOperands {
			return nil, fmt.Errorf("%w: number of operands: %d", ErrLimit, len(set))
		}
		if _, err := p.expect(tokRParen, "')' closing IN"); err != nil {
			return nil, err
		}
		return &inNode{operand: left, set: set}, nil
	}

	var op cmpOp
	switch p.peek().kind {
	case tokEq:
		op = opEq
	case tokNe:
		op = opNe
	case tokLt:
		op = opLt
	case tokLe:
		op = opLe
	case tokGt:
		op = opGt
	case tokGe:
		op = opGe
	default:
		return nil, fmt.Errorf("%w: expected a comparator at %d, got %q", ErrSyntax, p.peek().pos, p.peek().text)
	}
	p.next()
	right, err := p.parseOperand()
	if err != nil {
		return nil, err
	}
	return &cmpNode{op: op, left: left, right: right}, nil
}

func (p *parser) parseFunc(k funcKind, nargs int) (condNode, error) {
	p.next() // function name
	p.next() // '('
	first, err := p.parseOperand()
	if err != nil {
		return nil, err
	}
	path, ok := first.(*pathOperand)
	if !ok {
		return nil, fmt.Errorf("%w: first argument must be a document path", ErrSyntax)
	}
	fn := &funcNode{name: k, path: path}
	if nargs == 2 {
		if _, err := p.expect(tokComma, "',' between function arguments"); err != nil {
			return nil, err
		}
		arg, err := p.parseOperand()
		if err != nil {
			return nil, err
		}
		fn.arg = arg
	}
	if _, err := p.expect(tokRParen, "')' closing the function call"); err != nil {
		return nil, err
	}
	return fn, nil
}

func (p *parser) parseOperand() (operandNode, error) {
	t := p.peek()
	switch t.kind {
	case tokValueRef:
		p.next()
		p.addValue(t.text)
		return &valueOperand{ref: t.text}, nil
	case tokName:
		if strings.EqualFold(t.text, "size") && p.toks[p.i+1].kind == tokLParen {
			p.next() // size
			p.next() // '('
			inner, err := p.parsePath()
			if err != nil {
				return nil, err
			}
			if _, err := p.expect(tokRParen, "')' closing size()"); err != nil {
				return nil, err
			}
			return &sizeOperand{path: inner}, nil
		}
		return p.parsePath()
	case tokNameRef:
		return p.parsePath()
	default:
		return nil, fmt.Errorf("%w: expected an operand at %d, got %q", ErrSyntax, t.pos, t.text)
	}
}

func (p *parser) parsePath() (*pathOperand, error) {
	seg, err := p.parseNameSeg()
	if err != nil {
		return nil, err
	}
	segs := []pathSeg{seg}
	for {
		switch p.peek().kind {
		case tokDot:
			p.next()
			s, err := p.parseNameSeg()
			if err != nil {
				return nil, err
			}
			segs = append(segs, s)
		case tokLBracket:
			p.next()
			it, err := p.expect(tokName, "a list index")
			if err != nil {
				return nil, err
			}
			idx, err := strconv.Atoi(it.text)
			if err != nil || idx < 0 {
				return nil, fmt.Errorf("%w: bad list index %q at %d", ErrSyntax, it.text, it.pos)
			}
			if len(it.text) > 1 && it.text[0] == '0' {
				return nil, fmt.Errorf("%w: list index %q must not have leading zeros at %d", ErrSyntax, it.text, it.pos)
			}
			if _, err := p.expect(tokRBracket, "']'"); err != nil {
				return nil, err
			}
			segs = append(segs, pathSeg{isIndex: true, index: idx})
		default:
			return &pathOperand{segs: segs}, nil
		}
	}
}

func (p *parser) parseNameSeg() (pathSeg, error) {
	t := p.peek()
	switch t.kind {
	case tokName:
		p.next()
		return pathSeg{name: t.text}, nil
	case tokNameRef:
		p.next()
		p.addName(t.text)
		return pathSeg{nameRef: t.text}, nil
	default:
		return pathSeg{}, fmt.Errorf("%w: expected a path segment at %d, got %q", ErrSyntax, t.pos, t.text)
	}
}

// ParseUpdate parses an update expression: one or more clauses, each of which
// may appear at most once, in any order. Clause keywords and the two SET-only
// function names are matched case-insensitively.
//
//	update := clause { clause }
//	clause := 'SET' setAction {',' setAction}
//	        | 'REMOVE' path {',' path}
//	        | 'ADD' path ':' name {',' path ':' name}
//	        | 'DELETE' path ':' name {',' path ':' name}
func ParseUpdate(src string) (*Update, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	if p.peek().kind == tokEOF {
		return nil, fmt.Errorf("%w: expression is empty", ErrSyntax)
	}
	u := &Update{}
	seen := map[string]bool{}
	for p.peek().kind != tokEOF {
		t := p.peek()
		if t.kind != tokName {
			return nil, fmt.Errorf("%w: expected a clause keyword at %d, got %q", ErrSyntax, t.pos, t.text)
		}
		kw := strings.ToUpper(t.text)
		switch kw {
		case "SET", "REMOVE", "ADD", "DELETE":
		default:
			return nil, fmt.Errorf("%w: unknown clause keyword %q at %d", ErrSyntax, t.text, t.pos)
		}
		if seen[kw] {
			return nil, fmt.Errorf("%w: duplicate %s clause at %d", ErrSyntax, kw, t.pos)
		}
		seen[kw] = true
		p.next()

		var clauseErr error
		switch kw {
		case "SET":
			clauseErr = p.parseSetClause(u)
		case "REMOVE":
			clauseErr = p.parseRemoveClause(u)
		case "ADD", "DELETE":
			clauseErr = p.parseModClause(u, kw)
		}
		if clauseErr != nil {
			return nil, clauseErr
		}
	}
	u.names, u.values = p.names, p.values
	if n := countUpdateOperators(u); n > maxOperators {
		return nil, fmt.Errorf("%w: operator count: 301", ErrLimit)
	}
	return u, nil
}

// parseSetClause parses comma-separated "path = value" actions. It stops at the
// first token that is not a comma, which is either EOF or the next clause
// keyword — the grammar needs no lookahead beyond that.
func (p *parser) parseSetClause(u *Update) error {
	for {
		path, err := p.parsePath()
		if err != nil {
			return err
		}
		if _, err := p.expect(tokEq, "'=' in a SET action"); err != nil {
			return err
		}
		val, err := p.parseSetValue()
		if err != nil {
			return err
		}
		u.sets = append(u.sets, setAction{path: path, value: val})
		if p.peek().kind != tokComma {
			return nil
		}
		p.next()
	}
}

func (p *parser) parseRemoveClause(u *Update) error {
	for {
		path, err := p.parsePath()
		if err != nil {
			return err
		}
		u.removes = append(u.removes, path)
		if p.peek().kind != tokComma {
			return nil
		}
		p.next()
	}
}

// parseModClause parses an ADD or DELETE clause. DynamoDB restricts both to a
// top-level attribute and a :value operand, so anything else is rejected here
// rather than deferred to bind time.
func (p *parser) parseModClause(u *Update, kw string) error {
	for {
		path, err := p.parsePath()
		if err != nil {
			return err
		}
		if len(path.segs) != 1 {
			return fmt.Errorf("%w: %s requires a top-level attribute, not a document path", ErrSemantic, kw)
		}
		t := p.peek()
		if t.kind != tokValueRef {
			return fmt.Errorf("%w: %s requires a :value operand at %d, got %q", ErrSyntax, kw, t.pos, t.text)
		}
		p.next()
		p.addValue(t.text)
		act := modAction{path: path, value: &valueOperand{ref: t.text}}
		if kw == "ADD" {
			u.adds = append(u.adds, act)
		} else {
			u.deletes = append(u.deletes, act)
		}
		if p.peek().kind != tokComma {
			return nil
		}
		p.next()
	}
}

// setFuncs are the two functions allowed on the right of a SET action.
var setFuncs = map[string]bool{"if_not_exists": true, "list_append": true}

func (p *parser) parseSetValue() (setValueNode, error) {
	if t := p.peek(); t.kind == tokName && p.toks[p.i+1].kind == tokLParen {
		lower := strings.ToLower(t.text)
		if !setFuncs[lower] {
			return nil, fmt.Errorf("%w: unknown function %q in a SET action at %d", ErrSyntax, t.text, t.pos)
		}
		p.next() // function name
		p.next() // '('
		if lower == "if_not_exists" {
			path, err := p.parsePath()
			if err != nil {
				return nil, err
			}
			if _, err := p.expect(tokComma, "',' between if_not_exists arguments"); err != nil {
				return nil, err
			}
			alt, err := p.parseUpdateOperand()
			if err != nil {
				return nil, err
			}
			if _, err := p.expect(tokRParen, "')' closing if_not_exists"); err != nil {
				return nil, err
			}
			return &ifNotExistsNode{path: path, alt: alt}, nil
		}
		left, err := p.parseUpdateOperand()
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(tokComma, "',' between list_append arguments"); err != nil {
			return nil, err
		}
		right, err := p.parseUpdateOperand()
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(tokRParen, "')' closing list_append"); err != nil {
			return nil, err
		}
		return &listAppendNode{left: left, right: right}, nil
	}

	left, err := p.parseUpdateOperand()
	if err != nil {
		return nil, err
	}
	switch p.peek().kind {
	case tokPlus, tokMinus:
		plus := p.next().kind == tokPlus
		right, err := p.parseUpdateOperand()
		if err != nil {
			return nil, err
		}
		// Exactly one operator: a second one leaves the token stream on a
		// non-keyword, which ParseUpdate reports as a syntax error.
		return &arithNode{plus: plus, left: left, right: right}, nil
	}
	// parseUpdateOperand rejects size(), so the only operand types that reach
	// here — *pathOperand and *valueOperand — both implement setValueNode.
	sv, ok := left.(setValueNode)
	if !ok {
		return nil, fmt.Errorf("%w: unsupported SET value at %d", ErrSyntax, p.peek().pos)
	}
	return sv, nil
}

// parseUpdateOperand parses the update grammar's operand — a document path or a
// :value. size() belongs to the condition grammar only, so it is rejected here
// instead of being parsed into a node no update evaluator understands.
func (p *parser) parseUpdateOperand() (operandNode, error) {
	if t := p.peek(); t.kind == tokName && strings.EqualFold(t.text, "size") && p.toks[p.i+1].kind == tokLParen {
		return nil, fmt.Errorf("%w: size() is not allowed in an update expression at %d", ErrSyntax, t.pos)
	}
	return p.parseOperand()
}
