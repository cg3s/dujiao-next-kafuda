package integrationtest

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	productwrite "github.com/dujiao-next/internal/modules/catalog/product/application/write"
	productcontract "github.com/dujiao-next/internal/modules/catalog/product/contract"
	productdomain "github.com/dujiao-next/internal/modules/catalog/product/domain"

	categorydomain "github.com/dujiao-next/internal/modules/catalog/category/domain"

	"github.com/dujiao-next/internal/constants"
	kafudaapp "github.com/dujiao-next/internal/modules/kafudagpt/application"
	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
	kafudastore "github.com/dujiao-next/internal/modules/kafudagpt/infrastructure/gormstore"
	"github.com/dujiao-next/internal/shared/jsonmap"
	"github.com/shopspring/decimal"
)

func TestProductServiceCreateRejectsParentCategoryWithChildren(t *testing.T) {
	svc, db := newProductServiceForTest(t)

	parent := categorydomain.Category{
		Slug:     "games",
		NameJSON: jsonmap.JSON{"zh-CN": "games"},
	}
	if err := db.Create(&parent).Error; err != nil {
		t.Fatalf("create parent category failed: %v", err)
	}
	child := categorydomain.Category{
		ParentID: parent.ID,
		Slug:     "steam",
		NameJSON: jsonmap.JSON{"zh-CN": "steam"},
	}
	if err := db.Create(&child).Error; err != nil {
		t.Fatalf("create child category failed: %v", err)
	}

	_, err := svc.Write.Create(productwrite.CreateProductInput{
		CategoryID:      parent.ID,
		Slug:            "invalid-parent-product",
		TitleJSON:       map[string]interface{}{"zh-CN": "invalid-parent-product"},
		PriceAmount:     decimal.NewFromInt(10),
		PurchaseType:    constants.ProductPurchaseMember,
		FulfillmentType: constants.FulfillmentTypeManual,
		ManualStockTotal: func() *int {
			value := 1
			return &value
		}(),
	})
	if err != productcontract.ErrProductCategoryInvalid {
		t.Fatalf("expected productcontract.ErrProductCategoryInvalid, got %v", err)
	}
}

func TestProductServiceCreateFiltersUnavailablePaymentChannels(t *testing.T) {
	svc, db := newProductServiceForTest(t)

	category := categorydomain.Category{
		Slug:     "payment-channel-category",
		NameJSON: jsonmap.JSON{"zh-CN": "payment-channel-category"},
	}
	if err := db.Create(&category).Error; err != nil {
		t.Fatalf("create category failed: %v", err)
	}

	activeChannel := createProductTestPaymentChannel(t, db, "Active", true, false)
	inactiveChannel := createProductTestPaymentChannel(t, db, "Inactive", false, false)
	deletedChannel := createProductTestPaymentChannel(t, db, "Deleted", true, true)

	product, err := svc.Write.Create(productwrite.CreateProductInput{
		CategoryID:        category.ID,
		Slug:              "payment-channel-create",
		TitleJSON:         map[string]interface{}{"zh-CN": "payment-channel-create"},
		PriceAmount:       decimal.NewFromInt(10),
		PurchaseType:      constants.ProductPurchaseMember,
		FulfillmentType:   constants.FulfillmentTypeAuto,
		PaymentChannelIDs: []uint{deletedChannel.ID, inactiveChannel.ID, activeChannel.ID},
		IsActive: func() *bool {
			value := true
			return &value
		}(),
	})
	if err != nil {
		t.Fatalf("create product failed: %v", err)
	}

	got := productdomain.DecodePaymentChannelIDs(product.PaymentChannelIDs)
	if len(got) != 1 || got[0] != activeChannel.ID {
		t.Fatalf("expected only active payment channel %d, got %v", activeChannel.ID, got)
	}
}

