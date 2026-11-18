package kafudawiring

import (
	"errors"
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
	Store    kafudacontract.Store
	Records  *kafudaapp.Service
	Catalog  *kafudaapp.CatalogService
	Gateway  kafudacontract.Gateway
	Commerce *kafudaapp.CommerceService
}

func New(db *gorm.DB, cfg config.KafudaGPTConfig, orders ordercontract.Store) (*Module, error) {
	store := kafudastore.New(db)
	var gateway kafudacontract.Gateway
	if cfg.APIKey != "" {
		client, err := kafuda.New(cfg.APIBaseURL, cfg.APIKey)
		if err != nil {
			return nil, err
		}
		gateway = client
	}
	if err := syncLegacyManualOrders(db, orders); err != nil {
		return nil, err
	}
	catalog := kafudaapp.NewCatalogService(store, gateway, "default", time.Duration(cfg.CatalogTTLSeconds)*time.Second)
	return &Module{
		Store: store, Records: kafudaworkflow.WithShopOrderTransitions(kafudaapp.New(store), orders), Gateway: gateway,
		Catalog:  catalog,
		Commerce: kafudaapp.NewCommerceService(kafudaapp.CommerceOptions{Catalog: catalog, SalesEnabled: cfg.SalesEnabled, ReferenceLookupVerified: cfg.ReferenceLookupVerified, ProductCode: cfg.ProductCode, MaxCostCents: cfg.MaxCostCents, AllowedKinds: cfg.AllowedKinds}),
	}, nil
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
