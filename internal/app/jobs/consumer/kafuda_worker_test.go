package consumer

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/dujiao-next/internal/app/container"
	kafudawiring "github.com/dujiao-next/internal/bootstrap/kafudagpt"
	"github.com/dujiao-next/internal/config"
	"github.com/dujiao-next/internal/constants"
	fulfillmentdomain "github.com/dujiao-next/internal/modules/fulfillment/domain"
	userdomain "github.com/dujiao-next/internal/modules/identity/user/domain"
	kafudaapp "github.com/dujiao-next/internal/modules/kafudagpt/application"
	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
	"github.com/dujiao-next/internal/modules/kafudagpt/infrastructure/gormstore"
	ordercontract "github.com/dujiao-next/internal/modules/order/contract"
	orderdomain "github.com/dujiao-next/internal/modules/order/domain"
	orderstore "github.com/dujiao-next/internal/modules/order/infrastructure/gormstore"
	"github.com/dujiao-next/internal/queue"
	"github.com/dujiao-next/internal/shared/jsonmap"
	"github.com/dujiao-next/internal/shared/money"
	"github.com/glebarez/sqlite"
	"github.com/hibiken/asynq"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

type kafudaPollTestGateway struct {
	kafudacontract.Gateway
	result kafudacontract.OrderResult
	ids    []uint64
	err    error
	delay  time.Duration
}

type kafudaDeadlineTestGateway struct {
	kafudacontract.Gateway
	ids []uint64
}

func (g *kafudaDeadlineTestGateway) GetOrder(ctx context.Context, id uint64) (*kafudacontract.OrderResult, error) {
	g.ids = append(g.ids, id)
	<-ctx.Done()
	return nil, ctx.Err()
}

type kafudaStatusEmailQueue struct {
	ordercontract.Queue
	t       *testing.T
	orders  ordercontract.Store
	orderID uint
	calls   int
	err     error
}

func (q *kafudaStatusEmailQueue) Enabled() bool { return true }

func (q *kafudaStatusEmailQueue) EnqueueStatusEmail(orderID uint, status string) error {
	q.t.Helper()
	q.calls++
	order, err := q.orders.GetByID(orderID)
	if err != nil || order == nil || order.Status != constants.OrderStatusCompleted || orderID != q.orderID || status != constants.OrderStatusCompleted {
		q.t.Fatalf("unexpected completion notification: %d %s, order=%+v, err=%v", orderID, status, order, err)
	}
	return q.err
}

func (g *kafudaPollTestGateway) GetOrder(_ context.Context, id uint64) (*kafudacontract.OrderResult, error) {
	g.ids = append(g.ids, id)
	time.Sleep(g.delay)
	return &g.result, g.err
}

func TestKafudaRetryDelayIsBounded(t *testing.T) {
	now := time.Now()
	for _, pair := range [][2]time.Duration{{-time.Minute, time.Minute}, {time.Minute, 2 * time.Minute}, {4 * time.Minute, 8 * time.Minute}, {8 * time.Minute, 15 * time.Minute}, {time.Hour, 15 * time.Minute}} {
		next := now.Add(pair[0])
		if got := kafudaRetryDelay(kafudadomain.Attempt{UpdatedAt: now, NextPollAt: &next}, time.Minute); got != pair[1] {
			t.Errorf("delay after %s = %s, want %s", pair[0], got, pair[1])
		}
	}
}

