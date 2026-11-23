package gormstore

import (
	"context"
	"errors"
	"strings"
	"time"

	quefacontract "github.com/dujiao-next/internal/modules/quefa/contract"
	quefadomain "github.com/dujiao-next/internal/modules/quefa/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Store struct{ db *gorm.DB }

var (
	_ quefacontract.Store              = (*Store)(nil)
	_ quefacontract.SettingStore       = (*Store)(nil)
	_ quefacontract.Transaction        = (*txStore)(nil)
	_ quefacontract.CheckoutRepository = (*Store)(nil)
)

func New(db *gorm.DB) *Store { return &Store{db: db} }

// TxStore wraps Store to satisfy both CheckoutRepository and Transaction inside an active DB transaction.
type TxStore struct{ *Store }

func UseTransaction(db *gorm.DB) *TxStore { return &TxStore{Store: New(db)} }

func (t *TxStore) GetCatalog(profile string) (*quefadomain.Catalog, error) {
	return first[quefadomain.Catalog](t.db.Where("profile = ?", profile))
}

func (t *TxStore) GetOrderByOrderID(orderID uint) (*quefadomain.Order, error) {
	return first[quefadomain.Order](t.db.Where("order_id = ?", orderID))
}

func (t *TxStore) GetOrderByParentOrderID(parentOrderID uint) (*quefadomain.Order, error) {
	return first[quefadomain.Order](t.db.Where("parent_order_id = ?", parentOrderID))
}

func first[T any](q *gorm.DB) (*T, error) {
	var out T
	err := q.First(&out).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *Store) WithinTransaction(ctx context.Context, fn func(quefacontract.Transaction) error) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return fn(New(tx).asTx())
	})
}

func (s *Store) GetSetting(ctx context.Context, profile string) (*quefadomain.Setting, error) {
	return first[quefadomain.Setting](s.db.WithContext(ctx).Where("profile = ?", profile))
}

func (s *Store) SaveSetting(ctx context.Context, setting *quefadomain.Setting) error {
	if setting == nil || strings.TrimSpace(setting.Profile) == "" {
		return errors.New("quefa setting profile required")
	}
	if setting.AllowedPayDomains == nil {
		setting.AllowedPayDomains = []string{}
	}
	return s.db.WithContext(ctx).Save(setting).Error
}

func (s *Store) GetCatalog(ctx context.Context, profile string) (*quefadomain.Catalog, error) {
	return first[quefadomain.Catalog](s.db.WithContext(ctx).Where("profile = ?", profile))
}

// Transaction-scoped GetCatalog satisfies contract.Transaction.
type txStore struct{ *Store }

func (s *Store) asTx() *txStore { return &txStore{Store: s} }

func (t *txStore) GetCatalog(profile string) (*quefadomain.Catalog, error) {
	return first[quefadomain.Catalog](t.db.Where("profile = ?", profile))
}

func (s *Store) GetCatalogForUpdate(profile string) (*quefadomain.Catalog, error) {
	return first[quefadomain.Catalog](s.db.Clauses(clause.Locking{Strength: "UPDATE"}).Where("profile = ?", profile))
}

func (s *Store) SaveCatalog(catalog *quefadomain.Catalog) error {
	if catalog == nil || strings.TrimSpace(catalog.Profile) == "" {
		return errors.New("quefa catalog profile required")
	}
	return s.db.Save(catalog).Error
}

func (s *Store) CreateCheckout(input quefacontract.CheckoutInput) error {
	stage := quefadomain.StageWaitingPayment
	if input.CollectionMode == quefadomain.CollectionModeAgentCollect {
		stage = quefadomain.StageWaitingProcurement
	}
	record := &quefadomain.Order{
		OrderID:            input.OrderID,
		ParentOrderID:      input.ParentOrderID,
		ProductID:          input.ProductID,
		SKUID:              input.SKUID,
		ProductCode:        input.ProductCode,
		CollectionMode:     input.CollectionMode,
		DeliveryMode:       input.DeliveryMode,
		CostFloorAmount:    input.CostFloorAmount,
		SettlementCurrency: input.SettlementCurrency,
		MerchantOrderNo:    input.MerchantOrderNo,
		Stage:              stage,
		Version:            1,
		CreatedAt:          input.CreatedAt,
		UpdatedAt:          input.CreatedAt,
	}
	return s.CreateOrder(record)
}

