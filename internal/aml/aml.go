package aml

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
)

type Risk string

const (
	Low  Risk = "LOW"
	High Risk = "HIGH"
)

var (
	ErrUnavailable = errors.New("AML provider unavailable")
	ErrRisk        = errors.New("invalid AML risk")
)

type Subject struct {
	PaymentID          string
	ObservationVersion int64
	Sender             string
}

type Provider interface {
	Check(context.Context, Subject) (Risk, error)
}

// Mock's blacklist is immutable; only availability changes at runtime.
type Mock struct {
	blacklist   map[string]bool
	unavailable atomic.Bool
}

func NewMock(blacklist []string, unavailable bool) *Mock {
	m := &Mock{blacklist: make(map[string]bool, len(blacklist))}
	for _, address := range blacklist {
		m.blacklist[strings.ToLower(address)] = true
	}
	m.SetUnavailable(unavailable)
	return m
}

func (m *Mock) SetUnavailable(value bool) { m.unavailable.Store(value) }

func (m *Mock) Check(ctx context.Context, subject Subject) (Risk, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if m.unavailable.Load() {
		return "", ErrUnavailable
	}
	if m.blacklist[strings.ToLower(subject.Sender)] {
		return High, nil
	}
	return Low, nil
}
