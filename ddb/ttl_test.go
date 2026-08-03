package ddb

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/quells-bot/ddb-sqlite-core/attrval"
)

func TestUpdateTimeToLive(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	c.CreateTable(ctx, CreateTableInput{
		TableName:            "T",
		KeySchema:            []KeySchemaElement{{AttributeName: "pk", KeyType: "HASH"}},
		AttributeDefinitions: []AttributeDefinition{{AttributeName: "pk", AttributeType: "S"}},
	})

	// Enable.
	if _, err := c.UpdateTimeToLive(ctx, UpdateTimeToLiveInput{
		TableName:               "T",
		TimeToLiveSpecification: TimeToLiveSpecification{Enabled: true, AttributeName: "expire"},
	}); err != nil {
		t.Fatalf("enable: %v", err)
	}
	out, err := c.DescribeTimeToLive(ctx, DescribeTimeToLiveInput{TableName: "T"})
	if err != nil {
		t.Fatalf("describe after enable: %v", err)
	}
	if out.TimeToLiveStatus != "ENABLED" || out.AttributeName != "expire" {
		t.Errorf("after enable: status=%q attr=%q, want ENABLED/expire", out.TimeToLiveStatus, out.AttributeName)
	}

	// Idempotent: re-enable with the same attr is a no-op.
	if _, err := c.UpdateTimeToLive(ctx, UpdateTimeToLiveInput{
		TableName:               "T",
		TimeToLiveSpecification: TimeToLiveSpecification{Enabled: true, AttributeName: "expire"},
	}); err != nil {
		t.Fatalf("idempotent re-enable: %v", err)
	}

	// Re-specify a different attr while enabled overwrites.
	if _, err := c.UpdateTimeToLive(ctx, UpdateTimeToLiveInput{
		TableName:               "T",
		TimeToLiveSpecification: TimeToLiveSpecification{Enabled: true, AttributeName: "ttl"},
	}); err != nil {
		t.Fatalf("re-specify: %v", err)
	}
	out, _ = c.DescribeTimeToLive(ctx, DescribeTimeToLiveInput{TableName: "T"})
	if out.AttributeName != "ttl" {
		t.Errorf("after re-specify: attr=%q, want ttl", out.AttributeName)
	}

	// Disable.
	if _, err := c.UpdateTimeToLive(ctx, UpdateTimeToLiveInput{
		TableName:               "T",
		TimeToLiveSpecification: TimeToLiveSpecification{Enabled: false, AttributeName: "ttl"},
	}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	out, _ = c.DescribeTimeToLive(ctx, DescribeTimeToLiveInput{TableName: "T"})
	if out.TimeToLiveStatus != "DISABLED" || out.AttributeName != "" {
		t.Errorf("after disable: status=%q attr=%q, want DISABLED/empty", out.TimeToLiveStatus, out.AttributeName)
	}

	// Unknown table -> ErrTableNotFound.
	_, err = c.UpdateTimeToLive(ctx, UpdateTimeToLiveInput{
		TableName:               "nope",
		TimeToLiveSpecification: TimeToLiveSpecification{Enabled: true, AttributeName: "expire"},
	})
	if !errors.Is(err, ErrTableNotFound) {
		t.Errorf("unknown table: err = %v, want ErrTableNotFound", err)
	}

	// Empty attr name -> ErrValidation (enabling).
	_, err = c.UpdateTimeToLive(ctx, UpdateTimeToLiveInput{
		TableName:               "T",
		TimeToLiveSpecification: TimeToLiveSpecification{Enabled: true, AttributeName: ""},
	})
	if !errors.Is(err, ErrValidation) {
		t.Errorf("empty attr enabling: err = %v, want ErrValidation", err)
	}

	// Empty attr name -> ErrValidation even when disabling (required unconditionally).
	_, err = c.UpdateTimeToLive(ctx, UpdateTimeToLiveInput{
		TableName:               "T",
		TimeToLiveSpecification: TimeToLiveSpecification{Enabled: false, AttributeName: ""},
	})
	if !errors.Is(err, ErrValidation) {
		t.Errorf("empty attr disabling: err = %v, want ErrValidation", err)
	}

	// Oversized attr name -> ErrValidation.
	_, err = c.UpdateTimeToLive(ctx, UpdateTimeToLiveInput{
		TableName:               "T",
		TimeToLiveSpecification: TimeToLiveSpecification{Enabled: true, AttributeName: mustName(256)},
	})
	if !errors.Is(err, ErrValidation) {
		t.Errorf("oversized attr: err = %v, want ErrValidation", err)
	}

	// A 255-char name is accepted.
	if _, err := c.UpdateTimeToLive(ctx, UpdateTimeToLiveInput{
		TableName:               "T",
		TimeToLiveSpecification: TimeToLiveSpecification{Enabled: true, AttributeName: mustName(255)},
	}); err != nil {
		t.Errorf("255-char attr: err = %v, want nil", err)
	}

	// Precedence: table-exists error takes precedence over attr-name validation.
	_, err = c.UpdateTimeToLive(ctx, UpdateTimeToLiveInput{
		TableName:               "nope",
		TimeToLiveSpecification: TimeToLiveSpecification{Enabled: true, AttributeName: ""},
	})
	if !errors.Is(err, ErrTableNotFound) {
		t.Errorf("precedence: err = %v, want ErrTableNotFound (table-exists before spec validation)", err)
	}
}

func TestDescribeTimeToLive(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	c.CreateTable(ctx, CreateTableInput{
		TableName:            "T",
		KeySchema:            []KeySchemaElement{{AttributeName: "pk", KeyType: "HASH"}},
		AttributeDefinitions: []AttributeDefinition{{AttributeName: "pk", AttributeType: "S"}},
	})

	// Never configured -> DISABLED, empty attr.
	out, err := c.DescribeTimeToLive(ctx, DescribeTimeToLiveInput{TableName: "T"})
	if err != nil {
		t.Fatalf("describe never-set: %v", err)
	}
	if out.TimeToLiveStatus != "DISABLED" || out.AttributeName != "" {
		t.Errorf("never-set: status=%q attr=%q, want DISABLED/empty", out.TimeToLiveStatus, out.AttributeName)
	}

	// Unknown table -> ErrTableNotFound.
	_, err = c.DescribeTimeToLive(ctx, DescribeTimeToLiveInput{TableName: "nope"})
	if !errors.Is(err, ErrTableNotFound) {
		t.Errorf("unknown table: err = %v, want ErrTableNotFound", err)
	}
}

// mustName returns an n-character attribute name for validation boundary tests.
func mustName(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'a'
	}
	return string(b)
}

