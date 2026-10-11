package postgres

import (
	"context"

	"crypto-payment-gateway/internal/chain"
)

// Reorg is the local control operation: source rollback, hold cancellation and
// observation changes commit together. Final financial decisions reject rollback.
func (s *Store) Reorg(ctx context.Context, depth int) (chain.Block, error) {
	if depth < 1 || depth > chain.MaxBlocks {
		return chain.Block{}, chain.ErrInvalid
	}
	tx, head, err := s.beginLedgerChain(ctx)
	if err != nil {
		return chain.Block{}, err
	}
	defer tx.Rollback(context.Background())
	if int64(depth) > head.Height {
		return chain.Block{}, chain.ErrInvalid
	}
	var ancestor chain.Block
	err = tx.QueryRow(ctx, `SELECT hash,height,coalesce(parent_hash,''),timestamp FROM chain_blocks WHERE canonical AND height=$1`, head.Height-int64(depth)).
		Scan(&ancestor.Hash, &ancestor.Height, &ancestor.ParentHash, &ancestor.Timestamp)
	if err != nil {
		return chain.Block{}, err
	}
	rows, err := tx.tx.Query(ctx, `SELECT p.invoice_id::text FROM payments p JOIN payment_observations o ON o.payment_id=p.id AND o.version=p.observation_version
		JOIN chain_blocks b ON b.hash=o.block_hash WHERE o.canonical AND b.canonical AND b.height>$1 ORDER BY p.invoice_id`, ancestor.Height)
	if err != nil {
		return chain.Block{}, err
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
		return chain.Block{}, err
	}
	if err = lockDepositRows(ctx, tx.tx, ids); err != nil {
		return chain.Block{}, err
	}
	// Any failure below rolls the complete source/financial transaction back.
	if err = rollbackChain(ctx, tx.tx, ancestor); err != nil {
		return chain.Block{}, err
	}
	if err = s.orphanDeposits(ctx, tx); err != nil {
		return chain.Block{}, err
	}
	return ancestor, tx.Commit(ctx)
}
