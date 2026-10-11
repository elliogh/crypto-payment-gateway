package config

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func validEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{"PGHOST", "PGPORT", "PGUSER", "PGPASSWORD", "PGDATABASE", "PGSSLMODE"} {
		t.Setenv(name, "")
	}
	t.Setenv("DATABASE_URL", "postgres://local")
	t.Setenv("PARTNER_ALPHA_KEY", "local-alpha-key")
	t.Setenv("PARTNER_BETA_KEY", "local-beta-key")
	t.Setenv("MOCK_CONTROL_KEY", "local-control-key")
	t.Setenv("LOCAL_MOCKS", "true")
	t.Setenv("CONFIRMATIONS", "3")
	t.Setenv("AML_RETRY_INTERVAL", "5s")
}

func TestLoad(t *testing.T) {
	validEnvironment(t)
	if _, err := Load(); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		value string
	}{
		{name: "CONFIRMATIONS", value: "0"},
		{name: "CONFIRMATIONS", value: "abc"},
		{name: "AML_RETRY_INTERVAL", value: "0s"},
		{name: "LOCAL_MOCKS", value: "yes"},
		{name: "DATABASE_URL", value: ""},
		{name: "PARTNER_BETA_KEY", value: "local-alpha-key"},
		{name: "MOCK_CONTROL_KEY", value: "local-alpha-key"},
		{name: "PARTNER_ALPHA_KEY", value: " local-alpha-key"},
		{name: "PARTNER_BETA_KEY", value: "local-beta-key "},
		{name: "PARTNER_ALPHA_KEY", value: "local alpha key"},
		{name: "PARTNER_BETA_KEY", value: "local-beta-key\r\n"},
		{name: "PARTNER_ALPHA_KEY", value: "локальный-ключ"},
		{name: "MOCK_CONTROL_KEY", value: " local-control-key"},
		{name: "MOCK_CONTROL_KEY", value: strings.Repeat("x", 257)},
	}
	for _, tc := range tests {
		t.Run(tc.name+tc.value, func(t *testing.T) {
			t.Setenv(tc.name, tc.value)
			if _, err := Load(); err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
}

func TestSeparateDatabaseParameters(t *testing.T) {
	validEnvironment(t)
	t.Setenv("DATABASE_URL", "")
	t.Setenv("PGHOST", "localhost")
	t.Setenv("PGPORT", "54329")
	t.Setenv("PGUSER", "gateway")
	t.Setenv("PGDATABASE", "gateway")
	t.Setenv("PGSSLMODE", "disable")
	const password = "special#?%/@:'\\ password"
	t.Setenv("PGPASSWORD", password)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := pgx.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		t.Fatal("constructed connection URL cannot be parsed")
	}
	if parsed.Password != password || parsed.User != "gateway" || parsed.Database != "gateway" || parsed.Host != "localhost" || parsed.Port != 54329 || parsed.TLSConfig != nil {
		t.Fatal("database parameters changed during encoding")
	}
	t.Setenv("DATABASE_URL", "postgres://explicit@direct:5432/other?sslmode=disable")
	t.Setenv("PGPORT", "invalid") // Ignored when DATABASE_URL is explicit.
	cfg, err = Load()
	if err != nil || cfg.DatabaseURL != "postgres://explicit@direct:5432/other?sslmode=disable" {
		t.Fatal("explicit DATABASE_URL lost precedence")
	}
}

func TestIncompleteDatabaseParameters(t *testing.T) {
	for _, tc := range []struct{ name, value string }{
		{"PGHOST", ""}, {"PGUSER", ""}, {"PGDATABASE", ""},
		{"PGPORT", "invalid"}, {"PGPORT", "0"}, {"PGPORT", "65536"},
	} {
		t.Run(tc.name+tc.value, func(t *testing.T) {
			validEnvironment(t)
			t.Setenv("DATABASE_URL", "")
			t.Setenv("PGHOST", "localhost")
			t.Setenv("PGUSER", "gateway")
			t.Setenv("PGDATABASE", "gateway")
			t.Setenv(tc.name, tc.value)
			if _, err := Load(); err == nil {
				t.Fatal("accepted incomplete or invalid database parameters")
			}
		})
	}
}
