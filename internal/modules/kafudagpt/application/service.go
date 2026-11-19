package application

import (
	"context"
	"errors"
	"strings"
	"time"

	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"

	"github.com/google/uuid"
)

// Service owns only direct-recharge records. Calling workflows must authorize
// the buyer, tenant, paid order and actual fulfillment child before invoking it.
// Completion of the commerce order must share a transaction with the final
// recharge update; this service does not independently mark a shop order paid.
type Service struct {
	store              kafudacontract.Store
	attemptTransaction func(context.Context, uint, func(kafudacontract.Transaction) error) error
}

var errNoRecordChange = errors.New("kafuda recharge unchanged")

func New(store kafudacontract.Store) *Service { return &Service{store: store} }

// WithAttemptTransaction binds attempt mutations to the caller's order transaction.
// The returned service shares the same read store without changing the original.
func (s *Service) WithAttemptTransaction(run func(context.Context, uint, func(kafudacontract.Transaction) error) error) *Service {
	return &Service{store: s.store, attemptTransaction: run}
}

// Prepare preserves the first paid-order snapshot, including administrator cost.
// It performs no upstream call and does not reserve any card or manual stock.
func (s *Service) Prepare(ctx context.Context, orderID uint, snapshot kafudadomain.Snapshot, now time.Time) (*kafudadomain.Recharge, error) {
	if orderID == 0 || now.IsZero() {
		return nil, kafudadomain.ErrInvalidSnapshot
	}
	if err := snapshot.Validate(); err != nil {
		return nil, err
	}
	var record *kafudadomain.Recharge
	err := s.store.WithinTransaction(ctx, func(tx kafudacontract.Transaction) error {
		var err error
		record, err = tx.GetRechargeForUpdate(orderID)
		if err != nil || record != nil {
			return err
		}
		record = &kafudadomain.Recharge{OrderID: orderID, Snapshot: snapshot, Stage: kafudadomain.StageWaiting, Version: 1, CreatedAt: now, UpdatedAt: now}
		return tx.CreateRecharge(record)
	})
	return record, err
}

type Reservation struct {
	Recharge *kafudadomain.Recharge
	Attempt  *kafudadomain.Attempt
	Created  bool
}

// Reserve returns the existing live attempt on double-click or request replay.
// Only Created=true permits a caller to check Session and start a new request.
// leaseUntil permits recovery of a process that exits during credential checking.
func (s *Service) Reserve(ctx context.Context, orderID uint, now, leaseUntil time.Time) (*Reservation, error) {
	var reservation *Reservation
	err := s.store.WithinTransaction(ctx, func(tx kafudacontract.Transaction) error {
		var err error
		reservation, err = ReserveInTransaction(tx, orderID, now, leaseUntil)
		return err
	})
	if err != nil {
		return nil, err
	}
	return reservation, nil
}

// ReserveInTransaction lets commerce validate and lock the paid order before
// creating an attempt in the same transaction.
func ReserveInTransaction(tx kafudacontract.Transaction, orderID uint, now, leaseUntil time.Time) (*Reservation, error) {
	if now.IsZero() || !leaseUntil.After(now) {
		return nil, kafudadomain.ErrInvalidTransition
	}
	result := &Reservation{}
	record, err := tx.GetRechargeForUpdate(orderID)
	if err != nil {
		return nil, err
	}
	if record == nil {
		return nil, kafudadomain.ErrNotFound
	}
	result.Recharge = record
	if record.CurrentAttemptID != nil {
		attempt, err := tx.GetAttemptForUpdate(*record.CurrentAttemptID)
		if err != nil {
			return nil, err
		}
		if attempt == nil {
			return nil, kafudadomain.ErrConflict
		}
		if attempt.Status.Active() {
			result.Attempt = attempt
			return result, nil
		}
	}
	if record.Stage != kafudadomain.StageWaiting {
		return nil, kafudadomain.ErrInvalidTransition
	}
	activeID := orderID
	attempt := &kafudadomain.Attempt{
		OrderID: orderID, ActiveOrderID: &activeID, Sequence: record.AttemptCount + 1,
		Reference: "kgpt-" + uuid.NewString(), Status: kafudadomain.AttemptChecking,
		NextPollAt: &leaseUntil, CreatedAt: now, UpdatedAt: now,
	}
	if err := tx.CreateAttempt(attempt); err != nil {
		return nil, err
	}
	record.CurrentAttemptID, record.AttemptCount = &attempt.ID, attempt.Sequence
	record.Stage, record.ReasonCode, record.UpdatedAt = kafudadomain.StageChecking, "", now
	if err := tx.SaveRecharge(record); err != nil {
		return nil, err
	}
	result.Attempt, result.Created = attempt, true
	return result, nil
}

