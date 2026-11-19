package integrationtest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	cardsecretdomain "github.com/dujiao-next/internal/modules/cardsecret/domain"
	cardsecretgormstore "github.com/dujiao-next/internal/modules/cardsecret/infrastructure/gormstore"
	productwrite "github.com/dujiao-next/internal/modules/catalog/product/application/write"
	productcontract "github.com/dujiao-next/internal/modules/catalog/product/contract"
	productdomain "github.com/dujiao-next/internal/modules/catalog/product/domain"
	productgormstore "github.com/dujiao-next/internal/modules/catalog/product/store/gormstore"
	producthttp "github.com/dujiao-next/internal/modules/catalog/product/transport/http"
	kafudaapp "github.com/dujiao-next/internal/modules/kafudagpt/application"
	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
	kafudastore "github.com/dujiao-next/internal/modules/kafudagpt/infrastructure/gormstore"
	ordercontract "github.com/dujiao-next/internal/modules/order/contract"
	orderdomain "github.com/dujiao-next/internal/modules/order/domain"
	ordergormstore "github.com/dujiao-next/internal/modules/order/infrastructure/gormstore"

	categorydomain "github.com/dujiao-next/internal/modules/catalog/category/domain"
	categorygormstore "github.com/dujiao-next/internal/modules/catalog/category/infrastructure/gormstore"

	"github.com/dujiao-next/internal/constants"
	"github.com/dujiao-next/internal/platform/http/response"
	"github.com/dujiao-next/internal/shared/jsonmap"
	"github.com/dujiao-next/internal/shared/money"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
)

type productUpdateTransactionHook struct {
	ordercontract.Store
	before func()
}

func (s productUpdateTransactionHook) WithinTransaction(fn func(ordercontract.Transaction) error) error {
	s.before()
	return s.Store.WithinTransaction(fn)
}

