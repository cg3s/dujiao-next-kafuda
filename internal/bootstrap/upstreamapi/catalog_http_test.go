package upstreamwiring

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
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
	userdomain "github.com/dujiao-next/internal/modules/identity/user/domain"
	userstore "github.com/dujiao-next/internal/modules/identity/user/infrastructure/gormstore"
	upstreamhttp "github.com/dujiao-next/internal/modules/upstreamapi/transport/http"
	"github.com/dujiao-next/internal/shared/jsonmap"
	"github.com/dujiao-next/internal/upstream"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestAuthenticatedDownstreamCatalogExcludesDirectProducts(t *testing.T) {
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
	get := func(path string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		timestamp := time.Now().Unix()
		request.Header.Set(upstream.HeaderApiKey, credential.ApiKey)
		request.Header.Set(upstream.HeaderTimestamp, strconv.FormatInt(timestamp, 10))
		request.Header.Set(upstream.HeaderSignature, upstream.Sign(credential.ApiSecret, request.Method, request.URL.Path, timestamp, nil))
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
			response := get(base + test.query)
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
			response := get(fmt.Sprintf("%s/%d", base, product.ID))
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
}
