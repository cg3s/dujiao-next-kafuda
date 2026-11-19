package application

import (
	"context"
	"errors"
	"net/http"
	"time"

	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
)

var (
	ErrKnownOrderLookupFailed = errors.New("kafuda known order lookup failed")
	ErrKnownOrderRateLimited  = errors.New("kafuda known order lookup rate limited")
)

const DefaultPollReviewAfter = 24 * time.Hour

// PollKnownOrder performs a read-only lookup outside the database transaction.
// A failed lookup is rescheduled with the task context even if its lookup budget expires.
// The caller applies a successful result with the corresponding shop order update.
func (s *Service) PollKnownOrder(ctx, lookupCtx context.Context, gateway kafudacontract.Gateway, attemptID uint, now, retryAt time.Time, reviewAfter time.Duration) (*kafudacontract.OrderResult, error) {
	if gateway == nil || now.IsZero() || !retryAt.After(now) {
		return nil, kafudadomain.ErrInvalidTransition
	}
	started := time.Now()
	attempt, err := s.store.GetAttempt(lookupCtx, attemptID)
	if err != nil {
		return nil, err
	}
	if attempt == nil {
		return nil, kafudadomain.ErrNotFound
	}
	if !attempt.Status.Active() || attempt.Status == kafudadomain.AttemptChecking ||
		attempt.UpstreamOrderID == nil || *attempt.UpstreamOrderID == 0 ||
		attempt.NextPollAt == nil || attempt.NextPollAt.After(now) || now.Before(attempt.UpdatedAt) {
		return nil, kafudadomain.ErrConflict
	}
	record, err := s.store.GetRecharge(lookupCtx, attempt.OrderID)
	if err != nil {
		return nil, err
	}
	if record == nil || record.CurrentAttemptID == nil || *record.CurrentAttemptID != attemptID {
		return nil, kafudadomain.ErrConflict
	}
	if err := lookupCtx.Err(); err != nil {
		return nil, err
	}
	result, err := gateway.GetOrder(lookupCtx, *attempt.UpstreamOrderID)
	if err == nil && result != nil && result.ID == *attempt.UpstreamOrderID &&
		result.Reference == attempt.Reference && result.Product == record.Snapshot.Product &&
		result.Plan == record.Snapshot.Plan && result.Country == record.Snapshot.Country {
		_, allowed := supplierResultStatus(attempt.Status, kafudadomain.AttemptStatus(result.Status))
		if allowed && (result.ChargedUSDCents == nil || *result.ChargedUSDCents >= 0) {
			return result, nil
		}
	}
	// Start backoff when the lookup ends, so a slow request cannot consume it.
	failedAt := now.Add(time.Since(started))
	retryAt = failedAt.Add(retryAt.Sub(now))
	var apiErr *kafudacontract.APIError
	rateLimited := errors.As(err, &apiErr) && apiErr != nil && apiErr.HTTPStatus == http.StatusTooManyRequests
	if rateLimited && apiErr.RetryAfter > 0 {
		resetAt := failedAt.Add(apiErr.RetryAfter)
		if resetAt.After(retryAt) {
			retryAt = resetAt
		}
	}
	// Never return raw gateway errors, which may contain upstream payloads.
	rescheduleErr := s.rescheduleKnownOrderPoll(ctx, attemptID, *attempt.NextPollAt, now, failedAt, retryAt, reviewAfter, "lookup_deadline_exceeded")
	if rateLimited {
		return nil, errors.Join(ErrKnownOrderLookupFailed, ErrKnownOrderRateLimited, rescheduleErr)
	}
	return nil, errors.Join(ErrKnownOrderLookupFailed, rescheduleErr)
}

// RescheduleKnownOrderResult retries a verified lookup whose local application
// failed. After the review deadline, the attempt remains active for polling.
func (s *Service) RescheduleKnownOrderResult(ctx context.Context, attemptID uint, scheduledAt, startedAt, now, nextPoll time.Time, reviewAfter time.Duration) error {
	return s.rescheduleKnownOrderPoll(ctx, attemptID, scheduledAt, startedAt, now, nextPoll, reviewAfter, kafudadomain.ReasonResultApplyFailed)
}

