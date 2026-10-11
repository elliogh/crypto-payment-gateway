package httpapi

import (
	"errors"
	"net/http"

	"crypto-payment-gateway/internal/refund"
	"github.com/google/uuid"
)

func (a *api) setAMLAvailability(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Unavailable *bool `json:"unavailable"`
	}
	if !decodeMock(w, r, &request) {
		return
	}
	if request.Unavailable == nil {
		fail(w, http.StatusBadRequest, "invalid_simulation", "unavailable must be boolean")
		return
	}
	a.AMLControl.SetUnavailable(*request.Unavailable)
	reply(w, http.StatusOK, map[string]bool{"unavailable": *request.Unavailable})
}

func (a *api) approveRefund(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if len(id) != 36 || uuid.Validate(id) != nil {
		fail(w, http.StatusNotFound, "not_found", "refund not found")
		return
	}
	var request struct {
		Recipient string `json:"recipient"`
	}
	if !decodeMock(w, r, &request) {
		return
	}
	result, err := a.Refunds.ApproveRefund(r.Context(), id, request.Recipient)
	switch {
	case errors.Is(err, refund.ErrRecipient):
		fail(w, http.StatusBadRequest, "invalid_recipient", "valid confirmed refund route required")
	case errors.Is(err, refund.ErrNotFound):
		fail(w, http.StatusNotFound, "not_found", "refund not found")
	case errors.Is(err, refund.ErrConflict):
		fail(w, http.StatusConflict, "refund_conflict", "refund already has a different approved route")
	case err != nil:
		a.internalError(w, "approve_refund", err)
	default:
		reply(w, http.StatusOK, toRefundResponse(result))
	}
}
