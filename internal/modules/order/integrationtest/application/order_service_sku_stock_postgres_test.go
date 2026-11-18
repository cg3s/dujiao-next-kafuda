//go:build integration

package application_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dujiao-next/internal/constants"
	cardsecretdomain "github.com/dujiao-next/internal/modules/cardsecret/domain"
	productcontract "github.com/dujiao-next/internal/modules/catalog/product/contract"
	productdomain "github.com/dujiao-next/internal/modules/catalog/product/domain"
	productstore "github.com/dujiao-next/internal/modules/catalog/product/store/gormstore"
	orderapp "github.com/dujiao-next/internal/modules/order/application"
	ordercontract "github.com/dujiao-next/internal/modules/order/contract"
	orderstore "github.com/dujiao-next/internal/modules/order/infrastructure/gormstore"
	paymentdomain "github.com/dujiao-next/internal/modules/payment/domain"

	"gorm.io/gorm"
)

func TestPostgresManualOrderCannotReopenAfterSKUDeletion(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is empty")
	}
	db := openPostgresDirectRaceDB(t, dsn)
	f := seedCouponLimitRaceFixture(t, db, "manual-sku-reopen", 0, 0)
	if err := db.AutoMigrate(&cardsecretdomain.Secret{}, &paymentdomain.Payment{}); err != nil {
		t.Fatal(err)
	}
	service := f.service(f.couponRepo, f.usageRepo)
	order, err := service.CreateOrder(f.input(f.users[0].ID))
	if err != nil || order == nil || len(order.Children) != 1 {
		t.Fatalf("create manual order: %+v, %v", order, err)
	}
	if _, err := service.CancelOrder(order.ID, order.UserID); err != nil {
		t.Fatal(err)
	}
	if err := productstore.NewSKUStore(db).Delete(f.sku.ID); err != nil {
		t.Fatal(err)
	}
	orders := orderstore.New(db, "test-guest-credential-secret-with-32-bytes")
	err = orders.WithinTransaction(func(tx ordercontract.Transaction) error {
		locked, err := tx.Orders().GetByIDForUpdateWithChildren(order.ID)
		if err != nil {
			return err
		}
		return orderapp.ReopenCanceledOrderInTx(tx, locked, time.Now().UTC())
	})
	if !errors.Is(err, orderapp.ErrManualStockInsufficient) {
		t.Fatalf("reopen without original SKU: %v, want insufficient stock", err)
	}
	for _, id := range []uint{order.ID, order.Children[0].ID} {
		record, err := orders.GetByID(id)
		if err != nil || record == nil || record.Status != constants.OrderStatusCanceled || record.CanceledAt == nil || record.PaidAt != nil {
			t.Fatalf("failed reopen changed order %d: %+v, %v", id, record, err)
		}
	}
	usages, err := f.usageRepo.ListByOrderID(order.ID)
	if err != nil || len(usages) != 0 {
		t.Fatalf("failed reopen restored coupon usage: %d, %v", len(usages), err)
	}
}

func TestPostgresManualSKUDeleteRechecksConcurrentReservation(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is empty")
	}
	db := openPostgresDirectRaceDB(t, dsn)
	f := seedCouponLimitRaceFixture(t, db, "manual-sku-delete", 0, 0)
	if err := db.Model(&f.sku).Update("manual_stock_total", 5).Error; err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	reserving := db.WithContext(ctx).Begin()
	if reserving.Error != nil {
		t.Fatal(reserving.Error)
	}
	defer reserving.Rollback()
	var reservingPID int
	if err := reserving.Raw("SELECT pg_backend_pid()").Scan(&reservingPID).Error; err != nil {
		t.Fatal(err)
	}
	if affected, err := productstore.NewSKUStore(reserving).ReserveManualStock(f.sku.ID, 1); err != nil || affected != 1 {
		t.Fatalf("reserve stock: %d, %v", affected, err)
	}

	deletePID := make(chan int, 1)
	deleteResult := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		deleteResult <- db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			var pid int
			if err := tx.Raw("SELECT pg_backend_pid()").Scan(&pid).Error; err != nil {
				return err
			}
			deletePID <- pid
			return productstore.NewSKUStore(tx).Delete(f.sku.ID)
		})
	}()
	defer func() {
		cancel()
		<-done
	}()

	var deletingPID int
	select {
	case deletingPID = <-deletePID:
	case err := <-deleteResult:
		t.Fatalf("delete failed before obtaining a database connection: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		err := db.WithContext(ctx).Raw("SELECT ? = ANY(pg_blocking_pids(?))", reservingPID, deletingPID).Scan(&blocked).Error
		if err != nil {
			t.Fatalf("observe delete lock: %v", err)
		}
		if blocked {
			break
		}
		select {
		case err := <-deleteResult:
			t.Fatalf("delete returned before the reservation committed: %v", err)
		case <-ctx.Done():
			t.Fatalf("delete never waited on reservation: %v", ctx.Err())
		case <-ticker.C:
		}
	}
	if err := reserving.Commit().Error; err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-deleteResult:
		if !errors.Is(err, productcontract.ErrManualStockReserved) {
			t.Fatalf("delete after reservation commit: %v, want reserved stock error", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	var stored productdomain.ProductSKU
	if err := db.First(&stored, f.sku.ID).Error; err != nil || stored.ManualStockTotal != 4 || stored.ManualStockLocked != 1 || stored.DeletedAt != nil {
		t.Fatalf("reserved SKU after blocked delete: %+v, %v", stored, err)
	}
}
