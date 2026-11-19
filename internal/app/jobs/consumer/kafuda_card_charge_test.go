package consumer

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/dujiao-next/internal/constants"
	fulfillmentdomain "github.com/dujiao-next/internal/modules/fulfillment/domain"
	userdomain "github.com/dujiao-next/internal/modules/identity/user/domain"
	kafudaapp "github.com/dujiao-next/internal/modules/kafudagpt/application"
	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
	"github.com/dujiao-next/internal/queue"
	kafudaworkflow "github.com/dujiao-next/internal/workflows/kafudagpt/application"
	"github.com/hibiken/asynq"
)

func TestKafudaRecoveryTaskCollectsLateChargeWithoutRepeatingDelivery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const parentID, childID = 101, 1
		gateway := &kafudaPollTestGateway{}
		c, store, _, db := newKafudaPollBatch(t, gateway, childID)
		if err := db.AutoMigrate(&userdomain.User{}); err != nil {
			t.Fatal(err)
		}
		parent, err := c.OrderStore.GetByID(parentID)
		if err != nil || parent == nil {
			t.Fatalf("read checkout parent: %+v, %v", parent, err)
		}
		buyer := userdomain.User{ID: parent.UserID, Email: "late-charge@example.test", PasswordHash: "test"}
		if err := db.Create(&buyer).Error; err != nil {
			t.Fatal(err)
		}
		emailQueue := &kafudaStatusEmailQueue{t: t, orders: c.OrderStore, orderID: parentID}
		c.orderStatusQueue = emailQueue

		ctx := t.Context()
		due, err := store.DueKnownOrderPolls(ctx, time.Now().UTC(), 1)
		if err != nil || len(due) != 1 || due[0].UpstreamOrderID == nil {
			t.Fatalf("expected a known order due for its completion check: %+v, %v", due, err)
		}
		attemptID := due[0].ID
		gateway.result = kafudacontract.OrderResult{
			ID: *due[0].UpstreamOrderID, Reference: due[0].Reference,
			Product: "gpt", Plan: "plus", Country: "PH", Status: "completed",
		}
		mux := asynq.NewServeMux()
		c.Register(mux)
		runScan := func(wantLookups int) {
			t.Helper()
			if err := mux.ProcessTask(ctx, queue.NewKafudaRecoverChecksTask()); err != nil {
				t.Fatal(err)
			}
			if len(gateway.ids) != wantLookups {
				t.Fatalf("expected %d supplier lookups, got %v", wantLookups, gateway.ids)
			}
		}
		runScan(1)
		assertDirectPollOrderStatus(t, c.OrderStore, parentID, childID, constants.OrderStatusCompleted)
		beforeOrder, err := c.OrderStore.GetByID(parentID)
		if err != nil || beforeOrder == nil {
			t.Fatalf("read completed order: %+v, %v", beforeOrder, err)
		}
		before, err := store.GetRecharge(ctx, childID)
		if err != nil || before == nil || before.CompletedAt == nil || before.CompletionEmailPending || emailQueue.calls != 1 {
			t.Fatalf("expected one completed order notification: %+v, calls=%d, %v", before, emailQueue.calls, err)
		}
		beforeAttempt, err := store.GetAttempt(ctx, attemptID)
		if err != nil || beforeAttempt == nil || beforeAttempt.Status != kafudadomain.AttemptCompleted || beforeAttempt.ChargedUSDCents != nil {
			t.Fatalf("expected completion with unknown card amount: %+v, %v", beforeAttempt, err)
		}
		var beforeDelivery []fulfillmentdomain.Fulfillment
		if err := db.Where("order_id = ?", childID).Find(&beforeDelivery).Error; err != nil || len(beforeDelivery) != 1 {
			t.Fatalf("expected one committed fulfillment: %+v, %v", beforeDelivery, err)
		}

		time.Sleep(59 * time.Second)
		runScan(1)
		time.Sleep(time.Second)
		runScan(2)
		// The supplier still has no amount. A later over-cap charge remains an
		// accounting fact and must not reverse the completed delivery.
		charge := before.Snapshot.MaxCostCents + 1
		gateway.result.ChargedUSDCents = &charge
		time.Sleep(30*time.Minute - time.Second)
		runScan(2)
		time.Sleep(time.Second)
		runScan(3)
		time.Sleep(time.Hour)
		runScan(3)
		afterAttempt, err := store.GetAttempt(ctx, attemptID)
		if err != nil || afterAttempt == nil || afterAttempt.ChargedUSDCents == nil || *afterAttempt.ChargedUSDCents != charge {
			t.Fatalf("expected persisted late charge %d: %+v, %v", charge, afterAttempt, err)
		}
		afterAttempt.ChargedUSDCents = nil
		if !reflect.DeepEqual(afterAttempt, beforeAttempt) {
			t.Fatalf("late charge changed other attempt facts: before=%+v, after=%+v", beforeAttempt, afterAttempt)
		}
		after, err := store.GetRecharge(ctx, childID)
		if err != nil || !reflect.DeepEqual(before, after) || emailQueue.calls != 1 {
			t.Fatalf("late charge affected the completed recharge or email: before=%+v, after=%+v, calls=%d, %v", before, after, emailQueue.calls, err)
		}
		afterOrder, err := c.OrderStore.GetByID(parentID)
		if err != nil || !reflect.DeepEqual(beforeOrder, afterOrder) {
			t.Fatalf("late charge affected the parent or child order: before=%+v, after=%+v, %v", beforeOrder, afterOrder, err)
		}
		var afterDelivery []fulfillmentdomain.Fulfillment
		if err := db.Where("order_id = ?", childID).Find(&afterDelivery).Error; err != nil || !reflect.DeepEqual(beforeDelivery, afterDelivery) {
			t.Fatalf("late charge affected fulfillment: before=%+v, after=%+v, %v", beforeDelivery, afterDelivery, err)
		}
	})
}