func TestDirectProductEditPreservesConcurrentSKUConfiguration(t *testing.T) {
	for _, scenario := range []struct {
		name         string
		explicitSale bool
		costOnly     bool
		wantSale     int64
		wantSort     int
	}{
		{name: "omitted_sku_list", wantSale: 130, wantSort: 34},
		{name: "explicit_sale_omitted_cost", explicitSale: true, wantSale: 120, wantSort: 26},
		{name: "http_cost_only_preserves_latest_sale_and_sort", costOnly: true, wantSale: 130, wantSort: 34},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			svc, db := newProductServiceForTest(t)
			pool, err := db.DB()
			if err != nil {
				t.Fatal(err)
			}
			pool.SetMaxOpenConns(1)
			t.Cleanup(func() { _ = pool.Close() })

			targetCode := kafudacontract.SKUCode("plus", "PH")
			fallbackCode := kafudacontract.SKUCode("plus", "JP")
			revision := uint64(2)
			catalog := &directProductTestCatalog{options: []kafudacontract.SKUOption{
				{SKUCode: targetCode, Plan: "plus", Country: "PH", Available: true, CatalogRevision: revision},
				{SKUCode: fallbackCode, Plan: "plus", Country: "JP", Available: true, CatalogRevision: revision},
			}}
			svc.Write.SetDirectCatalog(catalog)
			active, inactive := true, false
			input := productwrite.CreateProductInput{
				Slug: "direct-concurrent-edit", TitleJSON: jsonmap.JSON{"zh-CN": "原商品"},
				FulfillmentType: constants.FulfillmentTypeKafudaGPT,
				SKUs: []productwrite.ProductSKUInput{
					{SKUCode: targetCode, PriceAmount: decimal.NewFromInt(100), IsActive: &inactive, DirectCatalogRevision: &revision, SortOrder: 23},
					{SKUCode: fallbackCode, PriceAmount: decimal.NewFromInt(180), CostPriceAmount: decimal.NewFromInt(90), CostPriceProvided: true,
						IsActive: &active, DirectCatalogRevision: &revision, SortOrder: 24},
				},
			}
			if scenario.costOnly {
				category := categorydomain.Category{Slug: "direct-edit", NameJSON: jsonmap.JSON{"zh-CN": "直充"}, IsActive: true}
				if err := db.Create(&category).Error; err != nil {
					t.Fatalf("create edit category: %v", err)
				}
				input.CategoryID = category.ID
			}
			product, err := svc.Write.Create(input)
			if err != nil || product == nil {
				t.Fatalf("create direct product: %+v, %v", product, err)
			}
			id := fmt.Sprint(product.ID)
			product, err = svc.Read.GetAdminByID(id)
			if err != nil || product == nil || len(product.SKUs) != 2 {
				t.Fatalf("load original direct SKUs: %+v, %v", product, err)
			}
			skuIDs := make(map[string]uint, len(product.SKUs))
			for _, sku := range product.SKUs {
				skuIDs[sku.SKUCode] = sku.ID
			}

			newCost := decimal.NewFromInt(55)
			newer := input
			newer.IsActive = &inactive
			newer.SKUs = []productwrite.ProductSKUInput{
				{ID: skuIDs[targetCode], SKUCode: targetCode, PriceAmount: decimal.NewFromInt(130), CostPriceAmount: newCost, CostPriceProvided: true,
					IsActive: &active, DirectCatalogRevision: &revision, SortOrder: 34},
				{ID: skuIDs[fallbackCode], SKUCode: fallbackCode, PriceAmount: decimal.NewFromInt(180), CostPriceAmount: decimal.NewFromInt(90),
					CostPriceProvided: true, IsActive: &inactive, DirectCatalogRevision: &revision, SortOrder: 24},
			}
			orders := productUpdateTransactionHook{Store: ordergormstore.New(db, "test-guest-credential-secret-with-32-bytes"), before: func() {
				if _, err := svc.Write.Update(id, newer); err != nil {
					t.Fatalf("save the newer SKU configuration after the first edit's preflight: %v", err)
				}
			}}
			editor := productwrite.NewWriteService(productwrite.Options{
				Products: productgormstore.NewProductStore(db), SKUs: productgormstore.NewSKUStore(db),
				Categories: categorygormstore.NewCategoryStore(db), CardSecrets: cardsecretgormstore.New(db), Orders: orders,
			})
			editor.SetDirectCatalog(catalog)
			input.TitleJSON = jsonmap.JSON{"zh-CN": "已编辑"}
			input.SKUs = nil
			if scenario.explicitSale {
				input.SKUs = []productwrite.ProductSKUInput{{
					ID: skuIDs[targetCode], SKUCode: targetCode, PriceAmount: decimal.NewFromInt(scenario.wantSale),
					DirectCatalogRevision: &revision, SortOrder: scenario.wantSort,
				}}
			}
			wantCost := newCost
			if scenario.costOnly {
				wantCost = decimal.NewFromInt(61)
				handler := producthttp.NewAdminProductHandler(svc.Read, editor, svc.Admin, nil, nil, nil)
				router := gin.New()
				router.PUT("/api/v1/admin/products/:id", handler.UpdateProduct)
				body := fmt.Sprintf(`{"category_id":%d,"slug":%q,"title":{"zh-CN":"已编辑"},"fulfillment_type":%q,"skus":[{"id":%d,"sku_code":%q,"direct_catalog_revision":%d,"cost_price_amount":61}]}`,
					input.CategoryID, input.Slug, input.FulfillmentType, skuIDs[targetCode], targetCode, revision)
				req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/products/"+id, strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				res := httptest.NewRecorder()
				router.ServeHTTP(res, req)
				var result response.Response
				if err := json.Unmarshal(res.Body.Bytes(), &result); err != nil || res.Code != http.StatusOK || result.StatusCode != response.CodeOK {
					t.Fatalf("save a cost-only edit: HTTP %d, API code %d, %v", res.Code, result.StatusCode, err)
				}
			} else if _, err := editor.Update(id, input); err != nil {
				t.Fatalf("save the first edit: %v", err)
			}
			product, err = svc.Read.GetAdminByID(id)
			if err != nil || product == nil || len(product.SKUs) != 2 {
				t.Fatalf("load saved direct product: %+v, %v", product, err)
			}
			wantSale := decimal.NewFromInt(scenario.wantSale)
			if !product.PriceAmount.Equal(wantSale) || !product.CostPriceAmount.Equal(wantCost) || product.IsActive ||
				product.TitleJSON["zh-CN"] != "已编辑" {
				t.Errorf("edit lost the new aggregate SKU prices, disabled state, or requested title: %+v", product)
			}
			for _, sku := range product.SKUs {
				if sku.ID != skuIDs[sku.SKUCode] || sku.DirectCatalogRevision != revision || !sku.DirectCostConfigured {
					t.Errorf("edit lost SKU identity, catalog review, or confirmed cost: %+v", sku)
				}
				switch sku.SKUCode {
				case targetCode:
					if !sku.IsActive || !sku.PriceAmount.Equal(wantSale) || !sku.CostPriceAmount.Equal(wantCost) || sku.SortOrder != scenario.wantSort {
						t.Errorf("edit replaced the target SKU configuration: %+v", sku)
					}
				case fallbackCode:
					if sku.IsActive || !sku.PriceAmount.Equal(decimal.NewFromInt(180)) || !sku.CostPriceAmount.Equal(decimal.NewFromInt(90)) || sku.SortOrder != 24 {
						t.Errorf("edit replaced the fallback SKU configuration: %+v", sku)
					}
				}
			}
		})
	}
}

