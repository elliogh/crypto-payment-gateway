package app_test

import (
	"errors"
	"io"
	"log/slog"
	"testing"

	"crypto-payment-gateway/internal/app"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestStartupPreservesDatabaseConfigurationCause(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://gateway:secret@bad%zz/gateway")
	t.Setenv("PARTNER_ALPHA_KEY", "local-alpha-key")
	t.Setenv("PARTNER_BETA_KEY", "local-beta-key")
	t.Setenv("LOCAL_MOCKS", "false")
	t.Setenv("CONFIRMATIONS", "3")
	t.Setenv("AML_RETRY_INTERVAL", "5s")
	err := app.Run(slog.New(slog.NewTextHandler(io.Discard, nil)))
	var cause *pgconn.ParseConfigError
	if !errors.As(err, &cause) {
		t.Fatal("startup discarded the database configuration cause")
	}
}
