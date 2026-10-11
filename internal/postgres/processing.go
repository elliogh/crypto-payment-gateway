package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"crypto-payment-gateway/internal/aml"
	"crypto-payment-gateway/internal/chain"
	"crypto-payment-gateway/internal/payment"
	"crypto-payment-gateway/internal/refund"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var _ payment.Processing = (*Store)(nil)

func (s *Store) PrepareReady(ctx context.Context, confirmations int) error {
	if confirmations < 1 {
		return payment.ErrNotConfirmed
	}
	rows, err := s.pool.Query(ctx, `SELECT p.id::text FROM payments p
		JOIN payment_observations o ON o.payment_id=p.id AND o.version=p.observation_version
		JOIN chain_blocks b ON b.hash=o.block_hash JOIN chain_state c ON c.chain_id=1 JOIN chain_blocks h ON h.hash=c.head_hash
		WHERE p.status='CONFIRMING' AND o.canonical AND b.canonical AND h.height-b.height+1 >= $1 ORDER BY p.id LIMIT 100`, confirmations)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	var failures []error
	for _, id := range ids {
		if err := s.preparePayment(ctx, id, confirmations); err != nil {
			failures = append(failures, err)
		}
		if ctx.Err() != nil {
			break
		}
	}
	return errors.Join(failures...)
}

func (s *Store) preparePayment(ctx context.Context, id string, confirmations int) error {
	tx, head, err := s.beginLedgerChain(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	p, err := lockPayment(ctx, tx, id)
	if err != nil {
		return err
	}
	if p.Status != payment.Confirming || chain.Confirmations(head.Height, p.Height, p.Canonical) < int64(confirmations) {
		return nil
	}
	a, err := readPaymentAccounts(ctx, tx, p)
	if err != nil {
		return err
	}
	if _, err = s.PostLedgerTx(ctx, tx, payment.ObservationKey(p.ID, p.Version, "hold"), a.HoldEntries(p.Amount)); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE payments SET status='AML_PENDING' WHERE id=$1`, id); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO aml_jobs(payment_id,observation_version,status) VALUES($1,$2,'PENDING')`, id, p.Version); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) ClaimAML(ctx context.Context, lease time.Duration) (payment.Job, bool, error) {
	if lease <= 0 {
		return payment.Job{}, false, payment.ErrStaleJob
	}
	var j payment.Job
	err := s.pool.QueryRow(ctx, `WITH due AS (
		SELECT payment_id,observation_version FROM aml_jobs WHERE
		(status='PENDING' AND next_attempt_at<=clock_timestamp()) OR (status='RUNNING' AND lease_until<=clock_timestamp())
		ORDER BY next_attempt_at,payment_id,observation_version FOR UPDATE SKIP LOCKED LIMIT 1
	), claimed AS (
		UPDATE aml_jobs j SET status='RUNNING',attempts=attempts+1,lease_version=lease_version+1,
		lease_until=clock_timestamp()+$1::interval FROM due
		WHERE j.payment_id=due.payment_id AND j.observation_version=due.observation_version
		RETURNING j.payment_id,j.observation_version,j.lease_version
	) SELECT c.payment_id::text,c.observation_version,c.lease_version,p.sender FROM claimed c JOIN payments p ON p.id=c.payment_id`, lease.String()).
		Scan(&j.PaymentID, &j.ObservationVersion, &j.LeaseVersion, &j.Sender)
	if errors.Is(err, pgx.ErrNoRows) {
		return j, false, nil
	}
	return j, err == nil, err
}

func (s *Store) CompleteAML(ctx context.Context, j payment.Job, risk aml.Risk, confirmations int, policy refund.Policy) error {
	if risk != aml.Low && risk != aml.High {
		return aml.ErrRisk
	}
	if confirmations < 1 {
		return payment.ErrNotConfirmed
	}
	tx, head, err := s.beginLedgerChain(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	p, err := lockPayment(ctx, tx, j.PaymentID)
	if err != nil {
		return err
	}
	if p.Version != j.ObservationVersion || p.Status != payment.AMLPending || !p.Canonical {
		return payment.ErrStaleJob
	}
	// Lock first, then evaluate time in a fresh statement after any lock wait.
	var version int64
	var status string
	err = tx.QueryRow(ctx, `SELECT lease_version,status FROM aml_jobs WHERE payment_id=$1 AND observation_version=$2 FOR UPDATE`, j.PaymentID, j.ObservationVersion).Scan(&version, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return payment.ErrStaleJob
	}
	if err != nil {
		return err
	}
	if status != "RUNNING" || version != j.LeaseVersion {
		return payment.ErrStaleJob
	}
	var active bool
	if err = tx.QueryRow(ctx, `SELECT coalesce(lease_until>clock_timestamp(),false) FROM aml_jobs WHERE payment_id=$1 AND observation_version=$2`, j.PaymentID, j.ObservationVersion).Scan(&active); err != nil {
		return err
	}
	if !active {
		return payment.ErrStaleJob
	}
	if chain.Confirmations(head.Height, p.Height, p.Canonical) < int64(confirmations) {
		return payment.ErrNotConfirmed
	}
	a, err := readPaymentAccounts(ctx, tx, p)
	if err != nil {
		return err
	}
	if risk == aml.Low {
		entries, err := a.CreditEntries(p.Amount, p.FeeBPS)
		if err != nil {
			return err
		}
		if _, err = s.PostLedgerTx(ctx, tx, payment.CreditKey(p.ID), entries); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE payments SET status='CREDITED' WHERE id=$1`, p.ID); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE invoices SET status='PAID' WHERE id=$1`, p.InvoiceID); err != nil {
			return err
		}
	} else {
		if _, err = s.PostLedgerTx(ctx, tx, payment.ObservationKey(p.ID, p.Version, "refund"), a.RefundEntries(p.Amount)); err != nil {
			return err
		}
		id, err := uuid.NewRandom()
		if err != nil {
			return fmt.Errorf("generate refund ID: %w", err)
		}
		status, reason := policy.Decide(p.Sender)
		if _, err = tx.Exec(ctx, `INSERT INTO refunds(id,payment_id,amount,recipient,reason,status) VALUES($1,$2,$3,$4,$5,$6)`, id.String(), p.ID, int64(p.Amount), p.Sender, reason, string(status)); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE payments SET status='REJECTED' WHERE id=$1`, p.ID); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(ctx, `INSERT INTO aml_results(payment_id,observation_version,risk) VALUES($1,$2,$3)`, p.ID, p.Version, string(risk)); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE aml_jobs SET status='DONE',lease_until=NULL,last_error=NULL WHERE payment_id=$1 AND observation_version=$2`, p.ID, p.Version); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) RetryAML(ctx context.Context, j payment.Job, reason string, delay time.Duration) error {
	if delay <= 0 {
		return payment.ErrStaleJob
	}
	// Store only known codes, never a provider/SQL error message.
	switch reason {
	case "aml_unavailable", "invalid_aml_result", "balance_limit", "awaiting_confirmations", "processing_failed":
	default:
		return payment.ErrStaleJob
	}
	_, err := s.pool.Exec(ctx, `UPDATE aml_jobs SET status='PENDING',lease_until=NULL,last_error=$4,next_attempt_at=clock_timestamp()+$5::interval
		WHERE payment_id=$1 AND observation_version=$2 AND lease_version=$3 AND status='RUNNING' AND lease_until>clock_timestamp()`,
		j.PaymentID, j.ObservationVersion, j.LeaseVersion, reason, delay.String())
	return err
}
