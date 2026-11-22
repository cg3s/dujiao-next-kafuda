package integrationtest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	promotiondomain "github.com/dujiao-next/internal/modules/promotion/domain"

	"github.com/dujiao-next/internal/constants"
	productdomain "github.com/dujiao-next/internal/modules/catalog/product/domain"
	producthttp "github.com/dujiao-next/internal/modules/catalog/product/transport/http"
	productpresenter "github.com/dujiao-next/internal/modules/catalog/product/transport/presenter"
	contentcontract "github.com/dujiao-next/internal/modules/content/contract"
	kafudaapp "github.com/dujiao-next/internal/modules/kafudagpt/application"
	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
	promotionapp "github.com/dujiao-next/internal/modules/promotion/application"
	promotiongormstore "github.com/dujiao-next/internal/modules/promotion/infrastructure/gormstore"
	reseller "github.com/dujiao-next/internal/modules/reseller/contract"
	"github.com/dujiao-next/internal/shared/jsonmap"
	"github.com/dujiao-next/internal/shared/money"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

type staticPublicProductQueries struct {
	product productdomain.Product
}

func (s staticPublicProductQueries) ListPublicForTenant(reseller.TenantContext, string, string, int, int) ([]productdomain.Product, int64, error) {
	return []productdomain.Product{s.product}, 1, nil
}

func (s staticPublicProductQueries) GetPublicBySlugForTenant(reseller.TenantContext, string) (*productdomain.Product, error) {
	product := s.product
	return &product, nil
}

func (staticPublicProductQueries) ApplyAutoStockCounts([]productdomain.Product) error {
	return nil
}

type emptyRelatedPostReader struct{}

func (emptyRelatedPostReader) ListPostsForProduct(context.Context, uint, int) ([]contentcontract.RelatedPost, error) {
	return nil, nil
}

