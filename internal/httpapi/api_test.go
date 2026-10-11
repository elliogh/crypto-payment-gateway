package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"crypto-payment-gateway/internal/httpapi"
	"crypto-payment-gateway/internal/invoice"
	"crypto-payment-gateway/internal/ledger"
	"crypto-payment-gateway/internal/money"
	"crypto-payment-gateway/internal/partner"
	"github.com/jackc/pgx/v5/pgconn"
)

const testInvoiceID = "00000000-0000-4000-8000-000000000001"

type authenticatorFunc func(context.Context, string) (partner.Partner, error)

func (f authenticatorFunc) Authenticate(ctx context.Context, key string) (partner.Partner, error) {
	return f(ctx, key)
}

type balanceFunc func(context.Context, string) (ledger.Balance, error)

func (f balanceFunc) Balance(ctx context.Context, partnerID string) (ledger.Balance, error) {
	return f(ctx, partnerID)
}

type invoiceRepository struct {
	create func(context.Context, string, string, money.USDT) (invoice.Invoice, error)
	get    func(context.Context, string, string) (invoice.Invoice, error)
}

func (r invoiceRepository) CreateInvoice(ctx context.Context, partnerID, key string, amount money.USDT) (invoice.Invoice, error) {
	return r.create(ctx, partnerID, key, amount)
}

func (r invoiceRepository) GetInvoice(ctx context.Context, partnerID, id string) (invoice.Invoice, error) {
	return r.get(ctx, partnerID, id)
}

func dependencies() httpapi.Dependencies {
	return httpapi.Dependencies{
		Partners: authenticatorFunc(func(context.Context, string) (partner.Partner, error) {
			return partner.Partner{ID: "alpha"}, nil
		}),
		Invoices: invoiceRepository{},
		Ready:    func(context.Context) error { return nil },
	}
}

func request(handler http.Handler, method, target, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.Header.Set("X-API-Key", "test-key")
	r.Header.Set("Idempotency-Key", "test-invoice")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func TestInvoiceResponseContract(t *testing.T) {
	deps := dependencies()
	deps.Invoices = invoiceRepository{
		create: func(_ context.Context, partnerID, key string, amount money.USDT) (invoice.Invoice, error) {
			if partnerID != "alpha" || key != "test-invoice" || amount != 100_000_000 {
				t.Fatalf("incorrect create arguments: %s %s %d", partnerID, key, amount)
			}
			return invoice.Invoice{
				ID:             testInvoiceID,
				Amount:         amount,
				Currency:       money.Currency,
				DepositAddress: "0x0123456789012345678901234567890123456789",
				FeeBPS:         100,
				Status:         invoice.Open,
				CreatedAt:      time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC),
			}, nil
		},
	}
	handler := httpapi.New(deps, slog.New(slog.NewTextHandler(io.Discard, nil)))
	w := request(handler, http.MethodPost, "/invoices", `{"amount":"100"}`)
	const expected = `{"id":"00000000-0000-4000-8000-000000000001","amount":"100.000000","currency":"USDT","deposit_address":"0x0123456789012345678901234567890123456789","fee_bps":100,"status":"OPEN","payment_status":null,"refund_status":null,"created_at":"2026-10-11T12:00:00Z"}` + "\n"
	if w.Code != http.StatusCreated || w.Body.String() != expected {
		t.Fatalf("response contract changed: %d %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Content-Type") != "application/json" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("response headers changed", w.Header())
	}
}

