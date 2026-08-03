package attrval

import (
	"reflect"
	"testing"

	"github.com/quells-bot/ddb-sqlite-core/internal/num"
)

func TestStringSet(t *testing.T) {
	ss := NewStringSet([]string{"c", "a", "b", "a"})
	if ss.Tag() != TagStringSet || ss.Type() != "SS" {
		t.Errorf("SS tag/type: %v %q", ss.Tag(), ss.Type())
	}
	if got := ss.SS(); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Errorf("SS = %v, want [a b c]", got)
	}
}

func TestNumberSet(t *testing.T) {
	// 1 and 1.0 collide; result is deduped and sorted numerically.
	ns := NewNumberSet([]num.Decimal{mustDec("2"), mustDec("1"), mustDec("1.0")})
	if ns.Tag() != TagNumberSet || ns.Type() != "NS" {
		t.Errorf("NS tag/type: %v %q", ns.Tag(), ns.Type())
	}
	if len(ns.NS()) != 2 || ns.NS()[0].String() != "1" || ns.NS()[1].String() != "2" {
		t.Errorf("NS = %v, want [1 2]", ns.NS())
	}
}

func TestBinarySet(t *testing.T) {
	bs := NewBinarySet([][]byte{{2}, {1}, {1}, {3}})
	if bs.Tag() != TagBinarySet || bs.Type() != "BS" {
		t.Errorf("BS tag/type: %v %q", bs.Tag(), bs.Type())
	}
	if got := bs.BS(); !reflect.DeepEqual(got, [][]byte{{1}, {2}, {3}}) {
		t.Errorf("BS = %v, want [[1] [2] [3]]", got)
	}
}

func TestNewNumberSetFromStrings(t *testing.T) {
	v, err := NewNumberSetFromStrings([]string{"2", "1", "1.0"})
	if err != nil {
		t.Fatalf("NewNumberSetFromStrings: %v", err)
	}
	if v.Tag() != TagNumberSet {
		t.Fatalf("tag = %v, want NumberSet", v.Tag())
	}
	// "1" and "1.0" collide; sorted numerically.
	ns := v.NS()
	if len(ns) != 2 || ns[0].String() != "1" || ns[1].String() != "2" {
		t.Errorf("NS = %v, want [1 2]", ns)
	}
}

func TestNewNumberSetFromStringsInvalid(t *testing.T) {
	if _, err := NewNumberSetFromStrings([]string{"1", "notanumber"}); err == nil {
		t.Error("invalid member should error")
	}
}
