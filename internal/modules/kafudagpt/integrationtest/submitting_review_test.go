package integrationtest

import (
	"errors"
	"testing"
	"time"

	kafudaapp "github.com/dujiao-next/internal/modules/kafudagpt/application"
	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
)

func TestReviewedAttemptAcceptsSubmittingWithoutClearingReview(t *testing.T) {
	for _, reason := range []string{kafudadomain.ReasonCostLimitExceeded, kafudadomain.ReasonResultApplyFailed} {
		t.Run(reason, func(t *testing.T) {
			_, store, service := database(t)
			ctx, now := t.Context(), time.Now().UTC()
			prepare(t, service, 10, now)
			attempt := reserve(t, service, 10, now).Attempt
			const quoteID = "quo_review"
			due := now.Add(time.Minute)
			if err := service.MarkSubmitting(ctx, attempt.ID, quoteID, now, due); err != nil {
				t.Fatal(err)
			}
			result := kafudacontract.OrderResult{ID: 101, Reference: attempt.Reference, Product: "gpt", Plan: "plus", Country: "PH", Status: "submitting"}
			initial := result
			if reason == kafudadomain.ReasonCostLimitExceeded {
				charge := snapshot().MaxCostCents + 1
				initial.Status, initial.ChargedUSDCents = "needs_review", &charge
			}
			if err := service.RecordResult(ctx, attempt.ID, initial, now, due); err != nil {
				t.Fatal(err)
			}
			if reason == kafudadomain.ReasonResultApplyFailed {
				retry := due.Add(time.Second)
				stored, err := store.GetAttempt(ctx, attempt.ID)
				if err != nil || stored == nil {
					t.Fatalf("known order poll fixture: %+v, %v", stored, err)
				}
				lease := claimTestKnownOrderPoll(t, ctx, store, *stored, due)
				if err := service.RescheduleKnownOrderResult(ctx, lease, due, due, retry, time.Minute); err != nil {
					t.Fatal(err)
				}
				due = retry
			}
			before, err := store.GetRecharge(ctx, attempt.OrderID)
			if err != nil || before == nil || before.Stage != kafudadomain.StageManualReview || before.ReasonCode != reason {
				t.Fatalf("review fixture = %+v, %v", before, err)
			}
			gateway := &knownOrderLookupGateway{result: &result}
			got, err := service.PollKnownOrder(ctx, ctx, gateway, attempt.ID, due, time.Minute, time.Hour)
			if err != nil || got == nil || got.Status != "submitting" || gateway.id != result.ID {
				t.Fatalf("submitting lookup = %+v, %v (upstream id %d)", got, err, gateway.id)
			}
			unchanged, err := store.GetRecharge(ctx, attempt.OrderID)
			if err != nil || unchanged == nil || unchanged.Version != before.Version {
				t.Fatalf("read-only lookup changed recharge = %+v, %v", unchanged, err)
			}
			receivedAt := due.Add(time.Second)
			retryAt := receivedAt.Add(time.Minute)
			apply := func(startedAt, received, retry time.Time) error {
				return store.WithinTransaction(ctx, func(tx kafudacontract.Transaction) error {
					completed, applyErr := kafudaapp.RecordResultInTransaction(tx, attempt.OrderID, attempt.ID, got.OrderResult, startedAt, received, retry, 0, got.Lease.Until)
					if completed {
						t.Error("submitting result cannot complete the recharge")
					}
					return applyErr
				})
			}
			if err := apply(due, receivedAt, retryAt); err != nil {
				t.Fatalf("apply matched submitting result: %v", err)
			}
			if err := apply(due, receivedAt.Add(time.Second), retryAt.Add(time.Minute)); !errors.Is(err, kafudadomain.ErrConflict) {
				t.Fatalf("older lookup regressed the schedule: %v", err)
			}
			updated, err := store.GetRecharge(ctx, attempt.OrderID)
			if err != nil || updated == nil || updated.Stage != before.Stage || updated.ReasonCode != reason || updated.CompletedAt != nil ||
				updated.Version != before.Version+1 || !updated.UpdatedAt.Equal(receivedAt) {
				t.Fatalf("review after matched submitting = %+v, %v", updated, err)
			}
			stored, err := store.GetAttempt(ctx, attempt.ID)
			if err != nil || stored == nil || stored.Status != kafudadomain.AttemptReview || stored.ActiveOrderID == nil || *stored.ActiveOrderID != attempt.OrderID ||
				stored.UpstreamOrderID == nil || *stored.UpstreamOrderID != result.ID || stored.Reference != attempt.Reference || stored.QuoteID != quoteID ||
				!stored.UpdatedAt.Equal(receivedAt) || stored.NextPollAt == nil || !stored.NextPollAt.Equal(retryAt) ||
				(initial.ChargedUSDCents != nil && (stored.ChargedUSDCents == nil || *stored.ChargedUSDCents != *initial.ChargedUSDCents)) {
				t.Fatalf("matched submitting altered attempt identity, review or poll schedule = %+v, %v", stored, err)
			}
		})
	}
}

