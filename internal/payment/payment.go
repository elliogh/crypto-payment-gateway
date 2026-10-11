// Package payment describes a deposit's current observation and confirmation state.
package payment

type Status string

const (
	Confirming Status = "CONFIRMING"
	Orphaned   Status = "ORPHANED"
)

type Observation struct {
	ID            string
	Status        Status
	Version       int64
	Confirmations int64
}
