package refund

import (
	"context"
	"errors"
	"strings"
	"time"

	"crypto-payment-gateway/internal/chain"
	"crypto-payment-gateway/internal/money"
)

type Status string

const (
	Held  Status = "HELD"
	Ready Status = "READY"
)

var (
	ErrRecipient = errors.New("invalid refund recipient")
	ErrNotFound  = errors.New("refund not found")
	ErrConflict  = errors.New("refund route already approved")
)

type Refund struct {
	ID        string
	PaymentID string
	Amount    money.USDT
	Recipient string
	Reason    string
	Status    Status
	CreatedAt time.Time
}

type Approver interface {
	ApproveRefund(context.Context, string, string) (Refund, error)
}

type Policy struct {
	Enabled       bool
	UnsafeSenders []string
}

func ValidRecipient(address string) bool {
	return chain.ValidHex(address, 20) && address != "0x"+strings.Repeat("0", 40)
}

// Local mock assumption: addresses outside this list have a safe return route.
// Real contract/exchange detection and transaction submission are outside scope.
func (p Policy) Decide(sender string) (Status, string) {
	if !ValidRecipient(sender) {
		return Held, "unsafe_return_route"
	}
	for _, address := range p.UnsafeSenders {
		if strings.EqualFold(sender, address) {
			return Held, "unsafe_return_route"
		}
	}
	if !p.Enabled {
		return Held, "authorization_required"
	}
	return Ready, "high_aml_risk"
}
