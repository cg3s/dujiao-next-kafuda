package application_test

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/dujiao-next/internal/config"
	"github.com/dujiao-next/internal/constants"
	cardsecretdomain "github.com/dujiao-next/internal/modules/cardsecret/domain"
	mappingdomain "github.com/dujiao-next/internal/modules/catalog/mapping/domain"
	mappingstore "github.com/dujiao-next/internal/modules/catalog/mapping/infrastructure/gormstore"
	fulfillmentapp "github.com/dujiao-next/internal/modules/fulfillment/application"
	fulfillmentdomain "github.com/dujiao-next/internal/modules/fulfillment/domain"
	fulfillmentstore "github.com/dujiao-next/internal/modules/fulfillment/infrastructure/gormstore"
	orderdomain "github.com/dujiao-next/internal/modules/order/domain"
	procurementapp "github.com/dujiao-next/internal/modules/procurement/application"
	procurementcontract "github.com/dujiao-next/internal/modules/procurement/contract"
	procurementdomain "github.com/dujiao-next/internal/modules/procurement/domain"
	procurementstore "github.com/dujiao-next/internal/modules/procurement/infrastructure/gormstore"
	procurementmapping "github.com/dujiao-next/internal/modules/procurement/infrastructure/mappingreader"
	procurementorder "github.com/dujiao-next/internal/modules/procurement/infrastructure/orderreader"
	siteconnectiondomain "github.com/dujiao-next/internal/modules/siteconnection/domain"
)

const (
	mixedCheckoutCardPayload     = "checkout-fixture-card"
	mixedCheckoutManualPayload   = "checkout-fixture-manual"
	mixedCheckoutUpstreamPayload = "checkout-fixture-upstream"
	mixedCheckoutUpstreamSKUID   = 501
	mixedCheckoutUpstreamOrderID = 701
	mixedCheckoutUpstreamOrderNo = "CHECKOUT-FIXTURE-701"
)

type mixedCheckoutProcurement struct {
	service      *procurementapp.Service
	connectionID uint
	submitIDs    []uint
	pollIDs      []uint
	created      []procurementcontract.CreateOrderRequest
	queried      []uint
}

func newMixedCheckoutProcurement(t *testing.T, f directCheckoutFixture, productID, skuID uint) *mixedCheckoutProcurement {
	t.Helper()
	if err := f.db.AutoMigrate(&siteconnectiondomain.Connection{}, &mappingdomain.Mapping{}, &mappingdomain.SKUMapping{}, &procurementdomain.Order{}); err != nil {
		t.Fatal(err)
	}
	connection := siteconnectiondomain.Connection{
		Name: "checkout-fixture", BaseURL: "https://upstream.example.test", Protocol: constants.ConnectionProtocolDujiaoNext,
	}
	if err := f.db.Create(&connection).Error; err != nil {
		t.Fatal(err)
	}
	mapping := mappingdomain.Mapping{ConnectionID: connection.ID, LocalProductID: productID, UpstreamProductID: 41, IsActive: true}
	if err := f.db.Create(&mapping).Error; err != nil {
		t.Fatal(err)
	}
	skuMapping := mappingdomain.SKUMapping{ProductMappingID: mapping.ID, LocalSKUID: skuID, UpstreamSKUID: mixedCheckoutUpstreamSKUID, UpstreamIsActive: true, UpstreamStock: 5}
	if err := f.db.Create(&skuMapping).Error; err != nil {
		t.Fatal(err)
	}

	procurement := &mixedCheckoutProcurement{connectionID: connection.ID}
	repository := procurementstore.New(f.db)
	procurement.service = procurementapp.NewService(procurementapp.Options{
		Repository: repository, Orders: procurementorder.New(f.options.OrderStore),
		ProductMappings: procurementmapping.NewProducts(mappingstore.NewMappingStore(f.db)),
		SKUMappings:     procurementmapping.NewSKUs(mappingstore.NewSKUMappingStore(f.db)),
		Connections:     procurement, Queue: procurement, OrderLifecycle: repository.NewLifecycle(nil, nil, config.EmailConfig{}),
	})
	return procurement
}

func (p *mixedCheckoutProcurement) Open(id uint) (procurementcontract.UpstreamConnection, error) {
	if id != p.connectionID {
		return nil, fmt.Errorf("unexpected fixture connection %d", id)
	}
	return p, nil
}

func (*mixedCheckoutProcurement) CallbackURL() string {
	return "https://shop.example.test/payment-fixture-callback"
}
func (*mixedCheckoutProcurement) RetryMax() int          { return 0 }
func (*mixedCheckoutProcurement) RetryIntervals() string { return "[]" }
func (*mixedCheckoutProcurement) CancelOrder(context.Context, uint) error {
	return fmt.Errorf("unexpected fixture cancellation")
}