func TestDirectProductEditUsesCurrentCatalogAfterPreflight(t *testing.T) {
	for _, scenario := range []struct {
		name             string
		explicitApproval bool
	}{
		{name: "metadata_after_pair_reappears"},
		{name: "stale_explicit_approval", explicitApproval: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			svc, db := newProductServiceForTest(t)
			pool, err := db.DB()
			if err != nil {
				t.Fatal(err)
			}
			pool.SetMaxOpenConns(1)
			t.Cleanup(func() { _ = pool.Close() })
			if err := db.AutoMigrate(&kafudadomain.Catalog{}); err != nil {
				t.Fatal(err)
			}
			const profile = "regional-checkout"
			const plan, country = "plus", "PH"
			const card = "regional-card"
			ctx, syncedAt := t.Context(), time.Now().UTC().Add(-time.Minute)
			store := kafudastore.New(db)
			pair := kafudadomain.CatalogOption{Plan: plan, Country: country, Product: "gpt", Label: "Plus", Available: true}
			saveCatalog := func(at time.Time, options []kafudadomain.CatalogOption) {
				t.Helper()
				if err := store.SaveCatalog(ctx, &kafudadomain.Catalog{
					Profile: profile, Enabled: true, SyncedAt: at, ExpiresAt: at.Add(time.Hour),
					Options: options, Cards: []kafudadomain.CardProduct{{ProductCode: card}},
				}); err != nil {
					t.Fatalf("save catalog at %s: %v", at, err)
				}
			}
			saveCatalog(syncedAt, []kafudadomain.CatalogOption{pair})
			policy := kafudaapp.NewCommerceService(kafudaapp.CommerceOptions{
				Catalog:      kafudaapp.NewCatalogService(store, nil, profile, time.Hour),
				SalesEnabled: true, ReferenceLookupVerified: true, ProductCode: card,
				MaxCostCents: 2500, AllowedKinds: []string{"new"}, WorkerReady: func(context.Context) bool { return true },
			})
			svc.Write.SetDirectCatalog(policy)
			code := kafudacontract.SKUCode(plan, country)
			active, initialRevision := true, uint64(0)
			input := productwrite.CreateProductInput{
				Slug: "direct-regional-catalog", TitleJSON: jsonmap.JSON{"zh-CN": "原商品"},
				FulfillmentType: constants.FulfillmentTypeKafudaGPT,
				SKUs: []productwrite.ProductSKUInput{{
					SKUCode: code, PriceAmount: decimal.NewFromInt(100), CostPriceAmount: decimal.NewFromInt(45),
					CostPriceProvided: true, IsActive: &active, DirectCatalogRevision: &initialRevision, SortOrder: 23,
				}},
			}
			product, err := svc.Write.Create(input)
			if err != nil || product == nil {
				t.Fatalf("create direct product: %+v, %v", product, err)
			}
			id := fmt.Sprint(product.ID)
			product, err = svc.Read.GetAdminByID(id)
			if err != nil || product == nil || len(product.SKUs) != 1 || !product.SKUs[0].IsActive ||
				product.SKUs[0].DirectCatalogRevision != initialRevision {
				t.Fatalf("load initially approved SKU: %+v, %v", product, err)
			}
			skuID := product.SKUs[0].ID
			removePair := func() {
				saveCatalog(syncedAt.Add(time.Second), []kafudadomain.CatalogOption{})
			}
			if !scenario.explicitApproval {
				removePair()
			}
			newSale, newCost := decimal.NewFromInt(130), decimal.NewFromInt(55)
			const currentRevision uint64 = 1
			const newerTitle = "复核后的商品"
			newer := input
			newer.TitleJSON = jsonmap.JSON{"zh-CN": newerTitle}
			orders := productUpdateTransactionHook{Store: ordergormstore.New(db, "test-guest-credential-secret-with-32-bytes"), before: func() {
				if scenario.explicitApproval {
					removePair()
				}
				saveCatalog(syncedAt.Add(2*time.Second), []kafudadomain.CatalogOption{pair})
				options, err := policy.SKUOptions(ctx)
				if err != nil || len(options) != 1 || options[0].SKUCode != code ||
					!options[0].Available || options[0].CatalogRevision != currentRevision {
					t.Fatalf("restored catalog did not keep removal history: %+v, %v", options, err)
				}
				reviewedRevision := options[0].CatalogRevision
				newer.SKUs = []productwrite.ProductSKUInput{{
					ID: skuID, SKUCode: code, PriceAmount: newSale, CostPriceAmount: newCost, CostPriceProvided: true,
					IsActive: &active, DirectCatalogRevision: &reviewedRevision, SortOrder: 34,
				}}
				if _, err := svc.Write.Update(id, newer); err != nil {
					t.Fatalf("approve the restored SKU during the older edit: %v", err)
				}
			}}
			editor := productwrite.NewWriteService(productwrite.Options{
				Products: productgormstore.NewProductStore(db), SKUs: productgormstore.NewSKUStore(db),
				CardSecrets: cardsecretgormstore.New(db), Orders: orders,
			})
			editor.SetDirectCatalog(policy)
			const editedTitle = "较早的商品编辑"
			input.TitleJSON = jsonmap.JSON{"zh-CN": editedTitle}
			input.SKUs = nil
			wantTitle := editedTitle
			if scenario.explicitApproval {
				input.SKUs = []productwrite.ProductSKUInput{{
					ID: skuID, SKUCode: code, PriceAmount: decimal.NewFromInt(120),
					IsActive: &active, DirectCatalogRevision: &initialRevision, SortOrder: 26,
				}}
				wantTitle = newerTitle
			}
			_, err = editor.Update(id, input)
			if scenario.explicitApproval {
				if !errors.Is(err, productcontract.ErrProductSKUInvalid) {
					t.Errorf("older explicit approval should be rejected against current revision: %v", err)
				}
			} else if err != nil {
				t.Fatalf("save metadata against restored catalog: %v", err)
			}
			product, err = svc.Read.GetAdminByID(id)
			if err != nil || product == nil || len(product.SKUs) != 1 {
				t.Fatalf("load saved direct product: %+v, %v", product, err)
			}
			if !product.IsActive || product.TitleJSON["zh-CN"] != wantTitle ||
				!product.PriceAmount.Equal(newSale) || !product.CostPriceAmount.Equal(newCost) {
				t.Errorf("older edit altered the reviewed product or lost the permitted title update: %+v", product)
			}
			sku := product.SKUs[0]
			if sku.ID != skuID || !sku.IsActive || sku.DirectCatalogRevision != currentRevision || !sku.DirectCostConfigured ||
				!sku.PriceAmount.Equal(newSale) || !sku.CostPriceAmount.Equal(newCost) || sku.SortOrder != 34 {
				t.Errorf("older edit altered the reviewed SKU configuration: %+v", sku)
			}
			terms, err := policy.PurchaseTerms(ctx, plan, country, sku.DirectCatalogRevision)
			if err != nil || terms == nil || terms.PaymentProfile != profile {
				t.Errorf("purchase terms lost the approved catalog profile: %+v, %v", terms, err)
			}
		})
	}
}

