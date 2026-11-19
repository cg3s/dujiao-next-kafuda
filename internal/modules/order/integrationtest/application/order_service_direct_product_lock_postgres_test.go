//go:build integration

package application_test

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dujiao-next/internal/constants"
	cardsecretdomain "github.com/dujiao-next/internal/modules/cardsecret/domain"
	cardsecretstore "github.com/dujiao-next/internal/modules/cardsecret/infrastructure/gormstore"
	productwrite "github.com/dujiao-next/internal/modules/catalog/product/application/write"
	productcontract "github.com/dujiao-next/internal/modules/catalog/product/contract"
	productstore "github.com/dujiao-next/internal/modules/catalog/product/store/gormstore"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
	orderapp "github.com/dujiao-next/internal/modules/order/application"
	ordercontract "github.com/dujiao-next/internal/modules/order/contract"
	orderdomain "github.com/dujiao-next/internal/modules/order/domain"
	orderstore "github.com/dujiao-next/internal/modules/order/infrastructure/gormstore"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

func TestPostgresDirectRegionalSKUStopPreservesPaidSnapshot(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is empty")
	}
	db := openPostgresDirectRaceDB(t, dsn)
	testDirectRegionalSKUStopPreservesPaidSnapshot(t, seedDirectCheckoutFixture(t, seedCouponLimitRaceFixture(t, db, "direct-regional-stop", 0, 0)))
}

