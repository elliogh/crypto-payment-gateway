package app

import (
	"context"
	"log/slog"
	"time"

	"crypto-payment-gateway/internal/diagnostic"
	"crypto-payment-gateway/internal/payment"
)

func startPaymentWorker(parent context.Context, worker payment.Worker, logger *slog.Logger) func() {
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
			attempt, finish := context.WithTimeout(ctx, 15*time.Second)
			worked, err := worker.RunOnce(attempt)
			finish()
			if err != nil && ctx.Err() == nil {
				logger.Error("payment processing failed", diagnostic.Fields("process_payment", err)...)
			}
			if worked {
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
