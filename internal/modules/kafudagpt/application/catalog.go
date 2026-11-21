package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
)

type CatalogService struct {
	store          kafudacontract.Store
	configMu       sync.RWMutex
	gateway        kafudacontract.Gateway
	profile        string
	ttl            time.Duration
	refreshMu      sync.Mutex
	nextRefresh    time.Time
	rateLimitUntil time.Time
}

func NewCatalogService(store kafudacontract.Store, gateway kafudacontract.Gateway, profile string, ttl time.Duration) *CatalogService {
	return &CatalogService{store: store, gateway: gateway, profile: profile, ttl: ttl}
}

// ApplyRuntime swaps the active upstream gateway and cache TTL in place.
func (s *CatalogService) ApplyRuntime(gateway kafudacontract.Gateway, ttl time.Duration) {
	if s == nil {
		return
	}
	s.configMu.Lock()
	s.gateway = gateway
	s.ttl = ttl
	s.configMu.Unlock()
	if s.refreshMu.TryLock() {
		s.nextRefresh = time.Time{}
		s.rateLimitUntil = time.Time{}
		s.refreshMu.Unlock()
	}
}

func (s *CatalogService) runtime() (kafudacontract.Gateway, time.Duration) {
	s.configMu.RLock()
	gateway, ttl := s.gateway, s.ttl
	s.configMu.RUnlock()
	return gateway, ttl
}

func (s *CatalogService) Cached(ctx context.Context) (*kafudadomain.Catalog, error) {
	catalog, err := s.store.GetCatalog(ctx, s.profile)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", kafudacontract.ErrCatalogReadFailed, err)
	}
	return catalog, nil
}

func (s *CatalogService) Sync(ctx context.Context) (*kafudadomain.Catalog, error) {
	gateway, ttl := s.runtime()
	if gateway == nil || s.profile == "" || ttl <= 0 {
		return nil, kafudadomain.ErrInvalidCatalog
	}
	// Timestamp the start, not the finish, so a slow older request cannot
	// overwrite the result of a later concurrent sync.
	started := time.Now().UTC()
	data, err := gateway.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	catalog, err := ParseCatalog(data, s.profile, started, started.Add(ttl))
	if err != nil {
		return nil, err
	}
	if err := s.store.SaveCatalog(ctx, catalog); err != nil {
		return nil, fmt.Errorf("%w: %w", kafudacontract.ErrCatalogWriteFailed, err)
	}
	return s.Cached(ctx)
}

// RefreshBeforeExpiry keeps an enabled storefront on a fresh supplier snapshot.
// Failed refreshes keep the last snapshot and are retried by a later worker tick.
func (s *CatalogService) RefreshBeforeExpiry(ctx context.Context, interval time.Duration) error {
	_, err := s.RefreshBeforeExpiryResult(ctx, interval)
	return err
}

// RefreshBeforeExpiryResult refreshes the catalog when missing or approaching TTL
// expiry, returning the newly synced catalog when an upstream sync occurred.
func (s *CatalogService) RefreshBeforeExpiryResult(ctx context.Context, interval time.Duration) (*kafudadomain.Catalog, error) {
	_, ttl := s.runtime()
	if ttl <= 0 || !s.refreshMu.TryLock() {
		return nil, nil
	}
	defer s.refreshMu.Unlock()
	now := time.Now()
	if now.Before(s.nextRefresh) {
		return nil, nil
	}
	lead := min(ttl/2, max(interval*2, 2*time.Minute))
	s.nextRefresh = now.Add(min(time.Minute, lead/2))
	catalog, err := s.Cached(ctx)
	if err != nil {
		return nil, err
	}
	if catalog != nil && catalog.ExpiresAt.After(now.Add(lead)) {
		s.nextRefresh = catalog.ExpiresAt.Add(-lead)
		return nil, nil
	}
	synced, err := s.Sync(ctx)
	if err != nil {
		s.applyRateLimitBackoff(err)
		return nil, err
	}
	s.nextRefresh = now.Add(ttl - lead)
	return synced, nil
}

