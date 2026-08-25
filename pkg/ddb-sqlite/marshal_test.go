package ddbsqlite_test

import (
	"reflect"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/quells-bot/ddb-sqlite-core/attrval"
	"github.com/quells-bot/ddb-sqlite/pkg/ddb-sqlite"
)

func TestRoundTripAllTags(t *testing.T) {
	in := map[string]types.AttributeValue{
		"s":  &types.AttributeValueMemberS{Value: "hi"},
		"n":  &types.AttributeValueMemberN{Value: "12.5"},
		"b":  &types.AttributeValueMemberB{Value: []byte{0, 255}},
		"bl": &types.AttributeValueMemberBOOL{Value: true},
		"nl": &types.AttributeValueMemberNULL{Value: true},
		"l":  &types.AttributeValueMemberL{Value: []types.AttributeValue{&types.AttributeValueMemberS{Value: "x"}}},
		"m":  &types.AttributeValueMemberM{Value: map[string]types.AttributeValue{"i": &types.AttributeValueMemberS{Value: "v"}}},
		"ss": &types.AttributeValueMemberSS{Value: []string{"a", "b"}},
		"ns": &types.AttributeValueMemberNS{Value: []string{"1", "2"}},
		"bs": &types.AttributeValueMemberBS{Value: [][]byte{{1}, {2}}},
	}
	item, err := ddbsqlite.FromSDKMap(in)
	if err != nil {
		t.Fatalf("FromSDKMap: %v", err)
	}
	out := ddbsqlite.ToSDKMap(item)
	if got := out["s"].(*types.AttributeValueMemberS).Value; got != "hi" {
		t.Errorf("s = %q", got)
	}
	if got := out["n"].(*types.AttributeValueMemberN).Value; got != "12.5" {
		t.Errorf("n = %q", got)
	}
	if got := out["bl"].(*types.AttributeValueMemberBOOL).Value; got != true {
		t.Errorf("bl = %v", got)
	}
	if got, want := out["ss"].(*types.AttributeValueMemberSS).Value, []string{"a", "b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ss = %v", got)
	}
	if got := out["ns"].(*types.AttributeValueMemberNS).Value; !reflect.DeepEqual(got, []string{"1", "2"}) {
		t.Errorf("ns = %v", got)
	}
}

func TestFromSDKInvalidNumber(t *testing.T) {
	if _, err := ddbsqlite.FromSDK(&types.AttributeValueMemberN{Value: "notanumber"}); err == nil {
		t.Error("invalid number should error")
	}
}

func TestFromSDKInvalidNumberSetMember(t *testing.T) {
	if _, err := ddbsqlite.FromSDK(&types.AttributeValueMemberNS{Value: []string{"1", "bad"}}); err == nil {
		t.Error("invalid NS member should error")
	}
}
func TestFromSDKNilMembersError(t *testing.T) {
	cases := map[string]types.AttributeValue{
		"s":  (*types.AttributeValueMemberS)(nil),
		"n":  (*types.AttributeValueMemberN)(nil),
		"b":  (*types.AttributeValueMemberB)(nil),
		"bl": (*types.AttributeValueMemberBOOL)(nil),
		"nl": (*types.AttributeValueMemberNULL)(nil),
		"l":  (*types.AttributeValueMemberL)(nil),
		"m":  (*types.AttributeValueMemberM)(nil),
		"ss": (*types.AttributeValueMemberSS)(nil),
		"ns": (*types.AttributeValueMemberNS)(nil),
		"bs": (*types.AttributeValueMemberBS)(nil),
	}
	for name, av := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ddbsqlite.FromSDK(av); err == nil {
				t.Errorf("FromSDK(%s) = nil error, want error for typed-nil member", name)
			}
		})
	}
}

func TestFromSDKMapTypedNilMember(t *testing.T) {
	in := map[string]types.AttributeValue{
		"ok":  &types.AttributeValueMemberS{Value: "fine"},
		"nil": (*types.AttributeValueMemberN)(nil),
	}
	if _, err := ddbsqlite.FromSDKMap(in); err == nil {
		t.Error("FromSDKMap with a typed-nil member should error, not panic")
	}
}

func TestFromSDKListTypedNilElement(t *testing.T) {
	in := &types.AttributeValueMemberL{Value: []types.AttributeValue{
		&types.AttributeValueMemberS{Value: "x"},
		(*types.AttributeValueMemberBOOL)(nil),
	}}
	if _, err := ddbsqlite.FromSDK(in); err == nil {
		t.Error("FromSDK with a typed-nil list element should error, not panic")
	}
}

func TestToSDKNumberUsesCanonicalString(t *testing.T) {
	v, err := attrval.NewNumberString("1.50")
	if err != nil {
		t.Fatalf("NewNumberString: %v", err)
	}
	got := ddbsqlite.ToSDK(v).(*types.AttributeValueMemberN).Value
	if got != "1.5" {
		t.Errorf("got %q, want canonical 1.5", got)
	}
}
