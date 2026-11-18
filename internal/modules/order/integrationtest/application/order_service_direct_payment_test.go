package application_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dujiao-next/internal/app/container"
	"github.com/dujiao-next/internal/app/httpserver/middleware"
	kafudawiring "github.com/dujiao-next/internal/bootstrap/kafudagpt"
	orderwiring "github.com/dujiao-next/internal/bootstrap/order"
	paymentwiring "github.com/dujiao-next/internal/bootstrap/payment"
	"github.com/dujiao-next/internal/config"
	"github.com/dujiao-next/internal/constants"
	fulfillmentdomain "github.com/dujiao-next/internal/modules/fulfillment/domain"
	userauthapp "github.com/dujiao-next/internal/modules/identity/userauth/application"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
	orderapp "github.com/dujiao-next/internal/modules/order/application"
	orderhttp "github.com/dujiao-next/internal/modules/order/transport/http"
	paymentapp "github.com/dujiao-next/internal/modules/payment/application"
	paymentcontract "github.com/dujiao-next/internal/modules/payment/contract"
	paymentdomain "github.com/dujiao-next/internal/modules/payment/domain"
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

func TestDirectCheckoutPaymentsPreserveRechargeSnapshot(t *testing.T) {
	testDirectCheckoutPaymentsPreserveRechargeSnapshot(t, newDirectCheckoutFixture)
}

