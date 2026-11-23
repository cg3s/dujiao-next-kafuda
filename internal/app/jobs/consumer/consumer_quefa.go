package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/dujiao-next/internal/logger"
	"github.com/dujiao-next/internal/queue"
	quefaworkflow "github.com/dujiao-next/internal/workflows/quefa/application"
	"github.com/hibiken/asynq"
)

const quefaPollBatchSize = 50

func (c *Consumer) handleQuefaSyncOrder(ctx context.Context, task *asynq.Task) error {
	if c == nil || c.Container == nil || c.Quefa == nil || c.Quefa.Orders == nil {
		return nil
	}
	var payload queue.QuefaSyncOrderPayload
	if err := json.Unmarshal(task.Payload(), &payload); err != nil || payload.ChildOrderID == 0 {
		return nil
	}
	if err := c.Quefa.Orders.SyncOrder(ctx, payload.ChildOrderID); err != nil {
		logger.Warnw("quefa_sync_order_failed", "child_order_id", payload.ChildOrderID, "error", err)
		return nil
	}
	return nil
}

func (c *Consumer) handleQuefaPollOrders(ctx context.Context, _ *asynq.Task) error {
	if c == nil || c.Container == nil || c.Quefa == nil || c.Quefa.Store == nil || c.Quefa.Orders == nil {
		return nil
	}
	now := time.Now().UTC()
	var aggErr error

	// Dispatch pending completion emails after commit (idempotent claim).
	if c.orderStatusQueue != nil && c.orderStatusQueue.Enabled() && c.OrderStore != nil {
		emailIDs, err := c.Quefa.Store.ListPendingCompletionEmails(ctx, now, quefaPollBatchSize)
		aggErr = errors.Join(aggErr, err)
		for _, childID := range emailIDs {
			quefaworkflow.EnqueueCompletionEmail(ctx, c.OrderStore, c.Quefa.Store, c.orderStatusQueue, c.SettingService, c.Config, childID)
		}
	}

	dueIDs, err := c.Quefa.Store.ListDuePollOrderIDs(ctx, now, quefaPollBatchSize)
	if err != nil {
		return errors.Join(aggErr, err)
	}
	for _, childID := range dueIDs {
		if syncErr := c.Quefa.Orders.SyncOrder(ctx, childID); syncErr != nil {
			logger.Warnw("quefa_poll_order_sync_failed", "child_order_id", childID, "error", syncErr)
		}
	}
	return aggErr
}
