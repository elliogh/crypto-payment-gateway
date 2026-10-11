package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// LedgerTx owns a root transaction created by BeginLedgerTx. It deliberately
// exposes neither Begin nor Conn: a savepoint cannot replace the transaction
// that PostLedgerTx must roll back together with financial status changes.
type LedgerTx struct {
	tx pgx.Tx
}

func (s *Store) BeginLedgerTx(ctx context.Context) (*LedgerTx, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("begin ledger transaction: %w", err)
	}
	return &LedgerTx{tx: tx}, nil
}

func (tx *LedgerTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return tx.tx.Exec(ctx, sql, args...)
}

func (tx *LedgerTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return tx.tx.QueryRow(ctx, sql, args...)
}

func (tx *LedgerTx) Commit(ctx context.Context) error {
	return tx.tx.Commit(ctx)
}

func (tx *LedgerTx) Rollback(ctx context.Context) error {
	return tx.tx.Rollback(ctx)
}
