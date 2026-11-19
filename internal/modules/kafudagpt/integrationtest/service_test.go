package integrationtest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	kafudaapp "github.com/dujiao-next/internal/modules/kafudagpt/application"
	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
	"github.com/dujiao-next/internal/modules/kafudagpt/infrastructure/gormstore"
	"github.com/dujiao-next/internal/shared/money"

	"github.com/glebarez/sqlite"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func database(t *testing.T) (*gorm.DB, *gormstore.Store, *kafudaapp.Service) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:kafuda_%d?mode=memory&cache=shared", time.Now().UnixNano())), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = pool.Close() })
	if err := db.AutoMigrate(&kafudadomain.Catalog{}, &kafudadomain.Recharge{}, &kafudadomain.Attempt{}); err != nil {
		t.Fatal(err)
	}
	store := gormstore.New(db)
	return db, store, kafudaapp.New(store)
}

func snapshot() kafudadomain.Snapshot {
	return kafudadomain.Snapshot{
		ProductID: 1, SKUID: 2, Product: "gpt", Plan: "plus", Country: "PH", PayMode: "auto_card",
		ProductCode: "P5378XB", PaymentProfile: "primary", Currency: "CNY",
		CostPrice: money.FromDecimal(decimal.NewFromInt(100)), SellPrice: money.FromDecimal(decimal.NewFromInt(150)), MaxCostCents: 2500,
	}
}

func prepare(t *testing.T, service *kafudaapp.Service, orderID uint, now time.Time) {
	t.Helper()
	if _, err := service.Prepare(context.Background(), orderID, snapshot(), now); err != nil {
		t.Fatal(err)
	}
}

func reserve(t *testing.T, service *kafudaapp.Service, orderID uint, now time.Time) *kafudaapp.Reservation {
	t.Helper()
	result, err := service.Reserve(context.Background(), orderID, now, now.Add(time.Minute))
	if err != nil || !result.Created {
		t.Fatalf("reserve = (%+v, %v)", result, err)
	}
	return result
}

func TestConcurrentSubmissionsReserveExactlyOneAttempt(t *testing.T) {
	db, store, service := database(t)
	now := time.Now().UTC()
	prepare(t, service, 10, now)
	var created atomic.Int32
	var group sync.WaitGroup
	ids := make(chan uint, 16)
	for range 16 {
		group.Go(func() {
			result, err := service.Reserve(context.Background(), 10, now, now.Add(time.Minute))
			if err != nil {
				t.Error(err)
				return
			}
			if result.Created {
				created.Add(1)
			}
			ids <- result.Attempt.ID
		})
	}
	group.Wait()
	close(ids)
	var first uint
	for id := range ids {
		if first == 0 {
			first = id
		}
		if id != first {
			t.Fatal("concurrent request received a new attempt")
		}
	}
	if created.Load() != 1 {
		t.Fatalf("created %d attempts, want 1", created.Load())
	}
	attempt, err := store.GetAttempt(context.Background(), first)
	if err != nil || len(attempt.Reference) > 80 || attempt.ActiveOrderID == nil {
		t.Fatalf("invalid reservation: %+v, %v", attempt, err)
	}
	// The database invariant also rejects callers that bypass the service.
	orderID := uint(10)
	if err := db.Create(&kafudadomain.Attempt{OrderID: 10, Sequence: 2, ActiveOrderID: &orderID, Reference: "bypass", Status: kafudadomain.AttemptChecking}).Error; err == nil {
		t.Fatal("database allowed two active attempts")
	}
}

