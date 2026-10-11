package postgres

import (
	"context"
	"fmt"

	"crypto-payment-gateway/internal/chain"
	"crypto-payment-gateway/internal/ledger"
	"crypto-payment-gateway/internal/money"
	"crypto-payment-gateway/internal/payment"
)

func (s *Store) beginLedgerChain(ctx context.Context) (*LedgerTx, chain.Block, error) {
	tx, err := s.BeginLedgerTx(ctx)
	if err != nil {
		return nil, chain.Block{}, err
	}
	head, err := lockChain(ctx, tx.tx)
	if err != nil {
		_ = tx.Rollback(context.Background())
		return nil, chain.Block{}, err
	}
	return tx, head, nil
}

type processingPayment struct {
	ID, InvoiceID, PartnerID, Sender string
	Amount                           money.USDT
	FeeBPS                           int
	Version                          int64
	Status                           payment.Status
	Height                           int64
	Canonical                        bool
}

// The chain lock precedes all business locks. Read state after acquiring them.
func lockPayment(ctx context.Context, tx *LedgerTx, id string) (processingPayment, error) {
	var invoiceID string
	if err := tx.QueryRow(ctx, `SELECT invoice_id::text FROM payments WHERE id=$1`, id).Scan(&invoiceID); err != nil {
		return processingPayment{}, err
	}
	if err := lockDepositRows(ctx, tx.tx, []string{invoiceID}); err != nil {
		return processingPayment{}, err
	}
	var p processingPayment
	err := tx.QueryRow(ctx, `SELECT p.id::text,p.invoice_id::text,i.partner_id::text,p.sender,p.amount,i.fee_bps,
		p.observation_version,p.status,b.height,o.canonical AND b.canonical FROM payments p
		JOIN invoices i ON i.id=p.invoice_id JOIN payment_observations o ON o.payment_id=p.id AND o.version=p.observation_version
		JOIN chain_blocks b ON b.hash=o.block_hash WHERE p.id=$1`, id).
		Scan(&p.ID, &p.InvoiceID, &p.PartnerID, &p.Sender, &p.Amount, &p.FeeBPS, &p.Version, &p.Status, &p.Height, &p.Canonical)
	return p, err
}

func readPaymentAccounts(ctx context.Context, tx *LedgerTx, p processingPayment) (payment.Accounts, error) {
	rows, err := tx.tx.Query(ctx, `SELECT kind,id::text FROM ledger_accounts WHERE currency='USDT' AND
		(invoice_id=$1 OR (kind='PARTNER_AVAILABLE' AND partner_id=$2) OR kind='INCOME')`, p.InvoiceID, p.PartnerID)
	if err != nil {
		return payment.Accounts{}, err
	}
	defer rows.Close()
	var a payment.Accounts
	for rows.Next() {
		var kind ledger.AccountKind
		var id string
		if err := rows.Scan(&kind, &id); err != nil {
			return a, err
		}
		switch kind {
		case ledger.Asset:
			a.Asset = id
		case ledger.Hold:
			a.Hold = id
		case ledger.PartnerAvailable:
			a.Available = id
		case ledger.Income:
			a.Income = id
		case ledger.Refund:
			a.Refund = id
		}
	}
	if err := rows.Err(); err != nil {
		return a, err
	}
	if a.Asset == "" || a.Hold == "" || a.Available == "" || a.Income == "" || a.Refund == "" {
		return a, ledger.ErrEntry
	}
	return a, nil
}

// Callers hold all affected invoice/payment locks. Cancel holds before changing
// versions, so the old observation's operation keys and jobs remain traceable.
func (s *Store) orphanDeposits(ctx context.Context, tx *LedgerTx) error {
	const removed = `FROM payments p JOIN payment_observations o ON o.payment_id=p.id AND o.version=p.observation_version
		JOIN chain_blocks b ON b.hash=o.block_hash WHERE o.canonical AND NOT b.canonical`
	rows, err := tx.tx.Query(ctx, `SELECT p.id::text,p.invoice_id::text,i.partner_id::text,p.amount,p.observation_version,p.status
		FROM payments p JOIN invoices i ON i.id=p.invoice_id JOIN payment_observations o ON o.payment_id=p.id AND o.version=p.observation_version
		JOIN chain_blocks b ON b.hash=o.block_hash WHERE o.canonical AND NOT b.canonical ORDER BY p.id`)
	if err != nil {
		return err
	}
	var removedPayments []processingPayment
	for rows.Next() {
		var p processingPayment
		if err = rows.Scan(&p.ID, &p.InvoiceID, &p.PartnerID, &p.Amount, &p.Version, &p.Status); err != nil {
			break
		}
		removedPayments = append(removedPayments, p)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	var allEntries []ledger.Entry
	postings := make(map[string][]ledger.Entry)
	for _, p := range removedPayments {
		if p.Status != payment.Confirming && p.Status != payment.AMLPending {
			return chain.ErrConflict
		}
		if p.Status == payment.AMLPending {
			a, err := readPaymentAccounts(ctx, tx, p)
			if err != nil {
				return err
			}
			entries := a.CancelHoldEntries(p.Amount)
			postings[p.ID] = entries
			allEntries = append(allEntries, entries...)
		}
	}
	// Multiple reversals share one transaction: acquire the complete account set
	// in UUID order before posting the first operation.
	if len(allEntries) > 0 {
		if _, err := lockLedgerAccounts(ctx, tx.tx, allEntries); err != nil {
			return err
		}
	}
	for _, p := range removedPayments {
		if entries, ok := postings[p.ID]; ok {
			if _, err := s.PostLedgerTx(ctx, tx, payment.ObservationKey(p.ID, p.Version, "cancel_hold"), entries); err != nil {
				return fmt.Errorf("cancel payment hold: %w", err)
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE aml_jobs SET status='CANCELLED',lease_until=NULL WHERE payment_id=$1 AND status IN ('PENDING','RUNNING')`, p.ID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO payment_observations(payment_id,version,block_hash,log_index,canonical)
		SELECT p.id,p.observation_version+1,o.block_hash,o.log_index,false `+removed); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE payments SET status='ORPHANED',observation_version=observation_version+1 WHERE id IN(SELECT p.id `+removed+`)`); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE payment_observations SET canonical=false WHERE canonical AND block_hash IN(SELECT hash FROM chain_blocks WHERE NOT canonical)`)
	return err
}
