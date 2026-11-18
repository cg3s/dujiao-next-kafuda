//go:build integration

package application_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	productcontract "github.com/dujiao-next/internal/modules/catalog/product/contract"
	productdomain "github.com/dujiao-next/internal/modules/catalog/product/domain"
	productstore "github.com/dujiao-next/internal/modules/catalog/product/store/gormstore"

	"gorm.io/gorm"
)

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
