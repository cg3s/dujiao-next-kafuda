package kafudawiring

import (
	"time"

	"github.com/dujiao-next/internal/config"
	kafudaapp "github.com/dujiao-next/internal/modules/kafudagpt/application"
	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	kafudastore "github.com/dujiao-next/internal/modules/kafudagpt/infrastructure/gormstore"
	"github.com/dujiao-next/internal/modules/kafudagpt/infrastructure/kafuda"

	"gorm.io/gorm"
)

// Module is assembled independently of the app container, so transports and
// workers can consume module contracts without a container dependency.
type Module struct {
	Store    kafudacontract.Store
	Records  *kafudaapp.Service
	Catalog  *kafudaapp.CatalogService
	Gateway  kafudacontract.Gateway
	Commerce *kafudaapp.CommerceService
}

func New(db *gorm.DB, cfg config.KafudaGPTConfig) (*Module, error) {
	store := kafudastore.New(db)
	var gateway kafudacontract.Gateway
	if cfg.APIKey != "" {
		client, err := kafuda.New(cfg.APIBaseURL, cfg.APIKey)
		if err != nil {
			return nil, err
		}
		gateway = client
	}
	catalog := kafudaapp.NewCatalogService(store, gateway, "default", time.Duration(cfg.CatalogTTLSeconds)*time.Second)
	return &Module{
		Store: store, Records: kafudaapp.New(store), Gateway: gateway,
		Catalog:  catalog,
		Commerce: kafudaapp.NewCommerceService(kafudaapp.CommerceOptions{Catalog: catalog, SalesEnabled: cfg.SalesEnabled, ReferenceLookupVerified: cfg.ReferenceLookupVerified, ProductCode: cfg.ProductCode, MaxCostCents: cfg.MaxCostCents, AllowedKinds: cfg.AllowedKinds}),
	}, nil
}
