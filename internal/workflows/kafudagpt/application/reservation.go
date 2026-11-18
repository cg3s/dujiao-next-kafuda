package application

import (
	"strings"
	"time"

	"github.com/dujiao-next/internal/constants"
	kafudaapp "github.com/dujiao-next/internal/modules/kafudagpt/application"
	orderapp "github.com/dujiao-next/internal/modules/order/application"
	ordercontract "github.com/dujiao-next/internal/modules/order/contract"
	orderdomain "github.com/dujiao-next/internal/modules/order/domain"
)

// ReserveBuyerAttempt resolves the fulfillment child from the buyer's checkout
// number. Callers may start upstream work only for a newly created reservation.
func ReserveBuyerAttempt(store ordercontract.Store, scope ordercontract.TenantScope, orderNo string, buyerID uint, now, leaseUntil time.Time) (*kafudaapp.Reservation, error) {
	orderNo = strings.TrimSpace(orderNo)
	if scope.ResellerID != nil || buyerID == 0 || orderNo == "" {
		return nil, orderapp.ErrOrderNotFound
	}
	order, err := store.GetByOrderNoAndUserScoped(orderNo, buyerID, scope)
	if err != nil {
		return nil, err
	}
	if order == nil {
		return nil, orderapp.ErrOrderNotFound
	}
	if len(order.Children) != 1 {
		return nil, orderapp.ErrOrderStatusInvalid
	}
	childID := order.Children[0].ID
	var reservation *kafudaapp.Reservation
	err = withLockedDirectOrderIDs(store, order.ID, childID, func(tx ordercontract.Transaction, parent *orderdomain.Order) error {
		if parent.UserID != buyerID || parent.OrderNo != orderNo {
			return orderapp.ErrOrderNotFound
		}
		if parent.Status != constants.OrderStatusPaid {
			return orderapp.ErrOrderStatusInvalid
		}
		reserved, err := kafudaapp.ReserveInTransaction(tx.KafudaGPT(), childID, now, leaseUntil)
		reservation = reserved
		return err
	})
	if err != nil {
		return nil, err
	}
	return reservation, nil
}
