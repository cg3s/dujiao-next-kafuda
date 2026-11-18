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
	orderapp "github.com/dujiao-next/internal/modules/order/application"
	ordercontract "github.com/dujiao-next/internal/modules/order/contract"
	orderdomain "github.com/dujiao-next/internal/modules/order/domain"
	orderstore "github.com/dujiao-next/internal/modules/order/infrastructure/gormstore"
	"gorm.io/gorm"
)

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

			// Report the backend PID just before either service issues its
			// genuine SELECT ... FOR UPDATE on the shared product row.
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
				lockPIDs <- pid
			}); err != nil {
				t.Fatal(err)
			}

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
