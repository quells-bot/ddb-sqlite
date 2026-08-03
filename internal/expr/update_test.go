package expr

import (
	"errors"
	"testing"

	"github.com/quells-bot/ddb-sqlite/attrval"
)

// applyWith parses, binds, and applies src against item. It returns the new
// item, the touched attribute metadata, and the first error from any phase.
func applyWith(t *testing.T, src string, item map[string]attrval.Value, values map[string]attrval.Value) (map[string]attrval.Value, []TouchedAttribute, error) {
	t.Helper()
	return applyWithEnv(t, src, item, nil, values)
}

func applyWithEnv(t *testing.T, src string, item map[string]attrval.Value, names map[string]string, values map[string]attrval.Value) (map[string]attrval.Value, []TouchedAttribute, error) {
	t.Helper()
	u, err := ParseUpdate(src)
	if err != nil {
		return nil, nil, err
	}
	b, err := u.Bind(Env{Names: names, Values: values})
	if err != nil {
		return nil, nil, err
	}
	return b.Apply(item)
}

// mustApply fails the test if any phase errors.
func mustApply(t *testing.T, src string, item map[string]attrval.Value, values map[string]attrval.Value) (map[string]attrval.Value, []TouchedAttribute) {
	t.Helper()
	out, touched, err := applyWith(t, src, item, values)
	if err != nil {
		t.Fatalf("apply %q: %v", src, err)
	}
	return out, touched
}

func TestApplySet(t *testing.T) {
	item := fixtureItem(t)
	values := map[string]attrval.Value{
		":x":   attrval.NewString("new"),
		":one": mustNumber(t, "1"),
		":lst": attrval.NewList([]attrval.Value{attrval.NewString("z")}),
	}

	t.Run("overwrite a scalar", func(t *testing.T) {
		out, touched := mustApply(t, "SET s = :x", item, values)
		if got := out["s"].Str(); got != "new" {
			t.Errorf("s = %q, want new", got)
		}
		if len(touched) != 1 || touched[0].Name != "s" {
			t.Errorf("touched = %v, want [s]", touched)
		}
		if item["s"].Str() != "hello" {
			t.Errorf("Apply mutated the original item: s = %q", item["s"].Str())
		}
	})

	t.Run("create a new attribute", func(t *testing.T) {
		out, _ := mustApply(t, "SET fresh = :x", item, values)
		if got, ok := out["fresh"]; !ok || got.Str() != "new" {
			t.Errorf("fresh = %v, %v", got, ok)
		}
	})

	t.Run("nested path", func(t *testing.T) {
		out, touched := mustApply(t, "SET m.deeper = :x", item, values)
		v, ok := out["m"].Lookup(attrval.Path{{Name: "deeper"}})
		if !ok || v.Str() != "new" {
			t.Errorf("m.deeper = %v, %v", v, ok)
		}
		// The sibling entry survives; only the touched spine is copied.
		if _, ok := out["m"].Lookup(attrval.Path{{Name: "inner"}}); !ok {
			t.Error("m.inner was lost")
		}
		if len(touched) != 1 || touched[0].Name != "m" {
			t.Errorf("touched = %v, want [m]", touched)
		}
	})

	t.Run("list index append at len", func(t *testing.T) {
		out, _ := mustApply(t, "SET l[2] = :x", item, values)
		if got := len(out["l"].List()); got != 3 {
			t.Errorf("len(l) = %d, want 3", got)
		}
	})

	t.Run("copy from another path", func(t *testing.T) {
		// "copy" is a DynamoDB reserved word; "dup" is not.
		out, _ := mustApply(t, "SET dup = s", item, nil)
		if got := out["dup"].Str(); got != "hello" {
			t.Errorf("dup = %q, want hello", got)
		}
	})

	t.Run("arithmetic", func(t *testing.T) {
		out, _ := mustApply(t, "SET n = n + :one", item, values)
		if got := out["n"].Num().String(); got != "43" {
			t.Errorf("n = %s, want 43", got)
		}
		out, _ = mustApply(t, "SET n = n - :one", item, values)
		if got := out["n"].Num().String(); got != "41" {
			t.Errorf("n = %s, want 41", got)
		}
	})

	t.Run("if_not_exists", func(t *testing.T) {
		out, _ := mustApply(t, "SET s = if_not_exists(s, :x)", item, values)
		if got := out["s"].Str(); got != "hello" {
			t.Errorf("s = %q, want the existing hello", got)
		}
		out, _ = mustApply(t, "SET fresh = if_not_exists(nope, :x)", item, values)
		if got := out["fresh"].Str(); got != "new" {
			t.Errorf("fresh = %q, want new", got)
		}
		// A present NULL counts as existing. "null" is a reserved word, so the
		// fixture's NULL attribute is reached through a #name alias.
		out2, _, err := applyWithEnv(t, "SET fresh = if_not_exists(#z, :x)", item, map[string]string{"#z": "null"}, values)
		if err != nil {
			t.Fatalf("if_not_exists on a NULL: %v", err)
		}
		if got := out2["fresh"].Type(); got != "NULL" {
			t.Errorf("fresh = %s, want NULL (a present NULL exists)", got)
		}
	})

	t.Run("list_append", func(t *testing.T) {
		out, _ := mustApply(t, "SET l = list_append(l, :lst)", item, values)
		got := out["l"].List()
		if len(got) != 3 || got[2].Str() != "z" {
			t.Errorf("l = %v, want the original two plus z", got)
		}
	})

	t.Run("actions read the original item", func(t *testing.T) {
		swapItem := map[string]attrval.Value{
			"a": attrval.NewString("A"),
			"b": attrval.NewString("B"),
		}
		out, touched := mustApply(t, "SET a = b, b = a", swapItem, nil)
		if out["a"].Str() != "B" || out["b"].Str() != "A" {
			t.Errorf("swap produced a=%q b=%q, want a=B b=A", out["a"].Str(), out["b"].Str())
		}
		if len(touched) != 2 {
			t.Errorf("touched = %v, want two names", touched)
		}
	})
}

