package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"crypto-payment-gateway/internal/invoice"
	"crypto-payment-gateway/internal/ledger"
	"crypto-payment-gateway/internal/partner"
)

type Dependencies struct {
	Partners partner.Authenticator
	Invoices invoice.Repository
	Balances ledger.BalanceReader
	Ready    func(context.Context) error
}

type api struct {
	Dependencies
	log *slog.Logger
}

func New(deps Dependencies, logger *slog.Logger) http.Handler {
	api := &api{Dependencies: deps, log: logger}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", api.health)
	mux.HandleFunc("GET /readyz", api.ready)
	mux.Handle("POST /invoices", api.authorize(http.HandlerFunc(api.createInvoice)))
	mux.Handle("GET /invoices/{id}", api.authorize(http.HandlerFunc(api.getInvoice)))
	mux.Handle("GET /balance", api.authorize(http.HandlerFunc(api.balance)))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fail(w, http.StatusNotFound, "not_found", "route not found")
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		w.Header().Set("Cache-Control", "no-store")
		mux.ServeHTTP(w, r.WithContext(ctx))
	})
}
