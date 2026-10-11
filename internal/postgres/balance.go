package postgres

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"crypto-payment-gateway/internal/ledger"
	"crypto-payment-gateway/internal/money"
)

func (s *Store) Balance(ctx context.Context, partnerID string) (ledger.Balance, error) {
	const query = `
		SELECT COALESCE(sum(CASE WHEN e.side = 'CREDIT' THEN e.amount ELSE -e.amount END), 0)::text
		FROM ledger_accounts a
		JOIN ledger_entries e ON e.account_id = a.id
		WHERE a.partner_id = $1 AND a.kind = $2`
	var rawBalance string
	if err := s.pool.QueryRow(ctx, query, partnerID, string(ledger.PartnerAvailable)).Scan(&rawBalance); err != nil {
		return ledger.Balance{}, fmt.Errorf("read partner balance: %w", err)
	}
	amount, err := strconv.ParseInt(rawBalance, 10, 64)
	if err != nil || amount < 0 {
		return ledger.Balance{}, errors.New("invalid ledger balance")
	}
	return ledger.Balance{Currency: money.Currency, Available: money.USDT(amount)}, nil
}