func TestProductEditPreservesLegacyOrderStockChanges(t *testing.T) {
	for _, setStock := range []bool{false, true} {
		t.Run(fmt.Sprintf("set_stock_%t", setStock), func(t *testing.T) {
			_, db := newProductServiceForTest(t)
			pool, err := db.DB()
			if err != nil {
				t.Fatal(err)
			}
			pool.SetMaxOpenConns(1)
			t.Cleanup(func() { _ = pool.Close() })
			price := decimal.NewFromInt(10)
			product := productdomain.Product{
				Slug: "legacy-manual-stock", TitleJSON: jsonmap.JSON{"zh-CN": "原标题"},
				FulfillmentType: constants.FulfillmentTypeManual, PriceAmount: money.FromDecimal(price),
				ManualStockTotal: 8, ManualStockLocked: 2, ManualStockSold: 3,
			}
			if err := db.Create(&product).Error; err != nil {
				t.Fatal(err)
			}
			products := productgormstore.NewProductStore(db)
			orders := productUpdateTransactionHook{
				Store: ordergormstore.New(db, "test-guest-credential-secret-with-32-bytes"),
				before: func() {
					if affected, err := products.ReleaseManualStock(product.ID, 1); err != nil || affected != 1 {
						t.Fatalf("release legacy hold: %d, %v", affected, err)
					}
					if affected, err := products.ConsumeManualStock(product.ID, 1); err != nil || affected != 1 {
						t.Fatalf("consume legacy hold: %d, %v", affected, err)
					}
				},
			}
			service := productwrite.NewWriteService(productwrite.Options{
				Products: products, SKUs: productgormstore.NewSKUStore(db), Orders: orders,
			})
			input := productwrite.CreateProductInput{Slug: product.Slug, TitleJSON: jsonmap.JSON{"zh-CN": "编辑后"}, PriceAmount: price}
			wantTotal := 9
			if setStock {
				wantTotal = 12
				input.ManualStockTotal = &wantTotal
			}
			updated, err := service.Update(strconv.FormatUint(uint64(product.ID), 10), input)
			if err != nil {
				t.Fatal(err)
			}
			if updated.TitleJSON["zh-CN"] != "编辑后" || updated.ManualStockTotal != wantTotal ||
				updated.ManualStockLocked != 0 || updated.ManualStockSold != 4 {
				t.Fatalf("edit lost legacy inventory changes: %+v", updated)
			}
		})
	}
}

func TestProductEditSummarizesCurrentActiveSKUStock(t *testing.T) {
	for _, scenario := range []struct {
		name         string
		secondTotal  int
		secondActive bool
		wantTotal    int
	}{
		{"finite", 4, true, 13},
		{"unlimited", constants.ManualStockUnlimited, true, constants.ManualStockUnlimited},
		{"inactive_unlimited", constants.ManualStockUnlimited, false, 9},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			_, db := newProductServiceForTest(t)
			pool, err := db.DB()
			if err != nil {
				t.Fatal(err)
			}
			pool.SetMaxOpenConns(1)
			t.Cleanup(func() { _ = pool.Close() })
			price := decimal.NewFromInt(10)
			product := productdomain.Product{
				Slug: "multi-sku-stock", TitleJSON: jsonmap.JSON{"zh-CN": "原标题"},
				FulfillmentType: constants.FulfillmentTypeManual, PriceAmount: money.FromDecimal(price),
			}
			if err := db.Create(&product).Error; err != nil {
				t.Fatal(err)
			}
			skus := []productdomain.ProductSKU{
				{ProductID: product.ID, SKUCode: "first", PriceAmount: product.PriceAmount, IsActive: true,
					ManualStockTotal: 8, ManualStockLocked: 2, ManualStockSold: 3},
				{ProductID: product.ID, SKUCode: "second", PriceAmount: product.PriceAmount, IsActive: true,
					ManualStockTotal: scenario.secondTotal},
			}
			if err := db.Create(&skus).Error; err != nil {
				t.Fatal(err)
			}
			skuRepo := productgormstore.NewSKUStore(db)
			orders := productUpdateTransactionHook{
				Store: ordergormstore.New(db, "test-guest-credential-secret-with-32-bytes"),
				before: func() {
					if affected, err := skuRepo.ReleaseManualStock(skus[0].ID, 1); err != nil || affected != 1 {
						t.Fatalf("release SKU hold: %d, %v", affected, err)
					}
					if affected, err := skuRepo.ConsumeManualStock(skus[0].ID, 1); err != nil || affected != 1 {
						t.Fatalf("consume SKU hold: %d, %v", affected, err)
					}
				},
			}
			service := productwrite.NewWriteService(productwrite.Options{
				Products: productgormstore.NewProductStore(db), SKUs: skuRepo, Orders: orders,
			})
			input := productwrite.CreateProductInput{
				Slug: product.Slug, TitleJSON: jsonmap.JSON{"zh-CN": "编辑后"},
				SKUs: []productwrite.ProductSKUInput{
					{ID: skus[0].ID, SKUCode: skus[0].SKUCode, PriceAmount: price},
					{ID: skus[1].ID, SKUCode: skus[1].SKUCode, PriceAmount: price, IsActive: &scenario.secondActive},
				},
			}
			updated, err := service.Update(strconv.FormatUint(uint64(product.ID), 10), input)
			if err != nil || updated == nil || updated.ManualStockTotal != scenario.wantTotal {
				t.Fatalf("expected current SKU stock summary %d: %+v, %v", scenario.wantTotal, updated, err)
			}
			first, err := skuRepo.GetByID(skus[0].ID)
			if err != nil || first == nil || first.ManualStockTotal != 9 || first.ManualStockLocked != 0 || first.ManualStockSold != 4 {
				t.Fatalf("edit lost SKU stock changes: %+v, %v", first, err)
			}
		})
	}
}

