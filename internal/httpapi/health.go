package httpapi

import (
	"context"
	"net/http"
	"time"
)

type healthResponse struct {
	Status string `json:"status"`
}

func (a *api) health(w http.ResponseWriter, r *http.Request) {
	reply(w, http.StatusOK, healthResponse{Status: "ok"})
}

func (a *api) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := a.Ready(ctx); err != nil {
		fail(w, http.StatusServiceUnavailable, "not_ready", "database unavailable")
		return
	}
	reply(w, http.StatusOK, healthResponse{Status: "ready"})
}
