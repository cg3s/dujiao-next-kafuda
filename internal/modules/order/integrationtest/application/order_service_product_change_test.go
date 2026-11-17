package application_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/dujiao-next/internal/constants"
	cardsecretdomain "github.com/dujiao-next/internal/modules/cardsecret/domain"
	cardsecretgormstore "github.com/dujiao-next/internal/modules/cardsecret/infrastructure/gormstore"
	productwrite "github.com/dujiao-next/internal/modules/catalog/product/application/write"
	productdomain "github.com/dujiao-next/internal/modules/catalog/product/domain"
	productgormstore "github.com/dujiao-next/internal/modules/catalog/product/store/gormstore"
	coupondomain "github.com/dujiao-next/internal/modules/coupon/domain"
	usergormstore "github.com/dujiao-next/internal/modules/identity/user/infrastructure/gormstore"
	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	orderapplication "github.com/dujiao-next/internal/modules/order/application"
	ordercontract "github.com/dujiao-next/internal/modules/order/contract"
	orderdomain "github.com/dujiao-next/internal/modules/order/domain"
	ordergormstore "github.com/dujiao-next/internal/modules/order/infrastructure/gormstore"
	promotiongormstore "github.com/dujiao-next/internal/modules/promotion/infrastructure/gormstore"
	"github.com/shopspring/decimal"
)

type orderProductChangeStore struct {
	ordercontract.Store
	before func()
}

func (s orderProductChangeStore) WithinTransaction(fn func(ordercontract.Transaction) error) error {
	s.before()
	return s.Store.WithinTransaction(fn)
}

type orderProductChangeCatalog struct{ kafudacontract.CommercePolicy }

func (orderProductChangeCatalog) SKUOptions(context.Context) ([]kafudacontract.SKUOption, error) {
	return []kafudacontract.SKUOption{{SKUCode: kafudacontract.SKUCode("plus", "PH"), Plan: "plus", Country: "PH", Available: true}}, nil
}

func TestCreateOrderRejectsProductChangesAfterSnapshot(t *testing.T) {
	for _, change := range []string{"direct_conversion", "product_disabled", "sku_disabled", "sku_deleted"} {
		t.Run(change, func(t *testing.T) {
			f := newCouponLimitRaceFixture(t, "product-change-"+change, 0, 0)
			if err := f.db.AutoMigrate(&cardsecretdomain.Secret{}); err != nil {
				t.Fatal(err)
			}
			orders := ordergormstore.New(f.db, "test-guest-credential-secret-with-32-bytes")
			products := productgormstore.NewProductStore(f.db)
			skus := productgormstore.NewSKUStore(f.db)
			hooked := orderProductChangeStore{Store: orders, before: func() {
				switch change {
				case "direct_conversion":
					// The order has already read the manual SKU. Remove its unlimited
					// stock setting, then convert through the real product write path.
					if err := f.db.Model(&f.sku).Update("manual_stock_total", 0).Error; err != nil {
						t.Fatal(err)
					}
					writer := productwrite.NewWriteService(productwrite.Options{Products: products, SKUs: skus, CardSecrets: cardsecretgormstore.New(f.db), Orders: orders})
					writer.SetDirectCatalog(orderProductChangeCatalog{})
					updated, err := writer.Update(fmt.Sprint(f.product.ID), productwrite.CreateProductInput{CategoryID: f.product.CategoryID, Slug: f.product.Slug, TitleJSON: f.product.TitleJSON, FulfillmentType: constants.FulfillmentTypeKafudaGPT, PriceAmount: decimal.NewFromInt(100)})
					if err != nil || updated.FulfillmentType != constants.FulfillmentTypeKafudaGPT {
						t.Fatalf("convert before order transaction: %+v, %v", updated, err)
					}
				case "product_disabled":
					if err := f.db.Model(&f.product).Update("is_active", false).Error; err != nil {
						t.Fatal(err)
					}
				case "sku_disabled":
					if err := f.db.Model(&f.sku).Update("is_active", false).Error; err != nil {
						t.Fatal(err)
					}
				case "sku_deleted":
					if err := skus.Delete(f.sku.ID); err != nil {
						t.Fatal(err)
					}
				}
			}}
			svc := orderapplication.NewOrderService(orderapplication.OrderServiceOptions{OrderStore: hooked, UserStore: usergormstore.New(f.db), ProductStore: products, ProductSKUStore: skus, CouponStore: f.couponRepo, CouponUsageStore: f.usageRepo, PromotionRepo: promotiongormstore.New(f.db), Queue: f.queue, ExpireMinutes: 15})
			_, err := svc.CreateOrder(f.input(f.users[0].ID))
			if !errors.Is(err, orderapplication.ErrProductNotAvailable) {
				t.Fatalf("expected stale snapshot rejection, got %v", err)
			}
			for _, model := range []interface{}{&orderdomain.Order{}, &orderdomain.OrderItem{}, &coupondomain.CouponUsage{}} {
				var count int64
				if err := f.db.Model(model).Count(&count).Error; err != nil {
					t.Fatal(err)
				}
				if count != 0 {
					t.Fatalf("stale order persisted %T rows: %d", model, count)
				}
			}
			var remaining []productdomain.ProductSKU
			if err := f.db.Unscoped().Where("product_id = ?", f.product.ID).Find(&remaining).Error; err != nil {
				t.Fatal(err)
			}
			for _, sku := range remaining {
				if sku.ManualStockLocked != 0 || sku.ManualStockSold != 0 {
					t.Fatal("stale order reserved inventory")
				}
			}
			if f.queue.enqueued.Load() != 0 {
				t.Fatal("stale order enqueued a timeout task")
			}
		})
	}
}
