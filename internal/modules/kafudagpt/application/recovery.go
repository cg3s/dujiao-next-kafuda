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

const (
	DefaultPollReviewAfter = 24 * time.Hour
	knownOrderPollLease    = time.Minute
)

type KnownOrderPollLease struct {
	AttemptID   uint
	ScheduledAt time.Time
	Until       time.Time
}

type KnownOrderPoll struct {
	kafudacontract.OrderResult
	Lease KnownOrderPollLease
}

// PollKnownOrder claims a due attempt before looking it up outside the database transaction.
// A failed lookup is rescheduled with the task context even if its lookup budget expires.
// The caller applies a successful result with the corresponding shop order update.
func (s *Service) PollKnownOrder(ctx, lookupCtx context.Context, gateway kafudacontract.Gateway, attemptID uint, now, retryAt time.Time, reviewAfter time.Duration) (*KnownOrderPoll, error) {
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
	lease := KnownOrderPollLease{
		AttemptID: attempt.ID, ScheduledAt: *attempt.NextPollAt,
		Until: now.Add(knownOrderPollLease).UTC().Truncate(time.Microsecond),
	}
	claimed, err := s.store.ClaimKnownOrderPoll(lookupCtx, *attempt, now, lease.Until)
	if err != nil {
		return nil, err
	}
	if !claimed {
		return nil, kafudadomain.ErrConflict
	}
	result, err := gateway.GetOrder(lookupCtx, *attempt.UpstreamOrderID)
	if err == nil && result != nil && result.ID == *attempt.UpstreamOrderID &&
		result.Reference == attempt.Reference && result.Product == record.Snapshot.Product &&
		result.Plan == record.Snapshot.Plan && result.Country == record.Snapshot.Country {
		_, allowed := supplierResultStatus(attempt.Status, kafudadomain.AttemptStatus(result.Status))
		if allowed && (result.ChargedUSDCents == nil || *result.ChargedUSDCents >= 0) {
			return &KnownOrderPoll{OrderResult: *result, Lease: lease}, nil
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
	rescheduleErr := s.rescheduleKnownOrderPoll(ctx, lease, now, failedAt, retryAt, reviewAfter, "lookup_deadline_exceeded")
	if rateLimited {
		return nil, errors.Join(ErrKnownOrderLookupFailed, ErrKnownOrderRateLimited, rescheduleErr)
	}
	return nil, errors.Join(ErrKnownOrderLookupFailed, rescheduleErr)
}

// RescheduleKnownOrderResult retries a verified lookup whose local application
// failed. After the review deadline, the attempt remains active for polling.
func (s *Service) RescheduleKnownOrderResult(ctx context.Context, lease KnownOrderPollLease, startedAt, now, nextPoll time.Time, reviewAfter time.Duration) error {
	return s.rescheduleKnownOrderPoll(ctx, lease, startedAt, now, nextPoll, reviewAfter, kafudadomain.ReasonResultApplyFailed)
}

func (s *Service) rescheduleKnownOrderPoll(ctx context.Context, lease KnownOrderPollLease, startedAt, now, nextPoll time.Time, reviewAfter time.Duration, reason string) error {
	if lease.AttemptID == 0 || lease.ScheduledAt.IsZero() || startedAt.Before(lease.ScheduledAt) ||
		!lease.Until.After(startedAt) || now.Before(startedAt) || !nextPoll.After(now) {
		return kafudadomain.ErrInvalidTransition
	}
	if reviewAfter <= 0 {
		reviewAfter = DefaultPollReviewAfter
	}
	return s.change(ctx, lease.AttemptID, func(record *kafudadomain.Recharge, attempt *kafudadomain.Attempt) error {
		if !attempt.Status.Active() || attempt.Status == kafudadomain.AttemptChecking ||
			attempt.UpstreamOrderID == nil || *attempt.UpstreamOrderID == 0 {
			return kafudadomain.ErrInvalidTransition
		}
		if attempt.KnownOrderPollLeaseUntil == nil || !attempt.KnownOrderPollLeaseUntil.Equal(lease.Until) ||
			attempt.NextPollAt == nil || !attempt.NextPollAt.Equal(lease.ScheduledAt) || startedAt.Before(attempt.UpdatedAt) {
			return kafudadomain.ErrConflict
		}
		attempt.NextPollAt, attempt.UpdatedAt = &nextPoll, now
		attempt.KnownOrderPollLeaseUntil = nil
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
