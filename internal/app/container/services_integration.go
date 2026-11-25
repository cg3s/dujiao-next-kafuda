package container

import (
	"context"
	catalogmappingbootstrap "github.com/dujiao-next/internal/bootstrap/catalogmapping"
	telegrambroadcast "github.com/dujiao-next/internal/bootstrap/telegrambroadcast"
	"github.com/dujiao-next/internal/logger"
	apicredentialapp "github.com/dujiao-next/internal/modules/apicredential/application"
	auditlogapp "github.com/dujiao-next/internal/modules/auditlog/application"
	channelclientapp "github.com/dujiao-next/internal/modules/channelclient/application"
	contentapp "github.com/dujiao-next/internal/modules/content/application"
	localfilestore "github.com/dujiao-next/internal/modules/content/infrastructure/filestore/local"
	contentgormstore "github.com/dujiao-next/internal/modules/content/infrastructure/gormstore"
	dashboardapp "github.com/dujiao-next/internal/modules/dashboard/application"
	"time"

	downstreamcallbackapp "github.com/dujiao-next/internal/modules/downstreamcallback/application"
	downstreamcallbackcontract "github.com/dujiao-next/internal/modules/downstreamcallback/contract"
	downstreamcallbackclient "github.com/dujiao-next/internal/modules/downstreamcallback/infrastructure/callbackclient"
	downstreamcallbackcredentialreader "github.com/dujiao-next/internal/modules/downstreamcallback/infrastructure/credentialreader"
	downstreamcallbackorderreader "github.com/dujiao-next/internal/modules/downstreamcallback/infrastructure/orderreader"
	downstreamcallbackqueue "github.com/dujiao-next/internal/modules/downstreamcallback/infrastructure/queueadapter"
	adminapitokenapp "github.com/dujiao-next/internal/modules/identity/adminapitoken/application"
	notificationapp "github.com/dujiao-next/internal/modules/notification/application"
	notificationasyncqueue "github.com/dujiao-next/internal/modules/notification/infrastructure/asyncqueue"
	notificationfeishu "github.com/dujiao-next/internal/modules/notification/infrastructure/feishu"
	orderqueue "github.com/dujiao-next/internal/modules/order/infrastructure/queueadapter"
	paymentapp "github.com/dujiao-next/internal/modules/payment/application"
	paymentqueue "github.com/dujiao-next/internal/modules/payment/infrastructure/queueadapter"
	procurementapp "github.com/dujiao-next/internal/modules/procurement/application"
	procurementmapping "github.com/dujiao-next/internal/modules/procurement/infrastructure/mappingreader"
	procurementnotification "github.com/dujiao-next/internal/modules/procurement/infrastructure/notificationadapter"
	procurementorder "github.com/dujiao-next/internal/modules/procurement/infrastructure/orderreader"
	procurementqueue "github.com/dujiao-next/internal/modules/procurement/infrastructure/queueadapter"
	procurementupstream "github.com/dujiao-next/internal/modules/procurement/infrastructure/upstreamgateway"
	quefaapp "github.com/dujiao-next/internal/modules/quefa/application"
	quefacontract "github.com/dujiao-next/internal/modules/quefa/contract"
	quefadomain "github.com/dujiao-next/internal/modules/quefa/domain"
	reconciliationapp "github.com/dujiao-next/internal/modules/reconciliation/application"
	reconciliationnotification "github.com/dujiao-next/internal/modules/reconciliation/infrastructure/notificationadapter"
	reconciliationprocurement "github.com/dujiao-next/internal/modules/reconciliation/infrastructure/procurementreader"
	reconciliationqueue "github.com/dujiao-next/internal/modules/reconciliation/infrastructure/queueadapter"
	reconciliationupstream "github.com/dujiao-next/internal/modules/reconciliation/infrastructure/upstreamreader"
	siteconnectionapp "github.com/dujiao-next/internal/modules/siteconnection/application"
	broadcastapp "github.com/dujiao-next/internal/modules/telegram/broadcast/application"
	notifyapp "github.com/dujiao-next/internal/modules/telegram/notify/application"
	notifybotapi "github.com/dujiao-next/internal/modules/telegram/notify/infrastructure/botapi"
	"github.com/dujiao-next/internal/platform/database/gormdb"
	quefaworkflow "github.com/dujiao-next/internal/workflows/quefa/application"
)

