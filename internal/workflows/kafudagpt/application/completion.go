package application

import (
	"time"

	"github.com/dujiao-next/internal/constants"
	fulfillmentdomain "github.com/dujiao-next/internal/modules/fulfillment/domain"
	kafudaapp "github.com/dujiao-next/internal/modules/kafudagpt/application"
	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
	orderapp "github.com/dujiao-next/internal/modules/order/application"
	ordercontract "github.com/dujiao-next/internal/modules/order/contract"
	orderdomain "github.com/dujiao-next/internal/modules/order/domain"
)

// ApplyResult records the supplier outcome and completes a paid direct order
// atomically. A fresh completion flag is safe to use only after commit.
func ApplyResult(store ordercontract.Store, childID, attemptID uint, result kafudacontract.OrderResult, observedAt, nextPoll time.Time) (bool, error) {
	var completed bool
	err := withLockedDirectOrder(store, childID, func(tx ordercontract.Transaction, parent *orderdomain.Order) error {
		var err error
		completed, err = kafudaapp.RecordResultInTransaction(tx.KafudaGPT(), childID, attemptID, result, observedAt, nextPoll)
		if err != nil || !completed {
			return err
		}
		return completeShopOrders(tx.Orders(), parent, childID)
	})
	return completed && err == nil, err
}

// CompleteManual records delivery already carried out by an authorized operator.
// The caller must attest that the previous supplier attempt cannot deliver.
func CompleteManual(store ordercontract.Store, childID, attemptID, adminID uint, confirmed bool, now time.Time) error {
	if childID == 0 || attemptID == 0 || adminID == 0 || !confirmed || now.IsZero() {
		return orderapp.ErrOrderStatusInvalid
	}
	return withLockedDirectOrder(store, childID, func(tx ordercontract.Transaction, parent *orderdomain.Order) error {
		if parent.Status != constants.OrderStatusPaid {
			return orderapp.ErrOrderStatusInvalid
		}
		recharges := tx.KafudaGPT()
		record, err := recharges.GetRechargeForUpdate(childID)
		if err != nil {
			return err
		}
		if record == nil || record.Stage != kafudadomain.StageManualDelivery ||
			record.CurrentAttemptID == nil || *record.CurrentAttemptID != attemptID {
			return orderapp.ErrOrderStatusInvalid
		}
		attempt, err := recharges.GetAttemptForUpdate(attemptID)
		if err != nil {
			return err
		}
		if attempt == nil || attempt.OrderID != childID || attempt.ActiveOrderID != nil ||
			(attempt.Status != kafudadomain.AttemptFailed && attempt.Status != kafudadomain.AttemptCancelled) ||
			now.Before(attempt.UpdatedAt) || now.Before(record.UpdatedAt) {
			return orderapp.ErrOrderStatusInvalid
		}
		if _, exists, err := tx.Fulfillments().FindByOrderIDForUpdate(childID); err != nil {
			return err
		} else if exists {
			return orderapp.ErrOrderStatusInvalid
		}
		if err := tx.Fulfillments().Create(&fulfillmentdomain.Fulfillment{
			OrderID: childID, Type: constants.FulfillmentTypeManual, Status: constants.FulfillmentStatusDelivered,
			DeliveredBy: &adminID, DeliveredAt: &now, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			return err
		}
		record.Stage, record.ReasonCode, record.UpdatedAt, record.CompletedAt = kafudadomain.StageCompleted, "", now, &now
		if err := recharges.SaveRecharge(record); err != nil {
			return err
		}
		return completeShopOrders(tx.Orders(), parent, childID)
	})
}

func completeShopOrders(orders ordercontract.Store, parent *orderdomain.Order, childID uint) error {
	if parent.Status != constants.OrderStatusPaid {
		return orderapp.ErrOrderStatusInvalid
	}
	for _, id := range []uint{childID, parent.ID} {
		if err := orders.UpdateStatus(id, constants.OrderStatusCompleted, nil); err != nil {
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
	return store.WithinTransaction(func(tx ordercontract.Transaction) error {
		orders := tx.Orders()
		parent, err := orders.GetByIDForUpdateWithChildren(*hint.ParentID)
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
		(order.Status == constants.OrderStatusPaid || order.Status == constants.OrderStatusCompleted)
}
