package ddb

import (
	"context"
	"errors"
	"testing"
)

func TestOpenClose(t *testing.T) {
	c, err := Open(context.Background(), Options{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if c == nil {
		t.Fatal("nil client")
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestErrorSentinels(t *testing.T) {
	if !errors.Is(ErrTableNotFound, ErrTableNotFound) {
		t.Error("ErrTableNotFound is not an errors.Is match with itself")
	}
	if errors.Is(ErrValidation, ErrTableNotFound) {
		t.Error("distinct sentinels collide")
	}
}
