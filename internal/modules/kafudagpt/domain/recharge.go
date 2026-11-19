package domain

import (
	"errors"
	"time"

	"github.com/dujiao-next/internal/shared/money"
)

const FulfillmentType = "kafuda_gpt_direct"

type Stage string

const (
	StageWaiting        Stage = "waiting_session"
	StageChecking       Stage = "checking"
	StageSubmitting     Stage = "submitting"
	StageProcessing     Stage = "processing"
	StageManualReview   Stage = "manual_review"
	StageManualDelivery Stage = "manual_delivery"
	StageCompleted      Stage = "completed"
)

const (
	ReasonQuoteRegionLocked           = "quote_region_locked"
	ReasonQuoteExpired                = "quote_expired"
	ReasonQuoteUnavailable            = "quote_unavailable"
	ReasonQuotePolicy                 = "quote_policy"
	ReasonCostLimitExceeded           = "cost_limit_exceeded"
	ReasonResultApplyFailed           = "result_apply_failed"
	ReasonMerchantBalanceInsufficient = "merchant_balance_insufficient"
)

var (
	ErrNotFound          = errors.New("kafuda recharge not found")
	ErrInvalidSnapshot   = errors.New("invalid kafuda recharge snapshot")
	ErrInvalidTransition = errors.New("invalid kafuda recharge transition")
	ErrConflict          = errors.New("kafuda recharge changed concurrently")
	ErrInvalidCatalog    = errors.New("invalid kafuda catalog")
)

// Snapshot contains only immutable commercial identifiers and local prices.
// PaymentProfile identifies deployment configuration, never its secret key.
type Snapshot struct {
	CatalogRevision uint64       `json:"catalog_revision"`
	AllowedKinds    []string     `json:"allowed_kinds,omitempty"`
	ProductID       uint         `json:"product_id"`
	SKUID           uint         `json:"sku_id"`
	Plan            string       `json:"plan"`
	Product         string       `json:"product"`
	Country         string       `json:"country"`
	PayMode         string       `json:"pay_mode"`
	ProductCode     string       `json:"product_code"`
	PaymentProfile  string       `json:"payment_profile"`
	Currency        string       `json:"currency"`
	CostPrice       money.Amount `json:"cost_price"`
	SellPrice       money.Amount `json:"sell_price"`
	MaxCostCents    int64        `json:"max_cost_cents"`
}

func (s Snapshot) Validate() error {
	if s.ProductID == 0 || s.SKUID == 0 || s.Plan == "" || (s.Product != "gpt" && s.Product != "gpt_credits") || s.Country == "" || s.PayMode != "auto_card" || s.ProductCode == "" || s.PaymentProfile == "" || s.Currency == "" || s.CostPrice.IsNegative() || !s.SellPrice.IsPositive() || s.MaxCostCents <= 0 {
		return ErrInvalidSnapshot
	}
	return nil
}

// Recharge is keyed by the actual fulfillment order, never the checkout parent.
// Its snapshot and internal cost information must not be serialized to buyers.
type Recharge struct {
	OrderID                uint       `gorm:"primaryKey;autoIncrement:false" json:"-"`
	Snapshot               Snapshot   `gorm:"serializer:json;type:json;not null" json:"-"`
	Stage                  Stage      `gorm:"type:varchar(32);not null;index;index:idx_kafuda_gpt_recharges_completed_at,priority:1,where:completed_at IS NOT NULL" json:"-"`
	AttemptCount           uint       `gorm:"not null;default:0" json:"-"`
	CurrentAttemptID       *uint      `json:"-"`
	Version                uint64     `gorm:"not null;default:1" json:"-"`
	ReasonCode             string     `gorm:"type:varchar(64);not null;default:''" json:"-"`
	CreatedAt              time.Time  `json:"-"`
	UpdatedAt              time.Time  `json:"-"`
	CompletedAt            *time.Time `gorm:"index:idx_kafuda_gpt_recharges_completed_at,priority:2" json:"-"`
	CompletionEmailPending bool       `gorm:"not null;default:false;index" json:"-"`
	CompletionEmailRetryAt *time.Time `json:"-"`
}

func (Recharge) TableName() string { return "kafuda_gpt_recharges" }

type AttemptStatus string

