package app

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"crypto-payment-gateway/internal/aml"
	"crypto-payment-gateway/internal/payment"
	"crypto-payment-gateway/internal/refund"
)

type shutdownProcessing struct{}

func (shutdownProcessing) PrepareReady(context.Context, int) error { return nil }
func (shutdownProcessing) ClaimAML(context.Context, time.Duration) (payment.Job, bool, error) {
	return payment.Job{}, true, nil
}
func (shutdownProcessing) CompleteAML(context.Context, payment.Job, aml.Risk, int, refund.Policy) error {
	return nil
}
func (shutdownProcessing) RetryAML(ctx context.Context, _ payment.Job, _ string, _ time.Duration) error {
	return ctx.Err()
}

type blockedAML struct{ started, finished chan struct{} }

func (p blockedAML) Check(ctx context.Context, _ aml.Subject) (aml.Risk, error) {
	close(p.started)
	<-ctx.Done()
	close(p.finished)
	return "", ctx.Err()
}
func TestPaymentWorkerShutdownCancelsAndWaits(t *testing.T) {
	provider := blockedAML{started: make(chan struct{}), finished: make(chan struct{})}
	stop := startPaymentWorker(context.Background(), payment.Worker{Store: shutdownProcessing{}, Provider: provider}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	select {
	case <-provider.started:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not start")
	}
	stopped := make(chan struct{})
	go func() { stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not stop")
	}
	select {
	case <-provider.finished:
	default:
		t.Fatal("stop returned before provider cleanup")
	}
}
