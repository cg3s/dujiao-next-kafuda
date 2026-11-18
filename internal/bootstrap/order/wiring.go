package orderwiring

import (
	"context"
	"time"

	"github.com/dujiao-next/internal/app/container"
	"github.com/dujiao-next/internal/logger"
	orderqueue "github.com/dujiao-next/internal/modules/order/infrastructure/queueadapter"
	ordertransport "github.com/dujiao-next/internal/modules/order/transport/http"
	kafudaworkflow "github.com/dujiao-next/internal/workflows/kafudagpt/application"
)

// Handlers contains every order HTTP entrypoint required by the router.
type Handlers struct {
	Admin       *ordertransport.AdminHandler
	AdminRefund *ordertransport.AdminRefundHandler
	User        *ordertransport.UserHandler
	Guest       *ordertransport.GuestHandler
	Preview     *ordertransport.PreviewHandler
	Create      *ordertransport.CreateHandler
}

// New assembles order transports and their composition-boundary adapters.
func New(c *container.Container) Handlers {
	return Handlers{
		Admin: ordertransport.NewAdminHandler(
			orderAdminQueryAdapter{orders: c.OrderService},
			orderAdminUserAdapter{users: c.UserStore},
			orderAdminCouponAdapter{coupons: c.CouponRepo},
			orderAdminPromotionAdapter{promotions: c.PromotionRepo},
			orderAdminPaymentAdapter{payments: c.PaymentStore},
			orderAdminPaymentChannelAdapter{channels: c.PaymentChannelStore},
			orderAdminAccountingAdapter{settings: c.SettingService},
			newAdminDirectStatusReader(c),
			func(childID, attemptID, adminID uint, confirmed bool) error {
				if c.OrderStore == nil {
					return ordertransport.ErrOrderNotFound
				}
				return mapOrderTransportError(kafudaworkflow.CompleteManualAndNotify(c.OrderStore, orderqueue.New(c.QueueClient),
					c.SettingService, c.Config, childID, attemptID, adminID, confirmed, time.Now().UTC()))
			},
			func(childID, attemptID, adminID uint, confirmed bool) error {
				if c.OrderStore == nil {
					return ordertransport.ErrOrderNotFound
				}
				return mapOrderTransportError(kafudaworkflow.ReleaseManual(c.OrderStore, childID, attemptID, adminID, confirmed, time.Now().UTC()))
			},
			func(ctx context.Context, childID, attemptID, adminID uint, upstreamID uint64) error {
				if c.OrderStore == nil || c.KafudaGPT == nil || c.KafudaGPT.Gateway == nil {
					return ordertransport.ErrOrderNotFound
				}
				completed, err := kafudaworkflow.ReconcileUnknownOrder(ctx, c.OrderStore, c.KafudaGPT.Gateway, childID, attemptID, upstreamID, c.Config.KafudaPollInterval())
				if err != nil {
					return mapOrderTransportError(err)
				}
				logger.Infow("kafuda_unknown_order_reconciled", "child_order_id", childID, "attempt_id", attemptID, "admin_id", adminID)
				if completed {
					kafudaworkflow.EnqueueCompletionEmail(c.OrderStore, orderqueue.New(c.QueueClient), c.SettingService, c.Config, childID)
				}
				return nil
			},
		),
		AdminRefund: NewAdminRefundHandler(c),
		User: ordertransport.NewUserHandler(
			orderUserQueryAdapter{orders: c.OrderService},
			orderUserPaymentChannelAdapter{payments: c.PaymentService},
			orderUserRefundRecordAdapter{records: c.OrderStore},
			orderUserLookupAdapter{users: c.UserStore},
			newDirectStatusReader(c),
		),
		Guest: ordertransport.NewGuestHandler(
			orderGuestQueryAdapter{orders: c.OrderService},
			orderUserPaymentChannelAdapter{payments: c.PaymentService},
			orderUserRefundRecordAdapter{records: c.OrderStore},
		),
		Preview: ordertransport.NewPreviewHandler(
			orderPreviewAdapter{orders: c.OrderService},
		),
		Create: ordertransport.NewCreateHandler(
			orderCreateAdapter{orders: c.OrderService},
			orderUserPaymentChannelAdapter{payments: c.PaymentService},
			orderGuestCreateCaptchaAdapter{captcha: c.CaptchaService},
			orderPaymentCreatorAdapter{payments: c.PaymentService},
		),
	}
}

// NewAdminRefundHandler exposes the focused refund composition for integration
// tests and command surfaces that do not need the complete order handler set.
func NewAdminRefundHandler(c *container.Container) *ordertransport.AdminRefundHandler {
	refunds := orderAdminRefundAdapter{refunds: c.OrderRefundService}
	return ordertransport.NewAdminRefundHandler(
		refunds,
		refunds,
		orderAdminWalletRefundAdapter{refunds: c.OrderRefundService},
		orderAdminOrderLookupAdapter{orders: c.OrderStore},
		orderAdminStatusEmailAdapter{queue: c.QueueClient},
	)
}
