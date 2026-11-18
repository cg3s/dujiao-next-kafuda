package integrationtest

import (
	"errors"
	"testing"
	"time"

	"github.com/dujiao-next/internal/constants"
	cardsecretapp "github.com/dujiao-next/internal/modules/cardsecret/application"
	cardsecretcontract "github.com/dujiao-next/internal/modules/cardsecret/contract"
	cardsecretdomain "github.com/dujiao-next/internal/modules/cardsecret/domain"
	cardsecretgormstore "github.com/dujiao-next/internal/modules/cardsecret/infrastructure/gormstore"
	productdomain "github.com/dujiao-next/internal/modules/catalog/product/domain"
	productgormstore "github.com/dujiao-next/internal/modules/catalog/product/store/gormstore"
	"github.com/dujiao-next/internal/shared/jsonmap"
	"gorm.io/gorm"
)

func TestCardSecretReactivationRechecksDestinationInTransaction(t *testing.T) {
	for _, change := range []string{"sku_removed", "product_removed", "product_converted"} {
		t.Run(change, func(t *testing.T) {
			db, options := cardStatusTestService(t)
			product, sku := createCardStatusProduct(t, db, "stock", constants.FulfillmentTypeAuto)
			card := cardsecretdomain.Secret{ProductID: product.ID, SKUID: sku.ID, Secret: "original", Status: cardsecretdomain.StatusUsed}
			if err := db.Create(&card).Error; err != nil {
				t.Fatal(err)
			}
			checked := false
			options.Transactions = cardSecretTransactionHook{UnitOfWork: options.Transactions, before: func() {
				checked = true
				var err error
				switch change {
				case "sku_removed":
					err = productgormstore.NewSKUStore(db).Delete(sku.ID)
				case "product_removed":
					err = db.Model(&product).Update("deleted_at", time.Now()).Error
				case "product_converted":
					err = db.Model(&product).Update("fulfillment_type", constants.FulfillmentTypeKafudaGPT).Error
				}
				if err != nil {
					t.Fatal(err)
				}
			}}
			svc := cardsecretapp.NewService(options)
			got, err := svc.UpdateCardSecret(card.ID, "replacement", cardsecretdomain.StatusAvailable)
			if !checked || !errors.Is(err, cardsecretapp.ErrInvalid) || got != nil {
				t.Fatalf("reactivation after %s = (%v, %v), reached transaction: %t", change, got != nil, err, checked)
			}
			stored, err := options.Secrets.GetByID(card.ID)
			if err != nil || stored == nil || stored.Status != cardsecretdomain.StatusUsed || stored.Secret != card.Secret || !stored.UpdatedAt.Equal(card.UpdatedAt) {
				t.Fatalf("rejected reactivation changed the card: %v", err)
			}
		})
	}
}

func TestBatchCardSecretReactivationSkipsInvalidDestinations(t *testing.T) {
	db, options := cardStatusTestService(t)
	product, sku := createCardStatusProduct(t, db, "stock", constants.FulfillmentTypeAuto)
	_, removedSKU := createCardStatusProduct(t, db, "orphan", constants.FulfillmentTypeAuto)
	direct, directSKU := createCardStatusProduct(t, db, "direct", constants.FulfillmentTypeKafudaGPT)
	if err := productgormstore.NewSKUStore(db).Delete(removedSKU.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&sku).Update("is_active", false).Error; err != nil {
		t.Fatal(err)
	}
	cards := []cardsecretdomain.Secret{
		{ProductID: product.ID, SKUID: sku.ID, Secret: "disabled-sku", Status: cardsecretdomain.StatusUsed},
		{ProductID: product.ID, Secret: "legacy-no-sku", Status: cardsecretdomain.StatusUsed},
		{ProductID: removedSKU.ProductID, SKUID: removedSKU.ID, Secret: "deleted-sku", Status: cardsecretdomain.StatusUsed},
		{ProductID: removedSKU.ProductID, SKUID: sku.ID, Secret: "wrong-sku-owner", Status: cardsecretdomain.StatusUsed},
		{ProductID: direct.ID, SKUID: directSKU.ID, Secret: "direct", Status: cardsecretdomain.StatusUsed},
		{ProductID: direct.ID + 1000, Secret: "missing-product", Status: cardsecretdomain.StatusUsed},
	}
	if err := db.Create(&cards).Error; err != nil {
		t.Fatal(err)
	}
	svc := cardsecretapp.NewService(options)
	affected, err := svc.BatchUpdateCardSecretStatus(nil, 0, ListCardSecretInput{Status: cardsecretdomain.StatusUsed}, cardsecretdomain.StatusReserved)
	if err != nil || affected != 2 {
		t.Fatalf("affected = %d, want 2: %v", affected, err)
	}
	for i, card := range cards {
		want := cardsecretdomain.StatusUsed
		if i < 2 {
			want = cardsecretdomain.StatusReserved
		}
		stored, err := options.Secrets.GetByID(card.ID)
		if err != nil || stored == nil || stored.Status != want {
			t.Fatalf("unexpected card %d status after batch update, want %s: %v", card.ID, want, err)
		}
	}
}

