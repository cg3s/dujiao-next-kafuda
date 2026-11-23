package application

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	quefacontract "github.com/dujiao-next/internal/modules/quefa/contract"
	quefadomain "github.com/dujiao-next/internal/modules/quefa/domain"
	"github.com/shopspring/decimal"
)

// CatalogView is returned to admin UI for binding SKUs and inspecting sync health.
type CatalogView struct {
	Configured     bool                         `json:"configured"`
	CollectionMode string                       `json:"collection_mode"`
	Stale          bool                         `json:"stale"`
	Revision       int64                        `json:"revision"`
	SyncedAt       *time.Time                   `json:"synced_at,omitempty"`
	LastErrorCode  string                       `json:"last_error_code,omitempty"`
	LastErrorAt    *time.Time                   `json:"last_error_at,omitempty"`
	Readiness      quefacontract.SalesReadiness `json:"readiness"`
	Products       []quefadomain.CatalogProduct `json:"products"`
}

// CatalogService manages Quefa catalog caching, synchronization, and sellability checks.
type CatalogService struct {
	mu          sync.RWMutex
	store       quefacontract.Store
	configSvc   *ConfigService
	profile     string
	workerReady func(context.Context) bool
	now         func() time.Time
}

var _ quefacontract.CommercePolicy = (*CatalogService)(nil)

func NewCatalogService(store quefacontract.Store, configSvc *ConfigService, profile string, workerReady func(context.Context) bool) *CatalogService {
	if strings.TrimSpace(profile) == "" {
		profile = quefadomain.DefaultProfile
	}
	return &CatalogService{
		store:       store,
		configSvc:   configSvc,
		profile:     profile,
		workerReady: workerReady,
		now:         time.Now,
	}
}

func (s *CatalogService) Read(ctx context.Context) (*CatalogView, error) {
	if s.configSvc != nil {
		s.configSvc.RefreshIfStale(ctx)
	}
	snap := s.currentConfig()
	row, err := s.store.GetCatalog(ctx, s.profile)
	if err != nil {
		return nil, err
	}
	parsed, stale := s.decodeCatalog(row, snap.Config.CatalogTTL())
	view := &CatalogView{
		Configured:     snap.Configured && !snap.GatewayConfigInvalid && !snap.LoadError,
		CollectionMode: snap.Config.CollectionMode,
		Stale:          stale,
		Readiness:      s.SalesReadiness(ctx),
		Products:       []quefadomain.CatalogProduct{},
	}
	if row != nil {
		view.Revision = row.Revision
		view.SyncedAt = row.SyncedAt
		view.LastErrorCode = row.LastErrorCode
		view.LastErrorAt = row.LastErrorAt
	}
	if parsed != nil && len(parsed.Products) > 0 {
		view.Products = parsed.Products
	}
	return view, nil
}

// Sync fetches GET /v1/products outside any DB transaction, parses and validates it,
// and only overwrites the cached snapshot on success. Failures preserve the last valid snapshot.
func (s *CatalogService) Sync(ctx context.Context) (*CatalogView, error) {
	if s.configSvc != nil {
		s.configSvc.RefreshIfStale(ctx)
	}
	snap := s.currentConfig()
	if snap.Gateway == nil || snap.GatewayConfigInvalid || snap.LoadError {
		return nil, ErrGatewayNotConfigured
	}
	now := s.now().UTC()
	raw, _, err := snap.Gateway.GetProducts(ctx)
	if err != nil {
		errCode := classifySyncErrorCode(err)
		_ = s.recordSyncError(ctx, errCode, now)
		return nil, err
	}
	products, err := quefadomain.ParseCatalogPayload(raw)
	if err != nil {
		_ = s.recordSyncError(ctx, "invalid_catalog", now)
		return nil, err
	}
	parsedBytes, err := json.Marshal(products)
	if err != nil {
		return nil, err
	}

	err = s.store.WithinTransaction(ctx, func(tx quefacontract.Transaction) error {
		existing, txErr := tx.GetCatalogForUpdate(s.profile)
		if txErr != nil {
			return txErr
		}
		var rev int64 = 1
		if existing != nil {
			rev = existing.Revision + 1
		}
		return tx.SaveCatalog(&quefadomain.Catalog{
			Profile:       s.profile,
			RawPayload:    string(raw),
			ParsedJSON:    string(parsedBytes),
			SyncedAt:      &now,
			Revision:      rev,
			LastErrorCode: "",
			LastErrorAt:   nil,
			UpdatedAt:     now,
		})
	})
	if err != nil {
		return nil, err
	}
	return s.Read(ctx)
}

func (s *CatalogService) SalesReadiness(ctx context.Context) quefacontract.SalesReadiness {
	snap := s.currentConfig()
	reasons := make([]string, 0, 5)
	if !snap.Configured || snap.Gateway == nil || snap.GatewayConfigInvalid || snap.LoadError {
		reasons = append(reasons, "gateway_not_configured")
	}
	if !snap.Config.SalesEnabled {
		reasons = append(reasons, "sales_disabled")
	}
	if snap.Config.CollectionMode == quefadomain.CollectionModePlatformCollect && len(snap.Config.AllowedPayDomains) == 0 {
		reasons = append(reasons, "allowed_pay_domains_empty")
	}
	if s.workerReady != nil && !s.workerReady(ctx) {
		reasons = append(reasons, "worker_not_ready")
	}
	row, err := s.store.GetCatalog(ctx, s.profile)
	if err != nil {
		reasons = append(reasons, "catalog_unavailable")
	} else {
		parsed, stale := s.decodeCatalog(row, snap.Config.CatalogTTL())
		if stale || parsed == nil || len(parsed.Products) == 0 {
			reasons = append(reasons, "catalog_stale")
		}
	}
	return quefacontract.SalesReadiness{
		Ready:          len(reasons) == 0,
		BlockReasons:   reasons,
		CollectionMode: snap.Config.CollectionMode,
	}
}