// initIntegrationServices 装配通知、站点对接、支付、采购、渠道与 Telegram 集成。
func (c *Container) initIntegrationServices() {
	c.AdminAPITokenService = adminapitokenapp.NewService(c.AdminAPITokenStore, c.AdminStore)
	c.UserLoginLogService = auditlogapp.NewUserLoginService(c.UserLoginLogRepo)
	c.AuthzAuditService = auditlogapp.NewAuthzService(c.AuthzAuditLogRepo)
	c.AdminLoginLogService = auditlogapp.NewAdminLoginService(c.AdminLoginLogRepo)
	c.NotificationLogService = notificationapp.NewLogService(c.NotificationLogRepo)
	c.DashboardService = dashboardapp.NewService(c.DashboardRepo, c.SettingService)
	telegramNotifyService := notifyapp.NewService(c.SettingService, c.Config.TelegramAuth, notifybotapi.New())
	c.NotificationService = notificationapp.NewService(
		c.SettingService,
		c.EmailSender,
		notificationasyncqueue.New(c.QueueClient),
		c.DashboardService,
		c.NotificationLogService,
		telegramNotifyService,
		notificationfeishu.New(),
	)
	c.ApiCredentialService = apicredentialapp.NewService(c.ApiCredentialRepo)
	c.SiteConnectionService = siteconnectionapp.NewService(c.SiteConnectionRepo, c.Config.App.SecretKey, "uploads")
	mediaCore := contentapp.NewMediaService(
		contentgormstore.NewMediaStore(gormdb.DB),
		localfilestore.New(),
		contentapp.WarningLoggerFunc(logger.Warnw),
	)
	c.ContentMediaService = mediaCore
	productMappingService, err := catalogmappingbootstrap.New(catalogmappingbootstrap.Dependencies{
		Mappings:    c.ProductMappingRepo,
		SKUMappings: c.SKUMappingRepo,
		Products:    c.ProductRepo,
		SKUs:        c.ProductSKURepo,
		Categories:  c.CategoryRepo,
		Connections: c.SiteConnectionService,
		Media:       mediaCore,
	})
	if err != nil {
		logger.Errorw("provider_init_product_mapping_failed", "error", err)
		panic(err)
	}
	c.ProductMappingService = productMappingService
	c.ProductMappingService.SetCategoryCreator(c.CategoryService)
	c.ProductMappingService.SetSettings(c.SettingService)
	c.SiteConnectionService.SetMarkupReapplier(c.ProductMappingService)
	c.OrderService.SetProductMappingService(c.ProductMappingService)
	var downstreamQueue downstreamcallbackcontract.CallbackQueue
	if c.QueueClient != nil {
		downstreamQueue = downstreamcallbackqueue.New(c.QueueClient)
	}
	c.DownstreamCallbackService = downstreamcallbackapp.NewService(downstreamcallbackapp.Options{
		References:  c.DownstreamOrderRefRepo,
		Orders:      downstreamcallbackorderreader.New(c.OrderStore),
		Credentials: downstreamcallbackcredentialreader.New(c.ApiCredentialRepo),
		Queue:       downstreamQueue,
		Deliverer:   downstreamcallbackclient.New(),
	})
	c.PaymentService = paymentapp.NewPaymentService(paymentapp.PaymentServiceOptions{
		QuefaCollectionMode: func() string {
			if c.Quefa == nil || c.Quefa.Orders == nil || c.Quefa.Config == nil {
				return ""
			}
			return c.Quefa.Config.Snapshot().Config.CollectionMode
		},
		QuefaPaidHook: func(parentOrderID uint) {
			if c.Quefa == nil || c.Quefa.Orders == nil {
				return
			}
			rec, err := c.Quefa.Orders.GetOrderByParentOrder(context.Background(), parentOrderID)
			if err != nil || rec == nil || rec.CollectionMode != quefadomain.CollectionModeAgentCollect {
				return
			}
			if c.QueueClient != nil && c.QueueClient.Enabled() {
				_ = c.QueueClient.EnqueueQuefaSyncOrder(context.Background(), rec.OrderID, 0)
			}
		},
		OrderStore:              c.OrderStore,
		ProductRepo:             c.ProductRepo,
		ProductSKURepo:          c.ProductSKURepo,
		PaymentStore:            c.PaymentStore,
		ChannelStore:            c.PaymentChannelStore,
		WalletRepo:              c.WalletRepo,
		UserStore:               c.UserStore,
		ExternalIdentityStore:   c.ExternalIdentityStore,
		Queue:                   paymentqueue.New(c.QueueClient),
		WalletService:           c.WalletService,
		SettingService:          c.SettingService,
		DefaultEmailConfig:      c.Config.Email,
		ExpireMinutes:           c.Config.Order.PaymentExpireMinutes,
		AffiliateService:        c.AffiliateService,
		NotificationService:     c.NotificationService,
		PaymentProviderRegistry: c.PaymentProviderRegistry,
		ResellerAccounting:      c.ResellerAccountingLedger,
	})
	c.ProcurementOrderService = procurementapp.NewService(procurementapp.Options{
		Repository:         c.ProcurementOrderRepo,
		Orders:             procurementorder.New(c.OrderStore),
		ProductMappings:    procurementmapping.NewProducts(c.ProductMappingRepo),
		SKUMappings:        procurementmapping.NewSKUs(c.SKUMappingRepo),
		Connections:        procurementupstream.New(c.SiteConnectionService),
		Queue:              procurementqueue.New(c.QueueClient),
		OrderLifecycle:     c.ProcurementOrderRepo.NewLifecycle(c.QueueClient, c.SettingService, c.Config.Email),
		DownstreamCallback: c.DownstreamCallbackService,
		BotNotifier:        c.FulfillmentService,
		Notifications:      procurementnotification.New(c.NotificationService),
	})
	c.ReconciliationService = reconciliationapp.NewService(reconciliationapp.Options{
		Jobs: c.ReconciliationJobRepo, Items: c.ReconciliationItemRepo,
		Procurements:  reconciliationprocurement.New(c.ProcurementOrderRepo),
		Upstream:      reconciliationupstream.New(c.SiteConnectionService),
		Queue:         reconciliationqueue.New(c.QueueClient),
		Notifications: reconciliationnotification.New(c.NotificationService),
	})
	c.ChannelClientService = channelclientapp.NewService(c.ChannelClientStore, c.Config.App.SecretKey)
	c.TelegramBroadcastService = broadcastapp.NewService(
		c.TelegramBroadcastRepo,
		telegrambroadcast.NewUserDirectory(c.ExternalIdentityStore),
		telegrambroadcast.NewBotTokenResolver(c.ChannelClientService),
		telegrambroadcast.NewDispatcher(c.QueueClient),
		telegramNotifyService,
	)
	c.wireQuefaLifecycle()
}

