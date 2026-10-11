package integration

import (
	"context"
	"strings"
	"testing"

	"crypto-payment-gateway/internal/postgres"
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
