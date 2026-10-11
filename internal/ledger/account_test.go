package ledger

import (
	"errors"
	"math"
	"testing"

	"crypto-payment-gateway/internal/money"
)

func TestNextBalance(t *testing.T) {
	for _, kind := range []AccountKind{Asset, Hold, PartnerAvailable, Income, Refund} {
		t.Run(string(kind), func(t *testing.T) {
			increase, decrease := money.USDT(30), money.USDT(10)
			if kind != Asset {
				increase, decrease = decrease, increase
			}
			if got, err := NextBalance(kind, 100, increase, decrease); err != nil || got != 120 {
				t.Fatal("incorrect natural balance", got, err)
			}
			if got, err := NextBalance(kind, 100, decrease, increase); err != nil || got != 80 {
				t.Fatal("incorrect debit/credit direction", got, err)
			}
			if _, err := NextBalance(kind, 0, decrease, increase); !errors.Is(err, ErrBalance) {
				t.Fatal("accepted negative balance", err)
			}
			if _, err := NextBalance(kind, math.MaxInt64, increase, decrease); !errors.Is(err, ErrBalance) {
				t.Fatal("accepted overflowing balance", err)
			}
			// Check the net change, not an artificial intermediate balance.
			if got, err := NextBalance(kind, math.MaxInt64, math.MaxInt64, math.MaxInt64); err != nil || got != math.MaxInt64 {
				t.Fatal("rejected a zero net change at the limit", got, err)
			}
		})
	}
	if _, err := NextBalance(AccountKind("UNKNOWN"), 0, 1, 1); !errors.Is(err, ErrEntry) {
		t.Fatal("accepted unknown account kind", err)
	}
	for _, values := range [][3]money.USDT{{-1, 0, 0}, {0, -1, 0}, {0, 0, -1}} {
		if _, err := NextBalance(Asset, values[0], values[1], values[2]); !errors.Is(err, ErrBalance) {
			t.Fatal("accepted invalid money", err)
		}
	}
}
