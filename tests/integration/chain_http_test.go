package integration

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"crypto-payment-gateway/internal/httpapi"
)

func TestHTTPStageTwoScenarioAndAccessControl(t *testing.T) {
	s := chainFixture(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	deps := httpapi.Dependencies{Partners: s.Store, Invoices: s.Store, Balances: s.Store, Ready: s.Ready,
		Simulator: s.Store, MockControlKey: "local-control-key", Confirmations: 3}
	handler := httpapi.New(deps, logger)
	request := func(method, path, key, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("X-API-Key", key)
		r.Header.Set("X-Mock-Key", key)
		r.Header.Set("Idempotency-Key", "http-chain")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	created := request("POST", "/invoices", alphaKey, `{"amount":"100"}`)
	if created.Code != 201 {
		t.Fatal(created.Code, created.Body.String())
	}
	var v struct {
		ID             string `json:"id"`
		DepositAddress string `json:"deposit_address"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	body := `{"tx_hash":"0x` + strings.Repeat("1", 64) + `","sender":"0x` + strings.Repeat("a", 40) + `","recipient":"` + v.DepositAddress + `","raw_amount":"100000000"}`
	for _, key := range []string{"", alphaKey, betaKey, "wrong-control-key"} {
		if w := request("POST", "/mock/transfers", key, body); w.Code != 401 {
			t.Fatal("mock control accepted partner key", w.Code, w.Body.String())
		}
	}
	first := request("POST", "/mock/transfers", "local-control-key", body)
	if first.Code != 201 {
		t.Fatal(first.Code, first.Body.String())
	}
	if w := request("POST", "/mock/transfers", "local-control-key", body); w.Code != 201 || w.Body.String() != first.Body.String() {
		t.Fatal("HTTP transfer replay", w.Code, w.Body.String())
	}
	scanChain(t, s.Store)
	check := func(status string, confirmations int64, ready bool) {
		t.Helper()
		w := request("GET", "/invoices/"+v.ID, alphaKey, "")
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		var response struct {
			Status        string `json:"status"`
			PaymentStatus string `json:"payment_status"`
			Payment       struct {
				Confirmations int64 `json:"confirmations"`
				Ready         bool  `json:"ready_for_aml"`
			} `json:"payment"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if response.Status != "OPEN" || response.PaymentStatus != status || response.Payment.Confirmations != confirmations || response.Payment.Ready != ready {
			t.Fatal(w.Body.String())
		}
	}
	check("CONFIRMING", 1, false)
	if w := request("POST", "/mock/advance", "local-control-key", `{"blocks":1}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	check("CONFIRMING", 2, false)
	if w := request("POST", "/mock/advance", "local-control-key", `{"blocks":1}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	check("CONFIRMING", 3, true)
	if w := request("GET", "/invoices/"+v.ID, betaKey, ""); w.Code != 404 {
		t.Fatal("foreign payment exposed", w.Code, w.Body.String())
	}
	if w := request("POST", "/mock/reorg", "local-control-key", `{"depth":3}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	check("ORPHANED", 0, false)
	scanChain(t, s.Store)
	check("ORPHANED", 0, false)
	if w := request("POST", "/mock/transfers", "local-control-key", body); w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	scanChain(t, s.Store)
	check("CONFIRMING", 1, false)
	for _, tc := range []struct {
		path, body string
		status     int
	}{
		{"/mock/advance", `{"blocks":0}`, 400},
		{"/mock/advance", `{"blocks":101}`, 400},
		{"/mock/advance", `{"blocks":1,"unknown":true}`, 400},
		{"/mock/advance", `{"blocks":1} {}`, 400},
		{"/mock/reorg", `{"depth":100}`, 400},
		{"/mock/transfers", `{"raw_amount":100}`, 400},
		{"/mock/transfers", strings.Repeat(" ", 4097), 413},
	} {
		if w := request("POST", tc.path, "local-control-key", tc.body); w.Code != tc.status {
			t.Fatal(tc.path, w.Code, w.Body.String())
		}
	}
	// Route registration follows LOCAL_MOCKS wiring; disabled controls return 404.
	deps.Simulator = nil
	handler = httpapi.New(deps, logger)
	if w := request("POST", "/mock/transfers", "local-control-key", body); w.Code != 404 {
		t.Fatal("disabled mock route visible", w.Code)
	}
	noFinancialChanges(t, s)
}
