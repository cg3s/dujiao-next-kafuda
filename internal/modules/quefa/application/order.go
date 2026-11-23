package application

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	quefacontract "github.com/dujiao-next/internal/modules/quefa/contract"
	quefadomain "github.com/dujiao-next/internal/modules/quefa/domain"
	"github.com/dujiao-next/internal/shared/money"
	"github.com/shopspring/decimal"
)

// LifecycleHooks connects Quefa upstream transitions to shop order/payment/fulfillment/refund workflows.
type LifecycleHooks struct {
	// OnPlatformOrderPaid is invoked when Quefa platform_collect confirms paid with verified amount & currency.
	OnPlatformOrderPaid func(ctx context.Context, record *quefadomain.Order, snap *quefacontract.OrderSnapshot) error
	// OnOrderSucceeded atomically records fulfillment delivery and transitions child/parent order to completed.
	OnOrderSucceeded func(ctx context.Context, record *quefadomain.Order) error
	// OnPlatformOrderRefunded records a verified platform_collect buyer refund (refund.succeeded).
	OnPlatformOrderRefunded func(ctx context.Context, record *quefadomain.Order, refundRef string) error
	// OnOrderClosed closes an unpaid platform_collect order after upstream confirmed expired/closed.
	OnOrderClosed func(ctx context.Context, record *quefadomain.Order) error
	// EnqueueSync schedules asynchronous order polling/sync via queue worker.
	EnqueueSync func(ctx context.Context, childOrderID uint, delay time.Duration) error
}

// PaymentCreateOutput returns validated white-label payment credentials for platform_collect.
type PaymentCreateOutput struct {
	UpstreamOrderID string
	PayURL          string
	QRCode          string
	ExpiresAt       *time.Time
}

// CancelEvaluation describes whether a local Quefa order may be safely canceled/expired.
type CancelEvaluation struct {
	CanClose   bool
	Reason     string
	UpstreamID string
}

// OrderService coordinates Quefa order creation, webhook dedup, polling, hosted entry, cancel check, and refund tracking.
type OrderService struct {
	mu        sync.RWMutex
	store     quefacontract.Store
	configSvc *ConfigService
	hooks     LifecycleHooks
	now       func() time.Time
}

func NewOrderService(store quefacontract.Store, configSvc *ConfigService) *OrderService {
	return &OrderService{
		store:     store,
		configSvc: configSvc,
		now:       time.Now,
	}
}

func (s *OrderService) SetHooks(hooks LifecycleHooks) {
	s.mu.Lock()
	s.hooks = hooks
	s.mu.Unlock()
}

func (s *OrderService) getHooks() LifecycleHooks {
	s.mu.RLock()
	h := s.hooks
	s.mu.RUnlock()
	return h
}

func (s *OrderService) currentConfig() ConfigSnapshot {
	if s.configSvc == nil {
		return ConfigSnapshot{}
	}
	return s.configSvc.Snapshot()
}

// GetBuyerStatusByParentOrder returns sanitized buyer-facing status without exposing fulfillment_url or cost.
func (s *OrderService) GetBuyerStatusByParentOrder(ctx context.Context, parentOrderID uint) (*quefacontract.BuyerStatus, error) {
	record, err := s.store.GetOrderByParentOrderID(ctx, parentOrderID)
	if err != nil || record == nil {
		return nil, err
	}
	return &quefacontract.BuyerStatus{
		Stage:              string(record.Stage),
		ProgressStage:      record.ProgressStage,
		ProgressMessage:    record.ProgressMessage,
		ProgressUpdatedAt:  record.ProgressUpdatedAt,
		CanOpenFulfillment: record.Stage.CanOpenFulfillmentEntry() && record.RecoveryAction != "refund",
		UpdatedAt:          record.UpdatedAt,
		CompletedAt:        record.CompletedAt,
	}, nil
}

// GetAdminStatusByParentOrder returns operator-facing Quefa reconciliation status.
func (s *OrderService) GetAdminStatusByParentOrder(ctx context.Context, parentOrderID uint) (*quefacontract.AdminOrderStatus, error) {
	record, err := s.store.GetOrderByParentOrderID(ctx, parentOrderID)
	if err != nil || record == nil {
		return nil, err
	}
	return toAdminStatus(record), nil
}

// GetOrderByParentOrder returns the persisted Quefa order record for internal cross-module workflows.
func (s *OrderService) GetOrderByParentOrder(ctx context.Context, parentOrderID uint) (*quefadomain.Order, error) {
	return s.store.GetOrderByParentOrderID(ctx, parentOrderID)
}

func toAdminStatus(r *quefadomain.Order) *quefacontract.AdminOrderStatus {
	if r == nil {
		return nil
	}
	var upID string
	if r.UpstreamOrderID != nil {
		upID = *r.UpstreamOrderID
	}
	return &quefacontract.AdminOrderStatus{
		OrderID:              r.OrderID,
		ParentOrderID:        r.ParentOrderID,
		ProductCode:          r.ProductCode,
		CollectionMode:       r.CollectionMode,
		DeliveryMode:         r.DeliveryMode,
		CostFloorAmount:      r.CostFloorAmount.Decimal.StringFixed(2),
		SaleAmount:           r.SaleAmount.Decimal.StringFixed(2),
		SettlementCurrency:   r.SettlementCurrency,
		MerchantOrderNo:      r.MerchantOrderNo,
		UpstreamOrderID:      upID,
		LatestRedemptionID:   r.LatestRedemptionID,
		AttemptNo:            r.AttemptNo,
		ProgressVersion:      r.ProgressVersion,
		ProgressStage:        r.ProgressStage,
		ProgressMessage:      r.ProgressMessage,
		ProgressUpdatedAt:    r.ProgressUpdatedAt,
		Stage:                string(r.Stage),
		UpstreamStatus:       r.UpstreamStatus,
		ResultCode:           r.ResultCode,
		RetryAllowed:         r.RetryAllowed,
		NextAction:           r.NextAction,
		RecoveryAction:       r.RecoveryAction,
		RefundRef:            r.RefundRef,
		ProcurementRefundRef: r.ProcurementRefundRef,
		ExceptionCode:        r.ExceptionCode,
		ReviewRemark:         r.ReviewRemark,
		PollStopped:          r.PollStopped,
		NextPollAt:           r.NextPollAt,
		UpdatedAt:            r.UpdatedAt,
		CompletedAt:          r.CompletedAt,
	}
}

