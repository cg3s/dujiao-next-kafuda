package directfulfillment

import (
	"sort"
	"sync"

	"github.com/dujiao-next/internal/config"
	ordercontract "github.com/dujiao-next/internal/modules/order/contract"
	"github.com/dujiao-next/internal/queue"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// RBACRule declares a single admin RBAC policy contributed by a direct-fulfillment plugin.
type RBACRule struct {
	Role   string
	Object string
	Action string
}

// Plugin defines the compile-time SPI for direct-fulfillment plugins.
type Plugin interface {
	// Name returns the unique plugin identifier (e.g. "kafuda_gpt").
	Name() string
	// FulfillmentType returns the product/order fulfillment type handled by this plugin.
	FulfillmentType() string
	// Init initializes the plugin runtime module during container bootstrap.
	Init(db *gorm.DB, cfg *config.Config, orders ordercontract.Store, queueClient *queue.Client) (any, error)
	// RegisterAdminRoutes mounts plugin-specific admin HTTP routes onto the authorized admin router group.
	RegisterAdminRoutes(authorized gin.IRoutes, instance any)
	// RBACRules returns the builtin role policies required by this plugin's admin routes.
	RBACRules() []RBACRule
	// AutoMigrateModels returns GORM models owned by this plugin for database AutoMigrate.
	AutoMigrateModels() []any
	// PostMigrate runs optional plugin-specific data migrations after AutoMigrate completes.
	PostMigrate(db *gorm.DB) error
}

var (
	mu      sync.RWMutex
	plugins = map[string]Plugin{}
)

// Register registers a direct-fulfillment plugin by its FulfillmentType.
// It is intended to be called from plugin package init() functions.
func Register(p Plugin) {
	if p == nil || p.FulfillmentType() == "" {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	plugins[p.FulfillmentType()] = p
}

// Get looks up a registered plugin by fulfillment type.
func Get(fulfillmentType string) (Plugin, bool) {
	mu.RLock()
	defer mu.RUnlock()
	p, ok := plugins[fulfillmentType]
	return p, ok
}

// All returns all registered plugins sorted deterministically by FulfillmentType.
func All() []Plugin {
	mu.RLock()
	defer mu.RUnlock()
	keys := make([]string, 0, len(plugins))
	for k := range plugins {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]Plugin, 0, len(keys))
	for _, k := range keys {
		out = append(out, plugins[k])
	}
	return out
}

// EnabledFulfillmentTypes returns the sorted list of fulfillment types backed by registered plugins.
func EnabledFulfillmentTypes() []string {
	all := All()
	out := make([]string, 0, len(all))
	for _, p := range all {
		out = append(out, p.FulfillmentType())
	}
	return out
}

// EnabledPluginNames returns the sorted list of registered plugin names.
func EnabledPluginNames() []string {
	all := All()
	out := make([]string, 0, len(all))
	for _, p := range all {
		out = append(out, p.Name())
	}
	sort.Strings(out)
	return out
}
