package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

func serve(ctx context.Context, address string, handler http.Handler, logger *slog.Logger) error {
	server := &http.Server{
		Addr:              address,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 * 1024,
	}
	failed := make(chan error, 1)
	go func() { failed <- server.ListenAndServe() }()
	logger.Info("API started", "address", address, "stage", 2)

	select {
	case err := <-failed:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve HTTP: %w", err)
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
			return fmt.Errorf("shutdown HTTP: %w", err)
		}
		return nil
	}
}
