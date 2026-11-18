package application

import (
	"context"
	"errors"
	"time"

	"github.com/dujiao-next/internal/constants"
	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
	orderapp "github.com/dujiao-next/internal/modules/order/application"
	ordercontract "github.com/dujiao-next/internal/modules/order/contract"
	orderdomain "github.com/dujiao-next/internal/modules/order/domain"
)

var ErrReconciliationLookup = errors.New("kafuda reconciliation lookup failed")

// ReconcileUnknownOrder verifies a supplier-provided numeric ID against the
// original attempt before applying it through the regular order transaction.
func ReconcileUnknownOrder(ctx context.Context, store ordercontract.Store, gateway kafudacontract.Gateway, childID, attemptID uint, upstreamID uint64, pollInterval time.Duration) (bool, error) {
	if pollInterval <= 0 || gateway == nil || childID == 0 || attemptID == 0 || upstreamID == 0 {
		return false, orderapp.ErrOrderStatusInvalid
	}
	err := withLockedDirectOrder(store, childID, func(tx ordercontract.Transaction, parent *orderdomain.Order) error {
		records := tx.KafudaGPT()
		record, err := records.GetRechargeForUpdate(childID)
		if err != nil {
			return err
		}
		if parent.Status != constants.OrderStatusPaid || record == nil || record.Stage != kafudadomain.StageManualReview ||
			record.ReasonCode != "outcome_unknown" || record.CurrentAttemptID == nil || *record.CurrentAttemptID != attemptID {
			return orderapp.ErrOrderStatusInvalid
		}
		attempt, err := records.GetAttemptForUpdate(attemptID)
		if err != nil {
			return err
		}
		if attempt == nil || attempt.OrderID != childID || attempt.Status != kafudadomain.AttemptReview ||
			attempt.UpstreamOrderID != nil || attempt.ActiveOrderID == nil || *attempt.ActiveOrderID != childID {
			return orderapp.ErrOrderStatusInvalid
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	observedAt := time.Now().UTC()
	result, err := gateway.GetOrder(ctx, upstreamID)
	if err != nil || result == nil {
		return false, ErrReconciliationLookup
	}
	if result.ID != upstreamID {
		return false, orderapp.ErrOrderStatusInvalid
	}
	// ApplyResult checks reference, product, plan and country again in the
	// transaction, and prevents binding a different ID after a concurrent lookup.
	completed, err := ApplyResult(store, childID, attemptID, *result, observedAt, observedAt.Add(pollInterval))
	if errors.Is(err, kafudadomain.ErrInvalidTransition) || errors.Is(err, kafudadomain.ErrConflict) {
		return false, orderapp.ErrOrderStatusInvalid
	}
	return completed, err
}
