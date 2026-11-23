package contract

import (
	"context"
	"time"

	quefadomain "github.com/dujiao-next/internal/modules/quefa/domain"
	"github.com/dujiao-next/internal/shared/money"
)

// CheckoutInput carries immutable Quefa snapshot fields persisted inside the order creation transaction.
type CheckoutInput struct {
	OrderID            uint
	ParentOrderID      uint
	ProductID          uint
	SKUID              uint
	ProductCode        string
	CollectionMode     string
	DeliveryMode       string
	CostFloorAmount    money.Amount
	SettlementCurrency string
	MerchantOrderNo    string
	CreatedAt          time.Time
}

// CheckoutRepository is passed into order creation transactions to atomically persist quefa_orders.
type CheckoutRepository interface {
	CreateCheckout(input CheckoutInput) error
}

// SettingStore persists encrypted Quefa global configuration.
type SettingStore interface {
	GetSetting(ctx context.Context, profile string) (*quefadomain.Setting, error)
	SaveSetting(ctx context.Context, setting *quefadomain.Setting) error
}

// Transaction exposes transactional Quefa store methods bound to an active DB transaction.
type Transaction interface {
	GetCatalog(profile string) (*quefadomain.Catalog, error)
	GetCatalogForUpdate(profile string) (*quefadomain.Catalog, error)
	SaveCatalog(catalog *quefadomain.Catalog) error

	GetOrderByOrderID(orderID uint) (*quefadomain.Order, error)
	GetOrderByOrderIDForUpdate(orderID uint) (*quefadomain.Order, error)
	GetOrderByParentOrderID(parentOrderID uint) (*quefadomain.Order, error)
	GetOrderByParentOrderIDForUpdate(parentOrderID uint) (*quefadomain.Order, error)
	GetOrderByUpstreamIDForUpdate(upstreamOrderID string) (*quefadomain.Order, error)
	GetOrderByMerchantNoForUpdate(merchantOrderNo string) (*quefadomain.Order, error)
	CreateOrder(order *quefadomain.Order) error
	SaveOrder(order *quefadomain.Order) error

	InsertWebhookEventIfAbsent(event *quefadomain.WebhookEvent) (inserted bool, err error)
	UpdateWebhookEventStatus(eventID, status, summary string, processedAt *time.Time) error
}

// Store is the top-level persistence contract for the Quefa module.
type Store interface {
	SettingStore
	CheckoutRepository
	WithinTransaction(ctx context.Context, fn func(Transaction) error) error

	GetCatalog(ctx context.Context, profile string) (*quefadomain.Catalog, error)
	GetOrderByOrderID(ctx context.Context, orderID uint) (*quefadomain.Order, error)
	GetOrderByParentOrderID(ctx context.Context, parentOrderID uint) (*quefadomain.Order, error)
	GetOrderByUpstreamID(ctx context.Context, upstreamOrderID string) (*quefadomain.Order, error)
	GetOrderByMerchantNo(ctx context.Context, merchantOrderNo string) (*quefadomain.Order, error)
	ListDuePollOrderIDs(ctx context.Context, now time.Time, limit int) ([]uint, error)
	ListPendingCompletionEmails(ctx context.Context, now time.Time, limit int) ([]uint, error)
	ClaimCompletionEmail(ctx context.Context, orderID uint, now, retryAt time.Time) (bool, error)
	ClearCompletionEmail(ctx context.Context, orderID uint) error
}

// CanRefundShopOrder reports whether a Quefa order is eligible for shop-side refund.
func CanRefundShopOrder(qOrder *quefadomain.Order) bool {
	if qOrder == nil {
		return false
	}
	if qOrder.Stage == quefadomain.StageSucceeded || qOrder.Stage == quefadomain.StageRecharging {
		return false
	}
	if qOrder.CollectionMode == quefadomain.CollectionModePlatformCollect && qOrder.Stage != quefadomain.StageRefunded {
		return false
	}
	return true
}

// MarkOrderRefunded transitions a Quefa order into terminal StageRefunded inside an active transaction.
func MarkOrderRefunded(qOrder *quefadomain.Order, now time.Time) bool {
	if qOrder == nil || qOrder.Stage == quefadomain.StageRefunded {
		return false
	}
	qOrder.Stage = quefadomain.StageRefunded
	qOrder.RetryAllowed = false
	qOrder.PollStopped = true
	qOrder.NextPollAt = nil
	qOrder.UpdatedAt = now
	return true
}
