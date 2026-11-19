package contract

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

var (
	ErrCreateQuoteExpired          = errors.New("kafuda rejected an expired order quote")
	ErrMerchantBalanceInsufficient = errors.New("kafuda rejected an order for insufficient merchant balance")
)

// Gateway is the server-side Kafuda API boundary. Session must never be persisted
// or placed in a background job; only Check consumes it synchronously.
type Gateway interface {
	Catalog(context.Context) (json.RawMessage, error)
	Check(context.Context, CheckRequest) (*CheckResult, error)
	CreateOrder(context.Context, CreateOrderRequest) (*OrderResult, error)
	GetOrder(context.Context, uint64) (*OrderResult, error)
}

type CheckRequest struct {
	Country     string `json:"country"`
	PayMode     string `json:"pay_mode"`
	ProductCode string `json:"product_code,omitempty"`
	Session     string `json:"session"`
}

func (r CheckRequest) String() string { return "Kafuda GPT check request [credentials redacted]" }

func (r CheckRequest) GoString() string { return r.String() }

type CheckResult struct {
	CheckID      string
	PayMode      string
	Country      string
	RegionLocked bool
	MaskedEmail  string
	Plans        []Quote
}

type Quote struct {
	Plan               string    `json:"plan"`
	Status             string    `json:"status"`
	Kind               string    `json:"kind"`
	QuoteID            string    `json:"quote_id"`
	Currency           string    `json:"currency"`
	PaymentAmountMinor int64     `json:"payment_amount_minor"`
	MinorExponent      int       `json:"minor_exponent"`
	PlanUSDCents       int64     `json:"plan_usd_cents"`
	PriceCents         int64     `json:"price_cents"`
	ExpiresAt          time.Time `json:"expires_at"`
	AutoCard           *AutoCard `json:"auto_card"`
}

type AutoCard struct {
	ProductCode  string `json:"product_code"`
	OpenFeeCents int64  `json:"open_fee_cents"`
	LoadCents    int64  `json:"load_cents"`
	TotalCents   int64  `json:"total_cents"`
}

// CreateOrderRequest contains no Session. A persisted request may only be
// replayed byte-for-byte with its original Reference and Idempotency-Key.
type CreateOrderRequest struct {
	QuoteID   string `json:"quote_id"`
	Reference string `json:"reference"`
}

type OrderResult struct {
	ID               uint64
	Reference        string
	Product          string
	Plan             string
	Country          string
	Status           string
	MaskedEmail      string
	ChargedUSDCents  *int64
	RenewalCancelled *bool
	CompletedAt      *time.Time
}

// APIError carries a local classification. It excludes upstream bodies,
// upstream error codes, request URLs and transport errors.
type APIError struct {
	HTTPStatus int
	Code       string
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	return fmt.Sprintf("kafuda request failed (HTTP %d, code %s)", e.HTTPStatus, e.Code)
}
