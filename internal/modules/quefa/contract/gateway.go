package contract

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/shopspring/decimal"
)

// PollHints carries server-provided rate-limit and polling cadence hints from response headers.
type PollHints struct {
	RetryAfter         time.Duration
	PollAfter          time.Duration
	PollStop           bool
	RateLimitLimit     int
	RateLimitRemaining int
	RateLimitReset     *time.Time
}

// APIError represents a classified error returned by the Quefa Partner API.
type APIError struct {
	StatusCode int
	Code       string
	Message    string
	Category   string // auth, forbidden, not_found, rate_limited, idempotency_in_progress, insufficient_balance, upstream_error, network_error, invalid_response, redirect_blocked
	Retryable  bool
	Hints      PollHints
}

func (e *APIError) Error() string {
	if e == nil {
		return ""
	}
	base := fmt.Sprintf("quefa api error (%s, http=%d)", e.Category, e.StatusCode)
	if e.Code != "" {
		base = fmt.Sprintf("quefa api error (%s, http=%d, code=%s)", e.Category, e.StatusCode, e.Code)
	}
	if e.Message != "" {
		return base + ": " + e.Message
	}
	return base
}

func IsAPIErrorCategory(err error, category string) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Category == category
	}
	return false
}

// CreateOrderRequest is the JSON body sent to POST /v1/orders.
// Per Quefa OpenAPI 1.4.3, additionalProperties is false; do not add undocumented fields.
type CreateOrderRequest struct {
	MerchantOrderNo string `json:"merchant_order_no"`
	ProductCode     string `json:"product_code"`
	Quantity        int    `json:"quantity"`
	SaleAmount      string `json:"sale_amount"`
	CollectionMode  string `json:"collection_mode,omitempty"`
	DeliveryMode    string `json:"delivery_mode,omitempty"`
	PaymentChannel  string `json:"payment_channel,omitempty"`
}

// PaymentCodeSnapshot is the response from POST /v1/orders/{order_id}/payment-code.
type PaymentCodeSnapshot struct {
	OrderID         string           `json:"order_id"`
	MerchantOrderNo string           `json:"merchant_order_no"`
	Amount          *decimal.Decimal `json:"amount,omitempty"`
	Currency        string           `json:"currency"`
	PaymentStatus   string           `json:"payment_status"`
	PaymentCodeType string           `json:"payment_code_type"`
	PaymentCode     string           `json:"payment_code"`
	QRImageDataURL  string           `json:"qr_image_data_url"`
	ExpiresAt       *time.Time       `json:"expires_at,omitempty"`
	Hints           PollHints        `json:"-"`
}

// OrderSnapshot is the normalized Quefa order payload returned by POST /v1/orders or GET /v1/orders/{id}.
type OrderSnapshot struct {
	OrderID                   string           `json:"order_id"`
	MerchantOrderNo           string           `json:"merchant_order_no"`
	ProductCode               string           `json:"product_code"`
	Status                    string           `json:"status"`
	PaymentStatus             string           `json:"payment_status"`
	FulfillmentStatus         string           `json:"fulfillment_status"`
	FulfillmentPaused         bool             `json:"fulfillment_paused"`
	SyncMark                  string           `json:"sync_mark"`
	FallbackRechargeAvailable bool             `json:"fallback_recharge_available"`
	ResultCode                string           `json:"result_code"`
	CollectionMode            string           `json:"collection_mode"`
	DeliveryMode              string           `json:"delivery_mode"`
	SaleAmount                *decimal.Decimal `json:"sale_amount,omitempty"`
	PaidAmount                *decimal.Decimal `json:"paid_amount,omitempty"`
	Currency                  string           `json:"currency"`
	PayURL                    string           `json:"pay_url"`
	QRCode                    string           `json:"qr_code"`
	PayExpiresAt              *time.Time       `json:"pay_expires_at,omitempty"`
	FulfillmentURL            string           `json:"fulfillment_url"`
	LatestRedemptionID        string           `json:"latest_redemption_id"`
	AttemptNo                 int              `json:"attempt_no"`
	ProgressVersion           int64            `json:"progress_version"`
	ProgressStage             string           `json:"progress_stage"`
	ProgressMessage           string           `json:"progress_message"`
	ProgressUpdatedAt         *time.Time       `json:"progress_updated_at,omitempty"`
	RetryAllowed              bool             `json:"retry_allowed"`
	NextAction                string           `json:"next_action"`
	RecoveryAction            string           `json:"recovery_action"`
	RefundRef                 string           `json:"refund_ref"`
	ProcurementRefundRef      string           `json:"procurement_refund_ref"`
	AcceptedAsync             bool             `json:"-"`
	Hints                     PollHints        `json:"-"`
}

// RedemptionSnapshot is the normalized response from GET /v1/redemptions/{id}.
type RedemptionSnapshot struct {
	RedemptionID       string     `json:"redemption_id"`
	OrderID            string     `json:"order_id"`
	LatestRedemptionID string     `json:"latest_redemption_id"`
	Status             string     `json:"status"`
	ResultCode         string     `json:"result_code"`
	AttemptNo          int        `json:"attempt_no"`
	ProgressVersion    int64      `json:"progress_version"`
	ProgressStage      string     `json:"progress_stage"`
	ProgressMessage    string     `json:"progress_message"`
	ProgressUpdatedAt  *time.Time `json:"progress_updated_at,omitempty"`
	RetryAllowed       bool       `json:"retry_allowed"`
	NextAction         string     `json:"next_action"`
	RecoveryAction     string     `json:"recovery_action"`
	FulfillmentURL     string     `json:"fulfillment_url"`
	Hints              PollHints  `json:"-"`
}

// Gateway defines the outbound Quefa Partner API V1 port.
type Gateway interface {
	GetProducts(ctx context.Context) (json.RawMessage, PollHints, error)
	CreateOrder(ctx context.Context, req CreateOrderRequest, idempotencyKey string) (*OrderSnapshot, error)
	CreateOrderPaymentCode(ctx context.Context, upstreamOrderID string, idempotencyKey string) (*PaymentCodeSnapshot, error)
	GetOrder(ctx context.Context, upstreamOrderID string) (*OrderSnapshot, error)
	LookupOrderByMerchantNo(ctx context.Context, merchantOrderNo string) (*OrderSnapshot, error)
	GetRedemption(ctx context.Context, redemptionID string) (*RedemptionSnapshot, error)
}
