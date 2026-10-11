package money

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
)

// USDT is a nonnegative amount in microUSDT. Validate casts at domain boundaries.
type USDT int64

const Currency = "USDT"

var ErrAmount = errors.New("invalid USDT amount")

func Parse(s string) (USDT, error) {
	if len(s) == 0 || len(s) > 26 {
		return 0, ErrAmount
	}
	parts := strings.Split(s, ".")
	if len(parts) > 2 || parts[0] == "" {
		return 0, ErrAmount
	}
	for _, part := range parts {
		if part == "" {
			return 0, ErrAmount
		}
		for _, char := range part {
			if char < '0' || char > '9' {
				return 0, ErrAmount
			}
		}
	}
	fraction := ""
	if len(parts) == 2 {
		fraction = parts[1]
	}
	if len(fraction) > 6 {
		return 0, ErrAmount
	}
	units, err := strconv.ParseInt(parts[0]+fraction+strings.Repeat("0", 6-len(fraction)), 10, 64)
	if err != nil {
		return 0, ErrAmount
	}
	return USDT(units), nil
}

func (a USDT) Valid() bool { return a >= 0 }

func (a USDT) String() string {
	return fmt.Sprintf("%d.%06d", a/1_000_000, a%1_000_000)
}

func Add(a, b USDT) (USDT, error) {
	if !a.Valid() || !b.Valid() || int64(a) > math.MaxInt64-int64(b) {
		return 0, ErrAmount
	}
	return a + b, nil
}

func Fee(gross USDT, bps int) (USDT, error) {
	if !gross.Valid() || bps < 0 || bps > 10_000 {
		return 0, ErrAmount
	}
	n := new(big.Int).Mul(big.NewInt(int64(gross)), big.NewInt(int64(bps)))
	n.Quo(n, big.NewInt(10_000))
	return USDT(n.Int64()), nil // fee <= gross <= MaxInt64
}
