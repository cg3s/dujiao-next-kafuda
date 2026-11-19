package gormstore

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestDueCardChargePolls(t *testing.T) {
	store := newCardChargeStore(t)
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	recent := now.Add(-10 * time.Minute)
	readyRetry := now.Add(-time.Minute)
	pendingRetry := now.Add(time.Second)
	zero := int64(0)
	newlyCompleted := addCardChargeAttempt(t, store, 1, recent, nil, nil)
	retried := addCardChargeAttempt(t, store, 2, recent.Add(-time.Minute), &readyRetry, nil)
	addCardChargeAttempt(t, store, 3, recent, &pendingRetry, nil)
	addCardChargeAttempt(t, store, 4, recent, nil, &zero)
	addCardChargeAttempt(t, store, 5, now.Add(-24*time.Hour), nil, nil)
	addCardChargeAttempt(t, store, 6, now.Add(-time.Second), nil, nil)
	onFirstLookupBoundary := addCardChargeAttempt(t, store, 7, now.Add(-time.Minute), nil, nil)
	noncurrent := addCardChargeAttempt(t, store, 8, recent, nil, nil)
	noncompleted := addCardChargeAttempt(t, store, 9, recent, nil, nil)
	noSupplierOrder := addCardChargeAttempt(t, store, 10, recent, nil, nil)
	failed := addCardChargeAttempt(t, store, 11, recent, nil, nil)
	for _, change := range []struct {
		model any
		where string
		id    uint
		col   string
		value any
	}{
		{&kafudadomain.Recharge{}, "order_id = ?", noncurrent.OrderID, "current_attempt_id", nil},
		{&kafudadomain.Recharge{}, "order_id = ?", noncompleted.OrderID, "stage", kafudadomain.StageManualReview},
		{&kafudadomain.Attempt{}, "id = ?", noSupplierOrder.ID, "upstream_order_id", nil},
		{&kafudadomain.Attempt{}, "id = ?", failed.ID, "status", kafudadomain.AttemptFailed},
	} {
		if err := store.db.Model(change.model).Where(change.where, change.id).UpdateColumn(change.col, change.value).Error; err != nil {
			t.Fatal(err)
		}
	}

	for _, limit := range []int{2, 10} {
		want := []uint{newlyCompleted.ID, retried.ID, onFirstLookupBoundary.ID}
		polls, err := store.DueCardChargePolls(t.Context(), now, now.Add(-24*time.Hour), now.Add(-time.Minute), limit)
		if err != nil {
			t.Fatal(err)
		}
		var got []uint
		for _, attempt := range polls {
			got = append(got, attempt.ID)
		}
		if limit < len(want) {
			want = want[:limit]
		}
		if !slices.Equal(got, want) {
			t.Fatalf("limit %d: got %v, want %v", limit, got, want)
		}
	}
}

func TestCardChargePollLease(t *testing.T) {
	store := newCardChargeStore(t)
	ctx := t.Context()
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	completedAt := now.Add(-2 * time.Minute)
	attempt := addCardChargeAttempt(t, store, 1, completedAt, nil, nil)
	before, err := store.GetRecharge(ctx, attempt.OrderID)
	if err != nil {
		t.Fatal(err)
	}

	firstLease := now.Add(time.Minute)
	wrongID := uint64(100)
	stale := attempt
	stale.UpstreamOrderID = &wrongID
	if claimed, err := store.ClaimCardChargePoll(ctx, stale, firstLease); err != nil || claimed {
		t.Fatalf("stale identity claimed a poll: %v, %v", claimed, err)
	}
	type claim struct {
		obtained bool
		err      error
	}
	const workers = 4
	claims := make(chan claim, workers)
	for range workers {
		go func() {
			obtained, err := store.ClaimCardChargePoll(ctx, attempt, firstLease)
			claims <- claim{obtained, err}
		}()
	}
	var obtained int
	for range workers {
		result := <-claims
		if result.err != nil {
			t.Error(result.err)
		}
		if result.obtained {
			obtained++
		}
	}
	if obtained != 1 {
		t.Fatalf("claims obtained = %d, want 1", obtained)
	}

	retryAt := now.Add(30 * time.Minute)
	if saved, err := store.SaveCardChargePoll(ctx, attempt.ID, firstLease, nil, &retryAt); err != nil || !saved {
		t.Fatalf("reschedule: %v, %v", saved, err)
	}
	due, err := store.DueCardChargePolls(ctx, retryAt, retryAt.Add(-24*time.Hour), retryAt.Add(-time.Minute), 2)
	if err != nil || len(due) != 1 || due[0].ID != attempt.ID {
		t.Fatalf("expected retry after expiry: %+v, %v", due, err)
	}
	secondLease := retryAt.Add(time.Minute)
	if claimed, err := store.ClaimCardChargePoll(ctx, due[0], secondLease); err != nil || !claimed {
		t.Fatalf("claim scheduled retry: %v, %v", claimed, err)
	}
	zero := int64(0)
	if saved, err := store.SaveCardChargePoll(ctx, attempt.ID, firstLease, &zero, nil); err != nil || saved {
		t.Fatalf("stale worker wrote a charge: %v, %v", saved, err)
	}
	negative := int64(-1)
	if saved, err := store.SaveCardChargePoll(ctx, attempt.ID, secondLease, &negative, nil); !errors.Is(err, kafudadomain.ErrInvalidTransition) || saved {
		t.Fatalf("negative charge accepted: %v, %v", saved, err)
	}
	if saved, err := store.SaveCardChargePoll(ctx, attempt.ID, secondLease, &zero, nil); err != nil || !saved {
		t.Fatalf("store known zero: %v, %v", saved, err)
	}
	stored, err := store.GetAttempt(ctx, attempt.ID)
	if err != nil || stored == nil || stored.ChargedUSDCents == nil || *stored.ChargedUSDCents != zero ||
		stored.NextCardChargePollAt != nil || !stored.UpdatedAt.Equal(attempt.UpdatedAt) || stored.Status != attempt.Status || stored.NextPollAt != nil {
		t.Fatalf("unexpected reconciled attempt: %+v, %v", stored, err)
	}
	after, err := store.GetRecharge(ctx, attempt.OrderID)
	if err != nil || after == nil || after.Stage != before.Stage || after.Version != before.Version ||
		after.CompletedAt == nil || !after.CompletedAt.Equal(*before.CompletedAt) || !after.UpdatedAt.Equal(before.UpdatedAt) ||
		after.CompletionEmailPending != before.CompletionEmailPending {
		t.Fatalf("charge update changed recharge: %+v, %v", after, err)
	}
}

