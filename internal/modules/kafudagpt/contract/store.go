package contract

import (
	"context"
	"time"

	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
)

type CheckoutSnapshot kafudadomain.Snapshot

type CheckoutInput struct {
	OrderID   uint
	Snapshot  CheckoutSnapshot
	CreatedAt time.Time
}

// CatalogForUpdate reads the cache through the current database transaction.
type CatalogForUpdate interface {
	GetCatalogForUpdate(string) (*kafudadomain.Catalog, error)
}

// CheckoutRepository is bound to the order creation transaction.
type CheckoutRepository interface {
	CatalogForUpdate
	CreateCheckout(CheckoutInput) error
}

type Store interface {
	WithinTransaction(context.Context, func(Transaction) error) error
	GetRecharge(context.Context, uint) (*kafudadomain.Recharge, error)
	GetAttempt(context.Context, uint) (*kafudadomain.Attempt, error)
	PendingCompletionEmails(context.Context, time.Time, int) ([]uint, error)
	ClaimCompletionEmail(context.Context, uint, time.Time, time.Time) (bool, error)
	ClearCompletionEmail(context.Context, uint) error
	DueAttempts(context.Context, time.Time, int) ([]kafudadomain.Attempt, error)
	DueKnownOrderPolls(context.Context, time.Time, int) ([]kafudadomain.Attempt, error)
	ClaimKnownOrderPoll(ctx context.Context, candidate kafudadomain.Attempt, now, leaseUntil time.Time) (bool, error)
	DueCardChargePolls(ctx context.Context, now, completedAfter, completedBefore time.Time, limit int) ([]kafudadomain.Attempt, error)
	ClaimCardChargePoll(ctx context.Context, candidate kafudadomain.Attempt, leaseUntil time.Time) (bool, error)
	SaveCardChargePoll(ctx context.Context, attemptID uint, leaseUntil time.Time, charged *int64, next *time.Time) (bool, error)
	DueCheckingAttempts(context.Context, time.Time, int) ([]kafudadomain.Attempt, error)
	DueUnidentifiedSubmissions(context.Context, time.Time, int) ([]kafudadomain.Attempt, error)
	GetCatalog(context.Context, string) (*kafudadomain.Catalog, error)
	SaveCatalog(context.Context, *kafudadomain.Catalog) error
}

// SettingStore persists encrypted merchant configuration for Kafuda GPT direct recharge.
type SettingStore interface {
	GetSetting(context.Context, string) (*kafudadomain.Setting, error)
	SaveSetting(context.Context, *kafudadomain.Setting) error
}

// Transaction only exposes a transaction-bound adapter. Callers must not call
// the outer Store or perform HTTP while this closure holds SQLite's connection.
type Transaction interface {
	GetRechargeForUpdate(uint) (*kafudadomain.Recharge, error)
	CreateRecharge(*kafudadomain.Recharge) error
	SaveRecharge(*kafudadomain.Recharge) error
	GetAttemptForUpdate(uint) (*kafudadomain.Attempt, error)
	CreateAttempt(*kafudadomain.Attempt) error
	SaveAttempt(*kafudadomain.Attempt) error
}

// CanResolveManualAttempt reports whether the current supplier attempt has
// definitively ended without delivery and is eligible for manual resolution.
func CanResolveManualAttempt(record *kafudadomain.Recharge, attempt *kafudadomain.Attempt) bool {
	return record != nil && record.Stage == kafudadomain.StageManualDelivery && record.CompletedAt == nil &&
		record.CurrentAttemptID != nil && attempt != nil && *record.CurrentAttemptID == attempt.ID &&
		attempt.OrderID == record.OrderID && attempt.ActiveOrderID == nil &&
		(attempt.Status == kafudadomain.AttemptFailed || attempt.Status == kafudadomain.AttemptCancelled)
}

// CanConfirmUpstreamCompletion identifies a supplier-delivered attempt whose
// cost hold still needs operator review. The hold reason records an earlier
// overage even if the last reported debit was lower.
func CanConfirmUpstreamCompletion(record *kafudadomain.Recharge, attempt *kafudadomain.Attempt) bool {
	return record != nil && record.Stage == kafudadomain.StageManualReview &&
		record.ReasonCode == kafudadomain.ReasonCostLimitExceeded && record.CompletedAt == nil &&
		record.Snapshot.MaxCostCents > 0 && record.CurrentAttemptID != nil &&
		attempt != nil && attempt.ID > 0 && *record.CurrentAttemptID == attempt.ID &&
		attempt.OrderID == record.OrderID && attempt.Status == kafudadomain.AttemptCompleted &&
		attempt.UpstreamOrderID != nil && attempt.ChargedUSDCents != nil && attempt.Validate() == nil
}
