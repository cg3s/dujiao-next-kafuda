package gormstore

import (
	"context"
	"time"

	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
	"gorm.io/gorm"
)

// DueCardChargePolls includes recent completed recharges that predate the poll schedule.
func (s *Store) DueCardChargePolls(ctx context.Context, now, completedAfter, completedBefore time.Time, limit int) ([]kafudadomain.Attempt, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	var attempts []kafudadomain.Attempt
	err := s.attemptsMissingCardCharge(ctx).Select("kafuda_gpt_attempts.*").
		Joins("JOIN kafuda_gpt_recharges AS recharge ON recharge.order_id = kafuda_gpt_attempts.order_id AND recharge.current_attempt_id = kafuda_gpt_attempts.id").
		Where("recharge.stage = ? AND recharge.completed_at > ? AND recharge.completed_at <= ?", kafudadomain.StageCompleted, completedAfter, completedBefore).
		Where("(kafuda_gpt_attempts.next_card_charge_poll_at IS NULL OR kafuda_gpt_attempts.next_card_charge_poll_at <= ?)", now).
		Order("COALESCE(kafuda_gpt_attempts.next_card_charge_poll_at, recharge.completed_at), kafuda_gpt_attempts.id").
		Limit(limit).Find(&attempts).Error
	return attempts, err
}

// ClaimCardChargePoll compares the poll schedule seen during selection before
// assigning a lease. Each worker finishes the lookup outside the database.
func (s *Store) ClaimCardChargePoll(ctx context.Context, candidate kafudadomain.Attempt, leaseUntil time.Time) (bool, error) {
	if leaseUntil.IsZero() || (candidate.NextCardChargePollAt != nil && !leaseUntil.After(*candidate.NextCardChargePollAt)) {
		return false, kafudadomain.ErrInvalidTransition
	}
	query := s.currentCompletedAttemptMissingCardCharge(ctx, candidate.ID).
		Where("order_id = ? AND upstream_order_id = ? AND reference = ?", candidate.OrderID, candidate.UpstreamOrderID, candidate.Reference)
	if candidate.NextCardChargePollAt == nil {
		query = query.Where("next_card_charge_poll_at IS NULL")
	} else {
		query = query.Where("next_card_charge_poll_at = ?", *candidate.NextCardChargePollAt)
	}
	result := query.UpdateColumn("next_card_charge_poll_at", leaseUntil)
	return result.RowsAffected == 1, result.Error
}

// SaveCardChargePoll only updates the missing amount and poll schedule. The
// lease prevents a late worker from overwriting a newer worker's result.
func (s *Store) SaveCardChargePoll(ctx context.Context, attemptID uint, leaseUntil time.Time, charged *int64, next *time.Time) (bool, error) {
	if leaseUntil.IsZero() || (charged != nil && *charged < 0) {
		return false, kafudadomain.ErrInvalidTransition
	}
	if charged != nil {
		next = nil
	}
	result := s.currentCompletedAttemptMissingCardCharge(ctx, attemptID).
		Where("next_card_charge_poll_at = ?", leaseUntil).
		UpdateColumns(map[string]any{"charged_usd_cents": charged, "next_card_charge_poll_at": next})
	return result.RowsAffected == 1, result.Error
}

func (s *Store) attemptsMissingCardCharge(ctx context.Context) *gorm.DB {
	return s.db.WithContext(ctx).Model(&kafudadomain.Attempt{}).
		Where("kafuda_gpt_attempts.status = ? AND kafuda_gpt_attempts.upstream_order_id > 0 AND kafuda_gpt_attempts.charged_usd_cents IS NULL", kafudadomain.AttemptCompleted)
}

func (s *Store) currentCompletedAttemptMissingCardCharge(ctx context.Context, attemptID uint) *gorm.DB {
	return s.attemptsMissingCardCharge(ctx).Where("kafuda_gpt_attempts.id = ?", attemptID).
		Where(`EXISTS (
			SELECT 1 FROM kafuda_gpt_recharges AS recharge
			WHERE recharge.order_id = kafuda_gpt_attempts.order_id AND recharge.current_attempt_id = kafuda_gpt_attempts.id
				AND recharge.stage = ? AND recharge.completed_at IS NOT NULL
		)`, kafudadomain.StageCompleted)
}
