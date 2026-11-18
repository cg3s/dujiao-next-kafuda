package application

import (
	"errors"
	"math"
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
	expired := false
	for _, candidate := range result.Plans {
		if candidate.Plan != snapshot.Plan || candidate.Status != "available" {
			continue
		}
		if !candidate.ExpiresAt.After(now) {
			expired = true
			continue
		}
		if selected != nil || strings.TrimSpace(candidate.QuoteID) == "" || !allowedKind(candidate.Kind, snapshot.AllowedKinds) || !validAutoCard(candidate, snapshot) {
			return nil, ErrQuotePolicy
		}
		quote := candidate
		selected = &quote
	}
	if selected == nil {
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

func validAutoCard(quote kafudacontract.Quote, snapshot kafudadomain.Snapshot) bool {
	card := quote.AutoCard
	if card == nil || card.ProductCode != snapshot.ProductCode || card.OpenFeeCents < 0 || card.LoadCents < 0 || quote.PriceCents < 0 || card.TotalCents <= 0 || card.TotalCents > snapshot.MaxCostCents {
		return false
	}
	if card.OpenFeeCents > math.MaxInt64-card.LoadCents {
		return false
	}
	cardCost := card.OpenFeeCents + card.LoadCents
	return quote.PriceCents <= math.MaxInt64-cardCost && cardCost+quote.PriceCents == card.TotalCents
}
