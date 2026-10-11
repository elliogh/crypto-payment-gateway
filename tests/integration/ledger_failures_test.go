package integration

import (
	"context"
	"errors"
	"math"
	"testing"

	"crypto-payment-gateway/internal/invoice"
	"crypto-payment-gateway/internal/ledger"
	"crypto-payment-gateway/internal/money"
	"crypto-payment-gateway/internal/postgres"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestValidationAndCancellationRollBackFinancialTransaction(t *testing.T) {
	for _, name := range []string{"unbalanced", "single entry", "cancelled", "savepoint"} {
		t.Run(name, func(t *testing.T) {
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
			entries := []ledger.Entry{
				{AccountID: a[ledger.Hold], Side: ledger.Debit, Amount: 80},
				{AccountID: a[ledger.PartnerAvailable], Side: ledger.Credit, Amount: 80},
			}
			want := ledger.ErrEntry
			switch name {
			case "unbalanced":
				entries[1].Amount = 79
			case "single entry":
				entries = entries[:1]
			case "savepoint":
				// The status precedes the savepoint. A posting error must still
				// roll back the root transaction and every successful posting.
				if _, err := tx.Exec(ctx, `SAVEPOINT nested`); err != nil {
					t.Fatal(err)
				}
				if _, err := s.PostLedgerTx(ctx, tx, "before-failure", entries); err != nil {
					t.Fatal(err)
				}
				entries[1].Amount = 79
			case "cancelled":
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
				want = context.Canceled
			}
			if _, err := s.PostLedgerTx(ctx, tx, "failed-posting", entries); !errors.Is(err, want) {
				t.Fatal("original failure was lost", err)
			}
			if err := tx.Commit(context.Background()); !errors.Is(err, pgx.ErrTxClosed) {
				t.Fatal("ignored posting error allowed commit", err)
			}
			assertFailedPostingRolledBack(t, s, v, "failed-posting")
		})
	}
}

func TestSQLFailureAfterFirstEntryRollsBackEverything(t *testing.T) {
	s := fixture(t)
	v, a := fundedInvoice(t, s, 100)
	ctx := context.Background()
	// Existing funding rows stay valid; new credits fail after the preceding debit.
	if _, err := s.pool.Exec(ctx, `ALTER TABLE ledger_entries ADD CONSTRAINT fail_new_credit CHECK (side <> 'CREDIT') NOT VALID`); err != nil {
		t.Fatal(err)
	}
	var before int64
	if err := s.pool.QueryRow(ctx, `SELECT last_value FROM ledger_entries_id_seq`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	tx, err := s.BeginLedgerTx(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `UPDATE invoices SET status='PAID' WHERE id=$1`, v.ID); err != nil {
		t.Fatal(err)
	}
	_, err = s.PostLedgerTx(ctx, tx, "sql-failure", []ledger.Entry{
		{AccountID: a[ledger.Hold], Side: ledger.Debit, Amount: 80},
		{AccountID: a[ledger.PartnerAvailable], Side: ledger.Credit, Amount: 80},
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "fail_new_credit" {
		t.Fatal("original SQL failure was lost", err)
	}
	if err := tx.Commit(ctx); !errors.Is(err, pgx.ErrTxClosed) {
		t.Fatal("SQL failure left the transaction open", err)
	}
	var after int64
	if err := s.pool.QueryRow(ctx, `SELECT last_value FROM ledger_entries_id_seq`).Scan(&after); err != nil || after != before+2 {
		t.Fatal("failure did not exercise both entry inserts", before, after, err)
	}
	assertFailedPostingRolledBack(t, s, v, "sql-failure")
}

func assertFailedPostingRolledBack(t *testing.T, s *databaseFixture, v invoice.Invoice, key string) {
	t.Helper()
	ctx := context.Background()
	got, err := s.GetInvoice(ctx, postgres.AlphaID, v.ID)
	if err != nil || got.Status != invoice.Open {
		t.Fatal("failed posting committed the invoice status", got, err)
	}
	var operations, entries int
	if err := s.pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM ledger_operations WHERE business_key=$1),
		(SELECT count(*) FROM ledger_entries)`, key).Scan(&operations, &entries); err != nil || operations != 0 || entries != 2 {
		t.Fatal("failed posting persisted partial journal rows", operations, entries, err)
	}
	if got, err := s.Balance(ctx, postgres.AlphaID); err != nil || got.Available != 0 {
		t.Fatal("failed posting changed the partner balance", got, err)
	}
}

func TestReplayAndSeedPreserveJournalAndAccountIdentity(t *testing.T) {
	s := fixture(t)
	_, a := fundedInvoice(t, s, math.MaxInt64)
	ctx := context.Background()
	entries := []ledger.Entry{
		{AccountID: a[ledger.Asset], Side: ledger.Debit, Amount: money.USDT(math.MaxInt64)},
		{AccountID: a[ledger.Hold], Side: ledger.Credit, Amount: money.USDT(math.MaxInt64)},
	}
	var originalID string
	if err := s.pool.QueryRow(ctx, `SELECT id::text FROM ledger_operations WHERE business_key='funding'`).Scan(&originalID); err != nil {
		t.Fatal(err)
	}
	snapshot := func() string {
		t.Helper()
		var data string
		if err := s.pool.QueryRow(ctx, `SELECT jsonb_build_object(
			'accounts', (SELECT jsonb_agg(a ORDER BY a.id) FROM ledger_accounts a),
			'operations', (SELECT jsonb_agg(o ORDER BY o.id) FROM ledger_operations o),
			'entries', (SELECT jsonb_agg(e ORDER BY e.id) FROM ledger_entries e)
		)::text`).Scan(&data); err != nil {
			t.Fatal(err)
		}
		return data
	}
	before := snapshot()
	// A valid replay must succeed even though a new deposit would now overflow.
	if id, err := s.PostLedger(ctx, "funding", entries); err != nil || id != originalID {
		t.Fatal("replay re-applied the operation", id, err)
	}
	entries[0].Amount--
	entries[1].Amount--
	if _, err := s.PostLedger(ctx, "funding", entries); !errors.Is(err, ledger.ErrConflict) {
		t.Fatal("existing operation can be rewritten", err)
	}
	if err := s.Seed(ctx, "changed-alpha-key", "changed-beta-key"); err != nil {
		t.Fatal(err)
	}
	if after := snapshot(); after != before {
		t.Fatal("replay, conflicting replay or seed changed existing financial data")
	}
}
