package application_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dujiao-next/internal/app/httpserver/middleware"
	orderwiring "github.com/dujiao-next/internal/bootstrap/order"
	"github.com/dujiao-next/internal/constants"
	fulfillmentdomain "github.com/dujiao-next/internal/modules/fulfillment/domain"
	admindomain "github.com/dujiao-next/internal/modules/identity/admin/domain"
	adminstore "github.com/dujiao-next/internal/modules/identity/admin/infrastructure/gormstore"
	adminauthapp "github.com/dujiao-next/internal/modules/identity/adminauth/application"
	"github.com/dujiao-next/internal/modules/identity/jwttoken"
	kafudaapp "github.com/dujiao-next/internal/modules/kafudagpt/application"
	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
	orderapp "github.com/dujiao-next/internal/modules/order/application"
	orderrefund "github.com/dujiao-next/internal/modules/order/application/refund"
	ordercontract "github.com/dujiao-next/internal/modules/order/contract"
	orderdomain "github.com/dujiao-next/internal/modules/order/domain"
	orderhttp "github.com/dujiao-next/internal/modules/order/transport/http"
	paymentdomain "github.com/dujiao-next/internal/modules/payment/domain"
	paymentstore "github.com/dujiao-next/internal/modules/payment/infrastructure/gormstore"
	walletapp "github.com/dujiao-next/internal/modules/wallet/application"
	walletdomain "github.com/dujiao-next/internal/modules/wallet/domain"
	walletstore "github.com/dujiao-next/internal/modules/wallet/infrastructure/gormstore"
	"github.com/dujiao-next/internal/platform/http/response"
	"github.com/dujiao-next/internal/shared/money"
	kafudaworkflow "github.com/dujiao-next/internal/workflows/kafudagpt/application"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

func TestConfirmOverCapDirectCompletion(t *testing.T) {
	testConfirmOverCapDirectCompletion(t, newDirectCheckoutFixture)
}

type overCapDirectFixture struct {
	directCheckoutFixture
	order  *orderdomain.Order
	paidAt time.Time
	result kafudacontract.OrderResult
	id     uint
}

