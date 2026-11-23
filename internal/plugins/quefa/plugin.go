package quefa

import (
	"context"

	quefawiring "github.com/dujiao-next/internal/bootstrap/quefa"
	"github.com/dujiao-next/internal/config"
	"github.com/dujiao-next/internal/constants"
	ordercontract "github.com/dujiao-next/internal/modules/order/contract"
	quefadomain "github.com/dujiao-next/internal/modules/quefa/domain"
	quefahttp "github.com/dujiao-next/internal/modules/quefa/transport/http"
	"github.com/dujiao-next/internal/plugin/directfulfillment"
	"github.com/dujiao-next/internal/queue"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func init() {
	directfulfillment.Register(&Plugin{})
}

// Plugin adapts the Quefa hosted direct-recharge module to the directfulfillment SPI.
type Plugin struct{}

func (*Plugin) Name() string {
	return "quefa_hosted"
}

func (*Plugin) FulfillmentType() string {
	return constants.FulfillmentTypeQuefaHosted
}

func (*Plugin) Init(db *gorm.DB, cfg *config.Config, _ ordercontract.Store, queueClient *queue.Client) (any, error) {
	var workerReady func(context.Context) bool
	if queueClient != nil {
		workerReady = queueClient.QuefaWorkerReady
	}
	return quefawiring.NewWithSettings(db, cfg.Quefa, cfg.App.SecretKey, workerReady)
}

func (*Plugin) RegisterAdminRoutes(authorized gin.IRoutes, instance any) {
	mod, _ := instance.(*quefawiring.Module)
	quefahttp.RegisterAdminRoutes(authorized, quefawiring.NewAdminHandler(mod))
}

func (*Plugin) RBACRules() []directfulfillment.RBACRule {
	return []directfulfillment.RBACRule{
		{Role: "integration", Object: "/admin/quefa/config", Action: "GET"},
		{Role: "integration", Object: "/admin/quefa/config", Action: "PUT"},
		{Role: "integration", Object: "/admin/quefa/config/test", Action: "POST"},
		{Role: "integration", Object: "/admin/quefa/catalog", Action: "GET"},
		{Role: "integration", Object: "/admin/quefa/catalog/sync", Action: "POST"},
		{Role: "operations", Object: "/admin/quefa/catalog", Action: "GET"},
		{Role: "operations", Object: "/admin/quefa/catalog/sync", Action: "POST"},
		{Role: "support", Object: "/admin/orders/:id/quefa", Action: "GET"},
		{Role: "support", Object: "/admin/orders/:id/quefa/sync", Action: "POST"},
		{Role: "support", Object: "/admin/orders/:id/quefa/mark-review", Action: "POST"},
	}
}

func (*Plugin) AutoMigrateModels() []any {
	return []any{
		&quefadomain.Setting{},
		&quefadomain.Catalog{},
		&quefadomain.Order{},
		&quefadomain.WebhookEvent{},
	}
}

func (*Plugin) PostMigrate(_ *gorm.DB) error {
	return nil
}
