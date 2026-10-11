package ledger

import (
	"context"
	"errors"

	"crypto-payment-gateway/internal/money"
)

type AccountKind string

const (
	Asset            AccountKind = "ASSET"
	Hold             AccountKind = "HOLD"
	PartnerAvailable AccountKind = "PARTNER_AVAILABLE"
	Income           AccountKind = "INCOME"
	Refund           AccountKind = "REFUND"
)

var ErrBalance = errors.New("ledger account balance outside USDT int64 range")

// NextBalance applies the whole operation to an account, independent of entry
// order. Assets have a debit balance; the other account kinds have credit balances.
func NextBalance(kind AccountKind, current, debit, credit money.USDT) (money.USDT, error) {
	if !current.Valid() || !debit.Valid() || !credit.Valid() {
		return 0, ErrBalance
	}
	var delta int64
	switch kind {
	case Asset:
		delta = int64(debit) - int64(credit)
	case Hold, PartnerAvailable, Income, Refund:
		delta = int64(credit) - int64(debit)
	default:
		return 0, ErrEntry
	}
	if delta < 0 {
		if int64(current) < -delta {
			return 0, ErrBalance
		}
		return current - money.USDT(-delta), nil
	}
	next, err := money.Add(current, money.USDT(delta))
	if err != nil {
		return 0, ErrBalance
	}
	return next, nil
}

type Balance struct {
	Currency  string
	Available money.USDT
}

type BalanceReader interface {
	Balance(ctx context.Context, partnerID string) (Balance, error)
}
