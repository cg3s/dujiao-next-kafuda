//go:build integration

package jobs

import (
	"bytes"
	"io"
	"net"
	"net/mail"
	"net/textproto"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dujiao-next/internal/app/container"
	jobconsumer "github.com/dujiao-next/internal/app/jobs/consumer"
	kafudawiring "github.com/dujiao-next/internal/bootstrap/kafudagpt"
	"github.com/dujiao-next/internal/config"
	"github.com/dujiao-next/internal/constants"
	fulfillmentdomain "github.com/dujiao-next/internal/modules/fulfillment/domain"
	userdomain "github.com/dujiao-next/internal/modules/identity/user/domain"
	userstore "github.com/dujiao-next/internal/modules/identity/user/infrastructure/gormstore"
	kafudaapp "github.com/dujiao-next/internal/modules/kafudagpt/application"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
	kafudastore "github.com/dujiao-next/internal/modules/kafudagpt/infrastructure/gormstore"
	notificationsmtp "github.com/dujiao-next/internal/modules/notification/infrastructure/smtp"
	orderdomain "github.com/dujiao-next/internal/modules/order/domain"
	orderstore "github.com/dujiao-next/internal/modules/order/infrastructure/gormstore"
	orderqueue "github.com/dujiao-next/internal/modules/order/infrastructure/queueadapter"
	"github.com/dujiao-next/internal/queue"
	"github.com/dujiao-next/internal/shared/money"
	"github.com/glebarez/sqlite"
	"github.com/hibiken/asynq"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

func kafudaRedisFixture(t *testing.T, dbIndex int) (*config.QueueConfig, *asynq.Inspector) {
	t.Helper()
	rawPort := os.Getenv("TEST_KAFUDA_REDIS_PORT")
	if rawPort == "" {
		t.Skip("TEST_KAFUDA_REDIS_PORT is empty")
	}
	port, err := strconv.ParseUint(rawPort, 10, 16)
	if err != nil || port == 0 {
		t.Fatal("invalid test Redis port")
	}
	cfg := &config.QueueConfig{Enabled: true, Host: "127.0.0.1", Port: int(port), DB: dbIndex, Concurrency: 1}
	opt, _ := queue.BuildServerConfig(cfg)
	inspector := asynq.NewInspector(opt)
	t.Cleanup(func() { _ = inspector.Close() })
	queues, err := inspector.Queues()
	if err != nil || len(queues) != 0 {
		t.Fatalf("an isolated, empty Redis test database is required: queues=%d, error=%v", len(queues), err)
	}

	return cfg, inspector
}

func TestRedisKafudaScheduledRecoveryAfterWorkerRestart(t *testing.T) {
	cfg, inspector := kafudaRedisFixture(t, 15)

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

func TestRedisKafudaCompletionReplayKeepsOneEmail(t *testing.T) {
	cfg, inspector := kafudaRedisFixture(t, 14)
	client, err := queue.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })

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
	if err := db.AutoMigrate(&kafudadomain.Recharge{}, &kafudadomain.Attempt{}, &orderdomain.Order{}, &orderdomain.OrderItem{}, &fulfillmentdomain.Fulfillment{}, &userdomain.User{}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	parent := orderdomain.Order{ID: 10, OrderNo: "completed-parent", UserID: 20, Status: constants.OrderStatusCompleted, PaidAt: &now, Currency: "CNY"}
	child := orderdomain.Order{ID: 11, ParentID: &parent.ID, OrderNo: "completed-child", UserID: parent.UserID, Status: parent.Status, PaidAt: &now, Currency: parent.Currency}
	buyer := userdomain.User{ID: parent.UserID, Email: "completion@example.test", PasswordHash: "fixture", Locale: "en-US"}
	recharge := kafudadomain.Recharge{OrderID: child.ID, Stage: kafudadomain.StageCompleted, CompletedAt: &now, CompletionEmailPending: true}
	for _, row := range []any{&buyer, &parent, &child, &recharge} {
		if err := db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}

	// Redis accepted the email before the previous process acknowledged it.
	if err := orderqueue.NewDirectCompletion(client).EnqueueStatusEmail(parent.ID, parent.Status); err != nil {
		t.Fatal(err)
	}
	store := kafudastore.New(db)
	port, received := startCompletionSMTPReceiver(t)
	sender := notificationsmtp.New(&config.EmailConfig{
		Enabled: true, Host: "127.0.0.1", Port: port, From: "shop@example.test",
	})
	worker := jobconsumer.New(&container.Container{
		KafudaGPT:   &kafudawiring.Module{Store: store, Records: kafudaapp.New(store)},
		OrderStore:  orderstore.New(db, "test-guest-credential-secret-with-32-bytes"),
		QueueClient: client,
		UserStore:   userstore.New(db),
		EmailSender: sender,
	})
	mux := asynq.NewServeMux()
	worker.Register(mux)
	for range 2 {
		if err := mux.ProcessTask(t.Context(), queue.NewKafudaRecoverChecksTask()); err != nil {
			t.Fatal(err)
		}
	}
	tasks, err := inspector.ListPendingTasks(queue.DefaultQueue)
	if err != nil || len(tasks) != 1 || tasks[0].Type != queue.TaskOrderStatusEmail || tasks[0].ID != "kafuda-completed:10" {
		t.Fatalf("expected exactly one parent-order email task: %v, %v", tasks, err)
	}
	pending, err := store.PendingCompletionEmails(t.Context(), 100)
	if err != nil || len(pending) != 0 {
		t.Fatalf("replayed enqueue failed to clear the pending marker: %v, %v", pending, err)
	}

	opt, serverConfig := queue.BuildServerConfig(cfg)
	server := asynq.NewServer(opt, serverConfig)
	if err := server.Start(mux); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Shutdown)
	select {
	case receipt := <-received:
		if receipt.err != nil {
			t.Fatal(receipt.err)
		}
		message, err := mail.ReadMessage(bytes.NewReader(receipt.message))
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(message.Body)
		if err != nil || !strings.Contains(string(body), parent.OrderNo) || strings.Contains(string(body), child.OrderNo) {
			t.Fatalf("unexpected order in delivery email: %q, %v", body, err)
		}
		if receipt.recipient != "RCPT TO:<"+buyer.Email+">" || !strings.Contains(message.Header.Get("Content-Type"), "text/plain") {
			t.Fatalf("unexpected delivery envelope or content type: %q, %q", receipt.recipient, message.Header.Get("Content-Type"))
		}
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for local SMTP delivery")
	}
	awaitKafudaRedis(t, 10*time.Second, func() bool {
		completed, err := inspector.ListCompletedTasks(queue.DefaultQueue)
		return err == nil && len(completed) == 1 && completed[0].ID == "kafuda-completed:10"
	})
}

