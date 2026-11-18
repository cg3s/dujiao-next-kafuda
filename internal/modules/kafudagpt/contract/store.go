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

// CheckoutRepository is bound to the order creation transaction.
type CheckoutRepository interface {
	GetCatalogForUpdate(string) (*kafudadomain.Catalog, error)
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
	DueCheckingAttempts(context.Context, time.Time, int) ([]kafudadomain.Attempt, error)
	DueUnidentifiedSubmissions(context.Context, time.Time, int) ([]kafudadomain.Attempt, error)
	GetCatalog(context.Context, string) (*kafudadomain.Catalog, error)
	SaveCatalog(context.Context, *kafudadomain.Catalog) error
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
