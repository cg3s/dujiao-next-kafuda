package application

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/dujiao-next/internal/config"
	"github.com/dujiao-next/internal/constants"
	"github.com/dujiao-next/internal/logger"
	fulfillmentdomain "github.com/dujiao-next/internal/modules/fulfillment/domain"
	orderapp "github.com/dujiao-next/internal/modules/order/application"
	ordercontract "github.com/dujiao-next/internal/modules/order/contract"
	orderdomain "github.com/dujiao-next/internal/modules/order/domain"
	paymentapp "github.com/dujiao-next/internal/modules/payment/application"
	paymentcontract "github.com/dujiao-next/internal/modules/payment/contract"
	quefacontract "github.com/dujiao-next/internal/modules/quefa/contract"
	quefadomain "github.com/dujiao-next/internal/modules/quefa/domain"
	settingsapp "github.com/dujiao-next/internal/modules/settings/application"
	walletapp "github.com/dujiao-next/internal/modules/wallet/application"
	walletcontract "github.com/dujiao-next/internal/modules/wallet/contract"
	"github.com/dujiao-next/internal/shared/money"
	"github.com/shopspring/decimal"
)

// CompleteOrder atomically creates a sanitized Fulfillment record for a succeeded Quefa child order,
// transitions child and parent orders to completed, and ensures replays never create duplicate deliveries.
func CompleteOrder(store ordercontract.Store, childID uint, now time.Time) (bool, error) {
	if store == nil || childID == 0 {
		return false, orderapp.ErrOrderNotFound
	}
	var completed bool
	err := store.WithinTransaction(func(tx ordercontract.Transaction) error {
		orders := tx.Orders()
		child, err := orders.GetByID(childID)
		if err != nil {
			return err
		}
		if child == nil || child.ParentID == nil {
			return orderapp.ErrOrderNotFound
		}
		// Lock parent -> child -> fulfillment in fixed order (compatible with SQLite & PostgreSQL).
		parent, err := orders.GetByIDForUpdateWithChildren(*child.ParentID)
		if err != nil {
			return err
		}
		if parent == nil {
			return orderapp.ErrOrderNotFound
		}
		lockedChild, err := orders.GetByIDForUpdateWithChildren(childID)
		if err != nil {
			return err
		}
		if lockedChild == nil {
			return orderapp.ErrOrderNotFound
		}
		if lockedChild.Status == constants.OrderStatusCompleted {
			return nil
		}
		if lockedChild.Status == constants.OrderStatusRefunded || lockedChild.Status == constants.OrderStatusCanceled {
			return orderapp.ErrOrderStatusInvalid
		}
		if lockedChild.PaidAt == nil {
			return orderapp.ErrOrderStatusInvalid
		}
		_, exists, err := tx.Fulfillments().FindByOrderIDForUpdate(childID)
		if err != nil {
			return err
		}
		if !exists {
			if err := tx.Fulfillments().Create(&fulfillmentdomain.Fulfillment{
				OrderID:     childID,
				Type:        constants.FulfillmentTypeQuefaHosted,
				Status:      constants.FulfillmentStatusDelivered,
				DeliveredAt: &now,
				CreatedAt:   now,
				UpdatedAt:   now,
			}); err != nil {
				return err
			}
		}
		if err := orders.UpdateStatus(childID, constants.OrderStatusCompleted, map[string]any{"updated_at": now}); err != nil {
			return err
		}
		if _, err := orderapp.SyncParentStatus(orders, parent.ID, now); err != nil {
			return err
		}
		completed = true
		return nil
	})
	return completed && err == nil, err
}

// EnqueueCompletionEmail sends the standard order completed email once after commit.
func EnqueueCompletionEmail(ctx context.Context, orderStore ordercontract.Store, quefaStore quefacontract.Store, q ordercontract.Queue, settings *settingsapp.Service, cfg *config.Config, childID uint) {
	if orderStore == nil || quefaStore == nil || q == nil || !q.Enabled() {
		return
	}
	now := time.Now().UTC()
	claimed, err := quefaStore.ClaimCompletionEmail(ctx, childID, now, now.Add(time.Minute))
	if err != nil || !claimed {
		return
	}
	child, err := orderStore.GetByID(childID)
	if err != nil || child == nil || child.ParentID == nil {
		return
	}
	var emailCfg config.EmailConfig
	if cfg != nil {
		emailCfg = cfg.Email
	}
	if _, err := orderapp.EnqueueStatusEmailTaskIfEligible(orderStore, q, settings, emailCfg, *child.ParentID, constants.OrderStatusCompleted); err != nil {
		logger.Warnw("quefa_completion_email_enqueue_failed", "order_id", *child.ParentID)
		return
	}
	_ = quefaStore.ClearCompletionEmail(ctx, childID)
}