// MarkSubmitting records a submission for which no quote breakdown is available.
func (s *Service) MarkSubmitting(ctx context.Context, attemptID uint, quoteID string, now, nextPoll time.Time) error {
	return s.markSubmitting(ctx, attemptID, quoteID, nil, now, nextPoll)
}

// MarkSubmittingWithQuote commits both the quote and the non-credential
// submission identifiers before the caller can send POST /gpt/orders.
func (s *Service) MarkSubmittingWithQuote(ctx context.Context, attemptID uint, quote kafudacontract.Quote, now, nextPoll time.Time) error {
	return s.markSubmitting(ctx, attemptID, quote.QuoteID, &quote, now, nextPoll)
}

func (s *Service) markSubmitting(ctx context.Context, attemptID uint, quoteID string, quote *kafudacontract.Quote, now, nextPoll time.Time) error {
	if strings.TrimSpace(quoteID) == "" || len(quoteID) > 128 || now.IsZero() || !nextPoll.After(now) {
		return kafudadomain.ErrInvalidTransition
	}
	return s.change(ctx, attemptID, func(record *kafudadomain.Recharge, attempt *kafudadomain.Attempt) error {
		if !hasLiveCheck(record, attempt, now) || attempt.QuoteID != "" {
			return kafudadomain.ErrInvalidTransition
		}
		var costs *kafudadomain.QuoteCosts
		if quote != nil {
			costs = autoCardQuoteCosts(*quote, record.Snapshot)
			if costs == nil || quote.Plan != record.Snapshot.Plan || quote.Status != "available" ||
				!allowedKind(quote.Kind, record.Snapshot.AllowedKinds) || !quote.ExpiresAt.After(now) {
				return ErrQuotePolicy
			}
		}
		setSubmitting(record, attempt, quoteID, costs, now, nextPoll)
		return nil
	})
}

func setSubmitting(record *kafudadomain.Recharge, attempt *kafudadomain.Attempt, quoteID string, costs *kafudadomain.QuoteCosts, now, nextPoll time.Time) {
	attempt.Status, attempt.QuoteID, attempt.QuoteCosts = kafudadomain.AttemptSubmitting, quoteID, costs
	attempt.UpdatedAt, attempt.NextPollAt = now, &nextPoll
	record.Stage, record.UpdatedAt = kafudadomain.StageSubmitting, now
}

// ExpireUndispatchedQuote may only be called before the gateway was invoked.
// Preserve the committed quote and reference while releasing the active slot.
func (s *Service) ExpireUndispatchedQuote(ctx context.Context, attemptID uint, quoteID, reference string, expiresAt, now time.Time) error {
	if expiresAt.IsZero() || now.Before(expiresAt) {
		return kafudadomain.ErrInvalidTransition
	}
	return s.releaseOrderSubmission(ctx, attemptID, quoteID, reference, kafudadomain.StageWaiting, kafudadomain.ReasonQuoteExpired, now)
}

// InterruptUndispatchedSubmission releases a canceled submission before the
// gateway was invoked. It preserves the prepared quote and reference.
func (s *Service) InterruptUndispatchedSubmission(ctx context.Context, attemptID uint, quoteID, reference string, now time.Time) error {
	return s.releaseOrderSubmission(ctx, attemptID, quoteID, reference, kafudadomain.StageWaiting, kafudadomain.ReasonSubmissionInterrupted, now)
}

// RejectOrderSubmission releases a definitively rejected submission.
func (s *Service) RejectOrderSubmission(ctx context.Context, attemptID uint, quoteID, reference, reason string, now time.Time) error {
	var stage kafudadomain.Stage
	switch reason {
	case kafudadomain.ReasonQuoteExpired:
		stage = kafudadomain.StageWaiting
	case kafudadomain.ReasonMerchantBalanceInsufficient:
		stage = kafudadomain.StageManualDelivery
	default:
		return kafudadomain.ErrInvalidTransition
	}
	return s.releaseOrderSubmission(ctx, attemptID, quoteID, reference, stage, reason, now)
}