func (s *Store) GetOrderByOrderID(ctx context.Context, orderID uint) (*quefadomain.Order, error) {
	return first[quefadomain.Order](s.db.WithContext(ctx).Where("order_id = ?", orderID))
}

func (t *txStore) GetOrderByOrderID(orderID uint) (*quefadomain.Order, error) {
	return first[quefadomain.Order](t.db.Where("order_id = ?", orderID))
}

func (s *Store) GetOrderByOrderIDForUpdate(orderID uint) (*quefadomain.Order, error) {
	return first[quefadomain.Order](s.db.Clauses(clause.Locking{Strength: "UPDATE"}).Where("order_id = ?", orderID))
}

func (s *Store) GetOrderByParentOrderID(ctx context.Context, parentOrderID uint) (*quefadomain.Order, error) {
	return first[quefadomain.Order](s.db.WithContext(ctx).Where("parent_order_id = ?", parentOrderID))
}

func (t *txStore) GetOrderByParentOrderID(parentOrderID uint) (*quefadomain.Order, error) {
	return first[quefadomain.Order](t.db.Where("parent_order_id = ?", parentOrderID))
}

func (s *Store) GetOrderByParentOrderIDForUpdate(parentOrderID uint) (*quefadomain.Order, error) {
	return first[quefadomain.Order](s.db.Clauses(clause.Locking{Strength: "UPDATE"}).Where("parent_order_id = ?", parentOrderID))
}

func (s *Store) GetOrderByUpstreamID(ctx context.Context, upstreamOrderID string) (*quefadomain.Order, error) {
	return first[quefadomain.Order](s.db.WithContext(ctx).Where("upstream_order_id = ?", strings.TrimSpace(upstreamOrderID)))
}

func (s *Store) GetOrderByUpstreamIDForUpdate(upstreamOrderID string) (*quefadomain.Order, error) {
	return first[quefadomain.Order](s.db.Clauses(clause.Locking{Strength: "UPDATE"}).Where("upstream_order_id = ?", strings.TrimSpace(upstreamOrderID)))
}

func (s *Store) GetOrderByMerchantNo(ctx context.Context, merchantOrderNo string) (*quefadomain.Order, error) {
	return first[quefadomain.Order](s.db.WithContext(ctx).Where("merchant_order_no = ?", strings.TrimSpace(merchantOrderNo)))
}

func (s *Store) GetOrderByMerchantNoForUpdate(merchantOrderNo string) (*quefadomain.Order, error) {
	return first[quefadomain.Order](s.db.Clauses(clause.Locking{Strength: "UPDATE"}).Where("merchant_order_no = ?", strings.TrimSpace(merchantOrderNo)))
}

func (s *Store) CreateOrder(order *quefadomain.Order) error {
	if err := order.ValidateSnapshot(); err != nil {
		return err
	}
	if order.Version <= 0 {
		order.Version = 1
	}
	return s.db.Create(order).Error
}