// RefreshOnInterval refreshes the catalog on a fixed schedule while respecting
// upstream 429 Retry-After backoff and skipping redundant fetches when a recent
// manual or scheduled sync already completed within the interval.
func (s *CatalogService) RefreshOnInterval(ctx context.Context, interval time.Duration) (*kafudadomain.Catalog, error) {
	_, ttl := s.runtime()
	if ttl <= 0 || interval <= 0 || !s.refreshMu.TryLock() {
		return nil, nil
	}
	defer s.refreshMu.Unlock()
	now := time.Now()
	if now.Before(s.nextRefresh) {
		return nil, nil
	}
	jitter := min(interval/10, 5*time.Second)
	s.nextRefresh = now.Add(min(time.Minute, max(time.Second, interval/2)))
	catalog, err := s.Cached(ctx)
	if err != nil {
		return nil, err
	}
	lead := min(ttl/2, max(interval/2, time.Minute))
	if catalog != nil {
		dueAt := catalog.SyncedAt.Add(interval)
		if expiryDue := catalog.ExpiresAt.Add(-lead); expiryDue.Before(dueAt) {
			dueAt = expiryDue
		}
		if dueAt.After(now.Add(jitter)) {
			s.nextRefresh = dueAt.Add(-jitter)
			return nil, nil
		}
	}
	synced, err := s.Sync(ctx)
	if err != nil {
		s.applyRateLimitBackoff(err)
		return nil, err
	}
	nextDue := now.Add(interval - jitter)
	if expiryDue := now.Add(ttl - lead); expiryDue.Before(nextDue) {
		nextDue = expiryDue
	}
	s.nextRefresh = nextDue
	return synced, nil
}

const DefaultVisitCatalogSyncMinInterval = time.Hour

// SyncOnDemand fetches the latest supplier catalog on demand (for example when
// a buyer views a direct-recharge product), skipping when the cached catalog
// was synced less than minInterval ago, respecting upstream 429 Retry-After
// backoff, and skipping if another sync is already in progress.
func (s *CatalogService) SyncOnDemand(ctx context.Context, minInterval time.Duration) (*kafudadomain.Catalog, error) {
	if s == nil {
		return nil, nil
	}
	gateway, ttl := s.runtime()
	if gateway == nil || s.profile == "" || ttl <= 0 || !s.refreshMu.TryLock() {
		return nil, nil
	}
	defer s.refreshMu.Unlock()
	now := time.Now()
	if now.Before(s.rateLimitUntil) {
		return nil, nil
	}
	if minInterval > 0 {
		catalog, err := s.Cached(ctx)
		if err != nil {
			return nil, err
		}
		if catalog != nil && !catalog.SyncedAt.IsZero() && now.Before(catalog.SyncedAt.Add(minInterval)) {
			return nil, nil
		}
	}
	synced, err := s.Sync(ctx)
	if err != nil {
		s.applyRateLimitBackoff(err)
		return nil, err
	}
	return synced, nil
}

func (s *CatalogService) applyRateLimitBackoff(err error) {
	var limit *kafudacontract.APIError
	if errors.As(err, &limit) && limit != nil && limit.HTTPStatus == http.StatusTooManyRequests && limit.RetryAfter > 0 {
		retryAt := time.Now().Add(limit.RetryAfter)
		if retryAt.After(s.rateLimitUntil) {
			s.rateLimitUntil = retryAt
		}
		if retryAt.After(s.nextRefresh) {
			s.nextRefresh = retryAt
		}
	}
}

