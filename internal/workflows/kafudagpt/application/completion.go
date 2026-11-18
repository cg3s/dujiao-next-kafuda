package application

import (
	"context"
	"time"

	"github.com/dujiao-next/internal/config"
	"github.com/dujiao-next/internal/constants"
	"github.com/dujiao-next/internal/logger"
	fulfillmentdomain "github.com/dujiao-next/internal/modules/fulfillment/domain"
	kafudaapp "github.com/dujiao-next/internal/modules/kafudagpt/application"
	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
	orderapp "github.com/dujiao-next/internal/modules/order/application"
	ordercontract "github.com/dujiao-next/internal/modules/order/contract"
	orderdomain "github.com/dujiao-next/internal/modules/order/domain"
	settingsapp "github.com/dujiao-next/internal/modules/settings/application"
)

// EnqueueCompletionEmail applies the existing parent-order notification policy
// after a fresh completion commits. Notification failure cannot undo delivery.
func EnqueueCompletionEmail(ctx context.Context, store ordercontract.Store, records kafudacontract.Store, queue ordercontract.Queue, settings *settingsapp.Service, cfg *config.Config, childID uint) {
	if records == nil || queue == nil || !queue.Enabled() {
		return
	}
	now := time.Now().UTC()
	claimed, err := records.ClaimCompletionEmail(ctx, childID, now, now.Add(time.Minute))
	if err != nil {
		logger.Warnw("kafuda_completion_email_state_unavailable", "child_order_id", childID)
		return
	}
	if !claimed {
		return
	}
	child, err := store.GetByID(childID)
	if err != nil || child == nil || child.ParentID == nil {
		logger.Warnw("kafuda_completion_email_order_unavailable", "child_order_id", childID)
		return
	}
	var email config.EmailConfig
	if cfg != nil {
		email = cfg.Email
	}
	if _, err := orderapp.EnqueueStatusEmailTaskIfEligible(store, queue, settings, email, *child.ParentID, constants.OrderStatusCompleted); err != nil {
		logger.Warnw("kafuda_completion_email_enqueue_failed", "order_id", *child.ParentID)
		return
	}
	if err := records.ClearCompletionEmail(ctx, childID); err != nil {
		logger.Warnw("kafuda_completion_email_ack_failed", "child_order_id", childID)
	}
}

// CompleteManualAndNotify is the administrator entry point for a confirmed
// delivery. A rejected or replayed operation never enqueues an email.
func CompleteManualAndNotify(ctx context.Context, store ordercontract.Store, records kafudacontract.Store, queue ordercontract.Queue, settings *settingsapp.Service, cfg *config.Config, childID, attemptID, adminID uint, confirmed bool, now time.Time) error {
	if err := CompleteManual(store, childID, attemptID, adminID, confirmed, now); err != nil {
		return err
	}
	EnqueueCompletionEmail(ctx, store, records, queue, settings, cfg, childID)
	return nil
}

// ApplyResult records the supplier outcome and completes a paid direct order
// atomically. A fresh completion flag is safe to use only after commit.
func ApplyResult(store ordercontract.Store, childID, attemptID uint, result kafudacontract.OrderResult, startedAt, receivedAt time.Time, pollInterval time.Duration) (bool, error) {
	var completed bool
	err := withLockedDirectOrder(store, childID, func(tx ordercontract.Transaction, parent *orderdomain.Order) error {
		var err error
		recharges := tx.KafudaGPT()
		completed, err = kafudaapp.RecordResultInTransaction(recharges, childID, attemptID, result, startedAt, receivedAt, receivedAt.Add(pollInterval))
		if err != nil {
			return err
		}
		record, err := recharges.GetRechargeForUpdate(childID)
		if err != nil {
			return err
		}
		if !completed {
			return syncManualShopOrders(tx.Orders(), parent, childID, record)
		}
		if record == nil || record.CompletedAt == nil {
			return orderapp.ErrOrderStatusInvalid
		}
		record.CompletionEmailPending = true
		if err := recharges.SaveRecharge(record); err != nil {
			return err
		}
		deliveredAt := *record.CompletedAt
		if err := tx.Fulfillments().Create(&fulfillmentdomain.Fulfillment{
			OrderID: childID, Type: constants.FulfillmentTypeKafudaGPT, Status: constants.FulfillmentStatusDelivered,
			DeliveredAt: &deliveredAt, CreatedAt: deliveredAt, UpdatedAt: deliveredAt,
		}); err != nil {
			return err
		}
		return setOpenShopOrderStatus(tx.Orders(), parent, childID, constants.OrderStatusCompleted)
	})
	return completed && err == nil, err
}

