package rehearsal_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dujiao-next/internal/app/container"
	"github.com/dujiao-next/internal/app/httpserver/middleware"
	kafudawiring "github.com/dujiao-next/internal/bootstrap/kafudagpt"
	orderwiring "github.com/dujiao-next/internal/bootstrap/order"
	walletwiring "github.com/dujiao-next/internal/bootstrap/wallet"
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
	paymentstore "github.com/dujiao-next/internal/modules/payment/infrastructure/gormstore"
	resellercontract "github.com/dujiao-next/internal/modules/reseller/contract"
	walletapp "github.com/dujiao-next/internal/modules/wallet/application"
	walletdomain "github.com/dujiao-next/internal/modules/wallet/domain"
	walletstore "github.com/dujiao-next/internal/modules/wallet/infrastructure/gormstore"
	"github.com/dujiao-next/internal/shared/money"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
)

var (
	serveDirectRehearsal         = flag.Bool("kafuda-rehearsal", false, "serve the isolated, synthetic buyer rehearsal on loopback")
	directRehearsalPort          = flag.Int("kafuda-rehearsal-port", 5179, "loopback port used by the buyer rehearsal")
	directRehearsalStorefrontDir = flag.String("kafuda-rehearsal-storefront-dir", "", "absolute path to the built user storefront")
	directRehearsalOnline        = flag.Bool("kafuda-rehearsal-online", false, "enable the synthetic same-origin online payment channel")
	directRehearsalPostgres      = flag.Bool("kafuda-rehearsal-postgres", false, "use an isolated PostgreSQL schema through TEST_POSTGRES_DSN")
	directRehearsalAdminDir      = flag.String("kafuda-rehearsal-admin-dir", "", "absolute path to the fullstack admin build")
	directRehearsalProductType   = flag.String("kafuda-rehearsal-product", "gpt", "synthetic supplier product: gpt or gpt_credits")
	directRehearsalRenewal       = flag.String("kafuda-rehearsal-renewal-cancelled", "", "supplier completion renewal result: true, false, or omitted")
)

type directRehearsalOptions struct {
	productType      string
	renewalCancelled *bool
	postgresDSN      string
}

func newDirectRehearsalRouter(t *testing.T, storefrontDir, onlineOrigin string, adminDirs ...string) (directCheckoutFixture, *gin.Engine, string, string) {
	t.Helper()
	adminDir := ""
	switch len(adminDirs) {
	case 0:
	case 1:
		adminDir = adminDirs[0]
	default:
		t.Fatal("expected at most one rehearsal admin directory")
	}
	return newConfiguredDirectRehearsalRouter(t, storefrontDir, onlineOrigin, adminDir, directRehearsalOptions{})
}

func newConfiguredDirectRehearsalRouter(t *testing.T, storefrontDir, onlineOrigin, adminDir string, options directRehearsalOptions) (directCheckoutFixture, *gin.Engine, string, string) {
	t.Helper()
	productType := options.productType
	if productType == "" {
		productType = "gpt"
	}
	if productType != "gpt" && productType != "gpt_credits" {
		t.Fatal("rehearsal product must be gpt or gpt_credits")
	}
	gin.SetMode(gin.TestMode)
	f := newDirectCheckoutFixture(t, productType, options.postgresDSN)
	if err := f.db.AutoMigrate(&kafudadomain.Attempt{}, &paymentdomain.Payment{}, &paymentdomain.PaymentChannel{}, &walletdomain.Account{}, &walletdomain.Transaction{}); err != nil {
		t.Fatal(err)
	}
	account := walletdomain.Account{UserID: f.users[0].ID, Balance: money.FromDecimal(decimal.NewFromInt(200))}
	if err := f.db.Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	return assembleDirectRehearsalRouter(t, f, storefrontDir, onlineOrigin, adminDir, options.renewalCancelled)
}