func TestKafudaRecoveryTaskRecoversChecksAndCompletesKnownOrder(t *testing.T) {
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
	store := gormstore.New(db)
	orders := orderstore.New(db, "test-guest-credential-secret-with-32-bytes")
	module, err := kafudawiring.New(db, config.KafudaGPTConfig{}, orders)
	if err != nil {
		t.Fatal(err)
	}
	records := module.Records
	ctx, past := context.Background(), time.Now().UTC().Add(-time.Hour)
	snapshot := kafudadomain.Snapshot{
		ProductID: 1, SKUID: 1, Plan: "plus", Product: "gpt", Country: "PH",
		PayMode: "auto_card", ProductCode: "test-card", PaymentProfile: "default",
		Currency: "CNY", SellPrice: money.FromDecimal(decimal.NewFromInt(1)), MaxCostCents: 2000,
	}
	seedPaidDirectPollOrder(t, db, 6, 1, past)
	seedPaidDirectPollOrder(t, db, 7, 2, past)
	if _, err := records.Prepare(ctx, 1, snapshot, past); err != nil {
		t.Fatal(err)
	}
	if _, err := records.Reserve(ctx, 1, past, past.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := records.Prepare(ctx, 2, snapshot, past); err != nil {
		t.Fatal(err)
	}
	unknown, err := records.Reserve(ctx, 2, past, past.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := records.MarkSubmitting(ctx, unknown.Attempt.ID, "unknown_quote", past, past.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	parent, child := seedPaidDirectPollOrder(t, db, 10, 11, past)
	buyer := userdomain.User{ID: parent.UserID, Email: "completion@example.test", PasswordHash: "test"}
	if err := db.Create(&buyer).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := records.Prepare(ctx, child.ID, snapshot, past); err != nil {
		t.Fatal(err)
	}
	reservation, err := records.Reserve(ctx, child.ID, past, past.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	id := reservation.Attempt.ID
	due := past.Add(10 * time.Second)
	if err := records.MarkSubmitting(ctx, id, "quote_1", past, due); err != nil {
		t.Fatal(err)
	}
	result := kafudacontract.OrderResult{ID: 101, Reference: reservation.Attempt.Reference, Product: "gpt", Plan: "plus", Country: "PH", Status: "processing"}
	if err := records.RecordResult(ctx, id, result, past, due); err != nil {
		t.Fatal(err)
	}
	gateway := &kafudaPollTestGateway{result: result, err: context.DeadlineExceeded}
	module.Gateway = gateway
	c := New(&container.Container{
		Config:     &config.Config{KafudaGPT: config.KafudaGPTConfig{PollIntervalSeconds: 10}},
		KafudaGPT:  module,
		OrderStore: orders,
	})
	emailQueue := &kafudaStatusEmailQueue{t: t, orders: c.OrderStore, orderID: parent.ID}
	c.orderStatusQueue = emailQueue
	mux := asynq.NewServeMux()
	c.Register(mux)
	task := queue.NewKafudaRecoverChecksTask()
	if len(task.Payload()) != 0 {
		t.Fatal("recovery scan must carry no credentials")
	}
	for _, delay := range []time.Duration{20 * time.Second, 40 * time.Second, 10 * time.Second} {
		err := mux.ProcessTask(ctx, task)
		if gateway.err != nil && !errors.Is(err, kafudaapp.ErrKnownOrderLookupFailed) || gateway.err == nil && err != nil {
			t.Fatalf("unexpected task result: %v", err)
		}
		stored, err := store.GetAttempt(ctx, id)
		if err != nil || stored == nil || stored.NextPollAt == nil ||
			stored.NextPollAt.Sub(stored.UpdatedAt) != delay || stored.Status != kafudadomain.AttemptProcessing {
			t.Fatalf("unexpected retry schedule, expected %s: %+v, %v", delay, stored, err)
		}
		// Advance the test clock for this attempt without sleeping.
		if err := db.Model(&kafudadomain.Attempt{}).Where("id = ?", id).
			Updates(map[string]any{"updated_at": past, "next_poll_at": past.Add(delay)}).Error; err != nil {
			t.Fatal(err)
		}
		if delay == 40*time.Second {
			gateway.err = nil
		}
	}
	emailQueue.err = errors.New("test queue unavailable")
	gateway.result.Status = "completed"
	if err := mux.ProcessTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	record, err := store.GetRecharge(ctx, 1)
	if err != nil || record.Stage != kafudadomain.StageWaiting {
		t.Fatalf("registered task did not recover check: %+v, %v", record, err)
	}
	record, err = store.GetRecharge(ctx, 2)
	if err != nil || record == nil || record.Stage != kafudadomain.StageManualReview || record.ReasonCode != "outcome_unknown" {
		t.Fatalf("overdue unidentified submission was not flagged: %+v, %v", record, err)
	}
	assertDirectPollOrderStatus(t, orders, 7, 2, constants.OrderStatusFulfilling)
	assertDirectPollOrderStatus(t, orders, 6, 1, constants.OrderStatusPaid)
	replay, err := records.Reserve(ctx, 2, time.Now(), time.Now().Add(time.Minute))
	if err != nil || replay == nil || replay.Created || replay.Attempt == nil || replay.Attempt.ActiveOrderID == nil ||
		replay.Attempt.Reference != unknown.Attempt.Reference || replay.Attempt.QuoteID != "unknown_quote" {
		t.Fatalf("unknown submission lost its original attempt: %+v, %v", replay, err)
	}
	pending, err := store.PendingCompletionEmails(ctx, time.Now().Add(time.Hour), 100)
	if err != nil || len(pending) != 1 || pending[0] != child.ID || emailQueue.calls != 1 {
		t.Fatalf("queue failure lost completion notice: %v, %v, calls=%d", pending, err, emailQueue.calls)
	}
	emailQueue.err = nil
	c.KafudaGPT.Gateway = nil
	if err := mux.ProcessTask(ctx, task); err != nil || emailQueue.calls != 1 {
		t.Fatalf("retried before scheduled time: %v, calls=%d", err, emailQueue.calls)
	}
	if err := db.Model(&kafudadomain.Recharge{}).Where("order_id = ?", child.ID).
		UpdateColumn("completion_email_retry_at", past).Error; err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := mux.ProcessTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	pending, err = store.PendingCompletionEmails(ctx, time.Now().Add(time.Hour), 100)
	if err != nil || len(pending) != 0 {
		t.Fatalf("successful retry did not clear completion notice: %v, %v", pending, err)
	}
	if len(gateway.ids) != 4 || gateway.ids[0] != 101 || gateway.ids[3] != 101 {
		t.Fatalf("unexpected upstream reads after replay: %v", gateway.ids)
	}
	if emailQueue.calls != 2 {
		t.Fatalf("completion enqueue attempts = %d, want one failure and one success", emailQueue.calls)
	}
	for _, orderID := range []uint{parent.ID, child.ID} {
		var stored orderdomain.Order
		if err := db.First(&stored, orderID).Error; err != nil || stored.Status != constants.OrderStatusCompleted {
			t.Fatalf("order %d did not complete: %+v, %v", orderID, stored, err)
		}
	}
}

func TestKafudaPollStopsCurrentBatchWhenUpstreamRateLimits(t *testing.T) {
	gateway := &kafudaPollTestGateway{err: &kafudacontract.APIError{HTTPStatus: 429, RetryAfter: 5 * time.Minute}}
	c, store, due, _ := newKafudaPollBatch(t, gateway, 1, 2)
	ctx := t.Context()
	if err := c.pollKnownKafudaOrders(ctx); !errors.Is(err, kafudaapp.ErrKnownOrderRateLimited) {
		t.Fatalf("missing rate-limit classification: %v", err)
	}
	if len(gateway.ids) != 1 {
		t.Fatalf("supplier requests after rate limit: %v", gateway.ids)
	}
	remaining, err := store.DueKnownOrderPolls(ctx, time.Now().UTC(), 10)
	if err != nil || len(remaining) != 1 || remaining[0].UpstreamOrderID == nil || *remaining[0].UpstreamOrderID == gateway.ids[0] || !remaining[0].NextPollAt.Equal(due) {
		t.Fatalf("rate limit changed an unqueried attempt: %+v, %v", remaining, err)
	}
}

func TestKafudaRecoveryTaskPersistsBackoffAfterLookupDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gateway := &kafudaDeadlineTestGateway{}
		c, store, _, _ := newKafudaPollBatch(t, gateway, 1, 2)
		c.Config = &config.Config{KafudaGPT: config.KafudaGPTConfig{PollReviewAfterSeconds: 60}}
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		started := time.Now().UTC()
		before, err := store.DueKnownOrderPolls(ctx, started, 10)
		if err != nil || len(before) != 2 {
			t.Fatalf("expected two due attempts, got %+v, %v", before, err)
		}

		mux := asynq.NewServeMux()
		c.Register(mux)
		err = mux.ProcessTask(ctx, queue.NewKafudaRecoverChecksTask())
		if !errors.Is(err, kafudaapp.ErrKnownOrderLookupFailed) || !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
			t.Fatalf("expected lookup timeout while the task remains active, got %v (task status %v)", err, ctx.Err())
		}
		failedAt := started.Add(55 * time.Second)
		if !time.Now().Equal(failedAt) || len(gateway.ids) != 1 || gateway.ids[0] != *before[0].UpstreamOrderID {
			t.Fatalf("expected the first lookup to exhaust the batch deadline, got %v at %v", gateway.ids, time.Now())
		}
		stored, err := store.GetAttempt(ctx, before[0].ID)
		wantRetry := failedAt.Add(2 * time.Minute)
		if err != nil || stored == nil || stored.NextPollAt == nil || !stored.NextPollAt.Equal(wantRetry) || !stored.UpdatedAt.Equal(failedAt) {
			t.Fatalf("retry was not persisted from the time of failure: %+v, %v", stored, err)
		}
		if stored.Status != kafudadomain.AttemptReview || stored.ActiveOrderID == nil || *stored.ActiveOrderID != before[0].OrderID ||
			stored.UpstreamOrderID == nil || *stored.UpstreamOrderID != gateway.ids[0] || stored.Reference != before[0].Reference {
			t.Fatalf("lookup failure changed the active attempt's identity: %+v", stored)
		}
		assertDirectPollOrderStatus(t, c.OrderStore, stored.OrderID+100, stored.OrderID, constants.OrderStatusFulfilling)
		remaining, err := store.DueKnownOrderPolls(ctx, failedAt, 10)
		if err != nil || len(remaining) != 1 || remaining[0].ID != before[1].ID || !remaining[0].NextPollAt.Equal(*before[1].NextPollAt) ||
			!remaining[0].UpdatedAt.Equal(before[1].UpdatedAt) {
			t.Fatalf("lookup deadline changed the unqueried attempt: %+v, %v", remaining, err)
		}
	})
}

