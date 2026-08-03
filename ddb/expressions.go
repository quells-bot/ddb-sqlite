package ddb

import (
	"fmt"
	"strings"

	"github.com/quells-bot/ddb-sqlite-core/attrval"
	"github.com/quells-bot/ddb-sqlite-core/internal/expr"
)

// ReturnValues modes. PutItem and DeleteItem accept NONE and ALL_OLD only;
// UpdateItem accepts all five.
const (
	returnValuesNone       = "NONE"
	returnValuesAllOld     = "ALL_OLD"
	returnValuesAllNew     = "ALL_NEW"
	returnValuesUpdatedOld = "UPDATED_OLD"
	returnValuesUpdatedNew = "UPDATED_NEW"
)

// expressionRequest is the expression input for one operation. Update is empty
// for PutItem and DeleteItem, which have no update expression.
type expressionRequest struct {
	Condition  string
	Update     string
	Filter     string
	Projection string
	Names      map[string]string
	Values     map[string]attrval.Value
}

// preparedExpressions holds one request's bound expressions. A field is nil
// when that expression was absent.
type preparedExpressions struct {
	Cond   *expr.BoundCondition
	Update *expr.BoundUpdate
	Filter *expr.BoundCondition
	Proj   *expr.BoundProjection
}

// prepareExpressions parses every expression on the request, validates the
// substitution maps ONCE against the union of their refs, and binds each.
//
// The unused check must see the union: a #n referenced only by the
// UpdateExpression must not be reported unused because the ConditionExpression
// does not mention it (spec §4.5). It runs even when no expression was supplied,
// because DynamoDB rejects a map entry no expression references at all.
//
// Parsing and binding happen before any row is read so a malformed expression
// fails identically whether or not the item exists.
func prepareExpressions(r expressionRequest) (preparedExpressions, error) {
	env := expr.Env{Names: r.Names, Values: r.Values}
	var out preparedExpressions

	var cond *expr.Condition
	var upd *expr.Update
	var filter *expr.Condition
	var proj *expr.Projection
	var names, values []string

	if r.Condition != "" {
		c, err := expr.ParseCondition(r.Condition)
		if err != nil {
			return out, fmt.Errorf("%w: ConditionExpression: %v", ErrValidation, err)
		}
		cond = c
		cn, cv := c.Refs()
		names = append(names, cn...)
		values = append(values, cv...)
	}
	if r.Update != "" {
		u, err := expr.ParseUpdate(r.Update)
		if err != nil {
			return out, fmt.Errorf("%w: UpdateExpression: %v", ErrValidation, err)
		}
		upd = u
		un, uv := u.Refs()
		names = append(names, un...)
		values = append(values, uv...)
	}
	if r.Filter != "" {
		f, err := expr.ParseCondition(r.Filter)
		if err != nil {
			return out, fmt.Errorf("%w: FilterExpression: %v", ErrValidation, err)
		}
		filter = f
		fn, fv := f.Refs()
		names = append(names, fn...)
		values = append(values, fv...)
	}
	if r.Projection != "" {
		pr, err := expr.ParseProjection(r.Projection)
		if err != nil {
			return out, fmt.Errorf("%w: ProjectionExpression: %v", ErrValidation, err)
		}
		proj = pr
		pn, pv := pr.Refs()
		names = append(names, pn...)
		values = append(values, pv...)
	}

	if err := expr.CheckUnused(env, names, values); err != nil {
		return out, fmt.Errorf("%w: %v", ErrValidation, err)
	}

	if cond != nil {
		b, err := cond.Bind(env)
		if err != nil {
			return out, fmt.Errorf("%w: ConditionExpression: %v", ErrValidation, err)
		}
		out.Cond = b
	}
	if upd != nil {
		b, err := upd.Bind(env)
		if err != nil {
			return out, fmt.Errorf("%w: UpdateExpression: %v", ErrValidation, err)
		}
		out.Update = b
	}
	if filter != nil {
		b, err := filter.Bind(env)
		if err != nil {
			return out, fmt.Errorf("%w: FilterExpression: %v", ErrValidation, err)
		}
		out.Filter = b
	}
	if proj != nil {
		b, err := proj.Bind(env)
		if err != nil {
			return out, fmt.Errorf("%w: ProjectionExpression: %v", ErrValidation, err)
		}
		out.Proj = b
	}
	return out, nil
}