func assembleDirectRehearsalRouter(t *testing.T, f directCheckoutFixture, storefrontDir, onlineOrigin, adminDir string, renewalCancelled *bool) (directCheckoutFixture, *gin.Engine, string, string) {
	t.Helper()
	if adminDir != "" && storefrontDir == "" {
		t.Fatal("admin rehearsal requires built storefront assets")
	}
	wallets := walletstore.New(f.db)
	walletService := walletapp.NewService(walletapp.Options{Repository: wallets, Transactions: wallets})
	payments := paymentstore.New(f.db, rand.Text())
	channels := paymentstore.NewChannelStore(f.db)
	var online *directRehearsalOnlinePayment
	var paymentRegistry paymentcontract.GatewayRegistry
	if onlineOrigin != "" {
		if storefrontDir == "" {
			t.Fatal("online rehearsal requires built storefront assets")
		}
		online, paymentRegistry = newDirectRehearsalOnlinePayment(t, f, payments, onlineOrigin)
	}
	deps := &container.Container{
		OrderStore: f.options.OrderStore, OrderService: orderapp.NewOrderService(f.options), UserStore: f.options.UserStore,
		WalletRepo: wallets, WalletService: walletService, PaymentStore: payments, PaymentChannelStore: channels,
		KafudaGPT: &kafudawiring.Module{Store: f.store, Commerce: f.commerce},
		PaymentService: paymentapp.NewPaymentService(paymentapp.PaymentServiceOptions{
			OrderStore: f.options.OrderStore, ProductRepo: f.options.ProductStore, ProductSKURepo: f.options.ProductSKUStore,
			PaymentStore: payments, ChannelStore: channels, UserStore: f.options.UserStore, WalletRepo: wallets, WalletService: walletService,
			PaymentProviderRegistry: paymentRegistry,
		}),
	}
	jwtSecret := rand.Text()
	auth := userauthapp.NewService(&config.Config{UserJWT: config.JWTConfig{SecretKey: jwtSecret}}, f.options.UserStore, nil, nil, nil, nil, nil)
	token, _, err := auth.GenerateUserJWT(&f.users[0], 1)
	if err != nil {
		t.Fatal(err)
	}
	otherToken, _, err := auth.GenerateUserJWT(&f.users[1], 1)
	if err != nil {
		t.Fatal(err)
	}

	router := gin.New()
	consolePath, storefrontSlug := "/", ""
	if storefrontDir != "" {
		consolePath, storefrontSlug = "/rehearsal", f.product.Slug
		available := []map[string]interface{}{}
		if online != nil {
			available, err = deps.PaymentService.GetAvailableChannels(paymentapp.AvailablePaymentChannelFilter{
				User: &f.users[0], PaymentType: constants.PaymentTypeOrder,
			})
			if err != nil || len(available) != 1 {
				t.Fatalf("expected one online rehearsal payment channel: %v", err)
			}
		}
		registerDirectRehearsalStorefront(t, router, f, storefrontDir, available)
	}
	router.GET(consolePath, func(c *gin.Context) {
		writeDirectRehearsalPage(c, token, f.product.ID, f.sku.ID, f.planLabel, storefrontSlug, online != nil, f.adminTokens["support"])
	})
	member := router.Group("/api/v1", func(c *gin.Context) {
		c.Request = c.Request.WithContext(resellercontract.WithTenantContext(c.Request.Context(), f.tenant))
	}, middleware.UserJWTAuthMiddleware(jwtSecret, f.options.UserStore))
	orders := orderwiring.New(deps)
	orderhttp.RegisterUserPreviewRoute(member, orders.Preview)
	orderhttp.RegisterUserCreateRoute(member, orders.Create)
	orderhttp.RegisterUserCreateAndPayRoute(member, orders.Create)
	orderhttp.RegisterUserReadRoutes(member, orders.User)
	orderhttp.RegisterUserPaymentChannelsRoute(member, orders.User)
	member.GET("/wallet", walletwiring.New(deps).User.GetWallet)
	newDirectRehearsalActions(f, adminDir != "", renewalCancelled).register(member)
	if online != nil {
		online.register(router, member, deps)
	}
	if adminDir != "" {
		f.adminTokens = registerDirectRehearsalAdmin(t, router, f, deps, adminDir)
	}
	return f, router, token, otherToken
}

func TestDirectBuyerFlowRehearsal(t *testing.T) {
	forEachDirectRehearsalDatabase(t, testDirectBuyerFlowRehearsal)
}

