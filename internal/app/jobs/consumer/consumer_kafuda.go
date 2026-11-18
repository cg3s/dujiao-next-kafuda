package consumer

import (
	"context"
	"errors"
	"time"

	"github.com/dujiao-next/internal/config"
	"github.com/dujiao-next/internal/constants"
	"github.com/dujiao-next/internal/logger"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
	orderapp "github.com/dujiao-next/internal/modules/order/application"
	kafudaworkflow "github.com/dujiao-next/internal/workflows/kafudagpt/application"
	"github.com/hibiken/asynq"
)

const (
	kafudaPollInterval  = time.Minute
	kafudaMaxRetryDelay = 15 * time.Minute
)

func kafudaRetryDelay(attempt kafudadomain.Attempt) time.Duration {
	if attempt.NextPollAt == nil {
		return kafudaPollInterval
	}
	lastDelay := attempt.NextPollAt.Sub(attempt.UpdatedAt)
	if lastDelay <= 0 {
		return kafudaPollInterval
	}
	if lastDelay >= kafudaMaxRetryDelay/2 {
		return kafudaMaxRetryDelay
	}
	return max(kafudaPollInterval, 2*lastDelay)
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
	if module.Store == nil || module.Gateway == nil || c.OrderStore == nil {
		return recoveryErr
	}
	return errors.Join(recoveryErr, c.pollKnownKafudaOrders(ctx))
}

func (c *Consumer) pollKnownKafudaOrders(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 55*time.Second)
	defer cancel()
	module := c.KafudaGPT
	attempts, err := module.Store.DueKnownOrderPolls(ctx, time.Now().UTC(), 10)
	if err != nil {
		return err
	}
	var failures []error
	var reviewAfter time.Duration
	if c.Config != nil {
		seconds := c.Config.KafudaGPT.PollReviewAfterSeconds
		if seconds > 0 && int64(seconds) <= int64((1<<63-1)/time.Second) {
			reviewAfter = time.Duration(seconds) * time.Second
		}
	}
	for _, attempt := range attempts {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		observedAt := time.Now().UTC()
		retryDelay := kafudaRetryDelay(attempt)
		result, err := module.Records.PollKnownOrder(ctx, module.Gateway, attempt.ID, observedAt, observedAt.Add(retryDelay), reviewAfter)
		if err != nil {
			if !errors.Is(err, kafudadomain.ErrConflict) && !errors.Is(err, kafudadomain.ErrNotFound) {
				failures = append(failures, err)
			}
			continue
		}
		completed, err := kafudaworkflow.ApplyResult(c.OrderStore, attempt.OrderID, attempt.ID, *result, observedAt, observedAt.Add(kafudaPollInterval))
		if completed {
			c.enqueueKafudaCompletionEmail(attempt.OrderID)
		}
		if err == nil || errors.Is(err, kafudadomain.ErrConflict) || errors.Is(err, kafudadomain.ErrNotFound) {
			continue
		}
		retryFrom := time.Now().UTC()
		if attempt.NextPollAt != nil {
			err = errors.Join(err, module.Records.RescheduleKnownOrderPoll(ctx, attempt.ID, *attempt.NextPollAt, retryFrom, retryFrom.Add(retryDelay)))
		}
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

func (c *Consumer) enqueueKafudaCompletionEmail(childID uint) {
	if c.orderStatusQueue == nil || !c.orderStatusQueue.Enabled() {
		return
	}
	child, err := c.OrderStore.GetByID(childID)
	if err != nil || child == nil || child.ParentID == nil {
		logger.Warnw("kafuda_completion_email_order_unavailable", "child_order_id", childID)
		return
	}
	var emailConfig config.EmailConfig
	if c.Config != nil {
		emailConfig = c.Config.Email
	}
	if _, err := orderapp.EnqueueStatusEmailTaskIfEligible(c.OrderStore, c.orderStatusQueue, c.SettingService, emailConfig, *child.ParentID, constants.OrderStatusCompleted); err != nil {
		logger.Warnw("kafuda_completion_email_enqueue_failed", "order_id", *child.ParentID)
	}
}
