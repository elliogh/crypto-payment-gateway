package postgres

import (
	"context"

	"crypto-payment-gateway/internal/invoice"
	"crypto-payment-gateway/internal/ledger"
	"crypto-payment-gateway/internal/partner"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store owns SQL operations; connection lifetime belongs to the application.
type Store struct {
	pool *pgxpool.Pool
}

var (
	_ partner.Authenticator = (*Store)(nil)
	_ invoice.Repository    = (*Store)(nil)
	_ ledger.BalanceReader  = (*Store)(nil)
)

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func (s *Store) Ready(ctx context.Context) error {
	return s.pool.Ping(ctx)
}