func TestProductEditCannotDiscardReservedManualStock(t *testing.T) {
	for _, scenario := range []string{"single_mode", "remove_sku", "unlimited_sku", "unlimited_legacy_product"} {
		t.Run(scenario, func(t *testing.T) {
			_, db := newProductServiceForTest(t)
			pool, err := db.DB()
			if err != nil {
				t.Fatal(err)
			}
			pool.SetMaxOpenConns(1)
			t.Cleanup(func() { _ = pool.Close() })
			price := decimal.NewFromInt(10)
			product := productdomain.Product{
				Slug: "reserved-stock-edit", TitleJSON: jsonmap.JSON{"zh-CN": "原标题"},
				FulfillmentType: constants.FulfillmentTypeManual, PriceAmount: money.FromDecimal(price), ManualStockTotal: 5,
			}
			if err := db.Create(&product).Error; err != nil {
				t.Fatal(err)
			}
			skus := []productdomain.ProductSKU{
				{ProductID: product.ID, SKUCode: productdomain.DefaultSKUCode, PriceAmount: product.PriceAmount, ManualStockTotal: 5},
				{ProductID: product.ID, SKUCode: "reserved", PriceAmount: product.PriceAmount, ManualStockTotal: 5},
			}
			if err := db.Create(&skus).Error; err != nil {
				t.Fatal(err)
			}
			products := productgormstore.NewProductStore(db)
			skuRepo := productgormstore.NewSKUStore(db)
			reserve, release, owner := skuRepo.ReserveManualStock, skuRepo.ReleaseManualStock, skus[1].ID
			if scenario == "unlimited_legacy_product" {
				reserve, release, owner = products.ReserveManualStock, products.ReleaseManualStock, product.ID
			}
			reserved := false
			orders := productUpdateTransactionHook{
				Store: ordergormstore.New(db, "test-guest-credential-secret-with-32-bytes"),
				before: func() {
					if reserved {
						return
					}
					reserved = true
					if affected, err := reserve(owner, 1); err != nil || affected != 1 {
						t.Fatalf("reserve after preflight: %d, %v", affected, err)
					}
				},
			}
			service := productwrite.NewWriteService(productwrite.Options{Products: products, SKUs: skuRepo, Orders: orders})
			input := productwrite.CreateProductInput{
				Slug: product.Slug, TitleJSON: jsonmap.JSON{"zh-CN": "编辑后"}, PriceAmount: decimal.NewFromInt(20),
			}
			unlimited := constants.ManualStockUnlimited
			switch scenario {
			case "remove_sku":
				input.SKUs = []productwrite.ProductSKUInput{{ID: skus[0].ID, SKUCode: skus[0].SKUCode, PriceAmount: input.PriceAmount}}
			case "unlimited_sku":
				input.SKUs = []productwrite.ProductSKUInput{
					{ID: skus[0].ID, SKUCode: skus[0].SKUCode, PriceAmount: input.PriceAmount},
					{ID: skus[1].ID, SKUCode: skus[1].SKUCode, PriceAmount: input.PriceAmount, ManualStockTotal: &unlimited},
				}
			case "unlimited_legacy_product":
				input.ManualStockTotal = &unlimited
			}
			id := strconv.FormatUint(uint64(product.ID), 10)
			if _, err := service.Update(id, input); !errors.Is(err, productcontract.ErrManualStockReserved) {
				t.Fatalf("edit must reject reserved inventory, got %v", err)
			}
			actual, err := products.GetByID(id)
			if err != nil || actual == nil || actual.TitleJSON["zh-CN"] != "原标题" {
				t.Fatalf("rejected edit changed product: %+v, %v", actual, err)
			}
			for _, original := range skus {
				stored, err := skuRepo.GetByID(original.ID)
				if err != nil || stored == nil || !stored.PriceAmount.Equal(price) {
					t.Fatalf("rejected edit changed SKU %d: %+v, %v", original.ID, stored, err)
				}
			}
			if affected, err := release(owner, 1); err != nil || affected != 1 {
				t.Fatalf("rejected edit lost the hold: %d, %v", affected, err)
			}
			if _, err := service.Update(id, input); err != nil {
				t.Fatalf("edit after release: %v", err)
			}
		})
	}
}

