package application

import (
	"context"
	"time"

	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
)

type CommerceOptions struct {
	Catalog                 *CatalogService
	SalesEnabled            bool
	ReferenceLookupVerified bool
	ProductCode             string
	MaxCostCents            int64
	AllowedKinds            []string
	WorkerReady             func(context.Context) bool
}

type CommerceService struct{ options CommerceOptions }

func NewCommerceService(options CommerceOptions) *CommerceService {
	return &CommerceService{options: options}
}

// SKUOptions exposes the last known supplier availability for configuration.
// PurchaseTerms separately enforces freshness before issuing purchase terms.
func (s *CommerceService) SKUOptions(ctx context.Context) ([]kafudacontract.SKUOption, error) {
	catalog, err := s.options.Catalog.Cached(ctx)
	if err != nil {
		return nil, err
	}
	if catalog == nil {
		return nil, kafudadomain.ErrInvalidCatalog
	}
	return CatalogSKUOptions(catalog), nil
}

// SKUOptionsForUpdate reads the last known options from the caller's locked
// cache row. Product edits use it after taking the product lock.
func (s *CommerceService) SKUOptionsForUpdate(reader kafudacontract.CatalogForUpdate) ([]kafudacontract.SKUOption, error) {
	if reader == nil || s.options.Catalog == nil {
		return nil, kafudadomain.ErrInvalidCatalog
	}
	catalog, err := reader.GetCatalogForUpdate(s.options.Catalog.profile)
	if err != nil {
		return nil, err
	}
	if catalog == nil {
		return nil, kafudadomain.ErrInvalidCatalog
	}
	return CatalogSKUOptions(catalog), nil
}

// CatalogSKUOptions derives the configuration options from a single catalog snapshot.
func CatalogSKUOptions(catalog *kafudadomain.Catalog) []kafudacontract.SKUOption {
	result := make([]kafudacontract.SKUOption, 0, len(catalog.Options))
	for _, entry := range catalog.Options {
		result = append(result, kafudacontract.SKUOption{
			SKUCode:             kafudacontract.SKUCode(entry.Plan, entry.Country),
			Plan:                entry.Plan,
			Country:             entry.Country,
			Label:               entry.Label,
			Currency:            entry.Currency,
			EstimatedLocalMinor: entry.EstimatedLocalMinor,
			LocalExponent:       entry.LocalExponent,
			Available:           catalog.Enabled && entry.Available,
			CatalogRevision:     catalog.Revision(entry.Plan, entry.Country),
		})
	}
	return result
}

func (s *CommerceService) PurchaseTerms(ctx context.Context, plan, country string, reviewedRevision uint64) (*kafudacontract.PurchaseTerms, error) {
	catalog, err := s.purchaseCatalog(ctx)
	if err != nil {
		return nil, err
	}
	if catalog == nil || !catalog.Available(plan, country, time.Now()) || catalog.Revision(plan, country) != reviewedRevision {
		return nil, ErrQuoteUnavailable
	}
	if !s.cardAllowed(catalog) {
		return nil, ErrQuotePolicy
	}
	opt := s.options
	for _, entry := range catalog.Options {
		if entry.Plan == plan && entry.Country == country {
			return &kafudacontract.PurchaseTerms{Plan: plan, Country: country, Product: entry.Product, PayMode: "auto_card", ProductCode: opt.ProductCode, PaymentProfile: catalog.Profile, MaxCostCents: opt.MaxCostCents, AllowedKinds: append([]string(nil), opt.AllowedKinds...)}, nil
		}
	}
	return nil, ErrQuoteUnavailable
}

// SyncCatalog performs an on-demand upstream catalog sync when the supplier
// gateway is configured, honoring rate-limit backoff and skipping concurrent
// duplicate fetches.
func (s *CommerceService) SyncCatalog(ctx context.Context) error {
	if s == nil || s.options.Catalog == nil {
		return nil
	}
	_, err := s.options.Catalog.SyncOnDemand(ctx)
	return err
}

// PurchasableSKURevisions returns the currently sellable supplier options from one local snapshot.
func (s *CommerceService) PurchasableSKURevisions(ctx context.Context) (map[string]uint64, error) {
	catalog, err := s.purchaseCatalog(ctx)
	if err != nil {
		return nil, err
	}
	if catalog == nil || !catalog.Enabled || !time.Now().Before(catalog.ExpiresAt) || !s.cardAllowed(catalog) {
		return nil, nil
	}
	revisions := make(map[string]uint64, len(catalog.Options))
	for _, option := range catalog.Options {
		if option.Available {
			revisions[kafudacontract.SKUCode(option.Plan, option.Country)] = catalog.Revision(option.Plan, option.Country)
		}
	}
	return revisions, nil
}

// SalesBlockReason identifies the first deployment prerequisite blocking new sales.
func (s *CommerceService) SalesBlockReason(ctx context.Context) string {
	opt := s.options
	switch {
	case !opt.SalesEnabled:
		return "sales_disabled"
	case !opt.ReferenceLookupVerified:
		return "reference_lookup_unverified"
	case opt.Catalog == nil || opt.ProductCode == "" || opt.MaxCostCents <= 0 || len(opt.AllowedKinds) == 0:
		return "merchant_policy_incomplete"
	case opt.WorkerReady == nil:
		return "fulfillment_not_wired"
	case !opt.WorkerReady(ctx):
		return "worker_not_ready"
	default:
		return ""
	}
}

// CatalogSalesBlockReason diagnoses the snapshot already read for the admin response.
func (s *CommerceService) CatalogSalesBlockReason(ctx context.Context, catalog *kafudadomain.Catalog) string {
	if reason := s.SalesBlockReason(ctx); reason != "" {
		return reason
	}
	switch {
	case catalog == nil || !time.Now().Before(catalog.ExpiresAt):
		return "catalog_stale"
	case !catalog.Enabled:
		return "catalog_disabled"
	case !s.cardAllowed(catalog):
		return "card_product_unavailable"
	default:
		return ""
	}
}

func (s *CommerceService) purchaseCatalog(ctx context.Context) (*kafudadomain.Catalog, error) {
	if s.SalesBlockReason(ctx) != "" {
		return nil, ErrQuoteUnavailable
	}
	return s.options.Catalog.Cached(ctx)
}

func (s *CommerceService) cardAllowed(catalog *kafudadomain.Catalog) bool {
	for _, card := range catalog.Cards {
		if card.ProductCode == s.options.ProductCode && !card.Restricted {
			return true
		}
	}
	return false
}
