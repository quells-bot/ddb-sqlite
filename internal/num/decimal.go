// Package num provides an exact decimal type used to represent DynamoDB
// Number values without float64 rounding. A Decimal is an exact decimal:
// value = coef × 10^(-scale), where scale >= 0 counts digits after the
// decimal point. Decimals are immutable; methods return new values.
package num

import (
	"errors"
	"math/big"
	"strings"
)

// ErrInvalidNumber is returned by Parse for a malformed number string.
var ErrInvalidNumber = errors.New("num: invalid number string")

// DynamoDB Number limits (see AWS "Supported data types and naming rules"):
//   - at most 38 significant digits
//   - positive range  1E-130 .. 9.9999999999999999999999999999999999999E+125
//   - negative range  -9.9999999999999999999999999999999999999E+125 .. -1E-130
const MaxSignificantDigits = 38

var (
	ErrTooManyDigits   = errors.New("num: number exceeds 38 significant digits")
	ErrNumberOverflow  = errors.New("num: number magnitude exceeds DynamoDB range")
	ErrNumberUnderflow = errors.New("num: number magnitude below DynamoDB range")
)

// Decimal is an exact decimal number. The zero value is NOT valid; obtain a
// Decimal via Parse or Zero.
type Decimal struct {
	coef  *big.Int // signed magnitude; value = coef × 10^(-scale). Set by Parse.
	scale int      // digits after the decimal point, >= 0
}

var ten = big.NewInt(10)

// Parse converts a DynamoDB Number wire string into a canonical Decimal.
// The grammar is a plain decimal literal:
//
//	decimal := ["-"] digits ["." digits] | ["-"] "." digits
//	digits  := "0".."9" { "0".."9" }
//
// At least one digit is required. Exponent notation, leading "+", and
// surrounding whitespace are rejected (DynamoDB treats N values as decimal
// literals, not scientific notation). Parse does NOT enforce DynamoDB
// precision/range limits; call Validate for that. The returned Decimal is in
// canonical form (see String).
func Parse(s string) (Decimal, error) {
	if s == "" {
		return Decimal{}, ErrInvalidNumber
	}
	neg := false
	body := s
	if body[0] == '-' {
		neg = true
		body = body[1:]
	} else if body[0] == '+' {
		return Decimal{}, ErrInvalidNumber
	}
	if body == "" {
		return Decimal{}, ErrInvalidNumber
	}

	// Split into integer and fractional parts on at most one '.'.
	var intPart, fracPart string
	if dot := strings.IndexByte(body, '.'); dot < 0 {
		intPart = body
	} else {
		intPart = body[:dot]
		fracPart = body[dot+1:]
		if strings.IndexByte(fracPart, '.') >= 0 {
			return Decimal{}, ErrInvalidNumber
		}
	}
	if intPart == "" && fracPart == "" {
		return Decimal{}, ErrInvalidNumber
	}
	if !onlyDigits(intPart) || !onlyDigits(fracPart) {
		return Decimal{}, ErrInvalidNumber
	}

	coef := new(big.Int)
	if d := intPart + fracPart; d != "" {
		if _, ok := coef.SetString(d, 10); !ok {
			return Decimal{}, ErrInvalidNumber
		}
	}
	if neg {
		coef.Neg(coef)
	}
	return canonicalize(Decimal{coef: coef, scale: len(fracPart)}), nil
}

// onlyDigits reports whether s contains only ASCII digits 0-9 (empty = true).
func onlyDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// canonicalize strips trailing fractional zeros (reducing scale) and
// normalizes negative zero to zero.
func canonicalize(d Decimal) Decimal {
	if d.coef.Sign() == 0 {
		return Decimal{coef: big.NewInt(0), scale: 0}
	}
	rem := new(big.Int)
	for d.scale > 0 && rem.Mod(d.coef, ten).Sign() == 0 {
		d.coef.Quo(d.coef, ten)
		d.scale--
	}
	return d
}

// String returns the canonical DynamoDB wire form: leading integer zeros and
// trailing fractional zeros trimmed; no "+"; no "-0"; no trailing ".";
// a single "0" before a bare decimal point (".5" -> "0.5"). Integer trailing
// zeros are preserved (trimming them would change the value).
func (d Decimal) String() string {
	if d.coef.Sign() == 0 {
		return "0"
	}
	neg := d.coef.Sign() < 0
	digits := new(big.Int).Abs(d.coef).String()
	if d.scale == 0 {
		if neg {
			return "-" + digits
		}
		return digits
	}
	if len(digits) <= d.scale {
		digits = "0." + strings.Repeat("0", d.scale-len(digits)) + digits
	} else {
		p := len(digits) - d.scale
		digits = digits[:p] + "." + digits[p:]
	}
	if neg {
		return "-" + digits
	}
	return digits
}

