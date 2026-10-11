package integration

import (
	"context"
	"errors"
	"sync"
	"testing"

	"crypto-payment-gateway/internal/invoice"
	"crypto-payment-gateway/internal/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestInvoicesConcurrentAndRestart(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	const workers = 16
	start := make(chan struct{})
	type result struct {
		v   invoice.Invoice
		err error
	}
	results := make(chan result, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			v, err := s.CreateInvoice(ctx, postgres.AlphaID, "same-key", 100_000_000)
			results <- result{v, err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	var first invoice.Invoice
	for got := range results {
		if got.err != nil {
			t.Fatal(got.err)
		}
		if first.ID == "" {
			first = got.v
		}
		if got.v.ID != first.ID || got.v.DepositAddress != first.DepositAddress {
			t.Fatal("duplicate invoice/address")
		}
	}
	for _, table := range []string{"invoices", "invoice_idempotency"} {
		var count int
		if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil || count != 1 {
			t.Fatal(table, count, err)
		}
	}
	if _, err := s.CreateInvoice(ctx, postgres.AlphaID, "same-key", 99); !errors.Is(err, invoice.ErrConflict) {
		t.Fatal("expected conflict", err)
	}
	second, err := s.CreateInvoice(ctx, postgres.BetaID, "same-key", 100_000_000)
	if err != nil || second.ID == first.ID || second.FeeBPS != 250 {
		t.Fatal(second, err)
	}
	if _, err = s.GetInvoice(ctx, postgres.BetaID, first.ID); !errors.Is(err, invoice.ErrNotFound) {
		t.Fatal("foreign invoice visible", err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE partners SET fee_bps=150 WHERE id=$1`, postgres.AlphaID); err != nil {
		t.Fatal(err)
	}
	// A fresh pool reproduces a process restart: persistence cannot rely on Go memory.
	reconnected, err := pgxpool.NewWithConfig(ctx, s.pool.Config().Copy())
	if err != nil {
		t.Fatal(err)
	}
	defer reconnected.Close()
	restarted := postgres.New(reconnected)
	if err = postgres.Migrate(ctx, reconnected); err != nil {
		t.Fatal(err)
	}
	if err = restarted.Seed(ctx, alphaKey, betaKey); err != nil {
		t.Fatal(err)
	}
	replayed, err := restarted.CreateInvoice(ctx, postgres.AlphaID, "same-key", 100_000_000)
	if err != nil || replayed.ID != first.ID || replayed.FeeBPS != 100 {
		t.Fatal("restart or fee snapshot failure", replayed, err)
	}
	newInvoice, err := restarted.CreateInvoice(ctx, postgres.AlphaID, "new-key", 100_000_000)
	if err != nil || newInvoice.FeeBPS != 150 {
		t.Fatal("seed reset partner settings", newInvoice, err)
	}
	var migrationCount int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM goose_db_version WHERE version_id > 0 AND is_applied`).Scan(&migrationCount); err != nil || migrationCount != currentMigrationVersion {
		t.Fatal(migrationCount, err)
	}
}
