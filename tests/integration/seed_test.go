package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"crypto-payment-gateway/internal/partner"
	"crypto-payment-gateway/internal/postgres"
	"github.com/google/uuid"
)

func TestSeedRejectsUnsafeKeysWithoutChangingPartners(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	for _, key := range []string{" local-alpha-key", "local-alpha-key ", "local alpha key", "local-alpha-key\r\n", "локальный-ключ", strings.Repeat("x", 257)} {
		for _, keys := range [][2]string{{key, betaKey}, {alphaKey, key}} {
			if err := s.Seed(ctx, keys[0], keys[1]); err == nil {
				t.Fatal("accepted a key that cannot authenticate over HTTP")
			}
		}
	}
	for _, want := range []struct{ key, id string }{{alphaKey, postgres.AlphaID}, {betaKey, postgres.BetaID}} {
		got, err := s.Authenticate(ctx, want.key)
		if err != nil || got.ID != want.id {
			t.Fatal("invalid seed changed existing keys", err)
		}
	}
	var count int
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM partners").Scan(&count); err != nil || count != 2 {
		t.Fatal("invalid seed changed partner count", count, err)
	}
}

func TestSeedSwapsKeysPreservingFinancialData(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	fundedInvoice(t, s, 100)
	if _, err := s.pool.Exec(ctx, `UPDATE partners SET fee_bps=150 WHERE id=$1`, postgres.AlphaID); err != nil {
		t.Fatal(err)
	}
	snapshot := func() string {
		t.Helper()
		var data string
		err := s.pool.QueryRow(ctx, `SELECT jsonb_build_object(
			'partners',(SELECT jsonb_agg(jsonb_build_object('id',id,'name',name,'fee_bps',fee_bps) ORDER BY id) FROM partners),
			'invoices',(SELECT jsonb_agg(i ORDER BY i.id) FROM invoices i),
			'accounts',(SELECT jsonb_agg(a ORDER BY a.id) FROM ledger_accounts a),
			'operations',(SELECT jsonb_agg(o ORDER BY o.id) FROM ledger_operations o),
			'entries',(SELECT jsonb_agg(e ORDER BY e.id) FROM ledger_entries e)
		)::text`).Scan(&data)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	before := snapshot()
	restarted := restartStore(t, s)
	for range 2 {
		if err := restarted.Seed(ctx, betaKey, alphaKey); err != nil {
			t.Fatal("key swap failed", err)
		}
		for _, want := range []struct{ key, id string }{{betaKey, postgres.AlphaID}, {alphaKey, postgres.BetaID}} {
			got, err := restarted.Authenticate(ctx, want.key)
			if err != nil || got.ID != want.id {
				t.Fatal("swapped key belongs to wrong partner", got, err)
			}
		}
		if snapshot() != before {
			t.Fatal("key swap changed financial data or partner settings")
		}
	}
}

func TestSeedKeyConflictRollsBackBothPartners(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	id, err := uuid.NewRandom()
	if err != nil {
		t.Fatal(err)
	}
	const occupied = "another-partner-key"
	hash := sha256.Sum256([]byte(occupied))
	if _, err := s.pool.Exec(ctx, `INSERT INTO partners(id,name,api_key_hash,fee_bps) VALUES($1,'other',$2,0)`, id.String(), hex.EncodeToString(hash[:])); err != nil {
		t.Fatal(err)
	}
	const newAlpha = "changed-alpha-key"
	if err := s.Seed(ctx, newAlpha, occupied); err == nil {
		t.Fatal("key belonging to another partner accepted")
	}
	for _, want := range []struct{ key, id string }{{alphaKey, postgres.AlphaID}, {betaKey, postgres.BetaID}, {occupied, id.String()}} {
		got, err := s.Authenticate(ctx, want.key)
		if err != nil || got.ID != want.id {
			t.Fatal("failed rotation changed keys", got, err)
		}
	}
	if _, err := s.Authenticate(ctx, newAlpha); !errors.Is(err, partner.ErrUnauthorized) {
		t.Fatal("failed rotation kept a partial update", err)
	}
}

func TestSeedConcurrentKeySwaps(t *testing.T) {
	s := fixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	const workers = 8
	start, results := make(chan struct{}), make(chan error, workers)
	for n := range workers {
		go func() {
			<-start
			if n%2 == 0 {
				results <- s.Seed(ctx, alphaKey, betaKey)
			} else {
				results <- s.Seed(ctx, betaKey, alphaKey)
			}
		}()
	}
	close(start)
	for range workers {
		if err := <-results; err != nil {
			t.Fatal("concurrent key rotation failed", err)
		}
	}
	a, err := s.Authenticate(ctx, alphaKey)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Authenticate(ctx, betaKey)
	if err != nil || a.ID == b.ID || (a.ID != postgres.AlphaID && a.ID != postgres.BetaID) || (b.ID != postgres.AlphaID && b.ID != postgres.BetaID) {
		t.Fatal("concurrent rotation left inconsistent keys", a, b, err)
	}
}
