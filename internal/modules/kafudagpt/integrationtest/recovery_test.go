package integrationtest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	kafudaapp "github.com/dujiao-next/internal/modules/kafudagpt/application"
	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
)

func TestInterruptedCheckRequiresFreshSubmission(t *testing.T) {
	_, store, service := database(t)
	ctx, now := context.Background(), time.Now().UTC()
	prepare(t, service, 10, now)
	first := reserve(t, service, 10, now)
	expires := now.Add(time.Minute)
	service = kafudaapp.New(store)
	if err := service.RecoverExpiredChecks(ctx, expires.Add(-time.Nanosecond), 100); err != nil {
		t.Fatal(err)
	}
	record, err := store.GetRecharge(ctx, 10)
	if err != nil || record.Stage != kafudadomain.StageChecking {
		t.Fatalf("live check was recovered: %+v, %v", record, err)
	}
	for _, late := range []time.Time{expires, expires.Add(time.Nanosecond)} {
		if err := service.MarkSubmitting(ctx, first.Attempt.ID, "late_quote", late, late.Add(time.Minute)); !errors.Is(err, kafudadomain.ErrInvalidTransition) {
			t.Fatalf("unrecovered expired check could submit at %s: %v", late, err)
		}
		for _, retryable := range []bool{true, false} {
			if err := service.CheckRejected(ctx, first.Attempt.ID, retryable, late); !errors.Is(err, kafudadomain.ErrInvalidTransition) {
				t.Fatalf("expired check rejection changed state at %s (retryable=%t): %v", late, retryable, err)
			}
		}
	}
	if err := service.MarkSubmitting(ctx, first.Attempt.ID, " ", now, expires); !errors.Is(err, kafudadomain.ErrInvalidTransition) {
		t.Fatalf("blank quote could submit: %v", err)
	}
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			if err := service.RecoverExpiredChecks(ctx, expires, 100); err != nil {
				t.Error(err)
			}
		})
	}
	workers.Wait()
	record, err = store.GetRecharge(ctx, 10)
	if err != nil || record.Stage != kafudadomain.StageWaiting || record.ReasonCode != "check_interrupted" {
		t.Fatalf("interrupted check not recovered: %+v, %v", record, err)
	}
	attempt, err := store.GetAttempt(ctx, first.Attempt.ID)
	if err != nil || attempt.Status != kafudadomain.AttemptFailed || attempt.ActiveOrderID != nil || attempt.NextPollAt != nil || attempt.Reference != first.Attempt.Reference {
		t.Fatalf("interrupted attempt not preserved: %+v, %v", attempt, err)
	}
	if err := service.MarkSubmitting(ctx, first.Attempt.ID, "stale_quote", expires, expires.Add(time.Minute)); !errors.Is(err, kafudadomain.ErrInvalidTransition) {
		t.Fatalf("expired handler could submit: %v", err)
	}
	second := reserve(t, service, 10, expires)
	if second.Attempt.ID == first.Attempt.ID || second.Attempt.Reference == first.Attempt.Reference || second.Attempt.Sequence != 2 {
		t.Fatalf("fresh submission reused interrupted attempt: %+v", second.Attempt)
	}
}