// CreatePlatformCollectPayment creates or reuses a Quefa platform_collect order for a pending shop order.
// It persists merchant_order_no, Idempotency-Key and request snapshot BEFORE outbound HTTP,
// executes HTTP outside DB transactions, and strictly validates payment URL/QR domains.
func (s *OrderService) CreatePlatformCollectPayment(ctx context.Context, parentOrderID uint, onlineAmount decimal.Decimal, returnURL string) (*PaymentCreateOutput, error) {
	if s.configSvc != nil {
		s.configSvc.RefreshIfStale(ctx)
	}
	cfgSnap := s.currentConfig()
	if cfgSnap.Gateway == nil || cfgSnap.GatewayConfigInvalid || cfgSnap.LoadError {
		return nil, ErrGatewayNotConfigured
	}

	var childOrderID uint
	var req quefacontract.CreateOrderRequest
	var idemKey string
	var existingOut *PaymentCreateOutput
	now := s.now().UTC()

	err := s.store.WithinTransaction(ctx, func(tx quefacontract.Transaction) error {
		record, err := tx.GetOrderByParentOrderIDForUpdate(parentOrderID)
		if err != nil {
			return err
		}
		if record == nil {
			return quefadomain.ErrOrderNotFound
		}
		if record.CollectionMode != quefadomain.CollectionModePlatformCollect {
			return quefadomain.ErrInvalidOrderSnapshot
		}
		childOrderID = record.OrderID

		// Reuse already created valid white-label payment entry for the same order.
		if record.UpstreamOrderID != nil && (record.PayURL != "" || record.QRCode != "") {
			if err := validatePaymentArtifacts(record.PayURL, record.QRCode, cfgSnap.Config.AllowedPayDomains); err != nil {
				return err
			}
			existingOut = &PaymentCreateOutput{
				UpstreamOrderID: *record.UpstreamOrderID,
				PayURL:          record.PayURL,
				QRCode:          record.QRCode,
				ExpiresAt:       record.PayExpiresAt,
			}
			return nil
		}

		// Enforce cost floor on actual online payment amount.
		frozenAmount := onlineAmount.Round(2)
		if frozenAmount.LessThan(record.CostFloorAmount.Decimal.Round(2)) || frozenAmount.LessThanOrEqual(decimal.Zero) {
			return quefacontract.ErrBelowCostFloor
		}

		if record.IdempotencyKey != "" && record.CreateRequestSnapshot != "" {
			if err := json.Unmarshal([]byte(record.CreateRequestSnapshot), &req); err != nil {
				return err
			}
			idemKey = record.IdempotencyKey
		} else {
			req = quefacontract.CreateOrderRequest{
				MerchantOrderNo: record.MerchantOrderNo,
				ProductCode:     record.ProductCode,
				Quantity:        1,
				SaleAmount:      frozenAmount.StringFixed(2),
				CollectionMode:  record.CollectionMode,
				DeliveryMode:    record.DeliveryMode,
			}
			_ = returnURL
			rawReq, err := json.Marshal(req)
			if err != nil {
				return err
			}
			idemKey = "quefa-order-" + record.MerchantOrderNo
			record.SaleAmount = money.FromDecimal(frozenAmount)
			record.IdempotencyKey = idemKey
			record.CreateRequestSnapshot = string(rawReq)
			record.CreateDispatched = true
			record.UpdatedAt = now
			if err := tx.SaveOrder(record); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if existingOut != nil {
		return existingOut, nil
	}

	// Outbound HTTP strictly outside transaction.
	snap, createErr := cfgSnap.Gateway.CreateOrder(ctx, req, idemKey)
	if createErr != nil {
		// On timeout / 409 idempotency_in_progress / retryable error, look up original order by merchant_order_no first.
		if quefacontract.IsAPIErrorCategory(createErr, "network_error") ||
			quefacontract.IsAPIErrorCategory(createErr, "idempotency_in_progress") ||
			quefacontract.IsAPIErrorCategory(createErr, "upstream_error") {
			if recovered, lookupErr := cfgSnap.Gateway.LookupOrderByMerchantNo(ctx, req.MerchantOrderNo); lookupErr == nil && recovered != nil {
				snap = recovered
				createErr = nil
			}
		}
	}
	if createErr != nil {
		// Schedule async recovery poll because CreateDispatched is already persisted.
		s.schedulePollAfter(ctx, childOrderID, cfgSnap.Config.PollInterval())
		return nil, createErr
	}

	// Prefer official white-label direct payment code (POST /v1/orders/{id}/payment-code) when the
	// order's default qr_payload is absent or points to a non-whitelisted / tibo.ink domain.
	if validatePaymentArtifacts(snap.PayURL, snap.QRCode, cfgSnap.Config.AllowedPayDomains) != nil && strings.TrimSpace(snap.OrderID) != "" {
		if payCode, pcErr := cfgSnap.Gateway.CreateOrderPaymentCode(ctx, snap.OrderID, "quefa-paycode-"+req.MerchantOrderNo); pcErr == nil && payCode != nil {
			if strings.TrimSpace(payCode.PaymentCode) != "" {
				snap.QRCode = strings.TrimSpace(payCode.PaymentCode)
				snap.PayURL = ""
			} else if strings.TrimSpace(payCode.QRImageDataURL) != "" {
				snap.QRCode = strings.TrimSpace(payCode.QRImageDataURL)
				snap.PayURL = ""
			}
			if payCode.ExpiresAt != nil {
				snap.PayExpiresAt = payCode.ExpiresAt
			}
		}
	}

	// White-label payment domain validation BEFORE persisting or returning anything to buyer.
	if err := validatePaymentArtifacts(snap.PayURL, snap.QRCode, cfgSnap.Config.AllowedPayDomains); err != nil {
		_ = s.markException(ctx, childOrderID, "payment_domain_blocked", true)
		return nil, err
	}

	if err := s.applyOrderSnapshot(ctx, childOrderID, snap, cfgSnap.Config); err != nil {
		return nil, err
	}
	return &PaymentCreateOutput{
		UpstreamOrderID: snap.OrderID,
		PayURL:          snap.PayURL,
		QRCode:          snap.QRCode,
		ExpiresAt:       snap.PayExpiresAt,
	}, nil
}

// CreateAgentCollectProcurement executes Quefa order creation after shop payment succeeds under agent_collect.
func (s *OrderService) CreateAgentCollectProcurement(ctx context.Context, childOrderID uint) error {
	if s.configSvc != nil {
		s.configSvc.RefreshIfStale(ctx)
	}
	cfgSnap := s.currentConfig()
	if cfgSnap.Gateway == nil || cfgSnap.GatewayConfigInvalid || cfgSnap.LoadError {
		s.schedulePollAfter(ctx, childOrderID, cfgSnap.Config.PollInterval())
		return ErrGatewayNotConfigured
	}

	var req quefacontract.CreateOrderRequest
	var idemKey string
	var alreadyCreated bool
	now := s.now().UTC()

	err := s.store.WithinTransaction(ctx, func(tx quefacontract.Transaction) error {
		record, err := tx.GetOrderByOrderIDForUpdate(childOrderID)
		if err != nil {
			return err
		}
		if record == nil {
			return quefadomain.ErrOrderNotFound
		}
		if record.CollectionMode != quefadomain.CollectionModeAgentCollect {
			return nil
		}
		if record.Stage.IsTerminal() {
			alreadyCreated = true
			return nil
		}
		if record.UpstreamOrderID != nil && *record.UpstreamOrderID != "" {
			alreadyCreated = true
			return nil
		}
		if record.IdempotencyKey != "" && record.CreateRequestSnapshot != "" {
			if err := json.Unmarshal([]byte(record.CreateRequestSnapshot), &req); err != nil {
				return err
			}
			idemKey = record.IdempotencyKey
			return nil
		}

		req = quefacontract.CreateOrderRequest{
			MerchantOrderNo: record.MerchantOrderNo,
			ProductCode:     record.ProductCode,
			Quantity:        1,
			SaleAmount:      record.CostFloorAmount.Decimal.Round(2).StringFixed(2),
			CollectionMode:  record.CollectionMode,
			DeliveryMode:    record.DeliveryMode,
		}
		rawReq, err := json.Marshal(req)
		if err != nil {
			return err
		}
		idemKey = "quefa-proc-" + record.MerchantOrderNo
		record.SaleAmount = record.CostFloorAmount
		record.IdempotencyKey = idemKey
		record.CreateRequestSnapshot = string(rawReq)
		record.CreateDispatched = true
		record.UpdatedAt = now
		return tx.SaveOrder(record)
	})
	if err != nil {
		return err
	}
	if alreadyCreated {
		return s.SyncOrder(ctx, childOrderID)
	}

	snap, createErr := cfgSnap.Gateway.CreateOrder(ctx, req, idemKey)
	if createErr != nil {
		if quefacontract.IsAPIErrorCategory(createErr, "insufficient_balance") {
			return s.markProcurementBlocked(ctx, childOrderID, "procurement_insufficient_balance", cfgSnap.Config.PollInterval()*6)
		}
		if quefacontract.IsAPIErrorCategory(createErr, "network_error") ||
			quefacontract.IsAPIErrorCategory(createErr, "idempotency_in_progress") ||
			quefacontract.IsAPIErrorCategory(createErr, "upstream_error") {
			if recovered, lookupErr := cfgSnap.Gateway.LookupOrderByMerchantNo(ctx, req.MerchantOrderNo); lookupErr == nil && recovered != nil {
				snap = recovered
				createErr = nil
			}
		}
	}
	if createErr != nil {
		nextDelay := cfgSnap.Config.PollInterval()
		var apiErr *quefacontract.APIError
		if errors.As(createErr, &apiErr) && apiErr.Hints.RetryAfter > nextDelay {
			nextDelay = apiErr.Hints.RetryAfter
		}
		s.schedulePollAfter(ctx, childOrderID, nextDelay)
		return createErr
	}
	return s.applyOrderSnapshot(ctx, childOrderID, snap, cfgSnap.Config)
}

// SyncOrder queries the latest Quefa order (and latest redemption if present) outside DB transactions
// and applies monotonic state updates + side-effect hooks.
func (s *OrderService) SyncOrder(ctx context.Context, childOrderID uint) error {
	if s.configSvc != nil {
		s.configSvc.RefreshIfStale(ctx)
	}
	cfgSnap := s.currentConfig()
	if cfgSnap.Gateway == nil || cfgSnap.GatewayConfigInvalid || cfgSnap.LoadError {
		return ErrGatewayNotConfigured
	}

	record, err := s.store.GetOrderByOrderID(ctx, childOrderID)
	if err != nil {
		return err
	}
	if record == nil {
		return quefadomain.ErrOrderNotFound
	}
	if record.Stage.IsTerminal() {
		return nil
	}

	// If agent_collect procurement never succeeded yet and no upstream order exists, check merchant_order_no or retry create.
	if record.UpstreamOrderID == nil || *record.UpstreamOrderID == "" {
		if !record.CreateDispatched {
			if record.CollectionMode == quefadomain.CollectionModeAgentCollect &&
				(record.Stage == quefadomain.StageWaitingProcurement || record.Stage == quefadomain.StageProcurementBlocked) {
				return s.CreateAgentCollectProcurement(ctx, childOrderID)
			}
			return nil
		}
		snap, lookupErr := cfgSnap.Gateway.LookupOrderByMerchantNo(ctx, record.MerchantOrderNo)
		if lookupErr != nil {
			if quefacontract.IsAPIErrorCategory(lookupErr, "not_found") &&
				record.CollectionMode == quefadomain.CollectionModeAgentCollect {
				return s.CreateAgentCollectProcurement(ctx, childOrderID)
			}
			s.handlePollError(ctx, record, lookupErr, cfgSnap.Config)
			return lookupErr
		}
		if err := s.applyOrderSnapshot(ctx, childOrderID, snap, cfgSnap.Config); err != nil {
			return err
		}
		record, err = s.store.GetOrderByOrderID(ctx, childOrderID)
		if err != nil || record == nil || record.Stage.IsTerminal() {
			return err
		}
	}

	if record.UpstreamOrderID == nil || *record.UpstreamOrderID == "" {
		return nil
	}

	orderSnap, err := cfgSnap.Gateway.GetOrder(ctx, *record.UpstreamOrderID)
	if err != nil {
		s.handlePollError(ctx, record, err, cfgSnap.Config)
		return err
	}
	if err := s.applyOrderSnapshot(ctx, childOrderID, orderSnap, cfgSnap.Config); err != nil {
		return err
	}

	// Follow latest_redemption_id to retrieve detailed progress, retry_allowed, and recovery_action.
	redemptionID := strings.TrimSpace(orderSnap.LatestRedemptionID)
	if redemptionID == "" {
		redemptionID = strings.TrimSpace(record.LatestRedemptionID)
	}
	if redemptionID != "" {
		redSnap, rErr := cfgSnap.Gateway.GetRedemption(ctx, redemptionID)
		if rErr == nil && redSnap != nil {
			// If Quefa points to an even newer redemption attempt, follow it once.
			if nextID := strings.TrimSpace(redSnap.LatestRedemptionID); nextID != "" && nextID != redemptionID {
				if newer, nErr := cfgSnap.Gateway.GetRedemption(ctx, nextID); nErr == nil && newer != nil {
					redSnap = newer
				}
			}
			if err := s.applyRedemptionSnapshot(ctx, childOrderID, redSnap, cfgSnap.Config); err != nil {
				return err
			}
		}
	}
	return nil
}

// OpenFulfillmentEntry validates eligibility and returns the hosted fulfillment_url for an authorized buyer.
func (s *OrderService) OpenFulfillmentEntry(ctx context.Context, parentOrderID uint) (*quefacontract.FulfillmentEntryResult, error) {
	record, err := s.store.GetOrderByParentOrderID(ctx, parentOrderID)
	if err != nil {
		return nil, err
	}
	if record == nil {
		return nil, quefadomain.ErrOrderNotFound
	}
	if record.RecoveryAction == "refund" || !record.Stage.CanOpenFulfillmentEntry() {
		return nil, quefadomain.ErrFulfillmentNotAllowed
	}

	// Refresh from upstream when fulfillment_url is missing or resubmit was newly enabled.
	if strings.TrimSpace(record.FulfillmentURL) == "" || record.Stage == quefadomain.StageResubmitRequired {
		_ = s.SyncOrder(ctx, record.OrderID)
		record, err = s.store.GetOrderByParentOrderID(ctx, parentOrderID)
		if err != nil {
			return nil, err
		}
		if record == nil {
			return nil, quefadomain.ErrOrderNotFound
		}
		if record.RecoveryAction == "refund" || !record.Stage.CanOpenFulfillmentEntry() {
			return nil, quefadomain.ErrFulfillmentNotAllowed
		}
	}

	if err := quefadomain.ValidateFulfillmentURL(record.FulfillmentURL); err != nil {
		return nil, quefadomain.ErrFulfillmentNotAllowed
	}
	return &quefacontract.FulfillmentEntryResult{
		FulfillmentURL: record.FulfillmentURL,
		Stage:          string(record.Stage),
	}, nil
}

// RecordWebhookEvent persists event_id deduplication and resolves the local child order ID for async sync.
func (s *OrderService) RecordWebhookEvent(ctx context.Context, env quefacontract.WebhookEnvelope) (duplicate bool, childOrderID uint, err error) {
	now := s.now().UTC()
	eventType := strings.ToLower(strings.TrimSpace(env.EffectiveType()))
	err = s.store.WithinTransaction(ctx, func(tx quefacontract.Transaction) error {
		inserted, txErr := tx.InsertWebhookEventIfAbsent(&quefadomain.WebhookEvent{
			EventID:         strings.TrimSpace(env.EventID),
			EventType:       eventType,
			UpstreamOrderID: strings.TrimSpace(env.OrderID),
			MerchantOrderNo: strings.TrimSpace(env.MerchantOrderNo),
			Status:          quefadomain.WebhookStatusReceived,
			ReceivedAt:      now,
		})
		if txErr != nil {
			return txErr
		}
		if !inserted {
			duplicate = true
			return nil
		}

		var order *quefadomain.Order
		if strings.TrimSpace(env.OrderID) != "" {
			order, txErr = tx.GetOrderByUpstreamIDForUpdate(env.OrderID)
			if txErr != nil {
				return txErr
			}
		}
		if order == nil && strings.TrimSpace(env.MerchantOrderNo) != "" {
			order, txErr = tx.GetOrderByMerchantNoForUpdate(env.MerchantOrderNo)
			if txErr != nil {
				return txErr
			}
		}
		if order != nil {
			childOrderID = order.OrderID
			// Record procurement.refunded strictly for procurement reconciliation, never buyer refund.
			if eventType == "procurement.refunded" && strings.TrimSpace(env.RefundRef) != "" {
				order.ProcurementRefundRef = strings.TrimSpace(env.RefundRef)
				order.UpdatedAt = now
				if saveErr := tx.SaveOrder(order); saveErr != nil {
					return saveErr
				}
			}
		}
		return nil
	})
	if err != nil || duplicate {
		return duplicate, 0, err
	}

	if eventType == "webhook.test" {
		doneAt := s.now().UTC()
		_ = s.store.WithinTransaction(ctx, func(tx quefacontract.Transaction) error {
			return tx.UpdateWebhookEventStatus(env.EventID, quefadomain.WebhookStatusProcessed, "test_ok", &doneAt)
		})
		return false, 0, nil
	}

	if childOrderID > 0 {
		hooks := s.getHooks()
		if hooks.EnqueueSync != nil {
			_ = hooks.EnqueueSync(ctx, childOrderID, 0)
		}
	}
	return false, childOrderID, nil
}

// EvaluateCancelOrExpire verifies upstream Quefa status before allowing a local platform_collect order to close.
// Because Quefa does not allow partners to actively cancel an order, active/pending or unknown upstream orders
// block immediate local cancellation.
func (s *OrderService) EvaluateCancelOrExpire(ctx context.Context, parentOrderID uint) (CancelEvaluation, error) {
	record, err := s.store.GetOrderByParentOrderID(ctx, parentOrderID)
	if err != nil {
		return CancelEvaluation{}, err
	}
	if record == nil {
		return CancelEvaluation{CanClose: true}, nil
	}
	if record.CollectionMode != quefadomain.CollectionModePlatformCollect {
		// In agent_collect, upstream order is only created AFTER local payment succeeds.
		return CancelEvaluation{CanClose: true}, nil
	}
	if !record.CreateDispatched && (record.UpstreamOrderID == nil || *record.UpstreamOrderID == "") {
		return CancelEvaluation{CanClose: true}, nil
	}
	if record.Stage == quefadomain.StageClosed {
		return CancelEvaluation{CanClose: true}, nil
	}

	if s.configSvc != nil {
		s.configSvc.RefreshIfStale(ctx)
	}
	cfgSnap := s.currentConfig()
	if cfgSnap.Gateway == nil {
		s.schedulePollAfter(ctx, record.OrderID, cfgSnap.Config.PollInterval())
		return CancelEvaluation{CanClose: false, Reason: "gateway_unavailable"}, quefadomain.ErrCancelBlockedByActive
	}

	var snap *quefacontract.OrderSnapshot
	if record.UpstreamOrderID != nil && *record.UpstreamOrderID != "" {
		snap, err = cfgSnap.Gateway.GetOrder(ctx, *record.UpstreamOrderID)
	} else {
		snap, err = cfgSnap.Gateway.LookupOrderByMerchantNo(ctx, record.MerchantOrderNo)
		if quefacontract.IsAPIErrorCategory(err, "not_found") {
			// Confirmed upstream never created the order.
			return CancelEvaluation{CanClose: true, Reason: "upstream_not_found"}, nil
		}
	}
	if err != nil {
		s.schedulePollAfter(ctx, record.OrderID, cfgSnap.Config.PollInterval())
		return CancelEvaluation{CanClose: false, Reason: "upstream_unknown"}, quefadomain.ErrCancelBlockedByActive
	}

	_ = s.applyOrderSnapshot(ctx, record.OrderID, snap, cfgSnap.Config)
	status := strings.ToLower(strings.TrimSpace(snap.Status))
	if status == "expired" || status == "closed" {
		return CancelEvaluation{CanClose: true, Reason: status, UpstreamID: snap.OrderID}, nil
	}
	return CancelEvaluation{CanClose: false, Reason: status, UpstreamID: snap.OrderID}, quefadomain.ErrCancelBlockedByActive
}

// MarkManualReview records an operator review note / transitions non-terminal anomalous orders to manual_review.
func (s *OrderService) MarkManualReview(ctx context.Context, parentOrderID uint, remark string) (*quefacontract.AdminOrderStatus, error) {
	now := s.now().UTC()
	var updated *quefadomain.Order
	err := s.store.WithinTransaction(ctx, func(tx quefacontract.Transaction) error {
		record, err := tx.GetOrderByParentOrderIDForUpdate(parentOrderID)
		if err != nil {
			return err
		}
		if record == nil {
			return quefadomain.ErrOrderNotFound
		}
		record.ReviewRemark = strings.TrimSpace(remark)
		if !record.Stage.IsTerminal() && record.Stage != quefadomain.StageManualReview {
			record.Stage = quefadomain.StageManualReview
		}
		record.UpdatedAt = now
		if err := tx.SaveOrder(record); err != nil {
			return err
		}
		updated = record
		return nil
	})
	if err != nil {
		return nil, err
	}
	return toAdminStatus(updated), nil
}

// MarkAgentCollectRefunded records shop-side refund completion for an agent_collect order after confirming
// upstream will not fulfill.
func (s *OrderService) MarkAgentCollectRefunded(ctx context.Context, parentOrderID uint, refundRef string) error {
	now := s.now().UTC()
	return s.store.WithinTransaction(ctx, func(tx quefacontract.Transaction) error {
		record, err := tx.GetOrderByParentOrderIDForUpdate(parentOrderID)
		if err != nil || record == nil {
			return err
		}
		if record.Stage == quefadomain.StageSucceeded {
			return quefadomain.ErrConflict
		}
		record.Stage = quefadomain.StageRefunded
		record.RetryAllowed = false
		record.PollStopped = true
		record.NextPollAt = nil
		if strings.TrimSpace(refundRef) != "" {
			record.RefundRef = strings.TrimSpace(refundRef)
		}
		record.UpdatedAt = now
		return tx.SaveOrder(record)
	})
}

// CanRefundAgentCollect reports whether an agent_collect Quefa order is safe for shop-side refund.
func (s *OrderService) CanRefundAgentCollect(ctx context.Context, parentOrderID uint) (bool, error) {
	record, err := s.store.GetOrderByParentOrderID(ctx, parentOrderID)
	if err != nil || record == nil {
		return false, err
	}
	if record.Stage == quefadomain.StageSucceeded || record.Stage == quefadomain.StageRecharging {
		return false, nil
	}
	if record.CollectionMode == quefadomain.CollectionModePlatformCollect {
		// Platform-collected orders must wait for verified Quefa refund.succeeded.
		return record.Stage == quefadomain.StageRefunded, nil
	}
	return record.Stage == quefadomain.StageProcurementBlocked ||
		record.Stage == quefadomain.StageManualReview ||
		record.Stage == quefadomain.StageRefunding ||
		record.Stage == quefadomain.StageClosed ||
		record.Stage == quefadomain.StageResubmitRequired, nil
}

func (s *OrderService) applyOrderSnapshot(ctx context.Context, childOrderID uint, snap *quefacontract.OrderSnapshot, cfg RuntimeConfig) error {
	if snap == nil {
		return nil
	}
	now := s.now().UTC()
	var (
		triggerPaid     bool
		triggerSuccess  bool
		triggerRefunded bool
		triggerClosed   bool
		snapshotCopy    *quefadomain.Order
	)

	err := s.store.WithinTransaction(ctx, func(tx quefacontract.Transaction) error {
		record, err := tx.GetOrderByOrderIDForUpdate(childOrderID)
		if err != nil {
			return err
		}
		if record == nil {
			return quefadomain.ErrOrderNotFound
		}

		if strings.TrimSpace(snap.OrderID) != "" && (record.UpstreamOrderID == nil || *record.UpstreamOrderID == "") {
			id := strings.TrimSpace(snap.OrderID)
			record.UpstreamOrderID = &id
		}
		if strings.TrimSpace(snap.ProcurementRefundRef) != "" {
			record.ProcurementRefundRef = strings.TrimSpace(snap.ProcurementRefundRef)
		}
		if record.Stage.IsTerminal() {
			record.UpdatedAt = now
			return tx.SaveOrder(record)
		}

		if strings.TrimSpace(snap.PayURL) != "" && validatePaymentArtifacts(snap.PayURL, "", cfg.AllowedPayDomains) == nil {
			record.PayURL = strings.TrimSpace(snap.PayURL)
		}
		if strings.TrimSpace(snap.QRCode) != "" && validatePaymentArtifacts("", snap.QRCode, cfg.AllowedPayDomains) == nil {
			record.QRCode = strings.TrimSpace(snap.QRCode)
		}
		if snap.PayExpiresAt != nil {
			record.PayExpiresAt = snap.PayExpiresAt
		}
		if strings.TrimSpace(snap.FulfillmentURL) != "" && quefadomain.ValidateFulfillmentURL(snap.FulfillmentURL) == nil {
			record.FulfillmentURL = strings.TrimSpace(snap.FulfillmentURL)
		}
		if strings.TrimSpace(snap.LatestRedemptionID) != "" {
			record.LatestRedemptionID = strings.TrimSpace(snap.LatestRedemptionID)
		}

		if !record.ShouldIgnoreProgress(snap.AttemptNo, snap.ProgressVersion) {
			if snap.AttemptNo > 0 {
				record.AttemptNo = snap.AttemptNo
			}
			if snap.ProgressVersion > 0 {
				record.ProgressVersion = snap.ProgressVersion
			}
			if snap.ProgressStage != "" {
				record.ProgressStage = snap.ProgressStage
			}
			if snap.ProgressMessage != "" {
				record.ProgressMessage = snap.ProgressMessage
			}
			if snap.ProgressUpdatedAt != nil {
				record.ProgressUpdatedAt = snap.ProgressUpdatedAt
			}
			record.RetryAllowed = snap.RetryAllowed
			record.NextAction = snap.NextAction
			record.RecoveryAction = snap.RecoveryAction
		}

		status := strings.ToLower(strings.TrimSpace(snap.Status))
		record.UpstreamStatus = status
		if snap.ResultCode != "" {
			record.ResultCode = snap.ResultCode
		}

		// Verify amount & currency for platform_collect before acknowledging paid.
		amountMismatch := false
		if record.CollectionMode == quefadomain.CollectionModePlatformCollect && isPaidOrBeyond(status) {
			if snap.Currency != "" && !strings.EqualFold(snap.Currency, record.SettlementCurrency) {
				amountMismatch = true
			}
			checkAmt := snap.PaidAmount
			if checkAmt == nil {
				checkAmt = snap.SaleAmount
			}
			if checkAmt != nil && !checkAmt.Round(2).Equal(record.SaleAmount.Decimal.Round(2)) {
				amountMismatch = true
			}
		}

		if amountMismatch {
			record.Stage = quefadomain.StageManualReview
			record.ExceptionCode = "payment_amount_or_currency_mismatch"
			record.RetryAllowed = false
			record.PollStopped = true
			record.NextPollAt = nil
			record.UpdatedAt = now
			if err := tx.SaveOrder(record); err != nil {
				return err
			}
			return nil
		}

		prevStage := record.Stage
		switch {
		case status == "refunded" || snap.SyncMark == "cancelled_refunded" || (snap.RecoveryAction == "refund" && strings.TrimSpace(snap.RefundRef) != "" && status != "succeeded"):
			record.Stage = quefadomain.StageRefunded
			record.RetryAllowed = false
			record.PollStopped = true
			record.NextPollAt = nil
			if strings.TrimSpace(snap.RefundRef) != "" {
				record.RefundRef = strings.TrimSpace(snap.RefundRef)
			}
			if record.CollectionMode == quefadomain.CollectionModePlatformCollect {
				triggerRefunded = true
			}
		case status == "partially_refunded":
			record.Stage = quefadomain.StageManualReview
			record.ExceptionCode = "order_partially_refunded"
			record.RetryAllowed = false
			next := computeNextPoll(now, snap.Hints, cfg.PollInterval())
			record.NextPollAt = next
			record.PollStopped = snap.Hints.PollStop
		case snap.RecoveryAction == "refund":
			record.Stage = quefadomain.StageRefunding
			record.RetryAllowed = false
			next := computeNextPoll(now, snap.Hints, cfg.PollInterval())
			record.NextPollAt = next
			record.PollStopped = snap.Hints.PollStop
		case status == "succeeded" || status == "completed":
			record.Stage = quefadomain.StageSucceeded
			record.RetryAllowed = false
			record.PollStopped = true
			record.NextPollAt = nil
			record.CompletedAt = &now
			record.CompletionEmailPending = true
			triggerSuccess = true
			if prevStage == quefadomain.StageWaitingPayment && record.CollectionMode == quefadomain.CollectionModePlatformCollect {
				triggerPaid = true
			}
		case status == "expired" || status == "closed":
			record.Stage = quefadomain.StageClosed
			record.RetryAllowed = false
			record.PollStopped = true
			record.NextPollAt = nil
			if record.CollectionMode == quefadomain.CollectionModePlatformCollect {
				triggerClosed = true
			}
		case status == "running" || status == "queued" || status == "review" || status == "processing":
			record.Stage = quefadomain.StageRecharging
			if prevStage == quefadomain.StageWaitingPayment && record.CollectionMode == quefadomain.CollectionModePlatformCollect {
				triggerPaid = true
			}
			next := computeNextPoll(now, snap.Hints, cfg.PollInterval())
			record.NextPollAt = next
			record.PollStopped = snap.Hints.PollStop
		case status == "paid":
			if prevStage == quefadomain.StageWaitingPayment && record.CollectionMode == quefadomain.CollectionModePlatformCollect {
				triggerPaid = true
			}
			if record.Stage != quefadomain.StageResubmitRequired && record.Stage != quefadomain.StageManualReview {
				record.Stage = quefadomain.StageWaitingSubmit
			}
			next := computeNextPoll(now, snap.Hints, cfg.PollInterval())
			record.NextPollAt = next
			record.PollStopped = snap.Hints.PollStop
		case status == "failed" || status == "cancelled" || status == "canceled":
			if snap.RetryAllowed && snap.NextAction == "resubmit" && snap.RecoveryAction != "refund" {
				record.Stage = quefadomain.StageResubmitRequired
				next := computeNextPoll(now, snap.Hints, cfg.PollInterval())
				record.NextPollAt = next
				record.PollStopped = snap.Hints.PollStop
			} else {
				record.Stage = quefadomain.StageManualReview
				record.RetryAllowed = false
				record.ExceptionCode = "upstream_failed"
				record.PollStopped = true
				record.NextPollAt = nil
			}
		default: // pending or accepted async
			if record.Stage == quefadomain.StageWaitingProcurement || record.Stage == quefadomain.StageProcurementBlocked {
				record.Stage = quefadomain.StageWaitingSubmit
			}
			next := computeNextPoll(now, snap.Hints, cfg.PollInterval())
			record.NextPollAt = next
			record.PollStopped = snap.Hints.PollStop
		}

		record.UpdatedAt = now
		if err := tx.SaveOrder(record); err != nil {
			return err
		}
		cp := *record
		snapshotCopy = &cp
		return nil
	})
	if err != nil {
		return err
	}

	// Execute cross-module side-effect hooks strictly OUTSIDE the Quefa store transaction.
	hooks := s.getHooks()
	if triggerPaid && hooks.OnPlatformOrderPaid != nil && snapshotCopy != nil {
		if err := hooks.OnPlatformOrderPaid(ctx, snapshotCopy, snap); err != nil {
			return err
		}
	}
	if triggerSuccess && hooks.OnOrderSucceeded != nil && snapshotCopy != nil {
		if err := hooks.OnOrderSucceeded(ctx, snapshotCopy); err != nil {
			return err
		}
	}
	if triggerRefunded && hooks.OnPlatformOrderRefunded != nil && snapshotCopy != nil {
		if err := hooks.OnPlatformOrderRefunded(ctx, snapshotCopy, snapshotCopy.RefundRef); err != nil {
			return err
		}
	}
	if triggerClosed && hooks.OnOrderClosed != nil && snapshotCopy != nil {
		if err := hooks.OnOrderClosed(ctx, snapshotCopy); err != nil {
			return err
		}
	}
	return nil
}

func (s *OrderService) applyRedemptionSnapshot(ctx context.Context, childOrderID uint, red *quefacontract.RedemptionSnapshot, cfg RuntimeConfig) error {
	if red == nil {
		return nil
	}
	now := s.now().UTC()
	var (
		triggerSuccess bool
		snapshotCopy   *quefadomain.Order
	)

	err := s.store.WithinTransaction(ctx, func(tx quefacontract.Transaction) error {
		record, err := tx.GetOrderByOrderIDForUpdate(childOrderID)
		if err != nil || record == nil {
			return err
		}
		if record.ShouldIgnoreProgress(red.AttemptNo, red.ProgressVersion) {
			return nil
		}
		if red.RedemptionID != "" {
			record.LatestRedemptionID = red.RedemptionID
		}
		if red.AttemptNo > 0 {
			record.AttemptNo = red.AttemptNo
		}
		if red.ProgressVersion > 0 {
			record.ProgressVersion = red.ProgressVersion
		}
		if red.ProgressStage != "" {
			record.ProgressStage = red.ProgressStage
		}
		if red.ProgressMessage != "" {
			record.ProgressMessage = red.ProgressMessage
		}
		if red.ProgressUpdatedAt != nil {
			record.ProgressUpdatedAt = red.ProgressUpdatedAt
		}
		if red.ResultCode != "" {
			record.ResultCode = red.ResultCode
		}
		record.RetryAllowed = red.RetryAllowed
		record.NextAction = red.NextAction
		record.RecoveryAction = red.RecoveryAction
		if strings.TrimSpace(red.FulfillmentURL) != "" && quefadomain.ValidateFulfillmentURL(red.FulfillmentURL) == nil {
			record.FulfillmentURL = strings.TrimSpace(red.FulfillmentURL)
		}

		status := strings.ToLower(strings.TrimSpace(red.Status))
		switch {
		case red.RecoveryAction == "refund":
			record.Stage = quefadomain.StageRefunding
			record.RetryAllowed = false
			record.NextPollAt = computeNextPoll(now, red.Hints, cfg.PollInterval())
			record.PollStopped = red.Hints.PollStop
		case status == "succeeded" || status == "completed":
			record.Stage = quefadomain.StageSucceeded
			record.RetryAllowed = false
			record.PollStopped = true
			record.NextPollAt = nil
			record.CompletedAt = &now
			record.CompletionEmailPending = true
			triggerSuccess = true
		case status == "running" || status == "queued" || status == "review" || status == "pending":
			record.Stage = quefadomain.StageRecharging
			record.NextPollAt = computeNextPoll(now, red.Hints, cfg.PollInterval())
			record.PollStopped = red.Hints.PollStop
		case status == "failed" || status == "cancelled" || status == "canceled":
			if red.RetryAllowed && red.NextAction == "resubmit" {
				record.Stage = quefadomain.StageResubmitRequired
				record.NextPollAt = computeNextPoll(now, red.Hints, cfg.PollInterval())
				record.PollStopped = red.Hints.PollStop
			} else {
				record.Stage = quefadomain.StageManualReview
				record.RetryAllowed = false
				record.ExceptionCode = "redemption_failed"
				record.PollStopped = true
				record.NextPollAt = nil
			}
		}

		record.UpdatedAt = now
		if err := tx.SaveOrder(record); err != nil {
			return err
		}
		cp := *record
		snapshotCopy = &cp
		return nil
	})
	if err != nil {
		return err
	}
	if triggerSuccess && snapshotCopy != nil {
		if hooks := s.getHooks(); hooks.OnOrderSucceeded != nil {
			return hooks.OnOrderSucceeded(ctx, snapshotCopy)
		}
	}
	return nil
}

func (s *OrderService) handlePollError(ctx context.Context, record *quefadomain.Order, err error, cfg RuntimeConfig) {
	now := s.now().UTC()
	reviewAfter := time.Duration(cfg.PollReviewAfterSeconds) * time.Second
	if reviewAfter <= 0 {
		reviewAfter = 24 * time.Hour
	}
	if now.Sub(record.CreatedAt) >= reviewAfter {
		_ = s.markException(ctx, record.OrderID, "poll_review_timeout", true)
		return
	}
	delay := cfg.PollInterval() * 2
	var apiErr *quefacontract.APIError
	if errors.As(err, &apiErr) {
		if apiErr.Hints.RetryAfter > delay {
			delay = apiErr.Hints.RetryAfter
		}
		if apiErr.Hints.PollAfter > delay {
			delay = apiErr.Hints.PollAfter
		}
	}
	s.schedulePollAfter(ctx, record.OrderID, delay)
}

func (s *OrderService) markProcurementBlocked(ctx context.Context, childOrderID uint, code string, retryAfter time.Duration) error {
	now := s.now().UTC()
	next := now.Add(retryAfter)
	return s.store.WithinTransaction(ctx, func(tx quefacontract.Transaction) error {
		record, err := tx.GetOrderByOrderIDForUpdate(childOrderID)
		if err != nil || record == nil || record.Stage.IsTerminal() {
			return err
		}
		record.Stage = quefadomain.StageProcurementBlocked
		record.ExceptionCode = code
		record.NextPollAt = &next
		record.PollStopped = false
		record.UpdatedAt = now
		return tx.SaveOrder(record)
	})
}

func (s *OrderService) markException(ctx context.Context, childOrderID uint, code string, stopPoll bool) error {
	now := s.now().UTC()
	return s.store.WithinTransaction(ctx, func(tx quefacontract.Transaction) error {
		record, err := tx.GetOrderByOrderIDForUpdate(childOrderID)
		if err != nil || record == nil || record.Stage.IsTerminal() {
			return err
		}
		record.Stage = quefadomain.StageManualReview
		record.ExceptionCode = code
		record.RetryAllowed = false
		record.PollStopped = stopPoll
		if stopPoll {
			record.NextPollAt = nil
		}
		record.UpdatedAt = now
		return tx.SaveOrder(record)
	})
}

func (s *OrderService) schedulePollAfter(ctx context.Context, childOrderID uint, delay time.Duration) {
	if delay < 5*time.Second {
		delay = 5 * time.Second
	}
	now := s.now().UTC()
	next := now.Add(delay)
	_ = s.store.WithinTransaction(ctx, func(tx quefacontract.Transaction) error {
		record, err := tx.GetOrderByOrderIDForUpdate(childOrderID)
		if err != nil || record == nil || record.Stage.IsTerminal() || record.PollStopped {
			return err
		}
		record.NextPollAt = &next
		record.UpdatedAt = now
		return tx.SaveOrder(record)
	})
}

func validatePaymentArtifacts(payURL, qrCode string, allowedDomains []string) error {
	if strings.TrimSpace(payURL) == "" && strings.TrimSpace(qrCode) == "" {
		return quefadomain.ErrPaymentURLInvalid
	}
	if strings.TrimSpace(payURL) != "" {
		if err := quefadomain.ValidatePaymentEntryURL(payURL, allowedDomains); err != nil {
			return err
		}
	}
	if strings.TrimSpace(qrCode) != "" {
		if err := quefadomain.ValidatePaymentQRCode(qrCode, allowedDomains); err != nil {
			return err
		}
	}
	return nil
}

func computeNextPoll(now time.Time, hints quefacontract.PollHints, defaultInterval time.Duration) *time.Time {
	if hints.PollStop {
		return nil
	}
	d := defaultInterval
	if d < 5*time.Second {
		d = 5 * time.Second
	}
	if hints.PollAfter > d {
		d = hints.PollAfter
	}
	if hints.RetryAfter > d {
		d = hints.RetryAfter
	}
	t := now.Add(d)
	return &t
}

func isPaidOrBeyond(status string) bool {
	switch status {
	case "paid", "running", "queued", "review", "processing", "succeeded", "completed":
		return true
	default:
		return false
	}
}
