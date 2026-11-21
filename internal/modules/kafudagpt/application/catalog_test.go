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

func TestRefreshOnIntervalSchedulesAndHonorsRateLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		data := json.RawMessage(`{"enabled":true,"plans":[{"plan":"plus","label":"Plus","credit":false,"regions":[{"country":"PH","currency":"PHP","estimated_plan_usd_cents":1000,"estimated_local_minor":55000}]}],"auto_card_products":[{"product_code":"test-card","restriction_reason":null}]}`)
		store := &refreshTestCatalogStore{}
		limit := &kafudacontract.APIError{HTTPStatus: http.StatusTooManyRequests, RetryAfter: 90 * time.Second}
		gateway := &rateLimitedCatalogGateway{data: data, rateLimit: limit}
		service := NewCatalogService(store, gateway, "default", time.Hour)
		const interval = 5 * time.Minute

		// Empty cache triggers immediate sync; first call is rate-limited.
		if synced, err := service.RefreshOnInterval(ctx, interval); synced != nil || !errors.Is(err, limit) {
			t.Fatalf("initial refresh = (%v, %v), want rate limit error", synced, err)
		}
		// Before Retry-After expires, upstream must not be called again.
		time.Sleep(limit.RetryAfter - time.Second)
		if synced, err := service.RefreshOnInterval(ctx, interval); synced != nil || err != nil || gateway.calls != 1 {
			t.Fatalf("refresh called upstream during rate-limit backoff: synced=%v, err=%v, calls=%d", synced, err, gateway.calls)
		}
		// After Retry-After expires, sync succeeds and populates the cache.
		time.Sleep(time.Second)
		synced, err := service.RefreshOnInterval(ctx, interval)
		if err != nil || synced == nil || gateway.calls != 2 {
			t.Fatalf("post-backoff refresh = (%v, %v), calls=%d", synced, err, gateway.calls)
		}
		// Subsequent tick well inside the 5-minute interval skips upstream fetch.
		time.Sleep(2 * time.Minute)
		if skipped, err := service.RefreshOnInterval(ctx, interval); skipped != nil || err != nil || gateway.calls != 2 {
			t.Fatalf("refresh did not skip within interval: synced=%v, err=%v, calls=%d", skipped, err, gateway.calls)
		}
		// After the 5-minute interval elapses (even though TTL is 1 hour), it refreshes again.
		time.Sleep(3 * time.Minute)
		refreshed, err := service.RefreshOnInterval(ctx, interval)
		if err != nil || refreshed == nil || gateway.calls != 3 {
			t.Fatalf("fixed-interval refresh did not trigger after interval: synced=%v, err=%v, calls=%d", refreshed, err, gateway.calls)
		}
	})
}

func TestParseCatalogPreservesLocalMinorAndEstimateCostCNY(t *testing.T) {
	now := time.Now().UTC()
	data := json.RawMessage(`{
		"enabled":true,
		"plans":[
			{"plan":"plus","label":"Plus","credit":false,"regions":[
				{"country":"PH","currency":"PHP","estimated_plan_usd_cents":1537,"estimated_local_minor":98214,"local_exponent":2},
				{"country":"JP","currency":"JPY","estimated_plan_usd_cents":1707,"estimated_local_minor":2727,"local_exponent":0}
			]}
		],
		"auto_card_products":[]
	}`)
	got, err := ParseCatalog(data, "default", now, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Options) != 2 {
		t.Fatalf("len(Options) = %d, want 2", len(got.Options))
	}
	if got.Options[0].EstimatedLocalMinor == nil || *got.Options[0].EstimatedLocalMinor != 98214 ||
		got.Options[0].LocalExponent == nil || *got.Options[0].LocalExponent != 2 {
		t.Fatalf("PHP local minor/exponent lost: %+v", got.Options[0])
	}
	if got.Options[1].EstimatedLocalMinor == nil || *got.Options[1].EstimatedLocalMinor != 2727 ||
		got.Options[1].LocalExponent == nil || *got.Options[1].LocalExponent != 0 {
		t.Fatalf("JPY local minor/exponent lost: %+v", got.Options[1])
	}

	rates := map[string]float64{"PHP": 0.125, "JPY": 0.048}
	phpCNY, ok := EstimateCostCNY("PHP", *got.Options[0].EstimatedLocalMinor, got.Options[0].LocalExponent, rates)
	if !ok || phpCNY != 122.77 {
		t.Fatalf("PHP EstimateCostCNY = (%v, %t), want (122.77, true)", phpCNY, ok)
	}
	jpyCNY, ok := EstimateCostCNY("JPY", *got.Options[1].EstimatedLocalMinor, got.Options[1].LocalExponent, rates)
	if !ok || jpyCNY != 130.90 {
		t.Fatalf("JPY EstimateCostCNY = (%v, %t), want (130.9, true)", jpyCNY, ok)
	}
	// Fallback when local_exponent is omitted for zero-decimal currency JPY.
	jpyFallback, ok := EstimateCostCNY("JPY", 2727, nil, rates)
	if !ok || jpyFallback != 130.90 {
		t.Fatalf("JPY fallback EstimateCostCNY = (%v, %t), want (130.9, true)", jpyFallback, ok)
	}
	// CNY does not require an external rate entry.
	cnyVal, ok := EstimateCostCNY("CNY", 15000, nil, nil)
	if !ok || cnyVal != 150 {
		t.Fatalf("CNY EstimateCostCNY = (%v, %t), want (150, true)", cnyVal, ok)
	}
}