func TestDirectTypeChangeRechecksDataAfterPreflight(t *testing.T) {
	for _, change := range []string{"order", "card", "sku_stock", "product_stock", "mapped", "type", "stale_edit"} {
		t.Run(change, func(t *testing.T) {
			_, db := newProductServiceForTest(t)
			pool, err := db.DB()
			if err != nil {
				t.Fatal(err)
			}
			pool.SetMaxOpenConns(1)
			t.Cleanup(func() { _ = pool.Close() })
			product := productdomain.Product{Slug: "preflight", TitleJSON: jsonmap.JSON{"zh-CN": "preflight"}, FulfillmentType: constants.FulfillmentTypeAuto, PriceAmount: money.FromDecimal(decimal.NewFromInt(10))}
			if err := db.Create(&product).Error; err != nil {
				t.Fatal(err)
			}
			sku := productdomain.ProductSKU{ProductID: product.ID, SKUCode: productdomain.DefaultSKUCode, PriceAmount: product.PriceAmount}
			if err := db.Create(&sku).Error; err != nil {
				t.Fatal(err)
			}
			var before productdomain.Product
			var beforeSKUs []productdomain.ProductSKU
			orders := productUpdateTransactionHook{Store: ordergormstore.New(db, "test-guest-credential-secret-with-32-bytes"), before: func() {
				var err error
				switch change {
				case "order":
					err = db.Create(&orderdomain.OrderItem{OrderID: 99, ProductID: product.ID, SKUID: sku.ID, TitleJSON: product.TitleJSON, FulfillmentType: product.FulfillmentType}).Error
				case "card":
					err = db.Create(&cardsecretdomain.Secret{ProductID: product.ID, SKUID: sku.ID, Secret: "late-card", Status: cardsecretdomain.StatusAvailable}).Error
				case "sku_stock":
					err = db.Model(&sku).Update("manual_stock_locked", 1).Error
				case "product_stock":
					err = db.Model(&product).Update("manual_stock_sold", 1).Error
				case "mapped":
					err = db.Model(&product).Update("is_mapped", true).Error
				case "type", "stale_edit":
					err = db.Model(&product).Update("fulfillment_type", constants.FulfillmentTypeManual).Error
				}
				if err != nil {
					t.Fatal(err)
				}
				if err := db.First(&before, product.ID).Error; err != nil {
					t.Fatal(err)
				}
				if err := db.Where("product_id = ?", product.ID).Find(&beforeSKUs).Error; err != nil {
					t.Fatal(err)
				}
			}}
			svc := productwrite.NewWriteService(productwrite.Options{Products: productgormstore.NewProductStore(db), SKUs: productgormstore.NewSKUStore(db), CardSecrets: cardsecretgormstore.New(db), Orders: orders})
			svc.SetDirectCatalog(&directProductTestCatalog{options: []kafudacontract.SKUOption{{SKUCode: kafudacontract.SKUCode("plus", "PH"), Plan: "plus", Country: "PH", Available: true}}})
			target := constants.FulfillmentTypeKafudaGPT
			if change == "stale_edit" {
				target = constants.FulfillmentTypeAuto
			}
			_, err = svc.Update(fmt.Sprint(product.ID), productwrite.CreateProductInput{Slug: product.Slug, TitleJSON: product.TitleJSON, FulfillmentType: target, PriceAmount: decimal.NewFromInt(10)})
			want := productcontract.ErrFulfillmentInvalid
			if change == "card" {
				want = productcontract.ErrProductSKUHasCardSecretStock
			}
			if !errors.Is(err, want) {
				t.Fatalf("expected transaction recheck rejection %v, got %v", want, err)
			}
			var after productdomain.Product
			var afterSKUs []productdomain.ProductSKU
			if err := db.First(&after, product.ID).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Where("product_id = ?", product.ID).Find(&afterSKUs).Error; err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(beforeSKUs, afterSKUs) {
				t.Fatal("rejected conversion changed product or SKUs")
			}
		})
	}
}

func TestDirectTypeChangePreservesIncompatibleProductData(t *testing.T) {
	for _, test := range []struct {
		name                         string
		cardStatus                   string
		skuTotal, skuLocked, skuSold int
		productSold                  int
		mapped, history, outgoing    bool
	}{
		{name: "available_cards_on_manual", cardStatus: cardsecretdomain.StatusAvailable},
		{name: "reserved_cards_on_manual", cardStatus: cardsecretdomain.StatusReserved},
		{name: "used_cards_on_manual", cardStatus: cardsecretdomain.StatusUsed},
		{name: "sku_stock", skuTotal: 2},
		{name: "sku_unlimited", skuTotal: -1},
		{name: "sku_locked", skuLocked: 1},
		{name: "sku_sold", skuSold: 1},
		{name: "product_sold", productSold: 1},
		{name: "mapped", mapped: true},
		{name: "order_history", history: true},
		{name: "outgoing_order_history", history: true, outgoing: true},
		{name: "outgoing_cards", cardStatus: cardsecretdomain.StatusUsed, outgoing: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			svc, db := newProductServiceForTest(t)
			pool, err := db.DB()
			if err != nil {
				t.Fatal(err)
			}
			pool.SetMaxOpenConns(1)
			t.Cleanup(func() { _ = pool.Close() })
			original, target := constants.FulfillmentTypeManual, constants.FulfillmentTypeKafudaGPT
			if test.outgoing {
				original, target = target, original
			}
			product := productdomain.Product{Slug: "protected", TitleJSON: jsonmap.JSON{"zh-CN": "original"}, FulfillmentType: original, PurchaseType: constants.ProductPurchaseMember, PriceAmount: money.FromDecimal(decimal.NewFromInt(10)), IsMapped: test.mapped, ManualStockSold: test.productSold}
			if err := db.Create(&product).Error; err != nil {
				t.Fatal(err)
			}
			sku := productdomain.ProductSKU{ProductID: product.ID, SKUCode: "original", PriceAmount: money.FromDecimal(decimal.NewFromInt(10)), ManualStockTotal: test.skuTotal, ManualStockLocked: test.skuLocked, ManualStockSold: test.skuSold}
			if err := db.Create(&sku).Error; err != nil {
				t.Fatal(err)
			}
			if test.cardStatus != "" {
				if err := db.Create(&cardsecretdomain.Secret{ProductID: product.ID, SKUID: sku.ID, Status: test.cardStatus, Secret: "test-only-card"}).Error; err != nil {
					t.Fatal(err)
				}
			}
			if test.history {
				order := orderdomain.Order{OrderNo: "type-change-history", Status: constants.OrderStatusCompleted, Currency: "CNY"}
				if err := db.Create(&order).Error; err != nil {
					t.Fatal(err)
				}
				item := orderdomain.OrderItem{OrderID: order.ID, ProductID: product.ID, SKUID: sku.ID, Quantity: 1, FulfillmentType: original, TitleJSON: jsonmap.JSON{"zh-CN": "paid snapshot"}, SKUSnapshotJSON: jsonmap.JSON{"sku_code": sku.SKUCode}, CostPrice: money.FromDecimal(decimal.NewFromInt(4)), UnitPrice: money.FromDecimal(decimal.NewFromInt(10))}
				if err := db.Create(&item).Error; err != nil {
					t.Fatal(err)
				}
			}
			var beforeProduct productdomain.Product
			var beforeSKUs []productdomain.ProductSKU
			var beforeCards []cardsecretdomain.Secret
			var beforeItems []orderdomain.OrderItem
			if err := db.First(&beforeProduct, product.ID).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Where("product_id = ?", product.ID).Find(&beforeSKUs).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Where("product_id = ?", product.ID).Find(&beforeCards).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Where("product_id = ?", product.ID).Find(&beforeItems).Error; err != nil {
				t.Fatal(err)
			}
			_, err = svc.Write.Update(fmt.Sprint(product.ID), productwrite.CreateProductInput{Slug: "changed", TitleJSON: map[string]interface{}{"zh-CN": "changed"}, FulfillmentType: target, PriceAmount: decimal.NewFromInt(20)})
			want := productcontract.ErrFulfillmentInvalid
			if test.cardStatus != "" {
				want = productcontract.ErrProductSKUHasCardSecretStock
			}
			if !errors.Is(err, want) {
				t.Fatalf("type change error = %v, want %v", err, want)
			}
			var afterProduct productdomain.Product
			var afterSKUs []productdomain.ProductSKU
			var afterCards []cardsecretdomain.Secret
			var afterItems []orderdomain.OrderItem
			if err := db.First(&afterProduct, product.ID).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Where("product_id = ?", product.ID).Find(&afterSKUs).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Where("product_id = ?", product.ID).Find(&afterCards).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Where("product_id = ?", product.ID).Find(&afterItems).Error; err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(beforeProduct, afterProduct) || !reflect.DeepEqual(beforeSKUs, afterSKUs) || !reflect.DeepEqual(beforeCards, afterCards) || !reflect.DeepEqual(beforeItems, afterItems) {
				t.Fatal("rejected conversion modified product, SKUs, card secrets or order snapshots")
			}
		})
	}
}

