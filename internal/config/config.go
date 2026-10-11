package config

import (
	"errors"
	"net"
	"net/url"
	"os"
	"strconv"
	"time"

	"crypto-payment-gateway/internal/partner"
)

type Config struct {
	DatabaseURL      string
	HTTPAddr         string
	Confirmations    int
	LocalMocks       bool
	MockControlKey   string
	AMLRetryInterval time.Duration
	AlphaKey         string
	BetaKey          string
}

func Load() (Config, error) {
	c := Config{
		DatabaseURL:    os.Getenv("DATABASE_URL"),
		HTTPAddr:       value("HTTP_ADDR", ":8080"),
		MockControlKey: os.Getenv("MOCK_CONTROL_KEY"),
		AlphaKey:       os.Getenv("PARTNER_ALPHA_KEY"),
		BetaKey:        os.Getenv("PARTNER_BETA_KEY"),
	}
	var err error
	c.Confirmations, err = strconv.Atoi(value("CONFIRMATIONS", "3"))
	if err != nil || c.Confirmations < 1 {
		return Config{}, errors.New("CONFIRMATIONS must be positive")
	}
	c.LocalMocks, err = strconv.ParseBool(value("LOCAL_MOCKS", "false"))
	if err != nil {
		return Config{}, errors.New("LOCAL_MOCKS must be boolean")
	}
	c.AMLRetryInterval, err = time.ParseDuration(value("AML_RETRY_INTERVAL", "5s"))
	if err != nil || c.AMLRetryInterval <= 0 {
		return Config{}, errors.New("AML_RETRY_INTERVAL must be positive duration")
	}
	c.DatabaseURL, err = databaseURL(c.DatabaseURL)
	if err != nil {
		return Config{}, err
	}
	if !partner.ValidAPIKey(c.AlphaKey) || !partner.ValidAPIKey(c.BetaKey) || c.AlphaKey == c.BetaKey {
		return Config{}, errors.New("distinct partner API keys of 12..256 printable ASCII bytes without spaces are required")
	}
	if c.LocalMocks && (!partner.ValidAPIKey(c.MockControlKey) || c.MockControlKey == c.AlphaKey || c.MockControlKey == c.BetaKey) {
		return Config{}, errors.New("local mock control requires a separate key of 12..256 printable ASCII bytes without spaces")
	}
	return c, nil
}

// An explicit DATABASE_URL takes precedence. Separate parameters are URL-escaped,
// so characters such as #, ?, %, / and @ in a password retain their meaning.
func databaseURL(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	host, user, database := os.Getenv("PGHOST"), os.Getenv("PGUSER"), os.Getenv("PGDATABASE")
	if host == "" || user == "" || database == "" {
		return "", errors.New("DATABASE_URL or PGHOST, PGUSER and PGDATABASE are required")
	}
	port := value("PGPORT", "5432")
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", errors.New("PGPORT must be in 1..65535")
	}
	u := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(user, os.Getenv("PGPASSWORD")),
		Host:   net.JoinHostPort(host, port),
		Path:   "/" + database,
	}
	query := url.Values{"sslmode": {value("PGSSLMODE", "require")}}
	u.RawQuery = query.Encode()
	return u.String(), nil
}

func value(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
