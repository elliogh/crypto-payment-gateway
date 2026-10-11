package postgres

import (
	"context"
	"errors"
	"strings"

	"crypto-payment-gateway/internal/refund"
	"github.com/jackc/pgx/v5"
)

const refundColumns = `id::text,payment_id::text,amount,recipient,reason,status,created_at`

func scanRefund(row pgx.Row) (refund.Refund, error) {
	var r refund.Refund
	err := row.Scan(&r.ID, &r.PaymentID, &r.Amount, &r.Recipient, &r.Reason, &r.Status, &r.CreatedAt)
	return r, err
}

// Explicit local approval asserts that this recipient is a confirmed safe route.
// No new postings are needed: READY still represents an outstanding obligation.
func (s *Store) ApproveRefund(ctx context.Context, id, recipient string) (refund.Refund, error) {
	recipient = strings.ToLower(recipient)
	if !refund.ValidRecipient(recipient) {
		return refund.Refund{}, refund.ErrRecipient
	}
	tx, _, err := s.beginLedgerChain(ctx)
	if err != nil {
		return refund.Refund{}, err
	}
	defer tx.Rollback(context.Background())
	var paymentID string
	err = tx.QueryRow(ctx, `SELECT payment_id::text FROM refunds WHERE id=$1`, id).Scan(&paymentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return refund.Refund{}, refund.ErrNotFound
	}
	if err != nil {
		return refund.Refund{}, err
	}
	if _, err = lockPayment(ctx, tx, paymentID); err != nil {
		return refund.Refund{}, err
	}
	r, err := scanRefund(tx.QueryRow(ctx, `SELECT `+refundColumns+` FROM refunds WHERE id=$1 FOR UPDATE`, id))
	if err != nil {
		return r, err
	}
	if r.Status == refund.Ready {
		if r.Recipient != recipient {
			return r, refund.ErrConflict
		}
	} else {
		r, err = scanRefund(tx.QueryRow(ctx, `UPDATE refunds SET recipient=$2,status='READY',reason='approved_return_route' WHERE id=$1 RETURNING `+refundColumns, id, recipient))
		if err != nil {
			return r, err
		}
	}
	return r, tx.Commit(ctx)
}
