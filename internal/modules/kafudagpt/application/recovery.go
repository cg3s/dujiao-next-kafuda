package application

import (
	"context"
	"errors"
	"time"

	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
)

// RescheduleKnownOrderPoll defers a failed numeric lookup. A worker that read
// an older schedule cannot delay a lookup already rescheduled by another worker.
func (s *Service) RescheduleKnownOrderPoll(ctx context.Context, attemptID uint, scheduledAt, now, nextPoll time.Time) error {
	if scheduledAt.IsZero() || now.Before(scheduledAt) || !nextPoll.After(now) {
		return kafudadomain.ErrInvalidTransition
	}
	return s.change(ctx, attemptID, func(record *kafudadomain.Recharge, attempt *kafudadomain.Attempt) error {
		if !attempt.Status.Active() || attempt.Status == kafudadomain.AttemptChecking ||
			attempt.UpstreamOrderID == nil || *attempt.UpstreamOrderID == 0 {
			return kafudadomain.ErrInvalidTransition
		}
		if attempt.NextPollAt == nil || !attempt.NextPollAt.Equal(scheduledAt) || now.Before(attempt.UpdatedAt) {
			return kafudadomain.ErrConflict
		}
		attempt.NextPollAt, attempt.UpdatedAt = &nextPoll, now
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
	var failures []error
	for _, candidate := range attempts {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		err := s.change(ctx, candidate.ID, func(record *kafudadomain.Recharge, attempt *kafudadomain.Attempt) error {
			// Recheck under the same locks used by MarkSubmitting. A live
			// request or another worker may have advanced since the scan.
			if attempt.Status != kafudadomain.AttemptChecking || attempt.NextPollAt == nil || attempt.NextPollAt.After(now) {
				return kafudadomain.ErrConflict
			}
			attempt.Status, attempt.ActiveOrderID, attempt.NextPollAt = kafudadomain.AttemptFailed, nil, nil
			attempt.UpdatedAt = now
			record.Stage, record.ReasonCode, record.UpdatedAt = kafudadomain.StageWaiting, "check_interrupted", now
			return nil
		})
		if err != nil && !errors.Is(err, kafudadomain.ErrConflict) && !errors.Is(err, kafudadomain.ErrNotFound) {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