func TestEmptyProductsCanSwitchDirectFulfillmentType(t *testing.T) {
	for _, source := range []string{constants.FulfillmentTypeAuto, constants.FulfillmentTypeManual, constants.FulfillmentTypeKafudaGPT} {
		t.Run(source, func(t *testing.T) {
			svc, db := newProductServiceForTest(t)
			pool, err := db.DB()
			if err != nil {
				t.Fatal(err)
			}
			pool.SetMaxOpenConns(1)
			t.Cleanup(func() { _ = pool.Close() })
			product := productdomain.Product{Slug: "empty", TitleJSON: jsonmap.JSON{"zh-CN": "empty"}, PurchaseType: constants.ProductPurchaseMember, FulfillmentType: source, PriceAmount: money.FromDecimal(decimal.NewFromInt(10))}
			if err := db.Create(&product).Error; err != nil {
				t.Fatal(err)
			}
			sku := productdomain.ProductSKU{ProductID: product.ID, SKUCode: productdomain.DefaultSKUCode, PriceAmount: product.PriceAmount}
			if err := db.Create(&sku).Error; err != nil {
				t.Fatal(err)
			}
			target := constants.FulfillmentTypeKafudaGPT
			if source == target {
				target = constants.FulfillmentTypeManual
			}
			svc.Write.SetDirectCatalog(&directProductTestCatalog{options: []kafudacontract.SKUOption{{SKUCode: kafudacontract.SKUCode("plus", "PH"), Plan: "plus", Country: "PH", Available: true}}})
			updated, err := svc.Write.Update(fmt.Sprint(product.ID), productwrite.CreateProductInput{Slug: product.Slug, TitleJSON: map[string]interface{}{"zh-CN": "empty"}, FulfillmentType: target, PriceAmount: decimal.NewFromInt(10)})
			if err != nil || updated.FulfillmentType != target {
				t.Fatalf("empty product conversion failed: %+v, %v", updated, err)
			}
		})
	}
}

