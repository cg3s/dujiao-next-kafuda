package kafudawiring

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/dujiao-next/internal/config"
	"github.com/dujiao-next/internal/constants"
	kafudaapp "github.com/dujiao-next/internal/modules/kafudagpt/application"
	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
	kafudastore "github.com/dujiao-next/internal/modules/kafudagpt/infrastructure/gormstore"
	"github.com/dujiao-next/internal/modules/kafudagpt/infrastructure/kafuda"
	orderapp "github.com/dujiao-next/internal/modules/order/application"
	ordercontract "github.com/dujiao-next/internal/modules/order/contract"
	kafudaworkflow "github.com/dujiao-next/internal/workflows/kafudagpt/application"

	"gorm.io/gorm"
)

// Module is assembled independently of the app container, so transports and
// workers can consume module contracts without a container dependency.
type Module struct {
	mu                   sync.RWMutex
	Store                kafudacontract.Store
	Records              *kafudaapp.Service
	Catalog              *kafudaapp.CatalogService
	Config               *kafudaapp.ConfigService
	Gateway              kafudacontract.Gateway
	FXRates              kafudacontract.FXRateProvider
	Commerce             *kafudaapp.CommerceService
	workerReady          func(context.Context) bool
	gatewayConfigInvalid bool
}

func New(db *gorm.DB, cfg config.KafudaGPTConfig, orders ordercontract.Store, workerReady func(context.Context) bool) (*Module, error) {
	return newModule(db, cfg, "", false, orders, workerReady)
}

// NewWithSettings initializes the Kafuda module backed by encrypted DB settings
// with config.yml fallback and live runtime hot-reload.
func NewWithSettings(db *gorm.DB, cfg config.KafudaGPTConfig, appSecretKey string, orders ordercontract.Store, workerReady func(context.Context) bool) (*Module, error) {
	return newModule(db, cfg, appSecretKey, true, orders, workerReady)
}

func newModule(db *gorm.DB, cfg config.KafudaGPTConfig, appSecretKey string, enableSettingStore bool, orders ordercontract.Store, workerReady func(context.Context) bool) (*Module, error) {
	store := kafudastore.New(db)
	var settingStore kafudacontract.SettingStore
	if enableSettingStore {
		settingStore = store
	}
	fallback := runtimeConfigFromYAML(cfg)
	factory := func(baseURL, apiKey string) (kafudacontract.Gateway, error) {
		return kafuda.New(baseURL, apiKey)
	}
	var configSvc *kafudaapp.ConfigService
	var initial kafudaapp.ConfigSnapshot
	if enableSettingStore {
		configSvc = kafudaapp.NewConfigService(context.Background(), kafudaapp.ConfigServiceOptions{
			Store:          settingStore,
			Profile:        kafudadomain.DefaultProfile,
			AppSecretKey:   appSecretKey,
			Fallback:       fallback,
			GatewayFactory: factory,
		})
		initial = configSvc.Snapshot()
	} else {
		staticSvc := kafudaapp.NewConfigService(context.Background(), kafudaapp.ConfigServiceOptions{
			Profile:        kafudadomain.DefaultProfile,
			Fallback:       fallback,
			GatewayFactory: factory,
		})
		initial = staticSvc.Snapshot()
	}

	if err := syncLegacyManualOrders(db, orders); err != nil {
		return nil, err
	}

	catalog := kafudaapp.NewCatalogService(store, initial.Gateway, kafudadomain.DefaultProfile, initial.Config.CatalogTTL())
	commerce := kafudaapp.NewCommerceService(kafudaapp.CommerceOptions{
		Catalog:                 catalog,
		SalesEnabled:            initial.Config.SalesEnabled && initial.Gateway != nil && !initial.LoadError,
		ReferenceLookupVerified: initial.Config.ReferenceLookupVerified,
		ProductCode:             initial.Config.ProductCode,
		MaxCostCents:            initial.Config.MaxCostCents,
		AllowedKinds:            initial.Config.AllowedKinds,
		WorkerReady:             workerReady,
	})
	module := &Module{
		Store:                store,
		Records:              kafudaworkflow.WithShopOrderTransitions(kafudaapp.New(store), orders),
		Catalog:              catalog,
		Config:               configSvc,
		Gateway:              initial.Gateway,
		FXRates:              kafuda.NewFXClient("", nil),
		Commerce:             commerce,
		workerReady:          workerReady,
		gatewayConfigInvalid: initial.GatewayConfigInvalid,
	}
	if configSvc != nil {
		configSvc.SetOnApply(module.applySnapshot)
	}
	return module, nil
}

