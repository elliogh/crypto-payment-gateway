package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"crypto-payment-gateway/internal/aml"
	"crypto-payment-gateway/internal/httpapi"
	"crypto-payment-gateway/internal/refund"
)

func TestHTTPCompletePaymentFlows(t *testing.T) {
	s := chainFixture(t)
	ctx := context.Background()
	bad := "0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	provider := aml.NewMock([]string{bad}, false)
	w := processingWorker(s.Store, provider)
	w.RefundPolicy = refund.Policy{Enabled: true, UnsafeSenders: []string{bad}}
	deps := httpapi.Dependencies{Partners: s.Store, Invoices: s.Store, Balances: s.Store, Ready: s.Ready, Simulator: s.Store, MockControlKey: "local-control-key", Confirmations: 3, AMLControl: provider, Refunds: s.Store}
	handler := httpapi.New(deps, slog.New(slog.NewTextHandler(io.Discard, nil)))
	request := func(method, path, key, body string, n int) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("X-API-Key", key)
		r.Header.Set("X-Mock-Key", key)
		r.Header.Set("Idempotency-Key", fmt.Sprintf("http-process-%d", n))
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, r)
		return out
	}
	for _, key := range []string{"", alphaKey, betaKey} {
		if r := request("POST", "/mock/aml", key, `{"unavailable":true}`, 1); r.Code != 401 {
			t.Fatal(r.Code, r.Body.String())
		}
	}
	if r := request("POST", "/mock/aml", "local-control-key", `{}`, 1); r.Code != 400 {
		t.Fatal(r.Code, r.Body.String())
	}
	for n, sender := range []string{"0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", bad} {
		r := request("POST", "/invoices", alphaKey, `{"amount":"100"}`, n)
		if r.Code != 201 {
			t.Fatal(r.Code, r.Body.String())
		}
		var v struct {
			ID      string `json:"id"`
			Address string `json:"deposit_address"`
		}
		if err := json.Unmarshal(r.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		original := r.Body.String()
		body := fmt.Sprintf(`{"tx_hash":"0x%064x","sender":"%s","recipient":"%s","raw_amount":"100000000"}`, n+1, sender, v.Address)
		if r = request("POST", "/mock/transfers", "local-control-key", body, n); r.Code != 201 {
			t.Fatal(r.Code, r.Body.String())
		}
		first := r.Body.String()
		if r = request("POST", "/mock/transfers", "local-control-key", body, n); r.Code != 201 || r.Body.String() != first {
			t.Fatal("transfer replay", r.Code, r.Body.String())
		}
		scanChain(t, s.Store)
		if _, err := w.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if r = request("POST", "/mock/advance", "local-control-key", `{"blocks":2}`, n); r.Code != 200 {
			t.Fatal(r.Code, r.Body.String())
		}
		if n == 0 {
			if r = request("POST", "/mock/aml", "local-control-key", `{"unavailable":true}`, n); r.Code != 200 {
				t.Fatal(r.Code, r.Body.String())
			}
			if _, err := w.RunOnce(ctx); err == nil {
				t.Fatal("expected AML outage")
			}
			if r = request("GET", "/balance", alphaKey, "", n); r.Code != 200 || !strings.Contains(r.Body.String(), `"available":"0.000000"`) {
				t.Fatal(r.Code, r.Body.String())
			}
			if r = request("POST", "/mock/aml", "local-control-key", `{"unavailable":false}`, n); r.Code != 200 {
				t.Fatal(r.Code, r.Body.String())
			}
			if _, err := s.pool.Exec(ctx, `UPDATE aml_jobs SET next_attempt_at=clock_timestamp()`); err != nil {
				t.Fatal(err)
			}
		}
		if worked, err := w.RunOnce(ctx); err != nil || !worked {
			t.Fatal(worked, err)
		}
		r = request("GET", "/invoices/"+v.ID, alphaKey, "", n)
		if r.Code != 200 {
			t.Fatal(r.Code, r.Body.String())
		}
		var result struct {
			Status        string  `json:"status"`
			PaymentStatus string  `json:"payment_status"`
			RefundStatus  *string `json:"refund_status"`
			Refund        *struct {
				ID        string `json:"id"`
				Amount    string `json:"amount"`
				Recipient string `json:"recipient"`
				Status    string `json:"status"`
			} `json:"refund"`
		}
		if err := json.Unmarshal(r.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			if result.Status != "PAID" || result.PaymentStatus != "CREDITED" || result.Refund != nil {
				t.Fatal(r.Body.String())
			}
		} else {
			if result.PaymentStatus != "REJECTED" || result.Refund == nil || result.RefundStatus == nil || *result.RefundStatus != "HELD" || result.Refund.Amount != "100.000000" {
				t.Fatal(r.Body.String())
			}
			path := "/mock/refunds/" + result.Refund.ID + "/approve"
			if r = request("POST", path, alphaKey, `{"recipient":"0xcccccccccccccccccccccccccccccccccccccccc"}`, n); r.Code != 401 {
				t.Fatal(r.Code)
			}
			if r = request("POST", path, "local-control-key", `{"recipient":"0x0000000000000000000000000000000000000000"}`, n); r.Code != 400 {
				t.Fatal(r.Code, r.Body.String())
			}
			before := rowCount(t, s, "ledger_operations")
			if r = request("POST", path, "local-control-key", `{"recipient":"0xcccccccccccccccccccccccccccccccccccccccc"}`, n); r.Code != 200 || !strings.Contains(r.Body.String(), `"status":"READY"`) {
				t.Fatal(r.Code, r.Body.String())
			}
			if rowCount(t, s, "ledger_operations") != before {
				t.Fatal("approval changed obligations")
			}
		}
		if r = request("GET", "/invoices/"+v.ID, betaKey, "", n); r.Code != 404 {
			t.Fatal("foreign payment visible", r.Code)
		}
		if r = request("POST", "/invoices", alphaKey, `{"amount":"100"}`, n); r.Code != 201 {
			t.Fatal(r.Code)
		} else {
			// Creation replay preserves the original shape; current status is PAID for
			// credit and OPEN for rejection, and GET carries payment/refund details.
			if n == 1 && r.Body.String() != original {
				t.Fatal("rejected invoice creation replay changed")
			}
		}
		if r = request("POST", "/mock/reorg", "local-control-key", `{"depth":3}`, n); r.Code != 409 {
			t.Fatal("final decision rolled back", r.Code, r.Body.String())
		}
	}
	r := request("GET", "/balance", alphaKey, "", 0)
	if r.Code != 200 || !strings.Contains(r.Body.String(), `"available":"99.000000"`) {
		t.Fatal(r.Code, r.Body.String())
	}
	deps.Simulator = nil
	handler = httpapi.New(deps, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if r = request("POST", "/mock/aml", "local-control-key", `{"unavailable":true}`, 0); r.Code != 404 {
		t.Fatal("disabled controls visible", r.Code)
	}
}
