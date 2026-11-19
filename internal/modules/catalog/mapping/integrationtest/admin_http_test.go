package integrationtest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	mappingdomain "github.com/dujiao-next/internal/modules/catalog/mapping/domain"

	siteconnectiondomain "github.com/dujiao-next/internal/modules/siteconnection/domain"

	productdomain "github.com/dujiao-next/internal/modules/catalog/product/domain"

	categoryapp "github.com/dujiao-next/internal/modules/catalog/category/application"
	categorydomain "github.com/dujiao-next/internal/modules/catalog/category/domain"

	"github.com/dujiao-next/internal/constants"
	categorygormstore "github.com/dujiao-next/internal/modules/catalog/category/infrastructure/gormstore"
	mappingapp "github.com/dujiao-next/internal/modules/catalog/mapping/application"
	mappingcontract "github.com/dujiao-next/internal/modules/catalog/mapping/contract"
	mappinggormstore "github.com/dujiao-next/internal/modules/catalog/mapping/infrastructure/gormstore"
	mappinghttp "github.com/dujiao-next/internal/modules/catalog/mapping/transport/http"
	productgormstore "github.com/dujiao-next/internal/modules/catalog/product/store/gormstore"
	"github.com/dujiao-next/internal/shared/jsonmap"
	"github.com/dujiao-next/internal/shared/money"
	"github.com/dujiao-next/internal/upstream"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

type adminNoopLocalMediaRecorder struct{}

func (adminNoopLocalMediaRecorder) RecordLocalFile(context.Context, string, string) {}

type mappingConnectionProvider struct {
	db      *gorm.DB
	adapter upstream.Adapter
}

func (p *mappingConnectionProvider) GetByID(id uint) (*siteconnectiondomain.Connection, error) {
	var conn siteconnectiondomain.Connection
	if err := p.db.First(&conn, id).Error; err != nil {
		return nil, err
	}
	return &conn, nil
}

func (p *mappingConnectionProvider) GetAdapter(conn *siteconnectiondomain.Connection) (upstream.Adapter, error) {
	return p.adapter, nil
}

type mappingImportUoW struct {
	products    *productgormstore.ProductStore
	skus        *productgormstore.SKUStore
	mappings    *mappinggormstore.MappingStore
	skuMappings *mappinggormstore.SKUMappingStore
}

func (unit *mappingImportUoW) WithinTransaction(fn func(mappingcontract.ImportRepositories) error) error {
	if fn == nil {
		return nil
	}
	return unit.products.Transaction(func(tx *gorm.DB) error {
		return fn(mappingcontract.ImportRepositories{
			Products:    unit.products.BindTx(tx),
			SKUs:        unit.skus.BindTx(tx),
			Mappings:    unit.mappings.WithTx(tx),
			SKUMappings: unit.skuMappings.WithTx(tx),
		})
	})
}

func setupAdminHandlerTest(t *testing.T, upstreamHandler http.HandlerFunc) (*mappinghttp.AdminHandler, *gorm.DB, uint, func()) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	dsn := fmt.Sprintf("file:admin_product_mapping_handler_test_%d?mode=memory&cache=shared", time.Now().UnixNano())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite failed: %v", err)
	}
	if err := db.AutoMigrate(
		&categorydomain.Category{},
		&productdomain.Product{},
		&productdomain.ProductSKU{},
		&siteconnectiondomain.Connection{},
		&mappingdomain.Mapping{},
		&mappingdomain.SKUMapping{},
	); err != nil {
		t.Fatalf("auto migrate failed: %v", err)
	}

	server := httptest.NewServer(upstreamHandler)
	categoryRepo := categorygormstore.NewCategoryStore(db)
	categoryService := categoryapp.NewService(categoryRepo)

	conn := siteconnectiondomain.Connection{
		Name:      "upstream",
		BaseURL:   server.URL,
		ApiKey:    "test-key",
		ApiSecret: "test-secret",
		Protocol:  constants.ConnectionProtocolDujiaoNext,
		Status:    constants.ConnectionStatusActive,
	}
	if err := db.Create(&conn).Error; err != nil {
		t.Fatalf("create connection failed: %v", err)
	}

	adapter, err := upstream.NewAdapter(&conn, t.TempDir())
	if err != nil {
		t.Fatalf("create upstream adapter failed: %v", err)
	}

	productStore := productgormstore.NewProductStore(db)
	skuStore := productgormstore.NewSKUStore(db)
	mappingStore := mappinggormstore.NewMappingStore(db)
	skuMappingStore := mappinggormstore.NewSKUMappingStore(db)

	mappingService, err := mappingapp.NewService(mappingapp.Options{
		Mappings:    mappingStore,
		SKUMappings: skuMappingStore,
		Products:    productStore,
		SKUs:        skuStore,
		Categories:  categoryRepo,
		Connections: &mappingConnectionProvider{db: db, adapter: adapter},
		Media:       adminNoopLocalMediaRecorder{},
		Transactions: &mappingImportUoW{
			products:    productStore,
			skus:        skuStore,
			mappings:    mappingStore,
			skuMappings: skuMappingStore,
		},
	})
	if err != nil {
		t.Fatalf("create product mapping service: %v", err)
	}
	mappingService.SetCategoryCreator(categoryService)

	h := mappinghttp.NewAdminHandler(mappingService)
	return h, db, conn.ID, server.Close
}

func TestBatchImportUpstreamProductsAutoCreatesCategory(t *testing.T) {
	h, db, connID, cleanup := setupAdminHandlerTest(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/upstream/categories":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"categories": []upstream.UpstreamCategory{
					{ID: 9, Slug: "upstream-streaming", Name: jsonmap.JSON{"zh-CN": "流媒体"}},
				},
			})
		case "/api/v1/upstream/products/101":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"product": upstream.UpstreamProduct{
					ID:              101,
					CategoryID:      9,
					Title:           jsonmap.JSON{"zh-CN": "上游商品"},
					Description:     jsonmap.JSON{"zh-CN": "描述"},
					Content:         jsonmap.JSON{"zh-CN": "内容"},
					Images:          []string{},
					Tags:            []string{},
					PriceAmount:     "10.00",
					Currency:        "CNY",
					FulfillmentType: constants.FulfillmentTypeAuto,
					IsActive:        true,
					SKUs: []upstream.UpstreamSKU{
						{ID: 201, SKUCode: "SKU-A", SpecValues: jsonmap.JSON{"name": "A"}, PriceAmount: "10.00", IsActive: true},
					},
				},
			})
		default:
			http.NotFound(w, r)
		}
	})
	defer cleanup()

	body := fmt.Sprintf(`{"connection_id":%d,"upstream_product_ids":[101],"auto_create_category":true}`, connID)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/product-mappings/batch-import", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req

	h.BatchImportUpstreamProducts(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d body=%s", w.Code, w.Body.String())
	}

	var imported productdomain.Product
	if err := db.First(&imported).Error; err != nil {
		t.Fatalf("load imported product failed: %v", err)
	}
	if imported.CategoryID == 0 {
		t.Fatalf("expected imported product to be assigned to auto-created category")
	}

	var category categorydomain.Category
	if err := db.First(&category, imported.CategoryID).Error; err != nil {
		t.Fatalf("load auto-created category failed: %v", err)
	}
	if category.Slug != "upstream-streaming" {
		t.Fatalf("expected auto-created category slug upstream-streaming, got %q", category.Slug)
	}
}

