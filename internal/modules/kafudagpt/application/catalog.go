package application

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
)

type CatalogService struct {
	store   kafudacontract.Store
	gateway kafudacontract.Gateway
	profile string
	ttl     time.Duration
}

func NewCatalogService(store kafudacontract.Store, gateway kafudacontract.Gateway, profile string, ttl time.Duration) *CatalogService {
	return &CatalogService{store: store, gateway: gateway, profile: profile, ttl: ttl}
}

func (s *CatalogService) Cached(ctx context.Context) (*kafudadomain.Catalog, error) {
	catalog, err := s.store.GetCatalog(ctx, s.profile)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", kafudacontract.ErrCatalogReadFailed, err)
	}
	return catalog, nil
}

func (s *CatalogService) Sync(ctx context.Context) (*kafudadomain.Catalog, error) {
	if s.gateway == nil || s.profile == "" || s.ttl <= 0 {
		return nil, kafudadomain.ErrInvalidCatalog
	}
	// Timestamp the start, not the finish, so a slow older request cannot
	// overwrite the result of a later concurrent sync.
	started := time.Now().UTC()
	data, err := s.gateway.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	catalog, err := ParseCatalog(data, s.profile, started, started.Add(s.ttl))
	if err != nil {
		return nil, err
	}
	if err := s.store.SaveCatalog(ctx, catalog); err != nil {
		return nil, fmt.Errorf("%w: %w", kafudacontract.ErrCatalogWriteFailed, err)
	}
	return s.Cached(ctx)
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
			if region.Currency == "" || (region.EstimatedUSDCents != nil && *region.EstimatedUSDCents < 0) || (region.LocalMinor != nil && *region.LocalMinor < 0) {
				return nil, kafudadomain.ErrInvalidCatalog
			}
			available := region.EstimatedUSDCents != nil && *region.EstimatedUSDCents > 0 && region.LocalMinor != nil && *region.LocalMinor > 0
			catalog.Options = append(catalog.Options, kafudadomain.CatalogOption{Plan: plan.Plan, Label: plan.Label, Product: product, Country: region.Country, Currency: region.Currency, EstimatedUSDCents: region.EstimatedUSDCents, Available: available})
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
