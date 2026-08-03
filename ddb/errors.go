package ddb

import "errors"

// Typed sentinel errors matching parent spec §6.6. Returned by value so
// errors.Is works; the adapter maps each to the matching SDK exception type.
var (
	ErrResourceNotFound = errors.New("ddb: resource not found")
	ErrTableNotFound    = errors.New("ddb: table not found")
	ErrTableInUse       = errors.New("ddb: table already exists")
	ErrValidation       = errors.New("ddb: validation error")
	ErrConditionalCheck = errors.New("ddb: conditional check failed") // used from M2
)

// ConditionalCheckFailedError is returned when a ConditionExpression evaluates
// false. Item carries the pre-write item, populated only when the request set
// ReturnValuesOnConditionCheckFailure=ALL_OLD.
//
// Is reports true for ErrConditionalCheck so existing errors.Is call sites keep
// working; the adapter uses errors.As to recover Item.
type ConditionalCheckFailedError struct {
	Item Item
}

func (e *ConditionalCheckFailedError) Error() string {
	return ErrConditionalCheck.Error()
}

func (e *ConditionalCheckFailedError) Is(target error) bool {
	return target == ErrConditionalCheck
}

// ErrGsiNotFound is returned when IndexName is non-empty but the table has no
// such GSI (GSIs arrive in M4; in M3 any IndexName is rejected).
var ErrGsiNotFound = errors.New("ddb: global secondary index not found")

// Engine sentinels for UpdateTable (M6a).
var (
	ErrGsiInUse             = errors.New("ddb: global secondary index already exists")
	ErrLimitExceeded        = errors.New("ddb: limit exceeded")
	ErrGsiNotFoundForDelete = errors.New("ddb: global secondary index not found")
)
