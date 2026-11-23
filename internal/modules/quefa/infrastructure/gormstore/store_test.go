package gormstore

import (
	"context"
	"testing"
	"time"

	quefacontract "github.com/dujiao-next/internal/modules/quefa/contract"
	quefadomain "github.com/dujiao-next/internal/modules/quefa/domain"
	"github.com/dujiao-next/internal/shared/money"
	"github.com/glebarez/sqlite"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

func openTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("sql db: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	if err := db.AutoMigrate(
		&quefadomain.Setting{},
		&quefadomain.Catalog{},
		&quefadomain.Order{},
		&quefadomain.WebhookEvent{},
	); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	return db
}

func TestStoreOrderLifecycleAndOptimisticLock(t *testing.T) {
	db := openTestDB(t)
	store := New(db)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	err := store.CreateCheckout(quefacontract.CheckoutInput{
		OrderID:            101,
		ParentOrderID:      100,
		ProductID:          10,
		SKUID:              20,
		ProductCode:        "chatgpt_plus_1m",
		CollectionMode:     quefadomain.CollectionModePlatformCollect,
		DeliveryMode:       quefadomain.DeliveryModeAutoRecharge,
		CostFloorAmount:    money.FromDecimal(decimal.NewFromInt(100)),
		SettlementCurrency: "CNY",
		MerchantOrderNo:    "QF-100",
		CreatedAt:          now,
	})
	if err != nil {
		t.Fatalf("CreateCheckout failed: %v", err)
	}

	got, err := store.GetOrderByOrderID(ctx, 101)
	if err != nil || got == nil {
		t.Fatalf("GetOrderByOrderID failed: %v", err)
	}
	if got.Stage != quefadomain.StageWaitingPayment || got.Version != 1 {
		t.Fatalf("unexpected initial order: %+v", got)
	}

	staleCopy := *got
	upstreamID := "qf_up_1"
	got.UpstreamOrderID = &upstreamID
	got.Stage = quefadomain.StageWaitingSubmit
	got.NextPollAt = &now
	got.UpdatedAt = now.Add(time.Second)

	if err := store.WithinTransaction(ctx, func(tx quefacontract.Transaction) error {
		return tx.SaveOrder(got)
	}); err != nil {
		t.Fatalf("SaveOrder failed: %v", err)
	}
	if got.Version != 2 {
		t.Fatalf("expected version 2, got %d", got.Version)
	}

	// Optimistic lock must reject stale copy.
	if err := store.SaveOrder(&staleCopy); err != quefadomain.ErrConflict {
		t.Fatalf("expected ErrConflict for stale copy, got %v", err)
	}

	due, err := store.ListDuePollOrderIDs(ctx, now.Add(time.Minute), 10)
	if err != nil || len(due) != 1 || due[0] != 101 {
		t.Fatalf("expected due order 101, got %v err=%v", due, err)
	}
}

func TestStoreWebhookEventIdempotency(t *testing.T) {
	db := openTestDB(t)
	store := New(db)
	now := time.Now().UTC()

	inserted, err := store.InsertWebhookEventIfAbsent(&quefadomain.WebhookEvent{
		EventID:         "evt_abc",
		EventType:       "order.paid",
		UpstreamOrderID: "qf_1",
		Status:          quefadomain.WebhookStatusReceived,
		ReceivedAt:      now,
	})
	if err != nil || !inserted {
		t.Fatalf("expected first insert true, got inserted=%v err=%v", inserted, err)
	}

	insertedAgain, err := store.InsertWebhookEventIfAbsent(&quefadomain.WebhookEvent{
		EventID:         "evt_abc",
		EventType:       "order.paid",
		UpstreamOrderID: "qf_1",
		Status:          quefadomain.WebhookStatusReceived,
		ReceivedAt:      now,
	})
	if err != nil || insertedAgain {
		t.Fatalf("expected second insert false, got inserted=%v err=%v", insertedAgain, err)
	}
}
