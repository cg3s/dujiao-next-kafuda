package consumer

import (
	"context"
	"testing"
	"time"

	"github.com/dujiao-next/internal/app/container"
	kafudawiring "github.com/dujiao-next/internal/bootstrap/kafudagpt"
	"github.com/dujiao-next/internal/constants"
	fulfillmentdomain "github.com/dujiao-next/internal/modules/fulfillment/domain"
	kafudaapp "github.com/dujiao-next/internal/modules/kafudagpt/application"
	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
	"github.com/dujiao-next/internal/modules/kafudagpt/infrastructure/gormstore"
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
}

func (g *kafudaPollTestGateway) GetOrder(_ context.Context, id uint64) (*kafudacontract.OrderResult, error) {
	g.ids = append(g.ids, id)
	return &g.result, nil
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
	if err := db.AutoMigrate(&kafudadomain.Recharge{}, &kafudadomain.Attempt{}, &orderdomain.Order{}, &orderdomain.OrderItem{}, &fulfillmentdomain.Fulfillment{}); err != nil {
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
	parent := orderdomain.Order{ID: 10, OrderNo: "poll-parent", UserID: 20, Status: constants.OrderStatusPaid, PaidAt: &past, Currency: "CNY"}
	child := orderdomain.Order{ID: 11, ParentID: &parent.ID, OrderNo: "poll-child", UserID: parent.UserID, Status: parent.Status, PaidAt: &past, Currency: parent.Currency}
	item := orderdomain.OrderItem{OrderID: child.ID, ProductID: 1, SKUID: 1, Quantity: 1, TitleJSON: jsonmap.JSON{"en-US": "Direct recharge"}, FulfillmentType: constants.FulfillmentTypeKafudaGPT}
	for _, row := range []any{&parent, &child, &item} {
		if err := db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	if _, err := records.Prepare(ctx, child.ID, snapshot, past); err != nil {
		t.Fatal(err)
	}
	reservation, err := records.Reserve(ctx, child.ID, past, past.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	id := reservation.Attempt.ID
	due := past.Add(time.Minute)
	if err := records.MarkSubmitting(ctx, id, "quote_1", past, due); err != nil {
		t.Fatal(err)
	}
	result := kafudacontract.OrderResult{ID: 101, Reference: reservation.Attempt.Reference, Product: "gpt", Plan: "plus", Country: "PH", Status: "processing"}
	if err := records.RecordResult(ctx, id, result, past, due); err != nil {
		t.Fatal(err)
	}
	result.Status = "completed"
	gateway := &kafudaPollTestGateway{result: result}
	c := New(&container.Container{
		KafudaGPT:  &kafudawiring.Module{Records: records, Store: store, Gateway: gateway},
		OrderStore: orderstore.New(db, "test-guest-credential-secret-with-32-bytes"),
	})
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
	if err := mux.ProcessTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if len(gateway.ids) != 1 || gateway.ids[0] != 101 {
		t.Fatalf("unexpected upstream reads after replay: %v", gateway.ids)
	}
	for _, orderID := range []uint{parent.ID, child.ID} {
		var stored orderdomain.Order
		if err := db.First(&stored, orderID).Error; err != nil || stored.Status != constants.OrderStatusCompleted {
			t.Fatalf("order %d did not complete: %+v, %v", orderID, stored, err)
		}
	}
}
