package payment

import (
	"context"
	"errors"
	"time"

	"crypto-payment-gateway/internal/aml"
	"crypto-payment-gateway/internal/ledger"
	"crypto-payment-gateway/internal/refund"
)

var (
	ErrStaleJob     = errors.New("AML job ownership or observation changed")
	ErrNotConfirmed = errors.New("payment no longer has enough confirmations")
)

type Job struct {
	aml.Subject
	LeaseVersion int64
}

type Processing interface {
	PrepareReady(context.Context, int) error
	ClaimAML(context.Context, time.Duration) (Job, bool, error)
	CompleteAML(context.Context, Job, aml.Risk, int, refund.Policy) error
	RetryAML(context.Context, Job, string, time.Duration) error
}

type Worker struct {
	Store         Processing
	Provider      aml.Provider
	Confirmations int
	RetryInterval time.Duration
	Lease         time.Duration
	RefundPolicy  refund.Policy
}

// RunOnce processes one claimed job; each ready deposit has its own transaction.
// Provider calls never hold a database transaction or financial row locks.
func (w Worker) RunOnce(ctx context.Context) (bool, error) {
	prepareErr := w.Store.PrepareReady(ctx, w.Confirmations)
	job, found, err := w.Store.ClaimAML(ctx, w.Lease)
	if err != nil || !found {
		return false, errors.Join(prepareErr, err)
	}
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	risk, err := w.Provider.Check(checkCtx, job.Subject)
	cancel()
	if err == nil && risk != aml.Low && risk != aml.High {
		err = aml.ErrRisk
	}
	if err == nil {
		err = w.Store.CompleteAML(ctx, job, risk, w.Confirmations, w.RefundPolicy)
	}
	if errors.Is(err, ErrStaleJob) {
		return true, prepareErr
	}
	if err != nil {
		reason := "processing_failed"
		switch {
		case errors.Is(err, aml.ErrUnavailable):
			reason = "aml_unavailable"
		case errors.Is(err, aml.ErrRisk):
			reason = "invalid_aml_result"
		case errors.Is(err, ledger.ErrBalance):
			reason = "balance_limit"
		case errors.Is(err, ErrNotConfirmed):
			reason = "awaiting_confirmations"
		}
		err = errors.Join(err, w.Store.RetryAML(ctx, job, reason, w.RetryInterval))
	}
	return true, errors.Join(prepareErr, err)
}
