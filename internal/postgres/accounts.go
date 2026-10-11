package postgres

import (
	"context"
	"fmt"

	"crypto-payment-gateway/internal/ledger"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func insertAccount(ctx context.Context, tx pgx.Tx, kind ledger.AccountKind, partnerID, invoiceID *string) error {
	const query = `
		INSERT INTO ledger_accounts (id, kind, partner_id, invoice_id)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (kind, partner_id, invoice_id, currency) DO NOTHING`

	id, err := uuid.NewRandom()
	if err != nil {
		return fmt.Errorf("generate ledger account ID: %w", err)
	}
	if _, err := tx.Exec(ctx, query, id.String(), string(kind), partnerID, invoiceID); err != nil {
		return fmt.Errorf("insert %s account: %w", kind, err)
	}
	return nil
}
