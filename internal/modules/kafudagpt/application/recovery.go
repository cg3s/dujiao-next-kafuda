package application

import (
	"context"
	"errors"
	"time"

	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
)

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
