package application

import (
	"time"

	"github.com/dujiao-next/internal/constants"
	kafudaapp "github.com/dujiao-next/internal/modules/kafudagpt/application"
	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	orderapp "github.com/dujiao-next/internal/modules/order/application"
	ordercontract "github.com/dujiao-next/internal/modules/order/contract"
	orderdomain "github.com/dujiao-next/internal/modules/order/domain"
)

// ApplyResult records the supplier outcome and completes a paid direct order
// atomically. A fresh completion flag is safe to use only after commit.
func ApplyResult(store ordercontract.Store, childID, attemptID uint, result kafudacontract.OrderResult, observedAt, nextPoll time.Time) (bool, error) {
	hint, err := store.GetByID(childID)
	if err != nil {
		return false, err
	}
	if hint == nil || hint.ParentID == nil {
		return false, orderapp.ErrOrderNotFound
	}
	var completed bool
	err = store.WithinTransaction(func(tx ordercontract.Transaction) error {
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
		completed, err = kafudaapp.RecordResultInTransaction(tx.KafudaGPT(), childID, attemptID, result, observedAt, nextPoll)
		if err != nil || !completed {
			return err
		}
		if parent.Status != constants.OrderStatusPaid {
			return orderapp.ErrOrderStatusInvalid
		}
		for _, id := range []uint{childID, parent.ID} {
			if err := orders.UpdateStatus(id, constants.OrderStatusCompleted, nil); err != nil {
				return err
			}
		}
		return nil
	})
	return completed && err == nil, err
}

func paidDirectOrder(order *orderdomain.Order) bool {
	return order.PaidAt != nil && order.RefundedAmount.IsZero() &&
		(order.Status == constants.OrderStatusPaid || order.Status == constants.OrderStatusCompleted)
}
