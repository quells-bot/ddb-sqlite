package ddb_test

import (
	"context"
	"errors"
	"testing"

	"github.com/quells-bot/ddb-sqlite/pkg/ddb"
)

// expirerMock satisfies ddb.API (via the embedded interface, whose methods
// are never invoked) and ddb.Expirer.
type expirerMock struct {
	ddb.API
	count int
	err   error
}

func (m expirerMock) ExpireExpired(ctx context.Context, table string) (int, error) {
	if m.err != nil {
		return 0, m.err
	}
	return m.count, nil
}

// plainMock satisfies ddb.API but not ddb.Expirer.
type plainMock struct {
	ddb.API
}

func TestExpireExpiredDispatch(t *testing.T) {
	ctx := context.Background()
	m := &expirerMock{count: 3}

	n, err := ddb.ExpireExpired(ctx, m, "catalog")
	if err != nil {
		t.Fatalf("ExpireExpired err = %v, want nil", err)
	}
	if n != 3 {
		t.Fatalf("ExpireExpired count = %d, want 3", n)
	}
}

func TestExpireExpiredPassthroughError(t *testing.T) {
	ctx := context.Background()
	wantErr := errors.New("boom")
	m := &expirerMock{err: wantErr}

	_, err := ddb.ExpireExpired(ctx, m, "catalog")
	if !errors.Is(err, wantErr) {
		t.Fatalf("ExpireExpired err = %v, want %v", err, wantErr)
	}
}

func TestExpireExpiredUnsupported(t *testing.T) {
	ctx := context.Background()
	m := &plainMock{}

	_, err := ddb.ExpireExpired(ctx, m, "catalog")
	if err == nil {
		t.Fatal("ExpireExpired err = nil, want error for non-Expirer API")
	}
}
