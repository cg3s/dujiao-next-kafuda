package gormstore

import (
	"errors"
	"fmt"
	"testing"
	"time"

	productcontract "github.com/dujiao-next/internal/modules/catalog/product/contract"
	productdomain "github.com/dujiao-next/internal/modules/catalog/product/domain"

	"github.com/dujiao-next/internal/constants"
	"github.com/dujiao-next/internal/shared/jsonmap"
	"github.com/dujiao-next/internal/shared/money"
	"github.com/glebarez/sqlite"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

func setupSKUStoreTest(t *testing.T) (*SKUStore, *gorm.DB) {
	t.Helper()
	dsn := fmt.Sprintf("file:product_sku_repository_%d?mode=memory&cache=shared", time.Now().UnixNano())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite failed: %v", err)
	}
	if err := db.AutoMigrate(&productdomain.ProductSKU{}); err != nil {
		t.Fatalf("migrate product sku failed: %v", err)
	}
	return NewSKUStore(db), db
}

func TestSKUStoreListByProductSortOrderDescending(t *testing.T) {
	repo, _ := setupSKUStoreTest(t)

	high := &productdomain.ProductSKU{
		ProductID:      1,
		SKUCode:        "HIGH",
		PriceAmount:    money.FromDecimal(decimal.NewFromInt(100)),
		IsActive:       true,
		SortOrder:      100,
		SpecValuesJSON: jsonmap.JSON{},
	}
	low := &productdomain.ProductSKU{
		ProductID:      1,
		SKUCode:        "LOW",
		PriceAmount:    money.FromDecimal(decimal.NewFromInt(100)),
		IsActive:       true,
		SortOrder:      1,
		SpecValuesJSON: jsonmap.JSON{},
	}
	if err := repo.Create(high); err != nil {
		t.Fatalf("create high sort sku failed: %v", err)
	}
	if err := repo.Create(low); err != nil {
		t.Fatalf("create low sort sku failed: %v", err)
	}

	rows, err := repo.ListByProduct(1, true)
	if err != nil {
		t.Fatalf("list skus failed: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 skus, got %d", len(rows))
	}
	if rows[0].SKUCode != "HIGH" || rows[1].SKUCode != "LOW" {
		t.Fatalf("expected high sort_order first, got %s then %s", rows[0].SKUCode, rows[1].SKUCode)
	}
}

func TestProductSKUEditPreservesChangesFromOrderStockOperations(t *testing.T) {
	repo, db := setupSKUStoreTest(t)
	sku := &productdomain.ProductSKU{
		ProductID: 1, SKUCode: "CONFIG", IsActive: true,
		PriceAmount:       money.FromDecimal(decimal.NewFromInt(10)),
		ManualStockTotal:  8,
		ManualStockLocked: 2,
		ManualStockSold:   3,
	}
	if err := repo.Create(sku); err != nil {
		t.Fatal(err)
	}
	stale, err := repo.GetByID(sku.ID)
	if err != nil || stale == nil {
		t.Fatalf("read before order updates: %+v, %v", stale, err)
	}
	if affected, err := repo.ReleaseManualStock(sku.ID, 1); err != nil || affected != 1 {
		t.Fatalf("release: %d, %v", affected, err)
	}
	if affected, err := repo.ConsumeManualStock(sku.ID, 1); err != nil || affected != 1 {
		t.Fatalf("consume: %d, %v", affected, err)
	}
	stale.PriceAmount = money.FromDecimal(decimal.NewFromInt(20))
	stale.IsActive = false
	zero := 0
	for _, edit := range []struct {
		explicitTotal *int
		wantTotal     int
	}{
		{nil, 9},
		{&zero, 0},
	} {
		if err := repo.UpdateForProductEdit(stale, edit.explicitTotal); err != nil {
			t.Fatal(err)
		}
		var stored productdomain.ProductSKU
		if err := db.First(&stored, sku.ID).Error; err != nil {
			t.Fatal(err)
		}
		if stored.ManualStockTotal != edit.wantTotal || stored.ManualStockLocked != 0 || stored.ManualStockSold != 4 ||
			stored.IsActive || stored.PriceAmount.String() != "20.00" {
			t.Fatalf("unexpected SKU after edit: %+v", stored)
		}
	}
}

