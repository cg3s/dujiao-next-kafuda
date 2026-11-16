package consumer

import (
	"context"
	"time"

	"github.com/hibiken/asynq"
)

func (c *Consumer) handleKafudaRecoverChecks(ctx context.Context, _ *asynq.Task) error {
	if c == nil || c.Container == nil || c.KafudaGPT == nil || c.KafudaGPT.Records == nil {
		return nil
	}
	return c.KafudaGPT.Records.RecoverExpiredChecks(ctx, time.Now().UTC(), 100)
}
