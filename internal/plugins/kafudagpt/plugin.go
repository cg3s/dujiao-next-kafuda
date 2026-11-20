//go:build !no_kafuda

package kafudagpt

import (
	kafudawiring "github.com/dujiao-next/internal/bootstrap/kafudagpt"
	"github.com/dujiao-next/internal/config"
	"github.com/dujiao-next/internal/constants"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
	kafudahttp "github.com/dujiao-next/internal/modules/kafudagpt/transport/http"
	ordercontract "github.com/dujiao-next/internal/modules/order/contract"
	"github.com/dujiao-next/internal/plugin/directfulfillment"
	"github.com/dujiao-next/internal/queue"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func init() {
	directfulfillment.Register(&Plugin{})
}

// Plugin adapts the Kafuda GPT direct-recharge module to the directfulfillment SPI.
type Plugin struct{}

func (*Plugin) Name() string {
	return "kafuda_gpt"
}

func (*Plugin) FulfillmentType() string {
	return constants.FulfillmentTypeKafudaGPT
}

func (*Plugin) Init(db *gorm.DB, cfg *config.Config, orders ordercontract.Store, queueClient *queue.Client) (any, error) {
	return kafudawiring.New(db, cfg.KafudaGPT, orders, queueClient.KafudaWorkerReady)
}

func (*Plugin) RegisterAdminRoutes(authorized gin.IRoutes, instance any) {
	mod, _ := instance.(*kafudawiring.Module)
	kafudahttp.RegisterAdminRoutes(authorized, kafudawiring.NewAdminHandler(mod))
}

func (*Plugin) RBACRules() []directfulfillment.RBACRule {
	return []directfulfillment.RBACRule{
		{Role: "operations", Object: "/admin/kafuda-gpt/catalog", Action: "GET"},
		{Role: "operations", Object: "/admin/kafuda-gpt/catalog/sync", Action: "POST"},
	}
}

func (*Plugin) AutoMigrateModels() []any {
	return []any{
		&kafudadomain.Catalog{},
		&kafudadomain.Recharge{},
		&kafudadomain.Attempt{},
	}
}

func (*Plugin) PostMigrate(db *gorm.DB) error {
	return backfillKafudaTerminalFailures(db)
}

// backfillKafudaTerminalFailures restores after-sales eligibility for over-cap
// recharges that have a verified terminal failure on their current attempt.
func backfillKafudaTerminalFailures(db *gorm.DB) error {
	terminalAttempt := db.Model(&kafudadomain.Attempt{}).Select("1").
		Where("kafuda_gpt_attempts.id = kafuda_gpt_recharges.current_attempt_id").
		Where("kafuda_gpt_attempts.order_id = kafuda_gpt_recharges.order_id").
		Where("status IN ?", []kafudadomain.AttemptStatus{kafudadomain.AttemptFailed, kafudadomain.AttemptCancelled}).
		Where("upstream_order_id > 0 AND active_order_id IS NULL AND next_poll_at IS NULL")
	return db.Model(&kafudadomain.Recharge{}).
		Where("stage = ? AND reason_code = ? AND completed_at IS NULL", kafudadomain.StageManualReview, kafudadomain.ReasonCostLimitExceeded).
		Where("EXISTS (?)", terminalAttempt).
		UpdateColumns(map[string]any{"stage": kafudadomain.StageManualDelivery, "version": gorm.Expr("version + 1")}).Error
}