func (p *mixedCheckoutProcurement) EnqueueSubmit(id uint, _ time.Duration) error {
	p.submitIDs = append(p.submitIDs, id)
	return nil
}

func (p *mixedCheckoutProcurement) EnqueuePoll(id uint, _ time.Duration) error {
	p.pollIDs = append(p.pollIDs, id)
	return nil
}

func (p *mixedCheckoutProcurement) CreateOrder(_ context.Context, req procurementcontract.CreateOrderRequest) (*procurementcontract.CreateOrderResult, error) {
	p.created = append(p.created, req)
	return &procurementcontract.CreateOrderResult{OK: true, OrderID: mixedCheckoutUpstreamOrderID, OrderNo: mixedCheckoutUpstreamOrderNo,
		Status: constants.OrderStatusFulfilling, Amount: "10.00", Currency: "CNY"}, nil
}

func (p *mixedCheckoutProcurement) GetOrder(_ context.Context, id uint) (*procurementcontract.UpstreamOrder, error) {
	p.queried = append(p.queried, id)
	return &procurementcontract.UpstreamOrder{OrderID: mixedCheckoutUpstreamOrderID, OrderNo: mixedCheckoutUpstreamOrderNo,
		Status: constants.OrderStatusDelivered, Fulfillment: &procurementcontract.Fulfillment{Payload: mixedCheckoutUpstreamPayload}}, nil
}