func testDirectCheckoutPaymentsPreserveRechargeSnapshot(t *testing.T, fixture func(*testing.T) directCheckoutFixture) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	for _, method := range []string{"wallet", "online_callback"} {
		t.Run(method, func(t *testing.T) {
			f := fixture(t)
			if err := f.db.AutoMigrate(&kafudadomain.Attempt{}, &paymentdomain.Payment{}, &paymentdomain.PaymentChannel{}, &walletdomain.Account{}, &walletdomain.Transaction{}); err != nil {
				t.Fatal(err)
			}
			wantAmount := money.FromDecimal(decimal.NewFromInt(90))
			walletPayment := method == "wallet"
			var channel paymentdomain.PaymentChannel
			if walletPayment {
				if err := f.db.Create(&walletdomain.Account{UserID: f.users[0].ID, Balance: wantAmount}).Error; err != nil {
					t.Fatal(err)
				}
			} else {
				channel = paymentdomain.PaymentChannel{
					Name: "Direct checkout test gateway", ProviderType: constants.PaymentProviderEpay,
					ChannelType: constants.PaymentChannelTypeAlipay, InteractionMode: constants.PaymentInteractionRedirect, IsActive: true,
				}
				if err := f.db.Create(&channel).Error; err != nil {
					t.Fatal(err)
				}
			}

			var initial *kafudadomain.Recharge
			var parentID uint
			queue := &directCheckoutPaymentQueue{
				concurrentCouponQueue: f.queue,
				onCheckout: func(orderID uint) {
					parentID = orderID
					order, err := f.options.OrderStore.GetByID(orderID)
					if err != nil || order == nil || order.Status != constants.OrderStatusPendingPayment || len(order.Children) != 1 {
						t.Fatalf("expected committed checkout before payment: %+v, %v", order, err)
					}
					initial, err = f.store.GetRecharge(t.Context(), order.Children[0].ID)
					if err != nil || initial == nil || initial.Stage != kafudadomain.StageWaiting || initial.Version != 1 ||
						!initial.Snapshot.SellPrice.Equal(wantAmount.Decimal) || initial.Snapshot.CostPrice.String() != "45.00" {
						t.Fatalf("unexpected checkout snapshot before payment: %+v, %v", initial, err)
					}
				},
			}
			f.options.Queue = queue
			orders := orderapp.NewOrderService(f.options)
			payments := paymentstore.New(f.db, "test-guest-credential-secret-with-32-bytes")
			channels := paymentstore.NewChannelStore(f.db)
			wallets := walletstore.New(f.db)
			gateway := &directCheckoutPaymentGateway{}
			registry := provider.NewRegistry()
			registry.Register(constants.PaymentProviderEpay, "", gateway)
			paymentService := paymentapp.NewPaymentService(paymentapp.PaymentServiceOptions{
				OrderStore: f.options.OrderStore, ProductRepo: f.options.ProductStore, ProductSKURepo: f.options.ProductSKUStore,
				PaymentStore: payments, ChannelStore: channels, UserStore: f.options.UserStore, Queue: queue, WalletRepo: wallets,
				WalletService:           walletapp.NewService(walletapp.Options{Repository: wallets, Transactions: wallets}),
				PaymentProviderRegistry: registry,
			})
			deps := &container.Container{
				OrderStore: f.options.OrderStore, OrderService: orders, PaymentService: paymentService,
				PaymentStore: payments, PaymentChannelStore: channels,
				KafudaGPT: &kafudawiring.Module{Store: f.store},
			}

			const userJWTSecret = "direct-checkout-payment-integration-test-key"
			auth := userauthapp.NewService(&config.Config{UserJWT: config.JWTConfig{SecretKey: userJWTSecret}}, f.options.UserStore, nil, nil, nil, nil, nil)
			token, _, err := auth.GenerateUserJWT(&f.users[0], 1)
			if err != nil {
				t.Fatal(err)
			}
			router := gin.New()
			api := router.Group("/api/v1")
			buyer := api.Group("", middleware.UserJWTAuthMiddleware(userJWTSecret, f.options.UserStore))
			orderHandlers := orderwiring.New(deps)
			orderhttp.RegisterUserCreateAndPayRoute(buyer, orderHandlers.Create)
			orderhttp.RegisterUserReadRoutes(buyer, orderHandlers.User)
			paymentcallback.RegisterRoutes(api, paymentwiring.New(deps).Callback)

			request := func(verb, path, body, contentType, bearer string) *httptest.ResponseRecorder {
				t.Helper()
				ctx := resellercontract.WithTenantContext(t.Context(), f.input.Tenant)
				req := httptest.NewRequest(verb, path, strings.NewReader(body)).WithContext(ctx)
				if bearer != "" {
					req.Header.Set("Authorization", "Bearer "+bearer)
				}
				req.Header.Set("Content-Type", contentType)
				result := httptest.NewRecorder()
				router.ServeHTTP(result, req)
				return result
			}
			body := fmt.Sprintf(`{"items":[{"product_id":%d,"sku_id":%d,"quantity":1}],"coupon_code":%q,"use_balance":%t,"channel_id":%d}`,
				f.product.ID, f.sku.ID, f.coupon.Code, walletPayment, channel.ID)
			created := request(http.MethodPost, "/api/v1/orders/create-and-pay", body, "application/json", token)
			assertDirectCheckoutPrivate(t, created.Body.Bytes())
			var result struct {
				StatusCode int `json:"status_code"`
				Data       struct {
					OrderNo      string `json:"order_no"`
					OrderPaid    bool   `json:"order_paid"`
					PaymentID    uint   `json:"payment_id"`
					PaymentError string `json:"payment_error"`
				} `json:"data"`
			}
			if err := json.Unmarshal(created.Body.Bytes(), &result); err != nil || result.StatusCode != 0 ||
				result.Data.OrderNo == "" || result.Data.PaymentError != "" || result.Data.OrderPaid != walletPayment || initial == nil {
				t.Fatalf("unexpected create-and-pay result: %s, %v", created.Body.String(), err)
			}
			if !walletPayment {
				if result.Data.PaymentID == 0 || gateway.created.PaymentID != result.Data.PaymentID || gateway.created.OrderNo == "" ||
					!gateway.created.Amount.Equal(wantAmount.Decimal) {
					t.Fatalf("wrong online gateway request: %+v", gateway.created)
				}
				form := url.Values{
					"pid": {"checkout-test"}, "out_trade_no": {gateway.created.OrderNo}, "trade_status": {constants.EpayTradeStatusSuccess},
				}
				for range 2 {
					callback := request(http.MethodPost, "/api/v1/payments/callback", form.Encode(), "application/x-www-form-urlencoded", "")
					if callback.Code != http.StatusOK || callback.Body.String() != constants.EpayCallbackSuccess {
						t.Fatalf("online payment callback: %d %s", callback.Code, callback.Body.String())
					}
				}
			}

			order, err := f.options.OrderStore.GetByID(parentID)
			if err != nil || order == nil || order.OrderNo != result.Data.OrderNo || order.Status != constants.OrderStatusPaid ||
				order.PaidAt == nil || len(order.Children) != 1 || order.Children[0].Status != constants.OrderStatusPaid || order.Children[0].PaidAt == nil {
				t.Fatalf("payment did not mark checkout and its child paid: %+v, %v", order, err)
			}
			recharge, err := f.store.GetRecharge(t.Context(), initial.OrderID)
			if err != nil || !reflect.DeepEqual(recharge, initial) {
				t.Fatalf("payment modified checkout recharge: before=%+v after=%+v err=%v", initial, recharge, err)
			}
			status := request(http.MethodGet, "/api/v1/orders/"+result.Data.OrderNo+"/direct-status", "", "", token)
			var buyerStatus struct {
				StatusCode int `json:"status_code"`
				Data       struct {
					Stage string `json:"stage"`
				} `json:"data"`
			}
			if err := json.Unmarshal(status.Body.Bytes(), &buyerStatus); err != nil || buyerStatus.StatusCode != 0 || buyerStatus.Data.Stage != "waiting_session" {
				t.Fatalf("unexpected buyer status after payment: %s, %v", status.Body.String(), err)
			}
			assertDirectCheckoutPrivate(t, status.Body.Bytes())
			for _, model := range []any{&kafudadomain.Attempt{}, &fulfillmentdomain.Fulfillment{}} {
				var count int64
				if err := f.db.Model(model).Count(&count).Error; err != nil || count != 0 {
					t.Fatalf("payment created %T: %d, %v", model, count, err)
				}
			}
			if queue.autoFulfill != 0 {
				t.Fatal("payment queued automatic fulfillment")
			}
		})
	}
}

