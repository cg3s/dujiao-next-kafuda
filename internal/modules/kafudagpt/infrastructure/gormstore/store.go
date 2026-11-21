package gormstore

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Store struct{ db *gorm.DB }

var _ kafudacontract.Store = (*Store)(nil)
var _ kafudacontract.SettingStore = (*Store)(nil)
var _ kafudacontract.Transaction = (*Store)(nil)
var _ kafudacontract.CheckoutRepository = (*Store)(nil)

func New(db *gorm.DB) *Store { return &Store{db: db} }

func (s *Store) GetCatalogForUpdate(profile string) (*kafudadomain.Catalog, error) {
	return first[kafudadomain.Catalog](s.db.Clauses(clause.Locking{Strength: "UPDATE"}).Where("profile = ?", profile))
}

func (s *Store) CreateCheckout(input kafudacontract.CheckoutInput) error {
	return s.CreateRecharge(&kafudadomain.Recharge{OrderID: input.OrderID, Snapshot: kafudadomain.Snapshot(input.Snapshot), Stage: kafudadomain.StageWaiting, Version: 1, CreatedAt: input.CreatedAt, UpdatedAt: input.CreatedAt})
}

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
			"completion_email_pending": record.CompletionEmailPending,
			"version":                  record.Version + 1,
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

func (s *Store) PendingCompletionEmails(ctx context.Context, now time.Time, limit int) ([]uint, error) {
	var ids []uint
	err := s.db.WithContext(ctx).Model(&kafudadomain.Recharge{}).
		Where("completion_email_pending = ? AND stage = ?", true, kafudadomain.StageCompleted).
		Where("(completion_email_retry_at IS NULL OR completion_email_retry_at <= ?)", now).
		Order("CASE WHEN completion_email_retry_at IS NULL THEN 0 ELSE 1 END").
		Order("completion_email_retry_at, order_id").Limit(limit).Pluck("order_id", &ids).Error
	return ids, err
}

// ClaimCompletionEmail defers the next attempt before contacting Redis. It
// also keeps concurrent scans from delivering the same notice simultaneously.
func (s *Store) ClaimCompletionEmail(ctx context.Context, childID uint, now, retryAt time.Time) (bool, error) {
	result := s.db.WithContext(ctx).Model(&kafudadomain.Recharge{}).
		Where("order_id = ? AND completion_email_pending = ? AND stage = ?", childID, true, kafudadomain.StageCompleted).
		Where("(completion_email_retry_at IS NULL OR completion_email_retry_at <= ?)", now).
		UpdateColumn("completion_email_retry_at", retryAt)
	return result.RowsAffected == 1, result.Error
}

