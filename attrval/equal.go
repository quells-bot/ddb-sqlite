package attrval

import (
	"bytes"

	"github.com/quells-bot/ddb-sqlite/internal/num"
)

// Equal reports whether v and o are the same DynamoDB value. Numbers compare
// numerically (1 == 1.0). Sets compare element-wise (order-independent).
// Equal relies on sets being canonically sorted at construction, so element-
// wise compare equals set equality. Lists compare in order. Maps compare by
// key set and per-key equality. Binary by bytes. Different tags are never
// equal.
func (v Value) Equal(o Value) bool {
	if v.tag != o.tag {
		return false
	}
	switch v.tag {
	case TagNull:
		return true
	case TagString:
		return v.str == o.str
	case TagNumber:
		return v.num.Equal(o.num)
	case TagBinary:
		return bytes.Equal(v.bin, o.bin)
	case TagBoolean:
		return v.b == o.b
	case TagList:
		if len(v.list) != len(o.list) {
			return false
		}
		for i := range v.list {
			if !v.list[i].Equal(o.list[i]) {
				return false
			}
		}
		return true
	case TagMap:
		if len(v.m) != len(o.m) {
			return false
		}
		for k, val := range v.m {
			ov, ok := o.m[k]
			if !ok || !val.Equal(ov) {
				return false
			}
		}
		return true
	case TagStringSet:
		return equalStrings(v.ss, o.ss)
	case TagNumberSet:
		return equalNumbers(v.ns, o.ns)
	case TagBinarySet:
		return equalByteSlices(v.bs, o.bs)
	}
	return false
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalNumbers(a, b []num.Decimal) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !a[i].Equal(b[i]) {
			return false
		}
	}
	return true
}

func equalByteSlices(a, b [][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}