type completionSMTPReceipt struct {
	recipient string
	message   []byte
	err       error
}

func startCompletionSMTPReceiver(t *testing.T) (int, <-chan completionSMTPReceipt) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	received := make(chan completionSMTPReceipt, 1)
	go func() {
		received <- receiveCompletionSMTP(listener)
	}()
	return listener.Addr().(*net.TCPAddr).Port, received
}

func receiveCompletionSMTP(listener net.Listener) (receipt completionSMTPReceipt) {
	conn, err := listener.Accept()
	if err != nil {
		receipt.err = err
		return
	}
	client := textproto.NewConn(conn)
	defer client.Close()
	if receipt.err = conn.SetDeadline(time.Now().Add(10 * time.Second)); receipt.err != nil {
		return
	}
	if receipt.err = client.PrintfLine("220 localhost test SMTP"); receipt.err != nil {
		return
	}
	for {
		line, err := client.ReadLine()
		if err != nil {
			receipt.err = err
			return
		}
		reply := "250 OK"
		switch {
		case strings.HasPrefix(line, "RCPT TO:"):
			receipt.recipient = line
		case line == "DATA":
			if receipt.err = client.PrintfLine("354 End data with a dot"); receipt.err != nil {
				return
			}
			receipt.message, receipt.err = client.ReadDotBytes()
			if receipt.err != nil {
				return
			}
		case line == "QUIT":
			receipt.err = client.PrintfLine("221 bye")
			return
		}
		if receipt.err = client.PrintfLine("%s", reply); receipt.err != nil {
			return
		}
	}
}
