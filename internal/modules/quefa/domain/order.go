package domain

import (
	"errors"
	"strings"
	"time"

	"github.com/dujiao-next/internal/shared/money"
	"github.com/shopspring/decimal"
)

type Stage string

const (
	StageWaitingPayment     Stage = "waiting_payment"
	StageWaitingProcurement Stage = "waiting_procurement"
	StageProcurementBlocked Stage = "procurement_blocked"
	StageWaitingSubmit      Stage = "waiting_submit"
	StageRecharging         Stage = "recharging"
	StageResubmitRequired   Stage = "resubmit_required"
	StageManualReview       Stage = "manual_review"
	StageRefunding          Stage = "refunding"
	StageSucceeded          Stage = "succeeded"
	StageClosed             Stage = "closed"
	StageRefunded           Stage = "refunded"
)

const (
	WebhookStatusReceived  = "received"
	WebhookStatusProcessed = "processed"
	WebhookStatusIgnored   = "ignored"
	WebhookStatusFailed    = "failed"
)

var (
	ErrConflict              = errors.New("quefa record conflict")
	ErrOrderNotFound         = errors.New("quefa order not found")
	ErrInvalidOrderSnapshot  = errors.New("quefa order snapshot invalid")
	ErrFulfillmentNotAllowed = errors.New("quefa fulfillment entry not allowed")
	ErrCancelBlockedByActive = errors.New("quefa order cannot be canceled while upstream is still payable")
)

// IsTerminal reports whether a Quefa order stage is final and cannot regress.
func (s Stage) IsTerminal() bool {
	return s == StageSucceeded || s == StageClosed || s == StageRefunded
}

// CanOpenFulfillmentEntry reports whether the buyer is allowed to open the hosted recharge URL.
func (s Stage) CanOpenFulfillmentEntry() bool {
	return s == StageWaitingSubmit || s == StageResubmitRequired
}

// Order tracks the Quefa hosted direct-recharge lifecycle bound 1:1 to a shop fulfillment child order.
type Order struct {
	ID                     uint         `gorm:"primaryKey" json:"id"`
	OrderID                uint         `gorm:"uniqueIndex;not null" json:"order_id"`
	ParentOrderID          uint         `gorm:"index;not null" json:"parent_order_id"`
	ProductID              uint         `gorm:"index;not null" json:"product_id"`
	SKUID                  uint         `gorm:"index;not null" json:"sku_id"`
	ProductCode            string       `gorm:"type:varchar(128);not null" json:"product_code"`
	CollectionMode         string       `gorm:"type:varchar(32);not null" json:"collection_mode"`
	DeliveryMode           string       `gorm:"type:varchar(32);not null" json:"delivery_mode"`
	CostFloorAmount        money.Amount `gorm:"type:decimal(20,2);not null;default:0" json:"cost_floor_amount"`
	SaleAmount             money.Amount `gorm:"type:decimal(20,2);not null;default:0" json:"sale_amount"`
	SettlementCurrency     string       `gorm:"type:varchar(16);not null;default:'CNY'" json:"settlement_currency"`
	MerchantOrderNo        string       `gorm:"type:varchar(128);uniqueIndex;not null" json:"merchant_order_no"`
	IdempotencyKey         string       `gorm:"type:varchar(128);not null;default:''" json:"idempotency_key"`
	CreateRequestSnapshot  string       `gorm:"type:text;not null;default:''" json:"-"`
	CreateDispatched       bool         `gorm:"not null;default:false" json:"create_dispatched"`
	UpstreamOrderID        *string      `gorm:"type:varchar(128);uniqueIndex" json:"upstream_order_id,omitempty"`
	LatestRedemptionID     string       `gorm:"type:varchar(128);not null;default:''" json:"latest_redemption_id,omitempty"`
	AttemptNo              int          `gorm:"not null;default:0" json:"attempt_no"`
	ProgressVersion        int64        `gorm:"not null;default:0" json:"progress_version"`
	ProgressStage          string       `gorm:"type:varchar(64);not null;default:''" json:"progress_stage,omitempty"`
	ProgressMessage        string       `gorm:"type:varchar(255);not null;default:''" json:"progress_message,omitempty"`
	ProgressUpdatedAt      *time.Time   `json:"progress_updated_at,omitempty"`
	Stage                  Stage        `gorm:"type:varchar(32);index;not null" json:"stage"`
	UpstreamStatus         string       `gorm:"type:varchar(64);not null;default:''" json:"upstream_status,omitempty"`
	ResultCode             string       `gorm:"type:varchar(64);not null;default:''" json:"result_code,omitempty"`
	RetryAllowed           bool         `gorm:"not null;default:false" json:"retry_allowed"`
	NextAction             string       `gorm:"type:varchar(64);not null;default:''" json:"next_action,omitempty"`
	RecoveryAction         string       `gorm:"type:varchar(64);not null;default:''" json:"recovery_action,omitempty"`
	FulfillmentURL         string       `gorm:"type:text;not null;default:''" json:"-"`
	PayURL                 string       `gorm:"type:text;not null;default:''" json:"-"`
	QRCode                 string       `gorm:"type:text;not null;default:''" json:"-"`
	PayExpiresAt           *time.Time   `json:"pay_expires_at,omitempty"`
	RefundRef              string       `gorm:"type:varchar(128);not null;default:''" json:"refund_ref,omitempty"`
	ProcurementRefundRef   string       `gorm:"type:varchar(128);not null;default:''" json:"procurement_refund_ref,omitempty"`
	ExceptionCode          string       `gorm:"type:varchar(64);not null;default:''" json:"exception_code,omitempty"`
	ReviewRemark           string       `gorm:"type:varchar(500);not null;default:''" json:"review_remark,omitempty"`
	NextPollAt             *time.Time   `gorm:"index" json:"next_poll_at,omitempty"`
	PollStopped            bool         `gorm:"not null;default:false" json:"poll_stopped"`
	CompletionEmailPending bool         `gorm:"not null;default:false" json:"-"`
	CompletionEmailRetryAt *time.Time   `json:"-"`
	Version                int64        `gorm:"not null;default:1" json:"version"`
	CreatedAt              time.Time    `gorm:"not null" json:"created_at"`
	UpdatedAt              time.Time    `gorm:"not null" json:"updated_at"`
	CompletedAt            *time.Time   `json:"completed_at,omitempty"`
}

