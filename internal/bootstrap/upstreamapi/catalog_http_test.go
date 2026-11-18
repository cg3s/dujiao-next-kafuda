package upstreamwiring

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dujiao-next/internal/app/httpserver/middleware"
	"github.com/dujiao-next/internal/constants"
	apicredentialdomain "github.com/dujiao-next/internal/modules/apicredential/domain"
	apicredentialstore "github.com/dujiao-next/internal/modules/apicredential/infrastructure/gormstore"
	cardsecretdomain "github.com/dujiao-next/internal/modules/cardsecret/domain"
	categorydomain "github.com/dujiao-next/internal/modules/catalog/category/domain"
	productapplication "github.com/dujiao-next/internal/modules/catalog/product/application"
	productdomain "github.com/dujiao-next/internal/modules/catalog/product/domain"
	productstore "github.com/dujiao-next/internal/modules/catalog/product/store/gormstore"
	downstreamcallbackdomain "github.com/dujiao-next/internal/modules/downstreamcallback/domain"
	userdomain "github.com/dujiao-next/internal/modules/identity/user/domain"
	userstore "github.com/dujiao-next/internal/modules/identity/user/infrastructure/gormstore"
	kafudaapp "github.com/dujiao-next/internal/modules/kafudagpt/application"
	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
	kafudastore "github.com/dujiao-next/internal/modules/kafudagpt/infrastructure/gormstore"
	orderapp "github.com/dujiao-next/internal/modules/order/application"
	orderdomain "github.com/dujiao-next/internal/modules/order/domain"
	orderstore "github.com/dujiao-next/internal/modules/order/infrastructure/gormstore"
	paymentdomain "github.com/dujiao-next/internal/modules/payment/domain"
	promotiondomain "github.com/dujiao-next/internal/modules/promotion/domain"
	promotionstore "github.com/dujiao-next/internal/modules/promotion/infrastructure/gormstore"
	resellercontract "github.com/dujiao-next/internal/modules/reseller/contract"
	upstreamhttp "github.com/dujiao-next/internal/modules/upstreamapi/transport/http"
	"github.com/dujiao-next/internal/shared/jsonmap"
	"github.com/dujiao-next/internal/shared/money"
	"github.com/dujiao-next/internal/upstream"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

