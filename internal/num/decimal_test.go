package num

import (
	"errors"
	"strings"
	"testing"
)

func TestParseAndString(t *testing.T) {
	// Cases where Parse must succeed and String must yield the canonical form.
	happy := []struct{ in, want string }{
		{"0", "0"},
		{"123", "123"},
		{"00123", "123"},
		{"000", "0"},
		{"-5", "-5"},
		{"1.5", "1.5"},
		{"1.50", "1.5"},
		{"1.00", "1"},
		{"0.5", "0.5"},
		{".5", "0.5"},
		{"5.", "5"},
		{"0.0", "0"},
		{"-0", "0"},
		{"-0.0", "0"},
		{"-1.50", "-1.5"},
		{"100", "100"},
		{"0.05", "0.05"},
		{"10.0", "10"},
		{"100.500", "100.5"},
		{"-0.05", "-0.05"},
	}
	for _, c := range happy {
		d, err := Parse(c.in)
		if err != nil {
			t.Errorf("Parse(%q) unexpected error: %v", c.in, err)
			continue
		}
		if got := d.String(); got != c.want {
			t.Errorf("Parse(%q).String() = %q, want %q", c.in, got, c.want)
		}
	}

	// Cases where Parse must fail.
	for _, in := range []string{
		"", "+", "+5", "-", ".", "-.", "abc", "1e5", "1E5", "1.2.3",
		"1e", " ", " 1", "1 ", "0x10", "1,000", "1.5e-3", "--1", "1..5",
		"a", "1a", "1.5e", "NaN", "inf",
	} {
		if _, err := Parse(in); err == nil {
			t.Errorf("Parse(%q) expected error, got nil", in)
		} else if !errors.Is(err, ErrInvalidNumber) {
			t.Errorf("Parse(%q) error = %v, want ErrInvalidNumber", in, err)
		}
	}
}

// bigNumber builds a decimal string of nines followed by zeros (used by
// later validation tests; defined here so all tests share it).
func bigNumber(nines, zeros int) string {
	return strings.Repeat("9", nines) + strings.Repeat("0", zeros)
}

func TestEqual(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"1", "1", true},
		{"1", "1.0", true},
		{"1", "1.00", true},
		{"1.0", "1.00", true},
		{"-0", "0", true},
		{"0.0", "0", true},
		{"100", "100.0", true},
		{"0.1", "0.10", true},
		{"1", "2", false},
		{"1", "-1", false},
		{"1.5", "1.50", true},
		{"-1.5", "1.5", false},
		{"0.05", "0.5", false},
		{"100", "99", false},
	}
	for _, c := range cases {
		a, _ := Parse(c.a)
		b, _ := Parse(c.b)
		if got := a.Equal(b); got != c.want {
			t.Errorf("Equal(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
		// Equality must be symmetric.
		if b.Equal(a) != c.want {
			t.Errorf("Equal(%q, %q) not symmetric", c.a, c.b)
		}
	}
}

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0", "0", 0},
		{"1", "1.0", 0},
		{"1.50", "1.5", 0},
		{"-1", "0", -1},
		{"0", "1", -1},
		{"1", "2", -1},
		{"1", "1.5", -1},
		{"99.99", "100", -1},
		{"-1.5", "-1.4", -1},
		{"0.05", "0.1", -1},
		{"2", "1", 1},
		{"100", "10", 1},
		{"1.5", "1.0", 1},
		{"0", "-1", 1},
		{"-1.4", "-1.5", 1},
	}
	for _, c := range cases {
		a, _ := Parse(c.a)
		b, _ := Parse(c.b)
		if got := a.Compare(b); sign(got) != c.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
		// Antisymmetry: Compare(a,b) == -Compare(b,a).
		if sign(a.Compare(b))+sign(b.Compare(a)) != 0 {
			t.Errorf("Compare not antisymmetric for %q,%q", c.a, c.b)
		}
		// Less consistency.
		if a.Less(b) != (c.want < 0) {
			t.Errorf("Less(%q, %q) mismatch", c.a, c.b)
		}
	}
}

func sign(n int) int {
	if n < 0 {
		return -1
	}
	if n > 0 {
		return 1
	}
	return 0
}