func testDirectBuyerFlowRehearsal(t *testing.T, postgresDSN string) {
	renewalCancelled, renewalNotCancelled := true, false
	for _, scenario := range []struct {
		name        string
		result      string
		stage       kafudadomain.Stage
		status      string
		options     directRehearsalOptions
		wantRenewal string
	}{
		{name: "success", result: "success", stage: kafudadomain.StageCompleted, status: constants.OrderStatusCompleted},
		{name: "manual_review", result: "manual_review", stage: kafudadomain.StageManualReview, status: constants.OrderStatusFulfilling},
		{name: "renewal_cancelled", result: "success", stage: kafudadomain.StageCompleted, status: constants.OrderStatusCompleted,
			options: directRehearsalOptions{productType: "gpt", renewalCancelled: &renewalCancelled}, wantRenewal: "true"},
		{name: "renewal_not_cancelled", result: "success", stage: kafudadomain.StageCompleted, status: constants.OrderStatusCompleted,
			options: directRehearsalOptions{productType: "gpt", renewalCancelled: &renewalNotCancelled}, wantRenewal: "false"},
		{name: "credits_omit_renewal", result: "success", stage: kafudadomain.StageCompleted, status: constants.OrderStatusCompleted,
			options: directRehearsalOptions{productType: "gpt_credits", renewalCancelled: &renewalCancelled}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			options := scenario.options
			options.postgresDSN = postgresDSN
			f, router, buyer, other := newConfiguredDirectRehearsalRouter(t, "", "", "", options)
			type data struct {
				OrderNo     string          `json:"order_no"`
				OrderPaid   bool            `json:"order_paid"`
				Balance     string          `json:"balance"`
				TotalAmount string          `json:"total_amount"`
				Stage       string          `json:"stage"`
				Status      string          `json:"status"`
				Renewal     json.RawMessage `json:"renewal_cancelled"`
				Children    []struct {
					Status string `json:"status"`
				} `json:"children"`
			}
			request := func(method, path, body, token string, wantOK bool) data {
				t.Helper()
				req := httptest.NewRequest(method, "/api/v1"+path, strings.NewReader(body)).WithContext(t.Context())
				req.Header.Set("Content-Type", "application/json")
				if token != "" {
					req.Header.Set("Authorization", "Bearer "+token)
				}
				res := httptest.NewRecorder()
				router.ServeHTTP(res, req)
				var envelope struct {
					Code int  `json:"status_code"`
					Data data `json:"data"`
				}
				if err := json.Unmarshal(res.Body.Bytes(), &envelope); err != nil {
					t.Fatalf("%s %s returned HTTP %d: %s", method, path, res.Code, res.Body.String())
				}
				ok := res.Code >= 200 && res.Code < 300 && envelope.Code == 0
				if ok != wantOK {
					t.Fatalf("%s %s returned HTTP %d: %s", method, path, res.Code, res.Body.String())
				}
				if wantOK {
					for _, private := range []string{"rehearsal-profile", "rehearsal-card", "cost_price", "max_cost", "45.00", "fixture-must-be-rejected"} {
						if strings.Contains(res.Body.String(), private) {
							t.Fatalf("%s %s exposed a private field", method, path)
						}
					}
				}
				return envelope.Data
			}
			status := func(orderNo string, want kafudadomain.Stage) {
				t.Helper()
				got := request(http.MethodGet, "/orders/"+orderNo+"/direct-status", "", buyer, true)
				if got.Stage != string(want) {
					t.Fatalf("buyer sees stage %q, want %q", got.Stage, want)
				}
				wantRenewal := ""
				if want == kafudadomain.StageCompleted {
					wantRenewal = scenario.wantRenewal
				}
				if string(got.Renewal) != wantRenewal {
					t.Fatalf("buyer sees renewal_cancelled %q at %s, want %q", got.Renewal, want, wantRenewal)
				}
			}
			items := fmt.Sprintf(`"items":[{"product_id":%d,"sku_id":%d,"quantity":1}]`, f.product.ID, f.sku.ID)
			if got := request(http.MethodGet, "/wallet", "", buyer, true); got.Balance != "200.00" {
				t.Fatalf("starting wallet balance = %q", got.Balance)
			}
			if got := request(http.MethodPost, "/orders/preview", "{"+items+"}", buyer, true); got.TotalAmount != "100.00" {
				t.Fatalf("preview total = %q", got.TotalAmount)
			}
			selection := fmt.Sprintf(`{"result":%q}`, scenario.result)
			draft := request(http.MethodPost, "/orders", "{"+items+"}", buyer, true)
			if draft.OrderNo == "" || draft.Status != constants.OrderStatusPendingPayment {
				t.Fatalf("expected an unpaid order, got %+v", draft)
			}
			request(http.MethodPost, "/rehearsal/orders/"+draft.OrderNo+"/authorize", selection, buyer, false)

			paid := request(http.MethodPost, "/orders/create-and-pay", "{"+items+`,"use_balance":true}`, buyer, true)
			if !paid.OrderPaid || paid.OrderNo == "" || paid.OrderNo == draft.OrderNo {
				t.Fatalf("expected a new wallet-paid order, got %+v", paid)
			}
			if got := request(http.MethodGet, "/wallet", "", buyer, true); got.Balance != "100.00" {
				t.Fatalf("settled wallet balance = %q", got.Balance)
			}
			status(paid.OrderNo, kafudadomain.StageWaiting)
			action := "/rehearsal/orders/" + paid.OrderNo
			request(http.MethodPost, action+"/authorize", selection, "", false)
			request(http.MethodPost, action+"/authorize", selection, other, false)
			request(http.MethodPost, action+"/authorize", `{"result":"success","session":"fixture-must-be-rejected"}`, buyer, false)
			status(paid.OrderNo, kafudadomain.StageWaiting)
			for range 2 {
				if got := request(http.MethodPost, action+"/authorize", selection, buyer, true); got.Stage != string(kafudadomain.StageProcessing) {
					t.Fatalf("authorization returned stage %q", got.Stage)
				}
			}
			status(paid.OrderNo, kafudadomain.StageProcessing)
			for range 2 {
				if got := request(http.MethodPost, action+"/advance", "", buyer, true); got.Stage != string(scenario.stage) {
					t.Fatalf("poll returned stage %q, want %q", got.Stage, scenario.stage)
				}
			}
			status(paid.OrderNo, scenario.stage)
			detail := request(http.MethodGet, "/orders/"+paid.OrderNo, "", buyer, true)
			if detail.Status != scenario.status || len(detail.Children) != 1 || detail.Children[0].Status != scenario.status {
				t.Fatalf("parent and child were not synchronized: %+v", detail)
			}
			var attempts []kafudadomain.Attempt
			if err := f.db.Find(&attempts).Error; err != nil || len(attempts) != 1 {
				t.Fatalf("expected exactly one attempt, got %d, %v", len(attempts), err)
			}
			if wantRenewal := scenario.options.renewalCancelled; wantRenewal != nil {
				got := attempts[0].RenewalCancelled
				if got == nil || *got != *wantRenewal {
					t.Fatalf("attempt did not store supplier renewal_cancelled=%t", *wantRenewal)
				}
				recharge, err := f.store.GetRecharge(t.Context(), attempts[0].OrderID)
				if err != nil || recharge == nil || recharge.Snapshot.Product != scenario.options.productType {
					t.Fatalf("checkout did not capture supplier product %s: %v", scenario.options.productType, err)
				}
			}
			var deliveries []fulfillmentdomain.Fulfillment
			if err := f.db.Find(&deliveries).Error; err != nil {
				t.Fatal(err)
			}
			if scenario.stage == kafudadomain.StageCompleted {
				if len(deliveries) != 1 || deliveries[0].Type != constants.FulfillmentTypeKafudaGPT || deliveries[0].Payload != "" {
					t.Fatalf("expected one content-free Direct delivery, got %+v", deliveries)
				}
			} else if len(deliveries) != 0 {
				t.Fatalf("manual review created a delivery: %+v", deliveries)
			}
		})
	}
}

