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
	if err := service.RecordResult(ctx, id, result, now.Add(time.Minute), now.Add(2*time.Minute)); err != nil {
		t.Fatalf("duplicate success: %v", err)
	}
	record, _ = store.GetRecharge(ctx, 10)
	if !record.CompletedAt.Equal(firstCompleted) {
		t.Fatal("duplicate success rewrote completion time")
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