func TestApplyRemove(t *testing.T) {
	item := fixtureItem(t)

	out, touched := mustApply(t, "REMOVE s", item, nil)
	if _, ok := out["s"]; ok {
		t.Error("s survived REMOVE")
	}
	if len(touched) != 1 || touched[0].Name != "s" {
		t.Errorf("touched = %v, want [s]", touched)
	}

	// A list index removes and shifts.
	out, _ = mustApply(t, "REMOVE l[0]", item, nil)
	got := out["l"].List()
	if len(got) != 1 || got[0].Num().String() != "7" {
		t.Errorf("l = %v, want the second element shifted down", got)
	}

	// A missing path is a silent no-op.
	if _, _, err := applyWith(t, "REMOVE nope", item, nil); err != nil {
		t.Errorf("REMOVE of a missing attribute: %v", err)
	}

	// Emptied containers are not pruned. Both map keys are DynamoDB reserved
	// words (INNER, LIST), so they need #name aliases.
	out, _, err := applyWithEnv(t, "REMOVE m.#i, m.#lst", item, map[string]string{"#i": "inner", "#lst": "list"}, nil)
	if err != nil {
		t.Fatalf("REMOVE m.#i, m.#lst: %v", err)
	}
	m, ok := out["m"]
	if !ok || m.Type() != "M" || len(m.Map()) != 0 {
		t.Errorf("m = %v, want an empty map (no pruning)", m)
	}
}