// ParseCatalog consumes the documented catalog shape supplied in
// docs/gpt_catalog_data.json. Only each plan's own regions generate options.
// Null regional estimates remain visible to operators but cannot be sold.
func ParseCatalog(data json.RawMessage, profile string, syncedAt, expiresAt time.Time) (*kafudadomain.Catalog, error) {
	var wire struct {
		Enabled *bool `json:"enabled"`
		Plans   []struct {
			Plan    string `json:"plan"`
			Label   string `json:"label"`
			Credit  *bool  `json:"credit"`
			Regions []struct {
				Country           string `json:"country"`
				Currency          string `json:"currency"`
				EstimatedUSDCents *int64 `json:"estimated_plan_usd_cents"`
				LocalMinor        *int64 `json:"estimated_local_minor"`
				LocalExponent     *int   `json:"local_exponent"`
			} `json:"regions"`
		} `json:"plans"`
		Cards []struct {
			ProductCode       string          `json:"product_code"`
			Name              string          `json:"name"`
			RestrictionReason json.RawMessage `json:"restriction_reason"`
			GPTRecommended    bool            `json:"gpt_recommended"`
			OpenFeeCents      int64           `json:"open_fee_cents"`
			MinAmountCents    int64           `json:"min_amount_cents"`
		} `json:"auto_card_products"`
	}
	if json.Unmarshal(data, &wire) != nil || wire.Enabled == nil || wire.Plans == nil || wire.Cards == nil {
		return nil, kafudadomain.ErrInvalidCatalog
	}
	catalog := &kafudadomain.Catalog{Profile: profile, Enabled: *wire.Enabled, SyncedAt: syncedAt, ExpiresAt: expiresAt, Options: []kafudadomain.CatalogOption{}, Cards: []kafudadomain.CardProduct{}}
	for _, plan := range wire.Plans {
		if plan.Plan == "" || plan.Label == "" || plan.Credit == nil || plan.Regions == nil {
			return nil, kafudadomain.ErrInvalidCatalog
		}
		product := "gpt"
		if *plan.Credit {
			product = "gpt_credits"
		}
		for _, region := range plan.Regions {
			if region.Currency == "" || (region.EstimatedUSDCents != nil && *region.EstimatedUSDCents < 0) || (region.LocalMinor != nil && *region.LocalMinor < 0) || (region.LocalExponent != nil && (*region.LocalExponent < 0 || *region.LocalExponent > 6)) {
				return nil, kafudadomain.ErrInvalidCatalog
			}
			available := region.EstimatedUSDCents != nil && *region.EstimatedUSDCents > 0 && region.LocalMinor != nil && *region.LocalMinor > 0
			catalog.Options = append(catalog.Options, kafudadomain.CatalogOption{Plan: plan.Plan, Label: plan.Label, Product: product, Country: region.Country, Currency: region.Currency, EstimatedUSDCents: region.EstimatedUSDCents, EstimatedLocalMinor: region.LocalMinor, LocalExponent: region.LocalExponent, Available: available})
		}
	}
	seenCards := make(map[string]bool)
	for _, card := range wire.Cards {
		// An explicit null declares an unrestricted card. An omitted field has no such declaration.
		var restrictionReason *string
		if card.ProductCode == "" || seenCards[card.ProductCode] || card.OpenFeeCents < 0 || card.MinAmountCents < 0 ||
			json.Unmarshal(card.RestrictionReason, &restrictionReason) != nil {
			return nil, kafudadomain.ErrInvalidCatalog
		}
		seenCards[card.ProductCode] = true
		catalog.Cards = append(catalog.Cards, kafudadomain.CardProduct{ProductCode: card.ProductCode, Name: card.Name, Restricted: restrictionReason != nil && *restrictionReason != "", GPTRecommended: card.GPTRecommended, OpenFeeCents: card.OpenFeeCents, MinAmountCents: card.MinAmountCents})
	}
	if err := catalog.Validate(); err != nil {
		return nil, err
	}
	return catalog, nil
}

// EstimateCostCNY converts a supplier option's estimated_local_minor amount to CNY
// using ratesToCNY (where each entry is CNY per 1 unit of the keyed currency).
func EstimateCostCNY(currency string, localMinor int64, localExponent *int, ratesToCNY map[string]float64) (float64, bool) {
	if localMinor <= 0 {
		return 0, false
	}
	code := strings.ToUpper(strings.TrimSpace(currency))
	if code == "" {
		return 0, false
	}
	rate := 0.0
	if code == "CNY" {
		rate = 1.0
	} else if ratesToCNY != nil {
		rate = ratesToCNY[code]
	}
	if rate <= 0 || math.IsNaN(rate) || math.IsInf(rate, 0) {
		return 0, false
	}
	exp := defaultCurrencyExponent(code)
	if localExponent != nil && *localExponent >= 0 && *localExponent <= 6 {
		exp = *localExponent
	}
	major := float64(localMinor) / math.Pow10(exp)
	cny := math.Round(major*rate*100) / 100
	if cny <= 0 || math.IsNaN(cny) || math.IsInf(cny, 0) {
		return 0, false
	}
	return cny, true
}

func defaultCurrencyExponent(code string) int {
	switch code {
	case "BIF", "CLP", "DJF", "GNF", "ISK", "JPY", "KMF", "KRW", "PYG", "RWF", "UGX", "UYI", "VND", "VUV", "XAF", "XOF", "XPF":
		return 0
	default:
		return 2
	}
}