func (s *CatalogService) ResolvePurchaseTerms(ctx context.Context, productCode string, adminCost decimal.Decimal, orderCurrency string, walletOnly bool) (*quefacontract.PurchaseTerms, error) {
	if walletOnly {
		return nil, quefacontract.ErrWalletOnlyBlocked
	}
	if s.configSvc != nil {
		s.configSvc.RefreshIfStale(ctx)
	}
	snap := s.currentConfig()
	if !snap.Config.SalesEnabled || snap.Gateway == nil || snap.GatewayConfigInvalid || snap.LoadError {
		return nil, quefacontract.ErrSalesDisabled
	}
	if snap.Config.CollectionMode == quefadomain.CollectionModePlatformCollect && len(snap.Config.AllowedPayDomains) == 0 {
		return nil, quefacontract.ErrSalesDisabled
	}
	if s.workerReady != nil && !s.workerReady(ctx) {
		return nil, quefacontract.ErrSalesDisabled
	}
	code := strings.TrimSpace(productCode)
	if code == "" {
		return nil, quefacontract.ErrProductUnavailable
	}
	if adminCost.IsNegative() {
		return nil, quefacontract.ErrCostNotConfigured
	}
	row, err := s.store.GetCatalog(ctx, s.profile)
	if err != nil {
		return nil, quefacontract.ErrCatalogStale
	}
	parsed, stale := s.decodeCatalog(row, snap.Config.CatalogTTL())
	if stale || parsed == nil {
		return nil, quefacontract.ErrCatalogStale
	}
	prod, ok := parsed.FindProduct(code)
	if !ok || !prod.SupportsMode(snap.Config.CollectionMode, quefadomain.DeliveryModeAutoRecharge) {
		return nil, quefacontract.ErrProductUnavailable
	}
	if orderCurrency != "" && !strings.EqualFold(strings.TrimSpace(orderCurrency), prod.Currency) {
		return nil, quefacontract.ErrCurrencyMismatch
	}
	supplyFloor := prod.EffectiveSupplyFloor()
	costFloor := supplyFloor
	if adminCost.Round(2).GreaterThan(costFloor) {
		costFloor = adminCost.Round(2)
	}
	if costFloor.LessThanOrEqual(decimal.Zero) {
		return nil, quefacontract.ErrCostNotConfigured
	}
	return &quefacontract.PurchaseTerms{
		ProductCode:        prod.ProductCode,
		CollectionMode:     snap.Config.CollectionMode,
		DeliveryMode:       quefadomain.DeliveryModeAutoRecharge,
		SupplyPrice:        prod.SupplyPrice.Round(2),
		AdminCostPrice:     adminCost.Round(2),
		CostFloor:          costFloor.Round(2),
		SettlementCurrency: prod.Currency,
		CatalogRevision:    parsed.Revision,
	}, nil
}

func (s *CatalogService) IsSellable(ctx context.Context, productCode string, adminCost decimal.Decimal, siteCurrency string, walletOnly bool) bool {
	_, err := s.ResolvePurchaseTerms(ctx, productCode, adminCost, siteCurrency, walletOnly)
	return err == nil
}

func (s *CatalogService) currentConfig() ConfigSnapshot {
	if s.configSvc == nil {
		return ConfigSnapshot{}
	}
	return s.configSvc.Snapshot()
}

func (s *CatalogService) decodeCatalog(row *quefadomain.Catalog, ttl time.Duration) (*quefadomain.ParsedCatalog, bool) {
	if row == nil || row.SyncedAt == nil || strings.TrimSpace(row.ParsedJSON) == "" {
		return nil, true
	}
	var products []quefadomain.CatalogProduct
	if err := json.Unmarshal([]byte(row.ParsedJSON), &products); err != nil || len(products) == 0 {
		return nil, true
	}
	stale := ttl > 0 && s.now().UTC().Sub(*row.SyncedAt) > ttl
	return &quefadomain.ParsedCatalog{
		Products: products,
		Revision: row.Revision,
		SyncedAt: row.SyncedAt,
	}, stale
}

func (s *CatalogService) recordSyncError(ctx context.Context, code string, now time.Time) error {
	return s.store.WithinTransaction(ctx, func(tx quefacontract.Transaction) error {
		existing, err := tx.GetCatalogForUpdate(s.profile)
		if err != nil {
			return err
		}
		if existing == nil {
			existing = &quefadomain.Catalog{Profile: s.profile}
		}
		existing.LastErrorCode = code
		existing.LastErrorAt = &now
		existing.UpdatedAt = now
		return tx.SaveCatalog(existing)
	})
}

func classifySyncErrorCode(err error) string {
	var apiErr *quefacontract.APIError
	if errors.As(err, &apiErr) && apiErr.Category != "" {
		return apiErr.Category
	}
	return "sync_failed"
}
