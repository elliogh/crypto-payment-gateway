package diagnostic_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"testing"

	"crypto-payment-gateway/internal/diagnostic"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestSafeDiagnosticsRetainCauses(t *testing.T) {
	const secret = "password-must-not-be-logged"
	pgErr := &pgconn.PgError{Code: "23514", Message: secret, Detail: secret, InternalQuery: secret}
	err := diagnostic.Wrap("initialize_partners", fmt.Errorf("query with %s: %w", secret, pgErr))
	var cause *pgconn.PgError
	if !errors.As(err, &cause) || cause != pgErr || strings.Contains(err.Error(), secret) {
		t.Fatal("cause was lost or exposed")
	}
	fields := loggedFields(t, diagnostic.Fields("startup", err))
	if fields["operation"] != "initialize_partners" || fields["error_type"] != "database_error" || fields["sqlstate"] != "23514" {
		t.Fatal("missing useful diagnostics", fields)
	}
	for _, v := range fields {
		if strings.Contains(fmt.Sprint(v), secret) {
			t.Fatal("secret reached logs")
		}
	}
}

func TestErrorClassification(t *testing.T) {
	_, parseErr := pgx.ParseConfig("postgres://user:secret@bad%zz/db")
	if parseErr == nil {
		t.Fatal("expected a malformed connection URL")
	}
	for _, tc := range []struct {
		name string
		err  error
		kind string
	}{
		{"timeout", context.DeadlineExceeded, "timeout"},
		{"canceled", context.Canceled, "canceled"},
		{"configuration", parseErr, "configuration_error"},
		{"connection", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("secret")}, "connection_error"},
		{"unknown", errors.New("secret"), "internal_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wrapped := diagnostic.Wrap("connect_database", tc.err)
			if !errors.Is(wrapped, tc.err) {
				t.Fatal("underlying cause was lost")
			}
			fields := loggedFields(t, diagnostic.Fields("startup", wrapped))
			if fields["operation"] != "connect_database" || fields["error_type"] != tc.kind {
				t.Fatal(fields)
			}
			if _, ok := fields["sqlstate"]; ok {
				t.Fatal("SQLSTATE invented for a non-PostgreSQL error")
			}
		})
	}
	if diagnostic.Wrap("successful_operation", nil) != nil {
		t.Fatal("successful operation returned an error")
	}
}

func loggedFields(t *testing.T, attrs []any) map[string]any {
	t.Helper()
	var logs bytes.Buffer
	slog.New(slog.NewJSONHandler(&logs, nil)).Error("failed", attrs...)
	var fields map[string]any
	if err := json.Unmarshal(logs.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	return fields
}