func (c *Container) wireQuefaLifecycle() {
	if c.Quefa == nil || c.Quefa.Orders == nil {
		return
	}
	if c.OrderService != nil {
		c.OrderService.SetQuefaCancelHooks(
			func(parentOrderID uint) bool {
				eval, err := c.Quefa.Orders.EvaluateCancelOrExpire(context.Background(), parentOrderID)
				return err == nil && eval.CanClose
			},
			func(parentOrderID uint) bool {
				rec, err := c.Quefa.Orders.GetOrderByParentOrder(context.Background(), parentOrderID)
				if err != nil || rec == nil {
					return false
				}
				if rec.CollectionMode != quefadomain.CollectionModePlatformCollect {
					return false
				}
				if !rec.CreateDispatched && (rec.UpstreamOrderID == nil || *rec.UpstreamOrderID == "") {
					return false
				}
				if c.QueueClient != nil && c.QueueClient.Enabled() {
					_ = c.QueueClient.EnqueueQuefaSyncOrder(context.Background(), rec.OrderID, 0)
				}
				return true
			},
		)
	}
	c.Quefa.Orders.SetHooks(quefaapp.LifecycleHooks{
		OnPlatformOrderPaid: func(ctx context.Context, record *quefadomain.Order, snap *quefacontract.OrderSnapshot) error {
			return quefaworkflow.ApplyPlatformCollectPaid(ctx, c.PaymentService, c.PaymentStore, record, snap)
		},
		OnOrderSucceeded: func(ctx context.Context, record *quefadomain.Order) error {
			completed, err := quefaworkflow.CompleteOrder(c.OrderStore, record.OrderID, time.Now().UTC())
			if err != nil {
				return err
			}
			if completed && c.QueueClient != nil && c.QueueClient.Enabled() {
				quefaworkflow.EnqueueCompletionEmail(ctx, c.OrderStore, c.Quefa.Store, orderqueue.NewDirectCompletion(c.QueueClient), c.SettingService, c.Config, record.OrderID)
			}
			return nil
		},
		OnPlatformOrderRefunded: func(_ context.Context, record *quefadomain.Order, refundRef string) error {
			if err := quefaworkflow.ApplyPlatformCollectRefunded(c.OrderStore, c.WalletService, record, refundRef); err != nil {
				return err
			}
			if c.AffiliateService != nil {
				if err := c.AffiliateService.HandleOrderCanceled(record.ParentOrderID, "quefa_order_refunded"); err != nil {
					logger.Warnw("quefa_refund_affiliate_cancel_failed", "order_id", record.ParentOrderID, "error", err)
				}
			}
			return nil
		},
		OnOrderClosed: func(_ context.Context, record *quefadomain.Order) error {
			if c.OrderService == nil || record == nil {
				return nil
			}
			_, err := c.OrderService.CancelExpiredOrder(record.ParentOrderID)
			return err
		},
		EnqueueSync: func(ctx context.Context, childOrderID uint, delay time.Duration) error {
			if c.QueueClient == nil || !c.QueueClient.Enabled() {
				return nil
			}
			return c.QueueClient.EnqueueQuefaSyncOrder(ctx, childOrderID, delay)
		},
	})
}