func TestSyncOnDemandBypassesScheduledCooldownAndHonorsRateLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		data := json.RawMessage(`{"enabled":true,"plans":[{"plan":"plus","label":"Plus","credit":false,"regions":[{"country":"PH","currency":"PHP","estimated_plan_usd_cents":1000,"estimated_local_minor":55000}]}],"auto_card_products":[{"product_code":"test-card","restriction_reason":null}]}`)
		store := &refreshTestCatalogStore{}
		limit := &kafudacontract.APIError{HTTPStatus: http.StatusTooManyRequests, RetryAfter: 90 * time.Second}
		gateway := &rateLimitedCatalogGateway{data: data, rateLimit: limit}
		service := NewCatalogService(store, gateway, "default", time.Hour)

		// First on-demand sync hits 429 and sets rate-limit backoff.
		if synced, err := service.SyncOnDemand(ctx); synced != nil || !errors.Is(err, limit) {
			t.Fatalf("first SyncOnDemand = (%v, %v), want rate limit error", synced, err)
		}
		// During Retry-After window, on-demand sync must not contact upstream.
		time.Sleep(limit.RetryAfter - time.Second)
		if synced, err := service.SyncOnDemand(ctx); synced != nil || err != nil || gateway.calls != 1 {
			t.Fatalf("SyncOnDemand contacted upstream during rate-limit backoff: synced=%v, err=%v, calls=%d", synced, err, gateway.calls)
		}
		// After Retry-After expires, scheduled refresh populates cache and sets nextRefresh far in future.
		time.Sleep(time.Second)
		if synced, err := service.RefreshOnInterval(ctx, 10*time.Minute); synced == nil || err != nil || gateway.calls != 2 {
			t.Fatalf("RefreshOnInterval after backoff = (%v, %v), calls=%d", synced, err, gateway.calls)
		}
		// Even while nextRefresh is still in the future, SyncOnDemand still syncs immediately.
		time.Sleep(time.Second)
		synced, err := service.SyncOnDemand(ctx)
		if err != nil || synced == nil || gateway.calls != 3 {
			t.Fatalf("SyncOnDemand did not bypass scheduled interval: synced=%v, err=%v, calls=%d", synced, err, gateway.calls)
		}
	})
}

type blockingCatalogGateway struct {
	kafudacontract.Gateway
	data    json.RawMessage
	started chan struct{}
	release chan struct{}
	calls   int
}

func (g *blockingCatalogGateway) Catalog(context.Context) (json.RawMessage, error) {
	g.calls++
	close(g.started)
	<-g.release
	return g.data, nil
}

func TestSyncOnDemandSkipsConcurrentCallsAndUnconfiguredGateway(t *testing.T) {
	ctx := t.Context()
	data := json.RawMessage(`{"enabled":true,"plans":[{"plan":"plus","label":"Plus","credit":false,"regions":[{"country":"PH","currency":"PHP","estimated_plan_usd_cents":1000,"estimated_local_minor":55000}]}],"auto_card_products":[{"product_code":"test-card","restriction_reason":null}]}`)

	// Unconfigured gateway is a clean no-op.
	unconfigured := NewCatalogService(&refreshTestCatalogStore{}, nil, "default", time.Hour)
	if synced, err := unconfigured.SyncOnDemand(ctx); synced != nil || err != nil {
		t.Fatalf("unconfigured SyncOnDemand = (%v, %v), want (nil, nil)", synced, err)
	}

	store := &refreshTestCatalogStore{}
	gateway := &blockingCatalogGateway{
		data:    data,
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	service := NewCatalogService(store, gateway, "default", time.Hour)

	done := make(chan error, 1)
	go func() {
		_, err := service.SyncOnDemand(ctx)
		done <- err
	}()
	<-gateway.started

	// Concurrent SyncOnDemand skips instead of blocking or calling upstream twice.
	if synced, err := service.SyncOnDemand(ctx); synced != nil || err != nil {
		t.Fatalf("concurrent SyncOnDemand = (%v, %v), want (nil, nil)", synced, err)
	}
	close(gateway.release)
	if err := <-done; err != nil {
		t.Fatalf("first SyncOnDemand failed: %v", err)
	}
	if gateway.calls != 1 {
		t.Fatalf("gateway.calls = %d, want 1", gateway.calls)
	}
}
