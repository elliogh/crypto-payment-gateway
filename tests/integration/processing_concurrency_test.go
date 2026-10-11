package integration

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	"crypto-payment-gateway/internal/aml"
	"crypto-payment-gateway/internal/chain"
	"crypto-payment-gateway/internal/ledger"
	"crypto-payment-gateway/internal/money"
	"crypto-payment-gateway/internal/payment"
	"crypto-payment-gateway/internal/postgres"
	"crypto-payment-gateway/internal/refund"
	"github.com/jackc/pgx/v5/pgxpool"
)

func namedProcessingStore(t *testing.T, s *databaseFixture, name string) *postgres.Store {
	t.Helper()
	cfg := s.pool.Config().Copy()
	cfg.ConnConfig.RuntimeParams["application_name"] = name
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return postgres.New(pool)
}
func awaitChainLock(t *testing.T, s *databaseFixture, ctx context.Context, name string) {
	t.Helper()
	for ctx.Err() == nil {
		var waiting bool
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE application_name=$1 AND wait_event_type='Lock')`, name).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
		runtime.Gosched()
	}
	t.Fatal("transaction did not reach chain lock", ctx.Err())
}
func TestProcessingReorgAndDecisionBothLockOrders(t *testing.T) {
	for _, risk := range []aml.Risk{aml.Low, aml.High} {
		for _, reorgFirst := range []bool{true, false} {
			t.Run(string(risk)+map[bool]string{true: " reorg first", false: " decision first"}[reorgFirst], func(t *testing.T) {
				s := chainFixture(t)
				v, _ := readyPayment(t, s, 1, "")
				j := claimed(t, s.Store)
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				blocker, err := s.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer blocker.Rollback(context.Background())
				if _, err = blocker.Exec(ctx, `SELECT chain_id FROM chain_state FOR UPDATE`); err != nil {
					t.Fatal(err)
				}
				schema := s.pool.Config().ConnConfig.RuntimeParams["search_path"]
				firstName, secondName := schema+"_first", schema+"_second"
				first, second := namedProcessingStore(t, s, firstName), namedProcessingStore(t, s, secondName)
				reorgResult, decisionResult := make(chan error, 1), make(chan error, 1)
				reorg := func(store *postgres.Store) { _, err := store.Reorg(ctx, 3); reorgResult <- err }
				decision := func(store *postgres.Store) {
					decisionResult <- store.CompleteAML(ctx, j, risk, 3, refund.Policy{Enabled: true})
				}
				if reorgFirst {
					go reorg(first)
				} else {
					go decision(first)
				}
				awaitChainLock(t, s, ctx, firstName)
				if reorgFirst {
					go decision(second)
				} else {
					go reorg(second)
				}
				awaitChainLock(t, s, ctx, secondName)
				if err = blocker.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				var reorgErr, decisionErr error
				select {
				case reorgErr = <-reorgResult:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				select {
				case decisionErr = <-decisionResult:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				if reorgFirst {
					if reorgErr != nil || !errors.Is(decisionErr, payment.ErrStaleJob) {
						t.Fatal(reorgErr, decisionErr)
					}
					financialState(t, s, v, nil)
					if rowCount(t, s, "refunds") != 0 || rowCount(t, s, "aml_results") != 0 {
						t.Fatal("stale result applied")
					}
				} else {
					if decisionErr != nil || !errors.Is(reorgErr, chain.ErrConflict) {
						t.Fatal(reorgErr, decisionErr)
					}
					var head int64
					var canonical bool
					if err = s.pool.QueryRow(ctx, `SELECT h.height,b.canonical FROM chain_state c JOIN chain_blocks h ON h.hash=c.head_hash JOIN payment_observations o ON o.payment_id=$1 AND o.version=1 JOIN chain_blocks b ON b.hash=o.block_hash`, j.PaymentID).Scan(&head, &canonical); err != nil || head != 3 || !canonical {
						t.Fatal(head, canonical, err)
					}
					want := map[ledger.AccountKind]money.USDT{ledger.Asset: 100_000_000}
					if risk == aml.Low {
						want[ledger.PartnerAvailable] = 99_000_000
						want[ledger.Income] = 1_000_000
					} else {
						want[ledger.Refund] = 100_000_000
					}
					financialState(t, s, v, want)
				}
			})
		}
	}
}
func TestProcessingConcurrentWorkersSettleOnce(t *testing.T) {
	for _, high := range []bool{false, true} {
		t.Run(map[bool]string{false: "credit", true: "refund"}[high], func(t *testing.T) {
			s := chainFixture(t)
			sender := "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			v, _ := confirmingPayment(t, s, 1, sender)
			if _, err := s.Advance(context.Background(), 2); err != nil {
				t.Fatal(err)
			}
			var blacklist []string
			if high {
				blacklist = []string{sender}
			}
			w := processingWorker(s.Store, aml.NewMock(blacklist, false))
			const workers = 12
			start, results := make(chan struct{}), make(chan error, workers)
			for range workers {
				go func() { <-start; _, err := w.RunOnce(context.Background()); results <- err }()
			}
			close(start)
			for range workers {
				if err := <-results; err != nil {
					t.Fatal(err)
				}
			}
			if rowCount(t, s, "ledger_operations") != 2 || rowCount(t, s, "aml_results") != 1 {
				t.Fatal("duplicate postings/results")
			}
			want := map[ledger.AccountKind]money.USDT{ledger.Asset: 100_000_000}
			if high {
				want[ledger.Refund] = 100_000_000
				if rowCount(t, s, "refunds") != 1 {
					t.Fatal("duplicate refunds")
				}
			} else {
				want[ledger.PartnerAvailable] = 99_000_000
				want[ledger.Income] = 1_000_000
			}
			financialState(t, s, v, want)
		})
	}
}
func TestProcessingReorgReversalsAreAtomic(t *testing.T) {
	s := chainFixture(t)
	ctx := context.Background()
	v1, _ := confirmingPayment(t, s, 1, "")
	v2, _ := confirmingPayment(t, s, 2, "")
	if _, err := s.Advance(ctx, 2); err != nil {
		t.Fatal(err)
	}
	if err := s.PrepareReady(ctx, 3); err != nil {
		t.Fatal(err)
	}
	if rowCount(t, s, "ledger_operations") != 2 {
		t.Fatal("missing holds")
	}
	// Fail after the reversal postings; both holds and source must survive.
	if _, err := s.pool.Exec(ctx, `ALTER TABLE payments ADD CONSTRAINT fail_reorg CHECK(status<>'ORPHANED')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reorg(ctx, 4); err == nil {
		t.Fatal("expected rollback failure")
	}
	if rowCount(t, s, "ledger_operations") != 2 {
		t.Fatal("partial reversal")
	}
	want := map[ledger.AccountKind]money.USDT{ledger.Asset: 100_000_000, ledger.Hold: 100_000_000}
	financialState(t, s, v1, want)
	financialState(t, s, v2, want)
	if _, err := s.pool.Exec(ctx, `ALTER TABLE payments DROP CONSTRAINT fail_reorg`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reorg(ctx, 4); err != nil {
		t.Fatal(err)
	}
	financialState(t, s, v1, nil)
	financialState(t, s, v2, nil)
	if rowCount(t, s, "ledger_operations") != 4 {
		t.Fatal("missing reversals")
	}
	var head int64
	var cancelled int
	if err := s.pool.QueryRow(ctx, `SELECT h.height,(SELECT count(*) FROM aml_jobs WHERE status='CANCELLED') FROM chain_state c JOIN chain_blocks h ON h.hash=c.head_hash`).Scan(&head, &cancelled); err != nil || head != 0 || cancelled != 2 {
		t.Fatal(head, cancelled, err)
	}
}
func TestProcessingRemainingCanonicalDepositLosesConfirmations(t *testing.T) {
	s := chainFixture(t)
	ctx := context.Background()
	v, _ := readyPayment(t, s, 1, "")
	j := claimed(t, s.Store)
	if _, err := s.Reorg(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteAML(ctx, j, aml.Low, 3, refund.Policy{}); !errors.Is(err, payment.ErrNotConfirmed) {
		t.Fatal("N was not rechecked", err)
	}
	financialState(t, s, v, map[ledger.AccountKind]money.USDT{ledger.Asset: 100_000_000, ledger.Hold: 100_000_000})
	if _, err := s.Advance(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteAML(ctx, j, aml.Low, 3, refund.Policy{}); err != nil {
		t.Fatal(err)
	}
}
