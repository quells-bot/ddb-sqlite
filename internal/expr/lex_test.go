package expr

import (
	"errors"
	"testing"
)

func TestLex(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []token
	}{
		{
			name: "comparison with substitutions",
			src:  "#n = :v",
			want: []token{{tokNameRef, "#n", 0}, {tokEq, "=", 3}, {tokValueRef, ":v", 5}},
		},
		{
			name: "document path",
			src:  "a.b[12].c",
			want: []token{
				{tokName, "a", 0}, {tokDot, ".", 1}, {tokName, "b", 2},
				{tokLBracket, "[", 3}, {tokName, "12", 4}, {tokRBracket, "]", 6},
				{tokDot, ".", 7}, {tokName, "c", 8},
			},
		},
		{
			name: "all comparators",
			src:  "= <> < <= > >=",
			want: []token{
				{tokEq, "=", 0}, {tokNe, "<>", 2}, {tokLt, "<", 5},
				{tokLe, "<=", 7}, {tokGt, ">", 10}, {tokGe, ">=", 12},
			},
		},
		{
			name: "function call punctuation",
			src:  "contains(a, :v)",
			want: []token{
				{tokName, "contains", 0}, {tokLParen, "(", 8}, {tokName, "a", 9},
				{tokComma, ",", 10}, {tokValueRef, ":v", 12}, {tokRParen, ")", 14},
			},
		},
		{
			name: "arithmetic operators",
			src:  "a + b - c",
			want: []token{
				{tokName, "a", 0}, {tokPlus, "+", 2}, {tokName, "b", 4},
				{tokMinus, "-", 6}, {tokName, "c", 8},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := lex(tc.src)
			if err != nil {
				t.Fatalf("lex(%q): %v", tc.src, err)
			}
			// lex appends a tokEOF sentinel.
			if len(got) != len(tc.want)+1 {
				t.Fatalf("got %d tokens %v, want %d", len(got), got, len(tc.want)+1)
			}
			for i, w := range tc.want {
				if got[i] != w {
					t.Errorf("token %d = %+v, want %+v", i, got[i], w)
				}
			}
			if got[len(got)-1].kind != tokEOF {
				t.Errorf("last token = %+v, want tokEOF", got[len(got)-1])
			}
		})
	}
}

func TestLexErrors(t *testing.T) {
	cases := []struct{ name, src string }{
		{"bare hash", "# = :v"},
		{"bare colon", "a = :"},
		{"unknown character", "a $ b"},
		{"lone bang", "a ! b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := lex(tc.src); !errors.Is(err, ErrSyntax) {
				t.Errorf("lex(%q) err = %v, want ErrSyntax", tc.src, err)
			}
		})
	}
}