func (Order) TableName() string { return "quefa_orders" }

// ValidateSnapshot ensures immutable checkout snapshot fields are present and valid.
func (o *Order) ValidateSnapshot() error {
	if o == nil || o.OrderID == 0 || o.ParentOrderID == 0 || o.ProductID == 0 || o.SKUID == 0 {
		return ErrInvalidOrderSnapshot
	}
	if strings.TrimSpace(o.ProductCode) == "" || strings.TrimSpace(o.MerchantOrderNo) == "" {
		return ErrInvalidOrderSnapshot
	}
	if !IsValidCollectionMode(o.CollectionMode) || o.DeliveryMode != DeliveryModeAutoRecharge {
		return ErrInvalidOrderSnapshot
	}
	if o.CostFloorAmount.Decimal.LessThanOrEqual(decimal.Zero) {
		return ErrInvalidOrderSnapshot
	}
	if strings.TrimSpace(o.SettlementCurrency) == "" {
		return ErrInvalidOrderSnapshot
	}
	return nil
}

// ShouldIgnoreProgress reports whether incoming attempt/progress metadata is stale.
func (o *Order) ShouldIgnoreProgress(attemptNo int, progressVersion int64) bool {
	if o == nil {
		return true
	}
	if o.Stage.IsTerminal() {
		return true
	}
	if attemptNo > 0 && o.AttemptNo > 0 {
		if attemptNo < o.AttemptNo {
			return true
		}
		if attemptNo > o.AttemptNo {
			return false
		}
	}
	if progressVersion > 0 && o.ProgressVersion > 0 && progressVersion < o.ProgressVersion {
		return true
	}
	return false
}

// WebhookEvent persists deduplicated Quefa webhook deliveries keyed by event_id.
type WebhookEvent struct {
	ID              uint       `gorm:"primaryKey" json:"id"`
	EventID         string     `gorm:"type:varchar(128);uniqueIndex;not null" json:"event_id"`
	EventType       string     `gorm:"type:varchar(64);index;not null" json:"event_type"`
	UpstreamOrderID string     `gorm:"type:varchar(128);index;not null;default:''" json:"upstream_order_id"`
	MerchantOrderNo string     `gorm:"type:varchar(128);index;not null;default:''" json:"merchant_order_no"`
	Status          string     `gorm:"type:varchar(32);not null;default:'received'" json:"status"`
	PayloadSummary  string     `gorm:"type:varchar(500);not null;default:''" json:"payload_summary"`
	ReceivedAt      time.Time  `gorm:"not null" json:"received_at"`
	ProcessedAt     *time.Time `json:"processed_at,omitempty"`
}

func (WebhookEvent) TableName() string { return "quefa_webhook_events" }
