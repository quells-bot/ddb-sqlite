// Package expr parses and evaluates DynamoDB condition, filter, and update
// expressions. Parsing is independent of substitution maps and of any item:
// ParseCondition yields an AST, Bind resolves #name/:value substitutions
// against an Env, and Eval applies the bound expression to one item. The
// package deals in attrval.Value and map[string]attrval.Value; it never
// imports ddb (that is the import-cycle direction).
package expr

import "errors"

// Sentinel errors. The ddb engine wraps all of them in ErrValidation, which the
// adapter renders as a ValidationException.
var (
	ErrSyntax    = errors.New("expr: syntax error")
	ErrUndefined = errors.New("expr: undefined substitution")
	ErrUnused    = errors.New("expr: unused substitution")
	ErrSemantic  = errors.New("expr: semantic error")
)
