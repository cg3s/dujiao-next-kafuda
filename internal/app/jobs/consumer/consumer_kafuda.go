package consumer

import (
	"context"
	"errors"
	"time"

	"github.com/dujiao-next/internal/logger"
	kafudaapp "github.com/dujiao-next/internal/modules/kafudagpt/application"
	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
	kafudaworkflow "github.com/dujiao-next/internal/workflows/kafudagpt/application"
	"github.com/hibiken/asynq"
)

const (
	kafudaPollBatchSize       = 10
	kafudaCardChargeBatchSize = 2
)

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

func (c *Consumer) handleKafudaRefreshCatalog(ctx context.Context, _ *asynq.Task) error {
	if c == nil || c.Container == nil || c.KafudaGPT == nil || c.KafudaGPT.Gateway == nil || c.KafudaGPT.Catalog == nil {
		return nil
	}
	interval := c.Config.KafudaCatalogRefreshInterval()
	if interval == 0 {
		return nil
	}
	refreshCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	mode := "before_expiry"
	var (
		catalog *kafudadomain.Catalog
		err     error
	)
	if c.Config.KafudaCatalogFixedRefresh() {
		mode = "fixed_interval"
		catalog, err = c.KafudaGPT.Catalog.RefreshOnInterval(refreshCtx, interval)
	} else {
		catalog, err = c.KafudaGPT.Catalog.RefreshBeforeExpiryResult(refreshCtx, interval)
	}
	if err != nil {
		reason, status := classifyKafudaCatalogError(err)
		// Never log upstream bodies, error codes, URLs, or raw database errors.
		logger.Warnw("kafuda_scheduled_catalog_refresh_failed",
			"mode", mode,
			"reason", reason,
			"upstream_http_status", status,
		)
		return nil
	}
	if catalog != nil {
		logger.Infow("kafuda_scheduled_catalog_refresh_ok",
			"mode", mode,
			"interval", interval.String(),
			"enabled", catalog.Enabled,
			"options_count", len(catalog.Options),
			"cards_count", len(catalog.Cards),
			"synced_at", catalog.SyncedAt,
			"expires_at", catalog.ExpiresAt,
		)
	}
	return nil
}

func classifyKafudaCatalogError(err error) (string, int) {
	reason := "catalog_failed"
	status := 0
	var upstream *kafudacontract.APIError
	switch {
	case errors.As(err, &upstream):
		status = upstream.HTTPStatus
		if status < 100 || status > 599 {
			status = 0
		}
		switch {
		case status == 401:
			reason = "auth_failed"
		case status == 403:
			reason = "forbidden"
		case status == 404:
			reason = "endpoint_missing"
		case status == 429:
			reason = "rate_limited"
		case status >= 300 && status < 400:
			reason = "redirect"
		case status >= 500:
			reason = "upstream_unavailable"
		case upstream.Code == "invalid_response":
			reason = "invalid_response"
		case status == 0:
			reason = "network_failed"
		}
	case errors.Is(err, kafudadomain.ErrInvalidCatalog):
		reason = "invalid_response"
	case errors.Is(err, kafudacontract.ErrCatalogReadFailed):
		reason = "cache_read_failed"
	case errors.Is(err, kafudacontract.ErrCatalogWriteFailed):
		reason = "cache_write_failed"
	}
	return reason, status
}

func (c *Consumer) pollKnownKafudaOrders(ctx context.Context) error {
	lookupCtx, cancel := context.WithTimeout(ctx, 55*time.Second)
	defer cancel()
	module := c.KafudaGPT
	attempts, err := module.Store.DueKnownOrderPolls(lookupCtx, time.Now().UTC(), kafudaPollBatchSize)
	if err != nil {
		return err
	}
	var failures []error
	interval := c.Config.KafudaPollInterval()
	reviewAfter := kafudaapp.DefaultPollReviewAfter
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
		result, err := module.Records.PollKnownOrder(ctx, lookupCtx, module.Gateway, attempt.ID, startedAt, interval, reviewAfter)
		if err != nil {
			if !errors.Is(err, kafudadomain.ErrConflict) && !errors.Is(err, kafudadomain.ErrNotFound) {
				failures = append(failures, err)
			}
			if errors.Is(err, kafudaapp.ErrKnownOrderRateLimited) {
				return errors.Join(failures...)
			}
			continue
		}
		receivedAt := startedAt.Add(time.Since(lookupStarted))
		completed, err := kafudaworkflow.ApplyPolledResult(c.OrderStore, attempt.OrderID, attempt.ID, *result, startedAt, receivedAt, interval, reviewAfter)
		if completed {
			kafudaworkflow.EnqueueCompletionEmail(ctx, c.OrderStore, module.Store, c.orderStatusQueue, c.SettingService, c.Config, attempt.OrderID)
		}
		if err == nil || errors.Is(err, kafudadomain.ErrConflict) || errors.Is(err, kafudadomain.ErrNotFound) {
			continue
		}
		retryFrom := startedAt.Add(time.Since(lookupStarted))
		err = errors.Join(err, module.Records.RescheduleKnownOrderResult(ctx, *result, startedAt, retryFrom, retryFrom.Add(result.RetryDelay), reviewAfter))
		failures = append(failures, err)
	}
	if limit := min(kafudaCardChargeBatchSize, kafudaPollBatchSize-len(attempts)); limit > 0 {
		failures = append(failures, c.pollKafudaCardCharges(ctx, lookupCtx, limit))
	}
	return errors.Join(failures...)
}

func (c *Consumer) pollKafudaCardCharges(ctx, lookupCtx context.Context, limit int) error {
	module := c.KafudaGPT
	attempts, err := module.Records.DueCardChargePolls(lookupCtx, time.Now().UTC(), limit)
	if err != nil {
		return err
	}
	var failures []error
	for _, attempt := range attempts {
		if err := lookupCtx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		err := module.Records.PollCardCharge(ctx, lookupCtx, module.Gateway, attempt, time.Now().UTC())
		if err != nil && !errors.Is(err, kafudadomain.ErrConflict) && !errors.Is(err, kafudadomain.ErrNotFound) {
			failures = append(failures, err)
		}
		if errors.Is(err, kafudaapp.ErrKnownOrderRateLimited) {
			break
		}
	}
	return errors.Join(failures...)
}
