package invoice

import (
	"crypto/rand"
	"encoding/hex"
)

// NewTestDepositAddress returns a simulated address without a corresponding private key.
func NewTestDepositAddress() (string, error) {
	var bytes [20]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return "0x" + hex.EncodeToString(bytes[:]), nil
}