func TestProductServiceCreateRejectsInvalidPurchaseLimits(t *testing.T) {
	svc, db := newProductServiceForTest(t)

	cat := categorydomain.Category{Slug: "test-purchase-limit", NameJSON: jsonmap.JSON{"zh-CN": "test"}}
	if err := db.Create(&cat).Error; err != nil {
		t.Fatalf("create category: %v", err)
	}

	intPtr := func(v int) *int { return &v }

	_, err := svc.Write.Create(productwrite.CreateProductInput{
		CategoryID:          cat.ID,
		Slug:                "invalid-limit-product",
		TitleJSON:           map[string]interface{}{"zh-CN": "invalid-limit-product"},
		PriceAmount:         decimal.NewFromInt(10),
		PurchaseType:        constants.ProductPurchaseMember,
		FulfillmentType:     constants.FulfillmentTypeManual,
		ManualStockTotal:    intPtr(1),
		MinPurchaseQuantity: intPtr(10),
		MaxPurchaseQuantity: intPtr(5),
	})
	if !errors.Is(err, productcontract.ErrProductPurchaseLimitInvalid) {
		t.Fatalf("expected productcontract.ErrProductPurchaseLimitInvalid, got %v", err)
	}
}

func TestProductServiceCreateRollsBackProductAndSKUWhenWholesaleValidationFails(t *testing.T) {
	svc, db := newProductServiceForTest(t)
	category := categorydomain.Category{
		Slug:     "write-rollback-category",
		NameJSON: jsonmap.JSON{"zh-CN": "write-rollback-category"},
	}
	if err := db.Create(&category).Error; err != nil {
		t.Fatalf("create category: %v", err)
	}

	_, err := svc.Write.Create(productwrite.CreateProductInput{
		CategoryID:      category.ID,
		Slug:            "write-rollback-product",
		TitleJSON:       map[string]interface{}{"zh-CN": "write-rollback-product"},
		PriceAmount:     decimal.NewFromInt(10),
		PurchaseType:    constants.ProductPurchaseMember,
		FulfillmentType: constants.FulfillmentTypeAuto,
		WholesalePrices: &[]productdomain.WholesalePriceInput{{
			MinQuantity: 0,
			UnitPrice:   decimal.NewFromInt(8),
		}},
	})
	if !errors.Is(err, productdomain.ErrWholesalePriceInvalid) {
		t.Fatalf("expected productdomain.ErrWholesalePriceInvalid, got %v", err)
	}

	var productCount int64
	if err := db.Model(&productdomain.Product{}).Where("slug = ?", "write-rollback-product").Count(&productCount).Error; err != nil {
		t.Fatalf("count products: %v", err)
	}
	if productCount != 0 {
		t.Fatalf("transaction must roll back product, got %d rows", productCount)
	}
	var skuCount int64
	if err := db.Model(&productdomain.ProductSKU{}).Count(&skuCount).Error; err != nil {
		t.Fatalf("count product SKUs: %v", err)
	}
	if skuCount != 0 {
		t.Fatalf("transaction must roll back default SKU, got %d rows", skuCount)
	}
}

type directProductTestCatalog struct {
	options []kafudacontract.SKUOption
}

func (c *directProductTestCatalog) SKUOptions(context.Context) ([]kafudacontract.SKUOption, error) {
	return c.options, nil
}

func (*directProductTestCatalog) PurchaseTerms(context.Context, string, string, uint64) (*kafudacontract.PurchaseTerms, error) {
	return nil, fmt.Errorf("purchase terms are outside product configuration")
}