func (s *Service) rescheduleKnownOrderPoll(ctx context.Context, attemptID uint, scheduledAt, startedAt, now, nextPoll time.Time, reviewAfter time.Duration, reason string) error {
	if scheduledAt.IsZero() || startedAt.Before(scheduledAt) || now.Before(startedAt) || !nextPoll.After(now) {
		return kafudadomain.ErrInvalidTransition
	}
	if reviewAfter <= 0 {
		reviewAfter = DefaultPollReviewAfter
	}
	return s.change(ctx, attemptID, func(record *kafudadomain.Recharge, attempt *kafudadomain.Attempt) error {
		if !attempt.Status.Active() || attempt.Status == kafudadomain.AttemptChecking ||
			attempt.UpstreamOrderID == nil || *attempt.UpstreamOrderID == 0 {
			return kafudadomain.ErrInvalidTransition
		}
		if attempt.NextPollAt == nil || !attempt.NextPollAt.Equal(scheduledAt) || startedAt.Before(attempt.UpdatedAt) {
			return kafudadomain.ErrConflict
		}
		attempt.NextPollAt, attempt.UpdatedAt = &nextPoll, now
		if now.Sub(attempt.CreatedAt) >= reviewAfter && record.Stage != kafudadomain.StageManualReview {
			attempt.Status = kafudadomain.AttemptReview
			record.Stage, record.ReasonCode = kafudadomain.StageManualReview, reason
		}
		record.UpdatedAt = now
		return nil
	})
}

// RecoverExpiredChecks releases attempts interrupted before a potentially
// chargeable request. The buyer must submit fresh credentials to try again.
// Submitting and unknown attempts retain their reference and active slot.
func (s *Service) RecoverExpiredChecks(ctx context.Context, now time.Time, limit int) error {
	if now.IsZero() {
		return kafudadomain.ErrInvalidTransition
	}
	attempts, err := s.store.DueCheckingAttempts(ctx, now, limit)
	if err != nil {
		return err
	}
	return s.recoverAttempts(ctx, attempts, func(record *kafudadomain.Recharge, attempt *kafudadomain.Attempt) error {
		if attempt.Status != kafudadomain.AttemptChecking || attempt.NextPollAt == nil || attempt.NextPollAt.After(now) {
			return kafudadomain.ErrConflict
		}
		attempt.Status, attempt.ActiveOrderID, attempt.NextPollAt = kafudadomain.AttemptFailed, nil, nil
		attempt.UpdatedAt = now
		record.Stage, record.ReasonCode, record.UpdatedAt = kafudadomain.StageWaiting, "check_interrupted", now
		return nil
	})
}

// RecoverUnidentifiedSubmissions flags overdue requests that may have reached
// the supplier. The active slot and reference remain reserved for reconciliation.
func (s *Service) RecoverUnidentifiedSubmissions(ctx context.Context, now time.Time, limit int) error {
	if now.IsZero() {
		return kafudadomain.ErrInvalidTransition
	}
	attempts, err := s.store.DueUnidentifiedSubmissions(ctx, now, limit)
	if err != nil {
		return err
	}
	return s.recoverAttempts(ctx, attempts, func(record *kafudadomain.Recharge, attempt *kafudadomain.Attempt) error {
		if attempt.Status != kafudadomain.AttemptSubmitting || attempt.UpstreamOrderID != nil ||
			attempt.NextPollAt == nil || attempt.NextPollAt.After(now) {
			return kafudadomain.ErrConflict
		}
		attempt.Status, attempt.UpdatedAt = kafudadomain.AttemptReview, now
		record.Stage, record.ReasonCode, record.UpdatedAt = kafudadomain.StageManualReview, "outcome_unknown", now
		return nil
	})
}

func (s *Service) recoverAttempts(ctx context.Context, attempts []kafudadomain.Attempt, update func(*kafudadomain.Recharge, *kafudadomain.Attempt) error) error {
	var failures []error
	for _, candidate := range attempts {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		err := s.change(ctx, candidate.ID, update)
		if err != nil && !errors.Is(err, kafudadomain.ErrConflict) && !errors.Is(err, kafudadomain.ErrNotFound) {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