// Equal reports whether two decimals have the same numeric value.
// Scale is insignificant: 1 == 1.0 == 1.00. Negative zero equals zero.
func (d Decimal) Equal(o Decimal) bool {
	return d.Compare(o) == 0
}

// Compare returns -1, 0, or +1 per d < o, d == o, d > o. Ordering is numeric
// and scale-insensitive.
func (d Decimal) Compare(o Decimal) int {
	if d.scale == o.scale {
		return d.coef.Cmp(o.coef) // fast path: no rescaling allocation
	}
	a, b, _ := align(d, o)
	return a.Cmp(b)
}

// align rescales two decimals to a common scale, returning fresh coefficients
// at that scale. Neither operand is mutated: both coefficients are copies.
func align(d, o Decimal) (a, b *big.Int, scale int) {
	a = new(big.Int).Set(d.coef)
	b = new(big.Int).Set(o.coef)
	switch {
	case d.scale < o.scale:
		a.Mul(a, pow10(o.scale-d.scale))
		scale = o.scale
	case d.scale > o.scale:
		b.Mul(b, pow10(d.scale-o.scale))
		scale = d.scale
	default:
		scale = d.scale
	}
	return a, b, scale
}

// Zero returns the Decimal 0. The zero Decimal{} value has a nil coefficient
// and is not usable; callers that need an additive identity — ADD on a missing
// attribute, for instance — use this.
func Zero() Decimal { return Decimal{coef: big.NewInt(0), scale: 0} }

// Add returns d + o exactly. The result is canonical (trailing fractional
// zeros stripped). Add does NOT enforce DynamoDB's precision/range limits;
// call Validate on the result at the point the value re-enters an item.
func (d Decimal) Add(o Decimal) Decimal {
	a, b, scale := align(d, o)
	return canonicalize(Decimal{coef: a.Add(a, b), scale: scale})
}

// Sub returns d - o exactly. See Add.
func (d Decimal) Sub(o Decimal) Decimal {
	a, b, scale := align(d, o)
	return canonicalize(Decimal{coef: a.Sub(a, b), scale: scale})
}

// Less reports whether d < o.
func (d Decimal) Less(o Decimal) bool {
	return d.Compare(o) < 0
}

// Digits returns the count of significant digits: the number of digits in the
// coefficient with all leading and trailing zeros removed (zero -> 1). This
// matches DynamoDB's "significant digits" (AWS: "Leading and trailing zeroes
// are trimmed", "up to 38 significant digits") used for the precision limit
// and item-size accounting ("1 byte per two significant digits + 1 byte").
// Integer trailing zeros are NOT significant (e.g. "100" -> 1, "10" -> 1).
func (d Decimal) Digits() int {
	if d.coef.Sign() == 0 {
		return 1
	}
	n := new(big.Int).Abs(d.coef)
	rem := new(big.Int)
	for rem.Mod(n, ten).Sign() == 0 {
		n.Quo(n, ten)
	}
	return len(n.String())
}

// pow10 returns 10^n as a new big.Int (n >= 0).
func pow10(n int) *big.Int {
	return new(big.Int).Exp(ten, big.NewInt(int64(n)), nil)
}

// Validate enforces DynamoDB Number limits: at most 38 significant digits and
// magnitude within [1E-130, 9.999..9E+125] (zero is always valid). Parse
// validates grammar only; callers at the wire boundary (attrval) call
// Validate to reject out-of-range Numbers.
func (d Decimal) Validate() error {
	if d.Digits() > MaxSignificantDigits {
		return ErrTooManyDigits
	}
	if d.coef.Sign() == 0 {
		return nil
	}
	abs := Decimal{coef: new(big.Int).Abs(d.coef), scale: d.scale}
	if abs.Compare(maxDecimal) > 0 {
		return ErrNumberOverflow
	}
	if abs.Compare(minDecimal) < 0 {
		return ErrNumberUnderflow
	}
	return nil
}

// maxDecimal = 9.9999999999999999999999999999999999999E+125
// = (10^38 - 1) × 10^88, as a Decimal (coef = that integer, scale 0).
var maxDecimal = func() Decimal {
	coef := new(big.Int).Exp(ten, big.NewInt(38), nil) // 10^38
	coef.Sub(coef, big.NewInt(1))                      // 10^38 - 1
	coef.Mul(coef, new(big.Int).Exp(ten, big.NewInt(88), nil))
	return Decimal{coef: coef, scale: 0}
}()

// minDecimal = 1E-130 (coef 1, scale 130): the smallest nonzero magnitude.
var minDecimal = Decimal{coef: big.NewInt(1), scale: 130}