func TestDirectProductCreateKeepsUnconfiguredVariantsDisabled(t *testing.T) {
	for _, configureOne := range []bool{false, true} {
		t.Run(fmt.Sprintf("configured_%t", configureOne), func(t *testing.T) {
			svc, db := newProductServiceForTest(t)
			pool, err := db.DB()
			if err != nil {
				t.Fatal(err)
			}
			pool.SetMaxOpenConns(1)
			t.Cleanup(func() { _ = pool.Close() })
			code := kafudacontract.SKUCode("plus", "PH")
			catalog := &directProductTestCatalog{options: []kafudacontract.SKUOption{
				{SKUCode: code, Plan: "plus", Country: "PH", Available: true},
				{SKUCode: kafudacontract.SKUCode("go", "US"), Plan: "go", Country: "US", Available: true},
			}}
			svc.Write.SetDirectCatalog(catalog)
			input := productwrite.CreateProductInput{
				Slug: "direct-defaults", TitleJSON: map[string]interface{}{"zh-CN": "Direct"},
				FulfillmentType: constants.FulfillmentTypeKafudaGPT,
			}
			if configureOne {
				active := true
				input.SKUs = []productwrite.ProductSKUInput{{
					SKUCode: code, PriceAmount: decimal.NewFromInt(100),
					CostPriceAmount: decimal.Zero, CostPriceProvided: true, IsActive: &active,
				}}
			}
			product, err := svc.Write.Create(input)
			if err != nil {
				t.Fatal(err)
			}
			product, err = svc.Read.GetAdminByID(fmt.Sprint(product.ID))
			if err != nil {
				t.Fatal(err)
			}
			if product.IsActive != configureOne {
				t.Errorf("product active = %t, want %t", product.IsActive, configureOne)
			}
			if len(product.SKUs) != 2 {
				t.Fatalf("got %d variants, want 2", len(product.SKUs))
			}
			for _, sku := range product.SKUs {
				wantActive := configureOne && sku.SKUCode == code
				if sku.DirectCostConfigured != wantActive {
					t.Errorf("variant %s cost confirmed = %t, want %t", sku.SKUCode, sku.DirectCostConfigured, wantActive)
				}
				if sku.IsActive != wantActive {
					t.Errorf("variant %s active = %t, want %t", sku.SKUCode, sku.IsActive, wantActive)
				}
			}

			// A later catalog addition must also stay disabled when saved.
			catalog.options = append(catalog.options, kafudacontract.SKUOption{
				SKUCode: kafudacontract.SKUCode("plus", "JP"), Plan: "plus", Country: "JP", Available: true,
			})
			updated, err := svc.Write.Update(fmt.Sprint(product.ID), input)
			if err != nil {
				t.Fatal(err)
			}
			updated, err = svc.Read.GetAdminByID(fmt.Sprint(updated.ID))
			if err != nil {
				t.Fatal(err)
			}
			if len(updated.SKUs) != 3 {
				t.Fatalf("got %d variants after update, want 3", len(updated.SKUs))
			}
			for _, sku := range updated.SKUs {
				wantActive := configureOne && sku.SKUCode == code
				if sku.IsActive != wantActive {
					t.Errorf("updated variant %s active = %t, want %t", sku.SKUCode, sku.IsActive, wantActive)
				}
			}
			var persisted productdomain.Product
			if err := db.First(&persisted, product.ID).Error; err != nil {
				t.Fatal(err)
			}
			if persisted.IsActive != configureOne {
				t.Errorf("persisted product active = %t, want %t", persisted.IsActive, configureOne)
			}
		})
	}
}

