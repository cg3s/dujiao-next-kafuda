//go:build integration

package jobs

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/mail"
	"net/textproto"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
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
	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
	kafudastore "github.com/dujiao-next/internal/modules/kafudagpt/infrastructure/gormstore"
	notificationsmtp "github.com/dujiao-next/internal/modules/notification/infrastructure/smtp"
	ordercontract "github.com/dujiao-next/internal/modules/order/contract"
	orderdomain "github.com/dujiao-next/internal/modules/order/domain"
	orderstore "github.com/dujiao-next/internal/modules/order/infrastructure/gormstore"
	orderqueue "github.com/dujiao-next/internal/modules/order/infrastructure/queueadapter"
	"github.com/dujiao-next/internal/queue"
	"github.com/dujiao-next/internal/shared/jsonmap"
	"github.com/dujiao-next/internal/shared/money"
	kafudaworkflow "github.com/dujiao-next/internal/workflows/kafudagpt/application"
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
	queueClient, err := queue.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = queueClient.Close() })
	if queueClient.KafudaWorkerReady(t.Context()) {
		t.Fatal("queue connection alone must not make Direct purchasable")
	}
	path := filepath.Join(t.TempDir(), "worker.sqlite")
	open := func() (*gorm.DB, func() error) {
		t.Helper()
		db, err := gorm.Open(sqlite.Open(path), &gorm.Config{})
		if err != nil {
			t.Fatal(err)
		}
		pool, err := db.DB()
		if err != nil {
			t.Fatal(err)
		}
		pool.SetMaxOpenConns(1)
		t.Cleanup(func() { _ = pool.Close() })
		return db, pool.Close
	}
	db, closeBeforeRestart := open()
	if err := db.AutoMigrate(&kafudadomain.Recharge{}, &kafudadomain.Attempt{}, &orderdomain.Order{}, &orderdomain.OrderItem{}, &fulfillmentdomain.Fulfillment{}); err != nil {
		t.Fatal(err)
	}
	appConfig := &config.Config{KafudaGPT: config.KafudaGPTConfig{PollIntervalSeconds: 5}}
	newWorker := func(db *gorm.DB, gateway kafudacontract.Gateway) (*Service, *kafudawiring.Module, ordercontract.Store) {
		t.Helper()
		orders := orderstore.New(db, "test-guest-credential-secret-with-32-bytes")
		module, err := kafudawiring.New(db, appConfig.KafudaGPT, orders, nil)
		if err != nil {
			t.Fatal(err)
		}
		module.Gateway = gateway
		worker, err := NewService(cfg, jobconsumer.New(&container.Container{Config: appConfig, KafudaGPT: module, OrderStore: orders}))
		if err != nil {
			t.Fatal(err)
		}
		return worker, module, orders
	}
	gateway := &kafudaRestartGateway{}
	beforeRestart, previous, _ := newWorker(db, gateway)
	ctx := t.Context()
	past := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	due := past.Add(time.Minute)
	amount := money.FromDecimal(decimal.NewFromInt(100))
	snapshot := kafudadomain.Snapshot{
		ProductID: 1, SKUID: 1, Plan: "plus", Product: "gpt", Country: "PH",
		PayMode: "auto_card", ProductCode: "test-card", PaymentProfile: "default",
		Currency: "CNY", SellPrice: amount, MaxCostCents: 2000,
	}
	seedAttempt := func(label string, parentID, childID uint) (orderdomain.Order, *kafudadomain.Attempt) {
		t.Helper()
		parent := orderdomain.Order{ID: parentID, OrderNo: label + "-parent", UserID: 20, PaidAt: &past,
			Status: constants.OrderStatusPaid, Currency: "CNY", TotalAmount: amount}
		child := orderdomain.Order{ID: childID, ParentID: &parent.ID, OrderNo: label + "-child", UserID: parent.UserID, PaidAt: &past,
			Status: parent.Status, Currency: parent.Currency, TotalAmount: amount}
		item := orderdomain.OrderItem{OrderID: child.ID, ProductID: snapshot.ProductID, SKUID: snapshot.SKUID,
			Quantity: 1, TitleJSON: jsonmap.JSON{"zh-CN": "直充恢复演练"},
			UnitPrice: amount, TotalPrice: amount, FulfillmentType: constants.FulfillmentTypeKafudaGPT}
		for _, row := range []any{&parent, &child, &item} {
			if err := db.Create(row).Error; err != nil {
				t.Fatal(err)
			}
		}
		if _, err := previous.Records.Prepare(ctx, childID, snapshot, past); err != nil {
			t.Fatal(err)
		}
		reserved, err := previous.Records.Reserve(ctx, childID, past, due)
		if err != nil {
			t.Fatal(err)
		}
		return parent, reserved.Attempt
	}
	_, checking := seedAttempt("checking", 10, 11)
	unknownParent, submitting := seedAttempt("unknown", 20, 21)
	knownParent, processing := seedAttempt("known", 30, 31)
	const unknownQuote = "quote-without-response"
	if err := previous.Records.MarkSubmitting(ctx, submitting.ID, unknownQuote, past, due); err != nil {
		t.Fatal(err)
	}
	if err := previous.Records.MarkSubmitting(ctx, processing.ID, "quote-with-known-order", past, due); err != nil {
		t.Fatal(err)
	}
	cardCharge := int64(100)
	gateway.result = kafudacontract.OrderResult{ID: 101, Reference: processing.Reference, Product: snapshot.Product,
		Plan: snapshot.Plan, Country: snapshot.Country, Status: "processing", ChargedUSDCents: &cardCharge}
	if err := previous.Records.RecordResult(ctx, processing.ID, gateway.result, past, due); err != nil {
		t.Fatal(err)
	}
	if err := beforeRestart.server.Start(beforeRestart.mux); err != nil {
		t.Fatal(err)
	}
	beforeRestart.server.Shutdown()
	if err := closeBeforeRestart(); err != nil {
		t.Fatal(err)
	}

	// A new worker opens the original database after the first worker stops.
	// Nothing was enqueued for the three attempts when they were written.
	reopened, _ := open()
	gateway.result.Status = "completed"
	afterRestart, current, orders := newWorker(reopened, gateway)
	if err := afterRestart.scheduler.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(afterRestart.scheduler.Shutdown)
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
	var runs atomic.Int32
	afterRestart.mux.Use(func(next asynq.Handler) asynq.Handler {
		return asynq.HandlerFunc(func(ctx context.Context, task *asynq.Task) error {
			err := next.ProcessTask(ctx, task)
			if task.Type() == queue.TaskKafudaRecoverChecks && err == nil {
				runs.Add(1)
			}
			return err
		})
	})
	if err := afterRestart.server.Start(afterRestart.mux); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(afterRestart.server.Shutdown)
	awaitKafudaRedis(t, 15*time.Second, func() bool { return runs.Load() >= 2 })
	awaitKafudaRedis(t, 15*time.Second, func() bool { return queueClient.KafudaWorkerReady(ctx) })
	assertRecovered := func(original *kafudadomain.Attempt, stage kafudadomain.Stage, status kafudadomain.AttemptStatus) (*kafudadomain.Recharge, *kafudadomain.Attempt) {
		t.Helper()
		recharge, err := current.Store.GetRecharge(ctx, original.OrderID)
		if err != nil {
			t.Fatal(err)
		}
		attempt, err := current.Store.GetAttempt(ctx, original.ID)
		if err != nil || recharge == nil || recharge.Stage != stage || recharge.AttemptCount != 1 ||
			attempt == nil || attempt.Status != status || attempt.Reference != original.Reference {
			t.Fatalf("expected recovered stage %s and original attempt in %s: recharge=%+v, attempt=%+v, error=%v", stage, status, recharge, attempt, err)
		}
		return recharge, attempt
	}
	waiting, released := assertRecovered(checking, kafudadomain.StageWaiting, kafudadomain.AttemptFailed)
	if waiting.ReasonCode != "check_interrupted" || released.ActiveOrderID != nil {
		t.Fatal("restarted worker did not release the expired check")
	}
	review, unknown := assertRecovered(submitting, kafudadomain.StageManualReview, kafudadomain.AttemptReview)
	if review.ReasonCode != "outcome_unknown" || unknown.ActiveOrderID == nil || *unknown.ActiveOrderID != submitting.OrderID ||
		unknown.QuoteID != unknownQuote || unknown.UpstreamOrderID != nil {
		t.Fatal("restarted worker did not preserve the unidentified submission for review")
	}
	now := time.Now().UTC()
	replay, err := kafudaworkflow.ReserveBuyerAttempt(orders, ordercontract.TenantScope{}, unknownParent.OrderNo, unknownParent.UserID, now, now.Add(time.Minute))
	if err != nil || replay == nil || replay.Created || replay.Attempt == nil || replay.Attempt.ID != unknown.ID {
		t.Fatalf("replayed checkout did not reuse the original submission: %+v, %v", replay, err)
	}
	_, known := assertRecovered(processing, kafudadomain.StageCompleted, kafudadomain.AttemptCompleted)
	if known.UpstreamOrderID == nil || *known.UpstreamOrderID != gateway.result.ID || known.ActiveOrderID != nil ||
		gateway.lookups.Load() != 1 || gateway.submissions.Load() != 0 {
		t.Fatalf("recovery must look up the original numeric order once and never submit: lookups=%d, submits=%d", gateway.lookups.Load(), gateway.submissions.Load())
	}
	for _, target := range []struct {
		parent       orderdomain.Order
		childID      uint
		wantedStatus string
	}{
		{unknownParent, submitting.OrderID, constants.OrderStatusFulfilling},
		{knownParent, processing.OrderID, constants.OrderStatusCompleted},
	} {
		parent, err := orders.GetByID(target.parent.ID)
		if err != nil || parent == nil || len(parent.Children) != 1 || parent.Children[0].ID != target.childID ||
			parent.Status != target.wantedStatus || parent.Children[0].Status != target.wantedStatus {
			t.Fatalf("expected recovered parent and child in %s: %+v, %v", target.wantedStatus, parent, err)
		}
		for _, order := range []orderdomain.Order{*parent, parent.Children[0]} {
			if order.PaidAt == nil || !order.PaidAt.Equal(past) || !order.TotalAmount.Equal(amount.Decimal) || !order.RefundedAmount.IsZero() {
				t.Fatal("recovery changed the original payment")
			}
		}
	}
	var deliveries []fulfillmentdomain.Fulfillment
	if err := reopened.Find(&deliveries).Error; err != nil || len(deliveries) != 1 || deliveries[0].OrderID != processing.OrderID ||
		deliveries[0].Type != constants.FulfillmentTypeKafudaGPT || deliveries[0].Status != constants.FulfillmentStatusDelivered ||
		deliveries[0].Payload != "" || deliveries[0].DeliveredAt == nil {
		t.Fatalf("expected exactly one durable supplier fulfillment: %+v, %v", deliveries, err)
	}
}

type kafudaRestartGateway struct {
	kafudacontract.Gateway
	result      kafudacontract.OrderResult
	lookups     atomic.Int32
	submissions atomic.Int32
}

func (g *kafudaRestartGateway) CreateOrder(context.Context, kafudacontract.CreateOrderRequest) (*kafudacontract.OrderResult, error) {
	g.submissions.Add(1)
	return nil, fmt.Errorf("unexpected supplier order creation during recovery")
}

func (g *kafudaRestartGateway) GetOrder(_ context.Context, id uint64) (*kafudacontract.OrderResult, error) {
	g.lookups.Add(1)
	if id != g.result.ID {
		return nil, fmt.Errorf("unexpected supplier order ID: %d", id)
	}
	return &g.result, nil
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
	pending, err := store.PendingCompletionEmails(t.Context(), time.Now().Add(time.Hour), 100)
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
