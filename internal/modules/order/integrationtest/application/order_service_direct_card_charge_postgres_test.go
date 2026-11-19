//go:build integration

package application_test

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
	kafudastore "github.com/dujiao-next/internal/modules/kafudagpt/infrastructure/gormstore"
	"gorm.io/gorm"
)

func TestPostgresDirectCardChargeClaim(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is empty")
	}
	db := openPostgresDirectRaceDB(t, dsn)
	if err := db.AutoMigrate(&kafudadomain.Recharge{}, &kafudadomain.Attempt{}); err != nil {
		t.Fatal(err)
	}
	if !db.Migrator().HasIndex(&kafudadomain.Recharge{}, "idx_kafuda_gpt_recharges_completed_at") {
		t.Fatal("completed recharge scan index missing")
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	completedAt := now.Add(-2 * time.Minute)
	upstreamID := uint64(20)
	attempt := kafudadomain.Attempt{
		OrderID: 10, Sequence: 1, Status: kafudadomain.AttemptCompleted, Reference: "charge-poll",
		UpstreamOrderID: &upstreamID, CreatedAt: completedAt, UpdatedAt: completedAt,
	}
	if err := db.Create(&attempt).Error; err != nil {
		t.Fatal(err)
	}
	recharge := kafudadomain.Recharge{
		OrderID: attempt.OrderID, CurrentAttemptID: &attempt.ID, Stage: kafudadomain.StageCompleted,
		CompletedAt: &completedAt, UpdatedAt: completedAt, Version: 3, CompletionEmailPending: true,
	}
	if err := db.Create(&recharge).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Migrator().DropColumn(&kafudadomain.Attempt{}, "NextCardChargePollAt"); err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&kafudadomain.Attempt{}); err != nil {
		t.Fatalf("add poll schedule to existing attempt: %v", err)
	}

	store := kafudastore.New(db)
	ctx := t.Context()
	due, err := store.DueCardChargePolls(ctx, now, now.Add(-24*time.Hour), now.Add(-time.Minute), 2)
	if err != nil || len(due) != 1 || due[0].ID != attempt.ID || due[0].NextCardChargePollAt != nil {
		t.Fatalf("existing completed attempt should be due: %+v, %v", due, err)
	}
	leaseUntil := now.Add(time.Minute)
	type outcome struct {
		claimed bool
		err     error
	}
	const workers = 4
	results := make(chan outcome, workers)
	start := make(chan struct{})
	for range workers {
		go func() {
			<-start
			claimed, err := store.ClaimCardChargePoll(ctx, due[0], leaseUntil)
			results <- outcome{claimed, err}
		}()
	}
	close(start)
	claims := 0
	for range workers {
		out := <-results
		if out.err != nil {
			t.Error(out.err)
		}
		if out.claimed {
			claims++
		}
	}
	if claims != 1 {
		t.Fatalf("concurrent leases = %d, want 1", claims)
	}
	stored, err := store.GetAttempt(ctx, attempt.ID)
	if err != nil || stored == nil || stored.NextCardChargePollAt == nil || !stored.NextCardChargePollAt.Equal(leaseUntil) {
		t.Fatalf("lease must round-trip exactly: %+v, %v", stored, err)
	}
	beforeLeaseEnd := leaseUntil.Add(-time.Microsecond)
	due, err = store.DueCardChargePolls(ctx, beforeLeaseEnd, now.Add(-24*time.Hour), now.Add(-time.Minute), 2)
	if err != nil || len(due) != 0 {
		t.Fatalf("claimed attempt due before its lease ends: %+v, %v", due, err)
	}
	due, err = store.DueCardChargePolls(ctx, leaseUntil, now.Add(-24*time.Hour), now.Add(-time.Minute), 2)
	if err != nil || len(due) != 1 || due[0].ID != attempt.ID {
		t.Fatalf("attempt should be due again after lease expiration: %+v, %v", due, err)
	}
	secondLease := leaseUntil.Add(time.Minute)
	claimed, err := store.ClaimCardChargePoll(ctx, due[0], secondLease)
	if err != nil || !claimed {
		t.Fatalf("claim expired lease: %v, %v", claimed, err)
	}
	staleCharge := int64(5)
	if applied, err := store.SaveCardChargePoll(ctx, attempt.ID, leaseUntil, &staleCharge, nil); err != nil || applied {
		t.Fatalf("expired owner wrote charge: %v, %v", applied, err)
	}
	zero := int64(0)
	if applied, err := store.SaveCardChargePoll(ctx, attempt.ID, secondLease, &zero, nil); err != nil || !applied {
		t.Fatalf("store confirmed zero charge: %v, %v", applied, err)
	}
	stored, err = store.GetAttempt(ctx, attempt.ID)
	if err != nil || stored == nil || stored.ChargedUSDCents == nil || *stored.ChargedUSDCents != 0 ||
		stored.NextCardChargePollAt != nil || stored.Status != kafudadomain.AttemptCompleted || !stored.UpdatedAt.Equal(completedAt) {
		t.Fatalf("charge lookup altered attempt history: %+v, %v", stored, err)
	}
	record, err := store.GetRecharge(ctx, recharge.OrderID)
	if err != nil || record == nil || record.Version != recharge.Version || record.Stage != recharge.Stage ||
		!record.CompletionEmailPending || record.CompletedAt == nil || !record.CompletedAt.Equal(completedAt) || !record.UpdatedAt.Equal(completedAt) {
		t.Fatalf("charge lookup altered completion or mail: %+v, %v", record, err)
	}
}