// SaveOrder persists mutable lifecycle fields using optimistic locking on version,
// while preserving immutable checkout snapshot fields.
func (s *Store) SaveOrder(order *quefadomain.Order) error {
	if order == nil || order.OrderID == 0 {
		return quefadomain.ErrInvalidOrderSnapshot
	}
	updates := map[string]any{
		"sale_amount":               order.SaleAmount,
		"idempotency_key":           order.IdempotencyKey,
		"create_request_snapshot":   order.CreateRequestSnapshot,
		"create_dispatched":         order.CreateDispatched,
		"upstream_order_id":         order.UpstreamOrderID,
		"latest_redemption_id":      order.LatestRedemptionID,
		"attempt_no":                order.AttemptNo,
		"progress_version":          order.ProgressVersion,
		"progress_stage":            order.ProgressStage,
		"progress_message":          order.ProgressMessage,
		"progress_updated_at":       order.ProgressUpdatedAt,
		"stage":                     order.Stage,
		"upstream_status":           order.UpstreamStatus,
		"result_code":               order.ResultCode,
		"retry_allowed":             order.RetryAllowed,
		"next_action":               order.NextAction,
		"recovery_action":           order.RecoveryAction,
		"fulfillment_url":           order.FulfillmentURL,
		"pay_url":                   order.PayURL,
		"qr_code":                   order.QRCode,
		"pay_expires_at":            order.PayExpiresAt,
		"refund_ref":                order.RefundRef,
		"procurement_refund_ref":    order.ProcurementRefundRef,
		"exception_code":            order.ExceptionCode,
		"review_remark":             order.ReviewRemark,
		"next_poll_at":              order.NextPollAt,
		"poll_stopped":              order.PollStopped,
		"completion_email_pending":  order.CompletionEmailPending,
		"completion_email_retry_at": order.CompletionEmailRetryAt,
		"updated_at":                order.UpdatedAt,
		"completed_at":              order.CompletedAt,
		"version":                   order.Version + 1,
	}
	res := s.db.Model(&quefadomain.Order{}).
		Where("order_id = ? AND version = ?", order.OrderID, order.Version).
		Updates(updates)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return quefadomain.ErrConflict
	}
	order.Version++
	return nil
}

func (s *Store) InsertWebhookEventIfAbsent(event *quefadomain.WebhookEvent) (bool, error) {
	if event == nil || strings.TrimSpace(event.EventID) == "" {
		return false, errors.New("webhook event_id required")
	}
	res := s.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "event_id"}},
		DoNothing: true,
	}).Create(event)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

func (s *Store) UpdateWebhookEventStatus(eventID, status, summary string, processedAt *time.Time) error {
	return s.db.Model(&quefadomain.WebhookEvent{}).
		Where("event_id = ?", strings.TrimSpace(eventID)).
		Updates(map[string]any{
			"status":          status,
			"payload_summary": summary,
			"processed_at":    processedAt,
		}).Error
}

func (s *Store) ListDuePollOrderIDs(ctx context.Context, now time.Time, limit int) ([]uint, error) {
	if limit <= 0 {
		limit = 100
	}
	var ids []uint
	err := s.db.WithContext(ctx).Model(&quefadomain.Order{}).
		Where("poll_stopped = ?", false).
		Where("stage NOT IN ?", []quefadomain.Stage{quefadomain.StageSucceeded, quefadomain.StageClosed, quefadomain.StageRefunded}).
		Where("next_poll_at IS NOT NULL AND next_poll_at <= ?", now).
		Order("next_poll_at ASC, order_id ASC").
		Limit(limit).
		Pluck("order_id", &ids).Error
	return ids, err
}

func (s *Store) ListPendingCompletionEmails(ctx context.Context, now time.Time, limit int) ([]uint, error) {
	if limit <= 0 {
		limit = 100
	}
	var ids []uint
	err := s.db.WithContext(ctx).Model(&quefadomain.Order{}).
		Where("completion_email_pending = ? AND stage = ?", true, quefadomain.StageSucceeded).
		Where("(completion_email_retry_at IS NULL OR completion_email_retry_at <= ?)", now).
		Order("order_id ASC").
		Limit(limit).
		Pluck("order_id", &ids).Error
	return ids, err
}

func (s *Store) ClaimCompletionEmail(ctx context.Context, orderID uint, now, retryAt time.Time) (bool, error) {
	res := s.db.WithContext(ctx).Model(&quefadomain.Order{}).
		Where("order_id = ? AND completion_email_pending = ? AND stage = ?", orderID, true, quefadomain.StageSucceeded).
		Where("(completion_email_retry_at IS NULL OR completion_email_retry_at <= ?)", now).
		UpdateColumn("completion_email_retry_at", retryAt)
	return res.RowsAffected == 1, res.Error
}

func (s *Store) ClearCompletionEmail(ctx context.Context, orderID uint) error {
	return s.db.WithContext(ctx).Model(&quefadomain.Order{}).
		Where("order_id = ? AND completion_email_pending = ? AND stage = ?", orderID, true, quefadomain.StageSucceeded).
		UpdateColumns(map[string]any{"completion_email_pending": false, "completion_email_retry_at": nil}).Error
}