// checkCondition evaluates a bound condition against the pre-write item, which
// is nil when no item exists at that key. A false result yields
// *ConditionalCheckFailedError, carrying the item when the caller asked for it.
func checkCondition(b *expr.BoundCondition, item Item, returnOnFailure string) error {
	if b == nil {
		return nil
	}
	ok, err := b.Eval(item)
	if err != nil {
		return fmt.Errorf("%w: ConditionExpression: %v", ErrValidation, err)
	}
	if ok {
		return nil
	}
	e := &ConditionalCheckFailedError{}
	if strings.EqualFold(returnOnFailure, returnValuesAllOld) && len(item) > 0 {
		e.Item = item
	}
	return e
}

// validateReturnValuesOldOnly normalizes the ReturnValues field for PutItem and
// DeleteItem, which DynamoDB restricts to NONE and ALL_OLD. The AWS enum is
// case-sensitive: only the exact upper-case spelling is accepted.
func validateReturnValuesOldOnly(rv string) (string, error) {
	switch rv {
	case "", returnValuesNone:
		return returnValuesNone, nil
	case returnValuesAllOld:
		return returnValuesAllOld, nil
	}
	return "", fmt.Errorf("%w: ReturnValues %q is not supported for this operation", ErrValidation, rv)
}

// validateReturnValuesOnConditionCheckFailure normalizes the
// ReturnValuesOnConditionCheckFailure field shared by UpdateItem, PutItem, and
// DeleteItem, which DynamoDB restricts to NONE and ALL_OLD. The normalized
// value flows into checkCondition's returnOnFailure argument.
func validateReturnValuesOnConditionCheckFailure(rv string) (string, error) {
	switch rv {
	case "", returnValuesNone:
		return returnValuesNone, nil
	case returnValuesAllOld:
		return returnValuesAllOld, nil
	}
	return "", fmt.Errorf("%w: ReturnValuesOnConditionCheckFailure %q is not supported for this operation", ErrValidation, rv)
}

// validateReturnValuesUpdate normalizes UpdateItem's ReturnValues, which
// accepts all five modes. The AWS enum is case-sensitive.
func validateReturnValuesUpdate(rv string) (string, error) {
	switch rv {
	case "", returnValuesNone:
		return returnValuesNone, nil
	case returnValuesAllOld, returnValuesAllNew, returnValuesUpdatedOld, returnValuesUpdatedNew:
		return rv, nil
	}
	return "", fmt.Errorf("%w: ReturnValues %q is not supported for this operation", ErrValidation, rv)
}

// projectReturnValues builds the Attributes map an operation returns. touched
// carries per-attribute projection metadata from Apply. An empty projection
// returns nil so the adapter omits the field entirely, matching DynamoDB's
// "no Attributes" response.
//
// UPDATED_OLD projects attributes whose specific path existed in the original
// item (OldExisted). UPDATED_NEW projects attributes touched by a non-REMOVE
// action (Modified) that also survive in the updated item.
func projectReturnValues(mode string, old, updated Item, touched []expr.TouchedAttribute) Item {
	project := func(src Item, include func(expr.TouchedAttribute) bool) Item {
		out := Item{}
		for _, t := range touched {
			if !include(t) {
				continue
			}
			if v, ok := src[t.Name]; ok {
				out[t.Name] = v
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	}
	switch mode {
	case returnValuesAllOld:
		if len(old) == 0 {
			return nil
		}
		return old
	case returnValuesAllNew:
		if len(updated) == 0 {
			return nil
		}
		return updated
	case returnValuesUpdatedOld:
		return project(old, func(t expr.TouchedAttribute) bool { return t.OldExisted })
	case returnValuesUpdatedNew:
		return project(updated, func(t expr.TouchedAttribute) bool { return t.Modified })
	}
	return nil
}