func seedOverCapDirectFixture(t *testing.T, f directCheckoutFixture) overCapDirectFixture {
	t.Helper()
	if err := f.db.AutoMigrate(&kafudadomain.Attempt{}); err != nil {
		t.Fatal(err)
	}
	order, err := orderapp.NewOrderService(f.options).CreateOrder(f.input)
	if err != nil || order == nil || len(order.Children) != 1 {
		t.Fatalf("create direct order for cost review: %+v, %v", order, err)
	}
	paidAt := time.Now().UTC().Truncate(time.Microsecond).Add(-2 * time.Second)
	childID := order.Children[0].ID
	if err := f.db.Model(&orderdomain.Order{}).Where("id IN ?", []uint{order.ID, childID}).
		Updates(map[string]any{"status": constants.OrderStatusPaid, "paid_at": paidAt}).Error; err != nil {
		t.Fatal(err)
	}
	records := kafudaworkflow.WithShopOrderTransitions(kafudaapp.New(f.store), f.options.OrderStore)
	reservation, err := records.Reserve(t.Context(), childID, paidAt, paidAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	quote := kafudacontract.Quote{Plan: "plus", Status: "available", Kind: "new", QuoteID: "over-cap-quote", PriceCents: 900,
		ExpiresAt: paidAt.Add(time.Minute), AutoCard: &kafudacontract.AutoCard{ProductCode: "checkout-card", OpenFeeCents: 50, LoadCents: 50, TotalCents: 1000}}
	if err := records.MarkSubmittingWithQuote(t.Context(), reservation.Attempt.ID, quote, paidAt, paidAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	charge := reservation.Recharge.Snapshot.MaxCostCents + 146
	result := kafudacontract.OrderResult{ID: 9007199254740993, Reference: reservation.Attempt.Reference, Product: "gpt", Plan: "plus", Country: "PH",
		Status: "processing", ChargedUSDCents: &charge, MaskedEmail: "b***@***"}
	return overCapDirectFixture{directCheckoutFixture: f, order: order, paidAt: paidAt, result: result, id: reservation.Attempt.ID}
}

func (f overCapDirectFixture) applySupplierResult(t *testing.T, status string) {
	t.Helper()
	result := f.result
	result.Status = status
	now := f.paidAt.Add(time.Second)
	completed, err := kafudaworkflow.ApplyResult(f.options.OrderStore, f.order.Children[0].ID, f.id, result, now, now, time.Minute)
	if err != nil || completed {
		t.Fatalf("over-cap %s response must enter review: completed=%t, err=%v", status, completed, err)
	}
	assertDirectShopOrderStatus(t, f.options.OrderStore, f.order, constants.OrderStatusFulfilling, f.paidAt)
}

func (f overCapDirectFixture) assertCompletion(t *testing.T, earlier *kafudadomain.Attempt, operator uint) {
	t.Helper()
	childID := f.order.Children[0].ID
	assertDirectShopOrderStatus(t, f.options.OrderStore, f.order, constants.OrderStatusCompleted, f.paidAt)
	record, err := f.store.GetRecharge(t.Context(), childID)
	if err != nil || record == nil || record.Stage != kafudadomain.StageCompleted || record.ReasonCode != kafudadomain.ReasonCostLimitExceeded ||
		record.CompletedAt == nil || record.CurrentAttemptID == nil || *record.CurrentAttemptID != earlier.ID {
		t.Fatalf("confirmation lost the original review facts: %+v, %v", record, err)
	}
	attempt, err := f.store.GetAttempt(t.Context(), earlier.ID)
	if err != nil || !reflect.DeepEqual(attempt, earlier) {
		t.Fatalf("confirmation changed the original upstream attempt: before=%+v after=%+v, %v", earlier, attempt, err)
	}
	var deliveries []fulfillmentdomain.Fulfillment
	if err := f.db.Where("order_id = ?", childID).Find(&deliveries).Error; err != nil || len(deliveries) != 1 {
		t.Fatalf("expected one delivery, got %d, %v", len(deliveries), err)
	}
	delivery := deliveries[0]
	if delivery.Type != constants.FulfillmentTypeKafudaGPT || delivery.Status != constants.FulfillmentStatusDelivered || delivery.Payload != "" ||
		delivery.DeliveredBy == nil || *delivery.DeliveredBy != operator || delivery.DeliveredAt == nil ||
		!delivery.DeliveredAt.Truncate(time.Microsecond).Equal(record.CompletedAt.Truncate(time.Microsecond)) {
		t.Fatalf("invalid supplier delivery or operator attribution: %+v", delivery)
	}
}

func testConfirmOverCapDirectCompletion(t *testing.T, fixture func(*testing.T) directCheckoutFixture) {
	t.Helper()
	t.Run("signed_admin_route", func(t *testing.T) {
		testOverCapDirectCompletionHTTP(t, seedOverCapDirectFixture(t, fixture(t)))
	})
	t.Run("rollback_and_competing_confirmations", func(t *testing.T) {
		testOverCapDirectCompletionConcurrency(t, seedOverCapDirectFixture(t, fixture(t)))
	})
}

func testOverCapDirectCompletionHTTP(t *testing.T, f overCapDirectFixture) {
	t.Helper()
	if err := f.db.AutoMigrate(&admindomain.Admin{}, &paymentdomain.Payment{}, &orderdomain.OrderRefundRecord{}, &walletdomain.Account{}, &walletdomain.Transaction{}); err != nil {
		t.Fatal(err)
	}
	admins := adminstore.New(f.db)
	admin := &admindomain.Admin{Username: "direct-cost-reviewer", PasswordHash: "hash"}
	if err := admins.Create(admin); err != nil {
		t.Fatal(err)
	}
	const secret = "direct-cost-review-test-signing-key"
	claims := adminauthapp.JWTClaims{AdminID: admin.ID, Username: admin.Username, TokenVersion: admin.TokenVersion, Typ: jwttoken.TypeAccess,
		RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)), IssuedAt: jwt.NewNumericDate(time.Now())}}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	const routePrefix = "/api/v1/admin"
	container := stoppedDirectSalesContainer(t, f.directCheckoutFixture)
	container.UserStore = f.options.UserStore
	container.PaymentStore = paymentstore.New(f.db, "test-guest-credential-secret-with-32-bytes")
	wallets := walletstore.New(f.db)
	container.OrderRefundService = orderrefund.New(f.options.OrderStore, nil, nil, nil, walletapp.NewService(walletapp.Options{Repository: wallets, Transactions: wallets}), nil)
	handlers := orderwiring.New(container)
	router := gin.New()
	adminRoutes := router.Group(routePrefix, middleware.JWTAuthMiddleware(secret, admins))
	orderhttp.RegisterAdminRoutes(adminRoutes, handlers.Admin)
	orderhttp.RegisterAdminRefundWriteRoutes(adminRoutes, handlers.AdminRefund)
	send := func(method, path, body string) response.Response {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, routePrefix+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(rec, req)
		var envelope response.Response
		if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
			t.Fatalf("invalid response for %s %s: %s, %v", method, path, rec.Body, err)
		}
		return envelope
	}
	request := func(method string, orderID uint, action, body string) response.Response {
		return send(method, fmt.Sprintf("/orders/%d%s", orderID, action), body)
	}
	assertInReviewQueue := func(want bool) {
		t.Helper()
		got := send(http.MethodGet, "/orders?direct_review=true", "")
		raw, err := json.Marshal(got.Data)
		if err != nil || got.StatusCode != response.CodeOK {
			t.Fatalf("load review queue: %+v, %v", got, err)
		}
		var items []orderhttp.AdminOrderListItem
		if err := json.Unmarshal(raw, &items); err != nil {
			t.Fatal(err)
		}
		for _, item := range items {
			if item.ID == f.order.ID {
				if !want {
					t.Fatal("confirmed order remained in the pending review queue")
				}
				return
			}
		}
		if want {
			t.Fatal("over-cap order missing from the review queue")
		}
	}
	details := func(wantStage string, wantEligibility bool) {
		t.Helper()
		got := request(http.MethodGet, f.order.ID, "", "")
		body, err := json.Marshal(got.Data)
		if err != nil || got.StatusCode != response.CodeOK {
			t.Fatalf("load admin order projection: %+v, %v", got, err)
		}
		var detail orderhttp.AdminOrderDetail
		if err := json.Unmarshal(body, &detail); err != nil {
			t.Fatal(err)
		}
		direct := detail.DirectRecharge
		if direct == nil || direct.Stage != wantStage || direct.Reason != kafudadomain.ReasonCostLimitExceeded ||
			direct.CanConfirmUpstreamCompletion != wantEligibility || direct.CostLimitUSD == nil || direct.CostLimitUSD.String() != "25.00" ||
			direct.Attempt == nil || direct.Attempt.ID != f.id || direct.Attempt.CardChargedUSD == nil || direct.Attempt.CardChargedUSD.String() != "26.46" ||
			direct.Attempt.QuoteCosts == nil || direct.Attempt.QuoteCosts.TotalUSD.String() != "10.00" {
			t.Fatalf("unexpected approval, debit, or cost cap projection: %s", body)
		}
	}
	body := func(id uint, confirmed bool) string {
		return fmt.Sprintf(`{"attempt_id":%d,"confirmed":%t,"admin_id":999,"amount":%q}`, id, confirmed, f.order.TotalAmount.String())
	}
	childID := f.order.Children[0].ID
	const confirmPath = "/direct-confirm-upstream"
	valid := body(f.id, true)
	assertRejected := func(target uint, path, payload string) {
		t.Helper()
		if got := request(http.MethodPost, target, path, payload); got.StatusCode != response.CodeBadRequest && got.StatusCode != response.CodeNotFound {
			t.Fatalf("unexpected response on rejected action %s: %+v", path, got)
		}
	}

	f.applySupplierResult(t, "processing")
	details(string(kafudadomain.StageManualReview), false)
	assertRejected(childID, confirmPath, valid)
	f.applySupplierResult(t, "completed")
	details(string(kafudadomain.StageManualReview), true)
	assertInReviewQueue(true)
	assertRejected(f.order.ID, confirmPath, valid)
	assertRejected(childID, confirmPath, body(f.id, false))
	assertRejected(childID, confirmPath, body(f.id+1, true))
	assertRejected(childID, "/direct-complete", valid)
	assertRejected(childID, "/direct-release", valid)
	assertRejected(f.order.ID, "/direct-refund-to-wallet", valid)
	assertRejected(f.order.ID, "/direct-manual-refund", valid)
	setShopStatus := func(status string, refunded money.Amount) {
		t.Helper()
		if err := f.db.Model(&orderdomain.Order{}).Where("id IN ?", []uint{f.order.ID, childID}).
			Updates(map[string]any{"status": status, "refunded_amount": refunded}).Error; err != nil {
			t.Fatal(err)
		}
	}
	setShopStatus(constants.OrderStatusCanceled, money.Amount{})
	assertRejected(childID, confirmPath, valid)
	setShopStatus(constants.OrderStatusRefunded, f.order.TotalAmount)
	assertRejected(childID, confirmPath, valid)
	setShopStatus(constants.OrderStatusFulfilling, money.Amount{})
	for _, model := range []any{&fulfillmentdomain.Fulfillment{}, &orderdomain.OrderRefundRecord{}, &walletdomain.Transaction{}} {
		var count int64
		if err := f.db.Model(model).Count(&count).Error; err != nil || count != 0 {
			t.Fatalf("a rejected action changed %T: %d rows, %v", model, count, err)
		}
	}
	earlier, err := f.store.GetAttempt(t.Context(), f.id)
	if err != nil || earlier == nil {
		t.Fatalf("read upstream attempt before confirmation: %+v, %v", earlier, err)
	}
	if got := request(http.MethodPost, childID, confirmPath, valid); got.StatusCode != response.CodeOK {
		t.Fatalf("confirm upstream completion via signed admin route: %+v", got)
	}
	details(string(kafudadomain.StageCompleted), false)
	assertInReviewQueue(false)
	assertRejected(childID, confirmPath, valid)
	f.assertCompletion(t, earlier, admin.ID)
}

