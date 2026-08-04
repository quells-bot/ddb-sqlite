package ddb

import (
	"github.com/quells-bot/ddb-sqlite-core/attrval"
	"github.com/quells-bot/ddb-sqlite-core/internal/num"
)

// maxItemSize is the exact DynamoDB item-size limit: 400 KB = 409600 bytes.
// Probe-verified: no per-item storage overhead counts toward the write
// rejection (M6c §3.1).
const maxItemSize int64 = 400 * 1024

// maxItemDepth is the DynamoDB nesting-depth limit for item structure
// (M6c §3.2). The expression path-depth limit (internal/expr) uses the same
// base and value; the two agree by construction.
const maxItemDepth = 32

// zeroDecimal is the shared additive identity used to detect zero numbers.
// num.Decimal.Digits returns 1 for zero, but the item-size formula counts
// zero as 0 significant digits (probe-verified: N_size("0") = 1).
var zeroDecimal = num.Zero()

// itemSize walks item computing the AWS-faithful byte size and maximum
// nesting depth in one allocation-light pass. Accessors return internal
// references; attrval Values are immutable by construction, so no copies.
func itemSize(item Item) (bytes int64, depth int) {
	for name, v := range item {
		bytes += int64(len(name)) + valueSize(v)
		if d := valueDepth(v); d > depth {
			depth = d
		}
	}
	return bytes, depth
}

// numberSize returns the N-value byte contribution: ceil(sig_digits/2)+1.
// Zero has 0 significant digits (size 1); Digits returns 1 for zero, so the
// zero case is special-cased.
func numberSize(d num.Decimal) int64 {
	sig := d.Digits()
	if d.Equal(zeroDecimal) {
		sig = 0
	}
	return int64((sig+1)/2 + 1) // ceil(sig/2) + 1
}

// valueSize returns the byte size of a value's content, excluding any
// attribute name. Container elements (list items, map entries) are sized
// via this same function; their per-element overhead and map keys are added
// by the container case.
func valueSize(v attrval.Value) int64 {
	switch v.Tag() {
	case attrval.TagString:
		return int64(len(v.Str()))
	case attrval.TagNumber:
		return numberSize(v.Num())
	case attrval.TagBinary:
		return int64(len(v.Bin()))
	case attrval.TagBoolean, attrval.TagNull:
		return 1
	case attrval.TagList:
		var n int64 = 3
		for _, e := range v.List() {
			n += 1 + valueSize(e)
		}
		return n
	case attrval.TagMap:
		var n int64 = 3
		for k, e := range v.Map() {
			n += 1 + int64(len(k)) + valueSize(e)
		}
		return n
	case attrval.TagStringSet:
		var n int64
		for _, s := range v.SS() {
			n += int64(len(s))
		}
		return n
	case attrval.TagNumberSet:
		var n int64
		for _, d := range v.NS() {
			n += numberSize(d)
		}
		return n
	case attrval.TagBinarySet:
		var n int64
		for _, b := range v.BS() {
			n += int64(len(b))
		}
		return n
	}
	return 0
}

// valueDepth returns the nesting depth of a value: scalars and sets are
// depth 1; containers are 1 + max child depth (1 when empty). This is the
// same depth base the expression path-depth check uses (M6c §3.2).
func valueDepth(v attrval.Value) int {
	switch v.Tag() {
	case attrval.TagList:
		d := 1
		for _, e := range v.List() {
			if ed := valueDepth(e) + 1; ed > d {
				d = ed
			}
		}
		return d
	case attrval.TagMap:
		d := 1
		for _, e := range v.Map() {
			if ed := valueDepth(e) + 1; ed > d {
				d = ed
			}
		}
		return d
	default:
		return 1
	}
}
