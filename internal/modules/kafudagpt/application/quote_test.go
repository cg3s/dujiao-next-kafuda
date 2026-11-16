package application

import (
	"errors"
	"math"
	"testing"
	"time"

	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
	"github.com/dujiao-next/internal/shared/money"
	"github.com/shopspring/decimal"
)

func TestQuoteSelectionUsesPurchasedPlanAndFullUSDCost(t *testing.T) {
	now := time.Now().UTC()
	snapshot := kafudadomain.Snapshot{ProductID: 1, SKUID: 1, Product: "gpt", Plan: "plus", Country: "PH", PayMode: "auto_card", ProductCode: "P5378XB", PaymentProfile: "primary", Currency: "CNY", SellPrice: money.FromDecimal(decimal.NewFromInt(1)), MaxCostCents: 2000}
	quote := kafudacontract.Quote{Plan: "plus", Status: "available", Kind: "new", QuoteID: "quo_1", ExpiresAt: now.Add(time.Minute), PriceCents: 30, AutoCard: &kafudacontract.AutoCard{ProductCode: "P5378XB", OpenFeeCents: 50, LoadCents: 1700, TotalCents: 1780}}
	result := kafudacontract.CheckResult{Country: "PH", PayMode: "auto_card", Plans: []kafudacontract.Quote{quote}}
	if got, err := SelectQuote(result, snapshot, []string{"new"}, now); err != nil || got.QuoteID != quote.QuoteID {
		t.Fatalf("valid quote = (%+v, %v)", got, err)
	}
	for _, test := range []struct {
		name   string
		change func(*kafudacontract.Quote)
		want   error
	}{
		{"other_plan", func(q *kafudacontract.Quote) { q.Plan = "pro_5x" }, ErrQuoteUnavailable},
		{"not_eligible", func(q *kafudacontract.Quote) { q.Status = "not_eligible" }, ErrQuoteUnavailable},
		{"expires_now", func(q *kafudacontract.Quote) { q.ExpiresAt = now }, ErrQuoteExpired},
		{"unexpected_upgrade", func(q *kafudacontract.Quote) { q.Kind = "upgrade" }, ErrQuotePolicy},
		{"wrong_card", func(q *kafudacontract.Quote) { q.AutoCard.ProductCode = "OTHER" }, ErrQuotePolicy},
		{"missing_costs", func(q *kafudacontract.Quote) { q.AutoCard = nil }, ErrQuotePolicy},
		{"over_cap", func(q *kafudacontract.Quote) { q.AutoCard.LoadCents = 2700; q.AutoCard.TotalCents = 2780 }, ErrQuotePolicy},
		{"omitted_service_fee", func(q *kafudacontract.Quote) { q.AutoCard.TotalCents = 1750 }, ErrQuotePolicy},
		{"negative_fee", func(q *kafudacontract.Quote) { q.PriceCents = -1 }, ErrQuotePolicy},
		{"integer_overflow", func(q *kafudacontract.Quote) { q.AutoCard.LoadCents = math.MaxInt64; q.AutoCard.TotalCents = 1 }, ErrQuotePolicy},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := quote
			card := *quote.AutoCard
			candidate.AutoCard = &card
			test.change(&candidate)
			result.Plans = []kafudacontract.Quote{candidate}
			if _, err := SelectQuote(result, snapshot, []string{"new"}, now); !errors.Is(err, test.want) {
				t.Fatalf("selection error = %v, want %v", err, test.want)
			}
		})
	}
	result.Plans = []kafudacontract.Quote{quote, quote}
	if _, err := SelectQuote(result, snapshot, []string{"new"}, now); !errors.Is(err, ErrQuotePolicy) {
		t.Fatalf("ambiguous quote accepted: %v", err)
	}
	result.Country = "US"
	if _, err := SelectQuote(result, snapshot, []string{"new"}, now); !errors.Is(err, ErrQuotePolicy) {
		t.Fatalf("wrong country accepted: %v", err)
	}
}