func TestDirectProductCostConfirmationSurvivesCatalogMerges(t *testing.T) {
	svc, db := newProductServiceForTest(t)
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = pool.Close() })
	code := kafudacontract.SKUCode("plus", "PH")
	catalog := &directProductTestCatalog{options: []kafudacontract.SKUOption{{SKUCode: code, Plan: "plus", Country: "PH", Available: true}}}
	svc.Write.SetDirectCatalog(catalog)
	active := false
	input := productwrite.CreateProductInput{
		Slug: "direct-cost", TitleJSON: map[string]interface{}{"zh-CN": "Direct"},
		FulfillmentType: constants.FulfillmentTypeKafudaGPT,
		SKUs:            []productwrite.ProductSKUInput{{SKUCode: code, PriceAmount: decimal.NewFromInt(100), IsActive: &active, SortOrder: 23}},
	}
	product, err := svc.Write.Create(input)
	if err != nil {
		t.Fatal(err)
	}
	active = true
	if _, err := svc.Write.Update(fmt.Sprint(product.ID), input); !errors.Is(err, productcontract.ErrProductPriceInvalid) {
		t.Fatalf("unconfirmed default zero allowed activation: %v", err)
	}
	input.SKUs[0].CostPriceProvided = true
	if _, err := svc.Write.Update(fmt.Sprint(product.ID), input); err != nil {
		t.Fatalf("explicit zero cost rejected: %v", err)
	}
	product, err = svc.Read.GetAdminByID(fmt.Sprint(product.ID))
	if err != nil || len(product.SKUs) != 1 {
		t.Fatalf("read configured SKU: %v", err)
	}
	first := product.SKUs[0]
	if !first.DirectCostConfigured || !first.CostPriceAmount.IsZero() || !first.IsActive {
		t.Fatalf("explicit zero confirmation was lost: %+v", first)
	}
	input.SKUs[0].CostPriceAmount = decimal.NewFromInt(45)
	if _, err := svc.Write.Update(fmt.Sprint(product.ID), input); err != nil {
		t.Fatal(err)
	}
	// A catalog-only save preserves the persisted cost, confirmation, ID and sort.
	input.SKUs = nil
	for _, available := range []bool{true, false, true} {
		catalog.options[0].Available = available
		if _, err := svc.Write.Update(fmt.Sprint(product.ID), input); err != nil {
			t.Fatal(err)
		}
		product, err = svc.Read.GetAdminByID(fmt.Sprint(product.ID))
		if err != nil || len(product.SKUs) != 1 {
			t.Fatalf("read merged SKU: %v", err)
		}
		sku := product.SKUs[0]
		if sku.ID != first.ID || !sku.DirectCostConfigured || !sku.CostPriceAmount.Equal(decimal.NewFromInt(45)) || !sku.PriceAmount.Equal(decimal.NewFromInt(100)) || sku.SortOrder != 23 {
			t.Fatalf("catalog merge changed administrator configuration: %+v", sku)
		}
		if !available && sku.IsActive {
			t.Fatal("unavailable option stayed active")
		}
	}
	if product.SKUs[0].IsActive {
		t.Fatal("reappeared option was automatically re-enabled")
	}
	input.SKUs = []productwrite.ProductSKUInput{{SKUCode: code, PriceAmount: decimal.NewFromInt(100), IsActive: &active}}
	if _, err := svc.Write.Update(fmt.Sprint(product.ID), input); err != nil {
		t.Fatalf("omitted cost failed to preserve confirmed cost: %v", err)
	}
	product, err = svc.Read.GetAdminByID(fmt.Sprint(product.ID))
	if err != nil || !product.SKUs[0].DirectCostConfigured || !product.SKUs[0].CostPriceAmount.Equal(decimal.NewFromInt(45)) || !product.SKUs[0].IsActive {
		t.Fatalf("reactivation lost confirmed cost: %+v, %v", product, err)
	}
}