func TestKafudaRecoveryTaskSchedulesFromSuccessfulLookupResponse(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const interval = 10 * time.Second
		gateway := &kafudaPollTestGateway{delay: 25 * time.Second}
		c, store, _, _ := newKafudaPollBatch(t, gateway, 1)
		c.Config = &config.Config{KafudaGPT: config.KafudaGPTConfig{PollIntervalSeconds: int(interval / time.Second)}}
		ctx := t.Context()
		started := time.Now().UTC()
		due, err := store.DueKnownOrderPolls(ctx, started, 10)
		if err != nil || len(due) != 1 || due[0].UpstreamOrderID == nil {
			t.Fatalf("expected one due attempt with an upstream ID, got %+v, %v", due, err)
		}
		gateway.result = kafudacontract.OrderResult{
			ID: *due[0].UpstreamOrderID, Reference: due[0].Reference, Product: "gpt", Plan: "plus", Country: "PH", Status: "processing",
		}

		mux := asynq.NewServeMux()
		c.Register(mux)
		task := queue.NewKafudaRecoverChecksTask()
		if err := mux.ProcessTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		receivedAt := started.Add(gateway.delay)
		stored, err := store.GetAttempt(ctx, due[0].ID)
		if err != nil || stored == nil || stored.Status != kafudadomain.AttemptProcessing || !stored.UpdatedAt.Equal(receivedAt) ||
			stored.NextPollAt == nil || !stored.NextPollAt.Equal(receivedAt.Add(interval)) {
			t.Fatalf("expected a processing attempt with the next poll one interval after the response, got %+v, %v", stored, err)
		}
		if delay := kafudaRetryDelay(*stored, interval); delay != 2*interval {
			t.Fatalf("slow successful lookup inflated future backoff to %v", delay)
		}
		time.Sleep(interval - time.Second)
		if err := mux.ProcessTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		if len(gateway.ids) != 1 || gateway.ids[0] != gateway.result.ID {
			t.Fatalf("scanner queried before the next scheduled poll: %v", gateway.ids)
		}
	})
}

