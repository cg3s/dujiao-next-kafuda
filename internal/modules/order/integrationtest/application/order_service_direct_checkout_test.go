package application_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dujiao-next/internal/app/container"
	"github.com/dujiao-next/internal/app/httpserver/middleware"
	kafudawiring "github.com/dujiao-next/internal/bootstrap/kafudagpt"
	orderwiring "github.com/dujiao-next/internal/bootstrap/order"
	"github.com/dujiao-next/internal/config"
	"github.com/dujiao-next/internal/constants"
	productdomain "github.com/dujiao-next/internal/modules/catalog/product/domain"
	productstore "github.com/dujiao-next/internal/modules/catalog/product/store/gormstore"
	coupondomain "github.com/dujiao-next/internal/modules/coupon/domain"
	fulfillmentapp "github.com/dujiao-next/internal/modules/fulfillment/application"
	fulfillmentdomain "github.com/dujiao-next/internal/modules/fulfillment/domain"
	userstore "github.com/dujiao-next/internal/modules/identity/user/infrastructure/gormstore"
	userauthapp "github.com/dujiao-next/internal/modules/identity/userauth/application"
	kafudaapp "github.com/dujiao-next/internal/modules/kafudagpt/application"
	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
	kafudastore "github.com/dujiao-next/internal/modules/kafudagpt/infrastructure/gormstore"
	memberapp "github.com/dujiao-next/internal/modules/memberlevel/application"
	memberdomain "github.com/dujiao-next/internal/modules/memberlevel/domain"
	memberstore "github.com/dujiao-next/internal/modules/memberlevel/infrastructure/gormstore"
	orderapp "github.com/dujiao-next/internal/modules/order/application"
	orderrefund "github.com/dujiao-next/internal/modules/order/application/refund"
	ordercontract "github.com/dujiao-next/internal/modules/order/contract"
	orderdomain "github.com/dujiao-next/internal/modules/order/domain"
	orderstore "github.com/dujiao-next/internal/modules/order/infrastructure/gormstore"
	orderhttp "github.com/dujiao-next/internal/modules/order/transport/http"
	orderpresenter "github.com/dujiao-next/internal/modules/order/transport/presenter"
	paymentapp "github.com/dujiao-next/internal/modules/payment/application"
	paymentdomain "github.com/dujiao-next/internal/modules/payment/domain"
	paymentstore "github.com/dujiao-next/internal/modules/payment/infrastructure/gormstore"
	promotiondomain "github.com/dujiao-next/internal/modules/promotion/domain"
	promotionstore "github.com/dujiao-next/internal/modules/promotion/infrastructure/gormstore"
	resellercontract "github.com/dujiao-next/internal/modules/reseller/contract"
	walletapp "github.com/dujiao-next/internal/modules/wallet/application"
	walletdomain "github.com/dujiao-next/internal/modules/wallet/domain"
	walletstore "github.com/dujiao-next/internal/modules/wallet/infrastructure/gormstore"
	"github.com/dujiao-next/internal/platform/http/response"
	"github.com/dujiao-next/internal/shared/jsonmap"
	"github.com/dujiao-next/internal/shared/money"
	kafudaworkflow "github.com/dujiao-next/internal/workflows/kafudagpt/application"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

type directCheckoutFixture struct {
	couponLimitRaceFixture
	options orderapp.OrderServiceOptions
	input   orderapp.CreateOrderInput
	catalog *kafudadomain.Catalog
	store   *kafudastore.Store
}

func TestAdminDirectReviewFilterAndProjection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	f := newDirectCheckoutFixture(t)
	if err := f.db.AutoMigrate(&kafudadomain.Attempt{}, &paymentdomain.Payment{}); err != nil {
		t.Fatal(err)
	}
	service := orderapp.NewOrderService(f.options)
	records := kafudaapp.New(f.store)
	ctx, now := context.Background(), time.Now().UTC()
	var parentID, childID uint
	for _, stage := range []kafudadomain.Stage{kafudadomain.StageManualReview, kafudadomain.StageManualDelivery, kafudadomain.StageWaiting} {
		order, err := service.CreateOrder(f.input)
		if err != nil {
			t.Fatal(err)
		}
		child := order.Children[0].ID
		if stage == kafudadomain.StageManualReview {
			parentID, childID = order.ID, child
			reserved, err := records.Reserve(ctx, child, now, now.Add(time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			if err := f.db.Model(&kafudadomain.Attempt{}).Where("id = ?", reserved.Attempt.ID).Updates(map[string]any{
				"status": kafudadomain.AttemptReview, "upstream_order_id": uint64(9007199254740993), "masked_email": "private@example.test", "quote_id": "private-quote",
			}).Error; err != nil {
				t.Fatal(err)
			}
		}
		if err := f.db.Model(&kafudadomain.Recharge{}).Where("order_id = ?", child).
			Updates(map[string]any{"stage": stage, "reason_code": "outcome_unknown"}).Error; err != nil {
			t.Fatal(err)
		}
	}
	ordinary := orderdomain.Order{OrderNo: "ORDINARY-REVIEW-TEST", UserID: f.users[0].ID, Currency: "CNY", Status: constants.OrderStatusPaid}
	if err := f.db.Create(&ordinary).Error; err != nil {
		t.Fatal(err)
	}
	handler := orderwiring.New(&container.Container{
		OrderService: service, OrderStore: f.options.OrderStore, UserStore: userstore.New(f.db),
		PaymentStore: paymentstore.New(f.db, "test-guest-credential-secret-with-32-bytes"),
		KafudaGPT:    &kafudawiring.Module{Store: f.store},
	}).Admin
	router := gin.New()
	orderhttp.RegisterAdminRoutes(router.Group("/admin"), handler)
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}
	for _, id := range []uint{parentID, childID} {
		raw := get(fmt.Sprintf("/admin/orders/%d", id)).Body.String()
		if !strings.Contains(raw, `"upstream_order_id":"9007199254740993"`) || !strings.Contains(raw, `"reason":"outcome_unknown"`) ||
			!strings.Contains(raw, `"active":true`) || strings.Contains(raw, "private@example.test") || strings.Contains(raw, "private-quote") {
			t.Fatalf("unexpected projection: %s", raw)
		}
	}
	if err := f.db.Model(&kafudadomain.Attempt{}).Where("order_id = ?", childID).Update("masked_email", "b***@***").Error; err != nil {
		t.Fatal(err)
	}
	if raw := get(fmt.Sprintf("/admin/orders/%d", childID)).Body.String(); !strings.Contains(raw, `"masked_email":"b***@***"`) {
		t.Fatalf("valid masked account was omitted: %s", raw)
	}
	if raw := get(fmt.Sprintf("/admin/orders/%d", ordinary.ID)).Body.String(); strings.Contains(raw, "direct_recharge") || !strings.Contains(raw, `"status_code":0`) {
		t.Fatalf("ordinary order response: %s", raw)
	}
	seen := make(map[uint]bool)
	for page := 1; page <= 2; page++ {
		raw := get(fmt.Sprintf("/admin/orders?direct_review=true&page=%d&page_size=1", page)).Body.Bytes()
		var result struct {
			Code int `json:"status_code"`
			Data []struct {
				ID uint `json:"id"`
			} `json:"data"`
			Pagination struct {
				Total int `json:"total"`
			} `json:"pagination"`
		}
		if err := json.Unmarshal(raw, &result); err != nil || result.Code != 0 || result.Pagination.Total != 2 || len(result.Data) != 1 {
			t.Fatalf("filtered page %d: %s, %v", page, raw, err)
		}
		seen[result.Data[0].ID] = true
	}
	if len(seen) != 2 || !seen[parentID] || seen[ordinary.ID] {
		t.Fatalf("filtered pages did not return unique review orders: %v", seen)
	}
	if raw := get("/admin/orders?direct_review=invalid").Body.String(); !strings.Contains(raw, `"status_code":400`) {
		t.Fatalf("invalid filter response: %s", raw)
	}
}

func TestDirectCheckoutGenericAfterSalesCannotBypassReview(t *testing.T) {
	for _, method := range []string{"wallet", "manual"} {
		t.Run(method, func(t *testing.T) { testDirectCheckoutRefund(t, method) })
	}
}