func TestDigits(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"0", 1},
		{"1", 1},
		{"-5", 1},
		{"10", 1},  // trailing integer zero is not significant
		{"100", 1}, // ditto
		{"123", 3},
		{"100.5", 4},  // no trailing zeros to strip
		{"100.50", 4}, // canonical "100.5"
		{"1.5", 2},
		{"0.5", 1},
		{"0.05", 1},
		{"0.050", 1}, // canonical "0.05"
		{"1.00", 1},  // canonical "1"
		{"1230", 3},  // trailing zero stripped
		{"-1.50", 2}, // canonical "-1.5"
		{bigNumber(38, 0), 38},
		{bigNumber(38, 88), 38}, // 9.999..9E+125: 88 trailing zeros not significant
		{bigNumber(1, 125), 1},  // 1E+125
	}
	for _, c := range cases {
		d, err := Parse(c.in)
		if err != nil {
			t.Fatalf("Parse(%q) unexpected error: %v", c.in, err)
		}
		if got := d.Digits(); got != c.want {
			t.Errorf("Digits(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestZero(t *testing.T) {
	z := Zero()
	if got := z.String(); got != "0" {
		t.Errorf("Zero().String() = %q, want \"0\"", got)
	}
	if err := z.Validate(); err != nil {
		t.Errorf("Zero().Validate() = %v, want nil", err)
	}
	five, err := Parse("5")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := z.Add(five).String(); got != "5" {
		t.Errorf("0 + 5 = %s, want 5", got)
	}
}

func TestAddSub(t *testing.T) {
	cases := []struct {
		name             string
		a, b             string
		wantAdd, wantSub string
	}{
		{"integers", "2", "3", "5", "-1"},
		{"mixed scales", "1.5", "0.25", "1.75", "1.25"},
		{"negative operand", "-2.5", "1", "-1.5", "-3.5"},
		{"trailing zeros are insignificant", "1.10", "1.1", "2.2", "0"},
		{"result canonicalizes", "0.1", "0.9", "1", "-0.8"},
		{"beyond int64", "99999999999999999999", "1", "100000000000000000000", "99999999999999999998"},
		{"tiny scales", "0.0000000001", "0.0000000002", "0.0000000003", "-0.0000000001"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, err := Parse(tc.a)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.a, err)
			}
			b, err := Parse(tc.b)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.b, err)
			}
			if got := a.Add(b).String(); got != tc.wantAdd {
				t.Errorf("%s + %s = %s, want %s", tc.a, tc.b, got, tc.wantAdd)
			}
			if got := a.Sub(b).String(); got != tc.wantSub {
				t.Errorf("%s - %s = %s, want %s", tc.a, tc.b, got, tc.wantSub)
			}
			// Operands are immutable: neither receiver nor argument changed.
			a2, _ := Parse(tc.a)
			b2, _ := Parse(tc.b)
			if a.Compare(a2) != 0 || b.Compare(b2) != 0 {
				t.Errorf("Add/Sub mutated an operand: a=%s b=%s", a.String(), b.String())
			}
		})
	}
}

// Arithmetic may produce a value outside DynamoDB's range; Validate is the
// caller's gate, so the result must still be well-formed and detectable.
func TestAddOverflowIsDetectable(t *testing.T) {
	big1, err := Parse("9" + strings.Repeat("9", 37)) // 38 nines
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	sum := big1.Add(big1)
	if err := sum.Validate(); err == nil {
		t.Errorf("Validate() = nil for %s, want an error", sum.String())
	}
}

func TestValidate(t *testing.T) {
	// 1E-130 = "0." + 129 zeros + "1"  (smallest nonzero magnitude).
	minValid := "0." + strings.Repeat("0", 129) + "1"
	// 1E-131 = "0." + 130 zeros + "1"  (too small).
	tooSmall := "0." + strings.Repeat("0", 130) + "1"

	valid := []string{
		"0", "-0", "1", "-1", "100", "0.05",
		bigNumber(38, 0),  // 38 nines: max precision, small magnitude
		bigNumber(1, 125), // 1E+125 (within range)
		bigNumber(38, 88), // 9.999..9E+125 == max magnitude
		minValid,          // 1E-130 == min magnitude
	}
	for _, in := range valid {
		d, err := Parse(in)
		if err != nil {
			t.Fatalf("Parse(%q) unexpected error: %v", in, err)
		}
		if err := d.Validate(); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", in, err)
		}
	}

	tooMany := []string{
		bigNumber(39, 0),  // 39 significant digits
		bigNumber(39, 88), // 39 nines + 88 zeros: precision exceeded
	}
	for _, in := range tooMany {
		d, _ := Parse(in)
		if err := d.Validate(); !errors.Is(err, ErrTooManyDigits) {
			t.Errorf("Validate(%q) = %v, want ErrTooManyDigits", in, err)
		}
	}

	overflow := []string{
		bigNumber(1, 126), // 1E+126 > 9.999..9E+125
		bigNumber(38, 89), // 38 nines + 89 zeros: 10x over max
	}
	for _, in := range overflow {
		d, _ := Parse(in)
		if err := d.Validate(); !errors.Is(err, ErrNumberOverflow) {
			t.Errorf("Validate(%q) = %v, want ErrNumberOverflow", in, err)
		}
	}

	d, _ := Parse(tooSmall)
	if err := d.Validate(); !errors.Is(err, ErrNumberUnderflow) {
		t.Errorf("Validate(%q) = %v, want ErrNumberUnderflow", tooSmall, err)
	}
}
