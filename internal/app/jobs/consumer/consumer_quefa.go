package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/dujiao-next/internal/logger"
	quefaapp "github.com/dujiao-next/internal/modules/quefa/application"
	quefacontract "github.com/dujiao-next/internal/modules/quefa/contract"
	quefadomain "github.com/dujiao-next/internal/modules/quefa/domain"
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

func (c *Consumer) handleQuefaRefreshCatalog(ctx context.Context, _ *asynq.Task) error {
	if c == nil || c.Container == nil || c.Quefa == nil || c.Quefa.Catalog == nil {
		return nil
	}
	module := c.Quefa
	module.RefreshConfig(ctx)
	if !module.HasGateway() {
		return nil
	}
	var fallbackInterval time.Duration
	var fallbackFixed bool
	if c.Config != nil {
		fallbackInterval = c.Config.QuefaCatalogRefreshInterval()
		fallbackFixed = c.Config.QuefaCatalogFixedRefresh()
	}
	interval, fixed := module.CatalogRefreshSchedule(fallbackInterval, fallbackFixed)
	if interval == 0 {
		return nil
	}
	refreshCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	mode := "before_expiry"
	var (
		catalog *quefaapp.CatalogView
		err     error
	)
	if fixed {
		mode = "fixed_interval"
		catalog, err = module.Catalog.RefreshOnInterval(refreshCtx, interval)
	} else {
		catalog, err = module.Catalog.RefreshBeforeExpiry(refreshCtx, interval)
	}
	if err != nil {
		reason, status := classifyQuefaCatalogError(err)
		// Never log upstream bodies, secrets, or URLs.
		logger.Warnw("quefa_scheduled_catalog_refresh_failed",
			"mode", mode,
			"reason", reason,
			"upstream_http_status", status,
		)
		return nil
	}
	if catalog != nil {
		logger.Infow("quefa_scheduled_catalog_refresh_ok",
			"mode", mode,
			"interval", interval.String(),
			"products_count", len(catalog.Products),
			"revision", catalog.Revision,
			"synced_at", catalog.SyncedAt,
		)
	}
	return nil
}

func classifyQuefaCatalogError(err error) (string, int) {
	reason := "catalog_failed"
	status := 0
	var upstream *quefacontract.APIError
	var parseErr *quefaapp.CatalogParseError
	switch {
	case errors.As(err, &upstream) && upstream != nil:
		status = upstream.StatusCode
		if status < 100 || status > 599 {
			status = 0
		}
		if upstream.Category != "" {
			reason = upstream.Category
		} else if status == 0 {
			reason = "network_error"
		}
	case errors.As(err, &parseErr) || errors.Is(err, quefadomain.ErrCatalogInvalid) || errors.Is(err, quefadomain.ErrCatalogEmpty):
		reason = "invalid_response"
	case errors.Is(err, quefaapp.ErrGatewayNotConfigured):
		reason = "gateway_not_configured"
	}
	return reason, status
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