func TestDirectCatalogExpiryPreservesAdministratorConfiguration(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(fmt.Sprintf("enabled_%t", enabled), func(t *testing.T) {
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
			ctx, now := context.Background(), time.Now().UTC()
			store := kafudastore.New(db)
			catalog := &kafudadomain.Catalog{
				Profile: "default", Enabled: true, SyncedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(time.Hour),
				Options: []kafudadomain.CatalogOption{{Plan: "plus", Country: "PH", Product: "gpt", Label: "Plus", Available: true}},
				Cards:   []kafudadomain.CardProduct{{ProductCode: "test-card"}},
			}
			if err := store.SaveCatalog(ctx, catalog); err != nil {
				t.Fatal(err)
			}
			policy := kafudaapp.NewCommerceService(kafudaapp.CommerceOptions{
				Catalog:      kafudaapp.NewCatalogService(store, nil, "default", time.Hour),
				SalesEnabled: true, ReferenceLookupVerified: true, ProductCode: "test-card",
				MaxCostCents: 2000, AllowedKinds: []string{"new"}, WorkerReady: func(context.Context) bool { return true },
			})
			svc.Write.SetDirectCatalog(policy)
			if _, err := policy.PurchaseTerms(ctx, "plus", "PH", 0); err != nil {
				t.Fatalf("fresh catalog rejected: %v", err)
			}
			input := productwrite.CreateProductInput{
				Slug: "expiry", TitleJSON: map[string]interface{}{"zh-CN": "Direct"},
				FulfillmentType: constants.FulfillmentTypeKafudaGPT,
				SKUs: []productwrite.ProductSKUInput{{
					SKUCode: kafudacontract.SKUCode("plus", "PH"), PriceAmount: decimal.NewFromInt(100),
					CostPriceAmount: decimal.NewFromInt(45), CostPriceProvided: true, IsActive: &enabled, SortOrder: 23,
				}},
			}
			product, err := svc.Write.Create(input)
			if err != nil {
				t.Fatal(err)
			}
			product, err = svc.Read.GetAdminByID(fmt.Sprint(product.ID))
			if err != nil || len(product.SKUs) != 1 {
				t.Fatalf("read created SKU: %v", err)
			}
			skuID := product.SKUs[0].ID
			catalog.SyncedAt, catalog.ExpiresAt = now.Add(-time.Hour), now.Add(-time.Second)
			if err := store.SaveCatalog(ctx, catalog); err != nil {
				t.Fatal(err)
			}
			if _, err := policy.PurchaseTerms(ctx, "plus", "PH", 0); !errors.Is(err, kafudaapp.ErrQuoteUnavailable) {
				t.Fatalf("expired catalog allowed purchase terms: %v", err)
			}
			input.SKUs = nil
			for _, refresh := range []bool{false, true} {
				if refresh {
					catalog.SyncedAt, catalog.ExpiresAt = now, now.Add(time.Hour)
					if err := store.SaveCatalog(ctx, catalog); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := svc.Write.Update(fmt.Sprint(product.ID), input); err != nil {
					t.Fatal(err)
				}
				product, err = svc.Read.GetAdminByID(fmt.Sprint(product.ID))
				if err != nil || len(product.SKUs) != 1 {
					t.Fatalf("read saved SKU: %v", err)
				}
				sku := product.SKUs[0]
				if sku.ID != skuID || sku.IsActive != enabled || !sku.DirectCostConfigured || !sku.CostPriceAmount.Equal(decimal.NewFromInt(45)) || !sku.PriceAmount.Equal(decimal.NewFromInt(100)) || sku.SortOrder != 23 {
					t.Fatalf("expiry/refresh changed administrator configuration: %+v", sku)
				}
			}
			catalog.Options[0].Available = false
			catalog.SyncedAt = now.Add(time.Second)
			if err := store.SaveCatalog(ctx, catalog); err != nil {
				t.Fatal(err)
			}
			if _, err := policy.PurchaseTerms(ctx, "plus", "PH", 0); !errors.Is(err, kafudaapp.ErrQuoteUnavailable) {
				t.Fatalf("unavailable plan allowed purchase terms: %v", err)
			}
			if _, err := svc.Write.Update(fmt.Sprint(product.ID), input); err != nil {
				t.Fatal(err)
			}
			product, err = svc.Read.GetAdminByID(fmt.Sprint(product.ID))
			if err != nil || product.SKUs[0].IsActive {
				t.Fatalf("confirmed removal did not disable SKU on save: %+v, %v", product, err)
			}
		})
	}
}

