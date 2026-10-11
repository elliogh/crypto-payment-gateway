package app

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"crypto-payment-gateway/internal/chain"
	"crypto-payment-gateway/internal/diagnostic"
)

// The scanner runs even with mock HTTP disabled, so existing deposits can resume.
// Its stop function waits for rollback/cleanup before the application closes its pool.
func startScanner(parent context.Context, scanner chain.Scanner, logger *slog.Logger) func() {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			if ctx.Err() != nil {
				return
			}
			attempt, finish := context.WithTimeout(ctx, 5*time.Second)
			result, err := scanner.Scan(attempt)
			finish()
			if err != nil && ctx.Err() == nil && !errors.Is(err, chain.ErrStale) {
				logger.Error("chain scan failed", diagnostic.Fields("scan_chain", err)...)
			}
			if err == nil && result.Isolated > 0 {
				logger.Warn("USDT transfers isolated: amount exceeds int64", "count", result.Isolated)
			}
			if err == nil && result.Blocks == chain.MaxBlocks {
				continue
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return func() { cancel(); <-done }
}
