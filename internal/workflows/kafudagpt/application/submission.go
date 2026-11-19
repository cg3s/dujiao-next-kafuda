package application

import (
	"context"
	"errors"
	"time"

	kafudaapp "github.com/dujiao-next/internal/modules/kafudagpt/application"
	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
	ordercontract "github.com/dujiao-next/internal/modules/order/contract"
)

var errCreateResultMissing = errors.New("kafuda create order result missing")

const submissionRecoveryTimeout = 5 * time.Second

// SubmitCheckedQuote consumes a server-side check result and a newly reserved
// attempt. The caller should enqueue completion mail when completed is true.
func SubmitCheckedQuote(ctx context.Context, orders ordercontract.Store, records *kafudaapp.Service, gateway kafudacontract.Gateway, reservation *kafudaapp.Reservation, check kafudacontract.CheckResult, pollInterval time.Duration) (bool, error) {
	if pollInterval <= 0 || reservation == nil || !reservation.Created || reservation.Attempt == nil || reservation.Recharge == nil ||
		reservation.Attempt.OrderID != reservation.Recharge.OrderID {
		return false, kafudadomain.ErrInvalidTransition
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	records = WithShopOrderTransitions(records, orders)
	attempt := reservation.Attempt
	now := time.Now().UTC()
	quote, err := records.CheckQuote(ctx, attempt.ID, check, reservation.Recharge.Snapshot, now)
	if err != nil {
		return false, err
	}

	// Persist the quote and reference before an upstream request can escape.
	const submissionLease = time.Minute
	if err := records.MarkSubmitting(ctx, attempt.ID, quote.QuoteID, now, now.Add(submissionLease)); err != nil {
		return false, err
	}
	submitCtx, cancelSubmit := context.WithDeadline(ctx, quote.ExpiresAt)
	if checkedAt := time.Now().UTC(); !checkedAt.Before(quote.ExpiresAt) {
		cancelSubmit()
		recoveryCtx, cancelRecovery := context.WithTimeout(context.WithoutCancel(ctx), submissionRecoveryTimeout)
		defer cancelRecovery()
		if err := records.ExpireUndispatchedQuote(recoveryCtx, attempt.ID, quote.QuoteID, attempt.Reference, quote.ExpiresAt, checkedAt); err != nil {
			return false, err
		}
		return false, kafudaapp.ErrQuoteExpired
	}
	result, err := gateway.CreateOrder(submitCtx, kafudacontract.CreateOrderRequest{
		QuoteID: quote.QuoteID, Reference: attempt.Reference,
	})
	cancelSubmit()
	var rejectionReason string
	switch {
	case err == nil && result != nil:
		observedAt := time.Now().UTC()
		completed, applyErr := ApplyResult(orders, attempt.OrderID, attempt.ID, *result, observedAt, observedAt, pollInterval)
		if applyErr == nil {
			return completed, nil
		}
		err = applyErr
	case err == nil:
		err = errCreateResultMissing
	case result == nil && errors.Is(err, kafudacontract.ErrCreateQuoteExpired):
		rejectionReason = kafudadomain.ReasonQuoteExpired
		err = kafudaapp.ErrQuoteExpired
	case result == nil && errors.Is(err, kafudacontract.ErrMerchantBalanceInsufficient):
		rejectionReason = kafudadomain.ReasonMerchantBalanceInsufficient
	}

	recoveryCtx, cancelRecovery := context.WithTimeout(context.WithoutCancel(ctx), submissionRecoveryTimeout)
	defer cancelRecovery()
	observedAt := time.Now().UTC()
	if rejectionReason != "" {
		if rejectErr := records.RejectOrderSubmission(recoveryCtx, attempt.ID, quote.QuoteID, attempt.Reference, rejectionReason, observedAt); rejectErr != nil {
			return false, rejectErr
		}
		return false, err
	}
	// When the request may have succeeded upstream, keep the original attempt.
	// The persisted submitting lease is recoverable if this update fails.
	return false, errors.Join(err, records.MarkUnknown(recoveryCtx, attempt.ID, observedAt, observedAt.Add(pollInterval)))
}
