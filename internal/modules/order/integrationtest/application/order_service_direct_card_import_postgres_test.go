//go:build integration

package application_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dujiao-next/internal/constants"
	cardsecretapp "github.com/dujiao-next/internal/modules/cardsecret/application"
	cardsecretcontract "github.com/dujiao-next/internal/modules/cardsecret/contract"
	cardsecretdomain "github.com/dujiao-next/internal/modules/cardsecret/domain"
	cardsecretstore "github.com/dujiao-next/internal/modules/cardsecret/infrastructure/gormstore"
	categorydomain "github.com/dujiao-next/internal/modules/catalog/category/domain"
	productwrite "github.com/dujiao-next/internal/modules/catalog/product/application/write"
	productcontract "github.com/dujiao-next/internal/modules/catalog/product/contract"
	productdomain "github.com/dujiao-next/internal/modules/catalog/product/domain"
	productstore "github.com/dujiao-next/internal/modules/catalog/product/store/gormstore"
	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	ordercontract "github.com/dujiao-next/internal/modules/order/contract"
	orderdomain "github.com/dujiao-next/internal/modules/order/domain"
	orderstore "github.com/dujiao-next/internal/modules/order/infrastructure/gormstore"
	"github.com/dujiao-next/internal/shared/jsonmap"
	"github.com/dujiao-next/internal/shared/money"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

func TestPostgresDirectTypeSwitchAndCardImportShareProductLock(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is empty")
	}
	for _, source := range []string{constants.CardSecretSourceManual, constants.CardSecretSourceCSV} {
		for _, winner := range []string{"direct", "import"} {
			t.Run(source+"/"+winner+"_first", func(t *testing.T) {
				testPostgresDirectTypeSwitchAndCardImport(t, dsn, source, winner == "direct")
			})
		}
	}
}