func TestPostgresDirectKnownOrderPollLease(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is empty")
	}
	db := openPostgresDirectRaceDB(t, dsn)
	if err := db.AutoMigrate(&kafudadomain.Recharge{}, &kafudadomain.Attempt{}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	dueAt, observedAt := now.Add(-time.Minute), now.Add(-2*time.Minute)
	orderID, upstreamID := uint(10), uint64(20)
	attempt := kafudadomain.Attempt{
		OrderID: orderID, Sequence: 1, Status: kafudadomain.AttemptProcessing, Reference: "known-order-poll",
		ActiveOrderID: &orderID, UpstreamOrderID: &upstreamID, NextPollAt: &dueAt,
		CreatedAt: observedAt, UpdatedAt: observedAt,
	}
	if err := db.Create(&attempt).Error; err != nil {
		t.Fatal(err)
	}
	recharge := kafudadomain.Recharge{
		OrderID: orderID, CurrentAttemptID: &attempt.ID, Stage: kafudadomain.StageProcessing, Version: 1,
	}
	if err := db.Create(&recharge).Error; err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	store := kafudastore.New(db)
	var workers sync.WaitGroup
	defer func() {
		cancel()
		workers.Wait()
	}()
	due, err := store.DueKnownOrderPolls(ctx, now, 1)
	if err != nil || len(due) != 1 || due[0].ID != attempt.ID {
		t.Fatalf("expected one due known attempt: %+v, %v", due, err)
	}
	candidate := due[0]
	leaseUntil := now.Add(time.Minute)

	const workerCount = 4
	type result struct {
		claimed bool
		err     error
	}
	ready, start := make(chan struct{}, workerCount), make(chan struct{})
	results := make(chan result, workerCount)
	for range workerCount {
		workers.Go(func() {
			var outcome result
			outcome.err = db.WithContext(ctx).Connection(func(conn *gorm.DB) error {
				ready <- struct{}{}
				select {
				case <-start:
				case <-ctx.Done():
					return ctx.Err()
				}
				var err error
				outcome.claimed, err = kafudastore.New(conn).ClaimKnownOrderPoll(ctx, candidate, now, leaseUntil)
				return err
			})
			results <- outcome
		})
	}
	for range workerCount {
		select {
		case <-ready:
		case outcome := <-results:
			t.Fatalf("worker finished before all connections were ready: %+v", outcome)
		case <-ctx.Done():
			t.Fatal("workers did not obtain independent PostgreSQL connections")
		}
	}
	close(start)
	claims := 0
	for range workerCount {
		outcome := <-results
		if outcome.err != nil {
			t.Error(outcome.err)
		}
		if outcome.claimed {
			claims++
		}
	}
	if claims != 1 {
		t.Fatalf("successful PostgreSQL claims = %d, want 1", claims)
	}

	assertLease := func(until time.Time) {
		t.Helper()
		stored, err := store.GetAttempt(ctx, attempt.ID)
		if err != nil || stored == nil || stored.KnownOrderPollLeaseUntil == nil || !stored.KnownOrderPollLeaseUntil.Equal(until) ||
			stored.NextPollAt == nil || !stored.NextPollAt.Equal(dueAt) || !stored.UpdatedAt.Equal(observedAt) {
			t.Fatalf("claim did not preserve the original poll schedule and observation: %+v, %v", stored, err)
		}
	}
	assertLease(leaseUntil)
	beforeExpiry := leaseUntil.Add(-time.Microsecond)
	if due, err := store.DueKnownOrderPolls(ctx, beforeExpiry, 1); err != nil || len(due) != 0 {
		t.Fatalf("attempt due before its lease expires: %+v, %v", due, err)
	}
	takeoverUntil := leaseUntil.Add(time.Minute)
	if claimed, err := store.ClaimKnownOrderPoll(ctx, candidate, beforeExpiry, takeoverUntil); err != nil || claimed {
		t.Fatalf("claim before lease expires = %t, %v", claimed, err)
	}
	due, err = store.DueKnownOrderPolls(ctx, leaseUntil, 1)
	if err != nil || len(due) != 1 || due[0].ID != attempt.ID {
		t.Fatalf("expected the attempt to be due at exact expiry: %+v, %v", due, err)
	}
	if claimed, err := store.ClaimKnownOrderPoll(ctx, due[0], leaseUntil, takeoverUntil); err != nil || !claimed {
		t.Fatalf("claim at exact expiry = %t, %v", claimed, err)
	}
	assertLease(takeoverUntil)
}
