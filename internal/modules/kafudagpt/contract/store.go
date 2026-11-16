package contract

import (
	"context"
	"time"

	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
)

type Store interface {
	WithinTransaction(context.Context, func(Transaction) error) error
	GetRecharge(context.Context, uint) (*kafudadomain.Recharge, error)
	GetAttempt(context.Context, uint) (*kafudadomain.Attempt, error)
	DueAttempts(context.Context, time.Time, int) ([]kafudadomain.Attempt, error)
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