// releaseOrderSubmission leaves the active slot intact if recovery already
// changed the attempt or a supplier order has been identified.
func (s *Service) releaseOrderSubmission(ctx context.Context, attemptID uint, quoteID, reference string, stage kafudadomain.Stage, reason string, now time.Time) error {
	if strings.TrimSpace(quoteID) == "" || reference == "" || now.IsZero() {
		return kafudadomain.ErrInvalidTransition
	}
	return s.change(ctx, attemptID, func(record *kafudadomain.Recharge, attempt *kafudadomain.Attempt) error {
		if record.Stage != kafudadomain.StageSubmitting || record.CompletedAt != nil || attempt.Status != kafudadomain.AttemptSubmitting ||
			attempt.QuoteID != quoteID || attempt.Reference != reference || attempt.UpstreamOrderID != nil || attempt.NextPollAt == nil ||
			attempt.ActiveOrderID == nil || *attempt.ActiveOrderID != attempt.OrderID ||
			now.Before(attempt.UpdatedAt) || now.Before(record.UpdatedAt) {
			return kafudadomain.ErrInvalidTransition
		}
		attempt.Status, attempt.ActiveOrderID, attempt.NextPollAt, attempt.UpdatedAt = kafudadomain.AttemptFailed, nil, nil, now
		record.Stage, record.ReasonCode, record.UpdatedAt = stage, reason, now
		return nil
	})
}

// CheckRejected applies only before a potentially chargeable request was sent.
// Reason is classified locally; never persist upstream messages or Session.
func (s *Service) CheckRejected(ctx context.Context, attemptID uint, retryable bool, now time.Time) error {
	if retryable {
		return s.rejectCheck(ctx, attemptID, kafudadomain.StageWaiting, "session_invalid", now)
	}
	return s.rejectCheck(ctx, attemptID, kafudadomain.StageManualDelivery, "check_rejected", now)
}

func (s *Service) rejectCheck(ctx context.Context, attemptID uint, stage kafudadomain.Stage, reason string, now time.Time) error {
	return s.change(ctx, attemptID, func(record *kafudadomain.Recharge, attempt *kafudadomain.Attempt) error {
		if !hasLiveCheck(record, attempt, now) {
			return kafudadomain.ErrInvalidTransition
		}
		setCheckRejected(record, attempt, stage, reason, now)
		return nil
	})
}

func setCheckRejected(record *kafudadomain.Recharge, attempt *kafudadomain.Attempt, stage kafudadomain.Stage, reason string, now time.Time) {
	attempt.Status, attempt.ActiveOrderID, attempt.NextPollAt, attempt.UpdatedAt = kafudadomain.AttemptFailed, nil, nil, now
	record.Stage, record.ReasonCode, record.UpdatedAt = stage, reason, now
}

func hasLiveCheck(record *kafudadomain.Recharge, attempt *kafudadomain.Attempt, now time.Time) bool {
	return record.Stage == kafudadomain.StageChecking && attempt.Status == kafudadomain.AttemptChecking &&
		attempt.NextPollAt != nil && !now.IsZero() && now.Before(*attempt.NextPollAt) && !now.Before(attempt.UpdatedAt)
}

func (s *Service) MarkUnknown(ctx context.Context, attemptID uint, now, nextPoll time.Time) error {
	if now.IsZero() || !nextPoll.After(now) {
		return kafudadomain.ErrInvalidTransition
	}
	return s.change(ctx, attemptID, func(record *kafudadomain.Recharge, attempt *kafudadomain.Attempt) error {
		if !attempt.Status.CanTransition(kafudadomain.AttemptReview) {
			return kafudadomain.ErrInvalidTransition
		}
		// A delayed submission failure cannot replace a verified supplier order.
		if attempt.UpstreamOrderID != nil || now.Before(attempt.UpdatedAt) {
			return kafudadomain.ErrConflict
		}
		attempt.Status, attempt.NextPollAt, attempt.UpdatedAt = kafudadomain.AttemptReview, &nextPoll, now
		record.Stage, record.UpdatedAt = kafudadomain.StageManualReview, now
		if record.ReasonCode != kafudadomain.ReasonCostLimitExceeded {
			record.ReasonCode = "outcome_unknown"
		}
		return nil
	})
}

