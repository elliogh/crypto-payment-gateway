package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"crypto-payment-gateway/internal/partner"
	"github.com/jackc/pgx/v5"
)

func keyHash(key string) string {
	hash := sha256.Sum256([]byte(key))
	return hex.EncodeToString(hash[:])
}

func (s *Store) Authenticate(ctx context.Context, key string) (partner.Partner, error) {
	const query = `
		SELECT id::text, name, fee_bps
		FROM partners
		WHERE api_key_hash = $1`

	var found partner.Partner
	err := s.pool.QueryRow(ctx, query, keyHash(key)).Scan(&found.ID, &found.Name, &found.FeeBPS)
	if errors.Is(err, pgx.ErrNoRows) {
		return partner.Partner{}, partner.ErrUnauthorized
	}
	if err != nil {
		return partner.Partner{}, fmt.Errorf("authenticate partner: %w", err)
	}
	return found, nil
}
