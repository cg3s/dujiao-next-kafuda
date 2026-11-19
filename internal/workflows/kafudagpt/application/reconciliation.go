package application

import (
	"context"
	"errors"
	"time"

	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
	orderapp "github.com/dujiao-next/internal/modules/order/application"
	ordercontract "github.com/dujiao-next/internal/modules/order/contract"
	orderdomain "github.com/dujiao-next/internal/modules/order/domain"
)

var ErrReconciliationLookup = errors.New("kafuda reconciliation lookup failed")

const reconciliationLookupLease = time.Minute

// ReconcileUnknownOrder verifies a supplier-provided numeric ID against the
// original attempt before applying it through the regular order transaction.
func ReconcileUnknownOrder(ctx context.Context, store ordercontract.Store, gateway kafudacontract.Gateway, childID, attemptID uint, upstreamID uint64, pollInterval time.Duration) (completed bool, err error) {
	if pollInterval <= 0 || gateway == nil || childID == 0 || attemptID == 0 || upstreamID == 0 {
		return false, orderapp.ErrOrderStatusInvalid
	}
	var leaseUntil time.Time
	err = withLockedDirectOrder(store, childID, func(tx ordercontract.Transaction, parent *orderdomain.Order) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		records := tx.KafudaGPT()
		record, err := records.GetRechargeForUpdate(childID)
		if err != nil {
			return err
		}
		if !isOpenPaidShopOrder(parent.Status) || record == nil || record.Stage != kafudadomain.StageManualReview ||
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
		now := time.Now().UTC()
		if attempt.KnownOrderPollLeaseUntil != nil && attempt.KnownOrderPollLeaseUntil.After(now) {
			return orderapp.ErrOrderStatusInvalid
		}
		// Use the same lookup lease as numeric polling. If a late create
		// response binds an ID, the worker waits for this lookup to finish.
		leaseUntil = now.Add(reconciliationLookupLease).Truncate(time.Microsecond)
		attempt.KnownOrderPollLeaseUntil = &leaseUntil
		return records.SaveAttempt(attempt)
	})
	if err != nil {
		return false, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, releaseReconciliationLease(store, childID, attemptID, leaseUntil))
		}
	}()
	lookupCtx, cancel := context.WithDeadline(ctx, leaseUntil)
	defer cancel()
	lookupStarted := time.Now()
	result, err := gateway.GetOrder(lookupCtx, upstreamID)
	if err != nil || result == nil {
		return false, ErrReconciliationLookup
	}
	if result.ID != upstreamID {
		return false, orderapp.ErrOrderStatusInvalid
	}
	// Applying the result checks reference, product, plan and country in the
	// transaction, and prevents binding a different ID after a concurrent lookup.
	startedAt := lookupStarted.UTC()
	receivedAt := startedAt.Add(time.Since(lookupStarted))
	completed, err = applyResult(store, childID, attemptID, *result, startedAt, receivedAt, pollInterval, 0, leaseUntil)
	if errors.Is(err, kafudadomain.ErrInvalidTransition) || errors.Is(err, kafudadomain.ErrConflict) {
		return false, orderapp.ErrOrderStatusInvalid
	}
	return completed, err
}

func releaseReconciliationLease(store ordercontract.Store, childID, attemptID uint, leaseUntil time.Time) error {
	return store.WithinTransaction(func(tx ordercontract.Transaction) error {
		records := tx.KafudaGPT()
		attempt, err := records.GetAttemptForUpdate(attemptID)
		if err != nil || attempt == nil || attempt.OrderID != childID ||
			attempt.KnownOrderPollLeaseUntil == nil || !attempt.KnownOrderPollLeaseUntil.Equal(leaseUntil) {
			return err
		}
		attempt.KnownOrderPollLeaseUntil = nil
		return records.SaveAttempt(attempt)
	})
}