type kafudaCardChargeBatchGateway struct {
	kafudacontract.Gateway
	results     map[uint64]kafudacontract.OrderResult
	ids         []uint64
	rateLimited bool
}

func (g *kafudaCardChargeBatchGateway) GetOrder(_ context.Context, id uint64) (*kafudacontract.OrderResult, error) {
	g.ids = append(g.ids, id)
	if g.rateLimited {
		return nil, &kafudacontract.APIError{HTTPStatus: http.StatusTooManyRequests, RetryAfter: 5 * time.Minute}
	}
	result := g.results[id]
	return &result, nil
}

func TestKafudaRecoveryTaskPrioritizesActiveOrdersWithinLookupLimits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const activeCount = 9
		orderIDs := []uint{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13}
		gateway := &kafudaCardChargeBatchGateway{results: make(map[uint64]kafudacontract.OrderResult), rateLimited: true}
		c, store, _, _ := newKafudaPollBatch(t, gateway, orderIDs...)
		ctx := t.Context()
		now := time.Now().UTC()
		attempts, err := store.DueKnownOrderPolls(ctx, now, len(orderIDs))
		if err != nil || len(attempts) != len(orderIDs) {
			t.Fatalf("read due batch: %d attempts, %v", len(attempts), err)
		}
		var charge int64
		for _, attempt := range attempts {
			if attempt.UpstreamOrderID == nil {
				t.Fatalf("missing upstream ID: %+v", attempt)
			}
			result := kafudacontract.OrderResult{
				ID: *attempt.UpstreamOrderID, Reference: attempt.Reference,
				Product: "gpt", Plan: "plus", Country: "PH", Status: "processing",
			}
			if attempt.OrderID > activeCount {
				result.Status = "completed"
				receivedAt := now.Add(-2 * time.Minute)
				completed, err := kafudaworkflow.ApplyResult(c.OrderStore, attempt.OrderID, attempt.ID, result, receivedAt, receivedAt, 10*time.Second)
				if err != nil || !completed {
					t.Fatalf("seed completed order %d: %v", attempt.OrderID, err)
				}
				result.ChargedUSDCents = &charge
			}
			gateway.results[result.ID] = result
		}

		mux := asynq.NewServeMux()
		c.Register(mux)
		task := queue.NewKafudaRecoverChecksTask()
		if err := mux.ProcessTask(ctx, task); !errors.Is(err, kafudaapp.ErrKnownOrderRateLimited) || !slices.Equal(gateway.ids, []uint64{1}) {
			t.Fatalf("active rate limit must stop every remaining lookup: %v, %v", gateway.ids, err)
		}
		time.Sleep(5*time.Minute + time.Second)
		gateway.rateLimited = false
		for _, want := range [][]uint64{{2, 3, 4, 5, 6, 7, 8, 9, 1, 10}, {11, 12}, {13}, {}} {
			gateway.ids = nil
			if err := mux.ProcessTask(ctx, task); err != nil || !slices.Equal(gateway.ids, want) {
				t.Fatalf("expected lookups %v, got %v, %v", want, gateway.ids, err)
			}
		}
		for _, initial := range attempts {
			if initial.OrderID <= activeCount {
				continue
			}
			got, err := store.GetAttempt(ctx, initial.ID)
			if err != nil || got == nil || got.ChargedUSDCents == nil || *got.ChargedUSDCents != 0 {
				t.Fatalf("zero charge must be recorded for order %d: %+v, %v", initial.OrderID, got, err)
			}
		}
	})
}
