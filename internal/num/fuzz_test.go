package num

import (
	"strings"
	"testing"
)

// FuzzParseRoundTrip asserts that any string Parse accepts round-trips through
// String and re-Parse to an equal, idempotent canonical form, and that Parse
// never panics on arbitrary input.
func FuzzParseRoundTrip(f *testing.F) {
	for _, seed := range []string{"0", "123", "-0.50", "001.2300", ".5", "100.", "-0"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		d, err := Parse(s)
		if err != nil {
			return
		}
		canon := d.String()
		d2, err := Parse(canon)
		if err != nil {
			t.Fatalf("Parse(%q) ok but Parse(canon=%q) failed: %v", s, canon, err)
		}
		if !d.Equal(d2) {
			t.Fatalf("round-trip not equal: %q -> %q", s, canon)
		}
		if d2.String() != canon {
			t.Fatalf("not idempotent: %q -> %q -> %q", s, canon, d2.String())
		}
	})
}

// FuzzCompareAntisymmetry asserts Compare is antisymmetric and consistent
// with Equal for any pair of parseable inputs.
func FuzzCompareAntisymmetry(f *testing.F) {
	for _, seed := range [][2]string{{"1.5", "-2.0"}, {"100", "0.001"}, {"0", "-0"}} {
		f.Add(seed[0], seed[1])
	}
	f.Fuzz(func(t *testing.T, sa, sb string) {
		a, errA := Parse(sa)
		b, errB := Parse(sb)
		if errA != nil || errB != nil {
			return
		}
		cab := a.Compare(b)
		cba := b.Compare(a)
		if cab+cba != 0 {
			t.Fatalf("Compare not antisymmetric: %q vs %q -> %d, %d", sa, sb, cab, cba)
		}
		if (cab == 0) != a.Equal(b) {
			t.Fatalf("Equal/Compare mismatch: %q vs %q -> %d", sa, sb, cab)
		}
	})
}

// FuzzAddSub asserts Add/Sub never panic and are exact: commutativity and
// the ring identities (a+b)-b == a and (a-b)+b == a hold for any parseable
// pair (M6c §9). Add/Sub deliberately do not enforce DynamoDB range or
// precision limits, so out-of-range operands are fair game and must still
// produce exact results.
func FuzzAddSub(f *testing.F) {
	big := strings.Repeat("9", 126)               // ~9.9e+125, past the 38-digit precision cap
	tiny := "0." + strings.Repeat("0", 129) + "1" // 1e-130, the minimum magnitude
	for _, seed := range [][2]string{
		{"0", "0"},
		{"-0", "0"},
		{"1.5", "-2.0"},
		{"0.1", "0.2"},
		{"100", "-100"},
		{"123456789.987654321", "-987654321.123456789"},
		{big, big},
		{big, "-" + big},
		{tiny, tiny},
		{big, tiny},
	} {
		f.Add(seed[0], seed[1])
	}
	f.Fuzz(func(t *testing.T, sa, sb string) {
		a, errA := Parse(sa)
		b, errB := Parse(sb)
		if errA != nil || errB != nil {
			return
		}
		sum := a.Add(b)
		if !sum.Equal(b.Add(a)) {
			t.Fatalf("Add not commutative: %s + %s", a, b)
		}
		if got := sum.Sub(b); !got.Equal(a) {
			t.Fatalf("(%s + %s) - %s = %s, want %s", a, b, b, got, a)
		}
		if got := a.Sub(b).Add(b); !got.Equal(a) {
			t.Fatalf("(%s - %s) + %s = %s, want %s", a, b, b, got, a)
		}
	})
}
