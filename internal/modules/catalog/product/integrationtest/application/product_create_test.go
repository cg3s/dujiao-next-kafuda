package integrationtest

import (
	"context"
	"errors"
	"fmt"
	"testing"

	productwrite "github.com/dujiao-next/internal/modules/catalog/product/application/write"
	productcontract "github.com/dujiao-next/internal/modules/catalog/product/contract"
	productdomain "github.com/dujiao-next/internal/modules/catalog/product/domain"

	categorydomain "github.com/dujiao-next/internal/modules/catalog/category/domain"

	"github.com/dujiao-next/internal/constants"
	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
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

func (*directProductTestCatalog) PurchaseTerms(context.Context, string, string) (*kafudacontract.PurchaseTerms, error) {
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