func TestBatchImportUpstreamProductsAutoCategoryRejectsUpstreamProductWithoutCategory(t *testing.T) {
	h, db, connID, cleanup := setupAdminHandlerTest(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/upstream/categories":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":         true,
				"categories": []upstream.UpstreamCategory{},
			})
		case "/api/v1/upstream/products/101":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"product": upstream.UpstreamProduct{
					ID:              101,
					CategoryID:      0,
					Title:           jsonmap.JSON{"zh-CN": "无分类上游商品"},
					PriceAmount:     "10.00",
					Currency:        "CNY",
					FulfillmentType: constants.FulfillmentTypeAuto,
					IsActive:        true,
					SKUs: []upstream.UpstreamSKU{
						{ID: 201, SKUCode: "SKU-A", SpecValues: jsonmap.JSON{"name": "A"}, PriceAmount: "10.00", IsActive: true},
					},
				},
			})
		default:
			http.NotFound(w, r)
		}
	})
	defer cleanup()

	body := fmt.Sprintf(`{"connection_id":%d,"upstream_product_ids":[101],"auto_create_category":true}`, connID)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/product-mappings/batch-import", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req

	h.BatchImportUpstreamProducts(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d body=%s", w.Code, w.Body.String())
	}

	var resp struct {
		Data struct {
			SuccessCount int `json:"success_count"`
			Results      []struct {
				UpstreamProductID uint   `json:"upstream_product_id"`
				Success           bool   `json:"success"`
				Error             string `json:"error"`
			} `json:"results"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response failed: %v body=%s", err, w.Body.String())
	}
	if resp.Data.SuccessCount != 0 || len(resp.Data.Results) != 1 || resp.Data.Results[0].Success {
		t.Fatalf("expected the category-less product to fail, got %+v", resp.Data)
	}
	if resp.Data.Results[0].Error != mappingcontract.ErrUpstreamProductNoCategory.Error() {
		t.Fatalf("unexpected error message %q", resp.Data.Results[0].Error)
	}

	var productCount, mappingCount int64
	if err := db.Model(&productdomain.Product{}).Count(&productCount).Error; err != nil {
		t.Fatalf("count products failed: %v", err)
	}
	if err := db.Model(&mappingdomain.Mapping{}).Count(&mappingCount).Error; err != nil {
		t.Fatalf("count mappings failed: %v", err)
	}
	if productCount != 0 || mappingCount != 0 {
		t.Fatalf("expected nothing imported, got products=%d mappings=%d", productCount, mappingCount)
	}
}

func TestBatchImportUpstreamProductsRestoresSoftDeletedAutoCategory(t *testing.T) {
	h, db, connID, cleanup := setupAdminHandlerTest(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/upstream/categories":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"categories": []upstream.UpstreamCategory{
					{ID: 9, Slug: "upstream-streaming", Name: jsonmap.JSON{"zh-CN": "流媒体"}},
				},
			})
		case "/api/v1/upstream/products/101":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"product": upstream.UpstreamProduct{
					ID:              101,
					CategoryID:      9,
					Title:           jsonmap.JSON{"zh-CN": "上游商品"},
					PriceAmount:     "10.00",
					Currency:        "CNY",
					FulfillmentType: constants.FulfillmentTypeAuto,
					IsActive:        true,
					SKUs: []upstream.UpstreamSKU{
						{ID: 201, SKUCode: "SKU-A", SpecValues: jsonmap.JSON{"name": "A"}, PriceAmount: "10.00", IsActive: true},
					},
				},
			})
		default:
			http.NotFound(w, r)
		}
	})
	defer cleanup()

	deletedCategory := categorydomain.Category{
		Slug:     "upstream-streaming",
		NameJSON: jsonmap.JSON{"zh-CN": "已删除分类"},
		IsActive: true,
	}
	if err := db.Create(&deletedCategory).Error; err != nil {
		t.Fatalf("create soft-delete target category failed: %v", err)
	}
	if err := categorygormstore.NewCategoryStore(db).Delete(fmt.Sprintf("%d", deletedCategory.ID)); err != nil {
		t.Fatalf("soft delete category failed: %v", err)
	}

	body := fmt.Sprintf(`{"connection_id":%d,"upstream_product_ids":[101],"auto_create_category":true}`, connID)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/product-mappings/batch-import", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req

	h.BatchImportUpstreamProducts(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d body=%s", w.Code, w.Body.String())
	}

	var imported productdomain.Product
	if err := db.First(&imported).Error; err != nil {
		t.Fatalf("load imported product failed: %v", err)
	}
	if imported.CategoryID != deletedCategory.ID {
		t.Fatalf("expected imported product category %d, got %d", deletedCategory.ID, imported.CategoryID)
	}

	var restored categorydomain.Category
	if err := db.First(&restored, deletedCategory.ID).Error; err != nil {
		t.Fatalf("expected category to be restored, got %v", err)
	}
	if restored.NameJSON["zh-CN"] != "流媒体" {
		t.Fatalf("expected restored category name to be refreshed, got %+v", restored.NameJSON)
	}
}

func TestBatchImportByCategoryUncategorizedGroup(t *testing.T) {
	newHandler := func(t *testing.T) (*mappinghttp.AdminHandler, *gorm.DB, uint, func()) {
		return setupAdminHandlerTest(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/api/v1/upstream/categories":
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "categories": []upstream.UpstreamCategory{}})
			case "/api/v1/upstream/products":
				_ = json.NewEncoder(w).Encode(map[string]any{
					"ok":       true,
					"items":    []upstream.UpstreamProduct{{ID: 101, CategoryID: 0, Title: jsonmap.JSON{"zh-CN": "无分类"}, IsActive: true}},
					"total":    1,
					"page":     1,
					"per_page": 50,
				})
			case "/api/v1/upstream/products/101":
				_ = json.NewEncoder(w).Encode(map[string]any{
					"ok": true,
					"product": upstream.UpstreamProduct{
						ID: 101, CategoryID: 0, Title: jsonmap.JSON{"zh-CN": "无分类"},
						PriceAmount: "10.00", Currency: "CNY", FulfillmentType: constants.FulfillmentTypeAuto, IsActive: true,
						SKUs: []upstream.UpstreamSKU{{ID: 201, SKUCode: "SKU-A", SpecValues: jsonmap.JSON{"name": "A"}, PriceAmount: "10.00", IsActive: true}},
					},
				})
			default:
				http.NotFound(w, r)
			}
		})
	}
	call := func(h *mappinghttp.AdminHandler, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/product-mappings/batch-import-by-category", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = req
		h.BatchImportByCategory(c)
		return w
	}

	t.Run("auto create without local category is rejected", func(t *testing.T) {
		h, db, connID, cleanup := newHandler(t)
		defer cleanup()
		w := call(h, fmt.Sprintf(`{"connection_id":%d,"upstream_category_id":0,"auto_create_category":true}`, connID))
		// 业务错误码在响应体 status_code 里，HTTP 状态码仍是 200
		var resp struct {
			StatusCode int `json:"status_code"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode response failed: %v body=%s", err, w.Body.String())
		}
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("expected status_code 400, got %d body=%s", resp.StatusCode, w.Body.String())
		}
		var n int64
		db.Model(&productdomain.Product{}).Count(&n)
		if n != 0 {
			t.Fatalf("expected nothing imported, got %d products", n)
		}
	})

	t.Run("local category imports uncategorized products", func(t *testing.T) {
		h, db, connID, cleanup := newHandler(t)
		defer cleanup()
		local := categorydomain.Category{Slug: "local", NameJSON: jsonmap.JSON{"zh-CN": "本地"}, IsActive: true}
		if err := db.Create(&local).Error; err != nil {
			t.Fatalf("create category failed: %v", err)
		}
		w := call(h, fmt.Sprintf(`{"connection_id":%d,"upstream_category_id":0,"local_category_id":%d}`, connID, local.ID))
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
		}
		var p productdomain.Product
		if err := db.First(&p).Error; err != nil {
			t.Fatalf("load product failed: %v", err)
		}
		if p.CategoryID != local.ID {
			t.Fatalf("expected category %d, got %d", local.ID, p.CategoryID)
		}
	})
}