func exerciseMixedCheckoutFulfillment(t *testing.T, f directCheckoutFixture, order *orderdomain.Order, autoJobIDs []uint, procurement *mixedCheckoutProcurement) {
	t.Helper()
	children := make(map[string]*orderdomain.Order, len(order.Children))
	for _, child := range order.Children {
		if child.ParentID == nil || *child.ParentID != order.ID || child.UserID != order.UserID || len(child.Items) != 1 || child.PaidAt == nil {
			t.Fatalf("unexpected paid checkout child: %+v", child)
		}
		children[child.Items[0].FulfillmentType] = &child
	}
	auto := children[constants.FulfillmentTypeAuto]
	manual := children[constants.FulfillmentTypeManual]
	upstream := children[constants.FulfillmentTypeUpstream]
	if len(order.Children) != 3 || auto == nil || manual == nil || upstream == nil || order.Status != constants.OrderStatusFulfilling ||
		auto.Status != constants.OrderStatusPaid || manual.Status != constants.OrderStatusFulfilling || upstream.Status != constants.OrderStatusFulfilling {
		t.Fatalf("unexpected mixed checkout after payment: %+v", order)
	}
	if !slices.Equal(autoJobIDs, []uint{auto.ID}) {
		t.Fatalf("automatic fulfillment jobs = %v, want only child %d", autoJobIDs, auto.ID)
	}
	procurements, total, err := procurement.service.List(procurementcontract.ListFilter{})
	if err != nil || total != 1 || len(procurements) != 1 {
		t.Fatalf("ordinary checkout procurement count = %d, rows = %d, err = %v", total, len(procurements), err)
	}
	orderToProcure := procurements[0]
	if orderToProcure.LocalOrderID != upstream.ID || orderToProcure.LocalOrderNo != upstream.OrderNo || orderToProcure.ConnectionID != procurement.connectionID ||
		orderToProcure.Status != constants.ProcurementStatusPending || !orderToProcure.LocalSellAmount.Equal(upstream.TotalAmount.Decimal) ||
		!slices.Equal(procurement.submitIDs, []uint{orderToProcure.ID}) || len(procurement.created) != 0 || len(procurement.pollIDs) != 0 {
		t.Fatalf("unexpected procurement routing after payment: %+v, submit jobs %v, poll jobs %v", orderToProcure, procurement.submitIDs, procurement.pollIDs)
	}
	var prematureDeliveries int64
	if err := f.db.Model(&fulfillmentdomain.Fulfillment{}).Count(&prematureDeliveries).Error; err != nil || prematureDeliveries != 0 {
		t.Fatalf("payment wrote %d fulfillment records before delivery, err = %v", prematureDeliveries, err)
	}

	fulfillments := fulfillmentapp.New(fulfillmentapp.Options{
		OrderStore: f.options.OrderStore, FulfillmentStore: fulfillmentstore.New(f.db), OrderQueue: f.options.Queue,
	})
	if _, err := fulfillments.CreateAuto(autoJobIDs[0]); err != nil {
		t.Fatalf("fulfill automatic card child: %v", err)
	}
	assertMixedCheckoutStatus(t, f, order.ID, constants.OrderStatusPartiallyDelivered)
	const manualOperatorID uint = 9
	if _, err := fulfillments.CreateManual(fulfillmentapp.CreateManualInput{OrderID: manual.ID, AdminID: manualOperatorID, Payload: mixedCheckoutManualPayload}); err != nil {
		t.Fatalf("fulfill manual child: %v", err)
	}
	assertMixedCheckoutStatus(t, f, order.ID, constants.OrderStatusPartiallyDelivered)

	if err := procurement.service.SubmitToUpstream(procurement.submitIDs[0]); err != nil {
		t.Fatalf("submit upstream procurement: %v", err)
	}
	if len(procurement.created) != 1 || !slices.Equal(procurement.pollIDs, []uint{orderToProcure.ID}) {
		t.Fatalf("upstream create requests = %d, poll jobs = %v", len(procurement.created), procurement.pollIDs)
	}
	request := procurement.created[0]
	if request.SKUID != mixedCheckoutUpstreamSKUID || request.Quantity != upstream.Items[0].Quantity || request.DownstreamOrderNo != upstream.OrderNo || request.TraceID != orderToProcure.TraceID {
		t.Fatalf("upstream request used the wrong checkout item: %+v", request)
	}
	for range 2 {
		if err := procurement.service.PollUpstreamStatus(procurement.pollIDs[0]); err != nil {
			t.Fatalf("poll upstream procurement: %v", err)
		}
	}
	if !slices.Equal(procurement.queried, []uint{mixedCheckoutUpstreamOrderID}) {
		t.Fatalf("upstream polling after completion did not skip replay: lookups %v", procurement.queried)
	}
	completedProcurement, err := procurementstore.New(f.db).GetByID(orderToProcure.ID)
	if err != nil || completedProcurement == nil || completedProcurement.Status != constants.ProcurementStatusFulfilled || completedProcurement.UpstreamOrderNo != mixedCheckoutUpstreamOrderNo ||
		completedProcurement.UpstreamPayload != mixedCheckoutUpstreamPayload {
		t.Fatalf("unexpected settled upstream procurement: %+v, err = %v", completedProcurement, err)
	}

	assertMixedCheckoutStatus(t, f, order.ID, constants.OrderStatusDelivered)
	assertMixedCheckoutStatus(t, f, auto.ID, constants.OrderStatusCompleted)
	assertMixedCheckoutStatus(t, f, manual.ID, constants.OrderStatusDelivered)
	assertMixedCheckoutStatus(t, f, upstream.ID, constants.OrderStatusDelivered)
	var deliveries []fulfillmentdomain.Fulfillment
	if err := f.db.Find(&deliveries).Error; err != nil || len(deliveries) != 3 {
		t.Fatalf("expected three checkout deliveries, got %d, err = %v", len(deliveries), err)
	}
	delivered := make(map[uint]fulfillmentdomain.Fulfillment, len(deliveries))
	for _, delivery := range deliveries {
		delivered[delivery.OrderID] = delivery
	}
	for _, expected := range []struct {
		child         *orderdomain.Order
		kind, payload string
	}{
		{auto, constants.FulfillmentTypeAuto, mixedCheckoutCardPayload},
		{manual, constants.FulfillmentTypeManual, mixedCheckoutManualPayload},
		{upstream, constants.FulfillmentTypeUpstream, mixedCheckoutUpstreamPayload},
	} {
		delivery := delivered[expected.child.ID]
		if delivery.Type != expected.kind || delivery.Status != constants.FulfillmentStatusDelivered || delivery.Payload != expected.payload || delivery.DeliveredAt == nil {
			t.Fatalf("unexpected delivery for child %d: %+v", expected.child.ID, delivery)
		}
	}
	if by := delivered[manual.ID].DeliveredBy; by == nil || *by != manualOperatorID {
		t.Fatalf("manual delivery operator = %v, want %d", by, manualOperatorID)
	}
	var used, unused cardsecretdomain.Secret
	if err := f.db.Where("product_id = ? AND secret = ?", auto.Items[0].ProductID, mixedCheckoutCardPayload).First(&used).Error; err != nil ||
		used.Status != cardsecretdomain.StatusUsed || used.OrderID == nil || *used.OrderID != auto.ID || used.UsedAt == nil || used.ReservedAt != nil {
		t.Fatalf("card not consumed by its automatic child: %+v, err = %v", used, err)
	}
	if err := f.db.Where("product_id = ? AND id <> ?", auto.Items[0].ProductID, used.ID).First(&unused).Error; err != nil ||
		unused.Status != cardsecretdomain.StatusAvailable || unused.OrderID != nil || unused.UsedAt != nil || unused.ReservedAt != nil {
		t.Fatalf("unrelated card inventory was changed: %+v, err = %v", unused, err)
	}
}

func assertMixedCheckoutStatus(t *testing.T, f directCheckoutFixture, orderID uint, expected string) {
	t.Helper()
	order, err := f.options.OrderStore.GetByID(orderID)
	if err != nil || order == nil || order.Status != expected {
		t.Fatalf("order %d should be %q, got %+v, err = %v", orderID, expected, order, err)
	}
}
