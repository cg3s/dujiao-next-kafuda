package orderwiring

import (
	"context"
	"errors"
	"time"

	"github.com/dujiao-next/internal/app/container"
	"github.com/dujiao-next/internal/logger"
	kafudaapp "github.com/dujiao-next/internal/modules/kafudagpt/application"
	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
	orderdomain "github.com/dujiao-next/internal/modules/order/domain"
	orderqueue "github.com/dujiao-next/internal/modules/order/infrastructure/queueadapter"
	ordertransport "github.com/dujiao-next/internal/modules/order/transport/http"
	quefacontract "github.com/dujiao-next/internal/modules/quefa/contract"
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
	var refundWindowExpired func(*orderdomain.Order) (bool, error)
	if c.OrderRefundService != nil {
		refundWindowExpired = c.OrderRefundService.RefundWindowExpired
	}
	handlers := Handlers{
		Admin: ordertransport.NewAdminHandler(
			orderAdminQueryAdapter{orders: c.OrderService},
			orderAdminUserAdapter{users: c.UserStore},
			orderAdminCouponAdapter{coupons: c.CouponRepo},
			orderAdminPromotionAdapter{promotions: c.PromotionRepo},
			orderAdminPaymentAdapter{payments: c.PaymentStore},
			orderAdminPaymentChannelAdapter{channels: c.PaymentChannelStore},
			orderAdminAccountingAdapter{settings: c.SettingService},
			refundWindowExpired,
			newAdminDirectStatusReader(c),
			func(childID, attemptID, adminID uint, confirmed bool) error {
				if c.OrderStore == nil {
					return ordertransport.ErrOrderNotFound
				}
				return mapOrderTransportError(kafudaworkflow.CompleteManualAndNotify(context.Background(), c.OrderStore, kafudaRecords(c), orderqueue.NewDirectCompletion(c.QueueClient),
					c.SettingService, c.Config, childID, attemptID, adminID, confirmed, time.Now().UTC()))
			},
			func(childID, attemptID, adminID uint, confirmed bool) error {
				if c.OrderStore == nil {
					return ordertransport.ErrOrderNotFound
				}
				return mapOrderTransportError(kafudaworkflow.ReleaseManual(c.OrderStore, childID, attemptID, adminID, confirmed, time.Now().UTC()))
			},
			func(ctx context.Context, childID, attemptID, adminID uint, upstreamID uint64) error {
				if c.OrderStore == nil || c.KafudaGPT == nil {
					return ordertransport.ErrOrderNotFound
				}
				gw := c.KafudaGPT.CurrentGateway(ctx)
				if gw == nil {
					return ordertransport.ErrOrderNotFound
				}
				completed, err := kafudaworkflow.ReconcileUnknownOrder(ctx, c.OrderStore, gw, childID, attemptID, upstreamID, c.KafudaGPT.PollInterval(c.Config.KafudaPollInterval()))
				if err != nil {
					return mapOrderTransportError(err)
				}
				logger.Infow("kafuda_unknown_order_reconciled", "child_order_id", childID, "attempt_id", attemptID, "admin_id", adminID)
				if completed {
					kafudaworkflow.EnqueueCompletionEmail(ctx, c.OrderStore, c.KafudaGPT.Store, orderqueue.NewDirectCompletion(c.QueueClient), c.SettingService, c.Config, childID)
				}
				return nil
			},
			func(childID, attemptID, adminID uint, confirmed bool) error {
				records := kafudaRecords(c)
				if c.OrderStore == nil || records == nil {
					return ordertransport.ErrOrderNotFound
				}
				return mapOrderTransportError(kafudaworkflow.ConfirmUpstreamCompletionAndNotify(context.Background(), c.OrderStore, records,
					orderqueue.NewDirectCompletion(c.QueueClient), c.SettingService, c.Config, childID, attemptID, adminID, confirmed, time.Now().UTC()))
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
	handlers.User.SetDirectAuthorizer(newDirectAuthorizer(c))
	handlers.Guest.SetDirectStatusReader(newGuestDirectStatusReader(c))
	handlers.Guest.SetDirectAuthorizer(newGuestDirectAuthorizer(c))
	if c.Quefa != nil && c.Quefa.Orders != nil && c.OrderStore != nil {
		qOrders := c.Quefa.Orders
		oStore := c.OrderStore
		handlers.User.SetHostedRecharge(
			func(ctx context.Context, orderNo string, userID uint) (*quefacontract.BuyerStatus, error) {
				order, err := oStore.GetByOrderNoAndUser(orderNo, userID)
				if err != nil || order == nil || !order.HasQuefaHosted() {
					return nil, err
				}
				return qOrders.GetBuyerStatusByParentOrder(ctx, order.ID)
			},
			func(ctx context.Context, orderNo string, userID uint) (*quefacontract.FulfillmentEntryResult, error) {
				order, err := oStore.GetByOrderNoAndUser(orderNo, userID)
				if err != nil {
					return nil, err
				}
				if order == nil || !order.HasQuefaHosted() {
					return nil, ordertransport.ErrOrderNotFound
				}
				return qOrders.OpenFulfillmentEntry(ctx, order.ID)
			},
		)
		handlers.Guest.SetHostedRecharge(
			func(ctx context.Context, orderNo, email, password string) (*quefacontract.BuyerStatus, error) {
				order, err := oStore.GetByOrderNoAndGuest(orderNo, email, password)
				if err != nil || order == nil || !order.HasQuefaHosted() {
					return nil, err
				}
				return qOrders.GetBuyerStatusByParentOrder(ctx, order.ID)
			},
			func(ctx context.Context, orderNo, email, password string) (*quefacontract.FulfillmentEntryResult, error) {
				order, err := oStore.GetByOrderNoAndGuest(orderNo, email, password)
				if err != nil {
					return nil, err
				}
				if order == nil || !order.HasQuefaHosted() {
					return nil, ordertransport.ErrGuestOrderNotFound
				}
				return qOrders.OpenFulfillmentEntry(ctx, order.ID)
			},
		)
	}
	return handlers
}

func mapDirectAuthorizationError(err error) error {
	switch {
	case errors.Is(err, kafudacontract.ErrCheckSessionInvalid):
		return ordertransport.ErrDirectSessionInvalid
	case errors.Is(err, kafudaworkflow.ErrBuyerCheckRetry), errors.Is(err, kafudaapp.ErrQuoteExpired):
		return ordertransport.ErrDirectRetry
	case errors.Is(err, kafudadomain.ErrNotFound):
		return ordertransport.ErrOrderNotFound
	case errors.Is(err, kafudadomain.ErrInvalidTransition):
		return ordertransport.ErrOrderStatusInvalid
	case err == kafudaapp.ErrQuotePolicy, err == kafudaapp.ErrQuoteUnavailable, err == kafudaapp.ErrQuoteRegionLocked,
		err == kafudacontract.ErrMerchantBalanceInsufficient:
		return nil
	default:
		return mapOrderTransportError(err)
	}
}

func newDirectAuthorizer(c *container.Container) ordertransport.DirectAuthorizer {
	if c.OrderStore == nil || c.KafudaGPT == nil || c.KafudaGPT.Records == nil || c.KafudaGPT.Store == nil {
		return nil
	}
	module := c.KafudaGPT
	return func(ctx context.Context, orderNo string, userID uint, session string) error {
		gw := module.CurrentGateway(ctx)
		if gw == nil {
			return ordertransport.ErrDirectRetry
		}
		childID, completed, err := kafudaworkflow.SubmitBuyerSession(ctx, c.OrderStore, module.Records, gw,
			orderNo, userID, session, module.PollInterval(c.Config.KafudaPollInterval()))
		if completed {
			kafudaworkflow.EnqueueCompletionEmail(ctx, c.OrderStore, module.Store, orderqueue.NewDirectCompletion(c.QueueClient), c.SettingService, c.Config, childID)
		}
		return mapDirectAuthorizationError(err)
	}
}

func newGuestDirectAuthorizer(c *container.Container) ordertransport.GuestDirectAuthorizer {
	if c.OrderStore == nil || c.KafudaGPT == nil || c.KafudaGPT.Records == nil || c.KafudaGPT.Store == nil {
		return nil
	}
	module := c.KafudaGPT
	return func(ctx context.Context, orderNo, email, password, session string) error {
		gw := module.CurrentGateway(ctx)
		if gw == nil {
			return ordertransport.ErrDirectRetry
		}
		childID, completed, err := kafudaworkflow.SubmitGuestSession(ctx, c.OrderStore, module.Records, gw,
			orderNo, email, password, session, module.PollInterval(c.Config.KafudaPollInterval()))
		if completed {
			kafudaworkflow.EnqueueCompletionEmail(ctx, c.OrderStore, module.Store, orderqueue.NewDirectCompletion(c.QueueClient), c.SettingService, c.Config, childID)
		}
		return mapDirectAuthorizationError(err)
	}
}

func kafudaRecords(c *container.Container) kafudacontract.Store {
	if c.KafudaGPT == nil {
		return nil
	}
	return c.KafudaGPT.Store
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
