package consumer

import (
	"context"
	"testing"
	"time"

	"github.com/dujiao-next/internal/app/container"
	kafudawiring "github.com/dujiao-next/internal/bootstrap/kafudagpt"
	kafudaapp "github.com/dujiao-next/internal/modules/kafudagpt/application"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
	"github.com/dujiao-next/internal/modules/kafudagpt/infrastructure/gormstore"
	"github.com/dujiao-next/internal/queue"
	"github.com/dujiao-next/internal/shared/money"
	"github.com/glebarez/sqlite"
	"github.com/hibiken/asynq"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

func TestKafudaRecoveryTaskResumesInterruptedCheck(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = pool.Close() })
	if err := db.AutoMigrate(&kafudadomain.Recharge{}, &kafudadomain.Attempt{}); err != nil {
		t.Fatal(err)
	}
	store := gormstore.New(db)
	records := kafudaapp.New(store)
	ctx, past := context.Background(), time.Now().UTC().Add(-time.Hour)
	snapshot := kafudadomain.Snapshot{
		ProductID: 1, SKUID: 1, Plan: "plus", Product: "gpt", Country: "PH",
		PayMode: "auto_card", ProductCode: "test-card", PaymentProfile: "default",
		Currency: "CNY", SellPrice: money.FromDecimal(decimal.NewFromInt(1)), MaxCostCents: 2000,
	}
	if _, err := records.Prepare(ctx, 1, snapshot, past); err != nil {
		t.Fatal(err)
	}
	if _, err := records.Reserve(ctx, 1, past, past.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	c := New(&container.Container{KafudaGPT: &kafudawiring.Module{Records: records}})
	mux := asynq.NewServeMux()
	c.Register(mux)
	task := queue.NewKafudaRecoverChecksTask()
	if len(task.Payload()) != 0 {
		t.Fatal("recovery scan must carry no credentials")
	}
	if err := mux.ProcessTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	record, err := store.GetRecharge(ctx, 1)
	if err != nil || record.Stage != kafudadomain.StageWaiting {
		t.Fatalf("registered task did not recover check: %+v, %v", record, err)
	}
}