func runtimeConfigFromYAML(cfg config.KafudaGPTConfig) kafudaapp.RuntimeConfig {
	return kafudaapp.RuntimeConfig{
		APIBaseURL:                    cfg.APIBaseURL,
		APIKey:                        cfg.APIKey,
		ProductCode:                   cfg.ProductCode,
		MaxCostCents:                  cfg.MaxCostCents,
		AllowedKinds:                  append([]string(nil), cfg.AllowedKinds...),
		SalesEnabled:                  cfg.SalesEnabled,
		ReferenceLookupVerified:       cfg.ReferenceLookupVerified,
		CatalogTTLSeconds:             cfg.CatalogTTLSeconds,
		CatalogRefreshIntervalSeconds: cfg.CatalogRefreshIntervalSeconds,
		PollReviewAfterSeconds:        cfg.PollReviewAfterSeconds,
		PollIntervalSeconds:           cfg.PollIntervalSeconds,
	}
}

func (m *Module) applySnapshot(snap kafudaapp.ConfigSnapshot) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.Gateway = snap.Gateway
	m.gatewayConfigInvalid = snap.GatewayConfigInvalid
	catalog := m.Catalog
	commerce := m.Commerce
	m.mu.Unlock()
	if catalog != nil {
		catalog.ApplyRuntime(snap.Gateway, snap.Config.CatalogTTL())
	}
	if commerce != nil {
		commerce.UpdatePolicy(
			snap.Config.SalesEnabled && snap.Gateway != nil && !snap.LoadError,
			snap.Config.ReferenceLookupVerified,
			snap.Config.ProductCode,
			snap.Config.MaxCostCents,
			snap.Config.AllowedKinds,
		)
	}
}

// RefreshConfig checks for cross-process DB setting updates when the local check cache is stale.
func (m *Module) RefreshConfig(ctx context.Context) {
	if m == nil || m.Config == nil {
		return
	}
	m.Config.RefreshIfStale(ctx)
}

// GatewayRef returns the active upstream gateway under read lock.
func (m *Module) GatewayRef() kafudacontract.Gateway {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	gw := m.Gateway
	m.mu.RUnlock()
	return gw
}

// CurrentGateway refreshes stale DB settings and returns the active upstream gateway.
func (m *Module) CurrentGateway(ctx context.Context) kafudacontract.Gateway {
	m.RefreshConfig(ctx)
	return m.GatewayRef()
}

// GatewayStatus reports whether an upstream gateway is currently active or invalid.
func (m *Module) GatewayStatus() (configured bool, invalid bool) {
	if m == nil {
		return false, false
	}
	m.mu.RLock()
	configured = m.Gateway != nil
	invalid = m.gatewayConfigInvalid
	m.mu.RUnlock()
	return configured, invalid
}

// WorkerReady reports whether the background queue worker is ready to process Kafuda tasks.
func (m *Module) WorkerReady(ctx context.Context) bool {
	if m == nil || m.workerReady == nil {
		return false
	}
	return m.workerReady(ctx)
}

// PollInterval returns the active order polling interval, preferring DB settings over YAML fallback.
func (m *Module) PollInterval(fallback time.Duration) time.Duration {
	if m != nil && m.Config != nil {
		if snap := m.Config.Snapshot(); snap.Source == kafudaapp.ConfigSourceDatabase {
			return snap.Config.PollInterval()
		}
	}
	return fallback
}

// PollReviewAfter returns the active unresolved-order review threshold, preferring DB settings over YAML fallback.
func (m *Module) PollReviewAfter(fallback time.Duration) time.Duration {
	if m != nil && m.Config != nil {
		if snap := m.Config.Snapshot(); snap.Source == kafudaapp.ConfigSourceDatabase {
			return snap.Config.PollReviewAfter()
		}
	}
	return fallback
}

// CatalogRefreshSchedule returns the active background catalog refresh interval and fixed-interval flag.
func (m *Module) CatalogRefreshSchedule(fallbackInterval time.Duration, fallbackFixed bool) (time.Duration, bool) {
	if m != nil && m.Config != nil {
		if snap := m.Config.Snapshot(); snap.Source == kafudaapp.ConfigSourceDatabase {
			return snap.Config.CatalogRefreshInterval(), snap.Config.CatalogFixedRefresh()
		}
	}
	return fallbackInterval, fallbackFixed
}

func syncLegacyManualOrders(db *gorm.DB, orders ordercontract.Store) error {
	var childIDs []uint
	if err := db.Model(&kafudadomain.Recharge{}).
		Joins("JOIN orders AS child ON child.id = kafuda_gpt_recharges.order_id").
		Joins("JOIN orders AS parent ON parent.id = child.parent_id").
		Where("stage IN ? AND completed_at IS NULL", []kafudadomain.Stage{kafudadomain.StageManualReview, kafudadomain.StageManualDelivery}).
		Where("child.status = ? AND parent.status = ? AND child.deleted_at IS NULL AND parent.deleted_at IS NULL", constants.OrderStatusPaid, constants.OrderStatusPaid).
		Pluck("kafuda_gpt_recharges.order_id", &childIDs).Error; err != nil {
		return err
	}
	for _, childID := range childIDs {
		err := kafudaworkflow.SyncManualShopOrderStatus(orders, childID)
		if err != nil && !errors.Is(err, orderapp.ErrOrderStatusInvalid) && !errors.Is(err, orderapp.ErrOrderNotFound) {
			return err
		}
	}
	return nil
}
