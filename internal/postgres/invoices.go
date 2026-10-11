package postgres

import (
	"context"
	"errors"
	"fmt"

	"crypto-payment-gateway/internal/invoice"
	"crypto-payment-gateway/internal/ledger"
	"crypto-payment-gateway/internal/money"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const invoiceColumns = `
	i.id::text, i.amount, i.currency, i.deposit_address,
	i.fee_bps, i.status, i.created_at`

func scanInvoice(row pgx.Row) (invoice.Invoice, error) {
	var found invoice.Invoice
	err := row.Scan(
		&found.ID, &found.Amount, &found.Currency, &found.DepositAddress,
		&found.FeeBPS, &found.Status, &found.CreatedAt,
	)
	return found, err
}

func (s *Store) CreateInvoice(ctx context.Context, partnerID, key string, amount money.USDT) (invoice.Invoice, error) {
	if err := invoice.ValidateCreate(amount, key); err != nil {
		return invoice.Invoice{}, err
	}
	id, err := uuid.NewRandom()
	if err != nil {
		return invoice.Invoice{}, fmt.Errorf("generate invoice ID: %w", err)
	}
	address, err := invoice.NewTestDepositAddress()
	if err != nil {
		return invoice.Invoice{}, fmt.Errorf("generate deposit address: %w", err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return invoice.Invoice{}, fmt.Errorf("begin invoice creation: %w", err)
	}
	defer tx.Rollback(context.Background())

	const reserveQuery = `
		INSERT INTO invoice_idempotency (partner_id, key, amount, invoice_id)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (partner_id, key) DO NOTHING
		RETURNING invoice_id::text`
	var reservedID string
	err = tx.QueryRow(ctx, reserveQuery, partnerID, key, int64(amount), id.String()).Scan(&reservedID)
	var result invoice.Invoice
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		result, err = replayInvoice(ctx, tx, partnerID, key, amount)
	case err != nil:
		return invoice.Invoice{}, fmt.Errorf("reserve invoice key: %w", err)
	default:
		result, err = createInvoice(ctx, tx, partnerID, reservedID, address, amount)
	}
	if err != nil {
		return invoice.Invoice{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return invoice.Invoice{}, fmt.Errorf("commit invoice creation or replay: %w", err)
	}
	return result, nil
}

func createInvoice(ctx context.Context, tx pgx.Tx, partnerID, id, address string, amount money.USDT) (invoice.Invoice, error) {
	const createQuery = `
		INSERT INTO invoices AS i (id, partner_id, amount, deposit_address, fee_bps)
		SELECT $1, p.id, $2, $3, p.fee_bps
		FROM partners p
		WHERE p.id = $4
		RETURNING ` + invoiceColumns
	created, err := scanInvoice(tx.QueryRow(ctx, createQuery, id, int64(amount), address, partnerID))
	if err != nil {
		return invoice.Invoice{}, fmt.Errorf("insert invoice: %w", err)
	}
	for _, kind := range []ledger.AccountKind{ledger.Asset, ledger.Hold, ledger.Refund} {
		if err := insertAccount(ctx, tx, kind, &partnerID, &id); err != nil {
			return invoice.Invoice{}, err
		}
	}
	return created, nil
}

func replayInvoice(ctx context.Context, tx pgx.Tx, partnerID, key string, amount money.USDT) (invoice.Invoice, error) {
	const amountQuery = `
		SELECT amount FROM invoice_idempotency
		WHERE partner_id = $1 AND key = $2`
	var originalAmount money.USDT
	if err := tx.QueryRow(ctx, amountQuery, partnerID, key).Scan(&originalAmount); err != nil {
		return invoice.Invoice{}, fmt.Errorf("read invoice key: %w", err)
	}
	if originalAmount != amount {
		return invoice.Invoice{}, invoice.ErrConflict
	}
	const query = `
		SELECT ` + invoiceColumns + `
		FROM invoices i
		JOIN invoice_idempotency k ON k.invoice_id = i.id
		WHERE k.partner_id = $1 AND k.key = $2`
	found, err := scanInvoice(tx.QueryRow(ctx, query, partnerID, key))
	if err != nil {
		return invoice.Invoice{}, fmt.Errorf("read idempotent invoice: %w", err)
	}
	return found, nil
}

func (s *Store) GetInvoice(ctx context.Context, partnerID, id string) (invoice.Invoice, error) {
	const query = `
		SELECT ` + invoiceColumns + `
		FROM invoices i
		WHERE i.id = $1 AND i.partner_id = $2`
	found, err := scanInvoice(s.pool.QueryRow(ctx, query, id, partnerID))
	if errors.Is(err, pgx.ErrNoRows) {
		return invoice.Invoice{}, invoice.ErrNotFound
	}
	if err != nil {
		return invoice.Invoice{}, fmt.Errorf("read invoice: %w", err)
	}
	return found, nil
}
