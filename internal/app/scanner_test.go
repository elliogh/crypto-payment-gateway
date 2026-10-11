package app

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"crypto-payment-gateway/internal/chain"
)

type blockedChainReader struct {
	started  chan struct{}
	finished chan struct{}
}

func (r blockedChainReader) ReadBatch(ctx context.Context) (chain.Batch, error) {
	close(r.started)
	<-ctx.Done()
	close(r.finished)
	return chain.Batch{}, ctx.Err()
}

func TestScannerShutdownCancelsAndWaits(t *testing.T) {
	r := blockedChainReader{started: make(chan struct{}), finished: make(chan struct{})}
	stop := startScanner(context.Background(), chain.Scanner{Source: r}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	select {
	case <-r.started:
	case <-time.After(5 * time.Second):
		t.Fatal("scanner did not start")
	}
	stopped := make(chan struct{})
	go func() { stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("scanner did not stop")
	}
	select {
	case <-r.finished:
	default:
		t.Fatal("stop returned before scanner cleanup")
	}
}
