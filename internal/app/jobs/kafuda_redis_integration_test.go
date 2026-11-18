//go:build integration

package jobs

import (
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/dujiao-next/internal/app/container"
	jobconsumer "github.com/dujiao-next/internal/app/jobs/consumer"
	kafudawiring "github.com/dujiao-next/internal/bootstrap/kafudagpt"
	"github.com/dujiao-next/internal/config"
	kafudaapp "github.com/dujiao-next/internal/modules/kafudagpt/application"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
	kafudastore "github.com/dujiao-next/internal/modules/kafudagpt/infrastructure/gormstore"
	"github.com/dujiao-next/internal/queue"
	"github.com/dujiao-next/internal/shared/money"
	"github.com/glebarez/sqlite"
	"github.com/hibiken/asynq"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

func TestRedisKafudaScheduledRecoveryAfterWorkerRestart(t *testing.T) {
	rawPort := os.Getenv("TEST_KAFUDA_REDIS_PORT")
	if rawPort == "" {
		t.Skip("TEST_KAFUDA_REDIS_PORT is empty")
	}
	port, err := strconv.ParseUint(rawPort, 10, 16)
	if err != nil || port == 0 {
		t.Fatal("invalid test Redis port")
	}
	cfg := &config.QueueConfig{Enabled: true, Host: "127.0.0.1", Port: int(port), DB: 15, Concurrency: 1}
	opt, _ := queue.BuildServerConfig(cfg)
	inspector := asynq.NewInspector(opt)
	t.Cleanup(func() { _ = inspector.Close() })
	queues, err := inspector.Queues()
	if err != nil || len(queues) != 0 {
		t.Fatalf("an isolated, empty Redis test database is required: queues=%d, error=%v", len(queues), err)
	}

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
	store := kafudastore.New(db)
	records := kafudaapp.New(store)
	ctx := t.Context()
	past := time.Now().UTC().Add(-time.Hour)
	snapshot := kafudadomain.Snapshot{
		ProductID: 1, SKUID: 1, Plan: "plus", Product: "gpt", Country: "PH",
		PayMode: "auto_card", ProductCode: "test-card", PaymentProfile: "default",
		Currency: "CNY", SellPrice: money.FromDecimal(decimal.NewFromInt(1)), MaxCostCents: 2000,
	}
	if _, err := records.Prepare(ctx, 1, snapshot, past); err != nil {
		t.Fatal(err)
	}
	reserved, err := records.Reserve(ctx, 1, past, past.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	consumer := jobconsumer.New(&container.Container{
		Config:    &config.Config{KafudaGPT: config.KafudaGPTConfig{PollIntervalSeconds: 5}},
		KafudaGPT: &kafudawiring.Module{Records: records, Store: store},
	})
	beforeRestart, err := NewService(cfg, consumer)
	if err != nil {
		t.Fatal(err)
	}
	if err := beforeRestart.server.Start(beforeRestart.mux); err != nil {
		t.Fatal(err)
	}
	beforeRestart.server.Shutdown()

	if err := beforeRestart.scheduler.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(beforeRestart.scheduler.Shutdown)
	awaitKafudaRedis(t, 20*time.Second, func() bool {
		tasks, err := inspector.ListPendingTasks(queue.DefaultQueue)
		if err != nil {
			return false
		}
		for _, task := range tasks {
			if task.Type == queue.TaskKafudaRecoverChecks {
				if len(task.Payload) != 0 {
					t.Fatal("scheduled recovery task carries a payload")
				}
				return true
			}
		}
		return false
	})
	afterRestart, err := NewService(cfg, consumer)
	if err != nil {
		t.Fatal(err)
	}
	if err := afterRestart.server.Start(afterRestart.mux); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(afterRestart.server.Shutdown)
	awaitKafudaRedis(t, 15*time.Second, func() bool {
		recharge, err := store.GetRecharge(ctx, 1)
		if err != nil {
			t.Fatal(err)
		}
		return recharge != nil && recharge.Stage == kafudadomain.StageWaiting
	})
	attempt, err := store.GetAttempt(ctx, reserved.Attempt.ID)
	if err != nil || attempt == nil || attempt.Status != kafudadomain.AttemptFailed || attempt.ActiveOrderID != nil {
		t.Fatalf("restarted worker did not release expired checking attempt: %+v, %v", attempt, err)
	}
}

func awaitKafudaRedis(t *testing.T, timeout time.Duration, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatal("timed out waiting for Redis recovery")
}
