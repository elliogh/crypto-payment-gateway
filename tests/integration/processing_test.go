package integration

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"crypto-payment-gateway/internal/aml"
	"crypto-payment-gateway/internal/chain"
	"crypto-payment-gateway/internal/invoice"
	"crypto-payment-gateway/internal/ledger"
	"crypto-payment-gateway/internal/money"
	"crypto-payment-gateway/internal/payment"
	"crypto-payment-gateway/internal/postgres"
	"crypto-payment-gateway/internal/refund"
	"github.com/jackc/pgx/v5/pgxpool"
)

func processingWorker(s *postgres.Store, provider aml.Provider) payment.Worker {
	return payment.Worker{Store: s, Provider: provider, Confirmations: 3, RetryInterval: time.Minute, Lease: 30 * time.Second, RefundPolicy: refund.Policy{Enabled: true}}
}
func confirmingPayment(t *testing.T, s *databaseFixture, n int, sender string) (invoice.Invoice, chain.Event) {
	t.Helper()
	ctx := context.Background()
	v := depositInvoice(t, s, fmt.Sprintf("process-%d", n))
	e := transfer(v, n)
	if sender != "" {
		e.Sender = sender
	}
	if _, err := s.AddTransfer(ctx, e); err != nil {
		t.Fatal(err)
	}
	scanChain(t, s.Store)
	return v, e
}
func readyPayment(t *testing.T, s *databaseFixture, n int, sender string) (invoice.Invoice, chain.Event) {
	t.Helper()
	v, e := confirmingPayment(t, s, n, sender)
	if _, err := s.Advance(context.Background(), 2); err != nil {
		t.Fatal(err)
	}
	if err := s.PrepareReady(context.Background(), 3); err != nil {
		t.Fatal(err)
	}
	return v, e
}
func claimed(t *testing.T, s *postgres.Store) payment.Job {
	t.Helper()
	j, ok, err := s.ClaimAML(context.Background(), 30*time.Second)
	if err != nil || !ok {
		t.Fatal("missing job", j, ok, err)
	}
	return j
}
func financialState(t *testing.T, s *databaseFixture, v invoice.Invoice, want map[ledger.AccountKind]money.USDT) {
	t.Helper()
	ctx := context.Background()
	rows, err := s.pool.Query(ctx, `SELECT a.kind,coalesce(sum(CASE WHEN (a.kind='ASSET' AND e.side='DEBIT') OR(a.kind<>'ASSET' AND e.side='CREDIT') THEN e.amount ELSE -e.amount END),0)::bigint
 FROM ledger_accounts a LEFT JOIN ledger_entries e ON e.account_id=a.id WHERE a.invoice_id=$1 OR a.kind='INCOME' OR(a.kind='PARTNER_AVAILABLE' AND a.partner_id=(SELECT partner_id FROM invoices WHERE id=$1)) GROUP BY a.id,a.kind`, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind ledger.AccountKind
		var amount money.USDT
		if err := rows.Scan(&kind, &amount); err != nil {
			t.Fatal(err)
		}
		if amount != want[kind] {
			t.Fatalf("%s=%s want %s", kind, amount, want[kind])
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	var balanced bool
	if err := s.pool.QueryRow(ctx, `SELECT coalesce(sum(CASE WHEN side='DEBIT' THEN amount ELSE -amount END),0)=0 FROM ledger_entries`).Scan(&balanced); err != nil || !balanced {
		t.Fatal("unbalanced journal", err)
	}

}
func rowCount(t *testing.T, s *databaseFixture, table string) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
func restartStore(t *testing.T, s *databaseFixture) *postgres.Store {
	t.Helper()
	pool, err := pgxpool.NewWithConfig(context.Background(), s.pool.Config().Copy())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	store := postgres.New(pool)
	if err := store.InitializeChain(context.Background()); err != nil {
		t.Fatal(err)
	}
	return store
}
func TestProcessingCreditsOnlyAfterNAndAML(t *testing.T) {
	s := chainFixture(t)
	ctx := context.Background()
	v, _ := confirmingPayment(t, s, 1, "")
	provider := aml.NewMock(nil, false)
	w := processingWorker(s.Store, provider)
	for confirmations := 1; confirmations <= 2; confirmations++ {
		if worked, err := w.RunOnce(ctx); err != nil || worked {
			t.Fatal(worked, err)
		}
		if rowCount(t, s, "ledger_operations") != 0 || rowCount(t, s, "aml_jobs") != 0 {
			t.Fatal("financial work before N")
		}
		if _, err := s.Advance(ctx, 1); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.PrepareReady(ctx, 3); err != nil {
		t.Fatal(err)
	}
	financialState(t, s, v, map[ledger.AccountKind]money.USDT{ledger.Asset: 100_000_000, ledger.Hold: 100_000_000})
	job := claimed(t, s.Store)
	if err := s.CompleteAML(ctx, job, aml.Low, 3, w.RefundPolicy); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetInvoice(ctx, postgres.AlphaID, v.ID)
	if err != nil || got.Status != invoice.Paid || got.Payment.Status != payment.Credited {
		t.Fatal(got, err)
	}
	financialState(t, s, v, map[ledger.AccountKind]money.USDT{ledger.Asset: 100_000_000, ledger.PartnerAvailable: 99_000_000, ledger.Income: 1_000_000})
	restarted := restartStore(t, s)
	w.Store = restarted
	if worked, err := w.RunOnce(ctx); err != nil || worked {
		t.Fatal("restarted replay", worked, err)
	}
	if err := restarted.CompleteAML(ctx, job, aml.Low, 3, w.RefundPolicy); !errors.Is(err, payment.ErrStaleJob) {
		t.Fatal(err)
	}
	if rowCount(t, s, "ledger_operations") != 2 || rowCount(t, s, "aml_results") != 1 || rowCount(t, s, "refunds") != 0 {
		t.Fatal("duplicate settlement")
	}
}
func TestProcessingHighRiskRefund(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy refund.Policy
		status refund.Status
	}{
		{"safe", refund.Policy{Enabled: true}, refund.Ready},
		{"unsafe", refund.Policy{Enabled: true, UnsafeSenders: []string{"0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}, refund.Held},
		{"approval required", refund.Policy{}, refund.Held},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := chainFixture(t)
			ctx := context.Background()
			sender := "0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			v, _ := readyPayment(t, s, 1, sender)
			w := processingWorker(s.Store, aml.NewMock([]string{sender}, false))
			w.RefundPolicy = tc.policy
			if worked, err := w.RunOnce(ctx); err != nil || !worked {
				t.Fatal(worked, err)
			}
			got, err := s.GetInvoice(ctx, postgres.AlphaID, v.ID)
			if err != nil || got.Refund == nil || got.Refund.Status != tc.status || got.Refund.Amount != 100_000_000 || got.Payment.Status != payment.Rejected || got.Status != invoice.Open {
				t.Fatal(got, err)
			}
			financialState(t, s, v, map[ledger.AccountKind]money.USDT{ledger.Asset: 100_000_000, ledger.Refund: 100_000_000})
			id := got.Refund.ID
			if tc.status == refund.Held {
				recipient := "0xcccccccccccccccccccccccccccccccccccccccc"
				approved, err := s.ApproveRefund(ctx, id, recipient)
				if err != nil || approved.Status != refund.Ready {
					t.Fatal(approved, err)
				}
				if replay, err := s.ApproveRefund(ctx, id, recipient); err != nil || replay != approved {
					t.Fatal("approval replay", replay, err)
				}
				if _, err := s.ApproveRefund(ctx, id, sender); !errors.Is(err, refund.ErrConflict) {
					t.Fatal(err)
				}
			}
			w.Store = restartStore(t, s)
			if worked, err := w.RunOnce(ctx); err != nil || worked {
				t.Fatal(worked, err)
			}
			if rowCount(t, s, "refunds") != 1 || rowCount(t, s, "ledger_operations") != 2 {
				t.Fatal("duplicate refund")
			}
		})
	}
}
func TestProcessingAMLOutageRetriesAfterRestart(t *testing.T) {
	s := chainFixture(t)
	ctx := context.Background()
	v, _ := readyPayment(t, s, 1, "")
	provider := aml.NewMock(nil, true)
	w := processingWorker(s.Store, provider)
	if _, err := w.RunOnce(ctx); !errors.Is(err, aml.ErrUnavailable) {
		t.Fatal(err)
	}
	var status, reason string
	var attempts int
	var future bool
	if err := s.pool.QueryRow(ctx, `SELECT status,last_error,attempts,next_attempt_at>clock_timestamp() FROM aml_jobs`).Scan(&status, &reason, &attempts, &future); err != nil || status != "PENDING" || reason != "aml_unavailable" || attempts != 1 || !future {
		t.Fatal(status, reason, attempts, future, err)
	}
	if worked, err := w.RunOnce(ctx); err != nil || worked {
		t.Fatal("retry delay ignored", worked, err)
	}
	financialState(t, s, v, map[ledger.AccountKind]money.USDT{ledger.Asset: 100_000_000, ledger.Hold: 100_000_000})
	if rowCount(t, s, "aml_results") != 0 {
		t.Fatal("outage treated as risk")
	}
	w.Store = restartStore(t, s)
	provider.SetUnavailable(false)
	if _, err := s.pool.Exec(ctx, `UPDATE aml_jobs SET next_attempt_at=clock_timestamp()`); err != nil {
		t.Fatal(err)
	}
	if worked, err := w.RunOnce(ctx); err != nil || !worked {
		t.Fatal(worked, err)
	}
	financialState(t, s, v, map[ledger.AccountKind]money.USDT{ledger.Asset: 100_000_000, ledger.PartnerAvailable: 99_000_000, ledger.Income: 1_000_000})
	if rowCount(t, s, "ledger_operations") != 2 {
		t.Fatal("duplicate hold")
	}
}
func TestProcessingLostLeaseCannotApplyResult(t *testing.T) {
	s := chainFixture(t)
	ctx := context.Background()
	v, _ := readyPayment(t, s, 1, "")
	old := claimed(t, s.Store)
	if _, err := s.pool.Exec(ctx, `UPDATE aml_jobs SET lease_until=clock_timestamp()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteAML(ctx, old, aml.High, 3, refund.Policy{Enabled: true}); !errors.Is(err, payment.ErrStaleJob) {
		t.Fatal("expired lease accepted", err)
	}
	current := claimed(t, s.Store)
	if current.LeaseVersion != old.LeaseVersion+1 {
		t.Fatal(old, current)
	}
	if err := s.CompleteAML(ctx, old, aml.Low, 3, refund.Policy{}); !errors.Is(err, payment.ErrStaleJob) {
		t.Fatal(err)
	}
	if err := s.RetryAML(ctx, old, "aml_unavailable", time.Second); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteAML(ctx, current, aml.Low, 3, refund.Policy{}); err != nil {
		t.Fatal(err)
	}
	financialState(t, s, v, map[ledger.AccountKind]money.USDT{ledger.Asset: 100_000_000, ledger.PartnerAvailable: 99_000_000, ledger.Income: 1_000_000})
	if rowCount(t, s, "refunds") != 0 {
		t.Fatal("stale high risk created refund")
	}
}
func TestProcessingReorgCancelsHoldAndStaleAML(t *testing.T) {
	s := chainFixture(t)
	ctx := context.Background()
	v, e := readyPayment(t, s, 1, "")
	job := claimed(t, s.Store)
	if _, err := s.Reorg(ctx, 3); err != nil {
		t.Fatal(err)
	}
	financialState(t, s, v, nil)
	got, err := s.GetInvoice(ctx, postgres.AlphaID, v.ID)
	if err != nil || got.Payment.Status != payment.Orphaned || got.Payment.Version != 2 {
		t.Fatal(got, err)
	}
	for _, risk := range []aml.Risk{aml.Low, aml.High} {
		if err := s.CompleteAML(ctx, job, risk, 3, refund.Policy{Enabled: true}); !errors.Is(err, payment.ErrStaleJob) {
			t.Fatal(err)
		}
	}
	var cancelled bool
	if err := s.pool.QueryRow(ctx, `SELECT status='CANCELLED' FROM aml_jobs`).Scan(&cancelled); err != nil || !cancelled {
		t.Fatal(cancelled, err)
	}
	e.LogIndex = 9
	if _, err := s.AddTransfer(ctx, e); err != nil {
		t.Fatal(err)
	}
	scanChain(t, s.Store)
	if _, err := s.Advance(ctx, 2); err != nil {
		t.Fatal(err)
	}
	w := processingWorker(s.Store, aml.NewMock(nil, false))
	if _, err := w.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetInvoice(ctx, postgres.AlphaID, v.ID)
	if err != nil || got.Payment.ID != job.PaymentID || got.Payment.Version != 3 || got.Payment.Status != payment.Credited {
		t.Fatal(got, err)
	}
	financialState(t, s, v, map[ledger.AccountKind]money.USDT{ledger.Asset: 100_000_000, ledger.PartnerAvailable: 99_000_000, ledger.Income: 1_000_000})
	if rowCount(t, s, "ledger_operations") != 4 || rowCount(t, s, "aml_results") != 1 {
		t.Fatal("bad compensation/reinclusion")
	}
}
func TestProcessingSQLFailuresRollBackStatusesAndJournal(t *testing.T) {
	for _, failure := range []string{"job", "credit", "refund"} {
		t.Run(failure, func(t *testing.T) {
			s := chainFixture(t)
			ctx := context.Background()
			v, _ := confirmingPayment(t, s, 1, "")
			if _, err := s.Advance(ctx, 2); err != nil {
				t.Fatal(err)
			}
			if failure == "job" {
				if _, err := s.pool.Exec(ctx, `ALTER TABLE aml_jobs ADD CONSTRAINT force_failure CHECK(status<>'PENDING')`); err != nil {
					t.Fatal(err)
				}
				if err := s.PrepareReady(ctx, 3); err == nil {
					t.Fatal("expected job failure")
				}
				if rowCount(t, s, "ledger_operations") != 0 || rowCount(t, s, "aml_jobs") != 0 {
					t.Fatal("partial hold")
				}
				if _, err := s.pool.Exec(ctx, `ALTER TABLE aml_jobs DROP CONSTRAINT force_failure`); err != nil {
					t.Fatal(err)
				}
				if err := s.PrepareReady(ctx, 3); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := s.PrepareReady(ctx, 3); err != nil {
					t.Fatal(err)
				}
				job := claimed(t, s.Store)
				risk := aml.Low
				sql := `ALTER TABLE invoices ADD CONSTRAINT force_failure CHECK(status<>'PAID')`
				drop := `ALTER TABLE invoices DROP CONSTRAINT force_failure`
				if failure == "refund" {
					risk = aml.High
					sql = `ALTER TABLE aml_results ADD CONSTRAINT force_failure CHECK(risk<>'HIGH')`
					drop = `ALTER TABLE aml_results DROP CONSTRAINT force_failure`
				}
				if _, err := s.pool.Exec(ctx, sql); err != nil {
					t.Fatal(err)
				}
				if err := s.CompleteAML(ctx, job, risk, 3, refund.Policy{Enabled: true}); err == nil {
					t.Fatal("expected final decision failure")
				}
				if rowCount(t, s, "ledger_operations") != 1 || rowCount(t, s, "refunds") != 0 || rowCount(t, s, "aml_results") != 0 {
					t.Fatal("partial decision")
				}
				got, err := s.GetInvoice(ctx, postgres.AlphaID, v.ID)
				if err != nil || got.Payment.Status != payment.AMLPending || got.Status != invoice.Open {
					t.Fatal(got, err)
				}
				financialState(t, s, v, map[ledger.AccountKind]money.USDT{ledger.Asset: 100_000_000, ledger.Hold: 100_000_000})
				if _, err := s.pool.Exec(ctx, drop); err != nil {
					t.Fatal(err)
				}
				if err := s.CompleteAML(ctx, job, risk, 3, refund.Policy{Enabled: true}); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
func TestProcessingOverflowDoesNotBlockAnotherPartner(t *testing.T) {
	s := chainFixture(t)
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, `UPDATE partners SET fee_bps=0`); err != nil {
		t.Fatal(err)
	}
	makePayment := func(partnerID, key string, amount money.USDT, n int) invoice.Invoice {
		t.Helper()
		v, err := s.CreateInvoice(ctx, partnerID, key, amount)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.AddTransfer(ctx, transfer(v, n)); err != nil {
			t.Fatal(err)
		}
		scanChain(t, s.Store)
		if _, err = s.Advance(ctx, 2); err != nil {
			t.Fatal(err)
		}
		if err = s.PrepareReady(ctx, 3); err != nil {
			t.Fatal(err)
		}
		return v
	}
	v := makePayment(postgres.AlphaID, "max", money.USDT(math.MaxInt64), 1)
	j := claimed(t, s.Store)
	if err := s.CompleteAML(ctx, j, aml.Low, 3, refund.Policy{}); err != nil {
		t.Fatal(err)
	}
	financialState(t, s, v, map[ledger.AccountKind]money.USDT{ledger.Asset: money.USDT(math.MaxInt64), ledger.PartnerAvailable: money.USDT(math.MaxInt64)})
	held := makePayment(postgres.AlphaID, "overflow", 1, 2)
	j = claimed(t, s.Store)
	if err := s.CompleteAML(ctx, j, aml.Low, 3, refund.Policy{}); !errors.Is(err, ledger.ErrBalance) {
		t.Fatal(err)
	}
	if err := s.RetryAML(ctx, j, "balance_limit", time.Minute); err != nil {
		t.Fatal(err)
	}
	other := makePayment(postgres.BetaID, "other", 1, 3)
	w := processingWorker(s.Store, aml.NewMock(nil, false))
	if worked, err := w.RunOnce(ctx); err != nil || !worked {
		t.Fatal(worked, err)
	}
	got, err := s.GetInvoice(ctx, postgres.AlphaID, held.ID)
	if err != nil || got.Payment.Status != payment.AMLPending {
		t.Fatal(got, err)
	}
	got, err = s.GetInvoice(ctx, postgres.BetaID, other.ID)
	if err != nil || got.Payment.Status != payment.Credited {
		t.Fatal(got, err)
	}
	b, err := s.Balance(ctx, postgres.BetaID)
	if err != nil || b.Available != 1 {
		t.Fatal(b, err)
	}
	financialState(t, s, held, map[ledger.AccountKind]money.USDT{ledger.Asset: 1, ledger.Hold: 1, ledger.PartnerAvailable: money.USDT(math.MaxInt64)})
}

func TestProcessingZeroAndFullCommission(t *testing.T) {
	for _, bps := range []int{0, 10000} {
		t.Run(fmt.Sprint(bps), func(t *testing.T) {
			s := chainFixture(t)
			ctx := context.Background()
			if _, err := s.pool.Exec(ctx, `UPDATE partners SET fee_bps=$1 WHERE id=$2`, bps, postgres.AlphaID); err != nil {
				t.Fatal(err)
			}
			v, _ := readyPayment(t, s, 1, "")
			w := processingWorker(s.Store, aml.NewMock(nil, false))
			if _, err := w.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			want := map[ledger.AccountKind]money.USDT{ledger.Asset: 100_000_000}
			if bps == 0 {
				want[ledger.PartnerAvailable] = 100_000_000
			} else {
				want[ledger.Income] = 100_000_000
			}
			financialState(t, s, v, want)
			if rowCount(t, s, "ledger_entries") != 4 {
				t.Fatal("zero entry included")
			}
		})
	}
}
