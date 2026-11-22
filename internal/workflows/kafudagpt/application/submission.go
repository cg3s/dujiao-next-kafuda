package application

import (
	"context"
	"errors"
	"net/http"
	"time"

	kafudaapp "github.com/dujiao-next/internal/modules/kafudagpt/application"
	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
	ordercontract "github.com/dujiao-next/internal/modules/order/contract"
)

var errCreateResultMissing = errors.New("kafuda create order result missing")
var ErrBuyerCheckRetry = errors.New("kafuda buyer check temporarily unavailable")

const submissionRecoveryTimeout = 5 * time.Second

// SubmitBuyerSession reserves the paid order as a whole before checking the
// supplied session. A repeated request never checks or submits an active attempt.
func SubmitBuyerSession(ctx context.Context, orders ordercontract.Store, records *kafudaapp.Service, gateway kafudacontract.Gateway, orderNo string, buyerID uint, session string, pollInterval time.Duration) (uint, bool, error) {
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	now := time.Now().UTC()
	reservation, err := ReserveBuyerAttempt(orders, ordercontract.TenantScope{}, orderNo, buyerID, now, now.Add(time.Minute))
	if err != nil {
		return 0, false, err
	}
	return submitReservedSession(ctx, orders, records, gateway, reservation, session, pollInterval)
}

// SubmitGuestSession reserves the paid guest order verified by guest email and
// order password before checking and submitting the supplied session.
func SubmitGuestSession(ctx context.Context, orders ordercontract.Store, records *kafudaapp.Service, gateway kafudacontract.Gateway, orderNo, email, password, session string, pollInterval time.Duration) (uint, bool, error) {
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	now := time.Now().UTC()
	reservation, err := ReserveGuestAttempt(orders, ordercontract.TenantScope{}, orderNo, email, password, now, now.Add(time.Minute))
	if err != nil {
		return 0, false, err
	}
	return submitReservedSession(ctx, orders, records, gateway, reservation, session, pollInterval)
}

func submitReservedSession(ctx context.Context, orders ordercontract.Store, records *kafudaapp.Service, gateway kafudacontract.Gateway, reservation *kafudaapp.Reservation, session string, pollInterval time.Duration) (uint, bool, error) {
	if !reservation.Created {
		return 0, false, nil
	}
	snapshot := reservation.Recharge.Snapshot
	checkCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	check, err := gateway.Check(checkCtx, kafudacontract.CheckRequest{
		Country: snapshot.Country, PayMode: snapshot.PayMode, ProductCode: snapshot.ProductCode, Session: session,
	})
	cancel()
	if err == nil && check != nil && ctx.Err() == nil {
		completed, err := SubmitCheckedQuote(ctx, orders, records, gateway, reservation, *check, pollInterval)
		return reservation.Recharge.OrderID, completed, err
	}

	recoverCtx, cancelRecovery := context.WithTimeout(context.WithoutCancel(ctx), submissionRecoveryTimeout)
	defer cancelRecovery()
	observedAt := time.Now().UTC()
	attemptID := reservation.Attempt.ID
	var upstream *kafudacontract.APIError
	switch {
	case errors.Is(err, kafudacontract.ErrCheckSessionInvalid):
		return 0, false, errors.Join(err, records.CheckRejected(recoverCtx, attemptID, true, observedAt))
	case errors.As(err, &upstream) && upstream.HTTPStatus == http.StatusBadRequest:
		// A definitive business rejection occurs before any chargeable request.
		return 0, false, records.CheckRejected(recoverCtx, attemptID, false, observedAt)
	default:
		return 0, false, errors.Join(ErrBuyerCheckRetry, records.InterruptCheck(recoverCtx, attemptID, observedAt))
	}
}

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
	var unappliedResult *kafudacontract.OrderResult
	switch {
	case err == nil && result != nil:
		startedAt := requestStarted.UTC()
		receivedAt := startedAt.Add(time.Since(requestStarted))
		completed, applyErr := ApplyResult(orders, attempt.OrderID, attempt.ID, *result, startedAt, receivedAt, pollInterval)
		if applyErr == nil {
			return completed, nil
		}
		err = applyErr
		unappliedResult = result
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
	if unappliedResult != nil {
		holdErr := records.HoldUnappliedSubmission(recoveryCtx, attempt.ID, request.QuoteID, *unappliedResult, observedAt, observedAt.Add(pollInterval))
		if holdErr == nil {
			return false, err
		}
		err = errors.Join(err, holdErr)
	}
	// When the request may have succeeded upstream, keep the original attempt.
	// The persisted submitting lease is recoverable if this update fails.
	return false, errors.Join(err, records.MarkUnknown(recoveryCtx, attempt.ID, observedAt, observedAt.Add(pollInterval)))
}
