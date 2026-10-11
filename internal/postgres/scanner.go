package postgres

import (
	"context"
	"errors"
	"fmt"

	"crypto-payment-gateway/internal/chain"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var _ chain.Deposits = (*Store)(nil)

func (s *Store) ApplyScan(ctx context.Context, batch chain.Batch, candidates []chain.Candidate) error {
	tx, _, err := s.beginChainChange(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	var version int64
	var cursor string
	if err = tx.QueryRow(ctx, `SELECT version,scan_hash FROM chain_state WHERE chain_id=1`).Scan(&version, &cursor); err != nil {
		return err
	}
	if version != batch.Version || cursor != batch.Cursor {
		return chain.ErrStale
	}
	// Resolve and lock every affected invoice before locking or creating payments.
	var recipients []string
	for _, c := range candidates {
		recipients = append(recipients, c.Event.Recipient)
	}
	rows, err := tx.Query(ctx, `SELECT id::text,deposit_address FROM invoices
		WHERE deposit_address=ANY($1::text[]) OR id IN (
			SELECT p.invoice_id FROM payments p
			JOIN payment_observations o ON o.payment_id=p.id AND o.version=p.observation_version
			JOIN chain_blocks b ON b.hash=o.block_hash WHERE o.canonical AND NOT b.canonical)
		ORDER BY id`, recipients)
	if err != nil {
		return err
	}
	byAddress := map[string]string{}
	var invoiceIDs []string
	for rows.Next() {
		var id, address string
		if err = rows.Scan(&id, &address); err != nil {
			break
		}
		invoiceIDs = append(invoiceIDs, id)
		byAddress[address] = id
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	if err = lockDepositRows(ctx, tx, invoiceIDs); err != nil {
		return err
	}
	// Reconcile removed observations even if no replacement blocks exist yet.
	if err = orphanDeposits(ctx, tx); err != nil {
		return err
	}
	for _, c := range candidates {
		id, ok := byAddress[c.Event.Recipient]
		if !ok {
			continue
		}
		if err = recordDeposit(ctx, tx, id, c); err != nil {
			return fmt.Errorf("record deposit: %w", err)
		}
	}
	if len(batch.Blocks) > 0 {
		last := batch.Blocks[len(batch.Blocks)-1]
		if _, err = tx.Exec(ctx, `UPDATE chain_state SET scan_hash=$1 WHERE chain_id=1`, last.Hash); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// Stage 2 has no financial states. Stage 3 must add hold/job cancellation here
// and coordinate final financial decisions with the local reorg command.
func orphanDeposits(ctx context.Context, tx pgx.Tx) error {
	const removed = `FROM payments p
		JOIN payment_observations o ON o.payment_id=p.id AND o.version=p.observation_version
		JOIN chain_blocks b ON b.hash=o.block_hash WHERE o.canonical AND NOT b.canonical`
	var unsupported bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 `+removed+` AND p.status<>'CONFIRMING')`).Scan(&unsupported); err != nil {
		return err
	}
	if unsupported {
		return chain.ErrConflict
	}
	if _, err := tx.Exec(ctx, `INSERT INTO payment_observations(payment_id,version,block_hash,log_index,canonical)
		SELECT p.id,p.observation_version+1,o.block_hash,o.log_index,false `+removed); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE payments SET status='ORPHANED',observation_version=observation_version+1
		WHERE id IN (SELECT p.id `+removed+`)`); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE payment_observations SET canonical=false
		WHERE canonical AND block_hash IN (SELECT hash FROM chain_blocks WHERE NOT canonical)`)
	return err
}

func recordDeposit(ctx context.Context, tx pgx.Tx, invoiceID string, c chain.Candidate) error {
	// Invalid or unsupported raw input is skipped without blocking other deposits.
	var expected int64
	if err := tx.QueryRow(ctx, `SELECT amount FROM invoices WHERE id=$1`, invoiceID).Scan(&expected); err != nil {
		return err
	}
	if expected != int64(c.Amount) {
		return nil
	}
	var boundHash string
	var boundIndex int
	err := tx.QueryRow(ctx, `INSERT INTO invoice_transfer_bindings(invoice_id,chain_id,tx_hash,tx_event_index)
		VALUES($1,1,$2,$3) ON CONFLICT(invoice_id) DO UPDATE SET invoice_id=excluded.invoice_id
		RETURNING tx_hash,tx_event_index`, invoiceID, c.Event.TxHash, c.Event.EventIndex).Scan(&boundHash, &boundIndex)
	if err != nil {
		return err
	}
	if boundHash != c.Event.TxHash || boundIndex != c.Event.EventIndex {
		return nil
	}
	var paymentID, status string
	var version int64
	err = tx.QueryRow(ctx, `SELECT id::text,status,observation_version FROM payments WHERE invoice_id=$1`, invoiceID).Scan(&paymentID, &status, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		id, err := uuid.NewRandom()
		if err != nil {
			return fmt.Errorf("generate payment ID: %w", err)
		}
		paymentID = id.String()
		version = 1
		_, err = tx.Exec(ctx, `INSERT INTO payments(id,invoice_id,chain_id,tx_hash,tx_event_index,amount,sender,observation_version,status)
			VALUES($1,$2,1,$3,$4,$5,$6,1,'CONFIRMING')`, paymentID, invoiceID, c.Event.TxHash, c.Event.EventIndex, int64(c.Amount), c.Event.Sender)
		if err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else {
		if status != "ORPHANED" {
			return nil
		}
		version++
		if _, err = tx.Exec(ctx, `UPDATE payments SET status='CONFIRMING',observation_version=$2 WHERE id=$1`, paymentID, version); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO payment_observations(payment_id,version,block_hash,log_index,canonical) VALUES($1,$2,$3,$4,true)`,
		paymentID, version, c.BlockHash, c.Event.LogIndex)
	return err
}

// lockDepositRows locks business rows after chain_state; never called by the simulator.
// Future AML transactions must acquire these locks before ledger account locks.
func lockDepositRows(ctx context.Context, tx pgx.Tx, invoiceIDs []string) error {
	for _, query := range []string{
		`SELECT id FROM invoices WHERE id=ANY($1::uuid[]) ORDER BY id FOR UPDATE`,
		`SELECT id FROM payments WHERE invoice_id=ANY($1::uuid[]) ORDER BY id FOR UPDATE`,
	} {
		rows, err := tx.Query(ctx, query, invoiceIDs)
		if err != nil {
			return err
		}
		for rows.Next() { /* acquire every row lock in deterministic order */
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	return nil
}
