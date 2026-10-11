package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"crypto-payment-gateway/internal/invoice"
	"crypto-payment-gateway/internal/money"
	"github.com/google/uuid"
)

const maxRequestBodyBytes = 4096

type createInvoiceRequest struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

type invoiceResponse struct {
	ID             string    `json:"id"`
	Amount         string    `json:"amount"`
	Currency       string    `json:"currency"`
	DepositAddress string    `json:"deposit_address"`
	FeeBPS         int       `json:"fee_bps"`
	Status         string    `json:"status"`
	PaymentStatus  *string   `json:"payment_status"`
	RefundStatus   *string   `json:"refund_status"`
	CreatedAt      time.Time `json:"created_at"`
}

func toInvoiceResponse(found invoice.Invoice) invoiceResponse {
	return invoiceResponse{
		ID:             found.ID,
		Amount:         found.Amount.String(),
		Currency:       found.Currency,
		DepositAddress: found.DepositAddress,
		FeeBPS:         found.FeeBPS,
		Status:         string(found.Status),
		CreatedAt:      found.CreatedAt,
	}
}

func (a *api) createInvoice(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Idempotency-Key")
	if len(r.Header.Values("Idempotency-Key")) != 1 || len(key) == 0 || len(key) > invoice.MaxIdempotencyKeyLength || strings.TrimSpace(key) != key {
		fail(w, http.StatusBadRequest, "invalid_idempotency_key", "Idempotency-Key must contain 1..128 bytes without surrounding whitespace")
		return
	}
	if err := invoice.ValidateIdempotencyKey(key); err != nil {
		fail(w, http.StatusBadRequest, "invalid_idempotency_key", "Idempotency-Key must contain printable ASCII without spaces")
		return
	}
	var request createInvoiceRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		bodyError(w, err)
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		bodyError(w, err)
		return
	}
	if request.Currency != "" && request.Currency != money.Currency {
		fail(w, http.StatusBadRequest, "invalid_currency", "only USDT is supported")
		return
	}
	amount, err := money.Parse(request.Amount)
	if err != nil || amount == 0 {
		fail(w, http.StatusBadRequest, "invalid_amount", "amount must be a positive USDT string with at most 6 decimals within int64 range")
		return
	}
	created, err := a.Invoices.CreateInvoice(r.Context(), requestPartner(r).ID, key, amount)
	if errors.Is(err, invoice.ErrConflict) {
		fail(w, http.StatusConflict, "idempotency_conflict", "key already used with a different amount")
		return
	}
	if err != nil {
		a.internalError(w, "create_invoice", err)
		return
	}
	// Original creation and idempotent replay have the same response code.
	reply(w, http.StatusCreated, toInvoiceResponse(created))
}

func (a *api) getInvoice(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if len(id) != 36 || uuid.Validate(id) != nil {
		fail(w, http.StatusNotFound, "not_found", "invoice not found")
		return
	}
	found, err := a.Invoices.GetInvoice(r.Context(), requestPartner(r).ID, id)
	if errors.Is(err, invoice.ErrNotFound) {
		fail(w, http.StatusNotFound, "not_found", "invoice not found")
		return
	}
	if err != nil {
		a.internalError(w, "get_invoice", err)
		return
	}
	reply(w, http.StatusOK, toInvoiceResponse(found))
}