func TestProductServiceUpdateKeepsMappedProductFulfillmentUpstream(t *testing.T) {
	svc, db := newProductServiceForTest(t)

	category := categorydomain.Category{
		Slug:     "mapped-category",
		NameJSON: jsonmap.JSON{"zh-CN": "mapped-category"},
	}
	if err := db.Create(&category).Error; err != nil {
		t.Fatalf("create category failed: %v", err)
	}

	product := productdomain.Product{
		CategoryID:       category.ID,
		Slug:             "mapped-product",
		TitleJSON:        jsonmap.JSON{"zh-CN": "mapped-product"},
		PriceAmount:      money.FromDecimal(decimal.NewFromInt(10)),
		PurchaseType:     constants.ProductPurchaseMember,
		FulfillmentType:  constants.FulfillmentTypeUpstream,
		ManualStockTotal: 0,
		IsMapped:         true,
		IsActive:         true,
	}
	if err := db.Create(&product).Error; err != nil {
		t.Fatalf("create mapped product failed: %v", err)
	}

	sku := productdomain.ProductSKU{
		ProductID:      product.ID,
		SKUCode:        productdomain.DefaultSKUCode,
		SpecValuesJSON: jsonmap.JSON{},
		PriceAmount:    money.FromDecimal(decimal.NewFromInt(10)),
		IsActive:       true,
	}
	if err := db.Create(&sku).Error; err != nil {
		t.Fatalf("create mapped product sku failed: %v", err)
	}

	updated, err := svc.Write.Update(strconv.FormatUint(uint64(product.ID), 10), productwrite.CreateProductInput{
		CategoryID:      category.ID,
		Slug:            "mapped-product-updated",
		TitleJSON:       map[string]interface{}{"zh-CN": "mapped-product-updated"},
		PriceAmount:     decimal.NewFromInt(20),
		PurchaseType:    constants.ProductPurchaseMember,
		FulfillmentType: constants.FulfillmentTypeAuto,
		ManualStockTotal: func() *int {
			value := 0
			return &value
		}(),
		IsActive: func() *bool {
			value := true
			return &value
		}(),
	})
	if err != nil {
		t.Fatalf("update mapped product failed: %v", err)
	}
	if updated.FulfillmentType != constants.FulfillmentTypeUpstream {
		t.Fatalf("expected mapped product fulfillment type to remain upstream, got %s", updated.FulfillmentType)
	}

	reloaded, err := svc.Read.GetAdminByID(strconv.FormatUint(uint64(product.ID), 10))
	if err != nil {
		t.Fatalf("reload mapped product failed: %v", err)
	}
	if reloaded.FulfillmentType != constants.FulfillmentTypeUpstream {
		t.Fatalf("expected persisted fulfillment type upstream, got %s", reloaded.FulfillmentType)
	}
}

func TestProductServiceUpdateFiltersUnavailablePaymentChannels(t *testing.T) {
	svc, db := newProductServiceForTest(t)

	category := categorydomain.Category{
		Slug:     "payment-channel-update-category",
		NameJSON: jsonmap.JSON{"zh-CN": "payment-channel-update-category"},
	}
	if err := db.Create(&category).Error; err != nil {
		t.Fatalf("create category failed: %v", err)
	}

	deletedChannel := createProductTestPaymentChannel(t, db, "Deleted", true, true)
	product := productdomain.Product{
		CategoryID:        category.ID,
		Slug:              "payment-channel-update",
		TitleJSON:         jsonmap.JSON{"zh-CN": "payment-channel-update"},
		PriceAmount:       money.FromDecimal(decimal.NewFromInt(10)),
		PurchaseType:      constants.ProductPurchaseMember,
		FulfillmentType:   constants.FulfillmentTypeAuto,
		PaymentChannelIDs: productdomain.EncodePaymentChannelIDs([]uint{deletedChannel.ID}),
		IsActive:          true,
	}
	if err := db.Create(&product).Error; err != nil {
		t.Fatalf("create product failed: %v", err)
	}

	updated, err := svc.Write.Update(strconv.FormatUint(uint64(product.ID), 10), productwrite.CreateProductInput{
		CategoryID:        category.ID,
		Slug:              product.Slug,
		TitleJSON:         map[string]interface{}{"zh-CN": "payment-channel-update"},
		PriceAmount:       decimal.NewFromInt(10),
		PurchaseType:      constants.ProductPurchaseMember,
		FulfillmentType:   constants.FulfillmentTypeAuto,
		PaymentChannelIDs: []uint{deletedChannel.ID},
		IsActive: func() *bool {
			value := true
			return &value
		}(),
	})
	if err != nil {
		t.Fatalf("update product failed: %v", err)
	}
	if got := productdomain.DecodePaymentChannelIDs(updated.PaymentChannelIDs); len(got) != 0 {
		t.Fatalf("expected stale-only payment channels to be cleared, got %v", got)
	}
}

func TestProductServiceUpdateRejectsInvalidPurchaseLimits(t *testing.T) {
	svc, db := newProductServiceForTest(t)

	cat := categorydomain.Category{Slug: "test-purchase-limit-update", NameJSON: jsonmap.JSON{"zh-CN": "test"}}
	if err := db.Create(&cat).Error; err != nil {
		t.Fatalf("create category: %v", err)
	}

	intPtr := func(v int) *int { return &v }

	created, err := svc.Write.Create(productwrite.CreateProductInput{
		CategoryID:          cat.ID,
		Slug:                "valid-limit-product",
		TitleJSON:           map[string]interface{}{"zh-CN": "valid"},
		PriceAmount:         decimal.NewFromInt(10),
		PurchaseType:        constants.ProductPurchaseMember,
		FulfillmentType:     constants.FulfillmentTypeManual,
		ManualStockTotal:    intPtr(1),
		MinPurchaseQuantity: intPtr(2),
		MaxPurchaseQuantity: intPtr(5),
	})
	if err != nil {
		t.Fatalf("create product failed: %v", err)
	}

	_, err = svc.Write.Update(strconv.FormatUint(uint64(created.ID), 10), productwrite.CreateProductInput{
		CategoryID:          cat.ID,
		Slug:                "valid-limit-product",
		TitleJSON:           map[string]interface{}{"zh-CN": "valid"},
		PriceAmount:         decimal.NewFromInt(10),
		PurchaseType:        constants.ProductPurchaseMember,
		FulfillmentType:     constants.FulfillmentTypeManual,
		ManualStockTotal:    intPtr(1),
		MaxPurchaseQuantity: intPtr(1), // 已存在 min=2，新设 max=1 应触发校验
	})
	if !errors.Is(err, productcontract.ErrProductPurchaseLimitInvalid) {
		t.Fatalf("expected productcontract.ErrProductPurchaseLimitInvalid on update, got %v", err)
	}
}