const (
	AttemptChecking   AttemptStatus = "checking"
	AttemptSubmitting AttemptStatus = "submitting"
	AttemptProcessing AttemptStatus = "processing"
	AttemptReview     AttemptStatus = "needs_review"
	AttemptCompleted  AttemptStatus = "completed"
	AttemptFailed     AttemptStatus = "failed"
	AttemptCancelled  AttemptStatus = "cancelled"
)

// The nullable unique ActiveOrderID enforces at most one live attempt per order
// in both SQLite and PostgreSQL. Unknown outcomes retain this slot.
type Attempt struct {
	ID               uint          `gorm:"primaryKey" json:"-"`
	OrderID          uint          `gorm:"not null;index;uniqueIndex:idx_kafuda_gpt_attempt_sequence" json:"-"`
	Sequence         uint          `gorm:"not null;uniqueIndex:idx_kafuda_gpt_attempt_sequence" json:"-"`
	ActiveOrderID    *uint         `gorm:"uniqueIndex:idx_kafuda_gpt_active_order;check:chk_kafuda_gpt_active_attempt,((status IN ('checking','submitting','processing','needs_review') AND active_order_id IS NOT NULL AND active_order_id = order_id) OR (status IN ('completed','failed','cancelled') AND active_order_id IS NULL))" json:"-"`
	Reference        string        `gorm:"type:varchar(80);not null;uniqueIndex" json:"-"`
	QuoteID          string        `gorm:"type:varchar(128);not null;default:''" json:"-"`
	Status           AttemptStatus `gorm:"type:varchar(32);not null;index" json:"-"`
	UpstreamOrderID  *uint64       `gorm:"uniqueIndex" json:"-"`
	MaskedEmail      string        `gorm:"type:varchar(32);not null;default:''" json:"-"`
	ChargedUSDCents  *int64        `json:"-"`
	RenewalCancelled *bool         `json:"-"`
	NextPollAt       *time.Time    `gorm:"index" json:"-"`
	// NextCardChargePollAt schedules reconciliation of a completed attempt's missing card charge.
	NextCardChargePollAt *time.Time `json:"-"`
	CreatedAt            time.Time  `json:"-"`
	UpdatedAt            time.Time  `json:"-"`
}

func (Attempt) TableName() string { return "kafuda_gpt_attempts" }

func (a Attempt) Validate() error {
	if a.OrderID == 0 || a.Sequence == 0 || len(a.Reference) == 0 || len(a.Reference) > 80 || len(a.QuoteID) > 128 || !a.Status.CanTransition(a.Status) {
		return ErrInvalidTransition
	}
	for _, ch := range a.Reference {
		if !((ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '.' || ch == '_' || ch == ':' || ch == '-') {
			return ErrInvalidTransition
		}
	}
	if a.Status.Active() {
		if a.ActiveOrderID == nil || *a.ActiveOrderID != a.OrderID || a.NextPollAt == nil {
			return ErrInvalidTransition
		}
	} else if a.ActiveOrderID != nil || a.NextPollAt != nil {
		return ErrInvalidTransition
	}
	if (a.UpstreamOrderID != nil && *a.UpstreamOrderID == 0) || (a.ChargedUSDCents != nil && *a.ChargedUSDCents < 0) {
		return ErrInvalidTransition
	}
	return nil
}

func (s AttemptStatus) Active() bool {
	return s == AttemptChecking || s == AttemptSubmitting || s == AttemptProcessing || s == AttemptReview
}

// CanTransition prevents stale polling events from undoing terminal outcomes.
// needs_review may recover to processing or converge to an upstream terminal.
func (s AttemptStatus) CanTransition(next AttemptStatus) bool {
	if s == next {
		return s.Active() || s == AttemptCompleted || s == AttemptFailed || s == AttemptCancelled
	}
	switch s {
	case AttemptChecking:
		return next == AttemptSubmitting || next == AttemptFailed
	case AttemptSubmitting:
		return next == AttemptProcessing || next == AttemptReview || next == AttemptCompleted || next == AttemptFailed || next == AttemptCancelled
	case AttemptProcessing:
		return next == AttemptReview || next == AttemptCompleted || next == AttemptFailed || next == AttemptCancelled
	case AttemptReview:
		return next == AttemptProcessing || next == AttemptCompleted || next == AttemptFailed || next == AttemptCancelled
	default:
		return false
	}
}