func TestEditingCardSecretTextPreservesConcurrentStatusChange(t *testing.T) {
	db, options := cardStatusTestService(t)
	product, sku := createCardStatusProduct(t, db, "stock", constants.FulfillmentTypeAuto)
	card := cardsecretdomain.Secret{ProductID: product.ID, SKUID: sku.ID, Secret: "original", Status: cardsecretdomain.StatusAvailable}
	if err := db.Create(&card).Error; err != nil {
		t.Fatal(err)
	}
	repo := options.Secrets
	options.Secrets = &cardStatusReadHook{Repository: repo, afterRead: func() {
		if _, err := repo.BatchUpdateStatus([]uint{card.ID}, cardsecretdomain.StatusUsed, time.Now()); err != nil {
			t.Fatal(err)
		}
	}}
	svc := cardsecretapp.NewService(options)
	item, err := svc.UpdateCardSecret(card.ID, "replacement", "")
	if err != nil || item == nil || item.Status != cardsecretdomain.StatusUsed {
		t.Fatalf("edit should return the current used card: %v", err)
	}
	stored, err := repo.GetByID(card.ID)
	if err != nil || stored == nil || stored.Status != cardsecretdomain.StatusUsed || stored.Secret != "replacement" {
		t.Fatalf("text edit restored stale inventory or lost the new value: %v", err)
	}
}

type cardStatusReadHook struct {
	cardsecretcontract.Repository
	afterRead func()
}

func (s *cardStatusReadHook) GetByID(id uint) (*cardsecretdomain.Secret, error) {
	item, err := s.Repository.GetByID(id)
	if err == nil && item != nil && s.afterRead != nil {
		afterRead := s.afterRead
		s.afterRead = nil
		afterRead()
	}
	return item, err
}

func cardStatusTestService(t *testing.T) (*gorm.DB, cardsecretapp.ServiceOptions) {
	t.Helper()
	db := setupCardSecretServiceTestDB(t)
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = pool.Close() })
	secrets := cardsecretgormstore.New(db)
	products, skus := productgormstore.NewProductStore(db), productgormstore.NewSKUStore(db)
	return db, cardsecretapp.ServiceOptions{Secrets: secrets, Batches: cardsecretgormstore.NewBatch(db),
		Transactions: secrets.WithProducts(products, skus), Products: products, ProductSKUs: skus}
}

func createCardStatusProduct(t *testing.T, db *gorm.DB, slug, fulfillmentType string) (productdomain.Product, productdomain.ProductSKU) {
	t.Helper()
	product := productdomain.Product{Slug: slug, FulfillmentType: fulfillmentType, TitleJSON: jsonmap.JSON{"zh-CN": slug}}
	if err := db.Create(&product).Error; err != nil {
		t.Fatal(err)
	}
	sku := productdomain.ProductSKU{ProductID: product.ID, SKUCode: productdomain.DefaultSKUCode, IsActive: true}
	if err := db.Create(&sku).Error; err != nil {
		t.Fatal(err)
	}
	return product, sku
}
