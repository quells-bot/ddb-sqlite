package expr

import "fmt"

// kind identifies a token type. Keywords (AND, OR, NOT, IN, BETWEEN) and
// function names are lexed as tokName and recognized case-insensitively by the
// parser — DynamoDB's reserved-word list exists for exactly this reason, and a
// caller who needs an attribute literally named "AND" must use a #name.
type kind uint8

const (
	tokEOF kind = iota
	tokName
	tokNameRef  // #foo
	tokValueRef // :foo
	tokDot
	tokLBracket
	tokRBracket
	tokLParen
	tokRParen
	tokComma
	tokEq
	tokNe
	tokLt
	tokLe
	tokGt
	tokGe
	tokPlus  // used by the update grammar (pass 2)
	tokMinus // used by the update grammar (pass 2)
)

// token is one lexed unit. text carries the source spelling (including the
// leading # or : for refs); pos is the byte offset, used in error messages.
type token struct {
	kind kind
	text string
	pos  int
}

// isIdentByte reports whether c may appear in a bare name or in a #name/:value
// token. '-' is deliberately excluded so it can lex as tokMinus for the update
// grammar's arithmetic; an attribute name containing '-' must be supplied via
// a #name, which is DynamoDB's own requirement.
func isIdentByte(c byte) bool {
	return c == '_' ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// lex scans src into tokens, always ending with a tokEOF sentinel.
func lex(src string) ([]token, error) {
	if len(src) > maxExprString {
		return nil, fmt.Errorf("%w: expression size has exceeded the maximum allowed size", ErrLimit)
	}
	var out []token
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
			continue
		case c == '#' || c == ':':
			j := i + 1
			for j < len(src) && isIdentByte(src[j]) {
				j++
			}
			if j == i+1 {
				return nil, fmt.Errorf("%w: expected a name after %q at %d", ErrSyntax, string(c), i)
			}
			k := tokNameRef
			if c == ':' {
				k = tokValueRef
			}
			out = append(out, token{k, src[i:j], i})
			i = j
			continue
		case isIdentByte(c):
			j := i
			for j < len(src) && isIdentByte(src[j]) {
				j++
			}
			out = append(out, token{tokName, src[i:j], i})
			i = j
			continue
		}

		switch c {
		case '.':
			out = append(out, token{tokDot, ".", i})
		case '[':
			out = append(out, token{tokLBracket, "[", i})
		case ']':
			out = append(out, token{tokRBracket, "]", i})
		case '(':
			out = append(out, token{tokLParen, "(", i})
		case ')':
			out = append(out, token{tokRParen, ")", i})
		case ',':
			out = append(out, token{tokComma, ",", i})
		case '+':
			out = append(out, token{tokPlus, "+", i})
		case '-':
			out = append(out, token{tokMinus, "-", i})
		case '=':
			out = append(out, token{tokEq, "=", i})
		case '<':
			if i+1 < len(src) && src[i+1] == '>' {
				out = append(out, token{tokNe, "<>", i})
				i += 2
				continue
			}
			if i+1 < len(src) && src[i+1] == '=' {
				out = append(out, token{tokLe, "<=", i})
				i += 2
				continue
			}
			out = append(out, token{tokLt, "<", i})
		case '>':
			if i+1 < len(src) && src[i+1] == '=' {
				out = append(out, token{tokGe, ">=", i})
				i += 2
				continue
			}
			out = append(out, token{tokGt, ">", i})
		default:
			return nil, fmt.Errorf("%w: unexpected character %q at %d", ErrSyntax, string(c), i)
		}
		i++
	}
	out = append(out, token{tokEOF, "", len(src)})
	return out, nil
}