func TestApplyAdd(t *testing.T) {
	item := fixtureItem(t)
	values := map[string]attrval.Value{
		":one": mustNumber(t, "1"),
		":ss":  attrval.NewStringSet([]string{"b", "c"}),
		":str": attrval.NewString("x"),
	}

	t.Run("number on an existing attribute", func(t *testing.T) {
		out, _ := mustApply(t, "ADD n :one", item, values)
		if got := out["n"].Num().String(); got != "43" {
			t.Errorf("n = %s, want 43", got)
		}
	})

	t.Run("number on a missing attribute starts at zero", func(t *testing.T) {
		out, _ := mustApply(t, "ADD fresh :one", item, values)
		if got := out["fresh"].Num().String(); got != "1" {
			t.Errorf("fresh = %s, want 1", got)
		}
	})

	t.Run("set union", func(t *testing.T) {
		out, _ := mustApply(t, "ADD ss :ss", item, values)
		got := out["ss"].SS()
		if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
			t.Errorf("ss = %v, want [a b c] deduped and sorted", got)
		}
	})

	t.Run("set on a missing attribute creates it", func(t *testing.T) {
		out, _ := mustApply(t, "ADD fresh :ss", item, values)
		if got := out["fresh"].Type(); got != "SS" {
			t.Errorf("fresh = %s, want SS", got)
		}
	})

	cases := []struct {
		name string
		src  string
	}{
		{"number onto a string", "ADD s :one"},
		{"set onto a string", "ADD s :ss"},
		{"mismatched set types", "ADD ns :ss"},
		{"unsupported value type", "ADD fresh :str"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := applyWith(t, tc.src, item, values); !errors.Is(err, ErrSemantic) {
				t.Errorf("apply %q err = %v, want ErrSemantic", tc.src, err)
			}
		})
	}
}

func TestApplyDelete(t *testing.T) {
	item := fixtureItem(t)
	values := map[string]attrval.Value{
		":a":    attrval.NewStringSet([]string{"a"}),
		":both": attrval.NewStringSet([]string{"a", "b"}),
		":str":  attrval.NewString("a"),
	}

	t.Run("removes listed elements", func(t *testing.T) {
		out, _ := mustApply(t, "DELETE ss :a", item, values)
		got := out["ss"].SS()
		if len(got) != 1 || got[0] != "b" {
			t.Errorf("ss = %v, want [b]", got)
		}
	})

	t.Run("emptying the set removes the attribute", func(t *testing.T) {
		out, touched := mustApply(t, "DELETE ss :both", item, values)
		if _, ok := out["ss"]; ok {
			t.Error("an emptied set must be removed: DynamoDB has no empty-set representation")
		}
		if len(touched) != 1 || touched[0].Name != "ss" {
			t.Errorf("touched = %v, want [ss]", touched)
		}
	})

	t.Run("missing attribute is a no-op", func(t *testing.T) {
		if _, _, err := applyWith(t, "DELETE nope :a", item, values); err != nil {
			t.Errorf("DELETE on a missing attribute: %v", err)
		}
	})

	cases := []struct {
		name string
		src  string
	}{
		{"non-set value", "DELETE ss :str"},
		{"attribute is not a set", "DELETE s :a"},
		{"set type mismatch", "DELETE ns :a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := applyWith(t, tc.src, item, values); !errors.Is(err, ErrSemantic) {
				t.Errorf("apply %q err = %v, want ErrSemantic", tc.src, err)
			}
		})
	}
}

func TestApplyErrors(t *testing.T) {
	item := fixtureItem(t)
	values := map[string]attrval.Value{
		":x":   attrval.NewString("new"),
		":one": mustNumber(t, "1"),
	}
	cases := []struct {
		name string
		src  string
	}{
		{"SET from a missing path", "SET fresh = nope"},
		{"arithmetic on a missing operand", "SET fresh = nope + :one"},
		{"arithmetic on a non-number", "SET fresh = s + :one"},
		{"list_append on a non-list", "SET fresh = list_append(s, s)"},
		{"nested SET through a scalar", "SET s.deeper = :x"},
		{"SET through a missing parent", "SET nope.x = :x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := applyWith(t, tc.src, item, values); err == nil {
				t.Errorf("apply %q err = nil, want an error", tc.src)
			}
		})
	}
}

func TestApplyOnAbsentItem(t *testing.T) {
	// Upsert: ddb passes a key-only item; every action still applies.
	base := map[string]attrval.Value{"pk": attrval.NewString("k")}
	values := map[string]attrval.Value{":one": mustNumber(t, "1")}
	out, touched := mustApply(t, "ADD hits :one", base, values)
	if got := out["hits"].Num().String(); got != "1" {
		t.Errorf("hits = %s, want 1", got)
	}
	if out["pk"].Str() != "k" {
		t.Error("the key attribute was lost")
	}
	if len(touched) != 1 || touched[0].Name != "hits" {
		t.Errorf("touched = %v, want [hits]", touched)
	}
}