// ApplyPlatformCollectPaid applies a verified Quefa platform_collect payment confirmation
// through PaymentService.HandleCallback so existing notifications, affiliate, and member-level hooks run cleanly.
func ApplyPlatformCollectPaid(_ context.Context, paymentSvc *paymentapp.PaymentService, paymentStore paymentcontract.Store, record *quefadomain.Order, snap *quefacontract.OrderSnapshot) error {
	if paymentSvc == nil || paymentStore == nil || record == nil {
		return nil
	}
	payments, err := paymentStore.ListByOrderID(record.ParentOrderID)
	if err != nil {
		return err
	}
	var targetID uint
	var channelID uint
	var orderNo string
	for i := range payments {
		p := &payments[i]
		if p.ProviderType != constants.PaymentProviderQuefa {
			continue
		}
		if p.Status == constants.PaymentStatusSuccess {
			return nil // Already confirmed
		}
		targetID = p.ID
		channelID = p.ChannelID
		orderNo = p.GatewayOrderNo
		break
	}
	if targetID == 0 {
		return nil
	}
	now := time.Now().UTC()
	providerRef := ""
	if snap != nil {
		providerRef = strings.TrimSpace(snap.OrderID)
	}
	if providerRef == "" && record.UpstreamOrderID != nil {
		providerRef = *record.UpstreamOrderID
	}
	_, err = paymentSvc.HandleCallback(paymentapp.PaymentCallbackInput{
		PaymentID:   targetID,
		OrderNo:     orderNo,
		ChannelID:   channelID,
		Status:      constants.PaymentStatusSuccess,
		ProviderRef: providerRef,
		Amount:      record.SaleAmount,
		Currency:    record.SettlementCurrency,
		PaidAt:      &now,
	})
	return err
}

// ApplyPlatformCollectRefunded records a verified Quefa platform_collect refund (refund.succeeded),
// refunds any mixed-payment wallet allocation back to the user's wallet, and marks the order refunded.
func ApplyPlatformCollectRefunded(orderStore ordercontract.Store, walletSvc *walletapp.Service, record *quefadomain.Order, refundRef string) error {
	if orderStore == nil || record == nil {
		return nil
	}
	now := time.Now().UTC()
	return orderStore.WithinTransaction(func(tx ordercontract.Transaction) error {
		orders := tx.Orders()
		parent, err := orders.GetByIDForUpdateWithChildren(record.ParentOrderID)
		if err != nil || parent == nil {
			return err
		}
		if parent.Status == constants.OrderStatusRefunded {
			return nil
		}
		if parent.PaidAt == nil {
			return nil
		}
		child, err := orders.GetByIDForUpdateWithChildren(record.OrderID)
		if err != nil || child == nil {
			return err
		}
		if child.Status == constants.OrderStatusCompleted {
			return orderapp.ErrOrderStatusInvalid
		}

		// Refund online portion via manual/external refund record (deduplicated by refundRef).
		onlineRefund := parent.OnlinePaidAmount.Decimal.Round(2)
		if onlineRefund.LessThanOrEqual(decimal.Zero) {
			onlineRefund = parent.TotalAmount.Decimal.Sub(parent.WalletPaidAmount.Decimal).Round(2)
		}
		remark := "Quefa refund confirmed"
		if strings.TrimSpace(refundRef) != "" {
			remark = fmt.Sprintf("Quefa refund confirmed (%s)", strings.TrimSpace(refundRef))
		}
		if onlineRefund.GreaterThan(decimal.Zero) {
			if err := orders.CreateRefundRecord(&orderdomain.OrderRefundRecord{
				UserID:     parent.UserID,
				GuestEmail: parent.GuestEmail,
				OrderID:    parent.ID,
				Type:       constants.OrderRefundTypeManual,
				Amount:     money.FromDecimal(onlineRefund),
				Currency:   parent.Currency,
				Remark:     remark,
				CreatedAt:  now,
				UpdatedAt:  now,
			}); err != nil {
				return err
			}
		}

		// Refund mixed-payment wallet allocation back to member wallet if present.
		walletRefund := parent.WalletPaidAmount.Decimal.Round(2)
		if walletRefund.GreaterThan(decimal.Zero) && parent.UserID > 0 && walletSvc != nil {
			orderID := parent.ID
			if _, _, err := walletSvc.CreditInTransaction(tx.Wallets(), walletcontract.CreditInput{
				UserID:    parent.UserID,
				Amount:    money.FromDecimal(walletRefund),
				Currency:  parent.Currency,
				Type:      constants.WalletTxnTypeOrderRefund,
				Reference: fmt.Sprintf("quefa-refund-wallet-%d", parent.ID),
				Remark:    remark,
				OrderID:   &orderID,
			}); err != nil {
				return err
			}
			if err := orders.CreateRefundRecord(&orderdomain.OrderRefundRecord{
				UserID:     parent.UserID,
				GuestEmail: parent.GuestEmail,
				OrderID:    parent.ID,
				Type:       constants.OrderRefundTypeWallet,
				Amount:     money.FromDecimal(walletRefund),
				Currency:   parent.Currency,
				Remark:     remark + " (wallet portion)",
				CreatedAt:  now,
				UpdatedAt:  now,
			}); err != nil {
				return err
			}
		}

		refundedTotal := parent.TotalAmount
		if err := orders.UpdateFields(child.ID, map[string]any{
			"status":          constants.OrderStatusRefunded,
			"refunded_amount": child.TotalAmount,
			"updated_at":      now,
		}); err != nil {
			return err
		}
		if err := orders.UpdateFields(parent.ID, map[string]any{
			"status":          constants.OrderStatusRefunded,
			"refunded_amount": refundedTotal,
			"updated_at":      now,
		}); err != nil {
			return err
		}
		if parent.UserID > 0 {
			_ = orders.ApplyOrderRefund(parent.UserID, refundedTotal.Decimal)
		}
		return nil
	})
}