// RecordResult consumes a normalized query/create response, not arbitrary text.
// A terminal duplicate is a no-op. Old terminal events never reopen a recharge.
func (s *Service) RecordResult(ctx context.Context, attemptID uint, result kafudacontract.OrderResult, now, nextPoll time.Time) error {
	return s.change(ctx, attemptID, resultUpdate(result, now, now, nextPoll, 0))
}

// RecordResultInTransaction allows the caller to complete the shop order in
// the same transaction. The returned flag is true only for a fresh completion.
func RecordResultInTransaction(tx kafudacontract.Transaction, orderID, attemptID uint, result kafudacontract.OrderResult, startedAt, now, nextPoll time.Time, reviewAfter time.Duration) (bool, error) {
	update := resultUpdate(result, startedAt, now, nextPoll, reviewAfter)
	completed := false
	err := changeInTransaction(tx, orderID, attemptID, func(record *kafudadomain.Recharge, attempt *kafudadomain.Attempt) error {
		if err := update(record, attempt); err != nil {
			return err
		}
		completed = record.Stage == kafudadomain.StageCompleted
		return nil
	})
	return completed && err == nil, err
}

func resultUpdate(result kafudacontract.OrderResult, startedAt, now, nextPoll time.Time, reviewAfter time.Duration) func(*kafudadomain.Recharge, *kafudadomain.Attempt) error {
	return func(record *kafudadomain.Recharge, attempt *kafudadomain.Attempt) error {
		if result.ID == 0 || result.Reference != attempt.Reference || result.Product != record.Snapshot.Product || result.Plan != record.Snapshot.Plan || result.Country != record.Snapshot.Country {
			return kafudadomain.ErrInvalidTransition
		}
		if attempt.UpstreamOrderID != nil && *attempt.UpstreamOrderID != result.ID {
			return kafudadomain.ErrInvalidTransition
		}
		reported := kafudadomain.AttemptStatus(result.Status)
		next, allowed := supplierResultStatus(attempt.Status, reported)
		if !allowed {
			return kafudadomain.ErrInvalidTransition
		}
		if !attempt.Status.Active() {
			return errNoRecordChange
		}
		if startedAt.IsZero() || now.IsZero() || now.Before(startedAt) {
			return kafudadomain.ErrInvalidTransition
		}
		if result.ChargedUSDCents != nil && *result.ChargedUSDCents < 0 {
			return kafudadomain.ErrInvalidTransition
		}
		if next.Active() {
			if !nextPoll.After(now) {
				return kafudadomain.ErrInvalidTransition
			}
			// Local recovery can move an unidentified submission to review before the
			// first supplier response arrives. That response can still bind its ID.
			firstIdentityAfterRecovery := attempt.UpstreamOrderID == nil && attempt.Status == kafudadomain.AttemptReview &&
				record.Stage == kafudadomain.StageManualReview && record.ReasonCode == "outcome_unknown"
			if startedAt.Before(attempt.UpdatedAt) && !firstIdentityAfterRecovery {
				return kafudadomain.ErrConflict
			}
			if now.Before(attempt.UpdatedAt) {
				nextPoll = attempt.UpdatedAt.Add(nextPoll.Sub(now))
			}
		}
		if now.Before(attempt.UpdatedAt) {
			// Keep timestamps monotonic for accepted late results.
			now = attempt.UpdatedAt
		}
		attempt.UpstreamOrderID, attempt.Status, attempt.UpdatedAt = &result.ID, next, now
		// The gateway already masks account_email. Never accept a full email at
		// the application boundary, even from an alternative gateway adapter.
		attempt.MaskedEmail = ""
		if result.MaskedEmail != "" && len(result.MaskedEmail) <= 32 && !containsFullEmail(result.MaskedEmail) {
			attempt.MaskedEmail = result.MaskedEmail
		}
		if result.ChargedUSDCents != nil {
			attempt.ChargedUSDCents = result.ChargedUSDCents
		}
		if result.RenewalCancelled != nil {
			attempt.RenewalCancelled = result.RenewalCancelled
		}
		attempt.NextPollAt = nil
		if next.Active() {
			attempt.NextPollAt = &nextPoll
		} else {
			attempt.ActiveOrderID = nil
		}
		record.UpdatedAt = now
		// An over-cap charge needs review even if already completed upstream.
		// A terminal failure or cancellation still permits controlled after-sales.
		if record.ReasonCode == kafudadomain.ReasonCostLimitExceeded ||
			(attempt.ChargedUSDCents != nil && *attempt.ChargedUSDCents > record.Snapshot.MaxCostCents) {
			record.Stage, record.ReasonCode = kafudadomain.StageManualReview, kafudadomain.ReasonCostLimitExceeded
			if next == kafudadomain.AttemptFailed || next == kafudadomain.AttemptCancelled {
				record.Stage = kafudadomain.StageManualDelivery
			}
			return nil
		}
		switch reported {
		case kafudadomain.AttemptSubmitting, kafudadomain.AttemptProcessing:
			if record.Stage == kafudadomain.StageManualReview {
				return nil
			}
			if reviewAfter > 0 && now.Sub(attempt.CreatedAt) >= reviewAfter {
				record.Stage, record.ReasonCode = kafudadomain.StageManualReview, kafudadomain.ReasonProcessingDeadlineExceeded
			} else {
				record.Stage = kafudadomain.StageProcessing
			}
		case kafudadomain.AttemptReview:
			record.Stage, record.ReasonCode = kafudadomain.StageManualReview, "upstream_review"
		case kafudadomain.AttemptFailed, kafudadomain.AttemptCancelled:
			record.Stage, record.ReasonCode = kafudadomain.StageManualDelivery, "upstream_"+string(next)
		case kafudadomain.AttemptCompleted:
			record.Stage, record.ReasonCode, record.CompletedAt = kafudadomain.StageCompleted, "", &now
		}
		return nil
	}
}