func TestServeDirectBuyerRehearsal(t *testing.T) {
	if !*serveDirectRehearsal {
		t.Skip("enable with -kafuda-rehearsal")
	}
	if *directRehearsalPort < 1 || *directRehearsalPort > 65535 {
		t.Fatal("invalid rehearsal port")
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(*directRehearsalPort))
	onlineOrigin := ""
	if *directRehearsalOnline {
		onlineOrigin = "http://" + addr
	}
	options := directRehearsalOptions{productType: *directRehearsalProductType}
	if *directRehearsalPostgres {
		options.postgresDSN = strings.TrimSpace(os.Getenv("TEST_POSTGRES_DSN"))
		if options.postgresDSN == "" {
			t.Fatal("-kafuda-rehearsal-postgres requires TEST_POSTGRES_DSN")
		}
	}
	switch *directRehearsalRenewal {
	case "true", "false":
		cancelled := *directRehearsalRenewal == "true"
		options.renewalCancelled = &cancelled
	case "":
		// No renewal result was supplied.
	default:
		t.Fatal("rehearsal renewal result must be true or false")
	}
	ctx, stop := signal.NotifyContext(t.Context(), os.Interrupt)
	defer stop()
	fixture, router, _, _ := newConfiguredDirectRehearsalRouter(t, *directRehearsalStorefrontDir, onlineOrigin, *directRehearsalAdminDir, options)
	listener, err := net.Listen("tcp4", addr)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      20 * time.Second,
		IdleTimeout:       30 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Host != addr {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Cache-Control", "no-store")
			router.ServeHTTP(w, r)
		}),
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	consolePath := "/"
	if *directRehearsalStorefrontDir != "" {
		consolePath = "/rehearsal"
	}
	t.Logf("Synthetic buyer rehearsal (%s): http://%s%s", fixture.db.Dialector.Name(), addr, consolePath)
	select {
	case err := <-done:
		t.Fatal(err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			t.Fatal(err)
		}
		if err := <-done; !errors.Is(err, http.ErrServerClosed) {
			t.Fatal(err)
		}
	}
}
