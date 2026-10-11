// Package chain models the simulated Ethereum chain and validates deposit logs.
package chain

import (
	"context"
	"encoding/hex"
	"errors"
	"math/big"
	"strconv"
	"strings"
	"time"

	"crypto-payment-gateway/internal/money"
)

const (
	ID            int64 = 1
	USDTContract        = "0xdac17f958d2ee523a2206206994597c13d831ec7"
	TransferTopic       = "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"
	MaxBlocks           = 100
)

var (
	ErrInvalid  = errors.New("invalid simulator input")
	ErrConflict = errors.New("conflicting transfer or unsupported chain change")
	ErrStale    = errors.New("chain snapshot changed")
)

type TransferID struct {
	ChainID    int64
	TxHash     string
	EventIndex int
}

type Event struct {
	TransferID
	LogIndex       int
	Sender         string
	Recipient      string
	TokenContract  string
	Topic0         string
	RawAmount      string
	ReceiptSuccess bool
}

type Block struct {
	Hash       string
	Height     int64
	ParentHash string
	Timestamp  time.Time
	Events     []Event
}

type Inclusion struct {
	TransferID
	BlockHash   string
	BlockHeight int64
	LogIndex    int
}

// Batch is a consistent source snapshot. Cursor and Version fence its application.
type Batch struct {
	Version int64
	Cursor  string
	Blocks  []Block
}

type Candidate struct {
	Event     Event
	BlockHash string
	Amount    money.USDT
}

// Reader returns canonical blocks after the durable scanner cursor.
type Reader interface {
	ReadBatch(context.Context) (Batch, error)
}

// Deposits records candidates and advances the cursor in one transaction,
// rejecting a batch whose source version or starting cursor has changed.
type Deposits interface {
	ApplyScan(context.Context, Batch, []Candidate) error
}

type Simulator interface {
	AddTransfer(context.Context, Event) (Inclusion, error)
	Advance(context.Context, int) (Block, error)
	Reorg(context.Context, int) (Block, error)
}

func ValidHex(value string, bytes int) bool {
	if len(value) != 2+bytes*2 || !strings.HasPrefix(value, "0x") {
		return false
	}
	_, err := hex.DecodeString(value[2:])
	return err == nil
}

func (id TransferID) Validate() error {
	if id.ChainID != ID || !ValidHex(id.TxHash, 32) || id.EventIndex < 0 || int64(id.EventIndex) > 2147483647 {
		return ErrInvalid
	}
	return nil
}

// Normalize validates raw uint256 input without imposing the narrower USDT limit.
func (e Event) Normalize() (Event, error) {
	if e.TransferID.Validate() != nil || e.LogIndex < 0 || int64(e.LogIndex) > 2147483647 ||
		!ValidHex(e.Sender, 20) || !ValidHex(e.Recipient, 20) || !ValidHex(e.TokenContract, 20) || !ValidHex(e.Topic0, 32) ||
		len(e.RawAmount) == 0 || len(e.RawAmount) > 78 {
		return Event{}, ErrInvalid
	}
	for _, digit := range e.RawAmount {
		if digit < '0' || digit > '9' {
			return Event{}, ErrInvalid
		}
	}
	amount, ok := new(big.Int).SetString(e.RawAmount, 10)
	if !ok || amount.BitLen() > 256 {
		return Event{}, ErrInvalid
	}
	e.TxHash = strings.ToLower(e.TxHash)
	e.Sender = strings.ToLower(e.Sender)
	e.Recipient = strings.ToLower(e.Recipient)
	e.TokenContract = strings.ToLower(e.TokenContract)
	e.Topic0 = strings.ToLower(e.Topic0)
	e.RawAmount = amount.String()
	return e, nil
}

// DepositAmount validates the token, signature and receipt before narrowing to int64.
// overflow identifies an isolated raw amount, not a fatal scanner error.
func (e Event) DepositAmount() (amount money.USDT, accepted, overflow bool) {
	if !e.IsUSDTTransfer() {
		return 0, false, false
	}
	n, err := strconv.ParseInt(e.RawAmount, 10, 64)
	if err != nil {
		return 0, false, true
	}
	return money.USDT(n), n > 0, false
}

func (e Event) IsUSDTTransfer() bool {
	return e.ChainID == ID && e.TokenContract == USDTContract && e.Topic0 == TransferTopic && e.ReceiptSuccess
}

func Confirmations(head, height int64, canonical bool) int64 {
	if !canonical || height > head {
		return 0
	}
	return head - height + 1
}