func testDirectCheckoutRefund(t *testing.T, method string) {
	f := newDirectCheckoutFixture(t)
	if err := f.db.AutoMigrate(&kafudadomain.Attempt{}, &orderdomain.OrderRefundRecord{}, &walletdomain.Account{}, &walletdomain.Transaction{}); err != nil {
		t.Fatal(err)
	}
	service := orderapp.NewOrderService(f.options)
	order, err := service.CreateOrder(f.input)
	if err != nil || order == nil || len(order.Children) != 1 {
		t.Fatalf("create direct order: %+v, %v", order, err)
	}
	ctx, now := context.Background(), time.Now().UTC()
	childID := order.Children[0].ID
	ids := []uint{order.ID, childID}
	if err := f.db.Model(&orderdomain.Order{}).Where("id IN ?", ids).Updates(map[string]any{
		"status": constants.OrderStatusPaid, "paid_at": now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	records := kafudaapp.New(f.store)
	reservation, err := records.Reserve(ctx, childID, now, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	id := reservation.Attempt.ID
	if err := records.MarkSubmitting(ctx, id, "quote", now, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := records.MarkUnknown(ctx, id, now, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	wallets := walletstore.New(f.db)
	refunds := orderrefund.New(f.options.OrderStore, nil, nil, nil, walletapp.NewService(walletapp.Options{Repository: wallets, Transactions: wallets}), nil)
	fulfillment := fulfillmentapp.New(fulfillmentapp.Options{OrderStore: f.options.OrderStore})
	amount := money.FromDecimal(decimal.NewFromInt(1))
	for _, orderID := range ids {
		if _, _, _, err := refunds.AdminRefundToWallet(orderrefund.AdminRefundToWalletInput{OrderID: orderID, Amount: amount}); !errors.Is(err, orderrefund.ErrOrderStatusInvalid) {
			t.Fatalf("wallet refund on %d: %v", orderID, err)
		}
		if _, _, err := refunds.AdminManualRefund(orderrefund.AdminManualRefundInput{OrderID: orderID, Amount: amount}); !errors.Is(err, orderrefund.ErrOrderStatusInvalid) {
			t.Fatalf("manual refund on %d: %v", orderID, err)
		}
		for _, status := range []string{constants.OrderStatusCompleted, constants.OrderStatusRefunded} {
			if _, err := service.UpdateOrderStatus(orderID, status); !errors.Is(err, orderapp.ErrOrderStatusInvalid) {
				t.Fatalf("common status update on %d to %s: %v", orderID, status, err)
			}
		}
		var stored orderdomain.Order
		if err := f.db.First(&stored, orderID).Error; err != nil || stored.Status != constants.OrderStatusPaid || !stored.RefundedAmount.IsZero() {
			t.Fatalf("after-sales changed %d: %+v, %v", orderID, stored, err)
		}
	}
	if _, err := fulfillment.CreateManual(fulfillmentapp.CreateManualInput{OrderID: childID, AdminID: 1, Payload: "test"}); !errors.Is(err, fulfillmentapp.ErrOrderStatusInvalid) {
		t.Fatalf("generic manual delivery: %v", err)
	}
	for _, model := range []any{&orderdomain.OrderRefundRecord{}, &walletdomain.Transaction{}, &fulfillmentdomain.Fulfillment{}} {
		var count int64
		if err := f.db.Model(model).Count(&count).Error; err != nil || count != 0 {
			t.Fatalf("rejected after-sales left %T rows: %d, %v", model, count, err)
		}
	}
	attempt, err := f.store.GetAttempt(ctx, id)
	if err != nil || attempt == nil || attempt.Status != kafudadomain.AttemptReview || attempt.ActiveOrderID == nil {
		t.Fatalf("after-sales changed the unknown attempt: %+v, %v", attempt, err)
	}
	apply := func(orderID, confirmedAttemptID uint, amount money.Amount) error {
		if method == "wallet" {
			_, _, _, err := refunds.AdminRefundToWallet(orderrefund.AdminRefundToWalletInput{OrderID: orderID, Amount: amount, DirectAttemptID: confirmedAttemptID})
			return err
		}
		_, _, err := refunds.AdminManualRefund(orderrefund.AdminManualRefundInput{OrderID: orderID, Amount: amount, DirectAttemptID: confirmedAttemptID})
		return err
	}
	if err := apply(order.ID, id, order.TotalAmount); !errors.Is(err, orderrefund.ErrOrderStatusInvalid) {
		t.Fatalf("confirmed unknown refund: %v", err)
	}
	result := kafudacontract.OrderResult{ID: 101, Reference: reservation.Attempt.Reference, Product: "gpt", Plan: "plus", Country: "PH", Status: "failed"}
	if _, err := kafudaworkflow.ApplyResult(f.options.OrderStore, childID, id, result, now, time.Time{}); err != nil {
		t.Fatal(err)
	}
	for _, input := range []struct{ orderID, attemptID uint }{{childID, id}, {order.ID, id + 1}, {order.ID, 0}} {
		if err := apply(input.orderID, input.attemptID, order.TotalAmount); !errors.Is(err, orderrefund.ErrOrderStatusInvalid) {
			t.Fatalf("invalid refund target accepted: %+v, %v", input, err)
		}
	}
	if err := apply(order.ID, id, amount); !errors.Is(err, orderrefund.ErrOrderStatusInvalid) {
		t.Fatalf("partial direct refund accepted: %v", err)
	}
	handler := orderwiring.NewAdminRefundHandler(&container.Container{OrderStore: f.options.OrderStore, OrderRefundService: refunds})
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("admin_id", uint(9)) })
	orderhttp.RegisterAdminRefundWriteRoutes(router.Group("/admin"), handler)
	path := "manual-refund"
	if method == "wallet" {
		path = "refund-to-wallet"
	}
	request := func(path string, confirmed bool) string {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/admin/orders/%d/%s", order.ID, path),
			strings.NewReader(fmt.Sprintf(`{"amount":%q,"attempt_id":%d,"confirmed":%t}`, order.TotalAmount.Decimal.StringFixed(2), id, confirmed)))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(rec, req)
		return rec.Body.String()
	}
	if body := request(path, true); strings.Contains(body, `"status_code":0`) {
		t.Fatalf("generic endpoint accepted direct refund: %s", body)
	}
	if body := request("direct-"+path, false); strings.Contains(body, `"status_code":0`) {
		t.Fatalf("unconfirmed direct refund accepted: %s", body)
	}
	if body := request("direct-"+path, true); !strings.Contains(body, `"status_code":0`) {
		t.Fatalf("direct refund failed: %s", body)
	}
	if err := apply(order.ID, id, order.TotalAmount); !errors.Is(err, orderrefund.ErrOrderStatusInvalid) {
		t.Fatalf("refund replay: %v", err)
	}
	for _, orderID := range ids {
		var stored orderdomain.Order
		if err := f.db.First(&stored, orderID).Error; err != nil || stored.Status != constants.OrderStatusRefunded {
			t.Fatalf("refunded order %d: %+v, %v", orderID, stored, err)
		}
	}
	if err := kafudaworkflow.ReleaseManual(f.options.OrderStore, childID, id, 9, true, time.Now().UTC()); !errors.Is(err, orderapp.ErrOrderStatusInvalid) {
		t.Fatalf("release after refund: %v", err)
	}
	reserveAt := time.Now().UTC()
	if _, err := kafudaworkflow.ReserveBuyerAttempt(f.options.OrderStore, ordercontract.TenantScope{}, order.OrderNo, order.UserID, reserveAt, reserveAt.Add(time.Minute)); !errors.Is(err, orderapp.ErrOrderStatusInvalid) {
		t.Fatalf("buyer reservation after refund: %v", err)
	}
	result.Status = "completed"
	if completed, err := kafudaworkflow.ApplyResult(f.options.OrderStore, childID, id, result, time.Now().UTC(), time.Time{}); completed || !errors.Is(err, orderapp.ErrOrderStatusInvalid) {
		t.Fatalf("supplier completion after refund: completed=%v err=%v", completed, err)
	}
	var refund orderdomain.OrderRefundRecord
	if err := f.db.Where("order_id = ?", order.ID).First(&refund).Error; err != nil || !refund.Amount.Decimal.Equal(order.TotalAmount.Decimal) {
		t.Fatalf("refund record: %+v, %v", refund, err)
	}
	var credits int64
	if err := f.db.Model(&walletdomain.Transaction{}).Count(&credits).Error; err != nil || (method == "wallet" && credits != 1) || (method == "manual" && credits != 0) {
		t.Fatalf("wallet credits: %d, %v", credits, err)
	}
}

func TestReserveBuyerAttemptRequiresPaidScopedCheckoutAndDeduplicates(t *testing.T) {
	f := newDirectCheckoutFixture(t)
	if err := f.db.AutoMigrate(&kafudadomain.Attempt{}); err != nil {
		t.Fatal(err)
	}
	order, err := orderapp.NewOrderService(f.options).CreateOrder(f.input)
	if err != nil {
		t.Fatal(err)
	}
	child := order.Children[0]
	now := time.Now().UTC()
	reserve := func(orderNo string, userID uint, scope ordercontract.TenantScope) (*kafudaapp.Reservation, error) {
		return kafudaworkflow.ReserveBuyerAttempt(f.options.OrderStore, scope, orderNo, userID, now, now.Add(time.Minute))
	}
	main := ordercontract.TenantScope{}
	if _, err := reserve(order.OrderNo, order.UserID, main); !errors.Is(err, orderapp.ErrOrderStatusInvalid) {
		t.Fatalf("unpaid checkout: %v", err)
	}
	ids := []uint{order.ID, child.ID}
	update := func(status string, refunded money.Amount) {
		t.Helper()
		if err := f.db.Model(&orderdomain.Order{}).Where("id IN ?", ids).
			Updates(map[string]any{"status": status, "paid_at": now, "refunded_amount": refunded}).Error; err != nil {
			t.Fatal(err)
		}
	}
	update(constants.OrderStatusCanceled, money.Amount{})
	if _, err := reserve(order.OrderNo, order.UserID, main); !errors.Is(err, orderapp.ErrOrderStatusInvalid) {
		t.Fatalf("canceled checkout: %v", err)
	}
	update(constants.OrderStatusRefunded, order.TotalAmount)
	if _, err := reserve(order.OrderNo, order.UserID, main); !errors.Is(err, orderapp.ErrOrderStatusInvalid) {
		t.Fatalf("refunded checkout: %v", err)
	}
	update(constants.OrderStatusPaid, money.Amount{})
	resellerID := uint(42)
	for _, request := range []struct {
		orderNo string
		userID  uint
		scope   ordercontract.TenantScope
	}{
		{order.OrderNo, 0, main},
		{order.OrderNo, f.users[1].ID, main},
		{child.OrderNo, order.UserID, main},
		{order.OrderNo, order.UserID, ordercontract.TenantScope{ResellerID: &resellerID}},
	} {
		if _, err := reserve(request.orderNo, request.userID, request.scope); !errors.Is(err, orderapp.ErrOrderNotFound) {
			t.Fatalf("unscoped reservation accepted: %+v, %v", request, err)
		}
	}
	var attempts int64
	if err := f.db.Model(&kafudadomain.Attempt{}).Count(&attempts).Error; err != nil || attempts != 0 {
		t.Fatalf("rejected attempts persisted: %d, %v", attempts, err)
	}

	const buyers = 8
	start := make(chan struct{})
	results := make(chan *kafudaapp.Reservation, buyers)
	for range buyers {
		go func() {
			<-start
			result, err := reserve(order.OrderNo, order.UserID, main)
			if err != nil {
				t.Errorf("reserve buyer attempt: %v", err)
			}
			results <- result
		}()
	}
	close(start)
	created := 0
	var attemptID uint
	for range buyers {
		result := <-results
		if result == nil {
			continue
		}
		if result.Created {
			created++
		}
		if attemptID == 0 {
			attemptID = result.Attempt.ID
		}
		if result.Attempt.ID != attemptID || result.Attempt.OrderID != child.ID || result.Recharge.OrderID != child.ID {
			t.Fatalf("unexpected reservation: %+v", result)
		}
	}
	if created != 1 {
		t.Fatalf("created %d attempts, want one", created)
	}
	if err := f.db.Model(&kafudadomain.Attempt{}).Count(&attempts).Error; err != nil || attempts != 1 {
		t.Fatalf("persisted attempts: %d, %v", attempts, err)
	}
}

type directCreateGateway struct {
	kafudacontract.Gateway
	create func(context.Context, kafudacontract.CreateOrderRequest) (*kafudacontract.OrderResult, error)
	get    func(context.Context, uint64) (*kafudacontract.OrderResult, error)
}

func (g directCreateGateway) CreateOrder(ctx context.Context, req kafudacontract.CreateOrderRequest) (*kafudacontract.OrderResult, error) {
	return g.create(ctx, req)
}

func (g directCreateGateway) GetOrder(ctx context.Context, id uint64) (*kafudacontract.OrderResult, error) {
	return g.get(ctx, id)
}

func TestSubmitCheckedDirectQuote(t *testing.T) {
	testSubmitCheckedDirectQuote(t, newDirectCheckoutFixture)
}

func testSubmitCheckedDirectQuote(t *testing.T, newFixture func(*testing.T) directCheckoutFixture) {
	t.Helper()
	for _, scenario := range []struct {
		name       string
		stage      kafudadomain.Stage
		wantCall   bool
		wantReason string
	}{
		{"region_locked", kafudadomain.StageManualDelivery, false, kafudadomain.ReasonQuoteRegionLocked},
		{"quote_expired", kafudadomain.StageManualDelivery, false, kafudadomain.ReasonQuoteExpired},
		{"quote_unavailable", kafudadomain.StageManualDelivery, false, kafudadomain.ReasonQuoteUnavailable},
		{"policy_rejected", kafudadomain.StageManualDelivery, false, kafudadomain.ReasonQuotePolicy},
		{"processing", kafudadomain.StageProcessing, true, ""},
		{"completed", kafudadomain.StageCompleted, true, ""},
		{"completed_after_sales_disabled", kafudadomain.StageCompleted, true, ""},
		{"poll_after_sales_disabled", kafudadomain.StageProcessing, true, ""},
		{"response_lost", kafudadomain.StageManualReview, true, "outcome_unknown"},
		{"response_mismatch", kafudadomain.StageManualReview, true, "outcome_unknown"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			f := newFixture(t)
			if err := f.db.AutoMigrate(&kafudadomain.Attempt{}); err != nil {
				t.Fatal(err)
			}
			order, err := orderapp.NewOrderService(f.options).CreateOrder(f.input)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			childID := order.Children[0].ID
			ids := []uint{order.ID, childID}
			if err := f.db.Model(&orderdomain.Order{}).Where("id IN ?", ids).
				Updates(map[string]any{"status": constants.OrderStatusPaid, "paid_at": now}).Error; err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if scenario.name == "completed_after_sales_disabled" || scenario.name == "poll_after_sales_disabled" {
				options := f.options
				policy := directCheckoutCommerce(f.store, false)
				options.DirectCatalog = policy
				closedCheckout := orderapp.NewOrderService(options)
				if _, err := closedCheckout.PreviewOrder(f.input); !errors.Is(err, orderapp.ErrProductNotAvailable) {
					t.Fatalf("preview with sales disabled: %v", err)
				}
				if _, err := closedCheckout.CreateOrder(f.input); !errors.Is(err, orderapp.ErrProductNotAvailable) {
					t.Fatalf("create with sales disabled: %v", err)
				}
				if revisions, err := policy.PurchasableSKURevisions(ctx); !errors.Is(err, kafudaapp.ErrQuoteUnavailable) || len(revisions) != 0 {
					t.Fatalf("storefront availability with sales disabled: %v, %v", revisions, err)
				}
				if options, err := policy.SKUOptions(ctx); err != nil || len(options) != 1 || options[0].SKUCode != kafudacontract.SKUCode("plus", "PH") {
					t.Fatalf("admin catalog with sales disabled: %v, %v", options, err)
				}
				var count int64
				if err := f.db.Model(&orderdomain.Order{}).Count(&count).Error; err != nil || count != 2 {
					t.Fatalf("disabled checkout changed persisted orders: %d, %v", count, err)
				}
			}
			records := kafudaapp.New(f.store)
			reservation, err := kafudaworkflow.ReserveBuyerAttempt(f.options.OrderStore, ordercontract.TenantScope{}, order.OrderNo, order.UserID, now, now.Add(time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			check := kafudacontract.CheckResult{
				Country: "PH", PayMode: "auto_card", RegionLocked: scenario.name == "region_locked",
				Plans: []kafudacontract.Quote{{
					Plan: "plus", Status: "available", Kind: "new", QuoteID: "quote_checkout",
					PriceCents: 900, ExpiresAt: now.Add(time.Minute),
					AutoCard: &kafudacontract.AutoCard{ProductCode: "checkout-card", OpenFeeCents: 50, LoadCents: 50, TotalCents: 1000},
				}},
			}
			switch scenario.name {
			case "quote_expired":
				check.Plans[0].ExpiresAt = now
			case "quote_unavailable":
				check.Plans[0].Status = "not_eligible"
			case "policy_rejected":
				check.Plans[0].AutoCard.ProductCode = "wrong-card"
			}
			calls := 0
			gateway := directCreateGateway{create: func(ctx context.Context, req kafudacontract.CreateOrderRequest) (*kafudacontract.OrderResult, error) {
				calls++
				attempt, err := f.store.GetAttempt(ctx, reservation.Attempt.ID)
				if err != nil || attempt == nil || attempt.Status != kafudadomain.AttemptSubmitting ||
					attempt.Reference != req.Reference || attempt.QuoteID != req.QuoteID ||
					req.Reference != reservation.Attempt.Reference || req.QuoteID != "quote_checkout" {
					t.Fatalf("create without committed attempt: %+v, %v", attempt, err)
				}
				result := &kafudacontract.OrderResult{ID: 100, Reference: req.Reference, Product: "gpt", Plan: "plus", Country: "PH", Status: "processing"}
				switch scenario.name {
				case "completed", "completed_after_sales_disabled":
					result.Status = "completed"
				case "response_lost":
					return nil, errors.New("supplier response lost")
				case "response_mismatch":
					result.Reference = "other-reference"
				}
				return result, nil
			}, get: func(_ context.Context, id uint64) (*kafudacontract.OrderResult, error) {
				if id != 100 {
					t.Fatalf("polled unexpected upstream order %d", id)
				}
				return &kafudacontract.OrderResult{ID: id, Reference: reservation.Attempt.Reference, Product: "gpt", Plan: "plus", Country: "PH", Status: "completed"}, nil
			}}
			completed, err := kafudaworkflow.SubmitCheckedQuote(ctx, f.options.OrderStore, records, gateway, reservation, check, 10*time.Second)
			wantError := scenario.stage == kafudadomain.StageManualDelivery || scenario.stage == kafudadomain.StageManualReview
			if completed != (scenario.stage == kafudadomain.StageCompleted) || (err != nil) != wantError {
				t.Fatalf("unexpected outcome: completed=%v, err=%v", completed, err)
			}
			if _, err := kafudaworkflow.SubmitCheckedQuote(ctx, f.options.OrderStore, records, gateway, reservation, check, 10*time.Second); !errors.Is(err, kafudadomain.ErrInvalidTransition) {
				t.Fatalf("stale submission replay: %v", err)
			}
			if (scenario.wantCall && calls != 1) || (!scenario.wantCall && calls != 0) {
				t.Fatalf("create called %d times", calls)
			}
			record, err := f.store.GetRecharge(ctx, childID)
			if err != nil || record == nil || record.Stage != scenario.stage || record.ReasonCode != scenario.wantReason {
				t.Fatalf("unexpected persisted recharge: %+v, %v", record, err)
			}
			if scenario.stage == kafudadomain.StageProcessing {
				attempt, err := f.store.GetAttempt(ctx, reservation.Attempt.ID)
				if err != nil || attempt == nil || attempt.NextPollAt == nil ||
					attempt.NextPollAt.Sub(attempt.UpdatedAt) != 10*time.Second {
					t.Fatalf("first lookup did not use configured interval: %+v, %v", attempt, err)
				}
			}
			if scenario.name == "poll_after_sales_disabled" {
				attempt, err := f.store.GetAttempt(ctx, reservation.Attempt.ID)
				if err != nil || attempt == nil || attempt.NextPollAt == nil {
					t.Fatalf("poll was not scheduled: %+v, %v", attempt, err)
				}
				at := *attempt.NextPollAt
				result, err := records.PollKnownOrder(ctx, gateway, attempt.ID, at, at.Add(time.Minute), 0)
				if err != nil {
					t.Fatal(err)
				}
				completed, err = kafudaworkflow.ApplyResult(f.options.OrderStore, childID, attempt.ID, *result, at, at.Add(time.Minute))
				if err != nil || !completed {
					t.Fatalf("polled order completion: completed=%t, err=%v", completed, err)
				}
			}
			if completed {
				var completedOrders int64
				if err := f.db.Model(&orderdomain.Order{}).Where("id IN ? AND status = ?", ids, constants.OrderStatusCompleted).
					Count(&completedOrders).Error; err != nil || completedOrders != 2 {
					t.Fatalf("create response did not complete both orders: %d, %v", completedOrders, err)
				}
			}
			if scenario.stage == kafudadomain.StageManualReview {
				attempt, err := f.store.GetAttempt(ctx, reservation.Attempt.ID)
				if err != nil || attempt == nil || attempt.ActiveOrderID == nil || attempt.UpstreamOrderID != nil {
					t.Fatalf("unknown outcome lost active attempt: %+v, %v", attempt, err)
				}
			}
			if scenario.name == "response_lost" {
				testReconcileUnknownDirectOrder(t, f, childID, reservation.Attempt)
			}
		})
	}
}

func testReconcileUnknownDirectOrder(t *testing.T, f directCheckoutFixture, childID uint, attempt *kafudadomain.Attempt) {
	t.Helper()
	const upstreamID = uint64(9_007_199_254_740_993)
	calls := 0
	gateway := directCreateGateway{get: func(ctx context.Context, id uint64) (*kafudacontract.OrderResult, error) {
		calls++
		if id != upstreamID {
			t.Fatalf("unexpected supplier ID: %d", id)
		}
		// This would block on the SQLite single connection inside a transaction.
		if _, err := f.store.GetAttempt(ctx, attempt.ID); err != nil {
			t.Fatal(err)
		}
		reference := attempt.Reference
		if calls == 1 {
			reference = "different-order"
		}
		return &kafudacontract.OrderResult{
			ID: id, Reference: reference, Product: "gpt", Plan: "plus", Country: "PH", Status: "processing",
		}, nil
	}}
	handler := orderwiring.New(&container.Container{
		Config:     &config.Config{KafudaGPT: config.KafudaGPTConfig{PollIntervalSeconds: 10}},
		OrderStore: f.options.OrderStore,
		KafudaGPT:  &kafudawiring.Module{Store: f.store, Gateway: gateway},
	}).Admin
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("admin_id", uint(9)) })
	orderhttp.RegisterAdminRoutes(router.Group("/admin"), handler)
	request := func(id uint, success bool) {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/admin/orders/%d/direct-reconcile", childID),
			strings.NewReader(fmt.Sprintf(`{"attempt_id":%d,"upstream_order_id":"%d"}`, id, upstreamID)))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(rec, req)
		if strings.Contains(rec.Body.String(), `"status_code":0`) != success {
			t.Fatalf("reconciliation response: %s", rec.Body.String())
		}
	}
	request(attempt.ID+1, false)
	request(attempt.ID, false)
	stored, err := f.store.GetAttempt(context.Background(), attempt.ID)
	if err != nil || stored == nil || stored.UpstreamOrderID != nil || stored.Status != kafudadomain.AttemptReview {
		t.Fatalf("mismatch changed the original attempt: %+v, %v", stored, err)
	}
	request(attempt.ID, true)
	request(attempt.ID, false)
	stored, err = f.store.GetAttempt(context.Background(), attempt.ID)
	if err != nil || stored == nil || stored.UpstreamOrderID == nil || *stored.UpstreamOrderID != upstreamID ||
		stored.NextPollAt == nil || stored.NextPollAt.Sub(stored.UpdatedAt) != 10*time.Second ||
		stored.Status != kafudadomain.AttemptProcessing || calls != 2 {
		t.Fatalf("verified ID did not restore scheduled lookups: %+v, %v, calls=%d", stored, err, calls)
	}
}