func TestKafudaRecoveryTaskEscalatesResultApplicationFailure(t *testing.T) {
	for _, scenario := range []struct {
		name          string
		reviewSeconds int
		reviewAfter   time.Duration
	}{
		{name: "configured", reviewSeconds: 3600 + 120, reviewAfter: time.Hour + 2*time.Minute},
		{name: "default", reviewAfter: kafudaapp.DefaultPollReviewAfter},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				const parentID, childID = 101, 1
				gateway := &kafudaPollTestGateway{}
				c, store, _, db := newKafudaPollBatch(t, gateway, childID)
				c.Config = &config.Config{KafudaGPT: config.KafudaGPTConfig{PollReviewAfterSeconds: scenario.reviewSeconds}}
				ctx := t.Context()
				due, err := store.DueKnownOrderPolls(ctx, time.Now().UTC(), 10)
				if err != nil || len(due) != 1 || due[0].UpstreamOrderID == nil {
					t.Fatalf("expected one due attempt with an upstream ID, got %+v, %v", due, err)
				}
				initial := due[0]
				gateway.result = kafudacontract.OrderResult{
					ID: *initial.UpstreamOrderID, Reference: initial.Reference, Product: "gpt", Plan: "plus", Country: "PH", Status: "completed",
				}
				trigger := fmt.Sprintf("CREATE TRIGGER reject_poll_completion BEFORE UPDATE ON orders WHEN OLD.id = %d AND NEW.status = 'completed' BEGIN SELECT RAISE(ABORT, 'local completion failed'); END", parentID)
				if err := db.Exec(trigger).Error; err != nil {
					t.Fatal(err)
				}
				mux := asynq.NewServeMux()
				c.Register(mux)
				task := queue.NewKafudaRecoverChecksTask()

				reviewAt := initial.CreatedAt.Add(scenario.reviewAfter)
				time.Sleep(time.Until(reviewAt.Add(-2 * time.Minute)))
				if err := mux.ProcessTask(ctx, task); err == nil {
					t.Fatal("expected the first completion transaction to fail")
				}
				beforeReview, err := store.GetAttempt(ctx, initial.ID)
				if err != nil || beforeReview == nil || beforeReview.Status != kafudadomain.AttemptProcessing ||
					beforeReview.NextPollAt == nil || !beforeReview.NextPollAt.Equal(reviewAt) {
					t.Fatalf("first failure should retry before escalating: %+v, %v", beforeReview, err)
				}
				assertDirectPollOrderStatus(t, c.OrderStore, parentID, childID, constants.OrderStatusPaid)

				time.Sleep(time.Until(reviewAt))
				if err := mux.ProcessTask(ctx, task); err == nil {
					t.Fatal("expected the completion transaction to still fail")
				}
				record, err := store.GetRecharge(ctx, childID)
				if err != nil || record == nil || record.Stage != kafudadomain.StageManualReview || record.ReasonCode != "result_apply_failed" ||
					record.CompletedAt != nil || record.CompletionEmailPending {
					t.Fatalf("result not escalated to manual review after the deadline: %+v, %v", record, err)
				}
				review, err := store.GetAttempt(ctx, initial.ID)
				if err != nil || review == nil || review.Status != kafudadomain.AttemptReview || review.ActiveOrderID == nil || *review.ActiveOrderID != childID ||
					review.NextPollAt == nil || !review.NextPollAt.Equal(reviewAt.Add(4*time.Minute)) ||
					review.UpstreamOrderID == nil || *review.UpstreamOrderID != *initial.UpstreamOrderID ||
					review.Reference != initial.Reference || kafudacontract.CanResolveManualAttempt(record, review) {
					t.Fatalf("manual review lost the active attempt or permitted manual resolution: %+v, %v", review, err)
				}
				assertDirectPollOrderStatus(t, c.OrderStore, parentID, childID, constants.OrderStatusFulfilling)
				var deliveries int64
				if err := db.Model(&fulfillmentdomain.Fulfillment{}).Where("order_id = ?", childID).Count(&deliveries).Error; err != nil || deliveries != 0 {
					t.Fatalf("failed completion left fulfillment rows: %d, %v", deliveries, err)
				}

				if err := db.Exec("DROP TRIGGER reject_poll_completion").Error; err != nil {
					t.Fatal(err)
				}
				time.Sleep(time.Until(*review.NextPollAt))
				for range 2 {
					if err := mux.ProcessTask(ctx, task); err != nil {
						t.Fatal(err)
					}
				}
				assertDirectPollOrderStatus(t, c.OrderStore, parentID, childID, constants.OrderStatusCompleted)
				if err := db.Model(&fulfillmentdomain.Fulfillment{}).Where("order_id = ?", childID).Count(&deliveries).Error; err != nil || deliveries != 1 || len(gateway.ids) != 3 {
					t.Fatalf("recovery did not fulfill exactly once: rows=%d lookups=%v, %v", deliveries, gateway.ids, err)
				}
			})
		})
	}
}