func TestRecoveredUnknownSubmissionBindsMatchingSubmitting(t *testing.T) {
	_, store, service := database(t)
	ctx, now := t.Context(), time.Now().UTC()
	prepare(t, service, 10, now)
	attempt := reserve(t, service, 10, now).Attempt
	due := now.Add(time.Minute)
	if err := service.MarkSubmitting(ctx, attempt.ID, "quo_recover", now, due); err != nil {
		t.Fatal(err)
	}
	if err := service.RecoverUnidentifiedSubmissions(ctx, due, 10); err != nil {
		t.Fatal(err)
	}
	matched := kafudacontract.OrderResult{ID: 101, Reference: attempt.Reference, Product: "gpt", Plan: "plus", Country: "PH", Status: "submitting"}
	gateway := &knownOrderLookupGateway{result: &matched}
	retry := due.Add(time.Minute)
	if got, err := service.PollKnownOrder(ctx, ctx, gateway, attempt.ID, due, time.Minute, time.Hour); !errors.Is(err, kafudadomain.ErrConflict) || got != nil || gateway.id != 0 {
		t.Fatalf("unidentified attempt performed numeric lookup: %+v, %v (id %d)", got, err, gateway.id)
	}
	if err := service.RecordResult(ctx, attempt.ID, matched, due, retry); err != nil {
		t.Fatalf("matched submitting reply was rejected: %v", err)
	}
	known, err := store.DueKnownOrderPolls(ctx, retry, 10)
	if err != nil || len(known) != 1 || known[0].ID != attempt.ID || known[0].UpstreamOrderID == nil || *known[0].UpstreamOrderID != matched.ID || known[0].Status != kafudadomain.AttemptReview {
		t.Fatalf("bound attempt is not eligible for numeric polling: %+v, %v", known, err)
	}
	matched.Status = "processing"
	got, err := service.PollKnownOrder(ctx, ctx, gateway, attempt.ID, retry, time.Minute, time.Hour)
	if err != nil || got == nil || got.Status != "processing" || gateway.id != matched.ID {
		t.Fatalf("known order did not resume progress: %+v, %v (id %d)", got, err, gateway.id)
	}
	if err := store.WithinTransaction(ctx, func(tx kafudacontract.Transaction) error {
		_, err := kafudaapp.RecordResultInTransaction(tx, attempt.OrderID, attempt.ID, got.OrderResult, retry, retry, retry.Add(time.Minute), 0, got.Lease.Until)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	record, err := store.GetRecharge(ctx, attempt.OrderID)
	if err != nil || record == nil || record.Stage != kafudadomain.StageManualReview || record.ReasonCode != "outcome_unknown" {
		t.Fatalf("progress incorrectly cleared manual review: %+v, %v", record, err)
	}
}

func TestProcessingAttemptRejectsSubmittingRegression(t *testing.T) {
	_, store, service := database(t)
	ctx, now := t.Context(), time.Now().UTC()
	prepare(t, service, 10, now)
	attempt := reserve(t, service, 10, now).Attempt
	due := now.Add(time.Minute)
	if err := service.MarkSubmitting(ctx, attempt.ID, "quo_progress", now, due); err != nil {
		t.Fatal(err)
	}
	result := kafudacontract.OrderResult{ID: 101, Reference: attempt.Reference, Product: "gpt", Plan: "plus", Country: "PH", Status: "processing"}
	if err := service.RecordResult(ctx, attempt.ID, result, now, due); err != nil {
		t.Fatal(err)
	}
	result.Status = "submitting"
	if err := service.RecordResult(ctx, attempt.ID, result, due, due.Add(time.Minute)); !errors.Is(err, kafudadomain.ErrInvalidTransition) {
		t.Fatalf("later received submitting regressed a processing order: %v", err)
	}
	stored, err := store.GetAttempt(ctx, attempt.ID)
	if err != nil || stored == nil || stored.Status != kafudadomain.AttemptProcessing || !stored.UpdatedAt.Equal(now) || stored.NextPollAt == nil || !stored.NextPollAt.Equal(due) {
		t.Fatalf("rejected status changed active polling: %+v, %v", stored, err)
	}
}
