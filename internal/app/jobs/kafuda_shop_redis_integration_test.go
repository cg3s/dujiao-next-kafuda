//go:build integration

package jobs

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dujiao-next/internal/app/container"
	"github.com/dujiao-next/internal/app/httpserver"
	jobconsumer "github.com/dujiao-next/internal/app/jobs/consumer"
	"github.com/dujiao-next/internal/bootstrap/database/migrations"
	"github.com/dujiao-next/internal/config"
	"github.com/dujiao-next/internal/constants"
	categorydomain "github.com/dujiao-next/internal/modules/catalog/category/domain"
	productwrite "github.com/dujiao-next/internal/modules/catalog/product/application/write"
	productpresenter "github.com/dujiao-next/internal/modules/catalog/product/transport/presenter"
	fulfillmentdomain "github.com/dujiao-next/internal/modules/fulfillment/domain"
	userdomain "github.com/dujiao-next/internal/modules/identity/user/domain"
	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
	ordercontract "github.com/dujiao-next/internal/modules/order/contract"
	orderhttp "github.com/dujiao-next/internal/modules/order/transport/http"
	walletdomain "github.com/dujiao-next/internal/modules/wallet/domain"
	"github.com/dujiao-next/internal/platform/database/gormdb"
	"github.com/dujiao-next/internal/platform/http/response"
	"github.com/dujiao-next/internal/queue"
	"github.com/dujiao-next/internal/shared/jsonmap"
	"github.com/dujiao-next/internal/shared/money"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/shopspring/decimal"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestRedisKafudaProductionShopStartsSellingAndFulfills(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		testRedisKafudaProductionShop(t, 13, "", false)
	})
	t.Run("postgres", func(t *testing.T) {
		dsn := strings.TrimSpace(os.Getenv("TEST_POSTGRES_DSN"))
		if dsn == "" {
			t.Skip("TEST_POSTGRES_DSN is empty")
		}
		testRedisKafudaProductionShop(t, 12, dsn, false)
	})
}

func TestRedisKafudaCatalogRefreshOutrunsLongOrderPoll(t *testing.T) {
	testRedisKafudaProductionShop(t, 11, "", true)
}