func TestPostgresDirectCheckoutAndTypeSwitchShareProductLock(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is empty")
	}
	for _, first := range []string{"checkout", "type_switch"} {
		t.Run(first+"_wins", func(t *testing.T) {
			db := openPostgresDirectRaceDB(t, dsn)
			f := seedDirectCheckoutFixture(t, seedCouponLimitRaceFixture(t, db, "direct-product-lock", 0, 0))
			if err := db.AutoMigrate(&cardsecretdomain.Secret{}); err != nil {
				t.Fatal(err)
			}
			f.input.CouponCode = f.coupon.Code

			lockPIDs := observeProductLockPIDs(t, db)

			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			orders := orderstore.New(db.WithContext(ctx), "test-guest-credential-secret-with-32-bytes")
			beforeCommit, release := make(chan struct{}), make(chan struct{})
			commit := sync.OnceFunc(func() { close(release) })
			defer commit()
			heldOrders := directProductLockStore{Store: orders, beforeCommit: beforeCommit, release: release}

			checkoutOrders, editOrders := ordercontract.Store(orders), ordercontract.Store(orders)
			if first == "checkout" {
				checkoutOrders = heldOrders
			} else {
				editOrders = heldOrders
			}
			f.options.OrderStore = checkoutOrders
			writer := productwrite.NewWriteService(productwrite.Options{
				Products: productstore.NewProductStore(db), SKUs: productstore.NewSKUStore(db),
				CardSecrets: cardsecretstore.New(db), Orders: editOrders,
			})
			id := strconv.FormatUint(uint64(f.product.ID), 10)
			checkout := func() error {
				_, err := orderapp.NewOrderService(f.options).CreateOrder(f.input)
				return err
			}
			switchType := func() error {
				_, err := writer.Update(id, productwrite.CreateProductInput{
					CategoryID: f.product.CategoryID, Slug: f.product.Slug, TitleJSON: f.product.TitleJSON,
					FulfillmentType: constants.FulfillmentTypeManual, PriceAmount: f.product.PriceAmount.Decimal,
				})
				return err
			}
			leader, follower := checkout, switchType
			wantRejection := productcontract.ErrFulfillmentInvalid
			if first == "type_switch" {
				leader, follower = switchType, checkout
				wantRejection = orderapp.ErrProductNotAvailable
			}
			firstResult, secondResult := make(chan error, 1), make(chan error, 1)
			go func() { firstResult <- leader() }()
			select {
			case <-beforeCommit:
			case err := <-firstResult:
				t.Fatalf("first write did not reach commit barrier: %v", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			var holder int
			select {
			case holder = <-lockPIDs:
			case <-ctx.Done():
				t.Fatal("first write did not issue a product lock query")
			}
			go func() { secondResult <- follower() }()
			var waiter int
			select {
			case waiter = <-lockPIDs:
			case err := <-secondResult:
				t.Fatalf("second write returned before taking the product lock: %v", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if holder == waiter {
				t.Fatal("both product writes used the same PostgreSQL connection")
			}
			assertProductLockBlocked(t, ctx, db, waiter, holder)
			commit()
			if err := <-firstResult; err != nil {
				t.Fatalf("first write failed after releasing the barrier: %v", err)
			}
			if err := <-secondResult; !errors.Is(err, wantRejection) {
				t.Fatalf("second write error = %v, want %v", err, wantRejection)
			}

			product, err := productstore.NewProductStore(db).GetByID(id)
			if err != nil || product == nil {
				t.Fatalf("load product: %v", err)
			}
			if first == "type_switch" {
				if product.FulfillmentType != constants.FulfillmentTypeManual {
					t.Fatalf("type switch did not persist: %s", product.FulfillmentType)
				}
				assertNoDirectCheckoutRows(t, f)
				return
			}
			if product.FulfillmentType != constants.FulfillmentTypeKafudaGPT || f.queue.enqueued.Load() != 1 {
				t.Fatalf("successful checkout changed the product type or the number of timeout jobs")
			}
			var item orderdomain.OrderItem
			if err := db.Where("product_id = ?", product.ID).First(&item).Error; err != nil {
				t.Fatal(err)
			}
			recharge, err := f.store.GetRecharge(t.Context(), item.OrderID)
			if err != nil || recharge == nil || recharge.Snapshot.SKUID != f.sku.ID ||
				recharge.Snapshot.Plan != "plus" || recharge.Snapshot.Country != "PH" ||
				recharge.Snapshot.SellPrice.String() != "90.00" || recharge.Snapshot.CostPrice.String() != "45.00" {
				t.Fatalf("successful checkout lost its snapshot: %+v, %v", recharge, err)
			}
		})
	}
}

func TestPostgresDirectCatalogOnlyEditPreservesConcurrentSKUChanges(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is empty")
	}
	db := openPostgresDirectRaceDB(t, dsn)
	f := seedDirectCheckoutFixture(t, seedCouponLimitRaceFixture(t, db, "direct-catalog-merge-lock", 0, 0))
	if err := db.Model(&f.product).UpdateColumn("is_active", false).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&f.sku).UpdateColumns(map[string]any{"is_active": false, "direct_cost_configured": false, "cost_price_amount": 0}).Error; err != nil {
		t.Fatal(err)
	}
	lockPIDs := observeProductLockPIDs(t, db)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	beforeCommit, release := make(chan struct{}), make(chan struct{})
	commit := sync.OnceFunc(func() { close(release) })
	var writes sync.WaitGroup
	defer func() {
		cancel()
		commit()
		writes.Wait()
	}()

	scopedDB := db.WithContext(ctx)
	orders := orderstore.New(scopedDB, "test-guest-credential-secret-with-32-bytes")
	products, skus := productstore.NewProductStore(scopedDB), productstore.NewSKUStore(scopedDB)
	catalog := directCheckoutCommerce(f.store, true)
	options, err := catalog.SKUOptions(ctx)
	if err != nil || len(options) != 1 || !options[0].Available {
		t.Fatalf("expected seeded direct SKU option: %+v, %v", options, err)
	}
	option := options[0]
	newWriter := func(orders ordercontract.Store) *productwrite.WriteService {
		writer := productwrite.NewWriteService(productwrite.Options{Products: products, SKUs: skus, Orders: orders})
		writer.SetDirectCatalog(catalog)
		return writer
	}
	leader := newWriter(directProductLockStore{Store: orders, beforeCommit: beforeCommit, release: release})
	follower := newWriter(orders)
	id := strconv.FormatUint(uint64(f.product.ID), 10)
	base := productwrite.CreateProductInput{
		CategoryID: f.product.CategoryID, Slug: f.product.Slug, TitleJSON: f.product.TitleJSON,
		FulfillmentType: constants.FulfillmentTypeKafudaGPT,
	}
	active := true
	price, cost := decimal.NewFromInt(129), decimal.NewFromInt(58)
	const sortOrder = 17
	priceEdit := base
	priceEdit.IsActive = &active
	priceEdit.SKUs = []productwrite.ProductSKUInput{{
		ID: f.sku.ID, SKUCode: option.SKUCode, PriceAmount: price, CostPriceAmount: cost,
		CostPriceProvided: true, IsActive: &active, SortOrder: sortOrder, DirectCatalogRevision: &option.CatalogRevision,
	}}
	const title = "Updated while catalog merge waited"
	catalogOnly := base
	catalogOnly.TitleJSON = map[string]any{"en-US": title}

	leaderResult, followerResult := make(chan error, 1), make(chan error, 1)
	writes.Go(func() {
		_, err := leader.Update(id, priceEdit)
		leaderResult <- err
	})
	select {
	case <-beforeCommit:
	case err := <-leaderResult:
		t.Fatalf("price update did not reach commit barrier: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	var holder int
	select {
	case holder = <-lockPIDs:
	case <-ctx.Done():
		t.Fatal("price update did not issue a product lock query")
	}
	writes.Go(func() {
		_, err := follower.Update(id, catalogOnly)
		followerResult <- err
	})
	var waiter int
	select {
	case waiter = <-lockPIDs:
	case err := <-followerResult:
		t.Fatalf("catalog edit returned before taking the product lock: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if holder == waiter {
		t.Fatal("both product updates used the same PostgreSQL connection")
	}
	assertProductLockBlocked(t, ctx, db, waiter, holder)
	commit()
	for _, write := range []struct {
		name   string
		result <-chan error
	}{{"price update", leaderResult}, {"catalog edit", followerResult}} {
		select {
		case err := <-write.result:
			if err != nil {
				t.Fatalf("%s failed: %v", write.name, err)
			}
		case <-ctx.Done():
			t.Fatalf("%s did not finish: %v", write.name, ctx.Err())
		}
	}

	product, err := products.GetAdminByID(id)
	if err != nil || product == nil || len(product.SKUs) != 1 {
		t.Fatalf("expected one persisted variant: %+v, %v", product, err)
	}
	if product.TitleJSON["en-US"] != title || !product.IsActive ||
		!product.PriceAmount.Equal(price) || !product.CostPriceAmount.Equal(cost) {
		t.Fatalf("catalog edit did not preserve the concurrent product activation and price aggregates: %+v", product)
	}
	sku := product.SKUs[0]
	if sku.ID != f.sku.ID || sku.SKUCode != option.SKUCode || !sku.IsActive || !sku.DirectCostConfigured ||
		!sku.PriceAmount.Equal(price) || !sku.CostPriceAmount.Equal(cost) || sku.SortOrder != sortOrder ||
		sku.DirectCatalogRevision != option.CatalogRevision {
		t.Fatalf("catalog merge overwrote the configured SKU: %+v", sku)
	}
}

func TestPostgresDirectActivationRejectsCatalogRevisionChangedWhileWaiting(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is empty")
	}
	db := openPostgresDirectRaceDB(t, dsn)
	f := seedDirectCheckoutFixture(t, seedCouponLimitRaceFixture(t, db, "direct-catalog-revision-lock", 0, 0))
	if err := db.Model(&f.product).UpdateColumn("is_active", false).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&f.sku).UpdateColumn("is_active", false).Error; err != nil {
		t.Fatal(err)
	}
	lockPIDs := observeProductLockPIDs(t, db)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	beforeCommit, release := make(chan struct{}), make(chan struct{})
	commit := sync.OnceFunc(func() { close(release) })
	var writes sync.WaitGroup
	defer func() {
		cancel()
		commit()
		writes.Wait()
	}()
	scopedDB := db.WithContext(ctx)
	orders := orderstore.New(scopedDB, "test-guest-credential-secret-with-32-bytes")
	products := productstore.NewProductStore(scopedDB)
	productID := strconv.FormatUint(uint64(f.product.ID), 10)
	before, err := products.GetAdminByID(productID)
	if err != nil || before == nil || len(before.SKUs) != 1 {
		t.Fatalf("expected persisted direct product: %+v, %v", before, err)
	}
	sku := before.SKUs[0]
	catalog := directCheckoutCommerce(f.store, true)
	options, err := catalog.SKUOptions(ctx)
	if err != nil || len(options) != 1 || !options[0].Available || options[0].CatalogRevision != sku.DirectCatalogRevision {
		t.Fatalf("expected available option for original revision: %+v, %v", options, err)
	}
	oldRevision := options[0].CatalogRevision
	writer := productwrite.NewWriteService(productwrite.Options{
		Products: products, SKUs: productstore.NewSKUStore(scopedDB), Orders: orders,
	})
	writer.SetDirectCatalog(catalog)
	active := true
	const title = "Approved after catalog restoration"
	price, cost := decimal.NewFromInt(129), decimal.NewFromInt(58)
	input := productwrite.CreateProductInput{
		CategoryID: f.product.CategoryID, Slug: f.product.Slug, TitleJSON: map[string]any{"en-US": title},
		FulfillmentType: constants.FulfillmentTypeKafudaGPT, IsActive: &active,
		SKUs: []productwrite.ProductSKUInput{{
			ID: sku.ID, SKUCode: sku.SKUCode, PriceAmount: price, CostPriceAmount: cost,
			CostPriceProvided: true, IsActive: &active, DirectCatalogRevision: &oldRevision,
		}},
	}

	holderResult, writerResult := make(chan error, 1), make(chan error, 1)
	heldOrders := directProductLockStore{Store: orders, beforeCommit: beforeCommit, release: release}
	writes.Go(func() {
		holderResult <- heldOrders.WithinTransaction(func(tx ordercontract.Transaction) error {
			return tx.Products().LockByIDs([]uint{f.product.ID})
		})
	})
	select {
	case <-beforeCommit:
	case err := <-holderResult:
		t.Fatalf("holder returned before taking the product lock: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	var holder int
	select {
	case holder = <-lockPIDs:
	case <-ctx.Done():
		t.Fatal("holder did not issue a product lock query")
	}
	writes.Go(func() {
		_, err := writer.Update(productID, input)
		writerResult <- err
	})
	var waiter int
	select {
	case waiter = <-lockPIDs:
	case err := <-writerResult:
		t.Fatalf("activation returned before taking the product lock: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if holder == waiter {
		t.Fatal("activation did not use an independent PostgreSQL connection")
	}
	assertProductLockBlocked(t, ctx, db, waiter, holder)

	removed, restored := *f.catalog, *f.catalog
	removed.SyncedAt, removed.Options = f.catalog.SyncedAt.Add(time.Second), []kafudadomain.CatalogOption{}
	restored.SyncedAt = removed.SyncedAt.Add(time.Second)
	for _, snapshot := range []*kafudadomain.Catalog{&removed, &restored} {
		if err := f.store.SaveCatalog(ctx, snapshot); err != nil {
			t.Fatalf("refresh catalog while activation waits: %v", err)
		}
	}
	options, err = catalog.SKUOptions(ctx)
	if err != nil || len(options) != 1 || !options[0].Available || options[0].CatalogRevision != oldRevision+1 {
		t.Fatalf("restored option did not receive a new approval revision: %+v, %v", options, err)
	}
	commit()
	select {
	case err := <-holderResult:
		if err != nil {
			t.Fatalf("product lock holder failed: %v", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case err := <-writerResult:
		if !errors.Is(err, productcontract.ErrProductSKUInvalid) {
			t.Fatalf("activation with old catalog revision returned %v", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	unchanged, err := products.GetAdminByID(productID)
	if err != nil || unchanged == nil || len(unchanged.SKUs) != 1 || unchanged.IsActive ||
		unchanged.TitleJSON["en-US"] == title || !unchanged.PriceAmount.Equal(before.PriceAmount.Decimal) ||
		!unchanged.CostPriceAmount.Equal(before.CostPriceAmount.Decimal) {
		t.Fatalf("rejected activation changed product: %+v, %v", unchanged, err)
	}
	got := unchanged.SKUs[0]
	if got.ID != sku.ID || got.IsActive || got.DirectCatalogRevision != oldRevision ||
		!got.PriceAmount.Equal(sku.PriceAmount.Decimal) || !got.CostPriceAmount.Equal(sku.CostPriceAmount.Decimal) {
		t.Fatalf("rejected activation changed original SKU: %+v", got)
	}

	currentRevision := options[0].CatalogRevision
	input.SKUs[0].DirectCatalogRevision = &currentRevision
	approved, err := writer.Update(productID, input)
	if err != nil || approved == nil || !approved.IsActive || approved.TitleJSON["en-US"] != title || len(approved.SKUs) != 1 {
		t.Fatalf("activation with current revision failed: %+v, %v", approved, err)
	}
	got = approved.SKUs[0]
	if got.ID != sku.ID || !got.IsActive || got.DirectCatalogRevision != currentRevision ||
		!got.PriceAmount.Equal(price) || !got.CostPriceAmount.Equal(cost) {
		t.Fatalf("current revision did not preserve and update the original SKU: %+v", got)
	}
}

func observeProductLockPIDs(t *testing.T, db *gorm.DB) <-chan int {
	t.Helper()
	// Report the backend PID just before each SELECT ... FOR UPDATE.
	lockPIDs := make(chan int, 2)
	if err := db.Callback().Query().Before("gorm:query").Register("test:product_lock_pid", func(tx *gorm.DB) {
		if _, locking := tx.Statement.Clauses["FOR"]; tx.Statement.Table != "products" || !locking {
			return
		}
		var pid int
		if err := tx.Statement.ConnPool.QueryRowContext(tx.Statement.Context, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
			tx.AddError(err)
			return
		}
		select {
		case lockPIDs <- pid:
		case <-tx.Statement.Context.Done():
			tx.AddError(tx.Statement.Context.Err())
		}
	}); err != nil {
		t.Fatal(err)
	}
	return lockPIDs
}

type directProductLockStore struct {
	ordercontract.Store
	beforeCommit chan<- struct{}
	release      <-chan struct{}
}

func (s directProductLockStore) WithinTransaction(run func(ordercontract.Transaction) error) error {
	return s.Store.WithinTransaction(func(tx ordercontract.Transaction) error {
		if err := run(tx); err != nil {
			return err
		}
		close(s.beforeCommit)
		<-s.release
		return nil
	})
}

func assertProductLockBlocked(t *testing.T, ctx context.Context, db *gorm.DB, waiter, holder int) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		if err := db.WithContext(ctx).Raw("SELECT ? = ANY(pg_blocking_pids(?))", holder, waiter).Scan(&blocked).Error; err != nil {
			t.Fatalf("observe blocker: %v", err)
		}
		if blocked {
			return
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal("the second write did not wait for the first product lock")
		}
	}
}
