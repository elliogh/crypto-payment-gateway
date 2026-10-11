package invoice

import (
	"errors"
	"strings"
	"testing"

	"crypto-payment-gateway/internal/money"
)

func TestValidateCreate(t *testing.T) {
	tests := []struct {
		name   string
		amount money.USDT
		key    string
		want   error
	}{
		{name: "minimum amount", amount: 1, key: "valid"},
		{name: "maximum key length", amount: 1, key: strings.Repeat("a", MaxIdempotencyKeyLength)},
		{name: "zero amount", amount: 0, key: "valid", want: money.ErrAmount},
		{name: "negative amount", amount: -1, key: "valid", want: money.ErrAmount},
		{name: "empty key", amount: 1, want: ErrIdempotencyKey},
		{name: "long key", amount: 1, key: strings.Repeat("a", MaxIdempotencyKeyLength+1), want: ErrIdempotencyKey},
		{name: "space", amount: 1, key: "two words", want: ErrIdempotencyKey},
		{name: "control character", amount: 1, key: "key\n", want: ErrIdempotencyKey},
		{name: "Unicode", amount: 1, key: "ключ", want: ErrIdempotencyKey},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateCreate(tc.amount, tc.key); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}
