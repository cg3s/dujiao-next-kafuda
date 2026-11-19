package contract

import (
	"errors"
	"time"

	cardsecretdomain "github.com/dujiao-next/internal/modules/cardsecret/domain"
	productdomain "github.com/dujiao-next/internal/modules/catalog/product/domain"
)

var (
	ErrNotFound         = errors.New("not found")
	ErrOrderReservation = errors.New("card secret reserved by an order")
)

// Repository 持久化卡密库存并提供订单占用所需的原子状态迁移。
type Repository interface {
	CreateBatch(items []cardsecretdomain.Secret) error
	List(filter ListFilter) ([]cardsecretdomain.Secret, int64, error)
	ListIDs(filter ListFilter) ([]uint, error)
	ListByIDs(ids []uint) ([]cardsecretdomain.Secret, error)
	ListIDsByBatchID(batchID uint) ([]uint, error)
	CountByBatchIDs(batchIDs []uint) ([]BatchStatusCount, error)
	ListByOrderAndStatus(orderID uint, status string) ([]cardsecretdomain.Secret, error)
	ListAvailableByProduct(productID, skuID uint, limit int) ([]cardsecretdomain.Secret, error)
	ListAvailableByProductForUpdate(productID, skuID uint, limit int) ([]cardsecretdomain.Secret, error)
	ListAvailableByProductBatchForUpdate(productID, skuID, batchID uint, limit int) ([]cardsecretdomain.Secret, error)
	GetByID(id uint) (*cardsecretdomain.Secret, error)
	// Update returns ErrNotFound for a missing card or ErrOrderReservation for a card reserved by an order.
	Update(secret *cardsecretdomain.Secret) error
	// BatchUpdateStatus and BatchDeleteByIDs skip order-reserved cards and return the affected row count.
	BatchUpdateStatus(ids []uint, status string, updatedAt time.Time) (int64, error)
	BatchDeleteByIDs(ids []uint) (int64, error)
	CountByProduct(productID, skuID uint) (int64, int64, int64, error)
	CountAvailable(productID, skuID uint) (int64, error)
	CountAvailableByProductIDs(productIDs []uint) (map[uint]int64, error)
	CountReserved(productID, skuID uint) (int64, error)
	CountStockByProductIDs(productIDs []uint) ([]SKUStockCount, error)
	Reserve(ids []uint, orderID uint, reservedAt time.Time) (int64, error)
	ReleaseByOrder(orderID uint) (int64, error)
	MarkUsed(ids []uint, orderID uint, usedAt time.Time) (int64, error)
	DeleteByProduct(productID uint) error
}

// BatchRepository 持久化卡密导入批次。
type BatchRepository interface {
	Create(batch *cardsecretdomain.Batch) error
	GetByID(id uint) (*cardsecretdomain.Batch, error)
	ListByProduct(productID, skuID uint, page, pageSize int) ([]cardsecretdomain.Batch, int64, error)
	DeleteByProduct(productID uint) error
}

// UnitOfWork 在同一事务中暴露库存、批次及商品端口，供录入时锁定商品并复核 SKU。
type UnitOfWork interface {
	Transaction(fn func(secrets Repository, batches BatchRepository, products ProductRepository, skus ProductSKURepository) error) error
}

type ProductRepository interface {
	LockByIDs(ids []uint) error
	GetByID(id string) (*productdomain.Product, error)
}

type ProductSKURepository interface {
	ListByProduct(productID uint, includeInactive bool) ([]productdomain.ProductSKU, error)
	GetByID(id uint) (*productdomain.ProductSKU, error)
	GetByProductAndCode(productID uint, skuCode string) (*productdomain.ProductSKU, error)
}