func newKafudaPollBatch(t *testing.T, gateway kafudacontract.Gateway, orderIDs ...uint) (*Consumer, *gormstore.Store, time.Time, *gorm.DB) {
	t.Helper()
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
	if err := db.AutoMigrate(&kafudadomain.Recharge{}, &kafudadomain.Attempt{}, &orderdomain.Order{}, &orderdomain.OrderItem{}, &fulfillmentdomain.Fulfillment{}); err != nil {
		t.Fatal(err)
	}
	store := gormstore.New(db)
	orders := orderstore.New(db, "test-guest-credential-secret-with-32-bytes")
	module, err := kafudawiring.New(db, config.KafudaGPTConfig{}, orders)
	if err != nil {
		t.Fatal(err)
	}
	records := module.Records
	ctx := t.Context()
	past := time.Now().UTC().Add(-time.Hour)
	due := past.Add(time.Minute)
	snapshot := kafudadomain.Snapshot{
		ProductID: 1, SKUID: 1, Plan: "plus", Product: "gpt", Country: "PH",
		PayMode: "auto_card", ProductCode: "test-card", PaymentProfile: "default",
		Currency: "CNY", SellPrice: money.FromDecimal(decimal.NewFromInt(1)), MaxCostCents: 2000,
	}
	for _, orderID := range orderIDs {
		seedPaidDirectPollOrder(t, db, orderID+100, orderID, past)
		if _, err := records.Prepare(ctx, orderID, snapshot, past); err != nil {
			t.Fatal(err)
		}
		reserved, err := records.Reserve(ctx, orderID, past, due)
		if err != nil {
			t.Fatal(err)
		}
		if err := records.MarkSubmitting(ctx, reserved.Attempt.ID, "quote", past, due); err != nil {
			t.Fatal(err)
		}
		result := kafudacontract.OrderResult{
			ID: uint64(orderID), Reference: reserved.Attempt.Reference, Product: "gpt", Plan: "plus", Country: "PH", Status: "processing",
		}
		if err := records.RecordResult(ctx, reserved.Attempt.ID, result, past, due); err != nil {
			t.Fatal(err)
		}
	}
	module.Gateway = gateway
	c := New(&container.Container{
		KafudaGPT:  module,
		OrderStore: orders,
	})
	return c, store, due, db
}

