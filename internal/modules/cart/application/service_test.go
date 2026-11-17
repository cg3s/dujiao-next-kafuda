package application

import (
	"errors"
	"testing"
	"time"

	"github.com/dujiao-next/internal/constants"
	"github.com/dujiao-next/internal/modules/cart/contract"
	"github.com/dujiao-next/internal/modules/cart/domain"
	productdomain "github.com/dujiao-next/internal/modules/catalog/product/domain"
	promotioncontract "github.com/dujiao-next/internal/modules/promotion/contract"
	promotiondomain "github.com/dujiao-next/internal/modules/promotion/domain"
	"github.com/dujiao-next/internal/shared/money"
	"github.com/shopspring/decimal"
)

type boundaryCartStore struct {
	records   []contract.StoredItem
	deleted   [][3]uint
	deleteErr error
	writes    int
}

func (s *boundaryCartStore) ListByUser(uint) ([]contract.StoredItem, error) { return s.records, nil }
func (s *boundaryCartStore) Upsert(*domain.Item) error                      { s.writes++; return nil }
func (s *boundaryCartStore) DeleteByUserProductSKU(u, p, k uint) error {
	s.deleted = append(s.deleted, [3]uint{u, p, k})
	return s.deleteErr
}

type boundaryProductReader struct{ product *productdomain.Product }

func (r boundaryProductReader) GetByID(string) (*productdomain.Product, error) { return r.product, nil }

type boundaryPromotions struct{ promotioncontract.Repository }

func (boundaryPromotions) GetAllActiveByProduct(uint, time.Time) ([]promotiondomain.Promotion, error) {
	return nil, nil
}

func TestListByUserRemovesDirectBeforeSKUAndPricing(t *testing.T) {
	for _, preloaded := range []bool{true, false} {
		for _, failDelete := range []bool{true, false} {
			t.Run(map[bool]string{true: "preloaded", false: "fallback"}[preloaded]+"/"+map[bool]string{true: "failure", false: "success"}[failDelete], func(t *testing.T) {
				product := &productdomain.Product{ID: 7, FulfillmentType: constants.FulfillmentTypeKafudaGPT, IsActive: true}
				record := contract.StoredItem{Item: domain.Item{UserID: 3, ProductID: 7, SKUID: 19, Quantity: 1, FulfillmentType: constants.FulfillmentTypeManual}}
				if preloaded {
					record.Product = product
				}
				store := &boundaryCartStore{records: []contract.StoredItem{record}}
				cleanupErr := errors.New("cleanup failed")
				if failDelete {
					store.deleteErr = cleanupErr
				}
				// Nil SKU and promotion ports detect accidental stock/pricing work for Direct.
				svc := NewService(store, boundaryProductReader{product}, nil, nil, nil)
				rows, err := svc.ListByUser(3)
				if failDelete {
					if !errors.Is(err, cleanupErr) {
						t.Fatalf("cleanup failure was hidden: %v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
				if len(rows) != 0 || len(store.deleted) != 1 || store.deleted[0] != [3]uint{3, 7, 19} || store.writes != 0 {
					t.Fatal("Direct row was returned or wrong cart row was removed")
				}
			})
		}
	}
}

func TestListByUserKeepsOrdinaryItemsWhenRemovingDirect(t *testing.T) {
	direct := &productdomain.Product{ID: 7, FulfillmentType: constants.FulfillmentTypeKafudaGPT, IsActive: true}
	records := []contract.StoredItem{{Item: domain.Item{UserID: 3, ProductID: 7, SKUID: 19, Quantity: 1}, Product: direct}}
	for i, kind := range []string{constants.FulfillmentTypeAuto, constants.FulfillmentTypeManual} {
		id := uint(i + 8)
		records = append(records, contract.StoredItem{Item: domain.Item{UserID: 3, ProductID: id, SKUID: id, Quantity: 1}, Product: &productdomain.Product{ID: id, FulfillmentType: kind, IsActive: true}, SKU: &productdomain.ProductSKU{ID: id, ProductID: id, IsActive: true, ManualStockTotal: 2, PriceAmount: money.FromDecimal(decimal.NewFromInt(10))}})
	}
	store := &boundaryCartStore{records: records}
	rows, err := NewService(store, nil, nil, boundaryPromotions{}, nil).ListByUser(3)
	if err != nil || len(rows) != 2 {
		t.Fatalf("ordinary cart items were lost: %+v, %v", rows, err)
	}
	if len(store.deleted) != 1 || store.deleted[0] != [3]uint{3, 7, 19} {
		t.Fatal("ordinary cart items were removed")
	}
	for _, row := range rows {
		if row.ProductID == 7 || row.UnitPrice.String() != "10.00" {
			t.Fatal("unexpected cart price or Direct row")
		}
	}
}
