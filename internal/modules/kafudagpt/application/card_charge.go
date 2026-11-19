package application

import (
	"context"
	"errors"
	"net/http"
	"time"

	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
)

const (
	firstCardChargePollDelay = time.Minute
	cardChargePollLease      = time.Minute
	cardChargeRetryDelay     = 30 * time.Minute
	cardChargePollWindow     = 24 * time.Hour
)

// DueCardChargePolls also discovers recent completions from before polling was enabled.
// The window starts at the locally recorded completion, regardless of supplier timestamps.
func (s *Service) DueCardChargePolls(ctx context.Context, now time.Time, limit int) ([]kafudadomain.Attempt, error) {
	if now.IsZero() {
		return nil, kafudadomain.ErrInvalidTransition
	}
	now = now.UTC()
	return s.store.DueCardChargePolls(ctx, now, now.Add(-cardChargePollWindow), now.Add(-firstCardChargePollDelay), limit)
}

// PollCardCharge reads an already completed upstream order and fills in its missing
// card debit. The claim and save are independent of the shop order transaction.
func (s *Service) PollCardCharge(ctx, lookupCtx context.Context, gateway kafudacontract.Gateway, candidate kafudadomain.Attempt, now time.Time) error {
	if gateway == nil || now.IsZero() {
		return kafudadomain.ErrInvalidTransition
	}
	if candidate.ID == 0 || candidate.OrderID == 0 || candidate.Status != kafudadomain.AttemptCompleted ||
		candidate.UpstreamOrderID == nil || *candidate.UpstreamOrderID == 0 || candidate.ChargedUSDCents != nil ||
		(candidate.NextCardChargePollAt != nil && candidate.NextCardChargePollAt.After(now)) {
		return kafudadomain.ErrConflict
	}
	started := time.Now()
	now = now.UTC()
	record, err := s.store.GetRecharge(lookupCtx, candidate.OrderID)
	if err != nil {
		return err
	}
	if record == nil || record.Stage != kafudadomain.StageCompleted ||
		record.CurrentAttemptID == nil || *record.CurrentAttemptID != candidate.ID ||
		record.CompletedAt == nil || !record.CompletedAt.After(now.Add(-cardChargePollWindow)) ||
		record.CompletedAt.After(now.Add(-firstCardChargePollDelay)) {
		return kafudadomain.ErrConflict
	}
	// Match the lease exactly on save, including PostgreSQL timestamp precision.
	leaseUntil := now.Add(cardChargePollLease).Truncate(time.Microsecond)
	claimed, err := s.store.ClaimCardChargePoll(lookupCtx, candidate, leaseUntil)
	if err != nil {
		return err
	}
	if !claimed {
		return kafudadomain.ErrConflict
	}

	result, lookupErr := gateway.GetOrder(lookupCtx, *candidate.UpstreamOrderID)
	var charge *int64
	var failure error
	if lookupErr == nil && result != nil && result.ID == *candidate.UpstreamOrderID &&
		result.Reference == candidate.Reference && result.Product == record.Snapshot.Product &&
		result.Plan == record.Snapshot.Plan && result.Country == record.Snapshot.Country &&
		result.Status == string(kafudadomain.AttemptCompleted) &&
		(result.ChargedUSDCents == nil || *result.ChargedUSDCents >= 0) {
		charge = result.ChargedUSDCents
	} else {
		// Gateway errors may contain private supplier data.
		failure = ErrKnownOrderLookupFailed
	}

	var next *time.Time
	if charge == nil {
		delay := cardChargeRetryDelay
		var apiErr *kafudacontract.APIError
		if errors.As(lookupErr, &apiErr) && apiErr != nil && apiErr.HTTPStatus == http.StatusTooManyRequests {
			delay = max(delay, apiErr.RetryAfter)
			failure = errors.Join(failure, ErrKnownOrderRateLimited)
		}
		due := now.Add(time.Since(started)).Add(delay).Truncate(time.Microsecond)
		next = &due
	}
	// The lookup context may have expired while getting the upstream order.
	saved, saveErr := s.store.SaveCardChargePoll(ctx, candidate.ID, leaseUntil, charge, next)
	if saveErr == nil && !saved {
		saveErr = kafudadomain.ErrConflict
	}
	return errors.Join(failure, saveErr)
}