func testRedisKafudaProductionShop(t *testing.T, redisDB int, postgresDSN string, shortCatalogTTL bool) {
	t.Helper()
	redis, inspector := kafudaRedisFixture(t, redisDB)
	ctx := t.Context()
	const secret = "kafuda-shop-fixture-secret-for-jwt"
	const cardCode = "shop-test-card"
	const session = "kafuda-shop-test-session"
	const apiKey = "kafuda-shop-fixture-api-key"
	const orderID = 321
	var submissions, lookups atomic.Int32
	var complete atomic.Bool
	var submitted atomic.Pointer[kafudacontract.CreateOrderRequest]
	supplier := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+apiKey {
			t.Error("supplier request missing merchant authentication")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		write := func(data any) {
			if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
				t.Error(err)
			}
		}
		order := func(reference, status string) {
			write(map[string]any{"id": orderID, "reference": reference, "product": "gpt", "plan": "plus", "country": "PH", "status": status, "charged_usd_cents": 1000})
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/gpt/catalog":
			write(json.RawMessage(`{"enabled":true,"plans":[{"plan":"plus","label":"Plus","credit":false,"regions":[{"country":"PH","currency":"PHP","estimated_plan_usd_cents":1000,"estimated_local_minor":55000}]}],"auto_card_products":[{"product_code":"shop-test-card","restriction_reason":null}]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/gpt/check":
			var input kafudacontract.CheckRequest
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.Session != session || input.Country != "PH" || input.PayMode != "auto_card" || input.ProductCode != cardCode {
				t.Error("eligibility check did not use the buyer Session and selected SKU")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			quote := kafudacontract.Quote{Plan: "plus", Status: "available", Kind: "new", QuoteID: "shop-quote", PriceCents: 900,
				ExpiresAt: time.Now().Add(time.Minute), AutoCard: &kafudacontract.AutoCard{ProductCode: cardCode, LoadCents: 100, TotalCents: 1000}}
			write(map[string]any{"check_id": "shop-check", "country": input.Country, "pay_mode": input.PayMode, "region_lock": nil, "plans": []kafudacontract.Quote{quote}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/gpt/orders":
			var input kafudacontract.CreateOrderRequest
			decoder := json.NewDecoder(r.Body)
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&input); err != nil || input.QuoteID != "shop-quote" || input.Reference == "" || r.Header.Get("Idempotency-Key") != input.Reference {
				t.Error("upstream order did not use the checked quote and stable reference")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			submissions.Add(1)
			submitted.Store(&input)
			order(input.Reference, "processing")
		case r.Method == http.MethodGet && r.URL.Path == fmt.Sprintf("/api/v1/gpt/orders/%d", orderID):
			input := submitted.Load()
			if input == nil {
				t.Error("worker looked up an order before its submission")
				w.WriteHeader(http.StatusNotFound)
				return
			}
			lookups.Add(1)
			status := "processing"
			if complete.Load() {
				status = "completed"
			}
			order(input.Reference, status)
		default:
			t.Errorf("unexpected supplier request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(supplier.Close)
	// Trust the fixture certificate without replacing the production gateway.
	previousTransport := http.DefaultTransport
	http.DefaultTransport = supplier.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = previousTransport })

	db := openKafudaProductionShopDB(t, postgresDSN)
	if err := migrations.AutoMigrate(); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		App: config.AppConfig{SecretKey: secret}, JWT: config.JWTConfig{SecretKey: secret}, UserJWT: config.JWTConfig{SecretKey: secret},
		Server: config.ServerConfig{Mode: gin.TestMode}, Web: config.WebConfig{AdminPath: "/admin"}, Queue: *redis,
		KafudaGPT: config.KafudaGPTConfig{APIBaseURL: supplier.URL, APIKey: apiKey, SalesEnabled: true, ReferenceLookupVerified: true,
			ProductCode: cardCode, MaxCostCents: 2500, AllowedKinds: []string{"new"}, CatalogTTLSeconds: 3600, PollIntervalSeconds: 5},
	}
	if shortCatalogTTL {
		cfg.KafudaGPT.CatalogTTLSeconds = 24
		cfg.KafudaGPT.PollIntervalSeconds = 120
	}
	deps, err := container.NewContainer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = deps.QueueClient.Close() })
	catalog, err := deps.KafudaGPT.Catalog.Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	category := &categorydomain.Category{Slug: "kafuda-shop", NameJSON: jsonmap.JSON{"en-US": "Direct"}, IsActive: true}
	if err := deps.CategoryRepo.Create(category); err != nil {
		t.Fatal(err)
	}
	active, revision := true, catalog.Revision("plus", "PH")
	product, err := deps.ProductWriteService.Create(productwrite.CreateProductInput{
		CategoryID: category.ID, Slug: "kafuda-direct-shop", TitleJSON: jsonmap.JSON{"en-US": "Direct"},
		PurchaseType: constants.ProductPurchaseMember, StockDisplayMode: constants.ProductStockDisplayHidden, FulfillmentType: constants.FulfillmentTypeKafudaGPT,
		SKUs: []productwrite.ProductSKUInput{{SKUCode: kafudacontract.SKUCode("plus", "PH"), DirectCatalogRevision: &revision, IsActive: &active,
			PriceAmount: decimal.NewFromInt(100), CostPriceAmount: decimal.NewFromInt(45), CostPriceProvided: true}},
	})
	if err != nil || product == nil || len(product.SKUs) != 1 {
		t.Fatalf("create configured Direct SKU: %v", err)
	}
	sku := product.SKUs[0]
	buyer := &userdomain.User{Email: "kafuda-worker-shop@example.test", PasswordHash: "fixture", Status: constants.UserStatusActive}
	if err := deps.UserStore.Create(buyer); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&walletdomain.Account{UserID: buyer.ID, Balance: money.FromDecimal(decimal.NewFromInt(200))}).Error; err != nil {
		t.Fatal(err)
	}
	token, _, err := deps.UserAuthService.GenerateUserJWT(buyer, 1)
	if err != nil {
		t.Fatal(err)
	}
	router := httpserver.SetupRouter(cfg, deps)
	request := func(method, path, body string, code int, data any) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		var res struct {
			Code int             `json:"status_code"`
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil || w.Code != http.StatusOK || res.Code != code {
			t.Fatalf("%s %s: want API %d, got HTTP %d, API %d (%v)", method, path, code, w.Code, res.Code, err)
		}
		if data != nil {
			if err := json.Unmarshal(res.Data, data); err != nil {
				t.Fatal(err)
			}
		}
	}
	assertAvailability := func(soldOut bool) {
		t.Helper()
		var public productpresenter.Product
		request(http.MethodGet, "/api/v1/public/products/"+product.Slug, "", response.CodeOK, &public)
		if public.ID != product.ID || public.IsSoldOut != soldOut || len(public.SKUs) != 1 ||
			public.SKUs[0].ID != sku.ID || !public.SKUs[0].IsActive || public.SKUs[0].IsSoldOut != soldOut {
			t.Fatalf("configured Direct SKU did not show sold_out=%t: %+v", soldOut, public)
		}
	}
	const orders = "/api/v1/orders"
	items := fmt.Sprintf(`{"items":[{"product_id":%d,"sku_id":%d,"quantity":1}]`, product.ID, sku.ID)
	if reason := deps.KafudaGPT.Commerce.SalesBlockReason(ctx); reason != "worker_not_ready" {
		t.Fatalf("unexpected sales gate when worker is stopped: %q", reason)
	}
	assertAvailability(true)
	request(http.MethodPost, orders+"/preview", items+"}", response.CodeBadRequest, nil)

	startWorker := func() *Service {
		t.Helper()
		worker, err := NewService(redis, jobconsumer.New(deps))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = worker.Stop(context.Background()) })
		worker.startScheduler(ctx)
		if _, err := inspector.GetQueueInfo(queue.DefaultQueue); err != nil {
			t.Fatalf("scheduler did not initialize the recovery queue at startup: %v", err)
		}
		if err := worker.server.Start(worker.mux); err != nil {
			t.Fatal(err)
		}
		awaitKafudaRedis(t, 25*time.Second, func() bool { return deps.KafudaGPT.Commerce.SalesBlockReason(ctx) == "" })
		return worker
	}
	worker := startWorker()
	assertAvailability(false)
	request(http.MethodPost, orders+"/preview", items+"}", response.CodeOK, nil)
	if shortCatalogTTL {
		awaitKafudaRedis(t, time.Until(catalog.ExpiresAt)+3*time.Second, func() bool {
			assertAvailability(false)
			return !time.Now().Before(catalog.ExpiresAt)
		})
		request(http.MethodPost, orders+"/preview", items+"}", response.CodeOK, nil)
		return
	}
	if err := inspector.PauseQueue(queue.DefaultQueue); err != nil {
		t.Fatal(err)
	}
	awaitKafudaRedis(t, 10*time.Second, func() bool { return deps.KafudaGPT.Commerce.SalesBlockReason(ctx) == "worker_not_ready" })
	assertAvailability(true)
	request(http.MethodPost, orders+"/preview", items+"}", response.CodeBadRequest, nil)
	if err := inspector.UnpauseQueue(queue.DefaultQueue); err != nil {
		t.Fatal(err)
	}
	awaitKafudaRedis(t, 10*time.Second, func() bool { return deps.KafudaGPT.Commerce.SalesBlockReason(ctx) == "" })
	assertAvailability(false)
	var created struct {
		OrderNo string `json:"order_no"`
		Paid    bool   `json:"order_paid"`
	}
	request(http.MethodPost, orders+"/create-and-pay", items+`,"use_balance":true}`, response.CodeOK, &created)
	if !created.Paid || created.OrderNo == "" {
		t.Fatal("wallet checkout did not create a paid Direct order")
	}
	orderPath := orders + "/" + created.OrderNo
	var status orderhttp.DirectStatus
	request(http.MethodGet, orderPath+"/direct-status", "", response.CodeOK, &status)
	if status.Stage != string(kafudadomain.StageWaiting) {
		t.Fatalf("paid order started at %q, want waiting_session", status.Stage)
	}
	request(http.MethodPost, orderPath+"/direct-authorize", fmt.Sprintf(`{"session":%q}`, session), response.CodeOK, nil)
	request(http.MethodGet, orderPath+"/direct-status", "", response.CodeOK, &status)
	if status.Stage != string(kafudadomain.StageProcessing) {
		t.Fatalf("authorized order stage = %q, want processing", status.Stage)
	}
	if err := worker.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	awaitKafudaRedis(t, 25*time.Second, func() bool { return deps.KafudaGPT.Commerce.SalesBlockReason(ctx) == "worker_not_ready" })
	assertAvailability(true)
	request(http.MethodPost, orders+"/preview", items+"}", response.CodeBadRequest, nil)
	request(http.MethodGet, orderPath+"/direct-status", "", response.CodeOK, &status)
	if status.Stage != string(kafudadomain.StageProcessing) {
		t.Fatalf("worker shutdown changed the paid order's processing stage to %q", status.Stage)
	}
	complete.Store(true)
	startWorker()
	assertAvailability(false)
	awaitKafudaRedis(t, 25*time.Second, func() bool {
		request(http.MethodGet, orderPath+"/direct-status", "", response.CodeOK, &status)
		return status.Stage == string(kafudadomain.StageCompleted)
	})
	order, err := deps.OrderStore.GetByOrderNoAndUserScoped(created.OrderNo, buyer.ID, ordercontract.TenantScope{})
	if err != nil || order == nil || len(order.Children) != 1 || order.Status != constants.OrderStatusCompleted ||
		order.Children[0].Status != constants.OrderStatusCompleted || order.WalletPaidAmount.String() != "100.00" {
		t.Fatalf("scheduled lookup did not complete the paid parent and child order: %v", err)
	}
	var deliveries int64
	if err := db.Model(&fulfillmentdomain.Fulfillment{}).Where("order_id = ?", order.Children[0].ID).Count(&deliveries).Error; err != nil ||
		deliveries != 1 || submissions.Load() != 1 || lookups.Load() == 0 {
		t.Fatalf("expected one submission and delivery via scheduled lookup, got submissions=%d lookups=%d deliveries=%d: %v", submissions.Load(), lookups.Load(), deliveries, err)
	}
}

func openKafudaProductionShopDB(t *testing.T, postgresDSN string) *gorm.DB {
	t.Helper()
	previousDB := gormdb.DB
	t.Cleanup(func() { gormdb.DB = previousDB })
	if postgresDSN == "" {
		if err := gormdb.InitDB("sqlite", ":memory:", gormdb.DBPoolConfig{MaxOpenConns: 1, MaxIdleConns: 1}, gin.TestMode); err != nil {
			t.Fatal(err)
		}
		pool, err := gormdb.DB.DB()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = pool.Close() })
		return gormdb.DB
	}

	cfg, err := pgx.ParseConfig(postgresDSN)
	if err != nil {
		t.Fatal("invalid PostgreSQL test DSN")
	}
	admin := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { _ = admin.Close() })
	schema := pgx.Identifier{"kafuda_shop_" + strings.ToLower(rand.Text())}
	if _, err := admin.ExecContext(t.Context(), "CREATE SCHEMA "+schema.Sanitize()); err != nil {
		t.Fatalf("create PostgreSQL test schema: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := admin.ExecContext(ctx, "DROP SCHEMA "+schema.Sanitize()+" CASCADE"); err != nil {
			t.Errorf("drop PostgreSQL test schema: %v", err)
		}
	})
	scoped := cfg.Copy()
	scoped.RuntimeParams["search_path"] = schema[0]
	pool := stdlib.OpenDB(*scoped)
	pool.SetMaxOpenConns(4)
	t.Cleanup(func() { _ = pool.Close() })
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: pool}), &gorm.Config{})
	if err != nil {
		t.Fatalf("open PostgreSQL test schema: %v", err)
	}
	gormdb.DB = db
	return db
}
