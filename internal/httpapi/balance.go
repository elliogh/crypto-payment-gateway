package httpapi

import "net/http"

type balanceResponse struct {
	Currency  string `json:"currency"`
	Available string `json:"available"`
}

func (a *api) balance(w http.ResponseWriter, r *http.Request) {
	balance, err := a.Balances.Balance(r.Context(), requestPartner(r).ID)
	if err != nil {
		a.internalError(w, "read_balance", err)
		return
	}
	reply(w, http.StatusOK, balanceResponse{
		Currency:  balance.Currency,
		Available: balance.Available.String(),
	})
}
