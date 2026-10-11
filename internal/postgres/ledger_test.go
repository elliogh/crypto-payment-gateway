package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"crypto-payment-gateway/internal/ledger"
	"github.com/jackc/pgx/v5"
)

type rollbackFailureTx struct {
	pgx.Tx
	err    error
	called bool
}

func (tx *rollbackFailureTx) Rollback(ctx context.Context) error {
	tx.called = true
	if ctx.Err() != nil {
		return errors.New("rollback inherited cancellation")
	}
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > 5*time.Second || time.Until(deadline) <= 0 {
		return errors.New("rollback has no bounded cleanup deadline")
	}
	return tx.err
}

func TestPostingErrorRetainsRollbackFailure(t *testing.T) {
	failure := errors.New("rollback failed")
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"successful rollback", nil},
		{"already closed", pgx.ErrTxClosed},
		{"rollback failure", failure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			tx := &rollbackFailureTx{err: tc.err}
			_, err := New(nil).PostLedgerTx(ctx, &LedgerTx{tx: tx}, "", nil)
			if !tx.called || !errors.Is(err, ledger.ErrEntry) {
				t.Fatal("posting error was not retained after rollback", err)
			}
			if errors.Is(err, failure) != (tc.err == failure) || errors.Is(err, pgx.ErrTxClosed) {
				t.Fatal("rollback failure was lost or already-closed error was added", err)
			}
			if tc.err == nil && err != ledger.ErrEntry {
				t.Fatal("cleanup inherited cancellation or an incorrect deadline", err)
			}
		})
	}
}
