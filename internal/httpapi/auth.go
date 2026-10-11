package httpapi

import (
	"context"
	"errors"
	"net/http"

	"crypto-payment-gateway/internal/partner"
)

type partnerContextKey struct{}

func (a *api) authorize(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("X-API-Key")
		if len(r.Header.Values("X-API-Key")) != 1 || len(key) == 0 || len(key) > 256 {
			fail(w, http.StatusUnauthorized, "unauthorized", "valid X-API-Key required")
			return
		}
		authenticated, err := a.Partners.Authenticate(r.Context(), key)
		if errors.Is(err, partner.ErrUnauthorized) {
			fail(w, http.StatusUnauthorized, "unauthorized", "valid X-API-Key required")
			return
		}
		if err != nil {
			a.internalError(w, "authenticate_partner", err)
			return
		}
		ctx := context.WithValue(r.Context(), partnerContextKey{}, authenticated)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func requestPartner(r *http.Request) partner.Partner {
	return r.Context().Value(partnerContextKey{}).(partner.Partner)
}