func TestRecoveryDoesNotReleaseSubmittedOrUnknownAttempts(t *testing.T) {
	_, store, service := database(t)
	ctx, now := context.Background(), time.Now().UTC()
	for i, status := range []string{"submitting", "processing", "needs_review"} {
		orderID := uint(i + 1)
		prepare(t, service, orderID, now)
		reservation := reserve(t, service, orderID, now)
		if err := service.MarkSubmitting(ctx, reservation.Attempt.ID, "quo_1", now, now.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		if status != "submitting" {
			result := kafudacontract.OrderResult{ID: uint64(i + 100), Reference: reservation.Attempt.Reference, Product: "gpt", Plan: "plus", Country: "PH", Status: status}
			if err := service.RecordResult(ctx, reservation.Attempt.ID, result, now, now.Add(time.Second)); err != nil {
				t.Fatal(err)
			}
		}
	}
	prepare(t, service, 10, now)
	reserve(t, service, 10, now)
	// The earlier submitted attempts must not consume the checking scan's limit.
	if err := service.RecoverExpiredChecks(ctx, now.Add(time.Minute), 1); err != nil {
		t.Fatal(err)
	}
	record, err := store.GetRecharge(ctx, 10)
	if err != nil || record.Stage != kafudadomain.StageWaiting {
		t.Fatalf("upstream backlog starved checking recovery: %+v, %v", record, err)
	}
	for orderID := uint(1); orderID <= 3; orderID++ {
		again, err := service.Reserve(ctx, orderID, now.Add(time.Minute), now.Add(2*time.Minute))
		if err != nil || again.Created || again.Attempt.ActiveOrderID == nil {
			t.Fatalf("recovery released submitted order %d: %+v, %v", orderID, again, err)
		}
	}
}

// Advance after the database scan to exercise a stale recovery candidate using
// real transactions and the single-connection SQLite pool.
type advancingRecoveryStore struct {
	kafudacontract.Store
	advance func()
}

func (s advancingRecoveryStore) DueCheckingAttempts(ctx context.Context, now time.Time, limit int) ([]kafudadomain.Attempt, error) {
	attempts, err := s.Store.DueCheckingAttempts(ctx, now, limit)
	if err == nil {
		s.advance()
	}
	return attempts, err
}

func TestRecoveryRechecksCandidateUnderLock(t *testing.T) {
	for _, advance := range []string{"submit", "new_attempt"} {
		t.Run(advance, func(t *testing.T) {
			_, store, service := database(t)
			ctx, now := context.Background(), time.Now().UTC()
			prepare(t, service, 10, now)
			first := reserve(t, service, 10, now)
			expires := now.Add(time.Minute)
			racingStore := advancingRecoveryStore{Store: store, advance: func() {
				if advance == "submit" {
					if err := service.MarkSubmitting(ctx, first.Attempt.ID, "quo_1", expires.Add(-time.Nanosecond), expires.Add(time.Minute)); err != nil {
						t.Fatal(err)
					}
					return
				}
				if err := service.CheckRejected(ctx, first.Attempt.ID, true, expires.Add(-time.Nanosecond)); err != nil {
					t.Fatal(err)
				}
				reserve(t, service, 10, expires)
			}}
			if err := kafudaapp.New(racingStore).RecoverExpiredChecks(ctx, expires, 100); err != nil {
				t.Fatal(err)
			}
			again, err := service.Reserve(ctx, 10, expires, expires.Add(time.Minute))
			if err != nil || again.Created || again.Attempt.ActiveOrderID == nil {
				t.Fatalf("stale recovery released active attempt: %+v, %v", again, err)
			}
			if advance == "submit" && again.Attempt.Status != kafudadomain.AttemptSubmitting {
				t.Fatalf("submission regressed: %+v", again.Attempt)
			}
			if advance == "new_attempt" && again.Attempt.Sequence != 2 {
				t.Fatalf("fresh attempt regressed: %+v", again.Attempt)
			}
		})
	}
}

type failingRecoveryWriteStore struct {
	kafudacontract.Store
	err error
}

func (s failingRecoveryWriteStore) WithinTransaction(ctx context.Context, run func(kafudacontract.Transaction) error) error {
	return s.Store.WithinTransaction(ctx, func(tx kafudacontract.Transaction) error {
		return run(failingRecoveryWriteTx{Transaction: tx, err: s.err})
	})
}

type failingRecoveryWriteTx struct {
	kafudacontract.Transaction
	err error
}

func (tx failingRecoveryWriteTx) SaveRecharge(record *kafudadomain.Recharge) error {
	if record.OrderID == 10 {
		return tx.err
	}
	return tx.Transaction.SaveRecharge(record)
}

func TestRecoveryContinuesAfterOneRechargeWriteFails(t *testing.T) {
	_, store, service := database(t)
	ctx, now := context.Background(), time.Now().UTC()
	prepare(t, service, 10, now)
	first := reserve(t, service, 10, now)
	prepare(t, service, 11, now)
	reserve(t, service, 11, now.Add(time.Second))
	deadline := now.Add(2 * time.Minute)
	writeErr := errors.New("recharge write failed")
	failing := kafudaapp.New(failingRecoveryWriteStore{Store: store, err: writeErr})
	if err := failing.RecoverExpiredChecks(ctx, deadline, 100); !errors.Is(err, writeErr) {
		t.Fatalf("recovery did not return the write failure: %v", err)
	}
	attempt, err := store.GetAttempt(ctx, first.Attempt.ID)
	if err != nil || attempt == nil || attempt.Status != kafudadomain.AttemptChecking || attempt.ActiveOrderID == nil {
		t.Fatalf("failed transaction did not roll back the attempt: %+v, %v", attempt, err)
	}
	for orderID, want := range map[uint]kafudadomain.Stage{10: kafudadomain.StageChecking, 11: kafudadomain.StageWaiting} {
		record, err := store.GetRecharge(ctx, orderID)
		if err != nil || record == nil || record.Stage != want {
			t.Fatalf("recharge %d: got %+v, %v, want %s", orderID, record, err, want)
		}
	}
	if err := service.RecoverExpiredChecks(ctx, deadline, 100); err != nil {
		t.Fatal(err)
	}
	record, err := store.GetRecharge(ctx, 10)
	if err != nil || record == nil || record.Stage != kafudadomain.StageWaiting {
		t.Fatalf("retry did not recover failed recharge: %+v, %v", record, err)
	}
}
