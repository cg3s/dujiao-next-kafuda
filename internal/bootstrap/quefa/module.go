package quefawiring

import (
	"context"
	"time"

	"github.com/dujiao-next/internal/config"
	quefaapp "github.com/dujiao-next/internal/modules/quefa/application"
	quefacontract "github.com/dujiao-next/internal/modules/quefa/contract"
	quefadomain "github.com/dujiao-next/internal/modules/quefa/domain"
	quefaclient "github.com/dujiao-next/internal/modules/quefa/infrastructure/client"
	quefastore "github.com/dujiao-next/internal/modules/quefa/infrastructure/gormstore"
	"gorm.io/gorm"
)

// Module is assembled independently of the app container.
type Module struct {
	Store   quefacontract.Store
	Config  *quefaapp.ConfigService
	Catalog *quefaapp.CatalogService
	Orders  *quefaapp.OrderService
}

func NewWithSettings(db *gorm.DB, cfg config.QuefaConfig, appSecretKey string, workerReady func(context.Context) bool) (*Module, error) {
	store := quefastore.New(db)
	fallback := quefaapp.RuntimeConfig{
		APIBaseURL:                    cfg.APIBaseURL,
		PartnerID:                     cfg.PartnerID,
		KeyID:                         cfg.KeyID,
		ClientSecret:                  cfg.ClientSecret,
		WebhookSecret:                 cfg.WebhookSecret,
		CollectionMode:                cfg.CollectionMode,
		SalesEnabled:                  cfg.SalesEnabled,
		AllowedPayDomains:             append([]string(nil), cfg.AllowedPayDomains...),
		CatalogTTLSeconds:             cfg.CatalogTTLSeconds,
		CatalogRefreshIntervalSeconds: cfg.CatalogRefreshIntervalSeconds,
		PollIntervalSeconds:           cfg.PollIntervalSeconds,
		PollReviewAfterSeconds:        cfg.PollReviewAfterSeconds,
		WebhookMaxSkewSeconds:         cfg.WebhookMaxSkewSeconds,
	}
	factory := func(baseURL, partnerID, keyID, clientSecret string) (quefacontract.Gateway, error) {
		return quefaclient.New(baseURL, partnerID, keyID, clientSecret)
	}
	configSvc := quefaapp.NewConfigService(context.Background(), quefaapp.ConfigServiceOptions{
		Store:          store,
		Profile:        quefadomain.DefaultProfile,
		AppSecretKey:   appSecretKey,
		Fallback:       fallback,
		GatewayFactory: factory,
	})
	catalogSvc := quefaapp.NewCatalogService(store, configSvc, quefadomain.DefaultProfile, workerReady)
	configSvc.SetOnApply(func(quefaapp.ConfigSnapshot) {
		catalogSvc.ResetRefreshSchedule()
	})
	orderSvc := quefaapp.NewOrderService(store, configSvc)
	return &Module{
		Store:   store,
		Config:  configSvc,
		Catalog: catalogSvc,
		Orders:  orderSvc,
	}, nil
}

// RefreshConfig reloads persisted DB settings when the reload interval has elapsed.
func (m *Module) RefreshConfig(ctx context.Context) {
	if m == nil || m.Config == nil {
		return
	}
	m.Config.RefreshIfStale(ctx)
}

// HasGateway reports whether a valid upstream Quefa API gateway is currently configured.
func (m *Module) HasGateway() bool {
	if m == nil || m.Config == nil {
		return false
	}
	snap := m.Config.Snapshot()
	return snap.Gateway != nil && !snap.GatewayConfigInvalid && !snap.LoadError
}

// PollInterval returns the active order polling interval, preferring DB settings over YAML fallback.
func (m *Module) PollInterval(fallback time.Duration) time.Duration {
	if m != nil && m.Config != nil {
		if snap := m.Config.Snapshot(); snap.Source == quefaapp.ConfigSourceDatabase {
			return snap.Config.PollInterval()
		}
	}
	return fallback
}

// CatalogRefreshSchedule returns the active background catalog refresh interval and fixed-interval flag.
func (m *Module) CatalogRefreshSchedule(fallbackInterval time.Duration, fallbackFixed bool) (time.Duration, bool) {
	if m != nil && m.Config != nil {
		if snap := m.Config.Snapshot(); snap.Source == quefaapp.ConfigSourceDatabase {
			return snap.Config.CatalogRefreshInterval(), snap.Config.CatalogFixedRefresh()
		}
	}
	return fallbackInterval, fallbackFixed
}
