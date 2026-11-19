package integrationtest

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"
	"time"

	kafudaapp "github.com/dujiao-next/internal/modules/kafudagpt/application"
	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
)

func completeWithoutCardCharge(t *testing.T, service *kafudaapp.Service, at time.Time) (uint, kafudacontract.OrderResult) {
	t.Helper()
	ctx := t.Context()
	prepare(t, service, 10, at)
	attempt := reserve(t, service, 10, at).Attempt
	if err := service.MarkSubmitting(ctx, attempt.ID, "quote_1", at, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	upstreamCompletedAt := at.Add(-48 * time.Hour)
	result := kafudacontract.OrderResult{
		ID: 101, Reference: attempt.Reference, Product: "gpt", Plan: "plus", Country: "PH",
		Status: "completed", CompletedAt: &upstreamCompletedAt, MaskedEmail: "a***@***",
	}
	if err := service.RecordResult(ctx, attempt.ID, result, at, time.Time{}); err != nil {
		t.Fatal(err)
	}
	return attempt.ID, result
}

func TestPollCardChargeFillsOnlyMissingDebit(t *testing.T) {
	for name, amount := range map[string]int64{"confirmed_zero": 0, "above_snapshot_cap": 9999} {
		t.Run(name, func(t *testing.T) {
			_, store, service := database(t)
			ctx := t.Context()
			completedAt := time.Now().UTC().Truncate(time.Microsecond)
			attemptID, result := completeWithoutCardCharge(t, service, completedAt)
			before, err := store.GetRecharge(ctx, 10)
			if err != nil || before == nil || before.Stage != kafudadomain.StageCompleted {
				t.Fatalf("fixture should be completed: %+v, %v", before, err)
			}
			firstDue := completedAt.Add(time.Minute)
			if due, err := service.DueCardChargePolls(ctx, firstDue.Add(-time.Microsecond), 2); err != nil || len(due) != 0 {
				t.Fatalf("found attempt before first poll delay: %+v, %v", due, err)
			}
			due, err := service.DueCardChargePolls(ctx, firstDue, 2)
			if err != nil || len(due) != 1 || due[0].ID != attemptID {
				t.Fatalf("missing historically completed attempt: %+v, %v", due, err)
			}
			gateway := &knownOrderLookupGateway{result: &result}
			if err := service.PollCardCharge(ctx, ctx, gateway, due[0], firstDue); err != nil || gateway.id != result.ID {
				t.Fatalf("lookup with pending card debit failed: %v, upstream ID %d", err, gateway.id)
			}
			stored, err := store.GetAttempt(ctx, attemptID)
			if err != nil || stored == nil || stored.ChargedUSDCents != nil || stored.NextCardChargePollAt == nil ||
				stored.NextCardChargePollAt.Before(firstDue.Add(30*time.Minute)) {
				t.Fatalf("missing amount did not reschedule: %+v, %v", stored, err)
			}

			// Restart the poller and use the persisted retry time.
			service = kafudaapp.New(store)
			retry := *stored.NextCardChargePollAt
			if early, err := service.DueCardChargePolls(ctx, retry.Add(-time.Microsecond), 2); err != nil || len(early) != 0 {
				t.Fatalf("retry became due early: %+v, %v", early, err)
			}
			result.ChargedUSDCents = &amount
			result.MaskedEmail = "b***@***"
			if err := service.PollCardCharge(ctx, ctx, gateway, *stored, retry); err != nil {
				t.Fatal(err)
			}
			expected := due[0]
			expected.ChargedUSDCents = &amount
			after, err := store.GetAttempt(ctx, attemptID)
			if err != nil || after == nil || !reflect.DeepEqual(*after, expected) {
				t.Fatalf("reconciliation changed attempt facts beyond the debit: %+v, %v", after, err)
			}
			recharge, err := store.GetRecharge(ctx, 10)
			if err != nil || !reflect.DeepEqual(recharge, before) {
				t.Fatalf("reconciliation changed completion facts: %+v, %v", recharge, err)
			}

			gateway.id = 0
			if err := service.PollCardCharge(ctx, ctx, gateway, *stored, retry); !errors.Is(err, kafudadomain.ErrConflict) || gateway.id != 0 {
				t.Fatalf("stale candidate queried again: %v, upstream ID %d", err, gateway.id)
			}
		})
	}
}

func TestPollCardChargeRejectsUntrustedResultsAndReschedules(t *testing.T) {
	for _, scenario := range []string{"wrong_id", "wrong_reference", "wrong_product", "wrong_plan", "wrong_country", "wrong_status", "negative_debit", "lookup_canceled", "rate_limited"} {
		t.Run(scenario, func(t *testing.T) {
			_, store, service := database(t)
			ctx := t.Context()
			lookupCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			now := time.Now().UTC().Truncate(time.Microsecond)
			attemptID, result := completeWithoutCardCharge(t, service, now)
			wantUpstreamID := result.ID
			dueAt := now.Add(time.Minute)
			due, err := service.DueCardChargePolls(ctx, dueAt, 2)
			if err != nil || len(due) != 1 {
				t.Fatalf("want one eligible attempt: %+v, %v", due, err)
			}
			amount := int64(900)
			result.ChargedUSDCents = &amount
			gateway := &knownOrderLookupGateway{result: &result}
			wantDelay := 30 * time.Minute
			switch scenario {
			case "wrong_id":
				result.ID++
			case "wrong_reference":
				result.Reference += "-mismatch"
			case "wrong_product":
				result.Product = "gpt_credits"
			case "wrong_plan":
				result.Plan = "pro"
			case "wrong_country":
				result.Country = "US"
			case "wrong_status":
				result.Status = "failed"
			case "negative_debit":
				amount = -1
			case "lookup_canceled":
				gateway.cancelLookup = cancel
			case "rate_limited":
				wantDelay = time.Hour
				gateway.err = &kafudacontract.APIError{HTTPStatus: http.StatusTooManyRequests, RetryAfter: wantDelay}
			}
			pollErr := service.PollCardCharge(ctx, lookupCtx, gateway, due[0], dueAt)
			var apiErr *kafudacontract.APIError
			if gateway.id != wantUpstreamID || !errors.Is(pollErr, kafudaapp.ErrKnownOrderLookupFailed) ||
				errors.As(pollErr, &apiErr) || errors.Is(pollErr, context.Canceled) ||
				errors.Is(pollErr, kafudaapp.ErrKnownOrderRateLimited) != (scenario == "rate_limited") {
				t.Fatalf("untrusted lookup was not classified safely: %v, upstream ID %d", pollErr, gateway.id)
			}
			if scenario == "lookup_canceled" && !errors.Is(gateway.lookupErr, context.Canceled) {
				t.Fatalf("gateway did not see its context cancel: %v", gateway.lookupErr)
			}
			after, err := store.GetAttempt(ctx, attemptID)
			if err != nil || after == nil || after.ChargedUSDCents != nil ||
				after.NextCardChargePollAt == nil || after.NextCardChargePollAt.Before(dueAt.Add(wantDelay)) ||
				after.Status != kafudadomain.AttemptCompleted || !after.UpdatedAt.Equal(now) {
				t.Fatalf("failure did not leave a retry without changing completion: %+v, %v", after, err)
			}
			expired := now.Add(24 * time.Hour)
			if due, err := service.DueCardChargePolls(ctx, expired, 2); err != nil || len(due) != 0 {
				t.Fatalf("eligible after polling window elapsed: %+v, %v", due, err)
			}
			gateway.id = 0
			if err := service.PollCardCharge(ctx, ctx, gateway, *after, expired); !errors.Is(err, kafudadomain.ErrConflict) || gateway.id != 0 {
				t.Fatalf("expired candidate queried again: %v, upstream ID %d", err, gateway.id)
			}
		})
	}
}
