package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"crypto-payment-gateway/internal/diagnostic"
)

type errorResponse struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (a *api) internalError(w http.ResponseWriter, operation string, err error) {
	a.log.Error("request failed", diagnostic.Fields(operation, err)...)
	fail(w, http.StatusInternalServerError, "internal_error", "internal server error")
}

func bodyError(w http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		fail(w, http.StatusRequestEntityTooLarge, "body_too_large", "request body exceeds 4096 bytes")
		return
	}
	fail(w, http.StatusBadRequest, "invalid_json", "expected one JSON object with amount as a string")
}

func fail(w http.ResponseWriter, status int, code, message string) {
	reply(w, status, errorResponse{Error: errorDetail{Code: code, Message: message}})
}

func reply(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