func seedPaidDirectPollOrder(t *testing.T, db *gorm.DB, parentID, childID uint, paidAt time.Time) (orderdomain.Order, orderdomain.Order) {
	t.Helper()
	parent := orderdomain.Order{ID: parentID, OrderNo: fmt.Sprintf("poll-parent-%d", childID), UserID: 20,
		Status: constants.OrderStatusPaid, PaidAt: &paidAt, Currency: "CNY"}
	child := orderdomain.Order{ID: childID, ParentID: &parent.ID, OrderNo: fmt.Sprintf("poll-child-%d", childID), UserID: parent.UserID,
		Status: parent.Status, PaidAt: &paidAt, Currency: parent.Currency}
	item := orderdomain.OrderItem{OrderID: childID, ProductID: 1, SKUID: 1, Quantity: 1,
		TitleJSON: jsonmap.JSON{"en-US": "Direct recharge"}, FulfillmentType: constants.FulfillmentTypeKafudaGPT}
	for _, row := range []any{&parent, &child, &item} {
		if err := db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	return parent, child
}

func assertDirectPollOrderStatus(t *testing.T, orders ordercontract.Store, parentID, childID uint, want string) {
	t.Helper()
	order, err := orders.GetByID(parentID)
	if err != nil || order == nil || order.Status != want || order.PaidAt == nil || len(order.Children) != 1 ||
		order.Children[0].ID != childID || order.Children[0].Status != want || order.Children[0].PaidAt == nil {
		t.Fatalf("expected both orders to be %s with payment preserved: %+v, %v", want, order, err)
	}
}
