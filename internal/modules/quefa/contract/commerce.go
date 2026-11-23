package contract

import (
	"context"
	"errors"
	"time"

	"github.com/shopspring/decimal"
)

var (
	ErrSalesDisabled      = errors.New("quefa sales disabled")
	ErrCatalogStale       = errors.New("quefa catalog stale or missing")
	ErrProductUnavailable = errors.New("quefa product unavailable")
	ErrCurrencyMismatch   = errors.New("quefa settlement currency mismatch")
	ErrBelowCostFloor     = errors.New("quefa discounted amount below cost floor")
	ErrCostNotConfigured  = errors.New("quefa sku cost not configured")
	ErrWalletOnlyBlocked  = errors.New("quefa product unavailable in wallet-only payment mode")
)

// PurchaseTerms holds validated purchase eligibility and frozen pricing floor for one Quefa SKU.
type PurchaseTerms struct {
	ProductCode        string
	CollectionMode     string
	DeliveryMode       string
	SupplyPrice        decimal.Decimal
	AdminCostPrice     decimal.Decimal
	CostFloor          decimal.Decimal
	SettlementCurrency string
	CatalogRevision    int64
}

// CommercePolicy validates Quefa product sellability and pricing floor during browsing, preview, and checkout.
type CommercePolicy interface {
	ResolvePurchaseTerms(ctx context.Context, productCode string, adminCost decimal.Decimal, orderCurrency string, walletOnly bool) (*PurchaseTerms, error)
	IsSellable(ctx context.Context, productCode string, adminCost decimal.Decimal, siteCurrency string, walletOnly bool) bool
	SalesReadiness(ctx context.Context) SalesReadiness
}

// SalesReadiness summarizes why Quefa hosted sales are ready or blocked for admin visibility.
type SalesReadiness struct {
	Ready          bool     `json:"ready"`
	BlockReasons   []string `json:"block_reasons"`
	CollectionMode string   `json:"collection_mode"`
}

// BuyerStatus is the sanitized buyer-facing hosted-recharge status (never exposes cost, secrets, or fulfillment_url).
type BuyerStatus struct {
	Stage              string     `json:"stage"`
	ProgressStage      string     `json:"progress_stage,omitempty"`
	ProgressMessage    string     `json:"progress_message,omitempty"`
	ProgressUpdatedAt  *time.Time `json:"progress_updated_at,omitempty"`
	CanOpenFulfillment bool       `json:"can_open_fulfillment"`
	UpdatedAt          time.Time  `json:"updated_at"`
	CompletedAt        *time.Time `json:"completed_at,omitempty"`
}

// FulfillmentEntryResult is returned only by authorized buyer actions when opening the hosted page.
type FulfillmentEntryResult struct {
	FulfillmentURL string `json:"fulfillment_url"`
	Stage          string `json:"stage"`
}

// AdminOrderStatus is the operator-facing Quefa status for order detail & support reconciliation.
type AdminOrderStatus struct {
	OrderID              uint       `json:"order_id"`
	ParentOrderID        uint       `json:"parent_order_id"`
	ProductCode          string     `json:"product_code"`
	CollectionMode       string     `json:"collection_mode"`
	DeliveryMode         string     `json:"delivery_mode"`
	CostFloorAmount      string     `json:"cost_floor_amount"`
	SaleAmount           string     `json:"sale_amount"`
	SettlementCurrency   string     `json:"settlement_currency"`
	MerchantOrderNo      string     `json:"merchant_order_no"`
	UpstreamOrderID      string     `json:"upstream_order_id,omitempty"`
	LatestRedemptionID   string     `json:"latest_redemption_id,omitempty"`
	AttemptNo            int        `json:"attempt_no"`
	ProgressVersion      int64      `json:"progress_version"`
	ProgressStage        string     `json:"progress_stage,omitempty"`
	ProgressMessage      string     `json:"progress_message,omitempty"`
	ProgressUpdatedAt    *time.Time `json:"progress_updated_at,omitempty"`
	Stage                string     `json:"stage"`
	UpstreamStatus       string     `json:"upstream_status,omitempty"`
	ResultCode           string     `json:"result_code,omitempty"`
	RetryAllowed         bool       `json:"retry_allowed"`
	NextAction           string     `json:"next_action,omitempty"`
	RecoveryAction       string     `json:"recovery_action,omitempty"`
	RefundRef            string     `json:"refund_ref,omitempty"`
	ProcurementRefundRef string     `json:"procurement_refund_ref,omitempty"`
	ExceptionCode        string     `json:"exception_code,omitempty"`
	ReviewRemark         string     `json:"review_remark,omitempty"`
	PollStopped          bool       `json:"poll_stopped"`
	NextPollAt           *time.Time `json:"next_poll_at,omitempty"`
	UpdatedAt            time.Time  `json:"updated_at"`
	CompletedAt          *time.Time `json:"completed_at,omitempty"`
}

// WebhookEnvelope is the parsed Quefa webhook JSON payload.
type WebhookEnvelope struct {
	EventID         string `json:"event_id"`
	EventType       string `json:"event_type"`
	Type            string `json:"type"`
	OrderID         string `json:"order_id"`
	MerchantOrderNo string `json:"merchant_order_no"`
	RedemptionID    string `json:"redemption_id"`
	RefundRef       string `json:"refund_ref"`
}

func (e WebhookEnvelope) EffectiveType() string {
	if e.EventType != "" {
		return e.EventType
	}
	return e.Type
}
