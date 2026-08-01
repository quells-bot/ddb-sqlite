package expr

import (
	"errors"
	"testing"

	"github.com/quells-bot/ddb-sqlite/attrval"
)

func mustBindCond(t *testing.T, src string, env Env) *BoundCondition {
	t.Helper()
	c := mustParse(t, src)
	b, err := c.Bind(env)
	if err != nil {
		t.Fatalf("Bind(%q): %v", src, err)
	}
	return b
}

func TestExtractKeyConditionPartitionOnly(t *testing.T) {
	env := Env{Values: map[string]attrval.Value{":v": attrval.NewString("p1")}}
	b := mustBindCond(t, "pk = :v", env)
	kc, err := b.ExtractKeyCondition("pk", "sk")
	if err != nil {
		t.Fatalf("ExtractKeyCondition: %v", err)
	}
	if kc.Partition.Name != "pk" {
		t.Errorf("Partition.Name = %q, want pk", kc.Partition.Name)
	}
	if kc.Partition.Value.Str() != "p1" {
		t.Errorf("Partition.Value = %q, want p1", kc.Partition.Value.Str())
	}
	if kc.Sort != nil {
		t.Errorf("Sort = %v, want nil", kc.Sort)
	}
}

func TestExtractKeyConditionValid(t *testing.T) {
	dec, _ := attrval.NewNumberString("5")
	cases := []struct {
		name   string
		src    string
		env    Env
		pkName string
		skName string
		wantOp string // "" means no sort
	}{
		{"partition only", "pk = :v", Env{Values: map[string]attrval.Value{":v": attrval.NewString("p1")}}, "pk", "sk", ""},
		{"pk AND sk=", "pk = :v AND sk = :s", Env{Values: map[string]attrval.Value{":v": attrval.NewString("p1"), ":s": dec}}, "pk", "sk", "="},
		{"sk AND pk (reversed order)", "sk = :s AND pk = :v", Env{Values: map[string]attrval.Value{":v": attrval.NewString("p1"), ":s": dec}}, "pk", "sk", "="},
		{"pk AND sk<", "pk = :v AND sk < :s", Env{Values: map[string]attrval.Value{":v": attrval.NewString("p1"), ":s": dec}}, "pk", "sk", "<"},
		{"pk AND sk<=", "pk = :v AND sk <= :s", Env{Values: map[string]attrval.Value{":v": attrval.NewString("p1"), ":s": dec}}, "pk", "sk", "<="},
		{"pk AND sk>", "pk = :v AND sk > :s", Env{Values: map[string]attrval.Value{":v": attrval.NewString("p1"), ":s": dec}}, "pk", "sk", ">"},
		{"pk AND sk>=", "pk = :v AND sk >= :s", Env{Values: map[string]attrval.Value{":v": attrval.NewString("p1"), ":s": dec}}, "pk", "sk", ">="},
		{"pk AND BETWEEN", "pk = :v AND sk BETWEEN :lo AND :hi", Env{Values: map[string]attrval.Value{":v": attrval.NewString("p1"), ":lo": dec, ":hi": dec}}, "pk", "sk", "BETWEEN"},
		{"pk AND begins_with", "pk = :v AND begins_with(sk, :p)", Env{Values: map[string]attrval.Value{":v": attrval.NewString("p1"), ":p": attrval.NewString("pre")}}, "pk", "sk", "BEGINS_WITH"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := mustBindCond(t, tc.src, tc.env)
			kc, err := b.ExtractKeyCondition(tc.pkName, tc.skName)
			if err != nil {
				t.Fatalf("ExtractKeyCondition: %v", err)
			}
			if kc.Partition.Name != tc.pkName {
				t.Errorf("Partition.Name = %q, want %q", kc.Partition.Name, tc.pkName)
			}
			if tc.wantOp == "" {
				if kc.Sort != nil {
					t.Errorf("Sort = %v, want nil", kc.Sort)
				}
			} else {
				if kc.Sort == nil {
					t.Fatalf("Sort = nil, want op %q", tc.wantOp)
				}
				if kc.Sort.Op != tc.wantOp {
					t.Errorf("Sort.Op = %q, want %q", kc.Sort.Op, tc.wantOp)
				}
			}
		})
	}
}

func TestExtractKeyConditionRejected(t *testing.T) {
	dec, _ := attrval.NewNumberString("5")
	env := Env{Values: map[string]attrval.Value{
		":v": attrval.NewString("p1"), ":s": dec, ":lo": dec, ":hi": dec,
		":p": attrval.NewString("pre"), ":w": attrval.NewString("p2"),
	}}
	cases := []struct {
		name string
		src  string
	}{
		{"OR", "pk = :v OR sk = :s"},
		{"two partition conditions", "pk = :v AND pk = :w"},
		{"NOT", "NOT pk = :v"},
		{"IN", "sk IN (:s, :lo)"},
		{"non-equality on PK", "pk > :v"},
		{"two sort conditions", "pk = :v AND sk = :s AND sk = :lo"},
		{"attribute_exists", "attribute_exists(sk)"},
		{"attribute_not_exists", "attribute_not_exists(sk)"},
		{"attribute_type", "attribute_type(sk, :s)"},
		{"contains", "contains(sk, :s)"},
		{"size()", "size(sk) = :s"},
		{"path on neither key", "extra = :v"},
		{"begins_with with path arg", "begins_with(sk, pk)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := mustBindCond(t, tc.src, env)
			_, err := b.ExtractKeyCondition("pk", "sk")
			if !errors.Is(err, ErrSemantic) {
				t.Errorf("ExtractKeyCondition(%q): err = %v, want ErrSemantic", tc.src, err)
			}
		})
	}
}

func TestExtractKeyConditionSortOnPartitionOnlyTable(t *testing.T) {
	dec, _ := attrval.NewNumberString("5")
	env := Env{Values: map[string]attrval.Value{":v": attrval.NewString("p1"), ":s": dec}}
	b := mustBindCond(t, "pk = :v AND sk = :s", env)
	_, err := b.ExtractKeyCondition("pk", "") // no sort key
	if !errors.Is(err, ErrSemantic) {
		t.Errorf("err = %v, want ErrSemantic (sort cond on partition-only table)", err)
	}
}
