package ledger

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"crypto-payment-gateway/internal/money"
	"github.com/google/uuid"
)

type Side string

const (
	Debit  Side = "DEBIT"
	Credit Side = "CREDIT"
)

var (
	ErrEntry    = errors.New("invalid or unbalanced ledger entries")
	ErrConflict = errors.New("ledger operation key reused with different entries")
)

type Entry struct {
	AccountID string
	Side      Side
	Amount    money.USDT
}

// Fingerprint validates and canonicalizes entries without changing the caller's slice.
// Account IDs must be canonical UUIDs, matching PostgreSQL's UUID identity.
func Fingerprint(entries []Entry) (string, error) {
	if len(entries) < 2 {
		return "", ErrEntry
	}
	var debit, credit money.USDT
	for _, e := range entries {
		accountID, err := uuid.Parse(e.AccountID)
		if err != nil || accountID.String() != e.AccountID || e.Amount <= 0 {
			return "", ErrEntry
		}
		switch e.Side {
		case Debit:
			debit, err = money.Add(debit, e.Amount)
		case Credit:
			credit, err = money.Add(credit, e.Amount)
		default:
			return "", ErrEntry
		}
		if err != nil {
			return "", ErrEntry
		}
	}
	if debit != credit {
		return "", ErrEntry
	}
	normalized := append([]Entry(nil), entries...)
	sort.Slice(normalized, func(i, j int) bool {
		a, b := normalized[i], normalized[j]
		if a.AccountID != b.AccountID {
			return a.AccountID < b.AccountID
		}
		if a.Side != b.Side {
			return a.Side < b.Side
		}
		return a.Amount < b.Amount
	})
	data, err := fingerprintBytes(normalized)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:]), nil
}

// This private wire format preserves existing hashes independently of Entry's
// fields and the public money JSON format. Field order, names and six decimals
// are persisted data: changing them requires an explicit compatibility strategy.
type fingerprintEntry struct {
	AccountID string `json:"AccountID"`
	Side      string `json:"Side"`
	Amount    string `json:"Amount"`
}

func fingerprintBytes(sorted []Entry) ([]byte, error) {
	encoded := make([]fingerprintEntry, len(sorted))
	for i, entry := range sorted {
		units := int64(entry.Amount)
		encoded[i] = fingerprintEntry{
			AccountID: entry.AccountID,
			Side:      string(entry.Side),
			Amount:    fmt.Sprintf("%d.%06d", units/1_000_000, units%1_000_000),
		}
	}
	return json.Marshal(encoded)
}
