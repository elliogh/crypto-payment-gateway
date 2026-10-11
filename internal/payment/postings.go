package payment

import (
	"fmt"

	"crypto-payment-gateway/internal/ledger"
	"crypto-payment-gateway/internal/money"
)

type Accounts struct{ Asset, Hold, Available, Income, Refund string }

func (a Accounts) HoldEntries(amount money.USDT) []ledger.Entry {
	return []ledger.Entry{{AccountID: a.Asset, Side: ledger.Debit, Amount: amount}, {AccountID: a.Hold, Side: ledger.Credit, Amount: amount}}
}
func (a Accounts) CancelHoldEntries(amount money.USDT) []ledger.Entry {
	return []ledger.Entry{{AccountID: a.Hold, Side: ledger.Debit, Amount: amount}, {AccountID: a.Asset, Side: ledger.Credit, Amount: amount}}
}
func (a Accounts) CreditEntries(gross money.USDT, bps int) ([]ledger.Entry, error) {
	fee, err := money.Fee(gross, bps)
	if err != nil {
		return nil, err
	}
	entries := []ledger.Entry{{AccountID: a.Hold, Side: ledger.Debit, Amount: gross}}
	if net := gross - fee; net > 0 {
		entries = append(entries, ledger.Entry{AccountID: a.Available, Side: ledger.Credit, Amount: net})
	}
	if fee > 0 {
		entries = append(entries, ledger.Entry{AccountID: a.Income, Side: ledger.Credit, Amount: fee})
	}
	return entries, nil
}
func (a Accounts) RefundEntries(amount money.USDT) []ledger.Entry {
	return []ledger.Entry{{AccountID: a.Hold, Side: ledger.Debit, Amount: amount}, {AccountID: a.Refund, Side: ledger.Credit, Amount: amount}}
}

func ObservationKey(id string, version int64, operation string) string {
	return fmt.Sprintf("payment:%s:%d:%s", id, version, operation)
}
func CreditKey(id string) string { return "payment:" + id + ":credit" }
