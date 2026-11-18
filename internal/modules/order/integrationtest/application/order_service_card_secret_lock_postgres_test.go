//go:build integration

package application_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dujiao-next/internal/constants"
	cardsecretapp "github.com/dujiao-next/internal/modules/cardsecret/application"
	cardsecretdomain "github.com/dujiao-next/internal/modules/cardsecret/domain"
	cardsecretstore "github.com/dujiao-next/internal/modules/cardsecret/infrastructure/gormstore"
	categorydomain "github.com/dujiao-next/internal/modules/catalog/category/domain"
	productwrite "github.com/dujiao-next/internal/modules/catalog/product/application/write"
	productdomain "github.com/dujiao-next/internal/modules/catalog/product/domain"
	productstore "github.com/dujiao-next/internal/modules/catalog/product/store/gormstore"
	orderstore "github.com/dujiao-next/internal/modules/order/infrastructure/gormstore"
	"github.com/dujiao-next/internal/shared/jsonmap"
	"github.com/dujiao-next/internal/shared/money"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

func TestPostgresCardSecretStatusRechecksConcurrentSKUDeletion(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is empty")
	}
	for _, mode := range []string{"single", "batch"} {
		t.Run(mode, func(t *testing.T) {
			db := openPostgresDirectRaceDB(t, dsn)
			if err := db.AutoMigrate(&categorydomain.Category{}, &productdomain.Product{}, &productdomain.ProductSKU{}, &cardsecretdomain.Batch{}, &cardsecretdomain.Secret{}); err != nil {
				t.Fatal(err)
			}
			category := categorydomain.Category{Slug: "card-status", NameJSON: jsonmap.JSON{"en-US": "Test category"}}
			if err := db.Create(&category).Error; err != nil {
				t.Fatal(err)
			}
			product := productdomain.Product{
				CategoryID: category.ID, Slug: "card-status-product", TitleJSON: jsonmap.JSON{"en-US": "Test product"},
				PriceAmount: money.FromDecimal(decimal.NewFromInt(10)), FulfillmentType: constants.FulfillmentTypeAuto,
			}
			if err := db.Create(&product).Error; err != nil {
				t.Fatal(err)
			}
			skus := []productdomain.ProductSKU{
				{ProductID: product.ID, SKUCode: productdomain.DefaultSKUCode, PriceAmount: product.PriceAmount, IsActive: true},
				{ProductID: product.ID, SKUCode: "retired", PriceAmount: product.PriceAmount, IsActive: true},
			}
			if err := db.Create(&skus).Error; err != nil {
				t.Fatal(err)
			}
			cards := []cardsecretdomain.Secret{
				{ProductID: product.ID, SKUID: skus[0].ID, Status: cardsecretdomain.StatusUsed, Secret: "kept"},
				{ProductID: product.ID, SKUID: skus[1].ID, Status: cardsecretdomain.StatusUsed, Secret: "historical"},
			}
			if err := db.Create(&cards).Error; err != nil {
				t.Fatal(err)
			}

			lockPIDs := make(chan int, 2)
			if err := db.Callback().Query().Before("gorm:query").Register("test:card_status_product_lock_pid", func(tx *gorm.DB) {
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
			scopedDB := db.WithContext(ctx)
			products, skuStore := productstore.NewProductStore(scopedDB), productstore.NewSKUStore(scopedDB)
			secrets, batches := cardsecretstore.New(scopedDB), cardsecretstore.NewBatch(scopedDB)
			service := cardsecretapp.NewService(cardsecretapp.ServiceOptions{
				Secrets: secrets, Batches: batches, Products: products, ProductSKUs: skuStore,
				Transactions: secrets.WithProducts(products, skuStore),
			})
			beforeCommit, release := make(chan struct{}), make(chan struct{})
			commit := sync.OnceFunc(func() { close(release) })
			var workers sync.WaitGroup
			defer func() {
				commit()
				cancel()
				workers.Wait()
			}()
			orders := directProductLockStore{
				Store:        orderstore.New(scopedDB, "test-guest-credential-secret-with-32-bytes"),
				beforeCommit: beforeCommit, release: release,
			}
			writer := productwrite.NewWriteService(productwrite.Options{
				Products: products, SKUs: skuStore, CardSecrets: secrets, Orders: orders,
			})
			deleted, reactivated := make(chan error, 1), make(chan error, 1)
			workers.Go(func() {
				_, err := writer.Update(strconv.FormatUint(uint64(product.ID), 10), productwrite.CreateProductInput{
					CategoryID: product.CategoryID, Slug: product.Slug, TitleJSON: product.TitleJSON,
					FulfillmentType: product.FulfillmentType, PriceAmount: product.PriceAmount.Decimal,
				})
				deleted <- err
			})
			select {
			case <-beforeCommit:
			case err := <-deleted:
				t.Fatalf("SKU deletion did not reach commit barrier: %v", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			var holder int
			select {
			case holder = <-lockPIDs:
			case <-ctx.Done():
				t.Fatal("SKU deletion did not lock the product")
			}

			workers.Go(func() {
				if mode == "single" {
					_, err := service.UpdateCardSecret(cards[1].ID, "changed", cardsecretdomain.StatusAvailable)
					reactivated <- err
					return
				}
				affected, err := service.BatchUpdateCardSecretStatus([]uint{cards[1].ID, cards[0].ID}, 0, cardsecretapp.ListCardSecretInput{}, cardsecretdomain.StatusAvailable)
				if err == nil && affected != 1 {
					err = fmt.Errorf("batch affected %d rows, want 1", affected)
				}
				reactivated <- err
			})
			var waiter int
			select {
			case waiter = <-lockPIDs:
			case err := <-reactivated:
				t.Fatalf("card status write returned before taking the product lock: %v", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if holder == waiter {
				t.Fatal("SKU deletion and card status write used the same PostgreSQL connection")
			}
			assertProductLockBlocked(t, ctx, db, waiter, holder)
			commit()
			select {
			case err := <-deleted:
				if err != nil {
					t.Fatalf("commit SKU deletion: %v", err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			wantErr := error(nil)
			if mode == "single" {
				wantErr = cardsecretapp.ErrInvalid
			}
			select {
			case err := <-reactivated:
				if !errors.Is(err, wantErr) {
					t.Fatalf("reactivate after SKU deletion: %v, want %v", err, wantErr)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if sku, err := skuStore.GetByID(skus[1].ID); err != nil || sku != nil {
				t.Fatalf("omitted SKU still exists: %+v, %v", sku, err)
			}
			history, err := secrets.GetByID(cards[1].ID)
			if err != nil || history == nil || history.SKUID != cards[1].SKUID || history.Status != cardsecretdomain.StatusUsed || history.Secret != cards[1].Secret {
				t.Fatalf("historical card was changed: %+v, %v", history, err)
			}
			if mode == "batch" {
				kept, err := secrets.GetByID(cards[0].ID)
				if err != nil || kept == nil || kept.Status != cardsecretdomain.StatusAvailable {
					t.Fatalf("valid card was not updated: %+v, %v", kept, err)
				}
			}
		})
	}
}
