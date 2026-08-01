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
