package integration

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"crypto-payment-gateway/internal/invoice"
	"crypto-payment-gateway/internal/ledger"
	"crypto-payment-gateway/internal/money"
	"crypto-payment-gateway/internal/postgres"
	"github.com/jackc/pgx/v5"
)

func fundedInvoice(t *testing.T, s *databaseFixture, amount int64) (invoice.Invoice, map[ledger.AccountKind]string) {
	t.Helper()
	v, err := s.CreateInvoice(context.Background(), postgres.AlphaID, "funded", 1)
	if err != nil {
		t.Fatal(err)
	}
	a := accounts(t, s, v)
	if _, err := s.PostLedger(context.Background(), "funding", []ledger.Entry{
		{AccountID: a[ledger.Asset], Side: ledger.Debit, Amount: money.USDT(amount)},
		{AccountID: a[ledger.Hold], Side: ledger.Credit, Amount: money.USDT(amount)},
	}); err != nil {
		t.Fatal(err)
	}
	return v, a
}

func TestBalanceFailureRollsBackInvoiceAndJournal(t *testing.T) {
	s := fixture(t)
	v, a := fundedInvoice(t, s, 100)
	ctx := context.Background()
	tx, err := s.BeginLedgerTx(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `UPDATE invoices SET status='PAID' WHERE id=$1`, v.ID); err != nil {
		t.Fatal(err)
	}
	_, err = s.PostLedgerTx(ctx, tx, "overspend", []ledger.Entry{
		{AccountID: a[ledger.Hold], Side: ledger.Debit, Amount: 101},
		{AccountID: a[ledger.PartnerAvailable], Side: ledger.Credit, Amount: 101},
	})
	if !errors.Is(err, ledger.ErrBalance) {
		t.Fatal("range error must be returned before commit", err)
	}
	// Even a caller that ignores the error cannot commit the incomplete operation.
	if err := tx.Commit(ctx); !errors.Is(err, pgx.ErrTxClosed) {
		t.Fatal("failed posting did not close the transaction", err)
	}
	got, err := s.GetInvoice(ctx, postgres.AlphaID, v.ID)
	if err != nil || got.Status != invoice.Open {
		t.Fatal("financial failure committed the invoice status", got, err)
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM ledger_operations WHERE business_key='overspend'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("partial journal persisted", count, err)
	}
}

func TestPostingsSeeEarlierEntriesInTheirTransaction(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	v, err := s.CreateInvoice(ctx, postgres.AlphaID, "same-transaction", 1)
	if err != nil {
		t.Fatal(err)
	}
	a := accounts(t, s, v)
	tx, err := s.BeginLedgerTx(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	for i, entries := range [][]ledger.Entry{
		{{AccountID: a[ledger.Asset], Side: ledger.Debit, Amount: 100}, {AccountID: a[ledger.Hold], Side: ledger.Credit, Amount: 100}},
		{{AccountID: a[ledger.Hold], Side: ledger.Debit, Amount: 100}, {AccountID: a[ledger.PartnerAvailable], Side: ledger.Credit, Amount: 100}},
	} {
		if _, err := s.PostLedgerTx(ctx, tx, fmt.Sprintf("in-transaction-%d", i), entries); err != nil {
			t.Fatal("posting did not see this transaction's history", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Balance(ctx, postgres.AlphaID); err != nil || got.Available != 100 {
		t.Fatal("incorrect balance after a multi-operation transaction", got, err)
	}
}

func TestConcurrentSpendingCannotExceedBalance(t *testing.T) {
	s := fixture(t)
	_, a := fundedInvoice(t, s, 100)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := make(chan struct{})
	results := make(chan error, 2)
	for i := range 2 {
		go func() {
			<-start
			entries := []ledger.Entry{
				{AccountID: a[ledger.Hold], Side: ledger.Debit, Amount: 80},
				{AccountID: a[ledger.PartnerAvailable], Side: ledger.Credit, Amount: 80},
			}
			if i == 1 {
				entries[0], entries[1] = entries[1], entries[0]
			}
			_, err := s.PostLedger(ctx, fmt.Sprintf("spend-%d", i), entries)
			results <- err
		}()
	}
	close(start)
	succeeded := 0
	for range 2 {
		if err := <-results; err == nil {
			succeeded++
		} else if !errors.Is(err, ledger.ErrBalance) {
			t.Fatal("unexpected concurrent posting failure", err)
		}
	}
	if succeeded != 1 {
		t.Fatalf("successful spendings = %d, want 1", succeeded)
	}
	if got, err := s.Balance(ctx, postgres.AlphaID); err != nil || got.Available != 80 {
		t.Fatal("double spending", got, err)
	}
}

func TestPostingReadsFreshBalanceAfterWaitingForLock(t *testing.T) {
	s := fixture(t)
	_, a := fundedInvoice(t, s, 100)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	first, err := s.BeginLedgerTx(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Rollback(context.Background())
	entries := []ledger.Entry{
		{AccountID: a[ledger.Hold], Side: ledger.Debit, Amount: 80},
		{AccountID: a[ledger.PartnerAvailable], Side: ledger.Credit, Amount: 80},
	}
	if _, err := s.PostLedgerTx(ctx, first, "first-spending", entries); err != nil {
		t.Fatal(err)
	}
	second, err := s.BeginLedgerTx(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var pid int32
	if err := second.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := s.PostLedgerTx(ctx, second, "waiting-spending", entries)
		_ = second.Rollback(context.Background())
		result <- err
	}()
	// Observe actual lock waiting rather than relying on scheduling or a sleep.
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting bool
		if err := s.pool.QueryRow(ctx, `SELECT COALESCE(wait_event_type = 'Lock', false) FROM pg_stat_activity WHERE pid=$1`, pid).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err := <-result:
			t.Fatal("second posting did not wait for account locks", err)
		case <-ctx.Done():
			t.Fatal("posting did not reach the account lock", ctx.Err())
		case <-ticker.C:
		}
	}
	if err := first.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, ledger.ErrBalance) {
		t.Fatal("posting used a balance from before the lock wait", err)
	}
}

func TestZeroNetChangeAtBalanceLimit(t *testing.T) {
	s := fixture(t)
	_, a := fundedInvoice(t, s, math.MaxInt64)
	if _, err := s.PostLedger(context.Background(), "net-zero", []ledger.Entry{
		{AccountID: a[ledger.Asset], Side: ledger.Debit, Amount: 1},
		{AccountID: a[ledger.Asset], Side: ledger.Credit, Amount: 1},
	}); err != nil {
		t.Fatal("entry order caused an artificial overflow", err)
	}
}

func TestLedgerRejectsRepeatableSnapshots(t *testing.T) {
	s := fixture(t)
	_, a := fundedInvoice(t, s, 100)
	ctx := context.Background()
	for _, isolation := range []pgx.TxIsoLevel{pgx.RepeatableRead, pgx.Serializable} {
		t.Run(string(isolation), func(t *testing.T) {
			tx, err := s.BeginLedgerTx(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if _, err := tx.Exec(ctx, "SET TRANSACTION ISOLATION LEVEL "+string(isolation)); err != nil {
				t.Fatal(err)
			}
			_, err = s.PostLedgerTx(ctx, tx, "invalid-isolation", []ledger.Entry{
				{AccountID: a[ledger.Hold], Side: ledger.Debit, Amount: 80},
				{AccountID: a[ledger.PartnerAvailable], Side: ledger.Credit, Amount: 80},
			})
			if !errors.Is(err, postgres.ErrLedgerIsolation) {
				t.Fatal("accepted an unsupported transaction isolation", err)
			}
		})
	}
}
