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
	// Select against the locked checkout snapshot and commit the quote and
	// reference before sending the supplier request.
	const submissionLease = time.Minute
	request, expiresAt, err := records.PrepareCheckedSubmission(ctx, attempt.ID, attempt.OrderID, attempt.Reference, check, submissionLease)
	if err != nil {
		return false, err
	}
	submitCtx, cancelSubmit := context.WithDeadline(ctx, expiresAt)
	interrupted := submitCtx.Err()
	checkedAt := time.Now().UTC()
	quoteExpired := !checkedAt.Before(expiresAt)
	if quoteExpired || interrupted != nil {
		cancelSubmit()
		recoveryCtx, cancelRecovery := context.WithTimeout(context.WithoutCancel(ctx), submissionRecoveryTimeout)
		defer cancelRecovery()
		if quoteExpired {
			if err := records.ExpireUndispatchedQuote(recoveryCtx, attempt.ID, request.QuoteID, request.Reference, expiresAt, checkedAt); err != nil {
				return false, err
			}
			return false, kafudaapp.ErrQuoteExpired
		}
		return false, errors.Join(interrupted, records.InterruptUndispatchedSubmission(recoveryCtx, attempt.ID, request.QuoteID, request.Reference, checkedAt))
	}
	requestStarted := time.Now()
	result, err := gateway.CreateOrder(submitCtx, request)
	cancelSubmit()
	var rejectionReason string
	switch {
	case err == nil && result != nil:
		startedAt := requestStarted.UTC()
		receivedAt := startedAt.Add(time.Since(requestStarted))
		completed, applyErr := ApplyResult(orders, attempt.OrderID, attempt.ID, *result, startedAt, receivedAt, pollInterval)
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
		if rejectErr := records.RejectOrderSubmission(recoveryCtx, attempt.ID, request.QuoteID, request.Reference, rejectionReason, observedAt); rejectErr != nil {
			return false, rejectErr
		}
		return false, err
	}
	// When the request may have succeeded upstream, keep the original attempt.
	// The persisted submitting lease is recoverable if this update fails.
	return false, errors.Join(err, records.MarkUnknown(recoveryCtx, attempt.ID, observedAt, observedAt.Add(pollInterval)))
}
