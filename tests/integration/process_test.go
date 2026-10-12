package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"crypto-payment-gateway/internal/invoice"
	"crypto-payment-gateway/internal/ledger"
	"crypto-payment-gateway/internal/money"
	"crypto-payment-gateway/internal/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
)

const processMockKey = "local-process-control-key"

type gatewayProcess struct {
	cmd     *exec.Cmd
	done    chan error
	stopped bool
	baseURL string
	logPath string
	client  *http.Client
}

func buildGateway(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "gateway")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-race", "-o", binary, "./cmd/gateway")
	cmd.Dir = "../.."
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build gateway: %v\n%s", err, output)
	}
	return binary
}

func startGatewayProcess(t *testing.T, binary string, pool *pgxpool.Pool, unavailable bool) *gatewayProcess {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	schema := pool.Config().ConnConfig.RuntimeParams["search_path"]
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if strings.HasPrefix(databaseURL, "postgres://") || strings.HasPrefix(databaseURL, "postgresql://") {
		u, err := url.Parse(databaseURL)
		if err != nil {
			t.Fatal("invalid integration database URL")
		}
		query := u.Query()
		query.Set("search_path", schema)
		query.Set("application_name", schema+"_gateway")
		u.RawQuery = query.Encode()
		databaseURL = u.String()
	} else {
		// The generated schema/application names contain only letters, digits and '_'.
		databaseURL += " search_path='" + schema + "' application_name='" + schema + "_gateway'"
	}
	settings := map[string]string{
		"DATABASE_URL": databaseURL, "HTTP_ADDR": address,
		"PARTNER_ALPHA_KEY": alphaKey, "PARTNER_BETA_KEY": betaKey,
		"LOCAL_MOCKS": "true", "MOCK_CONTROL_KEY": processMockKey,
		"CONFIRMATIONS": "3", "AML_RETRY_INTERVAL": "1s",
		"AML_BLACKLIST":   "0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"AML_UNAVAILABLE": fmt.Sprint(unavailable), "REFUNDS_ENABLED": "true", "REFUND_UNSAFE_SENDERS": "",
	}
	cmd := exec.Command(binary)
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if _, overridden := settings[name]; !overridden {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	for name, value := range settings {
		cmd.Env = append(cmd.Env, name+"="+value)
	}
	log, err := os.Create(filepath.Join(t.TempDir(), "gateway.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatal("start gateway", err)
	}
	p := &gatewayProcess{cmd: cmd, done: make(chan error, 1), baseURL: "http://" + address, logPath: log.Name(), client: &http.Client{Timeout: 3 * time.Second}}
	go func() { p.done <- cmd.Wait() }()
	t.Cleanup(func() { p.kill(t) })
	awaitProcessCondition(t, 20*time.Second, func() bool {
		select {
		case err := <-p.done:
			p.stopped = true
			output, _ := os.ReadFile(log.Name())
			t.Fatalf("gateway exited during startup: %v\n%s", err, output)
		default:
		}
		response, err := p.client.Get(p.baseURL + "/readyz")
		if err != nil {
			return false
		}
		defer response.Body.Close()
		return response.StatusCode == http.StatusOK
	})
	return p
}

func (p *gatewayProcess) kill(t *testing.T) {
	t.Helper()
	if p.stopped {
		return
	}
	p.stopped = true
	if err := p.cmd.Process.Kill(); err != nil && err != os.ErrProcessDone {
		t.Error("kill gateway", err)
	}
	select {
	case <-p.done:
	case <-time.After(10 * time.Second):
		t.Error("gateway did not exit")
	}
	if output, err := os.ReadFile(p.logPath); err == nil && bytes.Contains(output, []byte("WARNING: DATA RACE")) {
		t.Error("race detector reported a race in the child gateway process")
	}
}

func awaitProcessCondition(t *testing.T, timeout time.Duration, check func() bool) {
	t.Helper()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if check() {
			return
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatal("process scenario did not reach expected state")
		}
	}
}

func (p *gatewayProcess) request(t *testing.T, method, path, key, idem string, body any, status int, result any) {
	t.Helper()
	var data []byte
	if body != nil {
		var err error
		data, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	request, err := http.NewRequest(method, p.baseURL+path, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-API-Key", key)
	request.Header.Set("X-Mock-Key", key)
	if idem != "" {
		request.Header.Set("Idempotency-Key", idem)
	}
	response, err := p.client.Do(request)
	if err != nil {
		t.Fatal("process HTTP request", err)
	}
	defer response.Body.Close()
	if response.StatusCode != status {
		output, _ := io.ReadAll(response.Body)
		t.Fatalf("%s %s: status %d, want %d: %s", method, path, response.StatusCode, status, output)
	}
	if result != nil {
		if err := json.NewDecoder(response.Body).Decode(result); err != nil {
			t.Fatal(err)
		}
	}
}

type processInvoice struct {
	ID            string              `json:"id"`
	Address       string              `json:"deposit_address"`
	Status        string              `json:"status"`
	PaymentStatus string              `json:"payment_status"`
	RefundStatus  string              `json:"refund_status"`
	Payment       struct{ ID string } `json:"payment"`
	Refund        struct {
		ID     string `json:"id"`
		Amount string `json:"amount"`
	} `json:"refund"`
}

func (p *gatewayProcess) invoice(t *testing.T, id string) processInvoice {
	t.Helper()
	var result processInvoice
	p.request(t, "GET", "/invoices/"+id, alphaKey, "", nil, 200, &result)
	return result
}

func TestProcessCrashRecoveryBeforeAndAfterFinancialCommit(t *testing.T) {
	pool := databasePool(t) // The child process applies migrations to an empty schema.
	s := &databaseFixture{Store: postgres.New(pool), pool: pool}
	binary := buildGateway(t)
	p := startGatewayProcess(t, binary, pool, true)
	ctx := context.Background()
	var low processInvoice
	p.request(t, "POST", "/invoices", alphaKey, "process-low", map[string]string{"amount": "100"}, 201, &low)
	lowTransfer := map[string]string{"tx_hash": fmt.Sprintf("0x%064x", 1), "sender": "0x" + strings.Repeat("a", 40), "recipient": low.Address, "raw_amount": "100000000"}
	p.request(t, "POST", "/mock/transfers", processMockKey, "", lowTransfer, 201, nil)
	p.request(t, "POST", "/mock/advance", processMockKey, "", map[string]int{"blocks": 2}, 200, nil)
	awaitProcessCondition(t, 10*time.Second, func() bool {
		var waiting bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM aml_jobs WHERE status='PENDING' AND last_error='aml_unavailable')`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		return waiting
	})
	low = p.invoice(t, low.ID)
	if low.PaymentStatus != "AML_PENDING" {
		t.Fatal("AML outage did not retain pending payment", low)
	}
	financialState(t, s, invoice.Invoice{ID: low.ID}, map[ledger.AccountKind]money.USDT{ledger.Asset: 100_000_000, ledger.Hold: 100_000_000})
	// Stop the real completion transaction at an account lock after its AML call.
	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(ctx)
	if _, err := blocker.Exec(ctx, `SELECT id FROM ledger_accounts WHERE kind='PARTNER_AVAILABLE' AND partner_id=$1 FOR UPDATE`, postgres.AlphaID); err != nil {
		t.Fatal(err)
	}
	p.request(t, "POST", "/mock/aml", processMockKey, "", map[string]bool{"unavailable": false}, 200, nil)
	name := pool.Config().ConnConfig.RuntimeParams["search_path"] + "_gateway"
	awaitProcessCondition(t, 10*time.Second, func() bool {
		var waiting bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE application_name=$1 AND wait_event_type='Lock' AND query LIKE '%ledger_accounts%')`, name).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		return waiting
	})
	var epoch int64
	if err := pool.QueryRow(ctx, `SELECT lease_version FROM aml_jobs WHERE payment_id=$1 AND status='RUNNING'`, low.Payment.ID).Scan(&epoch); err != nil {
		t.Fatal(err)
	}
	p.kill(t)
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if rowCount(t, s, "ledger_operations") != 1 || rowCount(t, s, "aml_results") != 0 {
		t.Fatal("crash committed a partial financial decision")
	}
	financialState(t, s, invoice.Invoice{ID: low.ID}, map[ledger.AccountKind]money.USDT{ledger.Asset: 100_000_000, ledger.Hold: 100_000_000})
	p = startGatewayProcess(t, binary, pool, false)
	// Wait for the real 30s lease to expire; do not rewrite job timestamps.
	awaitProcessCondition(t, 45*time.Second, func() bool { return p.invoice(t, low.ID).PaymentStatus == "CREDITED" })
	credited := p.invoice(t, low.ID)
	if credited.Status != "PAID" || credited.Payment.ID != low.Payment.ID {
		t.Fatal("restart lost the original payment", credited)
	}
	var newEpoch int64
	if err := pool.QueryRow(ctx, `SELECT lease_version FROM aml_jobs WHERE payment_id=$1 AND status='DONE'`, low.Payment.ID).Scan(&newEpoch); err != nil || newEpoch != epoch+1 {
		t.Fatal("abandoned claim was not recovered", epoch, newEpoch, err)
	}
	financialState(t, s, invoice.Invoice{ID: low.ID}, map[ledger.AccountKind]money.USDT{ledger.Asset: 100_000_000, ledger.PartnerAvailable: 99_000_000, ledger.Income: 1_000_000})
	var high processInvoice
	p.request(t, "POST", "/invoices", alphaKey, "process-high", map[string]string{"amount": "100"}, 201, &high)
	highTransfer := map[string]string{"tx_hash": fmt.Sprintf("0x%064x", 2), "sender": "0x" + strings.Repeat("b", 40), "recipient": high.Address, "raw_amount": "100000000"}
	p.request(t, "POST", "/mock/transfers", processMockKey, "", highTransfer, 201, nil)
	p.request(t, "POST", "/mock/advance", processMockKey, "", map[string]int{"blocks": 2}, 200, nil)
	awaitProcessCondition(t, 10*time.Second, func() bool { return p.invoice(t, high.ID).PaymentStatus == "REJECTED" })
	high = p.invoice(t, high.ID)
	if high.Status != "OPEN" || high.RefundStatus != "READY" || high.Refund.ID == "" || high.Refund.Amount != "100.000000" {
		t.Fatal("high risk did not prepare the full refund", high)
	}
	financialState(t, s, invoice.Invoice{ID: high.ID}, map[ledger.AccountKind]money.USDT{ledger.Asset: 100_000_000, ledger.Refund: 100_000_000, ledger.PartnerAvailable: 99_000_000, ledger.Income: 1_000_000})
	p.kill(t) // Both decisions have committed; no process memory survives.
	p = startGatewayProcess(t, binary, pool, false)
	for _, tc := range []struct {
		key string
		v   processInvoice
		tr  map[string]string
	}{{"process-low", credited, lowTransfer}, {"process-high", high, highTransfer}} {
		var replay processInvoice
		p.request(t, "POST", "/invoices", alphaKey, tc.key, map[string]string{"amount": "100.000000"}, 201, &replay)
		if replay.ID != tc.v.ID || replay.Address != tc.v.Address {
			t.Fatal("restart lost invoice idempotency", replay)
		}
		p.request(t, "POST", "/mock/transfers", processMockKey, "", tc.tr, 201, nil)
		got := p.invoice(t, tc.v.ID)
		if got.Status != tc.v.Status || got.PaymentStatus != tc.v.PaymentStatus || got.Payment.ID != tc.v.Payment.ID || got.Refund != tc.v.Refund {
			t.Fatal("restart changed a committed decision", got)
		}
	}
	if rowCount(t, s, "ledger_operations") != 4 || rowCount(t, s, "ledger_entries") != 9 || rowCount(t, s, "aml_results") != 2 || rowCount(t, s, "refunds") != 1 {
		t.Fatal("restart duplicated a credit or refund")
	}
	var unbalanced int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM (SELECT operation_id FROM ledger_entries GROUP BY operation_id HAVING sum(CASE WHEN side='DEBIT' THEN amount ELSE -amount END)<>0) bad`).Scan(&unbalanced); err != nil || unbalanced != 0 {
		t.Fatal("unbalanced operation", unbalanced, err)
	}
}
