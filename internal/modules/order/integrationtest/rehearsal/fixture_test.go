package rehearsal_test

import (
	"context"
	"crypto/rand"
	"testing"
	"time"

	"github.com/dujiao-next/internal/constants"
	categorydomain "github.com/dujiao-next/internal/modules/catalog/category/domain"
	productdomain "github.com/dujiao-next/internal/modules/catalog/product/domain"
	productstore "github.com/dujiao-next/internal/modules/catalog/product/store/gormstore"
	fulfillmentdomain "github.com/dujiao-next/internal/modules/fulfillment/domain"
	userdomain "github.com/dujiao-next/internal/modules/identity/user/domain"
	userstore "github.com/dujiao-next/internal/modules/identity/user/infrastructure/gormstore"
	kafudaapp "github.com/dujiao-next/internal/modules/kafudagpt/application"
	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
	kafudastore "github.com/dujiao-next/internal/modules/kafudagpt/infrastructure/gormstore"
	orderapp "github.com/dujiao-next/internal/modules/order/application"
	orderdomain "github.com/dujiao-next/internal/modules/order/domain"
	orderstore "github.com/dujiao-next/internal/modules/order/infrastructure/gormstore"
	promotiondomain "github.com/dujiao-next/internal/modules/promotion/domain"
	promotionstore "github.com/dujiao-next/internal/modules/promotion/infrastructure/gormstore"
	resellercontract "github.com/dujiao-next/internal/modules/reseller/contract"
	"github.com/dujiao-next/internal/shared/jsonmap"
	"github.com/dujiao-next/internal/shared/money"
	"github.com/glebarez/sqlite"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

type directCheckoutFixture struct {
	db      *gorm.DB
	product productdomain.Product
	sku     productdomain.ProductSKU
	users   []userdomain.User
	options orderapp.OrderServiceOptions
	store   *kafudastore.Store
	tenant  resellercontract.TenantContext
}

type rehearsalOrderQueue struct{}

func (rehearsalOrderQueue) Enabled() bool                                  { return true }
func (rehearsalOrderQueue) EnqueueTimeoutCancel(uint, time.Duration) error { return nil }
func (rehearsalOrderQueue) EnqueueStatusEmail(uint, string) error          { return nil }

func newDirectCheckoutFixture(t *testing.T) directCheckoutFixture {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = pool.Close() })
	if err := db.AutoMigrate(&userdomain.User{}, &categorydomain.Category{}, &productdomain.Product{}, &productdomain.ProductSKU{},
		&orderdomain.Order{}, &orderdomain.OrderItem{}, &fulfillmentdomain.Fulfillment{}, &promotiondomain.Promotion{},
		&kafudadomain.Catalog{}, &kafudadomain.Recharge{}); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	const profile, card, plan, country = "rehearsal-profile", "rehearsal-card", "plus", "PH"
	store := kafudastore.New(db)
	catalog := &kafudadomain.Catalog{
		Profile: profile, Enabled: true, SyncedAt: now, ExpiresAt: now.Add(2 * time.Hour),
		Options: []kafudadomain.CatalogOption{{Plan: plan, Country: country, Product: "gpt", Available: true}},
		Cards:   []kafudadomain.CardProduct{{ProductCode: card}},
	}
	if err := store.SaveCatalog(t.Context(), catalog); err != nil {
		t.Fatal(err)
	}
	category := categorydomain.Category{Slug: "direct-rehearsal", NameJSON: jsonmap.JSON{"zh-CN": "直充演练"}, IsActive: true}
	if err := db.Create(&category).Error; err != nil {
		t.Fatal(err)
	}
	price := money.FromDecimal(decimal.NewFromInt(100))
	product := productdomain.Product{
		CategoryID: category.ID, Slug: "direct-rehearsal-product", TitleJSON: jsonmap.JSON{"zh-CN": "直充演练 · Plus · 菲律宾"},
		PriceAmount: price, PurchaseType: constants.ProductPurchaseMember, FulfillmentType: constants.FulfillmentTypeKafudaGPT, IsActive: true,
	}
	if err := db.Create(&product).Error; err != nil {
		t.Fatal(err)
	}
	sku := productdomain.ProductSKU{
		ProductID: product.ID, SKUCode: kafudacontract.SKUCode(plan, country), SpecValuesJSON: jsonmap.JSON{"plan": plan, "country": country},
		PriceAmount: price, CostPriceAmount: money.FromDecimal(decimal.NewFromInt(45)), DirectCostConfigured: true,
		DirectCatalogRevision: catalog.Revision(plan, country), IsActive: true,
	}
	if err := db.Create(&sku).Error; err != nil {
		t.Fatal(err)
	}
	users := []userdomain.User{
		{Email: "direct-rehearsal-buyer@example.test", PasswordHash: "fixture", Status: "active"},
		{Email: "direct-rehearsal-other@example.test", PasswordHash: "fixture", Status: "active"},
	}
	if err := db.Create(&users).Error; err != nil {
		t.Fatal(err)
	}
	return directCheckoutFixture{
		db: db, product: product, sku: sku, users: users, store: store, tenant: resellercontract.MainTenantContext("rehearsal.example.test"),
		options: orderapp.OrderServiceOptions{
			DirectCatalog: kafudaapp.NewCommerceService(kafudaapp.CommerceOptions{
				Catalog: kafudaapp.NewCatalogService(store, nil, profile, 2*time.Hour), SalesEnabled: true, ReferenceLookupVerified: true,
				ProductCode: card, MaxCostCents: 2500, AllowedKinds: []string{"new"}, WorkerReady: func(context.Context) bool { return true },
			}),
			OrderStore: orderstore.New(db, rand.Text()), UserStore: userstore.New(db),
			ProductStore: productstore.NewProductStore(db), ProductSKUStore: productstore.NewSKUStore(db),
			PromotionRepo: promotionstore.New(db), Queue: rehearsalOrderQueue{},
		},
	}
}