type directCheckoutPaymentQueue struct {
	*concurrentCouponQueue
	onCheckout  func(uint)
	autoFulfill int
}

func (q *directCheckoutPaymentQueue) EnqueueTimeoutCancel(orderID uint, delay time.Duration) error {
	q.onCheckout(orderID)
	return q.concurrentCouponQueue.EnqueueTimeoutCancel(orderID, delay)
}

func (q *directCheckoutPaymentQueue) EnqueueOrderAutoFulfill(uint) error {
	q.autoFulfill++
	return nil
}

func (*directCheckoutPaymentQueue) EnqueueAffiliateOrderPaid(uint) error { return nil }
func (*directCheckoutPaymentQueue) EnqueueWalletRechargeExpire(uint, time.Duration) error {
	return nil
}
func (*directCheckoutPaymentQueue) EnqueueBotNotification(paymentcontract.BotNotification) error {
	return nil
}

// directCheckoutPaymentGateway replaces the external provider only. It never sends a network request.
type directCheckoutPaymentGateway struct {
	created paymentcontract.GatewayCreateInput
}

func (*directCheckoutPaymentGateway) Type() string                              { return constants.PaymentProviderEpay + ":" }
func (*directCheckoutPaymentGateway) ValidateConfig(jsonmap.JSON, string) error { return nil }

func (g *directCheckoutPaymentGateway) CreatePayment(_ context.Context, _ jsonmap.JSON, input paymentcontract.GatewayCreateInput) (*paymentcontract.GatewayCreateResult, error) {
	g.created = input
	return &paymentcontract.GatewayCreateResult{RedirectURL: "https://checkout.example.test/pay"}, nil
}

func (g *directCheckoutPaymentGateway) VerifyCallback(_ jsonmap.JSON, form map[string][]string, _ []byte) (*paymentcontract.GatewayCallbackResult, error) {
	values := url.Values(form)
	if g.created.OrderNo == "" || values.Get("pid") != "checkout-test" || values.Get("out_trade_no") != g.created.OrderNo ||
		values.Get("trade_status") != constants.EpayTradeStatusSuccess {
		return nil, paymentcontract.ErrGatewaySignatureInvalid
	}
	return &paymentcontract.GatewayCallbackResult{
		OrderNo: g.created.OrderNo, Status: constants.PaymentStatusSuccess, Amount: g.created.Amount, Currency: g.created.Currency,
	}, nil
}
