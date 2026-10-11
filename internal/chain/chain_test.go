package chain_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"crypto-payment-gateway/internal/chain"
)

func event() chain.Event {
	return chain.Event{TransferID: chain.TransferID{ChainID: 1, TxHash: "0x" + strings.Repeat("a", 64)},
		Sender: "0x" + strings.Repeat("b", 40), Recipient: "0x" + strings.Repeat("c", 40),
		TokenContract: chain.USDTContract, Topic0: chain.TransferTopic, RawAmount: "100000000", ReceiptSuccess: true}
}

func TestNormalizeAndDepositValidation(t *testing.T) {
	for _, raw := range []string{"-1", "+1", "1.5", "", strings.Repeat("9", 79), "115792089237316195423570985008687907853269984665640564039457584007913129639936"} {
		e := event()
		e.RawAmount = raw
		if _, err := e.Normalize(); !errors.Is(err, chain.ErrInvalid) {
			t.Fatal("invalid uint256 accepted", raw, err)
		}
	}
	e := event()
	e.RawAmount = "000100000000"
	e.Sender = "0x" + strings.Repeat("B", 40)
	normalized, err := e.Normalize()
	if err != nil || normalized.RawAmount != "100000000" || normalized.Sender != "0x"+strings.Repeat("b", 40) {
		t.Fatal(normalized, err)
	}
	for _, tc := range []struct {
		name               string
		change             func(*chain.Event)
		accepted, overflow bool
	}{
		{"valid", func(*chain.Event) {}, true, false},
		{"other network", func(e *chain.Event) { e.ChainID = 2 }, false, false},
		{"other contract", func(e *chain.Event) { e.TokenContract = e.Sender }, false, false},
		{"other signature", func(e *chain.Event) { e.Topic0 = e.TxHash }, false, false},
		{"failed receipt", func(e *chain.Event) { e.ReceiptSuccess = false }, false, false},
		{"zero", func(e *chain.Event) { e.RawAmount = "0" }, false, false},
		{"int64 maximum", func(e *chain.Event) { e.RawAmount = "9223372036854775807" }, true, false},
		{"int64 overflow", func(e *chain.Event) { e.RawAmount = "9223372036854775808" }, false, true},
		{"uint256 maximum", func(e *chain.Event) {
			e.RawAmount = "115792089237316195423570985008687907853269984665640564039457584007913129639935"
		}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := event()
			tc.change(&e)
			_, accepted, overflow := e.DepositAmount()
			if accepted != tc.accepted || overflow != tc.overflow {
				t.Fatal(accepted, overflow)
			}
		})
	}
	if chain.Confirmations(12, 10, true) != 3 || chain.Confirmations(10, 10, true) != 1 || chain.Confirmations(12, 10, false) != 0 {
		t.Fatal("confirmation formula")
	}
}

type readerFunc func(context.Context) (chain.Batch, error)

func (f readerFunc) ReadBatch(ctx context.Context) (chain.Batch, error) { return f(ctx) }

type depositsFunc func(context.Context, chain.Batch, []chain.Candidate) error

func (f depositsFunc) ApplyScan(ctx context.Context, b chain.Batch, c []chain.Candidate) error {
	return f(ctx, b, c)
}

func TestScannerRetriesStaleSnapshotAndIsolatesOverflow(t *testing.T) {
	valid := event()
	invalid := valid
	invalid.RawAmount = "9223372036854775808"
	reads, applied := 0, 0
	s := chain.Scanner{Source: readerFunc(func(context.Context) (chain.Batch, error) {
		reads++
		return chain.Batch{Version: int64(reads), Blocks: []chain.Block{{Hash: "block", Events: []chain.Event{invalid, valid}}}}, nil
	}),
		Deposits: depositsFunc(func(_ context.Context, b chain.Batch, c []chain.Candidate) error {
			applied++
			if len(c) != 1 || c[0].Amount != 100000000 {
				t.Fatal("invalid candidate filtering", c)
			}
			if b.Version == 1 {
				return chain.ErrStale
			}
			return nil
		})}
	result, err := s.Scan(context.Background())
	if err != nil || reads != 2 || applied != 2 || result.Blocks != 1 || result.Isolated != 1 {
		t.Fatal(result, reads, applied, err)
	}
}
