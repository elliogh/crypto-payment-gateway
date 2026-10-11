package integration

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"crypto-payment-gateway/internal/httpapi"
)

func TestHTTPPartnerAPI(t *testing.T) {
	s := fixture(t)
	handler := httpapi.New(httpapi.Dependencies{
		Partners: s.Store,
		Invoices: s.Store,
		Balances: s.Store,
		Ready:    s.Ready,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	request := func(method, path, key, idem, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if key != "" {
			r.Header.Set("X-API-Key", key)
		}
		if idem != "" {
			r.Header.Set("Idempotency-Key", idem)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	created := request("POST", "/invoices", alphaKey, "api-key", `{"amount":"100","currency":"USDT"}`)
	if created.Code != http.StatusCreated {
		t.Fatal(created.Code, created.Body.String())
	}
	var response struct {
		ID            string  `json:"id"`
		Amount        string  `json:"amount"`
		FeeBPS        int     `json:"fee_bps"`
		PaymentStatus *string `json:"payment_status"`
		RefundStatus  *string `json:"refund_status"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Amount != "100.000000" || response.FeeBPS != 100 || response.PaymentStatus != nil || response.RefundStatus != nil {
		t.Fatal(response)
	}
	replay := request("POST", "/invoices", alphaKey, "api-key", `{"amount":"100.000000"}`)
	if replay.Code != http.StatusCreated || replay.Body.String() != created.Body.String() {
		t.Fatal("noncanonical replay", replay.Code, replay.Body.String())
	}
	if got := request("GET", "/invoices/"+response.ID, alphaKey, "", ""); got.Code != http.StatusOK {
		t.Fatal(got.Code)
	}
	if got := request("GET", "/invoices/"+response.ID, betaKey, "", ""); got.Code != http.StatusNotFound {
		t.Fatal("foreign invoice", got.Code)
	}
	if got := request("GET", "/balance", alphaKey, "", ""); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `"available":"0.000000"`) {
		t.Fatal(got.Code, got.Body.String())
	}
	for _, tc := range []struct {
		key, idem, body string
		want            int
	}{
		{"", "key", `{"amount":"1"}`, http.StatusUnauthorized},
		{"wrong", "key", `{"amount":"1"}`, http.StatusUnauthorized},
		{alphaKey, "", `{"amount":"1"}`, http.StatusBadRequest},
		{alphaKey, "api-key", `{"amount":"101"}`, http.StatusConflict},
		{alphaKey, "new", `{"amount":1}`, http.StatusBadRequest},
		{alphaKey, "new", `{"amount":"0"}`, http.StatusBadRequest},
		{alphaKey, "new", `{"amount":"-1"}`, http.StatusBadRequest},
		{alphaKey, "new", `{"amount":"1.0000001"}`, http.StatusBadRequest},
		{alphaKey, "new", `{"amount":"9223372036854.775808"}`, http.StatusBadRequest},
		{alphaKey, "new", `{"amount":"1","currency":"ETH"}`, http.StatusBadRequest},
		{alphaKey, "new", `{"amount":"1","partner_id":"other"}`, http.StatusBadRequest},
		{alphaKey, "new", `{"amount":"1"} {}`, http.StatusBadRequest},
		{alphaKey, "new", strings.Repeat(" ", 4097), http.StatusRequestEntityTooLarge},
	} {
		got := request("POST", "/invoices", tc.key, tc.idem, tc.body)
		if got.Code != tc.want {
			t.Errorf("%s: got %d want %d: %s", tc.body[:min(80, len(tc.body))], got.Code, tc.want, got.Body.String())
		}
	}
	if got := request("GET", "/invoices/not-a-uuid", alphaKey, "", ""); got.Code != http.StatusNotFound {
		t.Fatal(got.Code)
	}
	if got := request("GET", "/healthz", "", "", ""); got.Code != http.StatusOK {
		t.Fatal(got.Code)
	}
	if got := request("GET", "/readyz", "", "", ""); got.Code != http.StatusOK {
		t.Fatal(got.Code)
	}
	// The partner cannot mutate the ledger via HTTP.
	if got := request(http.MethodPost, "/balance", alphaKey, "", `{"available":"999"}`); got.Code < 400 {
		t.Fatalf("balance is writable: %d", got.Code)
	}
}
