package refund

import (
	"github.com/dujiao-next/internal/constants"
	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	ordercontract "github.com/dujiao-next/internal/modules/order/contract"
	orderdomain "github.com/dujiao-next/internal/modules/order/domain"
)

// validateDirectRefund runs with the payment order already locked. Lock its
// fulfillment child before the recharge and attempt, as upstream completion does.
func validateDirectRefund(tx ordercontract.Transaction, parent *orderdomain.Order, confirmedAttemptID uint) error {
	if !parent.HasDirectRecharge() && confirmedAttemptID == 0 {
		return nil
	}
	if confirmedAttemptID == 0 || parent.ParentID != nil || parent.ResellerID != nil ||
		parent.PaidAt == nil || !parent.RefundedAmount.IsZero() || len(parent.Children) != 1 ||
		(parent.Status != constants.OrderStatusPaid && parent.Status != constants.OrderStatusFulfilling) {
		return ErrOrderStatusInvalid
	}
	child, err := tx.Orders().GetByIDForUpdateWithChildren(parent.Children[0].ID)
	if err != nil {
		return err
	}
	if child == nil || child.ParentID == nil || *child.ParentID != parent.ID || child.ResellerID != nil ||
		child.UserID != parent.UserID || child.PaidAt == nil || !child.RefundedAmount.IsZero() || child.Status != parent.Status ||
		len(child.Children) != 0 || len(child.Items) != 1 || child.Items[0].FulfillmentType != constants.FulfillmentTypeKafudaGPT {
		return ErrOrderStatusInvalid
	}
	recharges := tx.KafudaGPT()
	record, err := recharges.GetRechargeForUpdate(child.ID)
	if err != nil {
		return err
	}
	if record == nil || record.OrderID != child.ID || record.CurrentAttemptID == nil || *record.CurrentAttemptID != confirmedAttemptID {
		return ErrOrderStatusInvalid
	}
	attempt, err := recharges.GetAttemptForUpdate(confirmedAttemptID)
	if err != nil {
		return err
	}
	if !kafudacontract.CanResolveManualAttempt(record, attempt) {
		return ErrOrderStatusInvalid
	}
	_, exists, err := tx.Fulfillments().FindByOrderIDForUpdate(child.ID)
	if err != nil {
		return err
	}
	if exists {
		return ErrOrderStatusInvalid
	}
	return nil
}
