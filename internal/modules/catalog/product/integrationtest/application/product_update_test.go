package integrationtest

import (
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"testing"

	cardsecretdomain "github.com/dujiao-next/internal/modules/cardsecret/domain"
	cardsecretgormstore "github.com/dujiao-next/internal/modules/cardsecret/infrastructure/gormstore"
	productwrite "github.com/dujiao-next/internal/modules/catalog/product/application/write"
	productcontract "github.com/dujiao-next/internal/modules/catalog/product/contract"
	productdomain "github.com/dujiao-next/internal/modules/catalog/product/domain"
	productgormstore "github.com/dujiao-next/internal/modules/catalog/product/store/gormstore"
	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	ordercontract "github.com/dujiao-next/internal/modules/order/contract"
	orderdomain "github.com/dujiao-next/internal/modules/order/domain"
	ordergormstore "github.com/dujiao-next/internal/modules/order/infrastructure/gormstore"

	categorydomain "github.com/dujiao-next/internal/modules/catalog/category/domain"

	"github.com/dujiao-next/internal/constants"
	"github.com/dujiao-next/internal/shared/jsonmap"
	"github.com/dujiao-next/internal/shared/money"
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