func TestProductSKUEditDoesNotRestoreDeletedSKUs(t *testing.T) {
	for _, mode := range []string{"soft", "hard"} {
		t.Run(mode, func(t *testing.T) {
			repo, _ := setupSKUStoreTest(t)
			sku := &productdomain.ProductSKU{ProductID: 1, SKUCode: "CONFIG"}
			if err := repo.Create(sku); err != nil {
				t.Fatal(err)
			}
			stale, err := repo.GetByID(sku.ID)
			if err != nil || stale == nil {
				t.Fatalf("read before delete: %+v, %v", stale, err)
			}
			if mode == "soft" {
				err = repo.DeleteByProduct(sku.ProductID)
			} else {
				err = repo.Delete(sku.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := repo.UpdateForProductEdit(stale, nil); !errors.Is(err, productcontract.ErrProductSKUInvalid) {
				t.Fatalf("edit after deletion: %v", err)
			}
			if got, err := repo.GetByID(sku.ID); err != nil || got != nil {
				t.Fatalf("deleted SKU reappeared: %+v, %v", got, err)
			}
		})
	}
}

func TestProductSKUManualStockLifecycleMatchesProductSemantics(t *testing.T) {
	repo, db := setupSKUStoreTest(t)
	sku := &productdomain.ProductSKU{
		ProductID:        1,
		SKUCode:          "STOCK-LIFECYCLE",
		PriceAmount:      money.FromDecimal(decimal.NewFromInt(100)),
		ManualStockTotal: 10,
		IsActive:         true,
	}
	if err := repo.Create(sku); err != nil {
		t.Fatalf("create stock sku: %v", err)
	}

	assertAffected := func(operation string, affected int64, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s manual stock: %v", operation, err)
		}
		if affected != 1 {
			t.Fatalf("%s affected rows want 1 got %d", operation, affected)
		}
	}

	affected, err := repo.ReserveManualStock(sku.ID, 3)
	assertAffected("reserve", affected, err)
	affected, err = repo.ConsumeManualStock(sku.ID, 2)
	assertAffected("consume", affected, err)
	affected, err = repo.ReleaseManualStock(sku.ID, 1)
	assertAffected("release", affected, err)

	var reloaded productdomain.ProductSKU
	if err := db.First(&reloaded, sku.ID).Error; err != nil {
		t.Fatalf("reload stock sku: %v", err)
	}
	if reloaded.ManualStockTotal != 8 || reloaded.ManualStockLocked != 0 || reloaded.ManualStockSold != 2 {
		t.Fatalf("unexpected stock lifecycle result: %#v", reloaded)
	}

	unlimited := &productdomain.ProductSKU{
		ProductID:        1,
		SKUCode:          "STOCK-UNLIMITED",
		PriceAmount:      money.FromDecimal(decimal.NewFromInt(100)),
		ManualStockTotal: constants.ManualStockUnlimited,
		IsActive:         true,
	}
	if err := repo.Create(unlimited); err != nil {
		t.Fatalf("create unlimited sku: %v", err)
	}
	if affected, err := repo.ReserveManualStock(unlimited.ID, 1); err != nil || affected != 0 {
		t.Fatalf("unlimited reserve should be no-op, affected=%d err=%v", affected, err)
	}
	if affected, err := repo.ConsumeManualStock(unlimited.ID, 1); err != nil || affected != 0 {
		t.Fatalf("unlimited consume should be no-op, affected=%d err=%v", affected, err)
	}
}

func TestSKUStoreDeleteByProductHidesRowsAndRejectsStockMutations(t *testing.T) {
	repo, db := setupSKUStoreTest(t)
	sku := &productdomain.ProductSKU{
		ProductID:         7,
		SKUCode:           "SOFT-DELETED",
		PriceAmount:       money.FromDecimal(decimal.NewFromInt(100)),
		ManualStockTotal:  10,
		ManualStockLocked: 1,
		IsActive:          true,
	}
	if err := repo.Create(sku); err != nil {
		t.Fatalf("create sku: %v", err)
	}
	if err := repo.DeleteByProduct(sku.ProductID); err != nil {
		t.Fatalf("soft delete skus by product: %v", err)
	}

	var persisted productdomain.ProductSKU
	if err := db.Where("id = ?", sku.ID).First(&persisted).Error; err != nil {
		t.Fatalf("load persisted soft-deleted sku: %v", err)
	}
	if persisted.DeletedAt == nil {
		t.Fatal("expected deleted_at to be persisted")
	}

	byID, err := repo.GetByID(sku.ID)
	if err != nil || byID != nil {
		t.Fatalf("soft-deleted sku must be hidden by id, sku=%#v err=%v", byID, err)
	}
	byCode, err := repo.GetByProductAndCode(sku.ProductID, sku.SKUCode)
	if err != nil || byCode != nil {
		t.Fatalf("soft-deleted sku must be hidden by code, sku=%#v err=%v", byCode, err)
	}
	rows, err := repo.ListByProduct(sku.ProductID, false)
	if err != nil || len(rows) != 0 {
		t.Fatalf("soft-deleted sku must be hidden from list, rows=%#v err=%v", rows, err)
	}

	for operation, mutate := range map[string]func() (int64, error){
		"reserve": func() (int64, error) { return repo.ReserveManualStock(sku.ID, 1) },
		"release": func() (int64, error) { return repo.ReleaseManualStock(sku.ID, 1) },
		"consume": func() (int64, error) { return repo.ConsumeManualStock(sku.ID, 1) },
	} {
		affected, err := mutate()
		if err != nil || affected != 0 {
			t.Fatalf("%s must not mutate soft-deleted sku, affected=%d err=%v", operation, affected, err)
		}
	}
}
