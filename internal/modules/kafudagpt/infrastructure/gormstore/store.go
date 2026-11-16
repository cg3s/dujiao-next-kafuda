package gormstore

import (
	"context"
	"errors"
	"time"

	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Store struct{ db *gorm.DB }

var _ kafudacontract.Store = (*Store)(nil)
var _ kafudacontract.Transaction = (*Store)(nil)

func New(db *gorm.DB) *Store { return &Store{db: db} }

func (s *Store) WithinTransaction(ctx context.Context, fn func(kafudacontract.Transaction) error) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error { return fn(New(tx)) })
}

func (s *Store) GetRecharge(ctx context.Context, orderID uint) (*kafudadomain.Recharge, error) {
	return first[kafudadomain.Recharge](s.db.WithContext(ctx).Where("order_id = ?", orderID))
}

func (s *Store) GetRechargeForUpdate(orderID uint) (*kafudadomain.Recharge, error) {
	return first[kafudadomain.Recharge](s.db.Clauses(clause.Locking{Strength: "UPDATE"}).Where("order_id = ?", orderID))
}

func (s *Store) CreateRecharge(record *kafudadomain.Recharge) error {
	if record.OrderID == 0 || record.Stage != kafudadomain.StageWaiting || record.Version != 1 {
		return kafudadomain.ErrInvalidSnapshot
	}
	if err := record.Snapshot.Validate(); err != nil {
		return err
	}
	return s.db.Create(record).Error
}

// SaveRecharge deliberately does not update Snapshot: it is immutable even when
// product prices, catalog availability, or deployment configuration change.
func (s *Store) SaveRecharge(record *kafudadomain.Recharge) error {
	result := s.db.Model(&kafudadomain.Recharge{}).
		Where("order_id = ? AND version = ?", record.OrderID, record.Version).
		Updates(map[string]any{
			"stage": record.Stage, "attempt_count": record.AttemptCount, "current_attempt_id": record.CurrentAttemptID,
			"reason_code": record.ReasonCode, "updated_at": record.UpdatedAt, "completed_at": record.CompletedAt,
			"version": record.Version + 1,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return kafudadomain.ErrConflict
	}
	record.Version++
	return nil
}

func (s *Store) GetAttempt(ctx context.Context, id uint) (*kafudadomain.Attempt, error) {
	return first[kafudadomain.Attempt](s.db.WithContext(ctx).Where("id = ?", id))
}

func (s *Store) GetAttemptForUpdate(id uint) (*kafudadomain.Attempt, error) {
	return first[kafudadomain.Attempt](s.db.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", id))
}

func (s *Store) CreateAttempt(record *kafudadomain.Attempt) error {
	if err := record.Validate(); err != nil {
		return err
	}
	return s.db.Create(record).Error
}

func (s *Store) SaveAttempt(record *kafudadomain.Attempt) error {
	if err := record.Validate(); err != nil {
		return err
	}
	// Do not allow an update to change the order, sequence or reference.
	result := s.db.Model(&kafudadomain.Attempt{}).Where("id = ?", record.ID).Updates(map[string]any{
		"quote_id": record.QuoteID,
		"status":   record.Status, "active_order_id": record.ActiveOrderID, "upstream_order_id": record.UpstreamOrderID,
		"masked_email": record.MaskedEmail, "charged_usd_cents": record.ChargedUSDCents,
		"renewal_cancelled": record.RenewalCancelled, "next_poll_at": record.NextPollAt, "updated_at": record.UpdatedAt,
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return kafudadomain.ErrNotFound
	}
	return nil
}

func (s *Store) DueAttempts(ctx context.Context, now time.Time, limit int) ([]kafudadomain.Attempt, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	var result []kafudadomain.Attempt
	err := s.db.WithContext(ctx).Where("active_order_id IS NOT NULL AND next_poll_at <= ?", now).
		Order("next_poll_at ASC, id ASC").Limit(limit).Find(&result).Error
	return result, err
}

// Keep checking leases separate from upstream polling so a backlog of unknown
// submissions cannot starve recovery of interrupted credential checks.
func (s *Store) DueCheckingAttempts(ctx context.Context, now time.Time, limit int) ([]kafudadomain.Attempt, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	var result []kafudadomain.Attempt
	err := s.db.WithContext(ctx).Where("status = ? AND active_order_id IS NOT NULL AND next_poll_at <= ?", kafudadomain.AttemptChecking, now).
		Order("next_poll_at ASC, id ASC").Limit(limit).Find(&result).Error
	return result, err
}

func (s *Store) GetCatalog(ctx context.Context, profile string) (*kafudadomain.Catalog, error) {
	return first[kafudadomain.Catalog](s.db.WithContext(ctx).Where("profile = ?", profile))
}

func (s *Store) SaveCatalog(ctx context.Context, catalog *kafudadomain.Catalog) error {
	if err := catalog.Validate(); err != nil {
		return err
	}
	// A slower older sync must not replace a newer valid directory.
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "profile"}},
		DoUpdates: clause.AssignmentColumns([]string{"options", "cards", "enabled", "synced_at", "expires_at"}),
		Where:     clause.Where{Exprs: []clause.Expression{clause.Expr{SQL: "excluded.synced_at > kafuda_gpt_catalogs.synced_at"}}},
	}).Create(catalog).Error
}

func first[T any](query *gorm.DB) (*T, error) {
	var record T
	if err := query.First(&record).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &record, nil
}