func TestBatchImportUsesCategoryMarkupWithParentInheritance(t *testing.T) {
	h, db, connID, cleanup := setupAdminHandlerTest(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/upstream/categories":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"categories": []upstream.UpstreamCategory{
					{ID: 9, Slug: "parent", Name: jsonmap.JSON{"zh-CN": "父"}},
					{ID: 10, ParentID: 9, Slug: "child", Name: jsonmap.JSON{"zh-CN": "子"}},
				},
			})
		case "/api/v1/upstream/products/101":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"product": upstream.UpstreamProduct{
					ID: 101, CategoryID: 10, Title: jsonmap.JSON{"zh-CN": "商品"},
					PriceAmount: "10.00", Currency: "CNY", FulfillmentType: constants.FulfillmentTypeAuto, IsActive: true,
					SKUs: []upstream.UpstreamSKU{{ID: 201, SKUCode: "SKU-A", SpecValues: jsonmap.JSON{"name": "A"}, PriceAmount: "10.00", IsActive: true}},
				},
			})
		default:
			http.NotFound(w, r)
		}
	})
	defer cleanup()

	// 连接默认加价 10%，父分类 9 单独设置 50%；商品在子分类 10 下，应继承父分类的 50%。
	if err := db.Model(&siteconnectiondomain.Connection{}).Where("id = ?", connID).Updates(map[string]interface{}{
		"price_markup_percent": 10,
		"category_markups":     jsonmap.JSON{"9": float64(50)},
	}).Error; err != nil {
		t.Fatalf("update connection failed: %v", err)
	}

	body := fmt.Sprintf(`{"connection_id":%d,"upstream_product_ids":[101],"auto_create_category":true}`, connID)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/product-mappings/batch-import", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	h.BatchImportUpstreamProducts(c)
	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d body=%s", w.Code, w.Body.String())
	}

	var sku productdomain.ProductSKU
	if err := db.First(&sku).Error; err != nil {
		t.Fatalf("load sku failed: %v", err)
	}
	if !sku.PriceAmount.Decimal.Equal(decimal.RequireFromString("15")) {
		t.Fatalf("expected inherited 50%% markup => 15.00, got %s", sku.PriceAmount.Decimal.String())
	}
	var mapping mappingdomain.Mapping
	if err := db.First(&mapping).Error; err != nil {
		t.Fatalf("load mapping failed: %v", err)
	}
	if mapping.UpstreamCategoryID != 10 {
		t.Fatalf("expected mapping upstream category 10, got %d", mapping.UpstreamCategoryID)
	}
}

type unmapTransactionHook struct {
	mappingcontract.UnitOfWork
	before func()
}

func (u unmapTransactionHook) WithinTransaction(fn func(mappingcontract.ImportRepositories) error) error {
	u.before()
	return u.UnitOfWork.WithinTransaction(fn)
}

