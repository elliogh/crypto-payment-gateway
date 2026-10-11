package main

import (
	"log/slog"
	"os"

	"crypto-payment-gateway/internal/app"
	"crypto-payment-gateway/internal/diagnostic"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := app.Run(logger); err != nil {
		logger.Error("service stopped", diagnostic.Fields("run_service", err)...)
		os.Exit(1)
	}
}
