package application

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
)

func TestCatalogUsesPerPlanRegionsAndKeepsUnknownPairsUnavailable(t *testing.T) {
	now := time.Now().UTC()
	data := json.RawMessage(`{
		"enabled":true,
		"plans":[
			{"plan":"plus","label":"Plus","credit":false,"regions":[{"country":"PH","currency":"PHP","estimated_plan_usd_cents":1537,"estimated_local_minor":98214}]},
			{"plan":"credit250","label":"Codex 250","credit":true,"regions":[{"country":"PH","currency":"PHP","estimated_plan_usd_cents":800,"estimated_local_minor":50000},{"country":"CL","currency":"CLP","estimated_plan_usd_cents":null,"estimated_local_minor":null}]}
		],
		"regions":[{"country":"PH"},{"country":"CL"},{"country":"US"}],
		"auto_card_products":[{"product_code":"P5378XB","name":"test","restriction_reason":null,"gpt_recommended":true,"open_fee_cents":100,"min_amount_cents":2000},{"product_code":"restricted","name":"test","restriction_reason":"not supported","gpt_recommended":false,"open_fee_cents":100,"min_amount_cents":2000},{"product_code":"explicit-empty","restriction_reason":""}]
	}`)
	got, err := ParseCatalog(data, "default", now, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Options) != 3 || !got.Available("plus", "PH", now) || got.Available("plus", "CL", now) || got.Available("credit250", "CL", now) || got.Available("plus", "US", now) {
		t.Fatalf("catalog incorrectly expanded or enabled regions: %+v", got)
	}
	if got.Options[0].Product != "gpt" || got.Options[1].Product != "gpt_credits" || len(got.Cards) != 3 ||
		got.Cards[0].Restricted || !got.Cards[1].Restricted || got.Cards[2].Restricted {
		t.Fatalf("lost product or card restriction: %+v", got)
	}
}

func TestInvalidCatalogCannotBeMistakenForConfirmedRemoval(t *testing.T) {
	now := time.Now().UTC()
	for _, data := range []string{
		`{}`, `null`, `{"enabled":true,"plans":null,"auto_card_products":[]}`,
		`{"enabled":true,"plans":[],"auto_card_products":null}`,
		`{"enabled":true,"plans":[{"plan":"plus","label":"Plus","credit":false,"regions":[{"country":"PH","currency":"PHP","estimated_plan_usd_cents":-1}]}],"auto_card_products":[]}`,
		`{"enabled":true,"plans":[{"plan":"plus","label":"Plus","credit":false,"regions":[{"country":"PH","currency":"PHP"},{"country":"PH","currency":"PHP"}]}],"auto_card_products":[]}`,
		`{"enabled":true,"plans":[{"plan":"credit250","label":"Codex 250","regions":[]}],"auto_card_products":[]}`,
		`{"enabled":true,"plans":[{"plan":"credit250","label":"Codex 250","credit":null,"regions":[]}],"auto_card_products":[]}`,
		`{"enabled":true,"plans":[],"auto_card_products":[{"product_code":"restricted"}]}`,
		`{"enabled":true,"plans":[],"auto_card_products":[{"product_code":"restricted","restriction_reason":false}]}`,
	} {
		if _, err := ParseCatalog(json.RawMessage(data), "default", now, now.Add(time.Hour)); !errors.Is(err, kafudadomain.ErrInvalidCatalog) {
			t.Fatalf("invalid catalog accepted: %s, %v", data, err)
		}
	}
	got, err := ParseCatalog(json.RawMessage(`{"enabled":false,"plans":[],"auto_card_products":[]}`), "default", now, now.Add(time.Hour))
	if err != nil || got.Enabled || len(got.Options) != 0 {
		t.Fatalf("confirmed disabled catalog rejected: %+v, %v", got, err)
	}
}

type refreshTestCatalogStore struct {
	kafudacontract.Store
	catalog *kafudadomain.Catalog
}

func (s *refreshTestCatalogStore) GetCatalog(context.Context, string) (*kafudadomain.Catalog, error) {
	return s.catalog, nil
}

func (s *refreshTestCatalogStore) SaveCatalog(_ context.Context, catalog *kafudadomain.Catalog) error {
	s.catalog = catalog
	return nil
}

type rateLimitedCatalogGateway struct {
	kafudacontract.Gateway
	data      json.RawMessage
	rateLimit *kafudacontract.APIError
	calls     int
}

func (g *rateLimitedCatalogGateway) Catalog(context.Context) (json.RawMessage, error) {
	g.calls++
	if g.calls == 1 {
		time.Sleep(5 * time.Second)
		return nil, g.rateLimit
	}
	return g.data, nil
}

func TestScheduledCatalogRefreshHonorsRateLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		now := time.Now()
		data := json.RawMessage(`{"enabled":true,"plans":[{"plan":"plus","label":"Plus","credit":false,"regions":[{"country":"PH","currency":"PHP","estimated_plan_usd_cents":1000,"estimated_local_minor":55000}]}],"auto_card_products":[{"product_code":"test-card","restriction_reason":null}]}`)
		initial, err := ParseCatalog(data, "default", now.Add(-time.Hour), now.Add(110*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		store := &refreshTestCatalogStore{catalog: initial}
		limit := &kafudacontract.APIError{HTTPStatus: http.StatusTooManyRequests, RetryAfter: 90 * time.Second}
		gateway := &rateLimitedCatalogGateway{data: data, rateLimit: limit}
		service := NewCatalogService(store, gateway, initial.Profile, time.Hour)
		const interval = 10 * time.Second

		if err := service.RefreshBeforeExpiry(ctx, interval); !errors.Is(err, limit) {
			t.Fatalf("initial refresh error = %v, want a rate limit", err)
		}
		time.Sleep(limit.RetryAfter - time.Second)
		if err := service.RefreshBeforeExpiry(ctx, interval); err != nil || gateway.calls != 1 {
			t.Fatalf("refresh contacted the supplier before the retry window ended: calls=%d, error=%v", gateway.calls, err)
		}
		if store.catalog != initial || !store.catalog.Available("plus", "PH", time.Now()) {
			t.Fatal("the last valid catalog should stay available during a brief rate limit")
		}
		time.Sleep(time.Second)
		if err := service.RefreshBeforeExpiry(ctx, interval); err != nil || gateway.calls != 2 {
			t.Fatalf("refresh did not retry after the wait: calls=%d, error=%v", gateway.calls, err)
		}
		if !store.catalog.ExpiresAt.After(initial.ExpiresAt) || !store.catalog.Available("plus", "PH", initial.ExpiresAt) {
			t.Fatal("successful retry did not renew the catalog")
		}
	})
}
