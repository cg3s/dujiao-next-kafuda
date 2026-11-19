package rehearsal_test

import (
	"context"
	"crypto/rand"
	"os"
	"strings"
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
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/shopspring/decimal"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

type directCheckoutFixture struct {
	db          *gorm.DB
	product     productdomain.Product
	sku         productdomain.ProductSKU
	planLabel   string
	users       []userdomain.User
	options     orderapp.OrderServiceOptions
	store       *kafudastore.Store
	commerce    *kafudaapp.CommerceService
	tenant      resellercontract.TenantContext
	adminTokens map[string]string
}

type rehearsalOrderQueue struct{}

func (rehearsalOrderQueue) Enabled() bool                                  { return true }
func (rehearsalOrderQueue) EnqueueTimeoutCancel(uint, time.Duration) error { return nil }
func (rehearsalOrderQueue) EnqueueStatusEmail(uint, string) error          { return nil }

func forEachDirectRehearsalDatabase(t *testing.T, run func(*testing.T, string)) {
	t.Helper()
	t.Run("sqlite", func(t *testing.T) { run(t, "") })
	t.Run("postgres", func(t *testing.T) {
		dsn := strings.TrimSpace(os.Getenv("TEST_POSTGRES_DSN"))
		if dsn == "" {
			t.Skip("TEST_POSTGRES_DSN is empty")
		}
		run(t, dsn)
	})
}

func openDirectRehearsalDatabase(t *testing.T, postgresDSN string) *gorm.DB {
	t.Helper()
	if postgresDSN != "" {
		return openDirectRehearsalPostgres(t, postgresDSN)
	}
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
	return db
}

func openDirectRehearsalPostgres(t *testing.T, dsn string) *gorm.DB {
	t.Helper()
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal("invalid PostgreSQL test DSN")
	}
	admin := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { _ = admin.Close() })

	schema := pgx.Identifier{"kafuda_rehearsal_" + strings.ToLower(rand.Text())}
	if _, err := admin.ExecContext(t.Context(), "CREATE SCHEMA "+schema.Sanitize()); err != nil {
		t.Fatalf("create rehearsal PostgreSQL schema: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := admin.ExecContext(ctx, "DROP SCHEMA "+schema.Sanitize()+" CASCADE"); err != nil {
			t.Errorf("drop rehearsal PostgreSQL schema: %v", err)
		}
	})

	// Set the search path on every connection before GORM can migrate or seed.
	scopedConfig := cfg.Copy()
	scopedConfig.RuntimeParams["search_path"] = schema[0]
	pool := stdlib.OpenDB(*scopedConfig)
	t.Cleanup(func() { _ = pool.Close() })
	pool.SetMaxOpenConns(4)
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: pool}), &gorm.Config{})
	if err != nil {
		t.Fatalf("open rehearsal PostgreSQL schema: %v", err)
	}
	return db
}

func newDirectCheckoutFixture(t *testing.T, productType, postgresDSN string) directCheckoutFixture {
	t.Helper()
	db := openDirectRehearsalDatabase(t, postgresDSN)
	if err := db.AutoMigrate(&userdomain.User{}, &categorydomain.Category{}, &productdomain.Product{}, &productdomain.ProductSKU{},
		&orderdomain.Order{}, &orderdomain.OrderItem{}, &fulfillmentdomain.Fulfillment{}, &promotiondomain.Promotion{},
		&kafudadomain.Catalog{}, &kafudadomain.Recharge{}); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	const profile, card, country = "rehearsal-profile", "rehearsal-card", "PH"
	plan, planLabel := "plus", "Plus"
	if productType == "gpt_credits" {
		plan, planLabel = "credit250", "Codex 250"
	}
	store := kafudastore.New(db)
	catalog := &kafudadomain.Catalog{
		Profile: profile, Enabled: true, SyncedAt: now, ExpiresAt: now.Add(2 * time.Hour),
		Options: []kafudadomain.CatalogOption{{Plan: plan, Country: country, Product: productType, Available: true}},
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
		CategoryID: category.ID, Slug: "direct-rehearsal-product", TitleJSON: jsonmap.JSON{"zh-CN": "直充演练 · " + planLabel + " · 菲律宾"},
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
	commerce := kafudaapp.NewCommerceService(kafudaapp.CommerceOptions{
		Catalog: kafudaapp.NewCatalogService(store, nil, profile, 2*time.Hour), SalesEnabled: true, ReferenceLookupVerified: true,
		ProductCode: card, MaxCostCents: 2500, AllowedKinds: []string{"new"}, WorkerReady: func(context.Context) bool { return true },
	})
	return directCheckoutFixture{
		db: db, product: product, sku: sku, planLabel: planLabel, users: users, store: store, commerce: commerce,
		tenant: resellercontract.MainTenantContext("rehearsal.example.test"),
		options: orderapp.OrderServiceOptions{
			DirectCatalog: commerce,
			OrderStore:    orderstore.New(db, rand.Text()), UserStore: userstore.New(db),
			ProductStore: productstore.NewProductStore(db), ProductSKUStore: productstore.NewSKUStore(db),
			PromotionRepo: promotionstore.New(db), Queue: rehearsalOrderQueue{},
		},
	}
}