func TestDirectProductRequiresReviewAfterCatalogOnlyRemoval(t *testing.T) {
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
	ctx, now := context.Background(), time.Now().UTC()
	store := kafudastore.New(db)
	catalog := &kafudadomain.Catalog{
		Profile: "default", Enabled: true, SyncedAt: now, ExpiresAt: now.Add(time.Hour),
		Options: []kafudadomain.CatalogOption{{Plan: "plus", Country: "PH", Product: "gpt", Label: "Plus", Available: true}},
		Cards:   []kafudadomain.CardProduct{{ProductCode: "test-card"}},
	}
	if err := store.SaveCatalog(ctx, catalog); err != nil {
		t.Fatal(err)
	}
	policy := kafudaapp.NewCommerceService(kafudaapp.CommerceOptions{
		Catalog:      kafudaapp.NewCatalogService(store, nil, "default", time.Hour),
		SalesEnabled: true, ReferenceLookupVerified: true, ProductCode: "test-card",
		MaxCostCents: 2000, AllowedKinds: []string{"new"}, WorkerReady: func(context.Context) bool { return true },
	})
	svc.Write.SetDirectCatalog(policy)
	active := true
	input := productwrite.CreateProductInput{
		Slug: "review", TitleJSON: map[string]interface{}{"zh-CN": "Direct"}, FulfillmentType: constants.FulfillmentTypeKafudaGPT,
		SKUs: []productwrite.ProductSKUInput{{SKUCode: kafudacontract.SKUCode("plus", "PH"), PriceAmount: decimal.NewFromInt(100), CostPriceAmount: decimal.NewFromInt(45), CostPriceProvided: true, IsActive: &active, SortOrder: 23}},
	}
	product, err := svc.Write.Create(input)
	if err != nil {
		t.Fatal(err)
	}
	product, err = svc.Read.GetAdminByID(fmt.Sprint(product.ID))
	if err != nil {
		t.Fatal(err)
	}
	first := product.SKUs[0]
	removed := *catalog
	removed.SyncedAt, removed.Options = now.Add(time.Second), []kafudadomain.CatalogOption{}
	if err := store.SaveCatalog(ctx, &removed); err != nil {
		t.Fatal(err)
	}
	catalog.SyncedAt = now.Add(2 * time.Second)
	if err := store.SaveCatalog(ctx, catalog); err != nil {
		t.Fatal(err)
	}
	if _, err := policy.PurchaseTerms(ctx, "plus", "PH", first.DirectCatalogRevision); !errors.Is(err, kafudaapp.ErrQuoteUnavailable) {
		t.Fatalf("catalog restoration accepted old approval: %v", err)
	}
	// The pre-removal editor cannot silently acknowledge the new revision.
	if _, err := svc.Write.Update(fmt.Sprint(product.ID), input); !errors.Is(err, productcontract.ErrProductSKUInvalid) {
		t.Fatalf("legacy stale editor re-enabled variant: %v", err)
	}
	oldRevision := uint64(0)
	input.SKUs[0].DirectCatalogRevision = &oldRevision
	if _, err := svc.Write.Update(fmt.Sprint(product.ID), input); !errors.Is(err, productcontract.ErrProductSKUInvalid) {
		t.Fatalf("stale revision re-enabled variant: %v", err)
	}
	input.SKUs = nil
	if _, err := svc.Write.Update(fmt.Sprint(product.ID), input); err != nil {
		t.Fatal(err)
	}
	product, err = svc.Read.GetAdminByID(fmt.Sprint(product.ID))
	if err != nil || product.SKUs[0].IsActive {
		t.Fatalf("catalog-only save re-enabled variant: %+v, %v", product, err)
	}
	options, err := policy.SKUOptions(ctx)
	if err != nil || len(options) != 1 {
		t.Fatalf("review options missing: %+v, %v", options, err)
	}
	revision := options[0].CatalogRevision
	if revision != 1 {
		t.Fatalf("revision = %d, want 1", revision)
	}
	input.SKUs = []productwrite.ProductSKUInput{{SKUCode: first.SKUCode, PriceAmount: decimal.NewFromInt(100), IsActive: &active, DirectCatalogRevision: &revision, SortOrder: 23}}
	if _, err := svc.Write.Update(fmt.Sprint(product.ID), input); err != nil {
		t.Fatal(err)
	}
	input.SKUs = nil
	if _, err := svc.Write.Update(fmt.Sprint(product.ID), input); err != nil {
		t.Fatal(err)
	}
	product, err = svc.Read.GetAdminByID(fmt.Sprint(product.ID))
	if err != nil {
		t.Fatal(err)
	}
	sku := product.SKUs[0]
	if !sku.IsActive || sku.DirectCatalogRevision != 1 || sku.ID != first.ID || !sku.CostPriceAmount.Equal(decimal.NewFromInt(45)) || !sku.PriceAmount.Equal(decimal.NewFromInt(100)) || sku.SortOrder != 23 {
		t.Fatalf("review lost configuration: %+v", sku)
	}
	if _, err := policy.PurchaseTerms(ctx, "plus", "PH", sku.DirectCatalogRevision); err != nil {
		t.Fatalf("reviewed variant rejected: %v", err)
	}
}
