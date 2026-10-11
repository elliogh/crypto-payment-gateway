package integration

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"

	"crypto-payment-gateway/internal/invoice"
	"crypto-payment-gateway/internal/ledger"
	"crypto-payment-gateway/internal/money"
	"crypto-payment-gateway/internal/postgres"
)

func accounts(t *testing.T, s *databaseFixture, v invoice.Invoice) map[ledger.AccountKind]string {
	t.Helper()
	out := map[ledger.AccountKind]string{}
	const query = `
        SELECT kind, id::text
        FROM ledger_accounts
        WHERE invoice_id = $1
            OR (kind = $2 AND partner_id = $3)
            OR kind = $4`
	rows, err := s.pool.Query(context.Background(), query,
		v.ID, string(ledger.PartnerAvailable), postgres.AlphaID, string(ledger.Income))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind ledger.AccountKind
		var id string
		if err := rows.Scan(&kind, &id); err != nil {
			t.Fatal(err)
		}
		out[kind] = id
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestLedgerPostingAndReplay(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	v, err := s.CreateInvoice(ctx, postgres.AlphaID, "ledger", 100_000_000)
	if err != nil {
		t.Fatal(err)
	}
	a := accounts(t, s, v)
	deposit := []ledger.Entry{
		{AccountID: a[ledger.Asset], Side: ledger.Debit, Amount: 100_000_000},
		{AccountID: a[ledger.Hold], Side: ledger.Credit, Amount: 100_000_000},
	}
	first, err := s.PostLedger(ctx, "deposit", deposit)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errs := make(chan error, 8)
	for range 8 {
		go func() {
			<-start
			id, err := s.PostLedger(ctx, "deposit", deposit)
			if err == nil && id != first {
				err = errors.New("duplicate ledger id")
			}
			errs <- err
		}()
	}
	close(start)
	for range 8 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.PostLedger(ctx, "deposit", []ledger.Entry{
		{AccountID: a[ledger.Asset], Side: ledger.Debit, Amount: 1},
		{AccountID: a[ledger.Hold], Side: ledger.Credit, Amount: 1},
	}); !errors.Is(err, ledger.ErrConflict) {
		t.Fatal("expected ledger conflict", err)
	}
	balance, err := s.Balance(ctx, postgres.AlphaID)
	if err != nil || balance.Available != 0 {
		t.Fatal("hold credited partner", balance, err)
	}
	// Unknown account and range failure must leave no journal behind.
	if _, err = s.PostLedger(ctx, "missing-account", []ledger.Entry{
		{AccountID: a[ledger.Asset], Side: ledger.Debit, Amount: 1},
		{AccountID: "00000000-0000-4000-8000-999999999999", Side: ledger.Credit, Amount: 1},
	}); err == nil {
		t.Fatal("unknown account accepted")
	}
	if _, err = s.PostLedger(ctx, "negative-hold", []ledger.Entry{
		{AccountID: a[ledger.Hold], Side: ledger.Debit, Amount: 100_000_001},
		{AccountID: a[ledger.PartnerAvailable], Side: ledger.Credit, Amount: 100_000_001},
	}); !errors.Is(err, ledger.ErrBalance) {
		t.Fatal("negative balance accepted", err)
	}
	settlement := []ledger.Entry{
		{AccountID: a[ledger.Hold], Side: ledger.Debit, Amount: 100_000_000},
		{AccountID: a[ledger.PartnerAvailable], Side: ledger.Credit, Amount: 99_000_000},
		{AccountID: a[ledger.Income], Side: ledger.Credit, Amount: 1_000_000},
	}
	if _, err = s.PostLedger(ctx, "settlement", settlement); err != nil {
		t.Fatal(err)
	}
	balance, err = s.Balance(ctx, postgres.AlphaID)
	if err != nil || balance.Available != 99_000_000 {
		t.Fatal(balance, err)
	}
	beta, err := s.Balance(ctx, postgres.BetaID)
	if err != nil || beta.Available != 0 {
		t.Fatal("foreign balance", beta, err)
	}
	var delta string
	if err = s.pool.QueryRow(ctx, `SELECT sum(CASE WHEN side='DEBIT' THEN amount ELSE -amount END)::text FROM ledger_entries`).Scan(&delta); err != nil || delta != "0" {
		t.Fatal("ledger imbalance", delta, err)
	}
}

func TestLedgerOverflow(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	v, err := s.CreateInvoice(ctx, postgres.AlphaID, "max", 1)
	if err != nil {
		t.Fatal(err)
	}
	a := accounts(t, s, v)
	if _, err = s.PostLedger(ctx, "max", []ledger.Entry{
		{AccountID: a[ledger.Asset], Side: ledger.Debit, Amount: money.USDT(math.MaxInt64)},
		{AccountID: a[ledger.Hold], Side: ledger.Credit, Amount: money.USDT(math.MaxInt64)},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PostLedger(ctx, "overflow", []ledger.Entry{
		{AccountID: a[ledger.Asset], Side: ledger.Debit, Amount: 1},
		{AccountID: a[ledger.Hold], Side: ledger.Credit, Amount: 1},
	}); !errors.Is(err, ledger.ErrBalance) {
		t.Fatal("overflowing account balance accepted", err)
	}
	var count int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM ledger_operations WHERE business_key='overflow'`).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
}

func TestConcurrentLedgerBalanceLimit(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	v, err := s.CreateInvoice(ctx, postgres.AlphaID, "concurrent-limit", 1)
	if err != nil {
		t.Fatal(err)
	}
	a := accounts(t, s, v)
	entries := []ledger.Entry{
		{AccountID: a[ledger.Asset], Side: ledger.Debit, Amount: money.USDT(math.MaxInt64)},
		{AccountID: a[ledger.Hold], Side: ledger.Credit, Amount: money.USDT(math.MaxInt64)},
	}
	start := make(chan struct{})
	errs := make(chan error, 2)
	for i := range 2 {
		go func() {
			<-start
			_, err := s.PostLedger(ctx, fmt.Sprintf("concurrent-limit-%d", i), entries)
			errs <- err
		}()
	}
	close(start)
	succeeded := 0
	for range 2 {
		if err := <-errs; err == nil {
			succeeded++
		}
	}
	if succeeded != 1 {
		t.Fatalf("expected one commit, got %d", succeeded)
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM ledger_operations`).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
}