func TestExpireExpired(t *testing.T) {
	ctx := context.Background()

	// Injectable clock: reassigning the captured variable is visible to the closure.
	now := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	c, err := Open(ctx, Options{DSN: ":memory:", Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	c.CreateTable(ctx, CreateTableInput{
		TableName:            "T",
		KeySchema:            []KeySchemaElement{{AttributeName: "pk", KeyType: "HASH"}},
		AttributeDefinitions: []AttributeDefinition{{AttributeName: "pk", AttributeType: "S"}},
	})
	c.UpdateTimeToLive(ctx, UpdateTimeToLiveInput{
		TableName:               "T",
		TimeToLiveSpecification: TimeToLiveSpecification{Enabled: true, AttributeName: "expire"},
	})

	epoch := now.Unix()
	past := strconv.FormatInt(epoch-60, 10)   // expired
	future := strconv.FormatInt(epoch+60, 10) // not expired
	c.PutItem(ctx, PutItemInput{TableName: "T", Item: Item{
		"pk":     attrval.NewString("expired"),
		"expire": attrval.NewNumber(mustNum(past)),
	}})
	c.PutItem(ctx, PutItemInput{TableName: "T", Item: Item{
		"pk":     attrval.NewString("future"),
		"expire": attrval.NewNumber(mustNum(future)),
	}})

	// Expired items are visible on reads BEFORE ExpireExpired (Faithful).
	if got, _ := c.GetItem(ctx, GetItemInput{TableName: "T", Key: Item{"pk": attrval.NewString("expired")}}); len(got.Item) == 0 {
		t.Fatal("expired item not visible before ExpireExpired; read filtering is on")
	}

	n, err := c.ExpireExpired(ctx, "T")
	if err != nil {
		t.Fatalf("ExpireExpired: %v", err)
	}
	if n != 1 {
		t.Fatalf("deleted = %d, want 1", n)
	}

	// Expired item is gone; survivor remains.
	if got, _ := c.GetItem(ctx, GetItemInput{TableName: "T", Key: Item{"pk": attrval.NewString("expired")}}); len(got.Item) != 0 {
		t.Error("expired item still present after ExpireExpired")
	}
	if got, _ := c.GetItem(ctx, GetItemInput{TableName: "T", Key: Item{"pk": attrval.NewString("future")}}); len(got.Item) == 0 {
		t.Error("future item missing after ExpireExpired")
	}

	// Advance the clock past the survivor; it now expires.
	now = now.Add(2 * time.Minute)
	n, _ = c.ExpireExpired(ctx, "T")
	if n != 1 {
		t.Errorf("after advancing clock: deleted = %d, want 1", n)
	}
}

func TestExpireExpiredEdgeCases(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	c, err := Open(ctx, Options{DSN: ":memory:", Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	c.CreateTable(ctx, CreateTableInput{
		TableName:            "T",
		KeySchema:            []KeySchemaElement{{AttributeName: "pk", KeyType: "HASH"}},
		AttributeDefinitions: []AttributeDefinition{{AttributeName: "pk", AttributeType: "S"}},
	})
	c.UpdateTimeToLive(ctx, UpdateTimeToLiveInput{
		TableName:               "T",
		TimeToLiveSpecification: TimeToLiveSpecification{Enabled: true, AttributeName: "expire"},
	})

	epoch := now.Unix()
	cases := []struct {
		pk   string
		item Item
	}{
		{"absent", Item{"pk": attrval.NewString("absent")}},                                                                                         // no TTL attr -> kept
		{"nonnum", Item{"pk": attrval.NewString("nonnum"), "expire": attrval.NewString("not-a-number")}},                                            // kept
		{"zero", Item{"pk": attrval.NewString("zero"), "expire": attrval.NewNumber(mustNum("0"))}},                                                  // 0 <= epoch -> expired
		{"neg", Item{"pk": attrval.NewString("neg"), "expire": attrval.NewNumber(mustNum("-5"))}},                                                   // negative -> expired
		{"frac", Item{"pk": attrval.NewString("frac"), "expire": attrval.NewNumber(mustNum(strconv.FormatFloat(float64(epoch)-0.5, 'f', -1, 64)))}}, // non-integer, < epoch -> expired
		{"future", Item{"pk": attrval.NewString("future"), "expire": attrval.NewNumber(mustNum(strconv.FormatInt(epoch+60, 10)))}},                  // kept
	}
	for _, tc := range cases {
		c.PutItem(ctx, PutItemInput{TableName: "T", Item: tc.item})
	}

	n, err := c.ExpireExpired(ctx, "T")
	if err != nil {
		t.Fatalf("ExpireExpired: %v", err)
	}
	if n != 3 { // zero, neg, frac expired
		t.Errorf("deleted = %d, want 3", n)
	}
	for _, kept := range []string{"absent", "nonnum", "future"} {
		if got, _ := c.GetItem(ctx, GetItemInput{TableName: "T", Key: Item{"pk": attrval.NewString(kept)}}); len(got.Item) == 0 {
			t.Errorf("item %q should have been kept", kept)
		}
	}

	// ExpireExpired returns 0 when TTL is disabled.
	c.UpdateTimeToLive(ctx, UpdateTimeToLiveInput{
		TableName:               "T",
		TimeToLiveSpecification: TimeToLiveSpecification{Enabled: false, AttributeName: "expire"},
	})
	n, _ = c.ExpireExpired(ctx, "T")
	if n != 0 {
		t.Errorf("disabled TTL: deleted = %d, want 0", n)
	}

	// Unknown table -> ErrTableNotFound.
	_, err = c.ExpireExpired(ctx, "nope")
	if !errors.Is(err, ErrTableNotFound) {
		t.Errorf("unknown table: err = %v, want ErrTableNotFound", err)
	}
}
