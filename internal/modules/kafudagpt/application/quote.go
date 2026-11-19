package application

import (
	"context"
	"errors"
	"strings"
	"time"

	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
)

var (
	ErrQuoteUnavailable  = errors.New("kafuda purchased plan is unavailable")
	ErrQuoteExpired      = errors.New("kafuda quote has expired")
	ErrQuotePolicy       = errors.New("kafuda quote exceeds merchant policy")
	ErrQuoteRegionLocked = errors.New("kafuda account is region locked")
)

// PrepareCheckedSubmission selects the quote against the locked order snapshot
// and commits the billing request before it can be sent to the supplier.
func (s *Service) PrepareCheckedSubmission(ctx context.Context, attemptID, orderID uint, reference string, result kafudacontract.CheckResult, lease time.Duration) (kafudacontract.CreateOrderRequest, time.Time, error) {
	if attemptID == 0 || orderID == 0 || reference == "" || lease <= 0 {
		return kafudacontract.CreateOrderRequest{}, time.Time{}, kafudadomain.ErrInvalidTransition
	}
	var request kafudacontract.CreateOrderRequest
	var expiresAt time.Time
	var rejection error
	err := s.change(ctx, attemptID, func(record *kafudadomain.Recharge, attempt *kafudadomain.Attempt) error {
		if record.OrderID != orderID || attempt.Reference != reference {
			return kafudadomain.ErrConflict
		}
		// Time spent waiting for order or attempt locks counts against the lease.
		now := time.Now().UTC()
		if !hasLiveCheck(record, attempt, now) || attempt.QuoteID != "" {
			return kafudadomain.ErrInvalidTransition
		}
		quote, err := SelectQuote(result, record.Snapshot, now)
		if err != nil {
			rejection = err
			stage, reason := quoteRejection(err)
			setCheckRejected(record, attempt, stage, reason, now)
			return nil
		}
		setSubmitting(record, attempt, quote.QuoteID, autoCardQuoteCosts(*quote, record.Snapshot), now, now.Add(lease))
		request = kafudacontract.CreateOrderRequest{QuoteID: attempt.QuoteID, Reference: attempt.Reference}
		expiresAt = quote.ExpiresAt
		return nil
	})
	if err != nil || rejection != nil {
		return kafudacontract.CreateOrderRequest{}, time.Time{}, errors.Join(rejection, err)
	}
	return request, expiresAt, nil
}

func quoteRejection(err error) (kafudadomain.Stage, string) {
	stage, reason := kafudadomain.StageManualDelivery, kafudadomain.ReasonQuotePolicy
	switch {
	case errors.Is(err, ErrQuoteRegionLocked):
		reason = kafudadomain.ReasonQuoteRegionLocked
	case errors.Is(err, ErrQuoteExpired):
		stage, reason = kafudadomain.StageWaiting, kafudadomain.ReasonQuoteExpired
	case errors.Is(err, ErrQuoteUnavailable):
		reason = kafudadomain.ReasonQuoteUnavailable
	}
	return stage, reason
}

// SelectQuote matches only the paid order's plan and country. Local prices are
// intentionally absent from cost calculations: upstream costs are USD cents.
func SelectQuote(result kafudacontract.CheckResult, snapshot kafudadomain.Snapshot, now time.Time) (*kafudacontract.Quote, error) {
	if snapshot.Validate() != nil || result.Country != snapshot.Country || result.PayMode != snapshot.PayMode {
		return nil, ErrQuotePolicy
	}
	if result.RegionLocked {
		return nil, ErrQuoteRegionLocked
	}
	var selected *kafudacontract.Quote
	expired, missingExpiry := false, false
	for _, candidate := range result.Plans {
		if candidate.Plan != snapshot.Plan || candidate.Status != "available" {
			continue
		}
		if candidate.ExpiresAt.IsZero() {
			missingExpiry = true
			continue
		}
		if !candidate.ExpiresAt.After(now) {
			expired = true
			continue
		}
		if selected != nil || strings.TrimSpace(candidate.QuoteID) == "" || len(candidate.QuoteID) > 128 || strings.ContainsRune(candidate.QuoteID, '\x00') ||
			!allowedKind(candidate.Kind, snapshot.AllowedKinds) || autoCardQuoteCosts(candidate, snapshot) == nil {
			return nil, ErrQuotePolicy
		}
		quote := candidate
		selected = &quote
	}
	if selected == nil {
		if missingExpiry {
			return nil, ErrQuotePolicy
		}
		if expired {
			return nil, ErrQuoteExpired
		}
		return nil, ErrQuoteUnavailable
	}
	return selected, nil
}

func allowedKind(kind string, allowed []string) bool {
	for _, candidate := range allowed {
		if kind != "" && candidate == kind {
			return true
		}
	}
	return false
}

func autoCardQuoteCosts(quote kafudacontract.Quote, snapshot kafudadomain.Snapshot) *kafudadomain.QuoteCosts {
	card := quote.AutoCard
	if card == nil || card.ProductCode != snapshot.ProductCode {
		return nil
	}
	costs := kafudadomain.QuoteCosts{
		ServiceFeeCents: quote.PriceCents, CardOpenFeeCents: card.OpenFeeCents,
		CardLoadCents: card.LoadCents, TotalCents: card.TotalCents,
	}
	if !costs.Valid() || costs.TotalCents > snapshot.MaxCostCents {
		return nil
	}
	return &costs
}