func TestAuthenticatedDownstreamExcludesDirectProducts(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "catalog.sqlite")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = pool.Close() })
	if err := db.AutoMigrate(&categorydomain.Category{}, &productdomain.Product{}, &productdomain.ProductSKU{},
		&cardsecretdomain.Secret{}, &userdomain.User{}, &apicredentialdomain.ApiCredential{}); err != nil {
		t.Fatal(err)
	}
	category := categorydomain.Category{Slug: "catalog", NameJSON: jsonmap.JSON{"en-US": "Catalog"}, IsActive: true}
	user := userdomain.User{Email: "downstream@example.test", PasswordHash: "test", Status: constants.UserStatusActive}
	if err := db.Create(&category).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	credentials := apicredentialstore.New(db)
	credential := apicredentialdomain.ApiCredential{
		UserID: user.ID, ApiKey: "test-downstream-key", ApiSecret: "test-downstream-secret",
		Status: constants.ApiCredentialStatusApproved, IsActive: true, LastUsedAt: &now,
	}
	if err := credentials.Create(&credential); err != nil {
		t.Fatal(err)
	}
	products := []productdomain.Product{
		{Slug: "direct-active", FulfillmentType: constants.FulfillmentTypeKafudaGPT, IsActive: true, SortOrder: 40},
		{Slug: "direct-inactive", FulfillmentType: constants.FulfillmentTypeKafudaGPT, SortOrder: 30},
		{Slug: "manual-active", FulfillmentType: constants.FulfillmentTypeManual, IsActive: true, SortOrder: 20},
		{Slug: "manual-inactive", FulfillmentType: constants.FulfillmentTypeManual, SortOrder: 10},
	}
	for index := range products {
		product := &products[index]
		product.CategoryID = category.ID
		product.TitleJSON = jsonmap.JSON{"en-US": product.Slug}
	}
	if err := db.Create(&products).Error; err != nil {
		t.Fatal(err)
	}

	repo := productstore.NewProductStore(db)
	handler := &upstreamhttp.Handler{Dependencies: upstreamhttp.Dependencies{
		Products: productServiceAdapter{products: productapplication.NewService(productapplication.Options{Products: repo})},
		Users:    userstore.New(db),
	}}
	router := gin.New()
	routes := router.Group("/api/v1/upstream", middleware.UpstreamAPIAuthMiddleware(credentials))
	upstreamhttp.RegisterAuthenticatedRoutes(routes, handler)
	request := func(method, path, body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		timestamp := time.Now().Unix()
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set(upstream.HeaderApiKey, credential.ApiKey)
		request.Header.Set(upstream.HeaderTimestamp, strconv.FormatInt(timestamp, 10))
		request.Header.Set(upstream.HeaderSignature, upstream.Sign(credential.ApiSecret, request.Method, request.URL.Path, timestamp, []byte(body)))
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		return recorder
	}

	const base = "/api/v1/upstream/products"
	for _, test := range []struct {
		name  string
		query string
		slug  string
		total int
	}{
		{"active", "?page_size=1", "manual-active", 1},
		{"full_first_page", "?include_inactive=true&page_size=1", "manual-active", 2},
		{"full_second_page", "?include_inactive=true&page=2&page_size=1", "manual-inactive", 2},
		{"incremental", "?include_inactive=true&page_size=1&updated_after=" + now.Add(-time.Hour).UTC().Format(time.RFC3339), "manual-active", 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := request(http.MethodGet, base+test.query, "")
			var body struct {
				OK    bool `json:"ok"`
				Total int  `json:"total"`
				Items []struct {
					Slug string `json:"slug"`
				} `json:"items"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if response.Code != http.StatusOK || !body.OK || body.Total != test.total ||
				len(body.Items) != 1 || body.Items[0].Slug != test.slug {
				t.Fatalf("unexpected list response: %d %s", response.Code, response.Body.String())
			}
		})
	}
	for _, product := range products {
		t.Run(product.Slug, func(t *testing.T) {
			response := request(http.MethodGet, fmt.Sprintf("%s/%d", base, product.ID), "")
			var body struct {
				OK        bool   `json:"ok"`
				ErrorCode string `json:"error_code"`
				Product   struct {
					Slug string `json:"slug"`
				} `json:"product"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if product.FulfillmentType == constants.FulfillmentTypeKafudaGPT {
				if response.Code != http.StatusNotFound || body.OK || body.ErrorCode != "product_not_found" || body.Product.Slug != "" {
					t.Fatalf("unexpected Direct detail response: %d %s", response.Code, response.Body.String())
				}
				return
			}
			if response.Code != http.StatusOK || !body.OK || body.Product.Slug != product.Slug {
				t.Fatalf("unexpected ordinary detail response: %d %s", response.Code, response.Body.String())
			}
		})
	}
	t.Run("order", func(t *testing.T) {
		writeModels := []any{
			&orderdomain.Order{}, &orderdomain.OrderItem{}, &kafudadomain.Recharge{},
			&downstreamcallbackdomain.OrderRef{}, &paymentdomain.Payment{},
		}
		if err := db.AutoMigrate(append([]any{&kafudadomain.Catalog{}, &promotiondomain.Promotion{}}, writeModels...)...); err != nil {
			t.Fatal(err)
		}
		sku := productdomain.ProductSKU{
			ProductID: products[0].ID, SKUCode: kafudacontract.SKUCode("plus", "PH"),
			PriceAmount: money.FromDecimal(decimal.NewFromInt(20)), DirectCostConfigured: true, IsActive: true,
			SpecValuesJSON: jsonmap.JSON{"plan": "plus", "country": "PH"},
		}
		if err := db.Create(&sku).Error; err != nil {
			t.Fatal(err)
		}
		catalog := kafudastore.New(db)
		if err := catalog.SaveCatalog(t.Context(), &kafudadomain.Catalog{
			Profile: "checkout", Enabled: true, SyncedAt: now, ExpiresAt: now.Add(time.Hour),
			Options: []kafudadomain.CatalogOption{{Plan: "plus", Country: "PH", Product: "gpt", Available: true}},
			Cards:   []kafudadomain.CardProduct{{ProductCode: "test-card"}},
		}); err != nil {
			t.Fatal(err)
		}
		skuRepo := productstore.NewSKUStore(db)
		queue := &downstreamTestQueue{}
		orders := orderapp.NewOrderService(orderapp.OrderServiceOptions{
			OrderStore:   orderstore.New(db, "test-guest-credential-secret-with-32-bytes"),
			ProductStore: repo, ProductSKUStore: skuRepo,
			PromotionRepo: promotionstore.New(db),
			Queue:         queue,
			DirectCatalog: kafudaapp.NewCommerceService(kafudaapp.CommerceOptions{
				Catalog:                 kafudaapp.NewCatalogService(catalog, nil, "checkout", time.Hour),
				SalesEnabled:            true,
				ReferenceLookupVerified: true,
				ProductCode:             "test-card",
				MaxCostCents:            1500,
				AllowedKinds:            []string{"new"},
				WorkerReady:             func(context.Context) bool { return true },
			}),
		})
		if _, err := orders.PreviewOrder(orderapp.CreateOrderInput{
			StorefrontRequest: true, UserID: user.ID, Tenant: resellercontract.MainTenantContext("shop.example.test"),
			Items: []orderapp.CreateOrderItem{{ProductID: sku.ProductID, SKUID: sku.ID, Quantity: 1}},
		}); err != nil {
			t.Fatalf("main storefront preview should accept this SKU: %v", err)
		}
		handler.Orders = orderServiceAdapter{orders: orders}
		handler.ProductRepository = repo
		handler.SKUs = skuRepo
		result := request(http.MethodPost, "/api/v1/upstream/orders", fmt.Sprintf(`{"sku_id":%d,"quantity":1,"storefront_request":true}`, sku.ID))
		var response struct {
			OK        bool   `json:"ok"`
			ErrorCode string `json:"error_code"`
		}
		if err := json.Unmarshal(result.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if result.Code != http.StatusBadRequest || response.OK || response.ErrorCode != "product_unavailable" {
			t.Fatalf("unexpected Direct checkout response: %d %s", result.Code, result.Body.String())
		}
		if queue.enqueued != 0 {
			t.Fatalf("rejected Direct checkout enqueued %d tasks", queue.enqueued)
		}
		for _, model := range writeModels {
			var count int64
			if err := db.Model(model).Count(&count).Error; err != nil || count != 0 {
				t.Fatalf("rejected Direct checkout wrote %T: count=%d, error=%v", model, count, err)
			}
		}
	})
}

type downstreamTestQueue struct{ enqueued int }

func (*downstreamTestQueue) Enabled() bool { return true }
func (q *downstreamTestQueue) EnqueueTimeoutCancel(uint, time.Duration) error {
	q.enqueued++
	return nil
}
func (q *downstreamTestQueue) EnqueueStatusEmail(uint, string) error {
	q.enqueued++
	return nil
}