func TestValidationBeforeRepository(t *testing.T) {
	tests := []struct {
		name   string
		method string
		target string
		body   string
		header string
		values []string
		status int
		code   string
	}{
		{
			name:   "duplicate API key",
			method: http.MethodPost,
			target: "/invoices",
			body:   `{"amount":"1"}`,
			header: "X-API-Key",
			values: []string{"one", "two"},
			status: http.StatusUnauthorized,
			code:   "unauthorized",
		},
		{
			name:   "duplicate idempotency key",
			method: http.MethodPost,
			target: "/invoices",
			body:   `{"amount":"1"}`,
			header: "Idempotency-Key",
			values: []string{"one", "two"},
			status: http.StatusBadRequest,
			code:   "invalid_idempotency_key",
		},
		{
			name:   "space inside key",
			method: http.MethodPost,
			target: "/invoices",
			body:   `{"amount":"1"}`,
			header: "Idempotency-Key",
			values: []string{"two words"},
			status: http.StatusBadRequest,
			code:   "invalid_idempotency_key",
		},
		{
			name:   "Unicode key",
			method: http.MethodPost,
			target: "/invoices",
			body:   `{"amount":"1"}`,
			header: "Idempotency-Key",
			values: []string{"ключ"},
			status: http.StatusBadRequest,
			code:   "invalid_idempotency_key",
		},
		{name: "malformed ID", method: http.MethodGet, target: "/invoices/invalid", status: http.StatusNotFound, code: "not_found"},
		{name: "UUID without hyphens", method: http.MethodGet, target: "/invoices/" + strings.ReplaceAll(testInvoiceID, "-", ""), status: http.StatusNotFound, code: "not_found"},
		{name: "UUID URN", method: http.MethodGet, target: "/invoices/urn:uuid:" + testInvoiceID, status: http.StatusNotFound, code: "not_found"},
		{name: "bracketed UUID", method: http.MethodGet, target: "/invoices/{" + testInvoiceID + "}", status: http.StatusNotFound, code: "not_found"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := httpapi.New(dependencies(), slog.New(slog.NewTextHandler(io.Discard, nil)))
			r := httptest.NewRequest(tc.method, tc.target, strings.NewReader(tc.body))
			r.Header.Set("X-API-Key", "test-key")
			r.Header.Set("Idempotency-Key", "test-invoice")
			if tc.header != "" {
				r.Header.Del(tc.header)
				for _, value := range tc.values {
					r.Header.Add(tc.header, value)
				}
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != tc.status || !strings.Contains(w.Body.String(), `"code":"`+tc.code+`"`) {
				t.Fatalf("got %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestGetInvoiceAcceptsUppercaseUUID(t *testing.T) {
	const id = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	deps := dependencies()
	deps.Invoices = invoiceRepository{get: func(_ context.Context, partnerID, receivedID string) (invoice.Invoice, error) {
		if partnerID != "alpha" || receivedID != strings.ToUpper(id) {
			t.Fatal("UUID validation changed the lookup arguments", partnerID, receivedID)
		}
		return invoice.Invoice{ID: id}, nil
	}}
	handler := httpapi.New(deps, slog.New(slog.NewTextHandler(io.Discard, nil)))
	w := request(handler, http.MethodGet, "/invoices/"+strings.ToUpper(id), "")
	if w.Code != http.StatusOK {
		t.Fatal("uppercase UUID rejected", w.Code, w.Body.String())
	}
}

func TestWrappedDomainErrors(t *testing.T) {
	tests := []struct {
		name   string
		method string
		target string
		deps   httpapi.Dependencies
		status int
	}{
		{
			name: "authentication", method: http.MethodGet, target: "/balance", status: http.StatusUnauthorized,
			deps: httpapi.Dependencies{Partners: authenticatorFunc(func(context.Context, string) (partner.Partner, error) {
				return partner.Partner{}, fmt.Errorf("wrapped: %w", partner.ErrUnauthorized)
			})},
		},
		{
			name: "idempotency conflict", method: http.MethodPost, target: "/invoices", status: http.StatusConflict,
			deps: httpapi.Dependencies{Invoices: invoiceRepository{create: func(context.Context, string, string, money.USDT) (invoice.Invoice, error) {
				return invoice.Invoice{}, fmt.Errorf("wrapped: %w", invoice.ErrConflict)
			}}},
		},
		{
			name: "missing invoice", method: http.MethodGet, target: "/invoices/" + testInvoiceID, status: http.StatusNotFound,
			deps: httpapi.Dependencies{Invoices: invoiceRepository{get: func(context.Context, string, string) (invoice.Invoice, error) {
				return invoice.Invoice{}, fmt.Errorf("wrapped: %w", invoice.ErrNotFound)
			}}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			deps := dependencies()
			if tc.deps.Partners != nil {
				deps.Partners = tc.deps.Partners
			}
			if tc.deps.Invoices != nil {
				deps.Invoices = tc.deps.Invoices
			}
			handler := httpapi.New(deps, slog.New(slog.NewTextHandler(io.Discard, nil)))
			w := request(handler, tc.method, tc.target, `{"amount":"1"}`)
			if w.Code != tc.status {
				t.Fatalf("got %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestReadinessFailure(t *testing.T) {
	deps := dependencies()
	deps.Ready = func(context.Context) error { return errors.New("database unavailable") }
	handler := httpapi.New(deps, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if w := request(handler, http.MethodGet, "/readyz", ""); w.Code != http.StatusServiceUnavailable {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := request(handler, http.MethodGet, "/healthz", ""); w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestInternalErrorDoesNotExposeSecrets(t *testing.T) {
	const secret = "test-secret-must-not-appear"
	deps := dependencies()
	deps.Balances = balanceFunc(func(context.Context, string) (ledger.Balance, error) {
		return ledger.Balance{}, errors.New(secret)
	})
	var logs bytes.Buffer
	handler := httpapi.New(deps, slog.New(slog.NewTextHandler(&logs, nil)))
	w := request(handler, http.MethodGet, "/balance", "")
	if w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String()+logs.String(), secret) {
		t.Fatalf("unsafe error response/log: %d %s %s", w.Code, w.Body.String(), logs.String())
	}
}

func TestDatabaseErrorLogsOperationAndSQLState(t *testing.T) {
	const secret = "postgres://user:secret@db/private-parameters"
	deps := dependencies()
	deps.Balances = balanceFunc(func(context.Context, string) (ledger.Balance, error) {
		return ledger.Balance{}, fmt.Errorf("read balance: %w", &pgconn.PgError{
			Code: "23514", Message: secret, Detail: secret,
		})
	})
	var logs bytes.Buffer
	handler := httpapi.New(deps, slog.New(slog.NewJSONHandler(&logs, nil)))
	w := request(handler, http.MethodGet, "/balance", "")
	if w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String()+logs.String(), secret) {
		t.Fatal("unsafe error response or log")
	}
	var fields map[string]any
	if err := json.Unmarshal(logs.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	if fields["operation"] != "read_balance" || fields["error_type"] != "database_error" || fields["sqlstate"] != "23514" {
		t.Fatal("error cannot be diagnosed", fields)
	}
}