func TestDeleteMappingUsesCurrentProductAndRollsBackFailures(t *testing.T) {
	for _, scenario := range []string{"upstream", "manual", "direct_after_read", "mapping_deleted", "mapping_reassigned", "product_missing", "fail_sku_mapping", "fail_product", "fail_mapping"} {
		t.Run(scenario, func(t *testing.T) {
			db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:unmap_%d?mode=memory&cache=shared", time.Now().UnixNano())), &gorm.Config{DisableForeignKeyConstraintWhenMigrating: true})
			if err != nil {
				t.Fatal(err)
			}
			pool, err := db.DB()
			if err != nil {
				t.Fatal(err)
			}
			pool.SetMaxOpenConns(1)
			t.Cleanup(func() { _ = pool.Close() })
			if err := db.AutoMigrate(&productdomain.Product{}, &productdomain.ProductSKU{}, &mappingdomain.Mapping{}, &mappingdomain.SKUMapping{}); err != nil {
				t.Fatal(err)
			}
			product := productdomain.Product{Slug: "unmap", TitleJSON: jsonmap.JSON{"zh-CN": "unmap"}, FulfillmentType: constants.FulfillmentTypeUpstream, IsMapped: true, IsActive: true}
			if scenario == "manual" {
				product.FulfillmentType = constants.FulfillmentTypeManual
			}
			if err := db.Create(&product).Error; err != nil {
				t.Fatal(err)
			}
			sku := productdomain.ProductSKU{ProductID: product.ID, SKUCode: "sku", IsActive: true}
			if err := db.Create(&sku).Error; err != nil {
				t.Fatal(err)
			}
			mapping := mappingdomain.Mapping{LocalProductID: product.ID, ConnectionID: 1, UpstreamProductID: 1}
			if err := db.Create(&mapping).Error; err != nil {
				t.Fatal(err)
			}
			skuMapping := mappingdomain.SKUMapping{ProductMappingID: mapping.ID, LocalSKUID: sku.ID, UpstreamSKUID: 1}
			if err := db.Create(&skuMapping).Error; err != nil {
				t.Fatal(err)
			}
			products := productgormstore.NewProductStore(db)
			mappings := mappinggormstore.NewMappingStore(db)
			unit := &mappingImportUoW{products: products, skus: productgormstore.NewSKUStore(db), mappings: mappings, skuMappings: mappinggormstore.NewSKUMappingStore(db)}
			injected := errors.New("injected unmap failure")
			var beforeSKU productdomain.ProductSKU
			var beforeProduct productdomain.Product
			var beforeMapping mappingdomain.Mapping
			var beforeSKUMapping mappingdomain.SKUMapping
			uow := unmapTransactionHook{UnitOfWork: unit, before: func() {
				switch scenario {
				case "direct_after_read":
					if err := db.Model(&product).Updates(map[string]interface{}{"fulfillment_type": constants.FulfillmentTypeKafudaGPT, "is_mapped": false}).Error; err != nil {
						t.Fatal(err)
					}
				case "mapping_deleted":
					if err := mappings.Delete(mapping.ID); err != nil {
						t.Fatal(err)
					}
				case "mapping_reassigned":
					if err := db.Model(&mapping).Update("local_product_id", product.ID+100).Error; err != nil {
						t.Fatal(err)
					}
				case "product_missing":
					if err := db.Model(&product).Update("deleted_at", time.Now()).Error; err != nil {
						t.Fatal(err)
					}
				}
				if err := db.Unscoped().First(&beforeProduct, product.ID).Error; err != nil {
					t.Fatal(err)
				}
				if err := db.First(&beforeMapping, mapping.ID).Error; err != nil {
					t.Fatal(err)
				}
				if err := db.First(&beforeSKUMapping, skuMapping.ID).Error; err != nil {
					t.Fatal(err)
				}
				if err := db.First(&beforeSKU, sku.ID).Error; err != nil {
					t.Fatal(err)
				}
				failTable := map[string]string{"fail_sku_mapping": "sku_mappings", "fail_product": "products", "fail_mapping": "product_mappings"}[scenario]
				if failTable != "" {
					if err := db.Callback().Update().Before("gorm:update").Register("fail_unmap", func(tx *gorm.DB) {
						if tx.Statement.Table == failTable {
							tx.AddError(injected)
						}
					}); err != nil {
						t.Fatal(err)
					}
				}
			}}
			svc, err := mappingapp.NewService(mappingapp.Options{Mappings: mappings, Products: products, Media: adminNoopLocalMediaRecorder{}, Transactions: uow})
			if err != nil {
				t.Fatal(err)
			}
			err = svc.Delete(mapping.ID)
			var afterProduct productdomain.Product
			var afterMapping mappingdomain.Mapping
			var afterSKUMapping mappingdomain.SKUMapping
			var afterSKU productdomain.ProductSKU
			for _, row := range []interface{}{&afterProduct, &afterMapping, &afterSKUMapping, &afterSKU} {
				if err := db.Unscoped().First(row, 1).Error; err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "mapping_deleted" || scenario == "mapping_reassigned" || len(scenario) > 5 && scenario[:5] == "fail_" {
				want := injected
				if scenario == "mapping_deleted" || scenario == "mapping_reassigned" {
					want = mappingcontract.ErrMappingNotFound
				}
				if !errors.Is(err, want) {
					t.Fatalf("expected %v, got %v", want, err)
				}
				if !reflect.DeepEqual(beforeProduct, afterProduct) || !reflect.DeepEqual(beforeMapping, afterMapping) || !reflect.DeepEqual(beforeSKUMapping, afterSKUMapping) {
					t.Fatal("failed unmap left partial changes")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if afterMapping.DeletedAt == nil || afterSKUMapping.DeletedAt == nil {
					t.Fatal("mapping rows not deleted together")
				}
				if scenario == "upstream" {
					if afterProduct.IsActive || afterProduct.IsMapped || afterProduct.FulfillmentType != constants.FulfillmentTypeManual {
						t.Fatal("upstream product not safely detached")
					}
				} else if scenario == "manual" {
					if !afterProduct.IsActive || afterProduct.IsMapped || afterProduct.FulfillmentType != constants.FulfillmentTypeManual {
						t.Fatal("manual product state changed unexpectedly")
					}
				} else if !reflect.DeepEqual(beforeProduct, afterProduct) {
					t.Fatal("detachment modified a converted or deleted product")
				}
			}
			if !reflect.DeepEqual(beforeSKU, afterSKU) {
				t.Fatal("detachment modified SKU stock or prices")
			}
		})
	}
}

type lateMappingAdapter struct {
	upstream.Adapter
	product upstream.UpstreamProduct
	deleted bool
	check   func()
}

func (a lateMappingAdapter) GetProduct(context.Context, uint) (*upstream.UpstreamProduct, error) {
	a.check()
	if a.deleted {
		return nil, upstream.ErrUpstreamProductDeleted
	}
	return &a.product, nil
}
func (a lateMappingAdapter) ListProducts(context.Context, upstream.ListProductsOpts) (*upstream.ProductListResult, error) {
	a.check()
	if a.deleted {
		return &upstream.ProductListResult{IncludesInactive: true}, nil
	}
	return &upstream.ProductListResult{Total: 1, Items: []upstream.UpstreamProduct{a.product}, IncludesInactive: true}, nil
}
func (a lateMappingAdapter) ListCategories(context.Context) (*upstream.CategoryListResult, error) {
	a.check()
	return &upstream.CategoryListResult{Supported: true}, nil
}

type mappingWriteSnapshot struct {
	Product     productdomain.Product
	Mapping     mappingdomain.Mapping
	SKUs        []productdomain.ProductSKU
	SKUMappings []mappingdomain.SKUMapping
}

func readMappingWriteSnapshot(t *testing.T, db *gorm.DB) mappingWriteSnapshot {
	t.Helper()
	var snapshot mappingWriteSnapshot
	for _, row := range []interface{}{&snapshot.Product, &snapshot.Mapping} {
		if err := db.First(row, 1).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Order("id").Find(&snapshot.SKUs).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Order("id").Find(&snapshot.SKUMappings).Error; err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestLateMappingWritesCannotChangeDetachedProducts(t *testing.T) {
	for _, mode := range []string{"single", "single_inactive", "single_deleted", "batch", "batch_inactive", "batch_deleted", "markup_local", "markup_remote"} {
		for _, change := range []string{"current", "detached_direct", "direct_type", "unmapped", "mapping_rebound", "product_deleted"} {
			t.Run(mode+"/"+change, func(t *testing.T) {
				db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:late_mapping_%d?mode=memory&cache=shared", time.Now().UnixNano())), &gorm.Config{DisableForeignKeyConstraintWhenMigrating: true})
				if err != nil {
					t.Fatal(err)
				}
				pool, err := db.DB()
				if err != nil {
					t.Fatal(err)
				}
				pool.SetMaxOpenConns(1)
				t.Cleanup(func() { _ = pool.Close() })
				if err := db.AutoMigrate(&productdomain.Product{}, &productdomain.ProductSKU{}, &mappingdomain.Mapping{}, &mappingdomain.SKUMapping{}, &siteconnectiondomain.Connection{}); err != nil {
					t.Fatal(err)
				}
				product := productdomain.Product{Slug: "late", TitleJSON: jsonmap.JSON{"zh-CN": "late"}, FulfillmentType: constants.FulfillmentTypeUpstream, IsMapped: true, IsActive: true}
				if err := db.Create(&product).Error; err != nil {
					t.Fatal(err)
				}
				sku := productdomain.ProductSKU{ProductID: product.ID, SKUCode: "OLD", IsActive: true}
				if err := db.Create(&sku).Error; err != nil {
					t.Fatal(err)
				}
				conn := siteconnectiondomain.Connection{ID: 1, Name: "late", ExchangeRate: decimal.NewFromInt(1), PriceMarkupPercent: decimal.NewFromInt(50), AutoSyncPrice: true}
				if err := db.Create(&conn).Error; err != nil {
					t.Fatal(err)
				}
				mapping := mappingdomain.Mapping{LocalProductID: product.ID, ConnectionID: conn.ID, UpstreamProductID: 20}
				if err := db.Create(&mapping).Error; err != nil {
					t.Fatal(err)
				}
				sm := mappingdomain.SKUMapping{ProductMappingID: mapping.ID, LocalSKUID: sku.ID, UpstreamSKUID: 201}
				if err := db.Create(&sm).Error; err != nil {
					t.Fatal(err)
				}
				if err := db.Model(&sm).Update("upstream_price", "20").Error; err != nil {
					t.Fatal(err)
				}
				products := productgormstore.NewProductStore(db)
				mappings := mappinggormstore.NewMappingStore(db)
				skus := productgormstore.NewSKUStore(db)
				unit := &mappingImportUoW{products: products, skus: skus, mappings: mappings, skuMappings: mappinggormstore.NewSKUMappingStore(db)}
				calls := 0
				adapter := lateMappingAdapter{deleted: strings.HasSuffix(mode, "deleted"), product: upstream.UpstreamProduct{ID: 20, IsActive: !strings.HasSuffix(mode, "inactive"), ManualFormSchema: jsonmap.JSON{"changed": true}, SKUs: []upstream.UpstreamSKU{{ID: 201, SKUCode: "OLD", PriceAmount: "20", IsActive: true}, {ID: 202, SKUCode: "NEW", PriceAmount: "30", IsActive: true}}}, check: func() {
					// SQLite has one connection: this query would hang if I/O moved inside the transaction.
					var n int64
					if err := db.Model(&productdomain.Product{}).Count(&n).Error; err != nil {
						t.Fatal(err)
					}
					calls++
				}}
				var before mappingWriteSnapshot
				entered := false
				hook := unmapTransactionHook{UnitOfWork: unit, before: func() {
					entered = true
					var err error
					switch change {
					case "detached_direct":
						deleter, e := mappingapp.NewService(mappingapp.Options{Mappings: mappings, Products: products, Transactions: unit, Media: adminNoopLocalMediaRecorder{}})
						if e != nil {
							t.Fatal(e)
						}
						if e := deleter.Delete(mapping.ID); e != nil {
							t.Fatal(e)
						}
						err = db.Model(&product).Updates(map[string]interface{}{"fulfillment_type": constants.FulfillmentTypeKafudaGPT, "is_mapped": false, "is_active": true}).Error
					case "direct_type":
						err = db.Model(&product).Update("fulfillment_type", constants.FulfillmentTypeKafudaGPT).Error
					case "unmapped":
						err = db.Model(&product).Update("is_mapped", false).Error
					case "mapping_rebound":
						err = db.Model(&mapping).Update("upstream_product_id", 99).Error
					case "product_deleted":
						err = db.Model(&product).Update("deleted_at", time.Now()).Error
					}
					if err != nil {
						t.Fatal(err)
					}
					before = readMappingWriteSnapshot(t, db)
				}}
				svc, err := mappingapp.NewService(mappingapp.Options{Mappings: mappings, SKUMappings: unit.skuMappings, Products: products, SKUs: skus, Media: adminNoopLocalMediaRecorder{}, Connections: &mappingConnectionProvider{db: db, adapter: adapter}, Transactions: hook})
				if err != nil {
					t.Fatal(err)
				}
				var updated int
				switch {
				case strings.HasPrefix(mode, "single"):
					err = svc.SyncProduct(mapping.ID)
				case strings.HasPrefix(mode, "batch"):
					err = svc.SyncConnectionStock(conn.ID, []mappingdomain.Mapping{mapping}, 100, 2)
				case mode == "markup_local":
					updated, err = svc.ReapplyMarkupLocal(conn.ID)
				default:
					updated, err = svc.ReapplyMarkup(conn.ID)
				}
				if !entered {
					t.Fatal("local transaction was not reached")
				}
				if strings.HasPrefix(mode, "single") && change != "current" {
					if !errors.Is(err, mappingcontract.ErrMappingNotFound) {
						t.Fatalf("expected stale mapping rejection, got %v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
				if !strings.HasPrefix(mode, "markup") && calls == 0 {
					t.Fatal("upstream read not exercised")
				}
				after := readMappingWriteSnapshot(t, db)
				if change != "current" {
					if !reflect.DeepEqual(before, after) {
						t.Fatal("late task changed detached product, SKU or mapping records")
					}
					if updated != 0 {
						t.Fatal("late markup was counted as applied")
					}
				} else if strings.HasPrefix(mode, "markup") {
					if updated != 1 || after.SKUs[0].PriceAmount.String() != "30.00" {
						t.Fatal("current mapped product was not repriced")
					}
				} else if strings.HasSuffix(mode, "inactive") || strings.HasSuffix(mode, "deleted") {
					if after.Product.IsActive || after.SKUs[0].IsActive {
						t.Fatal("current unavailable product not disabled")
					}
				} else if len(after.SKUs) != 2 || after.SKUs[0].PriceAmount.String() != "30.00" || after.Mapping.LastSyncedAt == nil {
					t.Fatal("current mapped product was not synchronized")
				}
			})
		}
	}
}

func TestMappedProductSyncRollsBackFailedWrites(t *testing.T) {
	for _, scenario := range []struct {
		name       string
		trigger    string
		status     string
		removedSKU bool
	}{
		{name: "insert_sku_mapping", trigger: "BEFORE INSERT ON sku_mappings WHEN NEW.upstream_sku_id = 202"},
		{name: "update_sku_mapping", trigger: "BEFORE UPDATE ON sku_mappings"},
		{name: "update_sku", trigger: "BEFORE UPDATE ON product_skus"},
		{name: "removed_sku", trigger: "BEFORE UPDATE ON product_skus", removedSKU: true},
		{name: "product_price", trigger: "BEFORE UPDATE ON products WHEN OLD.price_amount <> NEW.price_amount"},
		{name: "product_mapping", trigger: "BEFORE UPDATE ON product_mappings"},
		{name: "inactive_product", trigger: "BEFORE UPDATE ON products", status: mappingdomain.UpstreamStatusInactive},
		{name: "inactive_sku_mapping", trigger: "BEFORE UPDATE ON sku_mappings", status: mappingdomain.UpstreamStatusInactive},
		{name: "inactive_sku", trigger: "BEFORE UPDATE ON product_skus", status: mappingdomain.UpstreamStatusInactive},
		{name: "deleted_mapping", trigger: "BEFORE UPDATE ON product_mappings", status: mappingdomain.UpstreamStatusDeleted},
	} {
		for _, mode := range []string{"single", "batch"} {
			t.Run(mode+"/"+scenario.name, func(t *testing.T) {
				db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:atomic_mapping_sku_%d?mode=memory&cache=shared", time.Now().UnixNano())), &gorm.Config{DisableForeignKeyConstraintWhenMigrating: true})
				if err != nil {
					t.Fatal(err)
				}
				pool, err := db.DB()
				if err != nil {
					t.Fatal(err)
				}
				pool.SetMaxOpenConns(1)
				t.Cleanup(func() { _ = pool.Close() })
				if err := db.AutoMigrate(&productdomain.Product{}, &productdomain.ProductSKU{}, &mappingdomain.Mapping{}, &mappingdomain.SKUMapping{}, &siteconnectiondomain.Connection{}); err != nil {
					t.Fatal(err)
				}
				product := productdomain.Product{
					Slug: "atomic-sku", TitleJSON: jsonmap.JSON{"zh-CN": "映射商品"},
					FulfillmentType: constants.FulfillmentTypeUpstream, IsMapped: true, IsActive: true,
				}
				conn := siteconnectiondomain.Connection{Name: "atomic-sku", ExchangeRate: decimal.NewFromInt(1), AutoSyncPrice: true}
				for _, row := range []interface{}{&product, &conn} {
					if err := db.Create(row).Error; err != nil {
						t.Fatal(err)
					}
				}
				sku := productdomain.ProductSKU{ProductID: product.ID, SKUCode: "OLD", IsActive: true}
				mapping := mappingdomain.Mapping{LocalProductID: product.ID, ConnectionID: conn.ID, UpstreamProductID: 20}
				for _, row := range []interface{}{&sku, &mapping} {
					if err := db.Create(row).Error; err != nil {
						t.Fatal(err)
					}
				}
				skuMapping := mappingdomain.SKUMapping{ProductMappingID: mapping.ID, LocalSKUID: sku.ID, UpstreamSKUID: 201, UpstreamStock: 2}
				if err := db.Create(&skuMapping).Error; err != nil {
					t.Fatal(err)
				}

				products := productgormstore.NewProductStore(db)
				mappings := mappinggormstore.NewMappingStore(db)
				skus := productgormstore.NewSKUStore(db)
				skuMappings := mappinggormstore.NewSKUMappingStore(db)
				unit := &mappingImportUoW{products: products, skus: skus, mappings: mappings, skuMappings: skuMappings}
				upProduct := upstream.UpstreamProduct{
					ID: 20, CategoryID: 24, IsActive: scenario.status == "", ManualFormSchema: jsonmap.JSON{"updated": true},
					SKUs: []upstream.UpstreamSKU{
						{ID: 201, SKUCode: "OLD", PriceAmount: "20", StockQuantity: 8, IsActive: true},
						{ID: 202, SKUCode: "NEW", PriceAmount: "30", StockQuantity: 9, IsActive: true},
					},
				}
				if scenario.removedSKU {
					upProduct.SKUs = upProduct.SKUs[1:]
				}
				adapter := lateMappingAdapter{product: upProduct, deleted: scenario.status == mappingdomain.UpstreamStatusDeleted, check: func() {}}
				svc, err := mappingapp.NewService(mappingapp.Options{
					Mappings: mappings, SKUMappings: skuMappings, Products: products, SKUs: skus,
					Media: adminNoopLocalMediaRecorder{}, Connections: &mappingConnectionProvider{db: db, adapter: adapter}, Transactions: unit,
				})
				if err != nil {
					t.Fatal(err)
				}
				syncOnce := func() error {
					if mode == "single" {
						return svc.SyncProduct(mapping.ID)
					}
					return svc.SyncConnectionStock(conn.ID, []mappingdomain.Mapping{mapping}, 100, 2)
				}

				const injectedMessage = "injected product sync write failure"
				trigger := fmt.Sprintf("CREATE TRIGGER reject_sync_write %s BEGIN SELECT RAISE(ABORT, '%s'); END", scenario.trigger, injectedMessage)
				if err := db.Exec(trigger).Error; err != nil {
					t.Fatal(err)
				}
				before := readMappingWriteSnapshot(t, db)
				if err := syncOnce(); err == nil || !strings.Contains(err.Error(), injectedMessage) {
					t.Errorf("expected injected write failure, got %v", err)
				}
				if after := readMappingWriteSnapshot(t, db); !reflect.DeepEqual(before, after) {
					t.Fatal("failed synchronization left partial SKU, stock, product or mapping updates")
				}
				if err := db.Exec("DROP TRIGGER reject_sync_write").Error; err != nil {
					t.Fatal(err)
				}

				var newSKUID uint
				for range 2 {
					if err := syncOnce(); err != nil {
						t.Fatal(err)
					}
					after := readMappingWriteSnapshot(t, db)
					if scenario.status != "" {
						if len(after.SKUs) != 1 || len(after.SKUMappings) != 1 || after.Product.IsActive || after.SKUs[0].IsActive ||
							after.SKUMappings[0].UpstreamIsActive || after.SKUMappings[0].UpstreamStock != 0 ||
							after.Mapping.UpstreamStatus != scenario.status || after.Mapping.LastSyncedAt == nil ||
							after.Mapping.IsActive != (scenario.status != mappingdomain.UpstreamStatusDeleted) {
							t.Fatalf("retry did not disable product, SKU and mapped stock: %+v", after)
						}
						continue
					}
					wantStock, wantPrice := 8, "20.00"
					if scenario.removedSKU {
						wantStock, wantPrice = 0, "30.00"
					}
					if len(after.SKUs) != 2 || len(after.SKUMappings) != 2 || after.SKUs[0].ID != sku.ID ||
						after.SKUs[1].SKUCode != "NEW" || after.SKUMappings[1].UpstreamSKUID != 202 || after.SKUMappings[1].LocalSKUID != after.SKUs[1].ID ||
						after.SKUMappings[0].UpstreamStock != wantStock || after.SKUMappings[1].UpstreamStock != 9 || after.Product.PriceAmount.String() != wantPrice ||
						after.SKUs[0].IsActive == scenario.removedSKU || after.SKUMappings[0].UpstreamIsActive == scenario.removedSKU ||
						after.Mapping.UpstreamCategoryID != 24 || after.Mapping.LastSyncedAt == nil {
						t.Fatalf("successful sync did not save paired SKU, stock, and metadata: %+v", after)
					}
					if newSKUID != 0 && newSKUID != after.SKUs[1].ID {
						t.Fatal("repeat sync replaced the SKU")
					}
					newSKUID = after.SKUs[1].ID
				}
			})
		}
	}
}

func TestMappedProductMarkupRollsBackFailedWrites(t *testing.T) {
	for _, mode := range []struct {
		name    string
		reapply func(*mappingapp.Service, uint) (int, error)
	}{
		{name: "local", reapply: (*mappingapp.Service).ReapplyMarkupLocal},
		{name: "admin", reapply: (*mappingapp.Service).ReapplyMarkup},
	} {
		for _, failure := range []struct {
			name    string
			trigger string
		}{
			{name: "second_sku", trigger: "BEFORE UPDATE OF price_amount ON product_skus WHEN (SELECT COUNT(*) FROM product_skus WHERE product_id = NEW.product_id AND price_amount > 0) = 1"},
			{name: "product_summary", trigger: "BEFORE UPDATE OF price_amount ON products"},
		} {
			t.Run(mode.name+"/"+failure.name, func(t *testing.T) {
				db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{DisableForeignKeyConstraintWhenMigrating: true})
				if err != nil {
					t.Fatal(err)
				}
				pool, err := db.DB()
				if err != nil {
					t.Fatal(err)
				}
				pool.SetMaxOpenConns(1)
				t.Cleanup(func() { _ = pool.Close() })
				if err := db.AutoMigrate(&productdomain.Product{}, &productdomain.ProductSKU{}, &mappingdomain.Mapping{}, &mappingdomain.SKUMapping{}, &siteconnectiondomain.Connection{}); err != nil {
					t.Fatal(err)
				}
				create := func(row any) {
					t.Helper()
					if err := db.Create(row).Error; err != nil {
						t.Fatal(err)
					}
				}
				conn := siteconnectiondomain.Connection{Name: "markup-rollback", ExchangeRate: decimal.NewFromInt(1), PriceMarkupPercent: decimal.NewFromInt(50)}
				product := productdomain.Product{
					Slug: "markup-rollback", TitleJSON: jsonmap.JSON{"zh-CN": "映射商品"},
					FulfillmentType: constants.FulfillmentTypeUpstream, IsMapped: true, IsActive: true,
				}
				create(&conn)
				create(&product)
				mapping := mappingdomain.Mapping{LocalProductID: product.ID, ConnectionID: conn.ID, UpstreamProductID: 20}
				create(&mapping)
				for i, price := range []int64{20, 30} {
					sku := productdomain.ProductSKU{ProductID: product.ID, SKUCode: fmt.Sprintf("SKU-%d", i+1), IsActive: true}
					create(&sku)
					create(&mappingdomain.SKUMapping{ProductMappingID: mapping.ID, LocalSKUID: sku.ID, UpstreamSKUID: uint(201 + i),
						UpstreamPrice: money.FromDecimal(decimal.NewFromInt(price)), UpstreamStock: 10 + i})
				}
				unit := &mappingImportUoW{
					products: productgormstore.NewProductStore(db), skus: productgormstore.NewSKUStore(db),
					mappings: mappinggormstore.NewMappingStore(db), skuMappings: mappinggormstore.NewSKUMappingStore(db),
				}
				svc, err := mappingapp.NewService(mappingapp.Options{
					Mappings: unit.mappings, SKUMappings: unit.skuMappings, Products: unit.products, SKUs: unit.skus,
					Media: adminNoopLocalMediaRecorder{}, Connections: &mappingConnectionProvider{db: db}, Transactions: unit,
				})
				if err != nil {
					t.Fatal(err)
				}

				const injected = "injected markup write failure"
				trigger := fmt.Sprintf("CREATE TRIGGER reject_markup_write %s BEGIN SELECT RAISE(ABORT, '%s'); END", failure.trigger, injected)
				if err := db.Exec(trigger).Error; err != nil {
					t.Fatal(err)
				}
				before := readMappingWriteSnapshot(t, db)
				if updated, err := mode.reapply(svc, conn.ID); updated != 0 || err == nil || !strings.Contains(err.Error(), injected) {
					t.Errorf("expected write failure without counting the mapped product: updated=%d, err=%v", updated, err)
				}
				if after := readMappingWriteSnapshot(t, db); !reflect.DeepEqual(before, after) {
					t.Fatal("failed markup left partial product or SKU changes")
				}

				if err := db.Exec("DROP TRIGGER reject_markup_write").Error; err != nil {
					t.Fatal(err)
				}
				if updated, err := mode.reapply(svc, conn.ID); updated != 1 || err != nil {
					t.Fatalf("retry did not count one repriced product: updated=%d, err=%v", updated, err)
				}
				after := readMappingWriteSnapshot(t, db)
				if after.Product.PriceAmount.String() != "30.00" || after.Product.CostPriceAmount.String() != "20.00" ||
					len(after.SKUs) != 2 || after.SKUs[0].PriceAmount.String() != "30.00" || after.SKUs[0].CostPriceAmount.String() != "20.00" ||
					after.SKUs[1].PriceAmount.String() != "45.00" || after.SKUs[1].CostPriceAmount.String() != "30.00" {
					t.Fatal("retry did not update both SKU prices and the product summary")
				}
				if !reflect.DeepEqual(before.Mapping, after.Mapping) || !reflect.DeepEqual(before.SKUMappings, after.SKUMappings) {
					t.Fatal("markup changed mapping or upstream stock metadata")
				}
			})
		}
	}
}

type mappingStatusReadHook struct {
	mappingcontract.MappingRepository
	afterRead func()
}

func (r mappingStatusReadHook) GetByID(id uint) (*mappingdomain.Mapping, error) {
	snapshot, err := r.MappingRepository.GetByID(id)
	if err == nil && snapshot != nil {
		r.afterRead()
	}
	return snapshot, err
}

func TestSetMappingActiveUsesCurrentMappingAndProduct(t *testing.T) {
	for _, scenario := range []struct {
		name       string
		wantActive bool
		stale      bool
	}{
		{name: "enable_inactive", wantActive: true},
		{name: "disable_legacy_manual"},
		{name: "metadata_changed"},
		{name: "detached", stale: true},
		{name: "detached_then_direct", stale: true},
		{name: "direct_type", stale: true},
		{name: "rebound", stale: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:mapping_status_%d?mode=memory&cache=shared", time.Now().UnixNano())), &gorm.Config{DisableForeignKeyConstraintWhenMigrating: true})
			if err != nil {
				t.Fatal(err)
			}
			pool, err := db.DB()
			if err != nil {
				t.Fatal(err)
			}
			pool.SetMaxOpenConns(1)
			t.Cleanup(func() { _ = pool.Close() })
			if err := db.AutoMigrate(&productdomain.Product{}, &productdomain.ProductSKU{}, &mappingdomain.Mapping{}, &mappingdomain.SKUMapping{}, &siteconnectiondomain.Connection{}); err != nil {
				t.Fatal(err)
			}
			product := productdomain.Product{
				Slug: "mapping-status", TitleJSON: jsonmap.JSON{"zh-CN": "映射商品"},
				FulfillmentType: constants.FulfillmentTypeUpstream, IsMapped: true,
			}
			if scenario.name == "disable_legacy_manual" {
				product.FulfillmentType = constants.FulfillmentTypeManual
			}
			if err := db.Create(&product).Error; err != nil {
				t.Fatal(err)
			}
			sku := productdomain.ProductSKU{ProductID: product.ID, SKUCode: "mapping-status"}
			if err := db.Create(&sku).Error; err != nil {
				t.Fatal(err)
			}
			mapping := mappingdomain.Mapping{LocalProductID: product.ID, ConnectionID: 1, UpstreamProductID: 20}
			if err := db.Create(&mapping).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Model(&mapping).Update("is_active", !scenario.wantActive).Error; err != nil {
				t.Fatal(err)
			}
			skuMapping := mappingdomain.SKUMapping{ProductMappingID: mapping.ID, LocalSKUID: sku.ID, UpstreamSKUID: 201}
			if err := db.Create(&skuMapping).Error; err != nil {
				t.Fatal(err)
			}

			products := productgormstore.NewProductStore(db)
			mappings := mappinggormstore.NewMappingStore(db)
			unit := &mappingImportUoW{products: products, skus: productgormstore.NewSKUStore(db), mappings: mappings, skuMappings: mappinggormstore.NewSKUMappingStore(db)}
			options := mappingapp.Options{Mappings: mappings, Products: products, Transactions: unit, Media: adminNoopLocalMediaRecorder{}}
			deleter, err := mappingapp.NewService(options)
			if err != nil {
				t.Fatal(err)
			}
			var before mappingWriteSnapshot
			options.Mappings = mappingStatusReadHook{MappingRepository: mappings, afterRead: func() {
				switch scenario.name {
				case "detached", "detached_then_direct":
					if err := deleter.Delete(mapping.ID); err != nil {
						t.Fatal(err)
					}
				}
				var err error
				switch scenario.name {
				case "detached_then_direct", "direct_type":
					err = db.Model(&product).Update("fulfillment_type", constants.FulfillmentTypeKafudaGPT).Error
				case "rebound":
					err = db.Model(&mapping).Update("upstream_product_id", 99).Error
				case "metadata_changed":
					err = db.Model(&mapping).Updates(map[string]interface{}{
						"upstream_category_id": 12,
						"upstream_status":      mappingdomain.UpstreamStatusInactive,
						"last_synced_at":       time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC),
					}).Error
				}
				if err != nil {
					t.Fatal(err)
				}
				before = readMappingWriteSnapshot(t, db)
			}}
			service, err := mappingapp.NewService(options)
			if err != nil {
				t.Fatal(err)
			}
			err = service.SetActive(mapping.ID, scenario.wantActive)
			after := readMappingWriteSnapshot(t, db)
			if scenario.stale {
				if !errors.Is(err, mappingcontract.ErrMappingNotFound) {
					t.Errorf("expected stale mapping to be rejected, got %v", err)
				}
			} else {
				if err != nil || after.Mapping.IsActive != scenario.wantActive {
					t.Fatalf("expected active=%v, got %v with error %v", scenario.wantActive, after.Mapping.IsActive, err)
				}
				after.Mapping.IsActive = before.Mapping.IsActive
				after.Mapping.UpdatedAt = before.Mapping.UpdatedAt
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatal("status update changed mapping metadata, product or SKU rows")
			}
		})
	}
}