func TestUnknownOutcomeRetainsSlotAndLateSuccessConverges(t *testing.T) {
	_, store, service := database(t)
	ctx, now := context.Background(), time.Now().UTC()
	prepare(t, service, 10, now)
	reservation := reserve(t, service, 10, now)
	id := reservation.Attempt.ID
	if err := service.MarkSubmitting(ctx, id, "quo_1", now, now.Add(10*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := service.MarkUnknown(ctx, id, now, now.Add(20*time.Second)); err != nil {
		t.Fatal(err)
	}
	// Reconstructing the service simulates a new worker/process, not in-memory locks.
	service = kafudaapp.New(store)
	again, err := service.Reserve(ctx, 10, now, now.Add(time.Minute))
	if err != nil || again.Created || again.Attempt.ID != id || again.Recharge.Stage != kafudadomain.StageManualReview {
		t.Fatalf("unknown outcome allowed a new request: %+v, %v", again, err)
	}
	due, err := store.DueAttempts(ctx, now.Add(time.Minute), 100)
	if err != nil || len(due) != 1 || due[0].QuoteID != "quo_1" || due[0].Reference != reservation.Attempt.Reference {
		t.Fatalf("recovery lost original request: %+v, %v", due, err)
	}
	result := kafudacontract.OrderResult{ID: 100001, Reference: reservation.Attempt.Reference, Product: "gpt", Plan: "plus", Country: "PH", Status: "completed", MaskedEmail: "buyer@example.com"}
	if err := service.RecordResult(ctx, id, result, now, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	record, err := store.GetRecharge(ctx, 10)
	if err != nil || record.Stage != kafudadomain.StageCompleted || record.CompletedAt == nil {
		t.Fatalf("late success did not converge: %+v, %v", record, err)
	}
	firstCompleted := *record.CompletedAt
	firstVersion, firstUpdated := record.Version, record.UpdatedAt
	if err := service.RecordResult(ctx, id, result, now.Add(time.Minute), now.Add(2*time.Minute)); err != nil {
		t.Fatalf("duplicate success: %v", err)
	}
	record, _ = store.GetRecharge(ctx, 10)
	if !record.CompletedAt.Equal(firstCompleted) || record.Version != firstVersion || !record.UpdatedAt.Equal(firstUpdated) {
		t.Fatal("duplicate success rewrote the completed recharge")
	}
	attempt, _ := store.GetAttempt(ctx, id)
	if attempt.ActiveOrderID != nil || attempt.NextPollAt != nil || attempt.MaskedEmail != "" {
		t.Fatalf("terminal slot, schedule or PII not cleared: %+v", attempt)
	}
	result.Status = "processing"
	if err := service.RecordResult(ctx, id, result, now, now.Add(time.Minute)); !errors.Is(err, kafudadomain.ErrInvalidTransition) {
		t.Fatalf("stale event was accepted: %v", err)
	}
	if _, err := service.Reserve(ctx, 10, now, now.Add(time.Minute)); !errors.Is(err, kafudadomain.ErrInvalidTransition) {
		t.Fatalf("completed recharge was reopened: %v", err)
	}
}

func TestRechargeCompletionCanShareCallerTransaction(t *testing.T) {
	_, store, service := database(t)
	ctx, now := context.Background(), time.Now().UTC()
	prepare(t, service, 10, now)
	attempt := reserve(t, service, 10, now).Attempt
	if err := service.MarkSubmitting(ctx, attempt.ID, "quo_1", now, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	result := kafudacontract.OrderResult{ID: 101, Reference: attempt.Reference, Product: "gpt", Plan: "plus", Country: "PH", Status: "completed"}
	orderWriteErr := errors.New("shop order update failed")
	if err := store.WithinTransaction(ctx, func(tx kafudacontract.Transaction) error {
		completed, err := kafudaapp.RecordResultInTransaction(tx, 10, attempt.ID, result, now, now, time.Time{}, 0, time.Time{})
		if err != nil || !completed {
			t.Fatalf("expected fresh completion: %t, %v", completed, err)
		}
		return orderWriteErr
	}); !errors.Is(err, orderWriteErr) {
		t.Fatalf("transaction did not return the order error: %v", err)
	}
	record, err := store.GetRecharge(ctx, 10)
	if err != nil || record == nil || record.Stage != kafudadomain.StageSubmitting || record.CompletedAt != nil {
		t.Fatalf("caller failure did not roll back recharge: %+v, %v", record, err)
	}
	stored, err := store.GetAttempt(ctx, attempt.ID)
	if err != nil || stored == nil || stored.Status != kafudadomain.AttemptSubmitting || stored.ActiveOrderID == nil {
		t.Fatalf("caller failure did not roll back attempt: %+v, %v", stored, err)
	}
	for _, wantFresh := range []bool{true, false} {
		if err := store.WithinTransaction(ctx, func(tx kafudacontract.Transaction) error {
			completed, err := kafudaapp.RecordResultInTransaction(tx, 10, attempt.ID, result, now, now, time.Time{}, 0, time.Time{})
			if err != nil || completed != wantFresh {
				t.Fatalf("fresh completion = %t, %v; want %t", completed, err, wantFresh)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStalePollCannotRegressScheduleAndLateCompletionStillConverges(t *testing.T) {
	_, store, service := database(t)
	ctx, now := context.Background(), time.Now().UTC()
	prepare(t, service, 10, now)
	reservation := reserve(t, service, 10, now)
	id := reservation.Attempt.ID
	if err := service.MarkSubmitting(ctx, id, "quo_1", now, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	newerStart, newerReceipt := now.Add(5*time.Second), now.Add(10*time.Second)
	nextPoll := newerReceipt.Add(2 * time.Minute)
	lateReceipt := newerReceipt.Add(20 * time.Second)
	result := kafudacontract.OrderResult{ID: 101, Reference: reservation.Attempt.Reference, Product: "gpt", Plan: "plus", Country: "PH", Status: "processing"}
	apply := func(startedAt, receivedAt, next time.Time) (bool, error) {
		var completed bool
		err := store.WithinTransaction(ctx, func(tx kafudacontract.Transaction) error {
			var err error
			completed, err = kafudaapp.RecordResultInTransaction(tx, 10, id, result, startedAt, receivedAt, next, 0, time.Time{})
			return err
		})
		return completed, err
	}
	if completed, err := apply(newerStart, newerReceipt, nextPoll); err != nil || completed {
		t.Fatalf("new processing result = %t, %v", completed, err)
	}
	if completed, err := apply(now, lateReceipt, lateReceipt.Add(time.Minute)); completed || !errors.Is(err, kafudadomain.ErrConflict) {
		t.Fatalf("stale processing result was accepted: %t, %v", completed, err)
	}
	for _, failedAt := range []time.Time{now, lateReceipt} {
		if err := service.MarkUnknown(ctx, id, failedAt, failedAt.Add(time.Minute)); !errors.Is(err, kafudadomain.ErrConflict) {
			t.Fatalf("submission failure replaced a verified order at %v: %v", failedAt, err)
		}
	}
	attempt, err := store.GetAttempt(ctx, id)
	if err != nil || attempt == nil || attempt.Status != kafudadomain.AttemptProcessing ||
		attempt.NextPollAt == nil || !attempt.NextPollAt.Equal(nextPoll) || !attempt.UpdatedAt.Equal(newerReceipt) {
		t.Fatalf("stale polls changed the attempt: %+v, %v", attempt, err)
	}
	result.Status = "completed"
	// This terminal response was received earlier but applied after the newer write.
	if completed, err := apply(now, newerStart, time.Time{}); err != nil || !completed {
		t.Fatalf("late terminal result = %t, %v", completed, err)
	}
	record, err := store.GetRecharge(ctx, 10)
	if err != nil || record == nil || record.Stage != kafudadomain.StageCompleted ||
		record.CompletedAt == nil || !record.CompletedAt.Equal(newerReceipt) || !record.UpdatedAt.Equal(newerReceipt) {
		t.Fatalf("late completion did not converge with monotonic timestamps: %+v, %v", record, err)
	}
}

func TestCompletionUsesSupplierRenewalResultFromCompletedResponse(t *testing.T) {
	cancelled, notCancelled := true, false
	for _, test := range []struct {
		name              string
		processingRenewal bool
		completedRenewal  *bool
	}{
		{name: "missing", processingRenewal: false},
		{name: "not_cancelled", processingRenewal: true, completedRenewal: &notCancelled},
		{name: "cancelled", processingRenewal: false, completedRenewal: &cancelled},
	} {
		t.Run(test.name, func(t *testing.T) {
			const orderID = 10
			_, store, service := database(t)
			ctx, now := t.Context(), time.Now().UTC()
			prepare(t, service, orderID, now)
			attempt := reserve(t, service, orderID, now).Attempt
			if err := service.MarkSubmitting(ctx, attempt.ID, "quo_1", now, now.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			result := kafudacontract.OrderResult{
				ID: 101, Reference: attempt.Reference, Product: "gpt", Plan: "plus", Country: "PH",
				Status: string(kafudadomain.AttemptProcessing), RenewalCancelled: &test.processingRenewal,
			}
			apply := func(at, nextPoll time.Time, wantCompleted bool) {
				t.Helper()
				err := store.WithinTransaction(ctx, func(tx kafudacontract.Transaction) error {
					completed, err := kafudaapp.RecordResultInTransaction(tx, orderID, attempt.ID, result, at, at, nextPoll, 0, time.Time{})
					if err == nil && completed != wantCompleted {
						t.Errorf("apply %s result completed = %t, want %t", result.Status, completed, wantCompleted)
					}
					return err
				})
				if err != nil {
					t.Fatalf("apply %s result: %v", result.Status, err)
				}
			}
			apply(now.Add(time.Second), now.Add(time.Minute), false)

			result.Status, result.RenewalCancelled = string(kafudadomain.AttemptCompleted), test.completedRenewal
			apply(now.Add(2*time.Second), time.Time{}, true)
			stored, err := store.GetAttempt(ctx, attempt.ID)
			if err != nil || stored == nil || stored.Status != kafudadomain.AttemptCompleted {
				t.Fatalf("read completed attempt: %+v, %v", stored, err)
			}
			if got := stored.RenewalCancelled; test.completedRenewal == nil {
				if got != nil {
					t.Fatalf("completed response omitted renewal result but stored %t from processing", *got)
				}
			} else if got == nil || *got != *test.completedRenewal {
				t.Fatalf("completed response renewal result %t was not stored: %+v", *test.completedRenewal, stored)
			}
		})
	}
}

func TestOverCapChargeRequiresReviewAndKeepsSupplierOutcome(t *testing.T) {
	for _, initialStatus := range []string{"processing", "completed"} {
		t.Run(initialStatus, func(t *testing.T) {
			_, store, service := database(t)
			ctx, now := context.Background(), time.Now().UTC()
			prepare(t, service, 10, now)
			reservation := reserve(t, service, 10, now)
			id := reservation.Attempt.ID
			if err := service.MarkSubmitting(ctx, id, "quo_1", now, now.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			charge := reservation.Recharge.Snapshot.MaxCostCents + 1
			result := kafudacontract.OrderResult{ID: 101, Reference: reservation.Attempt.Reference, Product: "gpt", Plan: "plus", Country: "PH", Status: initialStatus, ChargedUSDCents: &charge}
			if err := service.RecordResult(ctx, id, result, now, now.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			if initialStatus == "processing" {
				attempt, err := store.GetAttempt(ctx, id)
				if err != nil || attempt == nil || attempt.ActiveOrderID == nil || attempt.NextPollAt == nil {
					t.Fatalf("in-flight attempt must retain its slot and schedule: %+v, %v", attempt, err)
				}
				failedAt := *attempt.NextPollAt
				lease := claimTestKnownOrderPoll(t, ctx, store, *attempt, failedAt)
				if err := service.RescheduleKnownOrderResult(ctx, lease, failedAt, failedAt, failedAt.Add(time.Minute), kafudaapp.DefaultPollReviewAfter); err != nil {
					t.Fatal(err)
				}
				now = failedAt
				record, err := store.GetRecharge(ctx, 10)
				if err != nil || record == nil || record.ReasonCode != "cost_limit_exceeded" {
					t.Fatalf("retrying a known result erased the review reason: %+v, %v", record, err)
				}
			}
			result.Status, result.ChargedUSDCents = "completed", nil
			if err := service.RecordResult(ctx, id, result, now.Add(time.Second), time.Time{}); err != nil {
				t.Fatal(err)
			}
			attempt, err := store.GetAttempt(ctx, id)
			if err != nil || attempt == nil || attempt.Status != kafudadomain.AttemptCompleted || attempt.ActiveOrderID != nil ||
				attempt.ChargedUSDCents == nil || *attempt.ChargedUSDCents != charge {
				t.Fatalf("supplier completion or charge lost: %+v, %v", attempt, err)
			}
			record, err := store.GetRecharge(ctx, 10)
			if err != nil || record == nil || record.Stage != kafudadomain.StageManualReview ||
				record.ReasonCode != "cost_limit_exceeded" || record.CompletedAt != nil ||
				!record.Snapshot.SellPrice.Equal(reservation.Recharge.Snapshot.SellPrice.Decimal) {
				t.Fatalf("over-cap recharge bypassed review or changed buyer price: %+v, %v", record, err)
			}
			if _, err := service.Reserve(ctx, 10, now.Add(time.Second), now.Add(time.Minute)); !errors.Is(err, kafudadomain.ErrInvalidTransition) {
				t.Fatalf("over-cap recharge allowed another attempt: %v", err)
			}
		})
	}
}

func TestOverCapTerminalFailurePermitsControlledAfterSales(t *testing.T) {
	for _, status := range []kafudadomain.AttemptStatus{kafudadomain.AttemptFailed, kafudadomain.AttemptCancelled} {
		t.Run(string(status), func(t *testing.T) {
			_, store, service := database(t)
			ctx, now := t.Context(), time.Now().UTC()
			prepare(t, service, 10, now)
			reservation := reserve(t, service, 10, now)
			attemptID := reservation.Attempt.ID
			if err := service.MarkSubmitting(ctx, attemptID, "quo_1", now, now.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			charge := reservation.Recharge.Snapshot.MaxCostCents + 1
			result := kafudacontract.OrderResult{
				ID: 101, Reference: reservation.Attempt.Reference, Product: "gpt", Plan: "plus",
				Country: "PH", Status: string(kafudadomain.AttemptProcessing), ChargedUSDCents: &charge,
			}
			if err := service.RecordResult(ctx, attemptID, result, now, now.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			active, err := store.GetAttempt(ctx, attemptID)
			if err != nil || active == nil {
				t.Fatalf("load active attempt: %+v, %v", active, err)
			}
			review, err := store.GetRecharge(ctx, 10)
			if err != nil || review == nil || review.Stage != kafudadomain.StageManualReview ||
				kafudacontract.CanResolveManualAttempt(review, active) {
				t.Fatalf("active over-cap attempt must remain blocked: %+v, %v", review, err)
			}

			result.Status, result.ChargedUSDCents = string(status), nil
			if err := service.RecordResult(ctx, attemptID, result, now.Add(time.Second), time.Time{}); err != nil {
				t.Fatal(err)
			}
			attempt, err := store.GetAttempt(ctx, attemptID)
			if err != nil || attempt == nil || attempt.Status != status || attempt.ActiveOrderID != nil || attempt.NextPollAt != nil ||
				attempt.ChargedUSDCents == nil || *attempt.ChargedUSDCents != charge {
				t.Fatalf("terminal supplier result or original charge missing: %+v, %v", attempt, err)
			}
			record, err := store.GetRecharge(ctx, 10)
			if err != nil || record == nil || record.Stage != kafudadomain.StageManualDelivery ||
				record.ReasonCode != kafudadomain.ReasonCostLimitExceeded || record.CompletedAt != nil ||
				!record.Snapshot.SellPrice.Equal(reservation.Recharge.Snapshot.SellPrice.Decimal) ||
				!kafudacontract.CanResolveManualAttempt(record, attempt) {
				t.Fatalf("terminal failure did not permit controlled after-sales: %+v, %v", record, err)
			}
		})
	}
}

func TestInputCorrectionReleasesSlotButBusinessFailureRequiresManual(t *testing.T) {
	_, store, service := database(t)
	ctx, now := context.Background(), time.Now().UTC()
	prepare(t, service, 10, now)
	first := reserve(t, service, 10, now)
	if err := service.CheckRejected(ctx, first.Attempt.ID, true, now); err != nil {
		t.Fatal(err)
	}
	second := reserve(t, service, 10, now)
	if first.Attempt.Reference == second.Attempt.Reference || second.Attempt.Sequence != 2 {
		t.Fatal("corrected input did not create a distinct recorded attempt")
	}
	if err := service.CheckRejected(ctx, second.Attempt.ID, false, now); err != nil {
		t.Fatal(err)
	}
	record, _ := store.GetRecharge(ctx, 10)
	if record.Stage != kafudadomain.StageManualDelivery {
		t.Fatalf("business rejection did not enter manual queue: %+v", record)
	}
	if _, err := service.Reserve(ctx, 10, now, now.Add(time.Minute)); !errors.Is(err, kafudadomain.ErrInvalidTransition) {
		t.Fatalf("manual takeover bypassed: %v", err)
	}
}

func TestTransactionRollbackAndSnapshotArePreserved(t *testing.T) {
	_, store, service := database(t)
	ctx, now := context.Background(), time.Now().UTC()
	prepare(t, service, 10, now)
	changed := snapshot()
	changed.CostPrice = money.FromDecimal(decimal.NewFromInt(999))
	changed.SellPrice = money.FromDecimal(decimal.NewFromInt(1999))
	if _, err := service.Prepare(ctx, 10, changed, now); err != nil {
		t.Fatal(err)
	}
	abort := errors.New("abort")
	if err := store.WithinTransaction(ctx, func(tx kafudacontract.Transaction) error {
		record, err := tx.GetRechargeForUpdate(10)
		if err != nil {
			return err
		}
		record.Stage = kafudadomain.StageChecking
		if err := tx.SaveRecharge(record); err != nil {
			return err
		}
		return abort
	}); !errors.Is(err, abort) {
		t.Fatal(err)
	}
	record, _ := store.GetRecharge(ctx, 10)
	if record.Stage != kafudadomain.StageWaiting || record.Snapshot.CostPrice.String() != "100.00" || record.Snapshot.SellPrice.String() != "150.00" {
		t.Fatalf("snapshot changed or transaction leaked: %+v", record)
	}
	stale := *record
	if err := store.SaveRecharge(record); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRecharge(&stale); !errors.Is(err, kafudadomain.ErrConflict) {
		t.Fatalf("stale write accepted: %v", err)
	}
}

func TestCatalogFailureStalenessAndExactPairs(t *testing.T) {
	_, store, _ := database(t)
	ctx, now := context.Background(), time.Now().UTC()
	valid := &kafudadomain.Catalog{Profile: "primary", Enabled: true, SyncedAt: now, ExpiresAt: now.Add(time.Hour), Options: []kafudadomain.CatalogOption{{Plan: "plus", Country: "PH", Available: true}, {Plan: "go", Country: "US", Available: true}}}
	if err := store.SaveCatalog(ctx, valid); err != nil {
		t.Fatal(err)
	}
	invalid := *valid
	invalid.Options = nil
	if err := store.SaveCatalog(ctx, &invalid); !errors.Is(err, kafudadomain.ErrInvalidCatalog) {
		t.Fatalf("invalid sync accepted: %v", err)
	}
	older := *valid
	older.SyncedAt = now.Add(-time.Hour)
	older.Options = []kafudadomain.CatalogOption{}
	if err := store.SaveCatalog(ctx, &older); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetCatalog(ctx, "primary")
	if err != nil || len(got.Options) != 2 || !got.Available("plus", "PH", now) || got.Available("plus", "US", now) || got.Available("plus", "PH", valid.ExpiresAt) {
		t.Fatalf("invalid catalog merge or availability: %+v, %v", got, err)
	}
	// A confirmed empty catalog is different from a failed/invalid request.
	empty := &kafudadomain.Catalog{Profile: "primary", SyncedAt: now.Add(time.Minute), ExpiresAt: now.Add(time.Hour), Options: []kafudadomain.CatalogOption{}}
	if err := store.SaveCatalog(ctx, empty); err != nil {
		t.Fatal(err)
	}
	got, _ = store.GetCatalog(ctx, "primary")
	if len(got.Options) != 0 {
		t.Fatal("confirmed removal did not replace catalog")
	}
}
