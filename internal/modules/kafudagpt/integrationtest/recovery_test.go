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
	// A due unknown submission sorts before the known IDs but must not
	// consume the numeric polling budget, and checking stays separate.
	polls, err := store.DueKnownOrderPolls(ctx, now.Add(time.Second), 1)
	if err != nil || len(polls) != 1 || polls[0].OrderID != 2 || polls[0].UpstreamOrderID == nil || *polls[0].UpstreamOrderID != 101 {
		t.Fatalf("numeric polling did not select the first due known order: %+v, %v", polls, err)
	}
	polls, err = store.DueKnownOrderPolls(ctx, now.Add(time.Second), 100)
	if err != nil || len(polls) != 2 || polls[1].OrderID != 3 {
		t.Fatalf("numeric polling did not retain known review attempts: %+v, %v", polls, err)
	}
	review := polls[1]
	due, retry := now.Add(time.Second), now.Add(2*time.Minute)
	if err := service.RescheduleKnownOrderResult(ctx, review.ID, due, due, due, retry, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := service.RescheduleKnownOrderResult(ctx, review.ID, due, due, due, retry.Add(time.Minute), time.Second); !errors.Is(err, kafudadomain.ErrConflict) {
		t.Fatalf("duplicate lookup failure overwrote the new schedule: %v", err)
	}
	reviewRecord, err := store.GetRecharge(ctx, review.OrderID)
	if err != nil || reviewRecord == nil || reviewRecord.Stage != kafudadomain.StageManualReview || reviewRecord.ReasonCode != "upstream_review" {
		t.Fatalf("result application failure changed the existing review reason: %+v, %v", reviewRecord, err)
	}
	updated, err := store.GetAttempt(ctx, review.ID)
	if err != nil || updated == nil || updated.Status != kafudadomain.AttemptReview || updated.ActiveOrderID == nil ||
		updated.NextPollAt == nil || !updated.NextPollAt.Equal(retry) {
		t.Fatalf("failed lookup changed review state or lost the schedule: %+v, %v", updated, err)
	}
	polls, err = store.DueKnownOrderPolls(ctx, now, 100)
	if err != nil || len(polls) != 0 {
		t.Fatalf("numeric polling selected a future attempt: %+v, %v", polls, err)
	}
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

type knownOrderLookupGateway struct {
	kafudacontract.Gateway
	result       *kafudacontract.OrderResult
	err          error
	id           uint64
	cancelLookup context.CancelFunc
	lookupErr    error
}

func (g *knownOrderLookupGateway) GetOrder(ctx context.Context, id uint64) (*kafudacontract.OrderResult, error) {
	g.id = id
	if g.cancelLookup != nil {
		g.cancelLookup()
		g.lookupErr = ctx.Err()
		return nil, g.lookupErr
	}
	return g.result, g.err
}

func TestPollKnownOrderValidatesResponseAndReschedulesFailure(t *testing.T) {
	for _, scenario := range []string{"completed", "timeout", "wrong_reference", "deadline", "lookup_canceled", "rate_limited", "rate_limit_below_backoff"} {
		t.Run(scenario, func(t *testing.T) {
			_, store, service := database(t)
			ctx, now := context.Background(), time.Now().UTC()
			lookupCtx, cancelLookup := context.WithCancel(ctx)
			defer cancelLookup()
			prepare(t, service, 10, now)
			attempt := reserve(t, service, 10, now).Attempt
			due, retry := now.Add(time.Minute), now.Add(2*time.Minute)
			if err := service.MarkSubmitting(ctx, attempt.ID, "quo_1", now, due); err != nil {
				t.Fatal(err)
			}
			result := kafudacontract.OrderResult{ID: 101, Reference: attempt.Reference, Product: "gpt", Plan: "plus", Country: "PH", Status: "processing"}
			if err := service.RecordResult(ctx, attempt.ID, result, now, due); err != nil {
				t.Fatal(err)
			}
			result.Status = "completed"
			gateway := &knownOrderLookupGateway{result: &result}
			switch scenario {
			case "timeout", "deadline":
				gateway.err = context.DeadlineExceeded
			case "lookup_canceled":
				gateway.cancelLookup = cancelLookup
			case "wrong_reference":
				result.Reference = "wrong"
			case "rate_limited":
				gateway.err = &kafudacontract.APIError{HTTPStatus: 429, RetryAfter: 3 * time.Minute}
			case "rate_limit_below_backoff":
				gateway.err = &kafudacontract.APIError{HTTPStatus: 429, RetryAfter: time.Second}
			}
			reviewAfter := time.Hour
			wantReview := scenario == "deadline" || scenario == "lookup_canceled"
			if wantReview {
				reviewAfter = time.Minute
			}
			got, pollErr := service.PollKnownOrder(ctx, lookupCtx, gateway, attempt.ID, due, retry, reviewAfter)
			if gateway.id != 101 {
				t.Fatalf("queried wrong upstream ID: %d", gateway.id)
			}
			if scenario == "lookup_canceled" && !errors.Is(gateway.lookupErr, context.Canceled) {
				t.Fatalf("gateway did not observe the canceled lookup context: %v", gateway.lookupErr)
			}
			if scenario == "completed" {
				if pollErr != nil || got == nil || got.Status != "completed" {
					t.Fatalf("valid result was lost: %+v, %v", got, pollErr)
				}
			} else {
				if got != nil || !errors.Is(pollErr, kafudaapp.ErrKnownOrderLookupFailed) || errors.Is(pollErr, context.DeadlineExceeded) || errors.Is(pollErr, context.Canceled) {
					t.Fatalf("lookup failure was not classified: %+v, %v", got, pollErr)
				}
			}
			wantStatus := kafudadomain.AttemptProcessing
			if wantReview {
				wantStatus = kafudadomain.AttemptReview
			}
			stored, err := store.GetAttempt(ctx, attempt.ID)
			if err != nil || stored == nil || stored.Status != wantStatus || stored.ActiveOrderID == nil || *stored.ActiveOrderID != attempt.OrderID ||
				stored.UpstreamOrderID == nil || *stored.UpstreamOrderID != 101 || stored.Reference != attempt.Reference || stored.NextPollAt == nil {
				t.Fatalf("lookup changed the attempt unexpectedly: %+v, %v", stored, err)
			}
			if scenario == "completed" {
				if !stored.NextPollAt.Equal(due) || !stored.UpdatedAt.Equal(now) {
					t.Fatalf("successful lookup unexpectedly wrote a schedule: %+v", stored)
				}
			} else {
				delay := retry.Sub(due)
				if scenario == "rate_limited" {
					delay = 3 * time.Minute
					var apiErr *kafudacontract.APIError
					if errors.As(pollErr, &apiErr) {
						t.Fatalf("rate-limit failure exposed gateway error: %v", pollErr)
					}
				}
				minimumDue := due.Add(delay)
				if actualDelay := stored.NextPollAt.Sub(stored.UpdatedAt); actualDelay != delay ||
					stored.NextPollAt.Before(minimumDue) || stored.NextPollAt.After(minimumDue.Add(30*time.Second)) {
					t.Fatalf("next poll = %v, delay = %v, want due no earlier than %v with delay %v", stored.NextPollAt, actualDelay, minimumDue, delay)
				}
			}
			if wantReview {
				record, err := store.GetRecharge(ctx, 10)
				if err != nil || record == nil || record.Stage != kafudadomain.StageManualReview || record.ReasonCode != "lookup_deadline_exceeded" {
					t.Fatalf("deadline did not enter review: %+v, %v", record, err)
				}
				if err := service.RecordResult(ctx, attempt.ID, result, retry, time.Time{}); err != nil {
					t.Fatal(err)
				}
				record, err = store.GetRecharge(ctx, 10)
				if err != nil || record == nil || record.Stage != kafudadomain.StageCompleted {
					t.Fatalf("late success did not complete: %+v, %v", record, err)
				}
			}
		})
	}
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

func TestCompletionEmailClaimDoesNotBlockOtherDueOrders(t *testing.T) {
	db, store, service := database(t)
	ctx := t.Context()
	now := time.Now().UTC()
	for _, orderID := range []uint{10, 20} {
		prepare(t, service, orderID, now)
		if err := db.Model(&kafudadomain.Recharge{}).Where("order_id = ?", orderID).
			UpdateColumns(map[string]any{"stage": kafudadomain.StageCompleted, "completion_email_pending": true}).Error; err != nil {
			t.Fatal(err)
		}
	}
	retryAt := now.Add(time.Minute)
	if claimed, err := store.ClaimCompletionEmail(ctx, 10, now, retryAt); err != nil || !claimed {
		t.Fatalf("first claim: %v, %v", claimed, err)
	}
	if claimed, err := store.ClaimCompletionEmail(ctx, 10, now, retryAt); err != nil || claimed {
		t.Fatalf("duplicate claim: %v, %v", claimed, err)
	}
	due, err := store.PendingCompletionEmails(ctx, now, 1)
	if err != nil || len(due) != 1 || due[0] != 20 {
		t.Fatalf("older held task blocked due order: %v, %v", due, err)
	}
	due, err = store.PendingCompletionEmails(ctx, retryAt, 2)
	if err != nil || len(due) != 2 || due[0] != 20 || due[1] != 10 {
		t.Fatalf("unacknowledged claim was not retried after expiry: %v, %v", due, err)
	}
}