func supplierResultStatus(current, reported kafudadomain.AttemptStatus) (kafudadomain.AttemptStatus, bool) {
	// A supplier can still be submitting after the local attempt entered review.
	// Accept the verified identity and schedule another lookup while retaining review.
	if current == kafudadomain.AttemptReview && reported == kafudadomain.AttemptSubmitting {
		return current, true
	}
	return reported, reported != kafudadomain.AttemptChecking && current.CanTransition(reported)
}

func (s *Service) change(ctx context.Context, attemptID uint, update func(*kafudadomain.Recharge, *kafudadomain.Attempt) error) error {
	// Read the immutable order ID before taking locks, then consistently lock
	// recharge before attempt to avoid lock-order inversions on PostgreSQL.
	initial, err := s.store.GetAttempt(ctx, attemptID)
	if err != nil {
		return err
	}
	if initial == nil {
		return kafudadomain.ErrNotFound
	}
	run := func(tx kafudacontract.Transaction) error {
		return changeInTransaction(tx, initial.OrderID, attemptID, update)
	}
	if s.attemptTransaction != nil {
		return s.attemptTransaction(ctx, initial.OrderID, run)
	}
	return s.store.WithinTransaction(ctx, run)
}

func changeInTransaction(tx kafudacontract.Transaction, orderID, attemptID uint, update func(*kafudadomain.Recharge, *kafudadomain.Attempt) error) error {
	record, err := tx.GetRechargeForUpdate(orderID)
	if err != nil {
		return err
	}
	if record == nil || record.CurrentAttemptID == nil || *record.CurrentAttemptID != attemptID {
		return kafudadomain.ErrConflict
	}
	attempt, err := tx.GetAttemptForUpdate(attemptID)
	if err != nil {
		return err
	}
	if attempt == nil {
		return kafudadomain.ErrNotFound
	}
	if attempt.OrderID != orderID {
		return kafudadomain.ErrConflict
	}
	if err := update(record, attempt); err == errNoRecordChange {
		return nil
	} else if err != nil {
		return err
	}
	if err := tx.SaveAttempt(attempt); err != nil {
		return err
	}
	return tx.SaveRecharge(record)
}

func containsFullEmail(value string) bool {
	// The sole accepted gateway mask is the first rune followed by ***@***.
	runes := []rune(value)
	return len(runes) != 8 || string(runes[1:]) != "***@***"
}
