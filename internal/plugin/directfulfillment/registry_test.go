package directfulfillment

import (
	"testing"

	"github.com/dujiao-next/internal/config"
	ordercontract "github.com/dujiao-next/internal/modules/order/contract"
	"github.com/dujiao-next/internal/queue"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type stubPlugin struct {
	name            string
	fulfillmentType string
}

func (s stubPlugin) Name() string            { return s.name }
func (s stubPlugin) FulfillmentType() string { return s.fulfillmentType }
func (s stubPlugin) Init(*gorm.DB, *config.Config, ordercontract.Store, *queue.Client) (any, error) {
	return nil, nil
}
func (s stubPlugin) RegisterAdminRoutes(gin.IRoutes, any) {}
func (s stubPlugin) RBACRules() []RBACRule                     { return nil }
func (s stubPlugin) AutoMigrateModels() []any                  { return nil }
func (s stubPlugin) PostMigrate(*gorm.DB) error                { return nil }

func TestRegistryRegisterAndQuery(t *testing.T) {
	mu.Lock()
	saved := plugins
	plugins = map[string]Plugin{}
	mu.Unlock()
	t.Cleanup(func() {
		mu.Lock()
		plugins = saved
		mu.Unlock()
	})

	Register(stubPlugin{name: "beta", fulfillmentType: "direct_beta"})
	Register(stubPlugin{name: "alpha", fulfillmentType: "direct_alpha"})

	if _, ok := Get("direct_alpha"); !ok {
		t.Fatalf("expected direct_alpha plugin to be registered")
	}
	if _, ok := Get("missing"); ok {
		t.Fatalf("expected missing plugin not to be registered")
	}

	types := EnabledFulfillmentTypes()
	if len(types) != 2 || types[0] != "direct_alpha" || types[1] != "direct_beta" {
		t.Fatalf("unexpected fulfillment types: %v", types)
	}

	names := EnabledPluginNames()
	if len(names) != 2 || names[0] != "alpha" || names[1] != "beta" {
		t.Fatalf("unexpected plugin names: %v", names)
	}
}
