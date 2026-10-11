package httpapi

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"crypto-payment-gateway/internal/chain"
)

func (a *api) authorizeMock(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keys := r.Header.Values("X-Mock-Key")
		if len(keys) != 1 || len(keys[0]) > 256 {
			fail(w, http.StatusUnauthorized, "unauthorized", "valid X-Mock-Key required")
			return
		}
		got, want := sha256.Sum256([]byte(keys[0])), sha256.Sum256([]byte(a.MockControlKey))
		if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			fail(w, http.StatusUnauthorized, "unauthorized", "valid X-Mock-Key required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

type transferRequest struct {
	ChainID        *int64 `json:"chain_id"`
	TxHash         string `json:"tx_hash"`
	EventIndex     int    `json:"tx_event_index"`
	LogIndex       int    `json:"log_index"`
	Sender         string `json:"sender"`
	Recipient      string `json:"recipient"`
	TokenContract  string `json:"token_contract"`
	Topic0         string `json:"topic0"`
	RawAmount      string `json:"raw_amount"`
	ReceiptSuccess *bool  `json:"receipt_success"`
}

type inclusionResponse struct {
	ChainID     int64  `json:"chain_id"`
	TxHash      string `json:"tx_hash"`
	EventIndex  int    `json:"tx_event_index"`
	BlockHash   string `json:"block_hash"`
	BlockHeight int64  `json:"block_height"`
	LogIndex    int    `json:"log_index"`
}

func toInclusion(i chain.Inclusion) inclusionResponse {
	return inclusionResponse{i.ChainID, i.TxHash, i.EventIndex, i.BlockHash, i.BlockHeight, i.LogIndex}
}

type blockResponse struct {
	Hash       string    `json:"hash"`
	Height     int64     `json:"height"`
	ParentHash string    `json:"parent_hash"`
	Timestamp  time.Time `json:"timestamp"`
}

func decodeMock(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		mockBodyError(w, err)
		return false
	}
	if err := d.Decode(&struct{}{}); err != io.EOF {
		mockBodyError(w, err)
		return false
	}
	return true
}

func mockBodyError(w http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		fail(w, http.StatusRequestEntityTooLarge, "body_too_large", "request body exceeds 4096 bytes")
		return
	}
	fail(w, http.StatusBadRequest, "invalid_json", "expected one JSON object with valid simulator fields")
}

func (a *api) simulatorError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, chain.ErrInvalid):
		fail(w, http.StatusBadRequest, "invalid_simulation", "invalid simulator input")
	case errors.Is(err, chain.ErrConflict):
		fail(w, http.StatusConflict, "simulation_conflict", "transfer identifier already has different data")
	default:
		a.internalError(w, "simulate_chain", err)
	}
}

func (a *api) addTransfer(w http.ResponseWriter, r *http.Request) {
	var request transferRequest
	if !decodeMock(w, r, &request) {
		return
	}
	e := chain.Event{TransferID: chain.TransferID{ChainID: chain.ID, TxHash: request.TxHash, EventIndex: request.EventIndex},
		LogIndex: request.LogIndex, Sender: request.Sender, Recipient: request.Recipient, TokenContract: request.TokenContract,
		Topic0: request.Topic0, RawAmount: request.RawAmount, ReceiptSuccess: true}
	if request.ChainID != nil {
		e.ChainID = *request.ChainID
	}
	if request.ReceiptSuccess != nil {
		e.ReceiptSuccess = *request.ReceiptSuccess
	}
	if e.TokenContract == "" {
		e.TokenContract = chain.USDTContract
	}
	if e.Topic0 == "" {
		e.Topic0 = chain.TransferTopic
	}
	result, err := a.Simulator.AddTransfer(r.Context(), e)
	if err != nil {
		a.simulatorError(w, err)
		return
	}
	reply(w, http.StatusCreated, toInclusion(result))
}

func (a *api) advanceChain(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Blocks int `json:"blocks"`
	}
	if !decodeMock(w, r, &request) {
		return
	}
	result, err := a.Simulator.Advance(r.Context(), request.Blocks)
	if err != nil {
		a.simulatorError(w, err)
		return
	}
	reply(w, http.StatusOK, blockResponse{result.Hash, result.Height, result.ParentHash, result.Timestamp})
}

func (a *api) reorgChain(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Depth int `json:"depth"`
	}
	if !decodeMock(w, r, &request) {
		return
	}
	result, err := a.Simulator.Reorg(r.Context(), request.Depth)
	if err != nil {
		a.simulatorError(w, err)
		return
	}
	reply(w, http.StatusOK, blockResponse{result.Hash, result.Height, result.ParentHash, result.Timestamp})
}
