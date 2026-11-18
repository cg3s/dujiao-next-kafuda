package application

import (
	"context"
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
type Service struct{ store kafudacontract.Store }

const costLimitExceeded = "cost_limit_exceeded"

func New(store kafudacontract.Store) *Service { return &Service{store: store} }

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
	if now.IsZero() || !leaseUntil.After(now) {
		return nil, kafudadomain.ErrInvalidTransition
	}
	result := &Reservation{}
	err := s.store.WithinTransaction(ctx, func(tx kafudacontract.Transaction) error {
		record, err := tx.GetRechargeForUpdate(orderID)
		if err != nil {
			return err
		}
		if record == nil {
			return kafudadomain.ErrNotFound
		}
		result.Recharge = record
		if record.CurrentAttemptID != nil {
			attempt, err := tx.GetAttemptForUpdate(*record.CurrentAttemptID)
			if err != nil {
				return err
			}
			if attempt == nil {
				return kafudadomain.ErrConflict
			}
			if attempt.Status.Active() {
				result.Attempt = attempt
				return nil
			}
		}
		if record.Stage != kafudadomain.StageWaiting {
			return kafudadomain.ErrInvalidTransition
		}
		activeID := orderID
		attempt := &kafudadomain.Attempt{
			OrderID: orderID, ActiveOrderID: &activeID, Sequence: record.AttemptCount + 1,
			Reference: "kgpt-" + uuid.NewString(), Status: kafudadomain.AttemptChecking,
			NextPollAt: &leaseUntil, CreatedAt: now, UpdatedAt: now,
		}
		if err := tx.CreateAttempt(attempt); err != nil {
			return err
		}
		record.CurrentAttemptID, record.AttemptCount = &attempt.ID, attempt.Sequence
		record.Stage, record.ReasonCode, record.UpdatedAt = kafudadomain.StageChecking, "", now
		if err := tx.SaveRecharge(record); err != nil {
			return err
		}
		result.Attempt, result.Created = attempt, true
		return nil
	})
	return result, err
}

// MarkSubmitting is committed before calling POST /gpt/orders. Its QuoteID and
// Reference preserve the exact non-credential request for safe reconciliation.
func (s *Service) MarkSubmitting(ctx context.Context, attemptID uint, quoteID string, now, nextPoll time.Time) error {
	if strings.TrimSpace(quoteID) == "" || len(quoteID) > 128 || now.IsZero() || !nextPoll.After(now) {
		return kafudadomain.ErrInvalidTransition
	}
	return s.change(ctx, attemptID, func(record *kafudadomain.Recharge, attempt *kafudadomain.Attempt) error {
		if !hasLiveCheck(record, attempt, now) {
			return kafudadomain.ErrInvalidTransition
		}
		attempt.Status, attempt.QuoteID, attempt.UpdatedAt = kafudadomain.AttemptSubmitting, quoteID, now
		attempt.NextPollAt = &nextPoll
		record.Stage, record.UpdatedAt = kafudadomain.StageSubmitting, now
		return nil
	})
}

// CheckRejected applies only before a potentially chargeable request was sent.
// Reason is classified locally; never persist upstream messages or Session.
func (s *Service) CheckRejected(ctx context.Context, attemptID uint, retryable bool, now time.Time) error {
	return s.change(ctx, attemptID, func(record *kafudadomain.Recharge, attempt *kafudadomain.Attempt) error {
		if !hasLiveCheck(record, attempt, now) {
			return kafudadomain.ErrInvalidTransition
		}
		attempt.Status, attempt.ActiveOrderID, attempt.NextPollAt, attempt.UpdatedAt = kafudadomain.AttemptFailed, nil, nil, now
		record.Stage, record.ReasonCode = kafudadomain.StageManualDelivery, "check_rejected"
		if retryable {
			record.Stage, record.ReasonCode = kafudadomain.StageWaiting, "session_invalid"
		}
		record.UpdatedAt = now
		return nil
	})
}

func hasLiveCheck(record *kafudadomain.Recharge, attempt *kafudadomain.Attempt, now time.Time) bool {
	return record.Stage == kafudadomain.StageChecking && attempt.Status == kafudadomain.AttemptChecking &&
		attempt.NextPollAt != nil && !now.IsZero() && now.Before(*attempt.NextPollAt) && !now.Before(attempt.UpdatedAt)
}

func (s *Service) MarkUnknown(ctx context.Context, attemptID uint, now, nextPoll time.Time) error {
	if !nextPoll.After(now) {
		return kafudadomain.ErrInvalidTransition
	}
	return s.change(ctx, attemptID, func(record *kafudadomain.Recharge, attempt *kafudadomain.Attempt) error {
		if !attempt.Status.CanTransition(kafudadomain.AttemptReview) {
			return kafudadomain.ErrInvalidTransition
		}
		attempt.Status, attempt.NextPollAt, attempt.UpdatedAt = kafudadomain.AttemptReview, &nextPoll, now
		record.Stage, record.UpdatedAt = kafudadomain.StageManualReview, now
		if record.ReasonCode != costLimitExceeded {
			record.ReasonCode = "outcome_unknown"
		}
		return nil
	})
}

// RecordResult consumes a normalized query/create response, not arbitrary text.
// A terminal duplicate is a no-op. Old terminal events never reopen a recharge.
func (s *Service) RecordResult(ctx context.Context, attemptID uint, result kafudacontract.OrderResult, now, nextPoll time.Time) error {
	return s.change(ctx, attemptID, func(record *kafudadomain.Recharge, attempt *kafudadomain.Attempt) error {
		if result.ID == 0 || result.Reference != attempt.Reference || result.Product != record.Snapshot.Product || result.Plan != record.Snapshot.Plan || result.Country != record.Snapshot.Country {
			return kafudadomain.ErrInvalidTransition
		}
		if attempt.UpstreamOrderID != nil && *attempt.UpstreamOrderID != result.ID {
			return kafudadomain.ErrInvalidTransition
		}
		next := kafudadomain.AttemptStatus(result.Status)
		if next == kafudadomain.AttemptChecking || !attempt.Status.CanTransition(next) {
			return kafudadomain.ErrInvalidTransition
		}
		if !attempt.Status.Active() {
			return nil
		}
		if result.ChargedUSDCents != nil && *result.ChargedUSDCents < 0 {
			return kafudadomain.ErrInvalidTransition
		}
		if next.Active() && !nextPoll.After(now) {
			return kafudadomain.ErrInvalidTransition
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
		// Keep the upstream outcome for reconciliation. An over-cap charge
		// needs review even if the supplier reports a completed recharge.
		if record.ReasonCode == costLimitExceeded ||
			(attempt.ChargedUSDCents != nil && *attempt.ChargedUSDCents > record.Snapshot.MaxCostCents) {
			record.Stage, record.ReasonCode = kafudadomain.StageManualReview, costLimitExceeded
			return nil
		}
		switch next {
		case kafudadomain.AttemptSubmitting, kafudadomain.AttemptProcessing:
			if record.Stage != kafudadomain.StageManualReview {
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
	})
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
	return s.store.WithinTransaction(ctx, func(tx kafudacontract.Transaction) error {
		record, err := tx.GetRechargeForUpdate(initial.OrderID)
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
		if err := update(record, attempt); err != nil {
			return err
		}
		if err := tx.SaveAttempt(attempt); err != nil {
			return err
		}
		return tx.SaveRecharge(record)
	})
}

func containsFullEmail(value string) bool {
	// The sole accepted gateway mask is the first rune followed by ***@***.
	runes := []rune(value)
	return len(runes) != 8 || string(runes[1:]) != "***@***"
}
