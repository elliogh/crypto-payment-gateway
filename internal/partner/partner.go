package partner

import (
	"context"
	"errors"
)

var ErrUnauthorized = errors.New("invalid API key")

// ValidAPIKey accepts keys that survive HTTP header transport unchanged.
func ValidAPIKey(key string) bool {
	if len(key) < 12 || len(key) > 256 {
		return false
	}
	for i := range key {
		if key[i] < 33 || key[i] > 126 {
			return false
		}
	}
	return true
}

type Partner struct {
	ID     string
	Name   string
	FeeBPS int
}

// Authenticator resolves the partner without exposing key storage to callers.
type Authenticator interface {
	Authenticate(context.Context, string) (Partner, error)
}
