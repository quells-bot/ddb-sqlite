package expr

import (
	"fmt"

	"github.com/quells-bot/ddb-sqlite-core/attrval"
)

// Projection is a parsed projection expression: a comma-separated list of
// document paths and nothing else — no comparators, functions, or :value
// refs (those fail parseNameSeg's grammar, so rejection falls out). It is
// independent of substitution maps: call Bind to resolve #name refs, then
// the resolved paths feed attrval.Project.
type Projection struct {
	paths []*pathOperand
	names []string // "#n" refs, deduped, in first-appearance order
}

// Refs returns the substitution tokens this expression references. The value
// list is always nil — projections have no :value refs. The engine unions
// these with every other expression's refs before calling CheckUnused.
func (p *Projection) Refs() (names, values []string) {
	return p.names, nil
}

// ParseProjection parses a projection expression: path {',' path}. The empty
// string is rejected here for direct expr users; the engine never passes ""
// (every op gates on ProjectionExpression != ""), and the SDK-visible
// present-but-empty rejection lives in the adapter (exprString), which alone
// can distinguish absent from present-but-empty.
func ParseProjection(src string) (*Projection, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	if p.peek().kind == tokEOF {
		return nil, fmt.Errorf("%w: expression is empty", ErrSyntax)
	}
	var paths []*pathOperand
	for {
		path, err := p.parsePath()
		if err != nil {
			return nil, err
		}
		paths = append(paths, path)
		if p.peek().kind == tokEOF {
			break
		}
		if _, err := p.expect(tokComma, "',' between projection paths"); err != nil {
			return nil, err
		}
	}
	return &Projection{paths: paths, names: p.names}, nil
}

// BoundProjection is a projection with every #name resolved to an
// attrval.Path. It is independent of the *Projection it came from.
type BoundProjection struct {
	paths []attrval.Path
}

// Paths returns the resolved document paths, in expression order.
func (b *BoundProjection) Paths() []attrval.Path {
	return b.paths
}

// Bind resolves every #name reference against env, returning the bound
// projection. A reference absent from env is ErrUndefined. Bind also rejects
// any pair of paths that overlap — one a prefix of the other, equal paths
// included — matching DynamoDB's "Two document paths overlap with each
// other" rejection of both duplicates ("top, top") and parent/child pairs
// ("obj, obj.a"). Overlap can only be computed after binding, because two
// different aliases may resolve to the same attribute name.
func (p *Projection) Bind(env Env) (*BoundProjection, error) {
	b := binder{env: env}
	out := &BoundProjection{paths: make([]attrval.Path, 0, len(p.paths))}
	for _, po := range p.paths {
		bp, err := b.path(po)
		if err != nil {
			return nil, err
		}
		out.paths = append(out.paths, bp.resolved)
	}
	for i := 0; i < len(out.paths); i++ {
		for j := i + 1; j < len(out.paths); j++ {
			if pathOverlaps(out.paths[i], out.paths[j]) {
				return nil, fmt.Errorf("%w: two document paths in the projection overlap", ErrSemantic)
			}
		}
	}
	return out, nil
}