var errOverCapParentUpdate = errors.New("injected parent checkout update failure")

type overCapFailingStore struct {
	ordercontract.Store
	parentID uint
}

func (s overCapFailingStore) WithinTransaction(run func(ordercontract.Transaction) error) error {
	return s.Store.WithinTransaction(func(tx ordercontract.Transaction) error {
		return run(overCapFailingTransaction{Transaction: tx, parentID: s.parentID})
	})
}

type overCapFailingTransaction struct {
	ordercontract.Transaction
	parentID uint
}

func (tx overCapFailingTransaction) Orders() ordercontract.Store {
	return overCapFailingOrders{Store: tx.Transaction.Orders(), parentID: tx.parentID}
}

type overCapFailingOrders struct {
	ordercontract.Store
	parentID uint
}

func (s overCapFailingOrders) UpdateStatus(id uint, status string, others map[string]any) error {
	if id == s.parentID && status == constants.OrderStatusCompleted {
		return errOverCapParentUpdate
	}
	return s.Store.UpdateStatus(id, status, others)
}

type overCapCompletionQueue struct {
	ordercontract.Queue
	store    ordercontract.Store
	parentID uint
	calls    atomic.Int32
}

func (q *overCapCompletionQueue) Enabled() bool { return true }

func (q *overCapCompletionQueue) EnqueueStatusEmail(orderID uint, status string) error {
	parent, err := q.store.GetByID(orderID)
	if err != nil || parent == nil || parent.Status != constants.OrderStatusCompleted ||
		orderID != q.parentID || status != constants.OrderStatusCompleted {
		return fmt.Errorf("notification did not observe committed parent %d: %s, %v", orderID, status, err)
	}
	q.calls.Add(1)
	return nil
}