func TestKnownOrderPollLease(t *testing.T) {
	store := newCardChargeStore(t)
	ctx := t.Context()
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	dueAt, observedAt := now.Add(-time.Second), now.Add(-time.Minute)
	orderID, upstreamID := uint(10), uint64(20)
	attempt := kafudadomain.Attempt{
		OrderID: orderID, Sequence: 1, ActiveOrderID: &orderID, Reference: "known-order",
		Status: kafudadomain.AttemptProcessing, UpstreamOrderID: &upstreamID, NextPollAt: &dueAt,
		CreatedAt: observedAt, UpdatedAt: observedAt,
	}
	if err := store.CreateAttempt(&attempt); err != nil {
		t.Fatal(err)
	}
	until := now.Add(time.Minute)
	if claimed, err := store.ClaimKnownOrderPoll(ctx, attempt, now, until); claimed || err != nil {
		t.Fatalf("claim without a current recharge = %t, %v", claimed, err)
	}
	recharge := kafudadomain.Recharge{
		OrderID: orderID, CurrentAttemptID: &attempt.ID, Stage: kafudadomain.StageProcessing,
		Version: 1, CreatedAt: observedAt, UpdatedAt: observedAt,
	}
	if err := store.db.Create(&recharge).Error; err != nil {
		t.Fatal(err)
	}
	stale := attempt
	stale.UpdatedAt = stale.UpdatedAt.Add(-time.Second)
	if claimed, err := store.ClaimKnownOrderPoll(ctx, stale, now, until); claimed || err != nil {
		t.Fatalf("claim with stale observation = %t, %v", claimed, err)
	}

	type claim struct {
		obtained bool
		err      error
	}
	const workers = 2
	claims := make(chan claim, workers)
	for range workers {
		go func() {
			obtained, err := store.ClaimKnownOrderPoll(ctx, attempt, now, until)
			claims <- claim{obtained, err}
		}()
	}
	obtained := 0
	for range workers {
		result := <-claims
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.obtained {
			obtained++
		}
	}
	if obtained != 1 {
		t.Fatalf("claims obtained = %d, want 1", obtained)
	}
	stored, err := store.GetAttempt(ctx, attempt.ID)
	if err != nil || stored == nil || stored.KnownOrderPollLeaseUntil == nil || !stored.KnownOrderPollLeaseUntil.Equal(until) ||
		stored.NextPollAt == nil || !stored.NextPollAt.Equal(dueAt) || !stored.UpdatedAt.Equal(observedAt) {
		t.Fatalf("claim changed stored poll schedule or did not set the lease: %+v, %v", stored, err)
	}
	if due, err := store.DueKnownOrderPolls(ctx, until.Add(-time.Microsecond), 2); err != nil || len(due) != 0 {
		t.Fatalf("attempt eligible before lease expiry: %+v, %v", due, err)
	}
	due, err := store.DueKnownOrderPolls(ctx, until, 2)
	if err != nil || len(due) != 1 || due[0].ID != attempt.ID {
		t.Fatalf("attempt unavailable at lease expiry: %+v, %v", due, err)
	}
	if claimed, err := store.ClaimKnownOrderPoll(ctx, due[0], until, until.Add(time.Minute)); err != nil || !claimed {
		t.Fatalf("claim at lease expiry = %t, %v", claimed, err)
	}
}

func newCardChargeStore(t *testing.T) *Store {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.AutoMigrate(&kafudadomain.Recharge{}, &kafudadomain.Attempt{}); err != nil {
		t.Fatal(err)
	}
	return New(db)
}

func addCardChargeAttempt(t *testing.T, store *Store, orderID uint, completedAt time.Time, next *time.Time, charged *int64) kafudadomain.Attempt {
	t.Helper()
	upstreamID := uint64(orderID)
	attempt := kafudadomain.Attempt{
		OrderID: orderID, Sequence: 1, Reference: fmt.Sprintf("charge-%d", orderID),
		Status: kafudadomain.AttemptCompleted, UpstreamOrderID: &upstreamID, ChargedUSDCents: charged,
		NextCardChargePollAt: next, CreatedAt: completedAt, UpdatedAt: completedAt,
	}
	if err := store.CreateAttempt(&attempt); err != nil {
		t.Fatal(err)
	}
	recharge := kafudadomain.Recharge{
		OrderID: orderID, CurrentAttemptID: &attempt.ID, Stage: kafudadomain.StageCompleted,
		Version: 3, CompletionEmailPending: true, CompletedAt: &completedAt, CreatedAt: completedAt, UpdatedAt: completedAt,
	}
	if err := store.db.Create(&recharge).Error; err != nil {
		t.Fatal(err)
	}
	return attempt
}
