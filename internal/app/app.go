package app

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"crypto-payment-gateway/internal/chain"
	"crypto-payment-gateway/internal/config"
	"crypto-payment-gateway/internal/diagnostic"
	"crypto-payment-gateway/internal/httpapi"
	"crypto-payment-gateway/internal/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Run builds the application, initializes storage and serves until termination.
func Run(logger *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return diagnostic.Wrap("load_config", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	startup, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	pool, err := pgxpool.New(startup, cfg.DatabaseURL)
	if err != nil {
		return diagnostic.Wrap("parse_database_config", err)
	}
	defer pool.Close()
	if err := pool.Ping(startup); err != nil {
		return diagnostic.Wrap("connect_database", err)
	}
	if err := postgres.Migrate(startup, pool); err != nil {
		return diagnostic.Wrap("apply_migrations", err)
	}
	store := postgres.New(pool)
	if err := store.Seed(startup, cfg.AlphaKey, cfg.BetaKey); err != nil {
		return diagnostic.Wrap("initialize_partners", err)
	}
	if err := store.InitializeChain(startup); err != nil {
		return diagnostic.Wrap("initialize_chain", err)
	}
	scanner := chain.Scanner{Source: store, Deposits: store}
	cancel()
	stopScanner := startScanner(ctx, scanner, logger)
	defer stopScanner()

	deps := httpapi.Dependencies{
		Partners:      store,
		Invoices:      store,
		Balances:      store,
		Ready:         store.Ready,
		Confirmations: cfg.Confirmations,
	}
	if cfg.LocalMocks {
		deps.Simulator = store
		deps.MockControlKey = cfg.MockControlKey
	}
	handler := httpapi.New(deps, logger)
	return diagnostic.Wrap("serve_http", serve(ctx, cfg.HTTPAddr, handler, logger))
}
