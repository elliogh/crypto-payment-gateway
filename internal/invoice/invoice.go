package invoice

import (
	"context"
	"errors"
	"time"

	"crypto-payment-gateway/internal/money"
	"crypto-payment-gateway/internal/payment"
	"crypto-payment-gateway/internal/refund"
)

var (
	ErrNotFound       = errors.New("invoice not found")
	ErrConflict       = errors.New("idempotency key reused with different request")
	ErrIdempotencyKey = errors.New("invalid idempotency key")
)

type Status string

const (
	Open Status = "OPEN"
	Paid Status = "PAID"
)

type Invoice struct {
	ID             string
	Amount         money.USDT
	Currency       string
	DepositAddress string
	FeeBPS         int
	Status         Status
	CreatedAt      time.Time
	Payment        *payment.Observation
	Refund         *refund.Refund
}

// Repository creates the invoice, idempotency record and ledger accounts atomically.
// Creation snapshots the current fee; a replay returns the original invoice.
type Repository interface {
	CreateInvoice(ctx context.Context, partnerID, key string, amount money.USDT) (Invoice, error)
	GetInvoice(ctx context.Context, partnerID, id string) (Invoice, error)
}

const MaxIdempotencyKeyLength = 128

func ValidateIdempotencyKey(key string) error {
	if len(key) == 0 || len(key) > MaxIdempotencyKeyLength {
		return ErrIdempotencyKey
	}
	for _, char := range key {
		if char < 33 || char > 126 {
			return ErrIdempotencyKey
		}
	}
	return nil
}

func ValidateCreate(amount money.USDT, key string) error {
	if amount <= 0 {
		return money.ErrAmount
	}
	return ValidateIdempotencyKey(key)
}