func (s *Store) ClearCompletionEmail(ctx context.Context, childID uint) error {
	return s.db.WithContext(ctx).Model(&kafudadomain.Recharge{}).
		Where("order_id = ? AND completion_email_pending = ? AND stage = ?", childID, true, kafudadomain.StageCompleted).
		UpdateColumns(map[string]any{"completion_email_pending": false, "completion_email_retry_at": nil}).Error
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
	updates := map[string]any{
		"status": record.Status, "active_order_id": record.ActiveOrderID, "upstream_order_id": record.UpstreamOrderID,
		"masked_email": record.MaskedEmail, "charged_usd_cents": record.ChargedUSDCents,
		"renewal_cancelled": record.RenewalCancelled, "next_poll_at": record.NextPollAt, "updated_at": record.UpdatedAt,
		"known_order_poll_lease_until": record.KnownOrderPollLeaseUntil,
	}
	if record.Status == kafudadomain.AttemptSubmitting {
		// The first checking-to-submitting transition freezes the quote.
		updates["quote_id"] = gorm.Expr("CASE WHEN status = ? AND quote_id = '' THEN ? ELSE quote_id END", kafudadomain.AttemptChecking, record.QuoteID)
		if record.QuoteCosts != nil {
			costsJSON, err := json.Marshal(record.QuoteCosts)
			if err != nil {
				return err
			}
			updates["quote_costs"] = gorm.Expr("CASE WHEN status = ? AND quote_id = '' AND quote_costs IS NULL THEN ? ELSE quote_costs END", kafudadomain.AttemptChecking, string(costsJSON))
		}
	}
	result := s.db.Model(&kafudadomain.Attempt{}).Where("id = ?", record.ID).Updates(updates)
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

// DueKnownOrderPolls selects only attempts that can be queried by numeric ID.
// Unknown submissions remain active but belong to reference reconciliation.
func (s *Store) DueKnownOrderPolls(ctx context.Context, now time.Time, limit int) ([]kafudadomain.Attempt, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	var result []kafudadomain.Attempt
	err := s.dueKnownOrderPolls(ctx, now).
		Order("next_poll_at ASC, id ASC").Limit(limit).Find(&result).Error
	return result, err
}

// ClaimKnownOrderPoll grants an expiring lease without altering the schedule or
// observation timestamps used to apply the supplier result.
func (s *Store) ClaimKnownOrderPoll(ctx context.Context, candidate kafudadomain.Attempt, now, leaseUntil time.Time) (bool, error) {
	if now.IsZero() || !leaseUntil.After(now) {
		return false, kafudadomain.ErrInvalidTransition
	}
	if candidate.NextPollAt == nil || candidate.UpstreamOrderID == nil {
		return false, nil
	}
	result := s.dueKnownOrderPolls(ctx, now).
		Where("id = ? AND order_id = ? AND upstream_order_id = ? AND status = ?", candidate.ID, candidate.OrderID, *candidate.UpstreamOrderID, candidate.Status).
		Where("next_poll_at = ? AND updated_at = ? AND updated_at <= ?", *candidate.NextPollAt, candidate.UpdatedAt, now).
		Where(`EXISTS (
			SELECT 1 FROM kafuda_gpt_recharges AS recharge
			WHERE recharge.order_id = kafuda_gpt_attempts.order_id AND recharge.current_attempt_id = kafuda_gpt_attempts.id
		)`).
		UpdateColumn("known_order_poll_lease_until", leaseUntil)
	return result.RowsAffected == 1, result.Error
}

func (s *Store) dueKnownOrderPolls(ctx context.Context, now time.Time) *gorm.DB {
	return s.db.WithContext(ctx).Model(&kafudadomain.Attempt{}).
		Where("active_order_id = order_id AND upstream_order_id > 0 AND next_poll_at <= ?", now).
		Where("status IN ?", []kafudadomain.AttemptStatus{kafudadomain.AttemptSubmitting, kafudadomain.AttemptProcessing, kafudadomain.AttemptReview}).
		Where("(known_order_poll_lease_until IS NULL OR known_order_poll_lease_until <= ?)", now)
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

func (s *Store) DueUnidentifiedSubmissions(ctx context.Context, now time.Time, limit int) ([]kafudadomain.Attempt, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	var result []kafudadomain.Attempt
	err := s.db.WithContext(ctx).
		Where("status = ? AND active_order_id IS NOT NULL AND upstream_order_id IS NULL AND next_poll_at <= ?", kafudadomain.AttemptSubmitting, now).
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
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		candidate := *catalog
		candidate.MergeAvailabilityHistory(nil)
		// Establish the profile before locking it, including concurrent first syncs.
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&candidate).Error; err != nil {
			return err
		}
		previous, err := first[kafudadomain.Catalog](tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("profile = ?", catalog.Profile))
		if err != nil {
			return err
		}
		if previous == nil || !catalog.SyncedAt.After(previous.SyncedAt) {
			return nil
		}
		candidate.MergeAvailabilityHistory(previous)
		return tx.Save(&candidate).Error
	})
}

func (s *Store) GetSetting(ctx context.Context, profile string) (*kafudadomain.Setting, error) {
	return first[kafudadomain.Setting](s.db.WithContext(ctx).Where("profile = ?", profile))
}

func (s *Store) SaveSetting(ctx context.Context, setting *kafudadomain.Setting) error {
	if setting == nil || setting.Profile == "" {
		return kafudadomain.ErrInvalidSnapshot
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		previous, err := first[kafudadomain.Setting](tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("profile = ?", setting.Profile))
		if err != nil {
			return err
		}
		record := *setting
		if record.AllowedKinds == nil {
			record.AllowedKinds = []string{}
		}
		if record.UpdatedAt.IsZero() {
			record.UpdatedAt = time.Now().UTC()
		}
		if previous == nil {
			record.Version = 1
			if err := tx.Create(&record).Error; err != nil {
				return err
			}
		} else {
			record.Version = previous.Version + 1
			if err := tx.Save(&record).Error; err != nil {
				return err
			}
		}
		*setting = record
		return nil
	})
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