func TestBuyerDirectStatusIsScopedAndReadOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)
	f := newDirectCheckoutFixture(t)
	service := orderapp.NewOrderService(f.options)
	order, err := service.CreateOrder(f.input)
	if err != nil || order == nil || len(order.Children) != 1 {
		t.Fatalf("create checkout: %+v, %v", order, err)
	}
	childID := order.Children[0].ID
	handler := orderwiring.New(&container.Container{
		OrderService: service, OrderStore: f.options.OrderStore,
		KafudaGPT: &kafudawiring.Module{Store: f.store},
	}).User
	const userJWTSecret = "orders-direct-status-integration-test-key"
	auth := userauthapp.NewService(&config.Config{UserJWT: config.JWTConfig{SecretKey: userJWTSecret}}, f.options.UserStore, nil, nil, nil, nil, nil)
	tokenFor := func(userIndex int, stale bool) string {
		t.Helper()
		user := f.users[userIndex]
		if stale {
			user.TokenVersion++
		}
		token, _, err := auth.GenerateUserJWT(&user, 1)
		if err != nil {
			t.Fatalf("generate test user token: %v", err)
		}
		return token
	}
	ownerToken, otherToken, staleToken := tokenFor(0, false), tokenFor(1, false), tokenFor(0, true)
	const resellerID = uint(1)
	resellerToken, err := auth.BindUserTokenTenant(ownerToken, resellerID)
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	router.Use(middleware.UserJWTAuthMiddleware(userJWTSecret, f.options.UserStore))
	orderhttp.RegisterUserReadRoutes(router, handler)
	request := func(orderNo, token string, tenant resellercontract.TenantContext, wantCode int, wantStage, wantRenewal string) {
		t.Helper()
		ctx := resellercontract.WithTenantContext(context.Background(), tenant)
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/orders/"+orderNo+"/direct-status", nil).WithContext(ctx)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		router.ServeHTTP(recorder, req)
		var responseBody struct {
			Code int                        `json:"status_code"`
			Data map[string]json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &responseBody); err != nil || responseBody.Code != wantCode {
			t.Fatalf("status response: %s, %v; expected %d", recorder.Body.String(), err, wantCode)
		}
		if wantStage != "" {
			wantFields := 2
			if wantRenewal != "" {
				wantFields++
			}
			if len(responseBody.Data) != wantFields || string(responseBody.Data["stage"]) != fmt.Sprintf("%q", wantStage) ||
				len(responseBody.Data["updated_at"]) == 0 || string(responseBody.Data["renewal_cancelled"]) != wantRenewal {
				t.Fatalf("unexpected buyer projection: %s", recorder.Body.String())
			}
		}
	}
	main := resellercontract.MainTenantContext("shop.example")
	request(order.OrderNo, ownerToken, main, response.CodeNotFound, "", "")
	now := time.Now().UTC()
	if err := f.db.Model(&orderdomain.Order{}).Where("id IN ?", []uint{order.ID, childID}).Updates(map[string]any{"paid_at": now, "status": constants.OrderStatusPaid}).Error; err != nil {
		t.Fatal(err)
	}
	request(order.OrderNo, "", main, response.CodeUnauthorized, "", "")
	request(order.OrderNo, otherToken, main, response.CodeNotFound, "", "")
	reseller := resellercontract.ResellerTenantContext("reseller.example", resellerID, 1, "")
	request(order.OrderNo, ownerToken, reseller, response.CodeUnauthorized, "", "")
	request(order.OrderNo, resellerToken, main, response.CodeUnauthorized, "", "")
	request(order.OrderNo, resellerToken, reseller, response.CodeNotFound, "", "")
	request(order.OrderNo, staleToken, main, response.CodeUnauthorized, "", "")
	request(order.OrderNo, "not-a-token", main, response.CodeUnauthorized, "", "")
	request(order.Children[0].OrderNo, ownerToken, main, response.CodeNotFound, "", "")
	for _, stage := range []kafudadomain.Stage{
		kafudadomain.StageWaiting, kafudadomain.StageChecking, kafudadomain.StageSubmitting, kafudadomain.StageProcessing,
		kafudadomain.StageManualReview, kafudadomain.StageManualDelivery, kafudadomain.StageCompleted,
	} {
		if stage == kafudadomain.StageCompleted {
			if err := f.db.Model(&orderdomain.Order{}).Where("id IN ?", []uint{order.ID, childID}).Update("status", constants.OrderStatusCompleted).Error; err != nil {
				t.Fatal(err)
			}
		}
		if err := f.db.Model(&kafudadomain.Recharge{}).Where("order_id = ?", childID).
			Updates(map[string]any{"stage": stage, "reason_code": "private-reason", "version": 5}).Error; err != nil {
			t.Fatal(err)
		}
		wantStage := stage
		if stage == kafudadomain.StageManualDelivery {
			wantStage = kafudadomain.StageManualReview
		}
		request(order.OrderNo, ownerToken, main, 0, string(wantStage), "")
		record, err := f.store.GetRecharge(context.Background(), childID)
		if err != nil || record == nil || record.Version != 5 {
			t.Fatalf("status read changed the recharge: %+v, %v", record, err)
		}
	}
	if err := f.db.AutoMigrate(&kafudadomain.Attempt{}); err != nil {
		t.Fatal(err)
	}
	upstreamID := uint64(123)
	renewalCancelled := false
	attempt := kafudadomain.Attempt{
		OrderID: childID, Sequence: 1, Reference: "kgpt-renewal-test", Status: kafudadomain.AttemptCompleted,
		UpstreamOrderID: &upstreamID, RenewalCancelled: &renewalCancelled,
		MaskedEmail: "m***@example.com",
	}
	if err := f.db.Create(&attempt).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Model(&kafudadomain.Recharge{}).Where("order_id = ?", childID).Update("current_attempt_id", attempt.ID).Error; err != nil {
		t.Fatal(err)
	}
	request(order.OrderNo, ownerToken, main, 0, "completed", "false")
	if err := f.db.Model(&attempt).Update("renewal_cancelled", true).Error; err != nil {
		t.Fatal(err)
	}
	request(order.OrderNo, ownerToken, main, 0, "completed", "true")
	if err := f.db.Model(&attempt).Update("renewal_cancelled", nil).Error; err != nil {
		t.Fatal(err)
	}
	request(order.OrderNo, ownerToken, main, 0, "completed", "")
	if err := f.db.Model(&attempt).Update("renewal_cancelled", true).Error; err != nil {
		t.Fatal(err)
	}
	record, err := f.store.GetRecharge(context.Background(), childID)
	if err != nil {
		t.Fatal(err)
	}
	record.Snapshot.Product = "gpt_credits"
	if err := f.db.Model(&kafudadomain.Recharge{}).Where("order_id = ?", childID).
		Select("snapshot").Updates(record).Error; err != nil {
		t.Fatal(err)
	}
	request(order.OrderNo, ownerToken, main, 0, "completed", "")
	if err := f.db.Model(&orderdomain.Order{}).Where("id IN ?", []uint{order.ID, childID}).
		Update("status", constants.OrderStatusRefunded).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Model(&orderdomain.Order{}).Where("id = ?", order.ID).
		Update("refunded_amount", order.TotalAmount).Error; err != nil {
		t.Fatal(err)
	}
	request(order.OrderNo, ownerToken, main, 0, "refunded", "")
	request(order.OrderNo, otherToken, main, response.CodeNotFound, "", "")
}

