package expr

import (
	"encoding/json"
	"fmt"
)

// Expression limits, probe-verified against dynamodb-local 3.3.1 (M6c §4).
const (
	// maxExprString is the 4KB expression-string byte-length limit.
	maxExprString = 4096
	// maxOperators is the operator/function count limit per expression.
	maxOperators = 300
	// maxInOperands is the IN operand count limit.
	maxInOperands = 100
	// maxNameToken is the byte-length limit on an ExpressionAttributeNames key
	// (the "#name" token string in the expression, including the '#').
	maxNameToken = 255
	// maxPathDepth is the document-path nesting limit (path-segment count).
	// Uses the same depth base as ddb.maxItemDepth (M6c §3.2).
	maxPathDepth = 32
	// maxSerializedExprValues is the ~1MB cap on the serialized
	// ExpressionAttributeValues JSON map (including keys and {"S":…} wrappers).
	// dynamodb-local enforces ~1MB serialized, not the AWS-documented 2MB raw
	// (M6c §11 divergence). The exact byte boundary is dynamodb-local-specific;
	// this constant is the approximate cap.
	maxSerializedExprValues = 1 << 20 // 1 MiB
)

// checkSubstitutionLimits enforces the probe-verified token limits (M6c §4 #4):
//   - Every ExpressionAttributeNames key (the "#name" token string) <= 255 bytes.
//   - The serialized ExpressionAttributeValues JSON map <= ~1MB.
//
// The ~1MB value cap subsumes the individual :value limit: a single value
// over ~1MB makes the serialized map exceed the cap. Called at the start of
// every Bind so all call paths (including BatchGetItem's direct
// Projection.Bind) are covered.
func checkSubstitutionLimits(env Env) error {
	for k := range env.Names {
		if len(k) > maxNameToken {
			return fmt.Errorf("%w: key too long; size of key: %d", ErrLimit, len(k))
		}
	}
	if len(env.Values) > 0 {
		wire, err := json.Marshal(env.Values)
		if err != nil {
			return fmt.Errorf("%w: marshal expression values: %v", ErrLimit, err)
		}
		if len(wire) > maxSerializedExprValues {
			return fmt.Errorf("%w: expression size has exceeded the maximum allowed size", ErrLimit)
		}
	}
	return nil
}

// countCondOperators counts operators/functions in a condition AST:
// AND, OR, NOT, comparators, BETWEEN, IN (1 each), and functions.
// IN counts as exactly 1 operator regardless of operand count (probe-verified).
func countCondOperators(n condNode) int {
	switch t := n.(type) {
	case *orNode:
		return 1 + countCondOperators(t.left) + countCondOperators(t.right)
	case *andNode:
		return 1 + countCondOperators(t.left) + countCondOperators(t.right)
	case *notNode:
		return 1 + countCondOperators(t.inner)
	case *cmpNode:
		return 1
	case *betweenNode:
		return 1
	case *inNode:
		return 1
	case *funcNode:
		return 1
	}
	return 0
}

// countUpdateOperators counts operators in an update AST: + / - (arithNode)
// and SET-only functions (if_not_exists, list_append). SET '=' is assignment
// syntax, NOT counted (probe-verified).
func countUpdateOperators(u *Update) int {
	n := 0
	for _, a := range u.sets {
		n += countSetValueOperators(a.value)
	}
	return n
}

func countSetValueOperators(v setValueNode) int {
	switch v.(type) {
	case *arithNode:
		return 1
	case *ifNotExistsNode:
		return 1
	case *listAppendNode:
		return 1
	}
	return 0
}