func testPostgresDirectTypeSwitchAndCardImport(t *testing.T, dsn, source string, directFirst bool) {
	t.Helper()
	db := openPostgresDirectRaceDB(t, dsn)
	if err := db.AutoMigrate(&categorydomain.Category{}, &productdomain.Product{}, &productdomain.ProductSKU{},
		&cardsecretdomain.Batch{}, &cardsecretdomain.Secret{}, &orderdomain.OrderItem{}); err != nil {
		t.Fatal(err)
	}
	category := categorydomain.Category{Slug: "card-import-race", NameJSON: jsonmap.JSON{"en-US": "Card imports"}}
	if err := db.Create(&category).Error; err != nil {
		t.Fatal(err)
	}
	product := productdomain.Product{
		CategoryID: category.ID, Slug: "card-import-race", TitleJSON: jsonmap.JSON{"en-US": "Card imports"},
		PriceAmount: money.FromDecimal(decimal.NewFromInt(10)), FulfillmentType: constants.FulfillmentTypeAuto,
	}
	if err := db.Create(&product).Error; err != nil {
		t.Fatal(err)
	}
	sku := productdomain.ProductSKU{
		ProductID: product.ID, SKUCode: productdomain.DefaultSKUCode, PriceAmount: product.PriceAmount,
		CostPriceAmount: money.FromDecimal(decimal.NewFromInt(3)), IsActive: true,
	}
	if err := db.Create(&sku).Error; err != nil {
		t.Fatal(err)
	}

	lockPIDs := make(chan int, 2)
	if err := db.Callback().Query().Before("gorm:query").Register("test:card_import_product_lock_pid", func(tx *gorm.DB) {
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
	products, skus := productstore.NewProductStore(scopedDB), productstore.NewSKUStore(scopedDB)
	secrets, batches := cardsecretstore.New(scopedDB), cardsecretstore.NewBatch(scopedDB)
	var orders ordercontract.Store = orderstore.New(scopedDB, "test-guest-credential-secret-with-32-bytes")
	var imports cardsecretcontract.UnitOfWork = secrets.WithProducts(products, skus)
	beforeCommit, release := make(chan struct{}), make(chan struct{})
	commit := sync.OnceFunc(func() { close(release) })
	var workers sync.WaitGroup
	defer func() {
		commit()
		cancel()
		workers.Wait()
	}()
	if directFirst {
		orders = directProductLockStore{Store: orders, beforeCommit: beforeCommit, release: release}
	} else {
		imports = heldCardStockTransaction{UnitOfWork: imports, beforeCommit: beforeCommit, release: release}
	}
	writer := productwrite.NewWriteService(productwrite.Options{Products: products, SKUs: skus, CardSecrets: secrets, Orders: orders})
	writer.SetDirectCatalog(orderProductChangeCatalog{})
	cards := cardsecretapp.NewService(cardsecretapp.ServiceOptions{
		Secrets: secrets, Batches: batches, Products: products, ProductSKUs: skus, Transactions: imports,
	})
	productID := strconv.FormatUint(uint64(product.ID), 10)
	directSKUCode := kafudacontract.SKUCode("plus", "PH")
	switchType := func() error {
		active := true
		_, err := writer.Update(productID, productwrite.CreateProductInput{
			CategoryID: product.CategoryID, Slug: product.Slug, TitleJSON: product.TitleJSON,
			FulfillmentType: constants.FulfillmentTypeKafudaGPT, PriceAmount: product.PriceAmount.Decimal,
			SKUs: []productwrite.ProductSKUInput{{
				SKUCode: directSKUCode, PriceAmount: decimal.NewFromInt(15), CostPriceAmount: decimal.NewFromInt(8),
				CostPriceProvided: true, IsActive: &active,
			}},
		})
		return err
	}
	const secret = "test-card-inventory"
	var csvFile *multipart.FileHeader
	if source == constants.CardSecretSourceCSV {
		csvFile = cardImportRaceCSV(t, secret)
	}
	importCards := func() error {
		var batch *cardsecretdomain.Batch
		var count int
		var err error
		if csvFile != nil {
			batch, count, err = cards.ImportCardSecretCSV(cardsecretapp.ImportCardSecretCSVInput{
				ProductID: product.ID, SKUID: sku.ID, File: csvFile,
			})
		} else {
			batch, count, err = cards.CreateCardSecretBatch(cardsecretapp.CreateCardSecretBatchInput{
				ProductID: product.ID, SKUID: sku.ID, Secrets: []string{secret},
			})
		}
		if err != nil && (batch != nil || count != 0) {
			return fmt.Errorf("failed import returned a batch or count: %d", count)
		}
		if err == nil && (batch == nil || count != 1) {
			return fmt.Errorf("successful import returned no batch or an incorrect count: %d", count)
		}
		return err
	}
	first, second := importCards, switchType
	wantRejection := productcontract.ErrProductSKUHasCardSecretStock
	if directFirst {
		first, second = switchType, importCards
		wantRejection = cardsecretapp.ErrInvalid
	}
	firstResult, secondResult := make(chan error, 1), make(chan error, 1)
	workers.Go(func() { firstResult <- first() })
	select {
	case <-beforeCommit:
	case err := <-firstResult:
		t.Fatalf("first operation stopped before commit: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	var holder int
	select {
	case holder = <-lockPIDs:
	case <-ctx.Done():
		t.Fatal("first operation did not lock the product")
	}
	workers.Go(func() { secondResult <- second() })
	var waiter int
	select {
	case waiter = <-lockPIDs:
	case err := <-secondResult:
		t.Fatalf("second operation returned before locking the product: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if holder == waiter {
		t.Fatal("competing transactions shared a PostgreSQL connection")
	}
	assertProductLockBlocked(t, ctx, db, waiter, holder)
	commit()
	if err := <-firstResult; err != nil {
		t.Fatalf("commit the first operation: %v", err)
	}
	if err := <-secondResult; !errors.Is(err, wantRejection) {
		t.Fatalf("second operation returned %v, want %v", err, wantRejection)
	}

	current, err := products.GetByID(productID)
	if err != nil || current == nil {
		t.Fatalf("load resulting product: %v", err)
	}
	currentSKUs, err := skus.ListByProduct(product.ID, false)
	if err != nil {
		t.Fatalf("list resulting SKUs: %v", err)
	}
	var savedBatches []cardsecretdomain.Batch
	if err := db.Find(&savedBatches).Error; err != nil {
		t.Fatal(err)
	}
	var savedCards []cardsecretdomain.Secret
	if err := db.Find(&savedCards).Error; err != nil {
		t.Fatal(err)
	}
	if directFirst {
		if current.FulfillmentType != constants.FulfillmentTypeKafudaGPT || len(savedBatches) != 0 || len(savedCards) != 0 {
			t.Fatalf("rejected import altered Direct state or added inventory: type=%s batches=%d cards=%d",
				current.FulfillmentType, len(savedBatches), len(savedCards))
		}
		activeSKUs, err := skus.ListByProduct(product.ID, true)
		if err != nil || len(currentSKUs) != 2 || len(activeSKUs) != 1 || activeSKUs[0].ID == sku.ID ||
			activeSKUs[0].SKUCode != directSKUCode || !activeSKUs[0].DirectCostConfigured ||
			!activeSKUs[0].PriceAmount.Equal(decimal.NewFromInt(15)) || !activeSKUs[0].CostPriceAmount.Equal(decimal.NewFromInt(8)) {
			t.Fatalf("expected one new active Direct SKU and a retained historical SKU: total=%d active=%+v err=%v", len(currentSKUs), activeSKUs, err)
		}
		historicalSKU, err := skus.GetByID(sku.ID)
		if err != nil || historicalSKU == nil || historicalSKU.IsActive || historicalSKU.ProductID != product.ID ||
			historicalSKU.SKUCode != sku.SKUCode || !historicalSKU.PriceAmount.Equal(sku.PriceAmount.Decimal) ||
			!historicalSKU.CostPriceAmount.Equal(sku.CostPriceAmount.Decimal) {
			t.Fatalf("expected original SKU disabled with prices and association preserved: SKU=%+v err=%v", historicalSKU, err)
		}
		return
	}
	if current.FulfillmentType != product.FulfillmentType || len(currentSKUs) != 1 || currentSKUs[0].ID != sku.ID || currentSKUs[0].SKUCode != sku.SKUCode ||
		!currentSKUs[0].PriceAmount.Equal(sku.PriceAmount.Decimal) || !currentSKUs[0].CostPriceAmount.Equal(sku.CostPriceAmount.Decimal) ||
		!currentSKUs[0].IsActive {
		t.Fatal("rejected type switch did not preserve the original automatic stock SKU")
	}
	if len(savedBatches) != 1 || savedBatches[0].Source != source || savedBatches[0].SKUID != sku.ID ||
		savedBatches[0].ProductID != product.ID || savedBatches[0].TotalCount != 1 {
		t.Fatalf("original batch was lost or reassigned: %d batches", len(savedBatches))
	}
	if len(savedCards) != 1 || savedCards[0].Status != cardsecretdomain.StatusAvailable || savedCards[0].ProductID != product.ID ||
		savedCards[0].SKUID != sku.ID || savedCards[0].Secret != secret || savedCards[0].BatchID == nil || *savedCards[0].BatchID != savedBatches[0].ID {
		t.Fatalf("original inventory was lost or reassigned: %d cards", len(savedCards))
	}
}

func TestPostgresDirectTypeSwitchAndCardExportShareProductLock(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is empty")
	}
	for _, scenario := range []struct {
		name        string
		deleteStock bool
		directFirst bool
	}{
		{name: "export_marks_used"},
		{name: "export_deletes_last_card", deleteStock: true},
		{name: "conversion_before_export", directFirst: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			db := openPostgresDirectRaceDB(t, dsn)
			f := seedDirectCheckoutFixture(t, seedCouponLimitRaceFixture(t, db, "direct-card-export", 0, 0))
			if err := db.AutoMigrate(&cardsecretdomain.Secret{}); err != nil {
				t.Fatal(err)
			}
			if err := db.Model(&f.product).UpdateColumn("fulfillment_type", constants.FulfillmentTypeAuto).Error; err != nil {
				t.Fatal(err)
			}
			const secret = "test-card-export-lock"
			if !scenario.directFirst {
				if err := db.Create(&cardsecretdomain.Secret{ProductID: f.product.ID, SKUID: f.sku.ID, Secret: secret, Status: cardsecretdomain.StatusAvailable}).Error; err != nil {
					t.Fatal(err)
				}
			}

			lockPIDs := observeProductLockPIDs(t, db)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			beforeCommit, release := make(chan struct{}), make(chan struct{})
			commit := sync.OnceFunc(func() { close(release) })
			var workers sync.WaitGroup
			defer func() {
				commit()
				cancel()
				workers.Wait()
			}()
			scoped := db.WithContext(ctx)
			products, skus, secrets := productstore.NewProductStore(scoped), productstore.NewSKUStore(scoped), cardsecretstore.New(scoped)
			var orders ordercontract.Store = orderstore.New(scoped, "test-guest-credential-secret-with-32-bytes")
			var exports cardsecretcontract.UnitOfWork = secrets.WithProducts(products, skus)
			if scenario.directFirst {
				orders = directProductLockStore{Store: orders, beforeCommit: beforeCommit, release: release}
			} else {
				exports = heldCardStockTransaction{UnitOfWork: exports, beforeCommit: beforeCommit, release: release}
			}
			writer := productwrite.NewWriteService(productwrite.Options{Products: products, SKUs: skus, CardSecrets: secrets, Orders: orders})
			writer.SetDirectCatalog(orderProductChangeCatalog{})
			cards := cardsecretapp.NewService(cardsecretapp.ServiceOptions{Secrets: secrets, Products: products, ProductSKUs: skus, Transactions: exports})
			productID := strconv.FormatUint(uint64(f.product.ID), 10)
			switchToDirect := func() error {
				active := true
				_, err := writer.Update(productID, productwrite.CreateProductInput{
					CategoryID: f.product.CategoryID, Slug: f.product.Slug, TitleJSON: f.product.TitleJSON,
					FulfillmentType: constants.FulfillmentTypeKafudaGPT, PriceAmount: f.product.PriceAmount.Decimal,
					SKUs: []productwrite.ProductSKUInput{{
						ID: f.sku.ID, SKUCode: f.sku.SKUCode, PriceAmount: f.sku.PriceAmount.Decimal,
						CostPriceAmount: f.sku.CostPriceAmount.Decimal, CostPriceProvided: true, IsActive: &active,
					}},
				})
				return err
			}
			export := func() error {
				result, err := cards.ExportAvailableCardSecrets(cardsecretapp.ExportAvailableCardSecretInput{
					ProductID: f.product.ID, SKUID: f.sku.ID, Limit: 1, Format: constants.ExportFormatTXT, DeleteAfterExport: scenario.deleteStock,
				})
				if err != nil && result != nil || err == nil && (result == nil || result.Count != 1 || strings.TrimSpace(string(result.Content)) != secret) {
					return errors.New("export returned inconsistent inventory or content")
				}
				return err
			}
			first, second, wantErr := export, switchToDirect, productcontract.ErrProductSKUHasCardSecretStock
			if scenario.directFirst {
				first, second, wantErr = switchToDirect, export, cardsecretapp.ErrInvalid
			} else if scenario.deleteStock {
				wantErr = nil
			}
			firstResult, secondResult := make(chan error, 1), make(chan error, 1)
			workers.Go(func() { firstResult <- first() })
			select {
			case <-beforeCommit:
			case err := <-firstResult:
				t.Fatalf("first operation stopped before commit: %v", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			var holder int
			select {
			case holder = <-lockPIDs:
			case <-ctx.Done():
				t.Fatal("first operation did not lock the product")
			}
			workers.Go(func() { secondResult <- second() })
			select {
			case waiter := <-lockPIDs:
				if holder == waiter {
					t.Fatal("competing transactions shared a PostgreSQL connection")
				}
				assertProductLockBlocked(t, ctx, db, waiter, holder)
			case err := <-secondResult:
				t.Fatalf("second operation returned before locking the product: %v", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			commit()
			if err := <-firstResult; err != nil {
				t.Fatalf("commit the first operation: %v", err)
			}
			if err := <-secondResult; !errors.Is(err, wantErr) {
				t.Fatalf("second operation = %v, want %v", err, wantErr)
			}
			wantType, wantTotal, wantUsed := constants.FulfillmentTypeAuto, int64(1), int64(1)
			if scenario.directFirst || scenario.deleteStock {
				wantType, wantTotal, wantUsed = constants.FulfillmentTypeKafudaGPT, 0, 0
			}
			product, err := products.GetByID(productID)
			if err != nil || product == nil || product.FulfillmentType != wantType {
				t.Fatalf("unexpected resulting fulfillment type: %+v, %v", product, err)
			}
			total, available, used, err := secrets.CountByProduct(f.product.ID, 0)
			if err != nil || total != wantTotal || available != 0 || used != wantUsed {
				t.Fatalf("unexpected final inventory: %d total, %d available, %d used: %v", total, available, used, err)
			}
		})
	}
}

type heldCardStockTransaction struct {
	cardsecretcontract.UnitOfWork
	beforeCommit chan<- struct{}
	release      <-chan struct{}
}

func (s heldCardStockTransaction) Transaction(run func(cardsecretcontract.Repository, cardsecretcontract.BatchRepository, cardsecretcontract.ProductRepository, cardsecretcontract.ProductSKURepository) error) error {
	return s.UnitOfWork.Transaction(func(secrets cardsecretcontract.Repository, batches cardsecretcontract.BatchRepository, products cardsecretcontract.ProductRepository, skus cardsecretcontract.ProductSKURepository) error {
		if err := run(secrets, batches, products, skus); err != nil {
			return err
		}
		close(s.beforeCommit)
		<-s.release
		return nil
	})
}

func cardImportRaceCSV(t *testing.T, secret string) *multipart.FileHeader {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "cards.csv")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(part, "secret\n"+secret+"\n"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	form, err := multipart.NewReader(&body, writer.Boundary()).ReadForm(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = form.RemoveAll() })
	if len(form.File["file"]) != 1 {
		t.Fatal("missing multipart card CSV")
	}
	return form.File["file"][0]
}
