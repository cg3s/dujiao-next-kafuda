package application

import (
	"context"

	"github.com/dujiao-next/internal/constants"
	kafudaapp "github.com/dujiao-next/internal/modules/kafudagpt/application"
	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
	orderapp "github.com/dujiao-next/internal/modules/order/application"
	ordercontract "github.com/dujiao-next/internal/modules/order/contract"
	orderdomain "github.com/dujiao-next/internal/modules/order/domain"
)

// WithShopOrderTransitions keeps recharge mutations and the manual status of
// the corresponding shop order in one transaction with the same lock order.
func WithShopOrderTransitions(records *kafudaapp.Service, orders ordercontract.Store) *kafudaapp.Service {
	return records.WithAttemptTransaction(func(ctx context.Context, childID uint, run func(kafudacontract.Transaction) error) error {
		return withLockedDirectOrder(orders, childID, func(tx ordercontract.Transaction, parent *orderdomain.Order) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if !isOpenPaidShopOrder(parent.Status) {
				return orderapp.ErrOrderStatusInvalid
			}
			if err := run(tx.KafudaGPT()); err != nil {
				return err
			}
			record, err := tx.KafudaGPT().GetRechargeForUpdate(childID)
			if err != nil {
				return err
			}
			return syncManualShopOrders(tx.Orders(), parent, childID, record)
		})
	})
}

// SyncManualShopOrderStatus reconciles older manual recharges under order locks.
func SyncManualShopOrderStatus(orders ordercontract.Store, childID uint) error {
	return withLockedDirectOrder(orders, childID, func(tx ordercontract.Transaction, parent *orderdomain.Order) error {
		record, err := tx.KafudaGPT().GetRechargeForUpdate(childID)
		if err != nil {
			return err
		}
		return syncManualShopOrders(tx.Orders(), parent, childID, record)
	})
}

func syncManualShopOrders(orders ordercontract.Store, parent *orderdomain.Order, childID uint, record *kafudadomain.Recharge) error {
	if record == nil {
		return orderapp.ErrOrderStatusInvalid
	}
	if record.CompletedAt != nil || record.Stage != kafudadomain.StageManualReview && record.Stage != kafudadomain.StageManualDelivery {
		return nil
	}
	if parent.Status == constants.OrderStatusFulfilling {
		return nil
	}
	return setOpenShopOrderStatus(orders, parent, childID, constants.OrderStatusFulfilling)
}

func isOpenPaidShopOrder(status string) bool {
	return status == constants.OrderStatusPaid || status == constants.OrderStatusFulfilling
}