func TestControlledDirectManualCompletionRequiresTerminalAttempt(t *testing.T) {
	t.Run("http", func(t *testing.T) { testControlledDirectManualAction(t, "direct-complete", false) })
	t.Run("notification", func(t *testing.T) { testControlledDirectManualAction(t, "direct-complete", true) })
}

type directCompletionEmailQueue struct {
	ordercontract.Queue
	t        *testing.T
	store    ordercontract.Store
	parentID uint
	calls    int
}

func (q *directCompletionEmailQueue) Enabled() bool { return true }

func (q *directCompletionEmailQueue) EnqueueStatusEmail(orderID uint, status string) error {
	q.t.Helper()
	q.calls++
	parent, err := q.store.GetByID(orderID)
	if err != nil || parent == nil || parent.Status != constants.OrderStatusCompleted ||
		orderID != q.parentID || status != constants.OrderStatusCompleted {
		q.t.Fatalf("notification did not observe committed parent: %d, %s, %v", orderID, status, err)
	}
	return nil
}

func TestControlledDirectManualReleaseRequiresTerminalAttempt(t *testing.T) {
	testControlledDirectManualAction(t, "direct-release", false)
}

func testControlledDirectManualAction(t *testing.T, action string, notify bool) {
	f := newDirectCheckoutFixture(t)
	if err := f.db.AutoMigrate(&kafudadomain.Attempt{}); err != nil {
		t.Fatal(err)
	}
	order, err := orderapp.NewOrderService(f.options).CreateOrder(f.input)
	if err != nil {
		t.Fatal(err)
	}
	ctx, now := context.Background(), time.Now().UTC()
	childID := order.Children[0].ID
	ids := []uint{order.ID, childID}
	if err := f.db.Model(&orderdomain.Order{}).Where("id IN ?", ids).
		Updates(map[string]any{"status": constants.OrderStatusPaid, "paid_at": now}).Error; err != nil {
		t.Fatal(err)
	}
	records := kafudaapp.New(f.store)
	reserved, err := records.Reserve(ctx, childID, now, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	id := reserved.Attempt.ID
	if err := records.MarkSubmitting(ctx, id, "quote", now, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := records.MarkUnknown(ctx, id, now, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	const adminID = 9
	emailQueue := &directCompletionEmailQueue{t: t, store: f.options.OrderStore, parentID: order.ID}
	apply := func(attemptID uint, confirmed bool) error {
		if action == "direct-release" {
			return kafudaworkflow.ReleaseManual(f.options.OrderStore, childID, attemptID, adminID, confirmed, now)
		}
		if notify {
			return kafudaworkflow.CompleteManualAndNotify(ctx, f.options.OrderStore, f.store, emailQueue, nil, nil, childID, attemptID, adminID, confirmed, now)
		}
		return kafudaworkflow.CompleteManual(f.options.OrderStore, childID, attemptID, adminID, confirmed, now)
	}
	if err := apply(id, true); !errors.Is(err, orderapp.ErrOrderStatusInvalid) {
		t.Fatalf("unknown attempt allowed manual completion: %v", err)
	}
	result := kafudacontract.OrderResult{ID: 101, Reference: reserved.Attempt.Reference, Product: "gpt", Plan: "plus", Country: "PH", Status: "failed"}
	if _, err := kafudaworkflow.ApplyResult(f.options.OrderStore, childID, id, result, now, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := apply(id, false); !errors.Is(err, orderapp.ErrOrderStatusInvalid) {
		t.Fatalf("missing confirmation allowed manual completion: %v", err)
	}
	if err := apply(id+1, true); !errors.Is(err, orderapp.ErrOrderStatusInvalid) {
		t.Fatalf("wrong attempt allowed manual completion: %v", err)
	}
	if notify {
		if err := apply(id, true); err != nil {
			t.Fatal(err)
		}
	} else {
		handler := orderwiring.New(&container.Container{OrderStore: f.options.OrderStore}).Admin
		router := gin.New()
		router.Use(func(c *gin.Context) { c.Set("admin_id", uint(adminID)) })
		orderhttp.RegisterAdminRoutes(router.Group("/admin"), handler)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/admin/orders/%d/%s", childID, action),
			strings.NewReader(fmt.Sprintf(`{"attempt_id":%d,"confirmed":true,"admin_id":999}`, id)))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(rec, req)
		if !strings.Contains(rec.Body.String(), `"status_code":0`) {
			t.Fatalf("direct completion endpoint: %s", rec.Body.String())
		}
	}
	if err := apply(id, true); !errors.Is(err, orderapp.ErrOrderStatusInvalid) {
		t.Fatalf("manual completion replay accepted: %v", err)
	}
	if action == "direct-release" {
		record, err := f.store.GetRecharge(ctx, childID)
		if err != nil || record == nil || record.Stage != kafudadomain.StageWaiting || record.CurrentAttemptID == nil || *record.CurrentAttemptID != id || record.CompletedAt != nil {
			t.Fatalf("release did not preserve attempt history: %+v, %v", record, err)
		}
		var paidOrders, deliveries int64
		if err := f.db.Model(&orderdomain.Order{}).Where("id IN ? AND status = ?", ids, constants.OrderStatusPaid).Count(&paidOrders).Error; err != nil || paidOrders != 2 {
			t.Fatalf("release changed paid orders: %d, %v", paidOrders, err)
		}
		if err := f.db.Model(&fulfillmentdomain.Fulfillment{}).Where("order_id = ?", childID).Count(&deliveries).Error; err != nil || deliveries != 0 {
			t.Fatalf("release created delivery: %d, %v", deliveries, err)
		}
		retryAt := time.Now().UTC()
		retry, err := records.Reserve(ctx, childID, retryAt, retryAt.Add(time.Minute))
		if err != nil || retry.Attempt.Sequence != 2 || retry.Attempt.Reference == reserved.Attempt.Reference {
			t.Fatalf("release did not allow a fresh attempt: %+v, %v", retry, err)
		}
		return
	}
	if notify && emailQueue.calls != 1 {
		t.Fatalf("manual completion and replay queued %d emails, want one", emailQueue.calls)
	}
	var delivery fulfillmentdomain.Fulfillment
	if err := f.db.Where("order_id = ?", childID).First(&delivery).Error; err != nil || delivery.DeliveredBy == nil || *delivery.DeliveredBy != adminID || delivery.Payload != "" {
		t.Fatalf("invalid manual delivery audit: %+v, %v", delivery, err)
	}
	for _, orderID := range ids {
		var stored orderdomain.Order
		if err := f.db.First(&stored, orderID).Error; err != nil || stored.Status != constants.OrderStatusCompleted {
			t.Fatalf("manual completion did not update order %d: %+v, %v", orderID, stored, err)
		}
	}
	record, err := f.store.GetRecharge(ctx, childID)
	if err != nil || record == nil || record.Stage != kafudadomain.StageCompleted || record.CompletedAt == nil {
		t.Fatalf("manual completion did not update recharge: %+v, %v", record, err)
	}
}

func TestDirectShopCompletionUpdatesParentAndChildAtomically(t *testing.T) {
	f := newDirectCheckoutFixture(t)
	if err := f.db.AutoMigrate(&kafudadomain.Attempt{}); err != nil {
		t.Fatal(err)
	}
	order, err := orderapp.NewOrderService(f.options).CreateOrder(f.input)
	if err != nil || order == nil || len(order.Children) != 1 {
		t.Fatalf("create checkout: %+v, %v", order, err)
	}
	ctx, now := context.Background(), time.Now().UTC()
	childID := order.Children[0].ID
	records := kafudaapp.New(f.store)
	reserved, err := records.Reserve(ctx, childID, now, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	attempt := reserved.Attempt
	if err := records.MarkSubmitting(ctx, attempt.ID, "quote_1", now, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	result := kafudacontract.OrderResult{ID: 101, Reference: attempt.Reference, Product: "gpt", Plan: "plus", Country: "PH", Status: "completed"}
	apply := func() (bool, error) {
		return kafudaworkflow.ApplyResult(f.options.OrderStore, childID, attempt.ID, result, now, time.Time{})
	}
	if completed, err := apply(); completed || !errors.Is(err, orderapp.ErrOrderStatusInvalid) {
		t.Fatalf("unpaid completion: %t, %v", completed, err)
	}
	if err := f.db.Model(&orderdomain.Order{}).Where("id IN ?", []uint{order.ID, childID}).Updates(map[string]any{"status": constants.OrderStatusPaid, "paid_at": now}).Error; err != nil {
		t.Fatal(err)
	}
	trigger := fmt.Sprintf("CREATE TRIGGER reject_parent_completion BEFORE UPDATE ON orders WHEN OLD.id = %d AND NEW.status = 'completed' BEGIN SELECT RAISE(ABORT, 'parent update failed'); END", order.ID)
	if err := f.db.Exec(trigger).Error; err != nil {
		t.Fatal(err)
	}
	if completed, err := apply(); completed || err == nil {
		t.Fatalf("parent write failure was not returned: %t, %v", completed, err)
	}
	for _, id := range []uint{order.ID, childID} {
		var row orderdomain.Order
		if err := f.db.First(&row, id).Error; err != nil || row.Status != constants.OrderStatusPaid {
			t.Fatalf("order %d did not roll back: %+v, %v", id, row, err)
		}
	}
	record, err := f.store.GetRecharge(ctx, childID)
	if err != nil || record == nil || record.Stage != kafudadomain.StageSubmitting {
		t.Fatalf("recharge did not roll back: %+v, %v", record, err)
	}
	var deliveries int64
	if err := f.db.Model(&fulfillmentdomain.Fulfillment{}).Count(&deliveries).Error; err != nil || deliveries != 0 {
		t.Fatalf("delivery was not rolled back: %d, %v", deliveries, err)
	}
	if err := f.db.Exec("DROP TRIGGER reject_parent_completion").Error; err != nil {
		t.Fatal(err)
	}
	for _, want := range []bool{true, false} {
		if completed, err := apply(); completed != want || err != nil {
			t.Fatalf("fresh completion %t, %v; want %t", completed, err, want)
		}
	}
	var delivery fulfillmentdomain.Fulfillment
	if err := f.db.Where("order_id = ?", childID).First(&delivery).Error; err != nil ||
		delivery.Type != constants.FulfillmentTypeKafudaGPT || delivery.Status != constants.FulfillmentStatusDelivered ||
		delivery.Payload != "" || delivery.DeliveredBy != nil || delivery.DeliveredAt == nil || !delivery.DeliveredAt.Equal(now) {
		t.Fatalf("unexpected supplier delivery: %+v, %v", delivery, err)
	}
	if err := f.db.Model(&fulfillmentdomain.Fulfillment{}).Count(&deliveries).Error; err != nil || deliveries != 1 {
		t.Fatalf("delivery count after replay: %d, %v", deliveries, err)
	}
	for _, id := range []uint{order.ID, childID} {
		var row orderdomain.Order
		if err := f.db.First(&row, id).Error; err != nil || row.Status != constants.OrderStatusCompleted {
			t.Fatalf("order %d did not complete: %+v, %v", id, row, err)
		}
	}
}

func TestDirectCheckoutDiscountsMatchPreviewAndSnapshot(t *testing.T) {
	for _, scenario := range []struct {
		name                                     string
		promotion, wholesale, floor              int64
		memberRate, exclusive                    int64
		total, promoOff, wholesaleOff, memberOff string
	}{
		{"promotion", 10, 0, 0, 0, 0, "90.00", "10.00", "0.00", "0.00"},
		{"wholesale_at_one", 10, 85, 1, 0, 0, "85.00", "0.00", "15.00", "0.00"},
		{"wholesale_above_one", 10, 70, 2, 0, 0, "90.00", "10.00", "0.00", "0.00"},
		{"member_after_promotion", 10, 0, 0, 80, 0, "72.00", "10.00", "0.00", "18.00"},
		{"exclusive_wins", 10, 85, 1, 80, 60, "60.00", "0.00", "0.00", "40.00"},
		{"exclusive_loses", 10, 85, 1, 80, 75, "68.00", "0.00", "15.00", "17.00"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			f := newDirectCheckoutFixture(t)
			amount := func(value int64) money.Amount { return money.FromDecimal(decimal.NewFromInt(value)) }
			if err := f.db.AutoMigrate(&memberdomain.MemberLevel{}, &memberdomain.MemberLevelPrice{}, &memberdomain.UserProductPrice{}); err != nil {
				t.Fatal(err)
			}
			promotion := promotiondomain.Promotion{
				Name: "direct discount", ScopeType: constants.ScopeTypeProduct, ScopeRefID: f.product.ID,
				Type: constants.PromotionTypeFixed, Value: amount(scenario.promotion), IsActive: true,
			}
			if err := f.db.Create(&promotion).Error; err != nil {
				t.Fatal(err)
			}
			if scenario.wholesale > 0 {
				tiers := productdomain.WholesalePriceTiers{{MinQuantity: int(scenario.floor), UnitPrice: amount(scenario.wholesale)}}
				if err := f.db.Model(&f.product).Update("wholesale_prices", tiers).Error; err != nil {
					t.Fatal(err)
				}
			}
			if scenario.memberRate > 0 {
				level := memberdomain.MemberLevel{Slug: "direct-member", NameJSON: jsonmap.JSON{"en-US": "Member"}, DiscountRate: amount(scenario.memberRate)}
				if err := f.db.Create(&level).Error; err != nil {
					t.Fatal(err)
				}
				if err := f.db.Model(&f.users[0]).Update("member_level_id", level.ID).Error; err != nil {
					t.Fatal(err)
				}
			}
			if scenario.exclusive > 0 {
				price := memberdomain.UserProductPrice{UserID: f.users[0].ID, ProductID: f.product.ID, SKUID: f.sku.ID, PriceAmount: amount(scenario.exclusive)}
				if err := f.db.Create(&price).Error; err != nil {
					t.Fatal(err)
				}
			}
			f.options.MemberLevelService = memberapp.NewService(memberstore.NewLevelStore(f.db), memberstore.NewPriceStore(f.db), memberstore.NewUserStore(f.db), memberstore.NewUserPriceStore(f.db))
			svc := orderapp.NewOrderService(f.options)
			preview, err := svc.PreviewOrder(f.input)
			if err != nil || preview.TotalAmount.String() != scenario.total {
				t.Fatalf("expected preview total %s, got %+v, %v", scenario.total, preview, err)
			}
			if preview.PromotionDiscountAmount.String() != scenario.promoOff ||
				preview.WholesaleDiscountAmount.String() != scenario.wholesaleOff || preview.MemberDiscountAmount.String() != scenario.memberOff {
				t.Fatalf("unexpected preview discount breakdown: %+v", preview)
			}
			assertNoDirectCheckoutRows(t, f)
			order, err := svc.CreateOrder(f.input)
			if err != nil || len(order.Children) != 1 {
				t.Fatalf("checkout failed: %+v, %v", order, err)
			}
			child := order.Children[0]
			for _, row := range []orderdomain.Order{*order, child} {
				if row.TotalAmount.String() != scenario.total || row.PromotionDiscountAmount.String() != scenario.promoOff ||
					row.WholesaleDiscountAmount.String() != scenario.wholesaleOff || row.MemberDiscountAmount.String() != scenario.memberOff {
					t.Fatalf("unexpected discount breakdown: %+v", row)
				}
			}
			record, err := f.store.GetRecharge(context.Background(), child.ID)
			if err != nil || record == nil || record.Snapshot.SellPrice.String() != scenario.total || record.Snapshot.CostPrice.String() != "45.00" {
				t.Fatalf("unexpected checkout snapshot: %+v, %v", record, err)
			}
		})
	}
}

func newDirectCheckoutFixture(t *testing.T) directCheckoutFixture {
	t.Helper()
	return seedDirectCheckoutFixture(t, newCouponLimitRaceFixture(t, "direct-checkout", 0, 0))
}

func seedDirectCheckoutFixture(t *testing.T, f couponLimitRaceFixture) directCheckoutFixture {
	t.Helper()
	if err := f.db.AutoMigrate(&kafudadomain.Catalog{}, &kafudadomain.Recharge{}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	catalog := &kafudadomain.Catalog{Profile: "checkout-profile", Enabled: true, SyncedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(time.Hour), Options: []kafudadomain.CatalogOption{{Plan: "plus", Country: "PH", Product: "gpt", Available: true}}, Cards: []kafudadomain.CardProduct{{ProductCode: "checkout-card"}}}
	store := kafudastore.New(f.db)
	if err := store.SaveCatalog(context.Background(), catalog); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Model(&f.product).Updates(map[string]interface{}{"fulfillment_type": constants.FulfillmentTypeKafudaGPT, "manual_stock_total": 0, "manual_form_schema_json": jsonmap.JSON{"legacy": "must-not-be-used"}}).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Model(&f.sku).Updates(map[string]interface{}{"sku_code": kafudacontract.SKUCode("plus", "PH"), "spec_values_json": jsonmap.JSON{"plan": "plus", "country": "PH", "merchant_cost": "private-cost"}, "direct_cost_configured": true, "cost_price_amount": "45", "manual_stock_total": 0}).Error; err != nil {
		t.Fatal(err)
	}
	policy := directCheckoutCommerce(store, true)
	options := orderapp.OrderServiceOptions{DirectCatalog: policy, OrderStore: orderstore.New(f.db, "test-guest-credential-secret-with-32-bytes"), UserStore: userstore.New(f.db), ProductStore: productstore.NewProductStore(f.db), ProductSKUStore: productstore.NewSKUStore(f.db), CouponStore: f.couponRepo, CouponUsageStore: f.usageRepo, PromotionRepo: promotionstore.New(f.db), Queue: f.queue}
	input := f.input(f.users[0].ID)
	input.CouponCode = ""
	input.StorefrontRequest = true
	input.Tenant = resellercontract.MainTenantContext("shop.example")
	input.ManualFormData = map[string]jsonmap.JSON{fmt.Sprint(f.product.ID): {"session": "session-must-not-persist"}}
	return directCheckoutFixture{couponLimitRaceFixture: f, options: options, input: input, catalog: catalog, store: store}
}
func directCheckoutCommerce(store *kafudastore.Store, salesEnabled bool) *kafudaapp.CommerceService {
	return kafudaapp.NewCommerceService(kafudaapp.CommerceOptions{
		Catalog:      kafudaapp.NewCatalogService(store, nil, "checkout-profile", time.Hour),
		SalesEnabled: salesEnabled, ReferenceLookupVerified: true, ProductCode: "checkout-card",
		MaxCostCents: 2500, AllowedKinds: []string{"new", "renew"}, WorkerReady: func(context.Context) bool { return true },
	})
}

func assertNoDirectCheckoutRows(t *testing.T, f directCheckoutFixture) {
	t.Helper()
	for _, model := range []interface{}{&orderdomain.Order{}, &orderdomain.OrderItem{}, &kafudadomain.Recharge{}, &coupondomain.CouponUsage{}} {
		var count int64
		if err := f.db.Model(model).Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("failed checkout left %T rows", model)
		}
	}
	if f.queue.enqueued.Load() != 0 {
		t.Fatal("failed checkout enqueued timeout work")
	}
}
func assertDirectCheckoutPrivate(t *testing.T, raw []byte) {
	t.Helper()
	for _, secret := range []string{"checkout-profile", "checkout-card", "merchant_cost", "private-cost", "cost_price", "max_cost", "45.00", "session-must-not-persist", "legacy"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("buyer response leaked %s: %s", secret, raw)
		}
	}
}
func TestCreateDirectCheckoutPersistsImmutableChildSnapshot(t *testing.T) {
	for _, scenario := range []string{"plain", "coupon", "zero_cost"} {
		t.Run(scenario, func(t *testing.T) {
			f := newDirectCheckoutFixture(t)
			if scenario == "coupon" {
				f.input.CouponCode = f.coupon.Code
			}
			if scenario == "zero_cost" {
				if err := f.db.Model(&f.sku).Update("cost_price_amount", 0).Error; err != nil {
					t.Fatal(err)
				}
			}
			svc := orderapp.NewOrderService(f.options)
			order, err := svc.CreateOrder(f.input)
			if err != nil || order == nil || len(order.Children) != 1 {
				t.Fatalf("checkout failed: %+v %v", order, err)
			}
			child := order.Children[0]
			record, err := f.store.GetRecharge(context.Background(), child.ID)
			if err != nil || record == nil || record.OrderID == order.ID || record.Stage != kafudadomain.StageWaiting || record.Version != 1 {
				t.Fatalf("missing child snapshot: %+v, %v", record, err)
			}
			wantPrice := "100.00"
			if scenario == "coupon" {
				wantPrice = "90.00"
			}
			wantCost := "45.00"
			if scenario == "zero_cost" {
				wantCost = "0.00"
			}
			if record.Snapshot.SellPrice.String() != wantPrice || record.Snapshot.CostPrice.String() != wantCost || record.Snapshot.Plan != "plus" || record.Snapshot.Country != "PH" || record.Snapshot.PaymentProfile != "checkout-profile" || record.Snapshot.ProductCode != "checkout-card" || record.Snapshot.MaxCostCents != 2500 || !reflect.DeepEqual(record.Snapshot.AllowedKinds, []string{"new", "renew"}) {
				t.Fatalf("incorrect commercial snapshot: %+v", record.Snapshot)
			}
			if len(child.Items) != 1 || len(child.Items[0].ManualFormSchemaSnapshotJSON) != 0 || len(child.Items[0].ManualFormSubmissionJSON) != 0 || child.Items[0].FulfillmentType != constants.FulfillmentTypeKafudaGPT {
				t.Fatal("checkout used manual fulfillment or form")
			}
			original := record.Snapshot
			raw, _ := json.Marshal(orderpresenter.NewOrderDetail(order))
			assertDirectCheckoutPrivate(t, raw)
			// Paid order snapshots survive later changes to live prices and catalog state.
			if err := f.db.Model(&orderdomain.Order{}).Where("id IN ?", []uint{order.ID, child.ID}).Update("status", constants.OrderStatusPaid).Error; err != nil {
				t.Fatal(err)
			}
			if err := f.db.Model(&f.sku).Updates(map[string]interface{}{"price_amount": "999", "cost_price_amount": "888", "is_active": false}).Error; err != nil {
				t.Fatal(err)
			}
			f.catalog.Enabled = false
			f.catalog.SyncedAt = time.Now()
			if err := f.store.SaveCatalog(context.Background(), f.catalog); err != nil {
				t.Fatal(err)
			}
			record.Snapshot.SellPrice = money.FromDecimal(decimal.NewFromInt(777))
			record.Snapshot.AllowedKinds = []string{"modified"}
			if err := f.store.SaveRecharge(record); err != nil {
				t.Fatal(err)
			}
			reloaded, err := f.store.GetRecharge(context.Background(), child.ID)
			if err != nil || !reflect.DeepEqual(original, reloaded.Snapshot) {
				t.Fatalf("live edits rewrote paid snapshot: %+v, %v", reloaded, err)
			}
			persisted, err := f.options.OrderStore.GetByID(order.ID)
			if err != nil || persisted.Children[0].TotalAmount.String() != wantPrice || persisted.Children[0].Items[0].OriginalUnitPrice.String() != "100.00" {
				t.Fatal("live edits changed order prices")
			}
			var sku productdomain.ProductSKU
			if err := f.db.First(&sku, f.sku.ID).Error; err != nil {
				t.Fatal(err)
			}
			if sku.ManualStockTotal != 0 || sku.ManualStockLocked != 0 || sku.ManualStockSold != 0 || f.queue.enqueued.Load() != 1 {
				t.Fatal("Direct checkout changed inventory or enqueued duplicate timeout work")
			}
		})
	}
}

func TestDirectCheckoutRechecksChangesAfterPreflight(t *testing.T) {
	for _, scenario := range []string{"sku_price", "sku_cost", "sku_disabled", "product_disabled", "cost_confirmation", "sku_revision", "sku_plan", "sku_code", "mapped", "type", "quantity_limit", "catalog_expired", "catalog_unlisted", "catalog_restored", "card_restricted", "catalog_product", "catalog_deleted", "snapshot_failure", "coupon_failure"} {
		t.Run(scenario, func(t *testing.T) {
			f := newDirectCheckoutFixture(t)
			if scenario == "coupon_failure" {
				f.input.CouponCode = f.coupon.Code
			}
			base := f.options.OrderStore
			f.options.OrderStore = orderProductChangeStore{Store: base, before: func() {
				var err error
				switch scenario {
				case "sku_price":
					err = f.db.Model(&f.sku).Update("price_amount", "101").Error
				case "sku_cost":
					err = f.db.Model(&f.sku).Update("cost_price_amount", "46").Error
				case "sku_disabled":
					err = f.db.Model(&f.sku).Update("is_active", false).Error
				case "product_disabled":
					err = f.db.Model(&f.product).Update("is_active", false).Error
				case "cost_confirmation":
					err = f.db.Model(&f.sku).Update("direct_cost_configured", false).Error
				case "sku_revision":
					err = f.db.Model(&f.sku).Update("direct_catalog_revision", 1).Error
				case "sku_plan":
					err = f.db.Model(&f.sku).Update("spec_values_json", jsonmap.JSON{"plan": "pro", "country": "PH"}).Error
				case "sku_code":
					err = f.db.Model(&f.sku).Update("sku_code", "changed").Error
				case "mapped":
					err = f.db.Model(&f.product).Update("is_mapped", true).Error
				case "type":
					err = f.db.Model(&f.product).Update("fulfillment_type", constants.FulfillmentTypeManual).Error
				case "quantity_limit":
					err = f.db.Model(&f.product).Update("min_purchase_quantity", 2).Error
				case "catalog_deleted":
					err = f.db.Where("profile = ?", f.catalog.Profile).Delete(&kafudadomain.Catalog{}).Error
				case "snapshot_failure", "coupon_failure":
					err = f.db.Callback().Create().Before("gorm:create").Register("reject_recharge", func(tx *gorm.DB) {
						if tx.Statement.Table == map[string]string{"snapshot_failure": "kafuda_gpt_recharges", "coupon_failure": (coupondomain.CouponUsage{}).TableName()}[scenario] {
							tx.AddError(errors.New("injected snapshot failure"))
						}
					})
				default:
					f.catalog.SyncedAt = time.Now().Add(-time.Hour)
					switch scenario {
					case "catalog_expired":
						f.catalog.ExpiresAt = time.Now().Add(-time.Minute)
					case "catalog_unlisted", "catalog_restored":
						f.catalog.Options[0].Available = false
					case "card_restricted":
						f.catalog.Cards[0].Restricted = true
					case "catalog_product":
						f.catalog.Options[0].Product = "gpt_credits"
					}
					err = f.store.SaveCatalog(context.Background(), f.catalog)
					if err == nil && scenario == "catalog_restored" {
						f.catalog.Options[0].Available = true
						f.catalog.SyncedAt = time.Now()
						err = f.store.SaveCatalog(context.Background(), f.catalog)
					}
				}
				if err != nil {
					t.Fatal(err)
				}
			}}
			_, err := orderapp.NewOrderService(f.options).CreateOrder(f.input)
			want := orderapp.ErrProductNotAvailable
			if scenario == "snapshot_failure" || scenario == "coupon_failure" {
				want = orderapp.ErrOrderCreateFailed
			}
			if !errors.Is(err, want) {
				t.Fatalf("expected %v, got %v", want, err)
			}
			assertNoDirectCheckoutRows(t, f)
			var sku productdomain.ProductSKU
			if err := f.db.First(&sku, f.sku.ID).Error; err != nil {
				t.Fatal(err)
			}
			if sku.ManualStockLocked != 0 || sku.ManualStockSold != 0 {
				t.Fatal("failed checkout touched inventory")
			}
		})
	}
}

func TestDirectCheckoutConcurrentRequestsEachOwnOneSnapshot(t *testing.T) {
	f := newDirectCheckoutFixture(t)
	svc := orderapp.NewOrderService(f.options)
	results := make(chan error, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Add(1)
		go func() { defer workers.Done(); _, err := svc.CreateOrder(f.input); results <- err }()
	}
	workers.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	var records []kafudadomain.Recharge
	if err := f.db.Find(&records).Error; err != nil {
		t.Fatal(err)
	}
	var parents, children int64
	if err := f.db.Model(&orderdomain.Order{}).Where("parent_id IS NULL").Count(&parents).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Model(&orderdomain.Order{}).Where("parent_id IS NOT NULL").Count(&children).Error; err != nil {
		t.Fatal(err)
	}
	if parents != 2 || children != 2 || len(records) != 2 || records[0].OrderID == records[1].OrderID || f.queue.enqueued.Load() != 2 {
		t.Fatal("distinct checkout orders did not receive independent unique snapshots")
	}
}

func TestDirectCheckoutMainHTTPStoresPrivateSnapshot(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, locale := range []string{"zh-CN", "zh-TW", "en-US"} {
		t.Run(locale, func(t *testing.T) {
			f := newDirectCheckoutFixture(t)
			svc := orderapp.NewOrderService(f.options)
			payments := paymentapp.NewPaymentService(paymentapp.PaymentServiceOptions{ProductRepo: productstore.NewProductStore(f.db)})
			handlers := orderwiring.New(&container.Container{OrderService: svc, PaymentService: payments})
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			body := fmt.Sprintf(`{"items":[{"product_id":%d,"sku_id":%d,"quantity":1,"fulfillment_type":"auto"}],"cost_price":"0","plan":"pro","payment_profile":"forged","storefront_request":false,"manual_form_data":{"%d":{"session":"session-must-not-persist"}}}`, f.product.ID, f.sku.ID, f.product.ID)
			c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/orders", strings.NewReader(body))
			c.Request.Header.Set("Content-Type", "application/json")
			c.Request.Header.Set("X-Lang", locale)
			c.Set("user_id", f.users[0].ID)
			handlers.Create.CreateOrder(c)
			var result response.Response
			if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.StatusCode != 0 {
				t.Fatalf("main checkout failed: %s", recorder.Body.String())
			}
			assertDirectCheckoutPrivate(t, recorder.Body.Bytes())
			var records []kafudadomain.Recharge
			if err := f.db.Find(&records).Error; err != nil {
				t.Fatal(err)
			}
			if len(records) != 1 || records[0].Snapshot.CostPrice.String() != "45.00" || records[0].Snapshot.Plan != "plus" || records[0].Snapshot.PaymentProfile != "checkout-profile" {
				t.Fatal("request fields overrode server snapshot")
			}
		})
	}
}

func TestDirectCheckoutRejectsUnsupportedInputsBeforePersistence(t *testing.T) {
	for _, scenario := range []string{"channel", "downstream", "guest", "reseller", "mixed", "quantity", "missing_sku", "unconfirmed_cost", "expired"} {
		t.Run(scenario, func(t *testing.T) {
			f := newDirectCheckoutFixture(t)
			switch scenario {
			case "channel":
				f.input.StorefrontRequest = false
			case "downstream":
				f.input.StorefrontRequest = false
				f.input.SkipRiskControl = true
			case "reseller":
				f.input.Tenant = resellercontract.ResellerTenantContext("reseller.example", 7, 1, "")
			case "mixed":
				f.input.Items = append(f.input.Items, orderapp.CreateOrderItem{ProductID: 99, SKUID: 99, Quantity: 1})
			case "quantity":
				f.input.Items[0].Quantity = 2
			case "missing_sku":
				f.input.Items[0].SKUID = 0
			case "unconfirmed_cost":
				if err := f.db.Model(&f.sku).Update("direct_cost_configured", false).Error; err != nil {
					t.Fatal(err)
				}
			case "expired":
				f.catalog.SyncedAt = time.Now().Add(-time.Hour)
				f.catalog.ExpiresAt = time.Now().Add(-time.Minute)
				if err := f.store.SaveCatalog(context.Background(), f.catalog); err != nil {
					t.Fatal(err)
				}
			}
			svc := orderapp.NewOrderService(f.options)
			var err error
			if scenario == "guest" {
				_, err = svc.CreateGuestOrder(orderapp.CreateGuestOrderInput{Email: "guest@example.test", OrderPassword: "password123", Tenant: f.input.Tenant, Items: f.input.Items})
			} else {
				_, err = svc.CreateOrder(f.input)
			}
			want := orderapp.ErrProductPurchaseNotAllowed
			switch scenario {
			case "missing_sku":
				want = orderapp.ErrProductSKURequired
			case "unconfirmed_cost":
				want = orderapp.ErrProductPriceInvalid
			case "expired":
				want = orderapp.ErrProductNotAvailable
			}
			if !errors.Is(err, want) {
				t.Fatalf("expected %v, got %v", want, err)
			}
			assertNoDirectCheckoutRows(t, f)
		})
	}
}
