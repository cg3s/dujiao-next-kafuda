package application_test

import (
	"crypto/md5" // EPay v1 callbacks use MD5 signatures.
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dujiao-next/internal/app/container"
	"github.com/dujiao-next/internal/app/httpserver/middleware"
	orderwiring "github.com/dujiao-next/internal/bootstrap/order"
	paymentwiring "github.com/dujiao-next/internal/bootstrap/payment"
	"github.com/dujiao-next/internal/config"
	"github.com/dujiao-next/internal/constants"
	cardsecretdomain "github.com/dujiao-next/internal/modules/cardsecret/domain"
	productdomain "github.com/dujiao-next/internal/modules/catalog/product/domain"
	userauthapp "github.com/dujiao-next/internal/modules/identity/userauth/application"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
	notificationcontract "github.com/dujiao-next/internal/modules/notification/contract"
	orderapp "github.com/dujiao-next/internal/modules/order/application"
	orderdomain "github.com/dujiao-next/internal/modules/order/domain"
	orderhttp "github.com/dujiao-next/internal/modules/order/transport/http"
	orderpresenter "github.com/dujiao-next/internal/modules/order/transport/presenter"
	paymentapp "github.com/dujiao-next/internal/modules/payment/application"
	paymentcontract "github.com/dujiao-next/internal/modules/payment/contract"
	paymentdomain "github.com/dujiao-next/internal/modules/payment/domain"
	epayadapter "github.com/dujiao-next/internal/modules/payment/infrastructure/gateway/adapters/epay"
	"github.com/dujiao-next/internal/modules/payment/infrastructure/gateway/provider"
	paymentstore "github.com/dujiao-next/internal/modules/payment/infrastructure/gormstore"
	paymentcallback "github.com/dujiao-next/internal/modules/payment/transport/http/callback"
	resellercontract "github.com/dujiao-next/internal/modules/reseller/contract"
	walletapp "github.com/dujiao-next/internal/modules/wallet/application"
	walletdomain "github.com/dujiao-next/internal/modules/wallet/domain"
	walletstore "github.com/dujiao-next/internal/modules/wallet/infrastructure/gormstore"
	"github.com/dujiao-next/internal/shared/jsonmap"
	"github.com/dujiao-next/internal/shared/money"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
)

type mixedCheckoutQueue struct {
	*concurrentCouponQueue
	autoOrderIDs []uint
}

func (q *mixedCheckoutQueue) EnqueueOrderAutoFulfill(orderID uint) error {
	q.autoOrderIDs = append(q.autoOrderIDs, orderID)
	return nil
}

func (*mixedCheckoutQueue) EnqueueAffiliateOrderPaid(uint) error                  { return nil }
func (*mixedCheckoutQueue) EnqueueWalletRechargeExpire(uint, time.Duration) error { return nil }
func (*mixedCheckoutQueue) EnqueueBotNotification(paymentcontract.BotNotification) error {
	return nil
}

type mixedCheckoutNotifications struct {
	manualOrderIDs []uint
}

func (n *mixedCheckoutNotifications) Enqueue(input notificationcontract.EnqueueInput) error {
	if input.EventType == constants.NotificationEventManualFulfillmentPending && input.BizType == constants.NotificationBizTypeOrder {
		n.manualOrderIDs = append(n.manualOrderIDs, input.BizID)
	}
	return nil
}

func TestMixedCheckoutPaymentsAndFulfillment(t *testing.T) {
	testMixedCheckoutPaymentsAndFulfillment(t, newDirectCheckoutFixture)
}

