package application

import (
	"context"
	"sync"
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

type CommerceService struct {
	mu      sync.RWMutex
	options CommerceOptions
}

func NewCommerceService(options CommerceOptions) *CommerceService {
	options.AllowedKinds = append([]string(nil), options.AllowedKinds...)
	return &CommerceService{options: options}
}

// UpdatePolicy updates dynamic merchant policy fields without replacing Catalog or WorkerReady.
func (s *CommerceService) UpdatePolicy(salesEnabled, referenceLookupVerified bool, productCode string, maxCostCents int64, allowedKinds []string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.options.SalesEnabled = salesEnabled
	s.options.ReferenceLookupVerified = referenceLookupVerified
	s.options.ProductCode = productCode
	s.options.MaxCostCents = maxCostCents
	s.options.AllowedKinds = append([]string(nil), allowedKinds...)
	s.mu.Unlock()
}

func (s *CommerceService) currentOptions() CommerceOptions {
	s.mu.RLock()
	opt := s.options
	opt.AllowedKinds = append([]string(nil), s.options.AllowedKinds...)
	s.mu.RUnlock()
	return opt
}

// SKUOptions exposes the last known supplier availability for configuration.
// PurchaseTerms separately enforces freshness before issuing purchase terms.
func (s *CommerceService) SKUOptions(ctx context.Context) ([]kafudacontract.SKUOption, error) {
	opt := s.currentOptions()
	catalog, err := opt.Catalog.Cached(ctx)
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
	opt := s.currentOptions()
	if reader == nil || opt.Catalog == nil {
		return nil, kafudadomain.ErrInvalidCatalog
	}
	catalog, err := reader.GetCatalogForUpdate(opt.Catalog.profile)
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
	opt := s.currentOptions()
	catalog, err := s.purchaseCatalogWith(ctx, opt)
	if err != nil {
		return nil, err
	}
	if catalog == nil || !catalog.Available(plan, country, time.Now()) || catalog.Revision(plan, country) != reviewedRevision {
		return nil, ErrQuoteUnavailable
	}
	if !cardAllowedWith(catalog, opt.ProductCode) {
		return nil, ErrQuotePolicy
	}
	for _, entry := range catalog.Options {
		if entry.Plan == plan && entry.Country == country {
			return &kafudacontract.PurchaseTerms{Plan: plan, Country: country, Product: entry.Product, PayMode: "auto_card", ProductCode: opt.ProductCode, PaymentProfile: catalog.Profile, MaxCostCents: opt.MaxCostCents, AllowedKinds: append([]string(nil), opt.AllowedKinds...)}, nil
		}
	}
	return nil, ErrQuoteUnavailable
}

// SyncCatalog performs an on-demand upstream catalog sync when the supplier
// gateway is configured, skipping if the cached catalog was synced within the
// last hour, honoring rate-limit backoff, and skipping concurrent duplicate
// fetches.
func (s *CommerceService) SyncCatalog(ctx context.Context) error {
	if s == nil {
		return nil
	}
	opt := s.currentOptions()
	if opt.Catalog == nil {
		return nil
	}
	_, err := opt.Catalog.SyncOnDemand(ctx, DefaultVisitCatalogSyncMinInterval)
	return err
}

// PurchasableSKURevisions returns the currently sellable supplier options from one local snapshot.
func (s *CommerceService) PurchasableSKURevisions(ctx context.Context) (map[string]uint64, error) {
	opt := s.currentOptions()
	catalog, err := s.purchaseCatalogWith(ctx, opt)
	if err != nil {
		return nil, err
	}
	if catalog == nil || !catalog.Enabled || !time.Now().Before(catalog.ExpiresAt) || !cardAllowedWith(catalog, opt.ProductCode) {
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
	return salesBlockReasonWith(ctx, s.currentOptions())
}

func salesBlockReasonWith(ctx context.Context, opt CommerceOptions) string {
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
	opt := s.currentOptions()
	if reason := salesBlockReasonWith(ctx, opt); reason != "" {
		return reason
	}
	switch {
	case catalog == nil || !time.Now().Before(catalog.ExpiresAt):
		return "catalog_stale"
	case !catalog.Enabled:
		return "catalog_disabled"
	case !cardAllowedWith(catalog, opt.ProductCode):
		return "card_product_unavailable"
	default:
		return ""
	}
}

func (s *CommerceService) purchaseCatalogWith(ctx context.Context, opt CommerceOptions) (*kafudadomain.Catalog, error) {
	if salesBlockReasonWith(ctx, opt) != "" {
		return nil, ErrQuoteUnavailable
	}
	return opt.Catalog.Cached(ctx)
}

func cardAllowedWith(catalog *kafudadomain.Catalog, productCode string) bool {
	for _, card := range catalog.Cards {
		if card.ProductCode == productCode && !card.Restricted {
			return true
		}
	}
	return false
}
