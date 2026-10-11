package integration

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"crypto-payment-gateway/internal/chain"
	"crypto-payment-gateway/internal/invoice"
	"crypto-payment-gateway/internal/payment"
	"crypto-payment-gateway/internal/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
)

func chainFixture(t *testing.T) *databaseFixture {
	t.Helper()
	s := fixture(t)
	if err := s.InitializeChain(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

func depositInvoice(t *testing.T, s *databaseFixture, key string) invoice.Invoice {
	t.Helper()
	v, err := s.CreateInvoice(context.Background(), postgres.AlphaID, key, 100_000_000)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func transfer(v invoice.Invoice, n int) chain.Event {
	return chain.Event{TransferID: chain.TransferID{ChainID: chain.ID, TxHash: fmt.Sprintf("0x%064x", n), EventIndex: 2},
		LogIndex: 3, Sender: "0x" + strings.Repeat("a", 40), Recipient: v.DepositAddress, TokenContract: chain.USDTContract,
		Topic0: chain.TransferTopic, RawAmount: strconv.FormatInt(int64(v.Amount), 10), ReceiptSuccess: true}
}

func scanChain(t *testing.T, s *postgres.Store) chain.ScanResult {
	t.Helper()
	r, err := (chain.Scanner{Source: s, Deposits: s}).Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func observed(t *testing.T, s *postgres.Store, v invoice.Invoice) *payment.Observation {
	t.Helper()
	got, err := s.GetInvoice(context.Background(), postgres.AlphaID, v.ID)
	if err != nil || got.Payment == nil {
		t.Fatal("missing payment", got, err)
	}
	if got.Status != invoice.Open {
		t.Fatal("stage 2 changed invoice status", got)
	}
	return got.Payment
}

func noFinancialChanges(t *testing.T, s *databaseFixture) {
	t.Helper()
	for _, table := range []string{"ledger_operations", "ledger_entries", "aml_jobs", "aml_results", "refunds"} {
		var count int
		if err := s.pool.QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&count); err != nil || count != 0 {
			t.Fatal(table, count, err)
		}
	}
	b, err := s.Balance(context.Background(), postgres.AlphaID)
	if err != nil || b.Available != 0 {
		t.Fatal(b, err)
	}
}

func TestChainConfirmationsReorgAndReinclusion(t *testing.T) {
	s := chainFixture(t)
	ctx := context.Background()
	v := depositInvoice(t, s, "chain")
	e := transfer(v, 1)
	first, err := s.AddTransfer(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	scanChain(t, s.Store)
	p := observed(t, s.Store, v)
	if p.Status != payment.Confirming || p.Confirmations != 1 || p.Version != 1 {
		t.Fatal(p)
	}
	paymentID := p.ID
	if _, err = s.Advance(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if p = observed(t, s.Store, v); p.Confirmations != 2 {
		t.Fatal(p)
	}
	if _, err = s.Reorg(ctx, 2); err != nil {
		t.Fatal(err)
	}
	// Readiness disappears immediately; observation changes are asynchronous.
	if p = observed(t, s.Store, v); p.Confirmations != 0 {
		t.Fatal(p)
	}
	if result := scanChain(t, s.Store); result.Blocks != 0 {
		t.Fatal("rollback created replacement blocks", result)
	}
	if p = observed(t, s.Store, v); p.ID != paymentID || p.Status != payment.Orphaned || p.Version != 2 {
		t.Fatal(p)
	}
	e.LogIndex = 7
	inclusion, err := s.AddTransfer(ctx, e)
	if err != nil || inclusion.BlockHash == first.BlockHash || inclusion.BlockHeight != 1 || inclusion.LogIndex != 7 {
		t.Fatal(inclusion, err)
	}
	if replay, err := s.AddTransfer(ctx, e); err != nil || replay != inclusion {
		t.Fatal("retry appended a block", replay, err)
	}
	scanChain(t, s.Store)
	if p = observed(t, s.Store, v); p.ID != paymentID || p.Version != 3 || p.Confirmations != 1 || p.Status != payment.Confirming {
		t.Fatal(p)
	}
	if _, err = s.Advance(ctx, 2); err != nil {
		t.Fatal(err)
	}
	scanChain(t, s.Store)
	if p = observed(t, s.Store, v); p.Confirmations != 3 {
		t.Fatal(p)
	}
	if result := scanChain(t, s.Store); result.Blocks != 0 {
		t.Fatal("cursor not preserved", result)
	}
	var count, index int
	var canonical bool
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM payments`).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	if err = s.pool.QueryRow(ctx, `SELECT log_index,canonical FROM payment_observations WHERE payment_id=$1 AND version=3`, paymentID).Scan(&index, &canonical); err != nil || index != 7 || !canonical {
		t.Fatal(index, canonical, err)
	}
	noFinancialChanges(t, s)
}

func TestChainRejectsInvalidLogsAndIsolatesOverflow(t *testing.T) {
	s := chainFixture(t)
	ctx := context.Background()
	v := depositInvoice(t, s, "logs")
	for n, change := range []func(*chain.Event){
		func(e *chain.Event) { e.TokenContract = e.Sender },
		func(e *chain.Event) { e.Topic0 = e.TxHash },
		func(e *chain.Event) { e.ReceiptSuccess = false },
		func(e *chain.Event) { e.RawAmount = "9223372036854775808" },
		func(e *chain.Event) { e.Recipient = e.Sender },
	} {
		e := transfer(v, n+1)
		change(&e)
		if _, err := s.AddTransfer(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.AddTransfer(ctx, transfer(v, 6)); err != nil {
		t.Fatal(err)
	}
	result := scanChain(t, s.Store)
	if result.Isolated != 1 || result.Blocks != 6 {
		t.Fatal(result)
	}
	var payments, bindings int
	if err := s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM payments),(SELECT count(*) FROM invoice_transfer_bindings)`).Scan(&payments, &bindings); err != nil || payments != 1 || bindings != 1 {
		t.Fatal(payments, bindings, err)
	}
	if p := observed(t, s.Store, v); p.Confirmations != 1 {
		t.Fatal(p)
	}
	noFinancialChanges(t, s)
}

func TestChainConflictingInputDoesNotChangeChain(t *testing.T) {
	s := chainFixture(t)
	ctx := context.Background()
	v := depositInvoice(t, s, "conflicts")
	e := transfer(v, 1)
	bad := e
	bad.ChainID = 2
	if _, err := s.AddTransfer(ctx, bad); !errors.Is(err, chain.ErrInvalid) {
		t.Fatal("other network", err)
	}
	first, err := s.AddTransfer(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*chain.Event){func(e *chain.Event) { e.Sender = e.Recipient }, func(e *chain.Event) { e.RawAmount = "99000000" }, func(e *chain.Event) { e.Topic0 = e.TxHash }} {
		bad := e
		change(&bad)
		if _, err := s.AddTransfer(ctx, bad); !errors.Is(err, chain.ErrConflict) {
			t.Fatal("changed payload accepted", err)
		}
	}
	if replay, err := s.AddTransfer(ctx, e); err != nil || replay != first {
		t.Fatal(replay, err)
	}
	var blocks, events, bindings int
	if err = s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM chain_blocks),(SELECT count(*) FROM chain_events),(SELECT count(*) FROM invoice_transfer_bindings)`).Scan(&blocks, &events, &bindings); err != nil || blocks != 2 || events != 1 || bindings != 0 {
		t.Fatal(blocks, events, bindings, err)
	}
}

func TestChainParallelTransfersAndScanners(t *testing.T) {
	s := chainFixture(t)
	ctx := context.Background()
	v := depositInvoice(t, s, "parallel")
	const workers = 12
	start := make(chan struct{})
	results := make(chan error, workers)
	for range workers {
		go func() { <-start; _, err := s.AddTransfer(ctx, transfer(v, 1)); results <- err }()
	}
	close(start)
	success := 0
	for range workers {
		err := <-results
		if err != nil {
			t.Fatal(err)
		}
		success++
	}
	if success != workers {
		t.Fatal("concurrent retries failed", success)
	}
	start = make(chan struct{})
	for range workers {
		go func() {
			<-start
			_, err := (chain.Scanner{Source: s.Store, Deposits: s.Store}).Scan(ctx)
			results <- err
		}()
	}
	close(start)
	for range workers {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	var blocks, events, payments int
	if err := s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM chain_blocks),(SELECT count(*) FROM chain_events),(SELECT count(*) FROM payments)`).Scan(&blocks, &events, &payments); err != nil || blocks != 2 || events != 1 || payments != 1 {
		t.Fatal(blocks, events, payments, err)
	}
	noFinancialChanges(t, s)
}

func TestConcurrentIndependentTransfersProduceOnePayment(t *testing.T) {
	s := chainFixture(t)
	v := depositInvoice(t, s, "independent")
	const workers = 12
	start, results := make(chan struct{}), make(chan error, workers)
	for n := range workers {
		go func() {
			<-start
			_, err := s.AddTransfer(context.Background(), transfer(v, n+1))
			results <- err
		}()
	}
	close(start)
	for range workers {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	scanChain(t, s.Store)
	var events, payments int
	var first bool
	err := s.pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM chain_events),
		(SELECT count(*) FROM payments),
		(SELECT p.tx_hash=e.tx_hash FROM payments p JOIN chain_events e ON e.block_hash=
			(SELECT hash FROM chain_blocks WHERE canonical AND height=1))`).Scan(&events, &payments, &first)
	if err != nil || events != workers || payments != 1 || !first {
		t.Fatal(events, payments, first, err)
	}
	noFinancialChanges(t, s)
}

func TestChainRestartsWithUnscannedAndOrphanedTransfers(t *testing.T) {
	s := chainFixture(t)
	ctx := context.Background()
	v := depositInvoice(t, s, "restart")
	e := transfer(v, 1)
	if _, err := s.AddTransfer(ctx, e); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, s.pool.Config().Copy())
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	restarted := postgres.New(pool)
	if err = restarted.InitializeChain(ctx); err != nil {
		t.Fatal(err)
	}
	scanChain(t, restarted)
	p := observed(t, restarted, v)
	id := p.ID
	if _, err = restarted.Reorg(ctx, 1); err != nil {
		t.Fatal(err)
	}
	// A new Store has no in-memory chain, cursor, binding or payment state.
	restarted = postgres.New(pool)
	if err = restarted.InitializeChain(ctx); err != nil {
		t.Fatal(err)
	}
	e.LogIndex = 9
	if _, err = restarted.AddTransfer(ctx, e); err != nil {
		t.Fatal(err)
	}
	scanChain(t, restarted)
	if p = observed(t, restarted, v); p.ID != id || p.Version != 3 || p.Confirmations != 1 {
		t.Fatal(p)
	}
	if result := scanChain(t, restarted); result.Blocks != 0 {
		t.Fatal(result)
	}
}

func TestChainStaleScanCannotRestoreOrphanedBlock(t *testing.T) {
	s := chainFixture(t)
	ctx := context.Background()
	v := depositInvoice(t, s, "stale")
	e := transfer(v, 1)
	first, err := s.AddTransfer(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	batch, err := s.ReadBatch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Reorg(ctx, 1); err != nil {
		t.Fatal(err)
	}
	err = s.ApplyScan(ctx, batch, []chain.Candidate{{Event: e, Amount: v.Amount, BlockHash: first.BlockHash}})
	if !errors.Is(err, chain.ErrStale) {
		t.Fatal("stale scan accepted", err)
	}
	scanChain(t, s.Store)
	var count int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM payments`).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
	noFinancialChanges(t, s)
}

func TestScannerOwnsInvoiceMatching(t *testing.T) {
	s := chainFixture(t)
	ctx := context.Background()
	v := depositInvoice(t, s, "matching")
	// Structurally valid chain events need not satisfy invoice business rules.
	for n, raw := range []string{"99000000", "0", "101000000"} {
		e := transfer(v, n+1)
		e.RawAmount = raw
		if _, err := s.AddTransfer(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	scanChain(t, s.Store)
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM invoice_transfer_bindings`).Scan(&count); err != nil || count != 0 {
		t.Fatal("unsupported amount bound an invoice", count, err)
	}
	// Independent transfers can coexist in the source; only the first complete
	// payment is supported. The binding survives a rollback of all these blocks.
	for _, n := range []int{4, 5} {
		if _, err := s.AddTransfer(ctx, transfer(v, n)); err != nil {
			t.Fatal(err)
		}
	}
	scanChain(t, s.Store)
	id := observed(t, s.Store, v).ID
	var hash string
	if err := s.pool.QueryRow(ctx, `SELECT tx_hash FROM payments`).Scan(&hash); err != nil || hash != transfer(v, 4).TxHash {
		t.Fatal(hash, err)
	}
	if _, err := s.Reorg(ctx, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddTransfer(ctx, transfer(v, 5)); err != nil {
		t.Fatal(err)
	}
	scanChain(t, s.Store)
	if p := observed(t, s.Store, v); p.ID != id || p.Status != payment.Orphaned {
		t.Fatal("another transfer replaced the binding", p)
	}
	if _, err := s.AddTransfer(ctx, transfer(v, 4)); err != nil {
		t.Fatal(err)
	}
	scanChain(t, s.Store)
	if p := observed(t, s.Store, v); p.ID != id || p.Status != payment.Confirming || p.Version != 3 {
		t.Fatal(p)
	}
	noFinancialChanges(t, s)
}

func TestChainScanAndReorgRollbackAtomically(t *testing.T) {
	s := chainFixture(t)
	ctx := context.Background()
	v := depositInvoice(t, s, "rollback")
	if _, err := s.AddTransfer(ctx, transfer(v, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `ALTER TABLE payments ADD CONSTRAINT force_scan_failure CHECK(amount<>100000000)`); err != nil {
		t.Fatal(err)
	}
	if _, err := (chain.Scanner{Source: s.Store, Deposits: s.Store}).Scan(ctx); err == nil {
		t.Fatal("expected SQL error")
	}
	var height, count int
	if err := s.pool.QueryRow(ctx, `SELECT b.height,(SELECT count(*) FROM payments) FROM chain_state c JOIN chain_blocks b ON b.hash=c.scan_hash`).Scan(&height, &count); err != nil || height != 0 || count != 0 {
		t.Fatal("partial scan", height, count, err)
	}
	if _, err := s.pool.Exec(ctx, `ALTER TABLE payments DROP CONSTRAINT force_scan_failure`); err != nil {
		t.Fatal(err)
	}
	scanChain(t, s.Store)
	before := *observed(t, s.Store, v)
	if _, err := s.pool.Exec(ctx, `ALTER TABLE chain_blocks ADD CONSTRAINT force_reorg_failure CHECK(canonical)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reorg(ctx, 1); err == nil {
		t.Fatal("expected reorg SQL error")
	}
	if after := *observed(t, s.Store, v); after != before {
		t.Fatal("partial reorg", before, after)
	}
	var headEqualsCursor bool
	if err := s.pool.QueryRow(ctx, `SELECT head_hash=scan_hash FROM chain_state`).Scan(&headEqualsCursor); err != nil || !headEqualsCursor {
		t.Fatal("partial cursor update", err)
	}
	if _, err := s.pool.Exec(ctx, `ALTER TABLE chain_blocks DROP CONSTRAINT force_reorg_failure`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reorg(ctx, 1); err != nil {
		t.Fatal(err)
	}
	// The source rollback has committed. Force a later observation failure:
	// a retry must reconcile it without any new blocks or partial versions.
	if _, err := s.pool.Exec(ctx, `ALTER TABLE payments ADD CONSTRAINT force_orphan_failure CHECK(status<>'ORPHANED')`); err != nil {
		t.Fatal(err)
	}
	if _, err := (chain.Scanner{Source: s.Store, Deposits: s.Store}).Scan(ctx); err == nil {
		t.Fatal("expected reconciliation SQL error")
	}
	var version int64
	if err := s.pool.QueryRow(ctx, `SELECT observation_version,(SELECT count(*) FROM payment_observations) FROM payments`).Scan(&version, &count); err != nil || version != 1 || count != 1 {
		t.Fatal("partial reconciliation", version, count, err)
	}
	if _, err := s.pool.Exec(ctx, `ALTER TABLE payments DROP CONSTRAINT force_orphan_failure`); err != nil {
		t.Fatal(err)
	}
	scanChain(t, s.Store)
	if p := observed(t, s.Store, v); p.Status != payment.Orphaned || p.Version != 2 || p.Confirmations != 0 {
		t.Fatal(p)
	}
	noFinancialChanges(t, s)
}