// CompleteManual records delivery already carried out by an authorized operator.
// The caller must attest that the previous supplier attempt cannot deliver.
func CompleteManual(store ordercontract.Store, childID, attemptID, adminID uint, confirmed bool, now time.Time) error {
	return withConfirmedManualAttempt(store, childID, attemptID, adminID, confirmed, now, func(tx ordercontract.Transaction, parent *orderdomain.Order, record *kafudadomain.Recharge) error {
		if err := tx.Fulfillments().Create(&fulfillmentdomain.Fulfillment{
			OrderID: childID, Type: constants.FulfillmentTypeManual, Status: constants.FulfillmentStatusDelivered,
			DeliveredBy: &adminID, DeliveredAt: &now, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			return err
		}
		record.Stage, record.ReasonCode, record.UpdatedAt, record.CompletedAt = kafudadomain.StageCompleted, "", now, &now
		record.CompletionEmailPending = true
		if err := tx.KafudaGPT().SaveRecharge(record); err != nil {
			return err
		}
		return setOpenShopOrderStatus(tx.Orders(), parent, childID, constants.OrderStatusCompleted)
	})
}

// ReleaseManual returns an undelivered order to the buyer submission stage.
// The terminal attempt and all financial information remain on record.
func ReleaseManual(store ordercontract.Store, childID, attemptID, adminID uint, confirmed bool, now time.Time) error {
	err := withConfirmedManualAttempt(store, childID, attemptID, adminID, confirmed, now, func(tx ordercontract.Transaction, _ *orderdomain.Order, record *kafudadomain.Recharge) error {
		record.Stage, record.ReasonCode, record.UpdatedAt = kafudadomain.StageWaiting, "", now
		return tx.KafudaGPT().SaveRecharge(record)
	})
	if err == nil {
		logger.Infow("kafuda_manual_takeover_released", "child_order_id", childID, "attempt_id", attemptID, "admin_id", adminID)
	}
	return err
}

func withConfirmedManualAttempt(store ordercontract.Store, childID, attemptID, adminID uint, confirmed bool, now time.Time,
	run func(ordercontract.Transaction, *orderdomain.Order, *kafudadomain.Recharge) error,
) error {
	if childID == 0 || attemptID == 0 || adminID == 0 || !confirmed || now.IsZero() {
		return orderapp.ErrOrderStatusInvalid
	}
	return withLockedDirectOrder(store, childID, func(tx ordercontract.Transaction, parent *orderdomain.Order) error {
		if !isOpenPaidShopOrder(parent.Status) {
			return orderapp.ErrOrderStatusInvalid
		}
		recharges := tx.KafudaGPT()
		record, err := recharges.GetRechargeForUpdate(childID)
		if err != nil {
			return err
		}
		if record == nil || record.OrderID != childID ||
			record.CurrentAttemptID == nil || *record.CurrentAttemptID != attemptID {
			return orderapp.ErrOrderStatusInvalid
		}
		attempt, err := recharges.GetAttemptForUpdate(attemptID)
		if err != nil {
			return err
		}
		if !kafudacontract.CanResolveManualAttempt(record, attempt) ||
			now.Before(attempt.UpdatedAt) || now.Before(record.UpdatedAt) {
			return orderapp.ErrOrderStatusInvalid
		}
		if _, exists, err := tx.Fulfillments().FindByOrderIDForUpdate(childID); err != nil {
			return err
		} else if exists {
			return orderapp.ErrOrderStatusInvalid
		}
		return run(tx, parent, record)
	})
}

func setOpenShopOrderStatus(orders ordercontract.Store, parent *orderdomain.Order, childID uint, status string) error {
	if !isOpenPaidShopOrder(parent.Status) {
		return orderapp.ErrOrderStatusInvalid
	}
	for _, id := range []uint{childID, parent.ID} {
		if err := orders.UpdateStatus(id, status, nil); err != nil {
			return err
		}
	}
	return nil
}

func withLockedDirectOrder(store ordercontract.Store, childID uint, run func(ordercontract.Transaction, *orderdomain.Order) error) error {
	hint, err := store.GetByID(childID)
	if err != nil {
		return err
	}
	if hint == nil || hint.ParentID == nil {
		return orderapp.ErrOrderNotFound
	}
	return withLockedDirectOrderIDs(store, *hint.ParentID, childID, run)
}

func withLockedDirectOrderIDs(store ordercontract.Store, parentID, childID uint, run func(ordercontract.Transaction, *orderdomain.Order) error) error {
	return store.WithinTransaction(func(tx ordercontract.Transaction) error {
		orders := tx.Orders()
		parent, err := orders.GetByIDForUpdateWithChildren(parentID)
		if err != nil {
			return err
		}
		if parent == nil || len(parent.Children) != 1 || parent.Children[0].ID != childID {
			return orderapp.ErrOrderStatusInvalid
		}
		child, err := orders.GetByIDForUpdateWithChildren(childID)
		if err != nil {
			return err
		}
		if child == nil || child.ParentID == nil || *child.ParentID != parent.ID ||
			parent.ParentID != nil || parent.ResellerID != nil || child.ResellerID != nil ||
			parent.UserID == 0 || child.UserID != parent.UserID ||
			len(child.Children) != 0 || len(child.Items) != 1 ||
			child.Items[0].FulfillmentType != constants.FulfillmentTypeKafudaGPT ||
			!paidDirectOrder(parent) || !paidDirectOrder(child) || child.Status != parent.Status {
			return orderapp.ErrOrderStatusInvalid
		}
		return run(tx, parent)
	})
}

func paidDirectOrder(order *orderdomain.Order) bool {
	return order.PaidAt != nil && order.RefundedAmount.IsZero() &&
		(isOpenPaidShopOrder(order.Status) || order.Status == constants.OrderStatusCompleted)
}
