package awsdynamodb_test

import (
	"reflect"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/quells-bot/ddb-sqlite-core/attrval"
	"github.com/quells-bot/ddb-sqlite-core/awsdynamodb"
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
	item, err := awsdynamodb.FromSDKMap(in)
	if err != nil {
		t.Fatalf("FromSDKMap: %v", err)
	}
	out := awsdynamodb.ToSDKMap(item)
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
	if _, err := awsdynamodb.FromSDK(&types.AttributeValueMemberN{Value: "notanumber"}); err == nil {
		t.Error("invalid number should error")
	}
}

func TestFromSDKInvalidNumberSetMember(t *testing.T) {
	if _, err := awsdynamodb.FromSDK(&types.AttributeValueMemberNS{Value: []string{"1", "bad"}}); err == nil {
		t.Error("invalid NS member should error")
	}
}

func TestToSDKNumberUsesCanonicalString(t *testing.T) {
	v, err := attrval.NewNumberString("1.50")
	if err != nil {
		t.Fatalf("NewNumberString: %v", err)
	}
	got := awsdynamodb.ToSDK(v).(*types.AttributeValueMemberN).Value
	if got != "1.5" {
		t.Errorf("got %q, want canonical 1.5", got)
	}
}
