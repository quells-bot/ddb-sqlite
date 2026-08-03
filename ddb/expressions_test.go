package ddb

import (
	"errors"
	"testing"

	"github.com/quells-bot/ddb-sqlite-core/attrval"
	"github.com/quells-bot/ddb-sqlite-core/internal/expr"
)

func TestPrepareCondition(t *testing.T) {
	cases := []struct {
		name    string
		req     expressionRequest
		wantNil bool
		wantErr bool
	}{
		{
			name:    "no expression and no maps",
			req:     expressionRequest{},
			wantNil: true,
		},
		{
			name: "valid condition",
			req: expressionRequest{
				Condition: "#n = :v",
				Names:     map[string]string{"#n": "attr"},
				Values:    map[string]attrval.Value{":v": attrval.NewString("x")},
			},
		},
		{
			name:    "malformed expression",
			req:     expressionRequest{Condition: "a ="},
			wantErr: true,
		},
		{
			name: "undefined name",
			req: expressionRequest{
				Condition: "#n = :v",
				Values:    map[string]attrval.Value{":v": attrval.NewString("x")},
			},
			wantErr: true,
		},
		{
			name: "unused value",
			req: expressionRequest{
				Condition: "attribute_exists(a)",
				Values:    map[string]attrval.Value{":v": attrval.NewString("x")},
			},
			wantErr: true,
		},
		{
			name: "maps supplied with no expression at all",
			req: expressionRequest{
				Names: map[string]string{"#n": "attr"},
			},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := prepareExpressions(tc.req)
			if tc.wantErr {
				if !errors.Is(err, ErrValidation) {
					t.Fatalf("err = %v, want ErrValidation", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("prepareExpressions: %v", err)
			}
			if tc.wantNil && got.Cond != nil {
				t.Errorf("got %v, want nil bound condition", got.Cond)
			}
			if !tc.wantNil && got.Cond == nil {
				t.Error("got nil, want a bound condition")
			}
		})
	}
}

func TestCheckCondition(t *testing.T) {
	item := Item{"a": attrval.NewString("x")}

	t.Run("nil condition always passes", func(t *testing.T) {
		if err := checkCondition(nil, item, ""); err != nil {
			t.Errorf("err = %v, want nil", err)
		}
	})

	t.Run("satisfied condition passes", func(t *testing.T) {
		ex, err := prepareExpressions(expressionRequest{Condition: "attribute_exists(a)"})
		if err != nil {
			t.Fatalf("prepareExpressions: %v", err)
		}
		if err := checkCondition(ex.Cond, item, ""); err != nil {
			t.Errorf("err = %v, want nil", err)
		}
	})

	t.Run("failed condition yields ErrConditionalCheck", func(t *testing.T) {
		ex, err := prepareExpressions(expressionRequest{Condition: "attribute_not_exists(a)"})
		if err != nil {
			t.Fatalf("prepareExpressions: %v", err)
		}
		err = checkCondition(ex.Cond, item, "")
		if !errors.Is(err, ErrConditionalCheck) {
			t.Fatalf("err = %v, want ErrConditionalCheck", err)
		}
		var ccf *ConditionalCheckFailedError
		if !errors.As(err, &ccf) {
			t.Fatalf("err = %v, want *ConditionalCheckFailedError", err)
		}
		if ccf.Item != nil {
			t.Errorf("Item = %v, want nil when ALL_OLD was not requested", ccf.Item)
		}
	})

	t.Run("ALL_OLD populates the item", func(t *testing.T) {
		ex, err := prepareExpressions(expressionRequest{Condition: "attribute_not_exists(a)"})
		if err != nil {
			t.Fatalf("prepareExpressions: %v", err)
		}
		err = checkCondition(ex.Cond, item, "ALL_OLD")
		var ccf *ConditionalCheckFailedError
		if !errors.As(err, &ccf) {
			t.Fatalf("err = %v, want *ConditionalCheckFailedError", err)
		}
		if len(ccf.Item) != 1 || ccf.Item["a"].Str() != "x" {
			t.Errorf("Item = %v, want the pre-write item", ccf.Item)
		}
	})
}

func TestValidateReturnValuesOldOnly(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "", want: returnValuesNone},
		{in: "NONE", want: returnValuesNone},
		{in: "ALL_OLD", want: returnValuesAllOld},
		{in: "all_old", wantErr: true},
		{in: "ALL_NEW", wantErr: true},
		{in: "UPDATED_OLD", wantErr: true},
		{in: "bogus", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := validateReturnValuesOldOnly(tc.in)
			if tc.wantErr {
				if !errors.Is(err, ErrValidation) {
					t.Errorf("err = %v, want ErrValidation", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestValidateReturnValuesOnConditionCheckFailure(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "", want: returnValuesNone},
		{in: "NONE", want: returnValuesNone},
		{in: "ALL_OLD", want: returnValuesAllOld},
		{in: "all_old", wantErr: true},
		{in: "ALL_NEW", wantErr: true},
		{in: "UPDATED_OLD", wantErr: true},
		{in: "NOPE", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := validateReturnValuesOnConditionCheckFailure(tc.in)
			if tc.wantErr {
				if !errors.Is(err, ErrValidation) {
					t.Errorf("err = %v, want ErrValidation", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPrepareExpressionsUnionsRefs(t *testing.T) {
	// A #name used only by the update and a :value used only by the condition
	// must both count as used: DynamoDB validates unused entries across ALL
	// expressions on a request jointly (spec §4.5).
	r := expressionRequest{
		Condition: "attribute_exists(a)",
		Update:    "SET #u = :uv",
		Names:     map[string]string{"#u": "updated"},
		Values:    map[string]attrval.Value{":uv": attrval.NewString("x")},
	}
	got, err := prepareExpressions(r)
	if err != nil {
		t.Fatalf("prepareExpressions: %v", err)
	}
	if got.Cond == nil {
		t.Error("Cond = nil, want a bound condition")
	}
	if got.Update == nil {
		t.Error("Update = nil, want a bound update")
	}
}

func TestPrepareExpressionsErrors(t *testing.T) {
	cases := []struct {
		name string
		req  expressionRequest
	}{
		{"malformed update", expressionRequest{Update: "SET a"}},
		{"undefined update value", expressionRequest{Update: "SET a = :missing"}},
		{"undefined update name", expressionRequest{Update: "SET #missing = :v", Values: map[string]attrval.Value{":v": attrval.NewString("x")}}},
		{
			"unused entry referenced by neither expression",
			expressionRequest{
				Condition: "attribute_exists(a)",
				Update:    "REMOVE b",
				Values:    map[string]attrval.Value{":spare": attrval.NewString("x")},
			},
		},
		{"overlapping update paths", expressionRequest{Update: "SET a = :v REMOVE a", Values: map[string]attrval.Value{":v": attrval.NewString("x")}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := prepareExpressions(tc.req); !errors.Is(err, ErrValidation) {
				t.Errorf("prepareExpressions err = %v, want ErrValidation", err)
			}
		})
	}
}

func TestValidateReturnValuesUpdate(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", returnValuesNone, false},
		{"NONE", returnValuesNone, false},
		{"ALL_OLD", returnValuesAllOld, false},
		{"ALL_NEW", returnValuesAllNew, false},
		{"UPDATED_OLD", returnValuesUpdatedOld, false},
		{"UPDATED_NEW", returnValuesUpdatedNew, false},
		{"all_new", "", true},
		{"NOPE", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := validateReturnValuesUpdate(tc.in)
			if tc.wantErr {
				if !errors.Is(err, ErrValidation) {
					t.Errorf("err = %v, want ErrValidation", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
	// PutItem/DeleteItem still reject the update-only modes.
	if _, err := validateReturnValuesOldOnly("ALL_NEW"); !errors.Is(err, ErrValidation) {
		t.Errorf("validateReturnValuesOldOnly(ALL_NEW) err = %v, want ErrValidation", err)
	}
}

func TestProjectReturnValues(t *testing.T) {
	old := Item{"pk": attrval.NewString("k"), "a": attrval.NewString("old"), "b": attrval.NewString("keep")}
	updated := Item{"pk": attrval.NewString("k"), "a": attrval.NewString("new"), "b": attrval.NewString("keep")}
	touched := []expr.TouchedAttribute{{Name: "a", OldExisted: true, Modified: true}, {Name: "gone", OldExisted: false, Modified: true}}

	cases := []struct {
		mode      string
		wantNil   bool
		wantAttrs map[string]string
	}{
		{returnValuesNone, true, nil},
		{returnValuesAllOld, false, map[string]string{"pk": "k", "a": "old", "b": "keep"}},
		{returnValuesAllNew, false, map[string]string{"pk": "k", "a": "new", "b": "keep"}},
		{returnValuesUpdatedOld, false, map[string]string{"a": "old"}},
		{returnValuesUpdatedNew, false, map[string]string{"a": "new"}},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			got := projectReturnValues(tc.mode, old, updated, touched)
			if tc.wantNil {
				if got != nil {
					t.Errorf("got %v, want nil", got)
				}
				return
			}
			if len(got) != len(tc.wantAttrs) {
				t.Fatalf("got %v, want %v", got, tc.wantAttrs)
			}
			for k, want := range tc.wantAttrs {
				if v, ok := got[k]; !ok || v.Str() != want {
					t.Errorf("%s = %v, want %q", k, v, want)
				}
			}
		})
	}

	// ALL_OLD on an item that did not exist yields no Attributes.
	if got := projectReturnValues(returnValuesAllOld, nil, updated, touched); got != nil {
		t.Errorf("ALL_OLD on an absent item = %v, want nil", got)
	}
	// UPDATED_NEW omits attributes the update removed.
	if got := projectReturnValues(returnValuesUpdatedNew, old, Item{"pk": attrval.NewString("k")}, []expr.TouchedAttribute{{Name: "a", OldExisted: true, Modified: false}}); got != nil {
		t.Errorf("UPDATED_NEW after a REMOVE = %v, want nil", got)
	}
}

func TestPrepareExpressionsProjection(t *testing.T) {
	// A #name referenced only by the projection is not reported unused.
	ex, err := prepareExpressions(expressionRequest{
		Projection: "#t, obj.a",
		Names:      map[string]string{"#t": "top"},
	})
	if err != nil {
		t.Fatalf("projection-only names: %v", err)
	}
	if ex.Proj == nil {
		t.Fatal("Proj = nil, want bound projection")
	}
	if len(ex.Proj.Paths()) != 2 {
		t.Errorf("len(Paths) = %d, want 2", len(ex.Proj.Paths()))
	}

	// A #name no expression references is still rejected.
	if _, err := prepareExpressions(expressionRequest{
		Projection: "top",
		Names:      map[string]string{"#x": "other"},
	}); !errors.Is(err, ErrValidation) {
		t.Errorf("unused name: err = %v, want ErrValidation", err)
	}

	// Refs union across expressions: condition's #c/:v plus projection's #p.
	ex, err = prepareExpressions(expressionRequest{
		Condition:  "#c = :v",
		Projection: "#p",
		Names:      map[string]string{"#c": "a", "#p": "b"},
		Values:     map[string]attrval.Value{":v": attrval.NewString("x")},
	})
	if err != nil {
		t.Fatalf("joint refs: %v", err)
	}
	if ex.Cond == nil || ex.Proj == nil {
		t.Error("Cond and Proj should both be bound")
	}

	// Parse and overlap failures surface as ErrValidation.
	if _, err := prepareExpressions(expressionRequest{Projection: "a = :v"}); !errors.Is(err, ErrValidation) {
		t.Errorf("bad projection: err = %v, want ErrValidation", err)
	}
	if _, err := prepareExpressions(expressionRequest{Projection: "obj, obj.a"}); !errors.Is(err, ErrValidation) {
		t.Errorf("overlap: err = %v, want ErrValidation", err)
	}
}
