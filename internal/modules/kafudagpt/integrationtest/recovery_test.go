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
	lease := claimTestKnownOrderPoll(t, ctx, store, review, due)
	if err := service.RescheduleKnownOrderResult(ctx, lease, due, due, retry, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := service.RescheduleKnownOrderResult(ctx, lease, due, due, retry.Add(time.Minute), time.Second); !errors.Is(err, kafudadomain.ErrConflict) {
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

func claimTestKnownOrderPoll(t *testing.T, ctx context.Context, store kafudacontract.Store, attempt kafudadomain.Attempt, now time.Time) kafudaapp.KnownOrderPollLease {
	t.Helper()
	if attempt.NextPollAt == nil {
		t.Fatal("known order does not have a poll schedule")
	}
	lease := kafudaapp.KnownOrderPollLease{
		AttemptID: attempt.ID, ScheduledAt: *attempt.NextPollAt, Until: now.Add(time.Minute).UTC().Truncate(time.Microsecond),
	}
	if claimed, err := store.ClaimKnownOrderPoll(ctx, attempt, now, lease.Until); err != nil || !claimed {
		t.Fatalf("claim known order poll: claimed=%t, err=%v", claimed, err)
	}
	return lease
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
	for _, scenario := range []struct {
		name          string
		previousDelay time.Duration
		baseInterval  time.Duration
		retryAfter    time.Duration
		wantDelay     time.Duration
	}{
		{name: "completed", previousDelay: time.Minute, wantDelay: 2 * time.Minute},
		{name: "timeout", previousDelay: time.Minute, wantDelay: 2 * time.Minute},
		{name: "wrong_reference", previousDelay: time.Minute, wantDelay: 2 * time.Minute},
		{name: "deadline", previousDelay: time.Minute, wantDelay: 2 * time.Minute},
		{name: "lookup_canceled", previousDelay: time.Minute, wantDelay: 2 * time.Minute},
		{name: "rate_limited", previousDelay: time.Minute, retryAfter: 3 * time.Minute, wantDelay: 3 * time.Minute},
		{name: "rate_limit_below_backoff", previousDelay: time.Minute, retryAfter: time.Second, wantDelay: 2 * time.Minute},
		{name: "rate_limit_above_cap", previousDelay: 8 * time.Minute, retryAfter: 20 * time.Minute, wantDelay: 20 * time.Minute},
		{name: "doubled_persisted_interval", previousDelay: 4 * time.Minute, wantDelay: 8 * time.Minute},
		{name: "capped_persisted_interval", previousDelay: 8 * time.Minute, wantDelay: 15 * time.Minute},
		{name: "capped_long_persisted_interval", previousDelay: time.Hour, wantDelay: 15 * time.Minute},
		{name: "capped_base_interval", previousDelay: time.Second, baseInterval: 20 * time.Minute, wantDelay: 15 * time.Minute},
		{name: "base_interval_for_short_delay", previousDelay: 10 * time.Second, wantDelay: time.Minute},
		{name: "base_interval_for_legacy_due_time", previousDelay: -time.Minute, wantDelay: time.Minute},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			db, store, service := database(t)
			ctx, now := context.Background(), time.Now().UTC()
			lookupCtx, cancelLookup := context.WithCancel(ctx)
			defer cancelLookup()
			prepare(t, service, 10, now)
			attempt := reserve(t, service, 10, now).Attempt
			due := now.Add(max(scenario.previousDelay, time.Second))
			if err := service.MarkSubmitting(ctx, attempt.ID, "quo_1", now, due); err != nil {
				t.Fatal(err)
			}
			result := kafudacontract.OrderResult{ID: 101, Reference: attempt.Reference, Product: "gpt", Plan: "plus", Country: "PH", Status: "processing"}
			if err := service.RecordResult(ctx, attempt.ID, result, now, due); err != nil {
				t.Fatal(err)
			}
			if scenario.previousDelay <= 0 {
				// Exercise persisted schedules whose update followed the due time.
				if err := db.Model(&kafudadomain.Attempt{}).Where("id = ?", attempt.ID).
					UpdateColumn("next_poll_at", now.Add(scenario.previousDelay)).Error; err != nil {
					t.Fatal(err)
				}
				due = now
			}
			result.Status = "completed"
			gateway := &knownOrderLookupGateway{result: &result}
			switch scenario.name {
			case "completed":
			case "lookup_canceled":
				gateway.cancelLookup = cancelLookup
			case "wrong_reference":
				result.Reference = "wrong"
			default:
				gateway.err = context.DeadlineExceeded
			}
			if scenario.retryAfter > 0 {
				gateway.err = &kafudacontract.APIError{HTTPStatus: 429, RetryAfter: scenario.retryAfter}
			}
			reviewAfter := 2 * time.Hour
			wantReview := scenario.name == "deadline" || scenario.name == "lookup_canceled"
			if wantReview {
				reviewAfter = time.Minute
			}
			baseInterval := scenario.baseInterval
			if baseInterval == 0 {
				baseInterval = time.Minute
			}
			got, pollErr := service.PollKnownOrder(ctx, lookupCtx, gateway, attempt.ID, due, baseInterval, reviewAfter)
			if gateway.id != 101 {
				t.Fatalf("queried wrong upstream ID: %d", gateway.id)
			}
			if scenario.name == "lookup_canceled" && !errors.Is(gateway.lookupErr, context.Canceled) {
				t.Fatalf("gateway did not observe the canceled lookup context: %v", gateway.lookupErr)
			}
			if scenario.name == "completed" {
				if pollErr != nil || got == nil || got.Status != "completed" {
					t.Fatalf("valid result was lost: %+v, %v", got, pollErr)
				}
				if got.RetryDelay != scenario.wantDelay {
					t.Fatalf("lookup retry delay = %v, want %v", got.RetryDelay, scenario.wantDelay)
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
			if scenario.name == "completed" {
				if !stored.NextPollAt.Equal(due) || !stored.UpdatedAt.Equal(now) ||
					stored.KnownOrderPollLeaseUntil == nil || !stored.KnownOrderPollLeaseUntil.Equal(got.Lease.Until) {
					t.Fatalf("successful lookup must hold its lease and leave the schedule unchanged: %+v", stored)
				}
			} else {
				if scenario.retryAfter > 0 {
					var apiErr *kafudacontract.APIError
					if !errors.Is(pollErr, kafudaapp.ErrKnownOrderRateLimited) || errors.As(pollErr, &apiErr) {
						t.Fatalf("rate-limit failure was not classified: %v", pollErr)
					}
				}
				minimumDue := due.Add(scenario.wantDelay)
				if actualDelay := stored.NextPollAt.Sub(stored.UpdatedAt); actualDelay != scenario.wantDelay || stored.KnownOrderPollLeaseUntil != nil ||
					stored.NextPollAt.Before(minimumDue) || stored.NextPollAt.After(minimumDue.Add(30*time.Second)) {
					t.Fatalf("next poll = %v, delay = %v, want due no earlier than %v with delay %v", stored.NextPollAt, actualDelay, minimumDue, scenario.wantDelay)
				}
			}
			if wantReview {
				record, err := store.GetRecharge(ctx, 10)
				if err != nil || record == nil || record.Stage != kafudadomain.StageManualReview || record.ReasonCode != "lookup_deadline_exceeded" {
					t.Fatalf("deadline did not enter review: %+v, %v", record, err)
				}
				if err := service.RecordResult(ctx, attempt.ID, result, *stored.NextPollAt, time.Time{}); err != nil {
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

func TestExpiredKnownOrderPollFencesOlderWorker(t *testing.T) {
	for _, oldStatus := range []string{"processing", "completed"} {
		t.Run(oldStatus, func(t *testing.T) {
			_, store, service := database(t)
			ctx, now := t.Context(), time.Now().UTC().Truncate(time.Microsecond)
			const orderID = 10
			prepare(t, service, orderID, now)
			attempt := reserve(t, service, orderID, now).Attempt
			due := now.Add(time.Minute)
			if err := service.MarkSubmitting(ctx, attempt.ID, "quo_lease", now, due); err != nil {
				t.Fatal(err)
			}
			processing := kafudacontract.OrderResult{
				ID: 101, Reference: attempt.Reference, Product: "gpt", Plan: "plus", Country: "PH", Status: "processing",
			}
			if err := service.RecordResult(ctx, attempt.ID, processing, now, due); err != nil {
				t.Fatal(err)
			}
			previousResponse := processing
			previousResponse.Status = oldStatus
			first, err := service.PollKnownOrder(ctx, ctx, &knownOrderLookupGateway{result: &previousResponse},
				attempt.ID, due, time.Minute, time.Hour)
			if err != nil || first == nil {
				t.Fatalf("initial poll = %+v, %v", first, err)
			}

			// Leave the first result unapplied, as if its worker stopped after GET.
			replacement := kafudaapp.New(store)
			expires := first.Lease.Until
			gateway := &knownOrderLookupGateway{result: &processing}
			beforeExpiry := expires.Add(-time.Microsecond)
			if polled, err := replacement.PollKnownOrder(ctx, ctx, gateway, attempt.ID, beforeExpiry, time.Minute, time.Hour); polled != nil ||
				!errors.Is(err, kafudadomain.ErrConflict) || gateway.id != 0 {
				t.Fatalf("live lease allowed a second lookup: %+v, %v, upstream ID %d", polled, err, gateway.id)
			}
			second, err := replacement.PollKnownOrder(ctx, ctx, gateway, attempt.ID, expires, time.Minute, time.Hour)
			if err != nil || second == nil || gateway.id != processing.ID || !second.Lease.Until.After(expires) || !second.Lease.ScheduledAt.Equal(due) {
				t.Fatalf("expired lease was not replaced on the original schedule: %+v, %v", second, err)
			}
			received := expires.Add(time.Second)
			next := received.Add(10 * time.Second)
			if err := service.RescheduleKnownOrderResult(ctx, first.Lease, due, received, next, time.Hour); !errors.Is(err, kafudadomain.ErrConflict) {
				t.Fatalf("old worker overwrote replacement lease on failure: %v", err)
			}
			apply := func(polled *kafudaapp.KnownOrderPoll, startedAt time.Time) (bool, error) {
				var completed bool
				err := store.WithinTransaction(ctx, func(tx kafudacontract.Transaction) error {
					var err error
					completed, err = kafudaapp.RecordResultInTransaction(tx, orderID, attempt.ID, polled.OrderResult,
						startedAt, received, next, time.Hour, polled.Lease.Until)
					return err
				})
				return completed, err
			}
			if oldStatus == "processing" {
				if completed, err := apply(first, due); completed || !errors.Is(err, kafudadomain.ErrConflict) {
					t.Fatalf("old processing result was accepted: completed=%t, err=%v", completed, err)
				}
			}
			stillOwned, err := store.GetAttempt(ctx, attempt.ID)
			if err != nil || stillOwned == nil || stillOwned.ActiveOrderID == nil || *stillOwned.ActiveOrderID != orderID ||
				stillOwned.Status != kafudadomain.AttemptProcessing || !stillOwned.UpdatedAt.Equal(now) ||
				stillOwned.NextPollAt == nil || !stillOwned.NextPollAt.Equal(due) || stillOwned.KnownOrderPollLeaseUntil == nil ||
				!stillOwned.KnownOrderPollLeaseUntil.Equal(second.Lease.Until) {
				t.Fatalf("stale worker altered active ownership or schedule: %+v, %v", stillOwned, err)
			}
			if oldStatus == "completed" {
				if completed, err := apply(first, due); err != nil || !completed {
					t.Fatalf("verified late terminal result was lost: completed=%t, err=%v", completed, err)
				}
				if completed, err := apply(second, expires); completed || !errors.Is(err, kafudadomain.ErrInvalidTransition) {
					t.Fatalf("progress reopened terminal result: completed=%t, err=%v", completed, err)
				}
			} else if completed, err := apply(second, expires); err != nil || completed {
				t.Fatalf("replacement owner could not save progress: completed=%t, err=%v", completed, err)
			}
			finished, err := store.GetAttempt(ctx, attempt.ID)
			if err != nil || finished == nil || finished.Reference != attempt.Reference || finished.KnownOrderPollLeaseUntil != nil ||
				!finished.UpdatedAt.Equal(received) {
				t.Fatalf("applied result did not release its lease: %+v, %v", finished, err)
			}
			if oldStatus == "completed" {
				if finished.Status != kafudadomain.AttemptCompleted || finished.ActiveOrderID != nil || finished.NextPollAt != nil {
					t.Fatalf("terminal result left active state: %+v", finished)
				}
			} else if finished.Status != kafudadomain.AttemptProcessing || finished.ActiveOrderID == nil || *finished.ActiveOrderID != orderID ||
				finished.NextPollAt == nil || !finished.NextPollAt.Equal(next) {
				t.Fatalf("progress lost the original active slot or next schedule: %+v", finished)
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
