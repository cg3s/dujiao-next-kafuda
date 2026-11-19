package consumer

import (
	"context"
	"errors"
	"time"

	kafudaapp "github.com/dujiao-next/internal/modules/kafudagpt/application"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
	kafudaworkflow "github.com/dujiao-next/internal/workflows/kafudagpt/application"
	"github.com/hibiken/asynq"
)

const (
	kafudaMaxRetryDelay = 15 * time.Minute
)

func kafudaRetryDelay(attempt kafudadomain.Attempt, baseInterval time.Duration) time.Duration {
	if attempt.NextPollAt == nil {
		return baseInterval
	}
	lastDelay := attempt.NextPollAt.Sub(attempt.UpdatedAt)
	if lastDelay <= 0 {
		return baseInterval
	}
	if lastDelay >= kafudaMaxRetryDelay/2 {
		return kafudaMaxRetryDelay
	}
	return max(baseInterval, 2*lastDelay)
}

func (c *Consumer) handleKafudaRecoverChecks(ctx context.Context, _ *asynq.Task) error {
	if c == nil || c.Container == nil || c.KafudaGPT == nil || c.KafudaGPT.Records == nil {
		return nil
	}
	module := c.KafudaGPT
	now := time.Now().UTC()
	recoveryErr := errors.Join(
		module.Records.RecoverExpiredChecks(ctx, now, 100),
		module.Records.RecoverUnidentifiedSubmissions(ctx, now, 100),
	)
	if module.Store == nil || c.OrderStore == nil {
		return recoveryErr
	}
	if c.orderStatusQueue != nil && c.orderStatusQueue.Enabled() {
		pending, err := module.Store.PendingCompletionEmails(ctx, now, 100)
		recoveryErr = errors.Join(recoveryErr, err)
		for _, childID := range pending {
			kafudaworkflow.EnqueueCompletionEmail(ctx, c.OrderStore, module.Store, c.orderStatusQueue, c.SettingService, c.Config, childID)
		}
	}
	if module.Gateway == nil {
		return recoveryErr
	}
	return errors.Join(recoveryErr, c.pollKnownKafudaOrders(ctx))
}

func (c *Consumer) pollKnownKafudaOrders(ctx context.Context) error {
	lookupCtx, cancel := context.WithTimeout(ctx, 55*time.Second)
	defer cancel()
	module := c.KafudaGPT
	attempts, err := module.Store.DueKnownOrderPolls(lookupCtx, time.Now().UTC(), 10)
	if err != nil {
		return err
	}
	var failures []error
	interval := c.Config.KafudaPollInterval()
	var reviewAfter time.Duration
	if c.Config != nil {
		seconds := c.Config.KafudaGPT.PollReviewAfterSeconds
		if seconds > 0 && int64(seconds) <= int64((1<<63-1)/time.Second) {
			reviewAfter = time.Duration(seconds) * time.Second
		}
	}
	for _, attempt := range attempts {
		if err := lookupCtx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		lookupStarted := time.Now()
		startedAt := lookupStarted.UTC()
		retryDelay := kafudaRetryDelay(attempt, interval)
		result, err := module.Records.PollKnownOrder(ctx, lookupCtx, module.Gateway, attempt.ID, startedAt, startedAt.Add(retryDelay), reviewAfter)
		if err != nil {
			if !errors.Is(err, kafudadomain.ErrConflict) && !errors.Is(err, kafudadomain.ErrNotFound) {
				failures = append(failures, err)
			}
			if errors.Is(err, kafudaapp.ErrKnownOrderRateLimited) {
				break
			}
			continue
		}
		receivedAt := startedAt.Add(time.Since(lookupStarted))
		completed, err := kafudaworkflow.ApplyResult(c.OrderStore, attempt.OrderID, attempt.ID, *result, startedAt, receivedAt, interval)
		if completed {
			kafudaworkflow.EnqueueCompletionEmail(ctx, c.OrderStore, module.Store, c.orderStatusQueue, c.SettingService, c.Config, attempt.OrderID)
		}
		if err == nil || errors.Is(err, kafudadomain.ErrConflict) || errors.Is(err, kafudadomain.ErrNotFound) {
			continue
		}
		retryFrom := startedAt.Add(time.Since(lookupStarted))
		if attempt.NextPollAt != nil {
			err = errors.Join(err, module.Records.RescheduleKnownOrderResult(ctx, attempt.ID, *attempt.NextPollAt, startedAt, retryFrom, retryFrom.Add(retryDelay), reviewAfter))
		}
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}