func testMixedCheckoutPaymentsAndFulfillment(t *testing.T, fixture func(*testing.T) directCheckoutFixture) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	for _, scenario := range []struct {
		name         string
		wallet       bool
		salesEnabled bool
	}{
		{name: "wallet/sales_enabled", wallet: true, salesEnabled: true},
		{name: "wallet/sales_disabled", wallet: true},
		{name: "signed_epay_callback/sales_enabled", salesEnabled: true},
		{name: "signed_epay_callback/sales_disabled"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			f := fixture(t)
			if err := f.db.AutoMigrate(&cardsecretdomain.Batch{}, &cardsecretdomain.Secret{}, &paymentdomain.Payment{}, &paymentdomain.PaymentChannel{}, &walletdomain.Account{}, &walletdomain.Transaction{}); err != nil {
				t.Fatal(err)
			}
			items := make([]orderhttp.OrderItemRequest, 0, 3)
			var manualSKUID, upstreamProductID, upstreamSKUID uint
			for i, kind := range []string{constants.FulfillmentTypeAuto, constants.FulfillmentTypeManual, constants.FulfillmentTypeUpstream} {
				price := money.FromDecimal(decimal.NewFromInt(int64(10 * (i + 1))))
				product := productdomain.Product{
					CategoryID: f.product.CategoryID, Slug: "mixed-" + kind, TitleJSON: jsonmap.JSON{"en-US": "Mixed " + kind},
					PurchaseType: constants.ProductPurchaseMember, FulfillmentType: kind, PriceAmount: price, IsActive: true,
					IsMapped: kind == constants.FulfillmentTypeUpstream,
				}
				if err := f.db.Create(&product).Error; err != nil {
					t.Fatal(err)
				}
				sku := productdomain.ProductSKU{ProductID: product.ID, SKUCode: productdomain.DefaultSKUCode, PriceAmount: price, IsActive: true}
				if kind == constants.FulfillmentTypeManual {
					sku.ManualStockTotal = 2
				}
				if err := f.db.Create(&sku).Error; err != nil {
					t.Fatal(err)
				}
				switch kind {
				case constants.FulfillmentTypeAuto:
					for _, payload := range []string{mixedCheckoutCardPayload, "mixed-checkout-unused-card"} {
						card := cardsecretdomain.Secret{ProductID: product.ID, SKUID: sku.ID, Secret: payload, Status: cardsecretdomain.StatusAvailable}
						if err := f.db.Create(&card).Error; err != nil {
							t.Fatal(err)
						}
					}
				case constants.FulfillmentTypeManual:
					manualSKUID = sku.ID
				case constants.FulfillmentTypeUpstream:
					upstreamProductID, upstreamSKUID = product.ID, sku.ID
				}
				items = append(items, orderhttp.OrderItemRequest{ProductID: product.ID, SKUID: sku.ID, Quantity: 1})
			}

			buyer := &f.users[0]
			total := money.FromDecimal(decimal.NewFromInt(60))
			walletPayment := scenario.wallet
			const merchantID, merchantKey = "mixed-checkout", "mixed-checkout-test-key"
			var channel paymentdomain.PaymentChannel
			if walletPayment {
				if err := f.db.Create(&walletdomain.Account{UserID: buyer.ID, Balance: total}).Error; err != nil {
					t.Fatal(err)
				}
			} else {
				channel = paymentdomain.PaymentChannel{
					Name: "Mixed checkout fixture", ProviderType: constants.PaymentProviderEpay,
					ChannelType: constants.PaymentChannelTypeAlipay, InteractionMode: constants.PaymentInteractionRedirect, IsActive: true,
					ConfigJSON: jsonmap.JSON{
						"gateway_url": "https://epay.example.test", "epay_version": "v1", "merchant_id": merchantID, "merchant_key": merchantKey,
						"notify_url": "https://shop.example.test/api/v1/payments/callback", "return_url": "https://shop.example.test/pay", "sign_type": "MD5",
					},
				}
				if err := f.db.Create(&channel).Error; err != nil {
					t.Fatal(err)
				}
			}

			queue := &mixedCheckoutQueue{concurrentCouponQueue: f.queue}
			notifications := &mixedCheckoutNotifications{}
			f.options.Queue = queue
			commerce := directCheckoutCommerce(f.store, scenario.salesEnabled)
			f.options.DirectCatalog = commerce
			orders := orderapp.NewOrderService(f.options)
			preview, err := orders.PreviewOrder(f.input)
			reason := commerce.SalesBlockReason(t.Context())
			if scenario.salesEnabled {
				if reason != "" || err != nil || preview == nil || len(preview.Items) != 1 || preview.Items[0].FulfillmentType != constants.FulfillmentTypeKafudaGPT {
					t.Fatalf("enabled direct product preview: reason=%q, preview=%+v, err=%v", reason, preview, err)
				}
			} else if reason != "sales_disabled" || !errors.Is(err, orderapp.ErrProductNotAvailable) || preview != nil {
				t.Fatalf("disabled direct product preview: reason=%q, preview=%+v, err=%v", reason, preview, err)
			}
			payments := paymentstore.New(f.db, "test-guest-credential-secret-with-32-bytes")
			channels := paymentstore.NewChannelStore(f.db)
			wallets := walletstore.New(f.db)
			registry := provider.NewRegistry()
			registry.Register(constants.PaymentProviderEpay, "", epayadapter.NewEpayAdapter())
			paymentService := paymentapp.NewPaymentService(paymentapp.PaymentServiceOptions{
				OrderStore: f.options.OrderStore, ProductRepo: f.options.ProductStore, ProductSKURepo: f.options.ProductSKUStore,
				PaymentStore: payments, ChannelStore: channels, UserStore: f.options.UserStore, Queue: queue, WalletRepo: wallets,
				WalletService:       walletapp.NewService(walletapp.Options{Repository: wallets, Transactions: wallets}),
				NotificationService: notifications, PaymentProviderRegistry: registry,
			})
			procurement := newMixedCheckoutProcurement(t, f, upstreamProductID, upstreamSKUID)
			paymentService.SetProcurementService(procurement.service)
			deps := &container.Container{
				OrderStore: f.options.OrderStore, OrderService: orders, PaymentService: paymentService,
				PaymentStore: payments, PaymentChannelStore: channels,
			}

			const jwtSecret = "mixed-checkout-payment-integration-key"
			auth := userauthapp.NewService(&config.Config{UserJWT: config.JWTConfig{SecretKey: jwtSecret}}, f.options.UserStore, nil, nil, nil, nil, nil)
			token, _, err := auth.GenerateUserJWT(buyer, 1)
			if err != nil {
				t.Fatal(err)
			}
			router := gin.New()
			api := router.Group("/api/v1")
			member := api.Group("", middleware.UserJWTAuthMiddleware(jwtSecret, f.options.UserStore))
			handlers := orderwiring.New(deps)
			orderhttp.RegisterUserCreateAndPayRoute(member, handlers.Create)
			orderhttp.RegisterUserReadRoutes(member, handlers.User)
			paymentcallback.RegisterRoutes(api, paymentwiring.New(deps).Callback)

			request := func(verb, path, body, contentType, bearer string) *httptest.ResponseRecorder {
				ctx := resellercontract.WithTenantContext(t.Context(), f.input.Tenant)
				req := httptest.NewRequest(verb, path, strings.NewReader(body)).WithContext(ctx)
				req.Header.Set("Content-Type", contentType)
				if bearer != "" {
					req.Header.Set("Authorization", "Bearer "+bearer)
				}
				response := httptest.NewRecorder()
				router.ServeHTTP(response, req)
				return response
			}
			body, err := json.Marshal(orderhttp.CreateOrderAndPayRequest{Items: items, UseBalance: walletPayment, ChannelID: channel.ID})
			if err != nil {
				t.Fatal(err)
			}
			response := request(http.MethodPost, "/api/v1/orders/create-and-pay", string(body), "application/json", token)
			var result struct {
				StatusCode int `json:"status_code"`
				Data       struct {
					OrderNo      string `json:"order_no"`
					OrderPaid    bool   `json:"order_paid"`
					PaymentID    uint   `json:"payment_id"`
					PaymentError string `json:"payment_error"`
				} `json:"data"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || response.Code != http.StatusOK || result.StatusCode != 0 ||
				result.Data.OrderNo == "" || result.Data.PaymentError != "" || result.Data.OrderPaid != walletPayment {
				t.Fatalf("mixed checkout failed: %d %s, %v", response.Code, response.Body.String(), err)
			}
			loadOrder := func() *orderdomain.Order {
				t.Helper()
				var row orderdomain.Order
				if err := f.db.Where("order_no = ?", result.Data.OrderNo).First(&row).Error; err != nil {
					t.Fatal(err)
				}
				order, err := f.options.OrderStore.GetByID(row.ID)
				if err != nil || order == nil {
					t.Fatalf("load checkout: %+v, %v", order, err)
				}
				return order
			}
			var replayOnlineCallback func()
			if !walletPayment {
				payment, err := payments.GetByID(result.Data.PaymentID)
				if err != nil || payment == nil || payment.GatewayOrderNo == "" || !payment.Amount.Equal(total.Decimal) {
					t.Fatalf("unexpected online payment: %+v, %v", payment, err)
				}
				form := url.Values{
					"pid": {merchantID}, "out_trade_no": {payment.GatewayOrderNo}, "trade_no": {"fixture-epay-transaction"},
					"money": {total.String()}, "trade_status": {constants.EpayTradeStatusSuccess}, "sign_type": {"MD5"}, "sign": {"invalid-signature"},
				}
				callback := func(want string) {
					t.Helper()
					result := request(http.MethodPost, "/api/v1/payments/callback", form.Encode(), "application/x-www-form-urlencoded", "")
					if result.Code != http.StatusOK || result.Body.String() != want {
						t.Fatalf("EPay callback: %d %q, want %q", result.Code, result.Body.String(), want)
					}
				}
				callback(constants.EpayCallbackFail)
				pending := loadOrder()
				if pending.Status != constants.OrderStatusPendingPayment || pending.PaidAt != nil || len(queue.autoOrderIDs) != 0 ||
					len(notifications.manualOrderIDs) != 0 || len(procurement.submitIDs) != 0 {
					t.Fatalf("invalid callback changed fulfillment: order=%+v auto=%v manual=%v procurement=%v", pending, queue.autoOrderIDs, notifications.manualOrderIDs, procurement.submitIDs)
				}
				content := fmt.Sprintf("money=%s&out_trade_no=%s&pid=%s&trade_no=%s&trade_status=%s", form.Get("money"), form.Get("out_trade_no"), form.Get("pid"), form.Get("trade_no"), form.Get("trade_status"))
				form.Set("sign", fmt.Sprintf("%x", md5.Sum([]byte(content+merchantKey))))
				replayOnlineCallback = func() { callback(constants.EpayCallbackSuccess) }
				replayOnlineCallback()
				replayOnlineCallback()
			}

			order := loadOrder()
			if order.UserID != buyer.ID || order.Status != constants.OrderStatusFulfilling || order.PaidAt == nil || len(order.Children) != 3 || !order.TotalAmount.Equal(total.Decimal) {
				t.Fatalf("unexpected paid mixed order: %+v", order)
			}
			if walletPayment {
				if !order.WalletPaidAmount.Equal(total.Decimal) || !order.OnlinePaidAmount.IsZero() {
					t.Fatalf("unexpected wallet payment totals: %+v", order)
				}
				var ledger []walletdomain.Transaction
				if err := f.db.Where("order_id = ? AND user_id = ?", order.ID, buyer.ID).Find(&ledger).Error; err != nil || len(ledger) != 1 ||
					ledger[0].Type != constants.WalletTxnTypeOrderPay || ledger[0].Direction != constants.WalletTxnDirectionOut ||
					!ledger[0].Amount.Equal(total.Decimal) || !ledger[0].BalanceAfter.IsZero() {
					t.Fatalf("wallet did not debit exactly once: %+v, %v", ledger, err)
				}
			} else if !order.OnlinePaidAmount.Equal(total.Decimal) || !order.WalletPaidAmount.IsZero() {
				t.Fatalf("unexpected online payment totals: %+v", order)
			}
			var manualID uint
			for _, child := range order.Children {
				if child.PaidAt == nil || len(child.Items) != 1 {
					t.Fatalf("mixed child did not receive payment: %+v", child)
				}
				if child.Items[0].FulfillmentType == constants.FulfillmentTypeManual {
					manualID = child.ID
				}
			}
			if manualID == 0 || !slices.Equal(notifications.manualOrderIDs, []uint{manualID}) {
				t.Fatalf("manual pending notification did not target just the manual child: %v", notifications.manualOrderIDs)
			}
			var manualSKU productdomain.ProductSKU
			if err := f.db.First(&manualSKU, manualSKUID).Error; err != nil || manualSKU.ManualStockTotal != 1 || manualSKU.ManualStockLocked != 0 || manualSKU.ManualStockSold != 1 {
				t.Fatalf("manual SKU inventory did not settle once: %+v, %v", manualSKU, err)
			}
			exerciseMixedCheckoutFulfillment(t, f, order, queue.autoOrderIDs, procurement)
			if replayOnlineCallback != nil {
				replayOnlineCallback()
				if len(queue.autoOrderIDs) != 1 || len(notifications.manualOrderIDs) != 1 || len(procurement.submitIDs) != 1 {
					t.Fatalf("settled payment replay dispatched new work: auto=%v manual=%v procurement=%v", queue.autoOrderIDs, notifications.manualOrderIDs, procurement.submitIDs)
				}
			}
			var count int64
			if err := f.db.Model(&kafudadomain.Recharge{}).Count(&count).Error; err != nil || count != 0 {
				t.Fatalf("ordinary checkout created %d direct recharges: %v", count, err)
			}
			detail := request(http.MethodGet, "/api/v1/orders/"+order.OrderNo, "", "", token)
			assertMixedBuyerDelivery(t, order, detail)
		})
	}
}

func assertMixedBuyerDelivery(t *testing.T, order *orderdomain.Order, response *httptest.ResponseRecorder) {
	t.Helper()
	var result struct {
		StatusCode int                        `json:"status_code"`
		Data       orderpresenter.OrderDetail `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || response.Code != http.StatusOK || result.StatusCode != 0 ||
		result.Data.OrderNo != order.OrderNo || result.Data.Status != constants.OrderStatusDelivered || len(result.Data.Children) != 3 {
		t.Fatalf("unexpected buyer order detail: %d %s, %v", response.Code, response.Body.String(), err)
	}
	byNumber := make(map[string]orderpresenter.OrderDetail, len(result.Data.Children))
	for _, child := range result.Data.Children {
		byNumber[child.OrderNo] = child
	}
	for _, child := range order.Children {
		kind := child.Items[0].FulfillmentType
		payload := mixedCheckoutManualPayload
		switch kind {
		case constants.FulfillmentTypeAuto:
			payload = mixedCheckoutCardPayload
		case constants.FulfillmentTypeUpstream:
			kind, payload = constants.FulfillmentTypeManual, mixedCheckoutUpstreamPayload
		}
		visible := byNumber[child.OrderNo]
		if len(visible.Items) != 1 || visible.Items[0].FulfillmentType != kind || visible.Fulfillment == nil || visible.Fulfillment.Type != kind || visible.Fulfillment.Payload != payload {
			t.Fatalf("unexpected buyer delivery for child %s: %+v", child.OrderNo, visible)
		}
	}
}