func TestPublicProductHTTPPromotionUsesDisplayPrice(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dsn := fmt.Sprintf("file:catalog_public_product_display_price_%d?mode=memory&cache=shared", time.Now().UnixNano())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite failed: %v", err)
	}
	if err := db.AutoMigrate(&promotiondomain.Promotion{}); err != nil {
		t.Fatalf("auto migrate failed: %v", err)
	}

	promotion := promotiondomain.Promotion{
		Name:       "fixed-10",
		ScopeType:  constants.ScopeTypeProduct,
		ScopeRefID: 1,
		Type:       constants.PromotionTypeFixed,
		Value:      money.FromDecimal(decimal.NewFromInt(10)),
		MinAmount:  money.FromDecimal(decimal.Zero),
		IsActive:   true,
	}
	if err := db.Create(&promotion).Error; err != nil {
		t.Fatalf("create promotion failed: %v", err)
	}

	queries := staticPublicProductQueries{product: productdomain.Product{
		ID:          1,
		Slug:        "display-price",
		PriceAmount: money.FromDecimal(decimal.RequireFromString("59.90")),
		SKUs: []productdomain.ProductSKU{
			{
				ID:                   21,
				IsActive:             true,
				SortOrder:            100,
				PriceAmount:          money.FromDecimal(decimal.RequireFromString("89.90")),
				CostPriceAmount:      money.FromDecimal(decimal.RequireFromString("25.00")),
				DirectCostConfigured: true,
			},
			{
				ID:          22,
				IsActive:    true,
				SortOrder:   10,
				PriceAmount: money.FromDecimal(decimal.RequireFromString("49.90")),
			},
		},
	}}
	promotions := promotionapp.NewService(promotiongormstore.New(db))
	handler := producthttp.NewPublicHandler(queries, nil, promotions, nil, nil, nil, emptyRelatedPostReader{}, nil)

	router := gin.New()
	producthttp.RegisterPublicRoutes(router, handler)
	request := httptest.NewRequest(http.MethodGet, "/products/display-price", nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	for _, field := range []string{"cost_price_amount", "direct_cost_configured", "direct_catalog_revision"} {
		if strings.Contains(recorder.Body.String(), field) {
			t.Fatalf("public response exposed %s", field)
		}
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d body=%s", recorder.Code, recorder.Body.String())
	}

	var envelope struct {
		Data productpresenter.Product `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode product response: %v", err)
	}
	if envelope.Data.PromotionPriceAmount == nil {
		t.Fatal("expected promotion price amount")
	}

	expectedDisplay := decimal.RequireFromString("89.90")
	expectedPromotion := decimal.RequireFromString("79.90")
	if !envelope.Data.PriceAmount.Decimal.Equal(expectedDisplay) {
		t.Fatalf("expected display price %s, got %s", expectedDisplay, envelope.Data.PriceAmount.String())
	}
	if !envelope.Data.PromotionPriceAmount.Decimal.Equal(expectedPromotion) {
		t.Fatalf("expected promotion display price %s, got %s", expectedPromotion, envelope.Data.PromotionPriceAmount.String())
	}
}

type publicDirectCatalogStore struct {
	kafudacontract.Store
	catalog *kafudadomain.Catalog
	err     error
	reads   int
}

func (s *publicDirectCatalogStore) GetCatalog(context.Context, string) (*kafudadomain.Catalog, error) {
	s.reads++
	return s.catalog, s.err
}

func TestPublicDirectProductAvailability(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, scenario := range []string{"available", "zero_revision", "expired", "unlisted", "disabled_catalog", "missing_catalog", "stale_revision", "restricted_card", "sales_disabled", "worker_down", "lookup_unverified", "missing_cost", "invalid_price", "invalid_selection", "disabled_sku", "deleted_sku", "mapped", "quantity_limit", "read_error"} {
		t.Run(scenario, func(t *testing.T) {
			catalog := &kafudadomain.Catalog{
				Profile: "private-profile", Enabled: true, ExpiresAt: time.Now().Add(time.Hour),
				Options:               []kafudadomain.CatalogOption{{Plan: "plus", Country: "PH", Product: "gpt", Available: true}},
				Cards:                 []kafudadomain.CardProduct{{ProductCode: "private-card"}},
				AvailabilityRevisions: map[string]uint64{"plus\x00PH": 2},
			}
			store := &publicDirectCatalogStore{catalog: catalog}
			options := kafudaapp.CommerceOptions{
				Catalog:      kafudaapp.NewCatalogService(store, nil, catalog.Profile, time.Hour),
				SalesEnabled: scenario != "sales_disabled", ReferenceLookupVerified: scenario != "lookup_unverified",
				ProductCode: "private-card", MaxCostCents: 2200, AllowedKinds: []string{"new"},
				WorkerReady: func(context.Context) bool { return scenario != "worker_down" },
			}
			sku := productdomain.ProductSKU{
				ID: 10, ProductID: 1, IsActive: true, SKUCode: kafudacontract.SKUCode("plus", "PH"),
				PriceAmount: money.FromDecimal(decimal.NewFromInt(90)), DirectCostConfigured: true, DirectCatalogRevision: 2,
				SpecValuesJSON: jsonmap.JSON{"plan": "plus", "country": "PH", "merchant_cost": "private-value"},
			}
			otherSKU := sku
			otherSKU.ID = 11
			otherSKU.SKUCode = kafudacontract.SKUCode("plus", "US")
			otherSKU.SpecValuesJSON = jsonmap.JSON{"plan": "plus", "country": "US"}
			otherSKU.PriceAmount = money.FromDecimal(decimal.NewFromInt(55))
			// Old stock fields must neither make an unavailable option purchasable nor leak counts.
			otherSKU.ManualStockTotal, otherSKU.ManualStockSold, otherSKU.AutoStockAvailable, otherSKU.UpstreamStock = -1, 5, 6, 7
			product := productdomain.Product{
				ID: 1, Slug: "direct", IsActive: true, FulfillmentType: constants.FulfillmentTypeKafudaGPT,
				PurchaseType: constants.ProductPurchaseGuest, StockDisplayMode: constants.ProductStockDisplayExact,
				ManualFormSchemaJSON: jsonmap.JSON{"legacy": "private-value"},
			}
			switch scenario {
			case "zero_revision":
				catalog.AvailabilityRevisions = nil
				sku.DirectCatalogRevision, otherSKU.DirectCatalogRevision = 0, 0
			case "expired":
				catalog.ExpiresAt = time.Now().Add(-time.Minute)
			case "unlisted":
				catalog.Options[0].Available = false
			case "disabled_catalog":
				catalog.Enabled = false
			case "missing_catalog":
				store.catalog = nil
			case "stale_revision":
				sku.DirectCatalogRevision--
			case "restricted_card":
				catalog.Cards[0].Restricted = true
			case "missing_cost":
				sku.DirectCostConfigured = false
			case "invalid_price":
				sku.PriceAmount = money.FromDecimal(decimal.Zero)
			case "invalid_selection":
				sku.SpecValuesJSON["country"] = "US"
			case "disabled_sku":
				sku.IsActive = false
			case "deleted_sku":
				deleted := time.Now()
				sku.DeletedAt = &deleted
			case "mapped":
				product.IsMapped = true
			case "quantity_limit":
				product.MinPurchaseQuantity = 2
			case "read_error":
				store.err = errors.New("private-value")
			}
			product.SKUs = []productdomain.ProductSKU{otherSKU, sku}
			handler := producthttp.NewPublicHandler(staticPublicProductQueries{product: product}, nil, nil, nil, nil, nil, emptyRelatedPostReader{}, kafudaapp.NewCommerceService(options))
			router := gin.New()
			producthttp.RegisterPublicRoutes(router, handler)
			for _, path := range []string{"/products", "/products/direct"} {
				store.reads = 0
				recorder := httptest.NewRecorder()
				router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
				var envelope struct {
					Status int             `json:"status_code"`
					Data   json.RawMessage `json:"data"`
				}
				if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil || recorder.Code != http.StatusOK || envelope.Status != 0 {
					t.Fatalf("%s: invalid response: status=%d body=%s err=%v", path, recorder.Code, recorder.Body.String(), err)
				}
				var result productpresenter.Product
				if path == "/products" {
					var list []productpresenter.Product
					if err := json.Unmarshal(envelope.Data, &list); err != nil || len(list) != 1 {
						t.Fatalf("invalid list: %s %v", envelope.Data, err)
					}
					result = list[0]
				} else if err := json.Unmarshal(envelope.Data, &result); err != nil {
					t.Fatal(err)
				}
				for _, field := range []string{"cost_price_amount", "direct_catalog_revision", "direct_cost_configured", "private-"} {
					if strings.Contains(recorder.Body.String(), field) {
						t.Fatalf("%s: exposed private field %q", path, field)
					}
				}
				wantSoldOut := scenario != "available" && scenario != "zero_revision"
				if len(result.SKUs) != 2 || result.IsSoldOut != wantSoldOut || !result.StockQuantityHidden ||
					result.PurchaseType != constants.ProductPurchaseGuest || result.ManualFormSchema != nil {
					t.Fatalf("%s: invalid product availability: %+v", path, result)
				}
				if !result.SKUs[0].IsSoldOut || result.SKUs[1].IsSoldOut != wantSoldOut || (!wantSoldOut && !result.PriceAmount.Equal(sku.PriceAmount.Decimal)) {
					t.Fatalf("%s: invalid SKU availability or price: %+v", path, result.SKUs)
				}
				for _, option := range result.SKUs {
					if !option.StockQuantityHidden || option.ManualStockTotal != 0 || option.ManualStockSold != 0 || option.AutoStockAvailable != 0 || option.UpstreamStock != 0 {
						t.Fatalf("%s: unexpected direct inventory: %+v", path, option)
					}
				}
				wantReads := 1
				if !options.SalesEnabled || !options.ReferenceLookupVerified || !options.WorkerReady(context.Background()) {
					wantReads = 0
				}
				if store.reads != wantReads {
					t.Fatalf("%s: catalog reads = %d, want %d", path, store.reads, wantReads)
				}
			}
		})
	}
}
