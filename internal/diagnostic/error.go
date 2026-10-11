// Package diagnostic retains error causes while exposing only safe log fields.
package diagnostic

import (
	"context"
	"errors"
	"net"

	"github.com/jackc/pgx/v5/pgconn"
)

type operationError struct {
	operation string
	cause     error
}

// Error omits the cause text, which can contain credentials or SQL parameters.
func (e *operationError) Error() string { return e.operation + " failed" }
func (e *operationError) Unwrap() error { return e.cause }

func Wrap(operation string, err error) error {
	if err == nil {
		return nil
	}
	return &operationError{operation: operation, cause: err}
}

// Fields deliberately excludes Error(), PostgreSQL Message/Detail and connection
// settings. Operation names are static labels supplied by application code.
func Fields(operation string, err error) []any {
	var op *operationError
	if errors.As(err, &op) {
		operation = op.operation
	}
	kind := "internal_error"
	var pgErr *pgconn.PgError
	var parseErr *pgconn.ParseConfigError
	var connectErr *pgconn.ConnectError
	var networkErr net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		kind = "timeout"
	case errors.Is(err, context.Canceled):
		kind = "canceled"
	case errors.As(err, &pgErr):
		kind = "database_error"
	case errors.As(err, &parseErr):
		kind = "configuration_error"
	case errors.As(err, &connectErr), errors.As(err, &networkErr):
		kind = "connection_error"
	}
	fields := []any{"operation", operation, "error_type", kind}
	// A connect error may itself wrap a PostgreSQL error (e.g. authentication).
	if errors.As(err, &pgErr) {
		fields = append(fields, "sqlstate", pgErr.Code)
	}
	return fields
}
