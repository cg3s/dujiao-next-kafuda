package consumer

import (
	"context"
	"errors"
	"time"

	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
	kafudaworkflow "github.com/dujiao-next/internal/workflows/kafudagpt/application"
	"github.com/hibiken/asynq"
)

const kafudaPollInterval = time.Minute

func (c *Consumer) handleKafudaRecoverChecks(ctx context.Context, _ *asynq.Task) error {
	if c == nil || c.Container == nil || c.KafudaGPT == nil || c.KafudaGPT.Records == nil {
		return nil
	}
	module := c.KafudaGPT
	recoveryErr := module.Records.RecoverExpiredChecks(ctx, time.Now().UTC(), 100)
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
	for _, attempt := range attempts {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		observedAt := time.Now().UTC()
		result, err := module.Records.PollKnownOrder(ctx, module.Gateway, attempt.ID, observedAt, observedAt.Add(kafudaPollInterval))
		if err != nil {
			if !errors.Is(err, kafudadomain.ErrConflict) && !errors.Is(err, kafudadomain.ErrNotFound) {
				failures = append(failures, err)
			}
			continue
		}
		_, err = kafudaworkflow.ApplyResult(c.OrderStore, attempt.OrderID, attempt.ID, *result, observedAt, time.Now().UTC().Add(kafudaPollInterval))
		if err == nil || errors.Is(err, kafudadomain.ErrConflict) || errors.Is(err, kafudadomain.ErrNotFound) {
			continue
		}
		retryFrom := time.Now().UTC()
		if attempt.NextPollAt != nil {
			err = errors.Join(err, module.Records.RescheduleKnownOrderPoll(ctx, attempt.ID, *attempt.NextPollAt, retryFrom, retryFrom.Add(kafudaPollInterval)))
		}
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}