func testOverCapDirectCompletionConcurrency(t *testing.T, f overCapDirectFixture) {
	t.Helper()
	f.applySupplierResult(t, "completed")
	orders, childID := f.options.OrderStore, f.order.Children[0].ID
	earlier, err := f.store.GetAttempt(t.Context(), f.id)
	if err != nil || earlier == nil {
		t.Fatalf("load over-cap attempt: %+v, %v", earlier, err)
	}
	before, err := f.store.GetRecharge(t.Context(), childID)
	if err != nil || before == nil {
		t.Fatalf("load over-cap recharge: %+v, %v", before, err)
	}
	queue := &overCapCompletionQueue{store: orders, parentID: f.order.ID}
	confirm := func(store ordercontract.Store, operator uint) error {
		return kafudaworkflow.ConfirmUpstreamCompletionAndNotify(t.Context(), store, f.store, queue, nil, nil, childID, f.id, operator, true, time.Now().UTC())
	}
	if err := confirm(overCapFailingStore{Store: orders, parentID: f.order.ID}, 13); !errors.Is(err, errOverCapParentUpdate) {
		t.Fatalf("parent checkout error did not abort confirmation: %v", err)
	}
	var count int64
	if err := f.db.Model(&fulfillmentdomain.Fulfillment{}).Count(&count).Error; err != nil || count != 0 || queue.calls.Load() != 0 {
		t.Fatalf("failed transaction committed delivery or mail: %d/%d, %v", count, queue.calls.Load(), err)
	}
	rolledBack, err := f.store.GetRecharge(t.Context(), childID)
	if err != nil || !reflect.DeepEqual(rolledBack, before) {
		t.Fatalf("failed transaction changed recharge: %+v, %v", rolledBack, err)
	}
	assertDirectShopOrderStatus(t, orders, f.order, constants.OrderStatusFulfilling, f.paidAt)

	start := make(chan struct{})
	type outcome struct {
		operator uint
		err      error
	}
	results := make(chan outcome, 2)
	for _, operator := range []uint{14, 15} {
		go func() {
			<-start
			results <- outcome{operator: operator, err: confirm(orders, operator)}
		}()
	}
	close(start)
	var winningOperator uint
	for range 2 {
		result := <-results
		if result.err == nil && winningOperator == 0 {
			winningOperator = result.operator
		} else if !errors.Is(result.err, orderapp.ErrOrderStatusInvalid) {
			t.Fatalf("unexpected competing result: %+v", result)
		}
	}
	if winningOperator == 0 || queue.calls.Load() != 1 {
		t.Fatalf("expected one successful confirmation and one notification: operator=%d mail=%d", winningOperator, queue.calls.Load())
	}
	if err := confirm(orders, 16); !errors.Is(err, orderapp.ErrOrderStatusInvalid) || queue.calls.Load() != 1 {
		t.Fatalf("replay changed completion: mail=%d, err=%v", queue.calls.Load(), err)
	}
	f.assertCompletion(t, earlier, winningOperator)
}
