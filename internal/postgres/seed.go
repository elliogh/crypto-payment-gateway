package postgres

import (
	"context"
	"errors"
	"fmt"

	"crypto-payment-gateway/internal/ledger"
	"crypto-payment-gateway/internal/partner"
	"github.com/google/uuid"
)

const (
	AlphaID = "00000000-0000-4000-8000-000000000001"
	BetaID  = "00000000-0000-4000-8000-000000000002"
)

func (s *Store) Seed(ctx context.Context, alphaKey, betaKey string) error {
	if alphaKey == betaKey || !partner.ValidAPIKey(alphaKey) || !partner.ValidAPIKey(betaKey) {
		return errors.New("invalid demo partner keys")
	}
	const query = `
		INSERT INTO partners (id, name, api_key_hash, fee_bps)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (id) DO UPDATE SET api_key_hash = excluded.api_key_hash`

	demoPartners := []struct {
		id     string
		name   string
		key    string
		feeBPS int
	}{
		{id: AlphaID, name: "alpha", key: alphaKey, feeBPS: 100},
		{id: BetaID, name: "beta", key: betaKey, feeBPS: 250},
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin partner initialization: %w", err)
	}
	defer tx.Rollback(context.Background())
	// Serialize initialization, including the first startup when no rows exist.
	// Readers keep seeing the old committed keys until the complete rotation commits.
	if _, err := tx.Exec(ctx, `LOCK TABLE partners IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return fmt.Errorf("lock partner initialization: %w", err)
	}
	// Free both old hashes before assigning either new hash: this permits swaps
	// with the existing immediate UNIQUE constraint, without changing migrations.
	for _, demo := range demoPartners {
		placeholder, err := uuid.NewRandom()
		if err != nil {
			return fmt.Errorf("generate temporary partner key: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE partners SET api_key_hash=$2 WHERE id=$1`, demo.id, keyHash(placeholder.String())); err != nil {
			return fmt.Errorf("prepare partner key rotation: %w", err)
		}
	}
	for _, demo := range demoPartners {
		if _, err := tx.Exec(ctx, query, demo.id, demo.name, keyHash(demo.key), demo.feeBPS); err != nil {
			return fmt.Errorf("initialize demo partner: %w", err)
		}
		if err := insertAccount(ctx, tx, ledger.PartnerAvailable, &demo.id, nil); err != nil {
			return err
		}
	}
	if err := insertAccount(ctx, tx, ledger.Income, nil, nil); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit partner initialization: %w", err)
	}
	return nil
}
