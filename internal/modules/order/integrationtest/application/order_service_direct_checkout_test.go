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
	"testing/synctest"
	"time"

	"github.com/dujiao-next/internal/app/container"
	"github.com/dujiao-next/internal/app/httpserver/middleware"
	kafudawiring "github.com/dujiao-next/internal/bootstrap/kafudagpt"
	orderwiring "github.com/dujiao-next/internal/bootstrap/order"
	"github.com/dujiao-next/internal/config"
	"github.com/dujiao-next/internal/constants"
	productadmin "github.com/dujiao-next/internal/modules/catalog/product/application/admin"
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
	promotionapp "github.com/dujiao-next/internal/modules/promotion/application"
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
	testAdminDirectReviewFilterAndProjection(t, newDirectCheckoutFixture(t))
}

func testAdminDirectReviewFilterAndProjection(t *testing.T, f directCheckoutFixture) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	if err := f.db.AutoMigrate(&kafudadomain.Attempt{}, &paymentdomain.Payment{}); err != nil {
		t.Fatal(err)
	}
	service := orderapp.NewOrderService(f.options)
	records := kafudaapp.New(f.store)
	ctx, now := context.Background(), time.Now().UTC()
	const upstreamOrderID uint64 = 9007199254740993
	var parentID, childID, manualDeliveryID, refundedID uint
	var parentOrderNo string
	for _, scenario := range []struct {
		name          string
		stage         kafudadomain.Stage
		status        string
		parentUpdates map[string]any
		childUpdates  map[string]any
		completed     bool
	}{
		{name: "manual_review", stage: kafudadomain.StageManualReview, status: constants.OrderStatusFulfilling},
		{name: "manual_delivery", stage: kafudadomain.StageManualDelivery, status: constants.OrderStatusPaid},
		{name: "waiting", stage: kafudadomain.StageWaiting, status: constants.OrderStatusPaid},
		{name: "refunded", stage: kafudadomain.StageManualDelivery, status: constants.OrderStatusRefunded, parentUpdates: map[string]any{"refunded_amount": 1}},
		{name: "canceled", stage: kafudadomain.StageManualDelivery, status: constants.OrderStatusCanceled},
		{name: "mismatched_status", stage: kafudadomain.StageManualDelivery, status: constants.OrderStatusFulfilling, childUpdates: map[string]any{"status": constants.OrderStatusPaid}},
		{name: "unpaid_parent", stage: kafudadomain.StageManualDelivery, status: constants.OrderStatusPaid, parentUpdates: map[string]any{"paid_at": nil}},
		{name: "unpaid_child", stage: kafudadomain.StageManualDelivery, status: constants.OrderStatusPaid, childUpdates: map[string]any{"paid_at": nil}},
		{name: "parent_has_refund", stage: kafudadomain.StageManualDelivery, status: constants.OrderStatusPaid, parentUpdates: map[string]any{"refunded_amount": 1}},
		{name: "child_has_refund", stage: kafudadomain.StageManualDelivery, status: constants.OrderStatusFulfilling, childUpdates: map[string]any{"refunded_amount": 1}},
		{name: "completed_recharge", stage: kafudadomain.StageManualDelivery, status: constants.OrderStatusFulfilling, completed: true},
	} {
		order, err := service.CreateOrder(f.input)
		if err != nil {
			t.Fatal(err)
		}
		child := order.Children[0].ID
		switch scenario.name {
		case "manual_delivery":
			manualDeliveryID = order.ID
		case "refunded":
			refundedID = order.ID
		case "manual_review":
			parentID, childID = order.ID, child
			parentOrderNo = order.OrderNo
			reserved, err := records.Reserve(ctx, child, now, now.Add(time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			if err := f.db.Model(&kafudadomain.Attempt{}).Where("id = ?", reserved.Attempt.ID).Updates(map[string]any{
				"status": kafudadomain.AttemptReview, "upstream_order_id": upstreamOrderID, "masked_email": "private@example.test", "quote_id": "private-quote", "charged_usd_cents": 1546,
			}).Error; err != nil {
				t.Fatal(err)
			}
		}
		if err := f.db.Model(&orderdomain.Order{}).Where("id IN ?", []uint{order.ID, child}).
			Updates(map[string]any{"status": scenario.status, "paid_at": now}).Error; err != nil {
			t.Fatal(err)
		}
		if len(scenario.parentUpdates) > 0 {
			if err := f.db.Model(&orderdomain.Order{}).Where("id = ?", order.ID).Updates(scenario.parentUpdates).Error; err != nil {
				t.Fatal(err)
			}
		}
		if len(scenario.childUpdates) > 0 {
			if err := f.db.Model(&orderdomain.Order{}).Where("id = ?", child).Updates(scenario.childUpdates).Error; err != nil {
				t.Fatal(err)
			}
		}
		rechargeUpdates := map[string]any{"stage": scenario.stage, "reason_code": "outcome_unknown"}
		if scenario.name == "manual_review" {
			rechargeUpdates["reason_code"] = kafudadomain.ReasonResultApplyFailed
		}
		if scenario.completed {
			rechargeUpdates["completed_at"] = now
		}
		if err := f.db.Model(&kafudadomain.Recharge{}).Where("order_id = ?", child).
			Updates(rechargeUpdates).Error; err != nil {
			t.Fatal(err)
		}
	}
	ordinary := orderdomain.Order{OrderNo: "ORDINARY-REVIEW-TEST", UserID: f.users[0].ID, Currency: "CNY", Status: constants.OrderStatusPaid}
	if err := f.db.Create(&ordinary).Error; err != nil {
		t.Fatal(err)
	}
	handlers := orderwiring.New(&container.Container{
		OrderService: service, OrderStore: f.options.OrderStore, UserStore: userstore.New(f.db),
		PaymentStore: paymentstore.New(f.db, "test-guest-credential-secret-with-32-bytes"),
		KafudaGPT:    &kafudawiring.Module{Store: f.store},
	})
	router := gin.New()
	orderhttp.RegisterAdminRoutes(router.Group("/admin"), handlers.Admin)
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}
	for _, id := range []uint{parentID, childID} {
		raw := get(fmt.Sprintf("/admin/orders/%d", id)).Body.String()
		if !strings.Contains(raw, `"status":"fulfilling"`) || !strings.Contains(raw, `"upstream_order_id":"9007199254740993"`) || !strings.Contains(raw, `"reason":"result_apply_failed"`) ||
			!strings.Contains(raw, `"active":true`) || !strings.Contains(raw, `"card_charged_usd":"15.46"`) ||
			strings.Contains(raw, "private@example.test") || strings.Contains(raw, "private-quote") || strings.Contains(raw, `"charged_usd_cents"`) ||
			strings.Contains(raw, `"quote_costs"`) {
			t.Fatalf("unexpected projection: %s", raw)
		}
	}
	for _, scenario := range []struct {
		name       string
		upstreamID any
		charge     any
		want       string
	}{
		{name: "zero", upstreamID: upstreamOrderID, charge: 0, want: `"0.00"`},
		{name: "unreported", upstreamID: upstreamOrderID},
		{name: "negative", upstreamID: upstreamOrderID, charge: -25},
		{name: "unidentified", charge: 1546},
		{name: "invalid_id", upstreamID: 0, charge: 1546},
	} {
		t.Run("card_charge_"+scenario.name, func(t *testing.T) {
			if err := f.db.Model(&kafudadomain.Attempt{}).Where("order_id = ?", childID).
				Updates(map[string]any{"upstream_order_id": scenario.upstreamID, "charged_usd_cents": scenario.charge}).Error; err != nil {
				t.Fatal(err)
			}
			raw := get(fmt.Sprintf("/admin/orders/%d", childID)).Body.Bytes()
			var body struct {
				Code int `json:"status_code"`
				Data struct {
					Direct struct {
						Attempt map[string]json.RawMessage `json:"attempt"`
					} `json:"direct_recharge"`
				} `json:"data"`
			}
			if err := json.Unmarshal(raw, &body); err != nil || body.Code != 0 || len(body.Data.Direct.Attempt) == 0 ||
				string(body.Data.Direct.Attempt["card_charged_usd"]) != scenario.want {
				t.Fatalf("unexpected card charge: %s, %v", raw, err)
			}
		})
	}
	t.Run("quote_costs_without_upstream_id", func(t *testing.T) {
		if err := f.db.Model(&kafudadomain.Attempt{}).Where("order_id = ?", childID).
			Select("quote_costs", "upstream_order_id", "charged_usd_cents").Updates(&kafudadomain.Attempt{
			QuoteCosts: &kafudadomain.QuoteCosts{ServiceFeeCents: 905, CardLoadCents: 46, TotalCents: 951},
		}).Error; err != nil {
			t.Fatal(err)
		}
		wantCosts := map[string]string{
			"service_fee_usd": "9.05", "card_open_fee_usd": "0.00", "card_load_usd": "0.46", "total_usd": "9.51",
		}
		for _, id := range []uint{parentID, childID} {
			raw := get(fmt.Sprintf("/admin/orders/%d", id)).Body.Bytes()
			var body struct {
				Code int `json:"status_code"`
				Data struct {
					Direct struct {
						Attempt struct {
							QuoteCosts      map[string]string `json:"quote_costs"`
							UpstreamOrderID *string           `json:"upstream_order_id"`
							CardChargedUSD  *string           `json:"card_charged_usd"`
						} `json:"attempt"`
					} `json:"direct_recharge"`
				} `json:"data"`
			}
			if err := json.Unmarshal(raw, &body); err != nil || body.Code != 0 {
				t.Fatalf("unexpected admin order response: %s, %v", raw, err)
			}
			attempt := body.Data.Direct.Attempt
			if !reflect.DeepEqual(attempt.QuoteCosts, wantCosts) || attempt.UpstreamOrderID != nil || attempt.CardChargedUSD != nil || strings.Contains(string(raw), "private-quote") {
				t.Fatalf("quote costs or upstream facts projected incorrectly: %s", raw)
			}
		}

		const userJWTSecret = "orders-private-quote-integration-test-key"
		auth := userauthapp.NewService(&config.Config{UserJWT: config.JWTConfig{SecretKey: userJWTSecret}}, f.options.UserStore, nil, nil, nil, nil, nil)
		ownerToken, _, err := auth.GenerateUserJWT(&f.users[0], 1)
		if err != nil {
			t.Fatal(err)
		}
		buyer := router.Group("", middleware.UserJWTAuthMiddleware(userJWTSecret, f.options.UserStore))
		orderhttp.RegisterUserReadRoutes(buyer, handlers.User)
		for suffix, visibleField := range map[string]string{"": "order_no", "/direct-status": "stage"} {
			req := httptest.NewRequest(http.MethodGet, "/orders/"+parentOrderNo+suffix, nil).
				WithContext(resellercontract.WithTenantContext(ctx, resellercontract.MainTenantContext("shop.example")))
			req.Header.Set("Authorization", "Bearer "+ownerToken)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			var body struct {
				Code int                        `json:"status_code"`
				Data map[string]json.RawMessage `json:"data"`
			}
			raw := rec.Body.String()
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Code != 0 || len(body.Data[visibleField]) == 0 {
				t.Fatalf("unexpected buyer order response: %s, %v", raw, err)
			}
			for _, field := range []string{`"quote_costs"`, `"service_fee_usd"`, `"card_open_fee_usd"`, `"card_load_usd"`, `"total_usd"`, "private-quote"} {
				if strings.Contains(raw, field) {
					t.Fatalf("buyer response leaked quote data: %s", raw)
				}
			}
		}
		if err := f.db.Model(&kafudadomain.Attempt{}).Where("order_id = ?", childID).
			Updates(map[string]any{"upstream_order_id": upstreamOrderID, "charged_usd_cents": 1546}).Error; err != nil {
			t.Fatal(err)
		}
		if raw := get(fmt.Sprintf("/admin/orders/%d", childID)).Body.String(); !strings.Contains(raw, `"total_usd":"9.51"`) ||
			!strings.Contains(raw, `"card_charged_usd":"15.46"`) {
			t.Fatalf("quote total and actual card debit must be shown independently: %s", raw)
		}
	})
	if err := f.db.Model(&kafudadomain.Attempt{}).Where("order_id = ?", childID).Update("masked_email", "b***@***").Error; err != nil {
		t.Fatal(err)
	}
	if raw := get(fmt.Sprintf("/admin/orders/%d", childID)).Body.String(); !strings.Contains(raw, `"masked_email":"b***@***"`) {
		t.Fatalf("valid masked account was omitted: %s", raw)
	}
	if raw := get(fmt.Sprintf("/admin/orders/%d", ordinary.ID)).Body.String(); strings.Contains(raw, "direct_recharge") || !strings.Contains(raw, `"status_code":0`) {
		t.Fatalf("ordinary order response: %s", raw)
	}
	if raw := get(fmt.Sprintf("/admin/orders/%d", refundedID)).Body.String(); !strings.Contains(raw, `"status":"refunded"`) || !strings.Contains(raw, `"stage":"manual_delivery"`) {
		t.Fatalf("refunded order historical detail: %s", raw)
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
	if len(seen) != 2 || !seen[parentID] || !seen[manualDeliveryID] {
		t.Fatalf("filtered pages did not return unique review orders: %v", seen)
	}
	if raw := get("/admin/orders?direct_review=invalid").Body.String(); !strings.Contains(raw, `"status_code":400`) {
		t.Fatalf("invalid filter response: %s", raw)
	}
}

func TestDirectCheckoutGenericAfterSalesCannotBypassReview(t *testing.T) {
	testDirectCheckoutRefunds(t, newDirectCheckoutFixture)
}

func testDirectCheckoutRefunds(t *testing.T, fixture func(*testing.T) directCheckoutFixture) {
	t.Helper()
	for _, method := range []string{"wallet", "manual"} {
		t.Run(method, func(t *testing.T) { testDirectCheckoutRefund(t, fixture(t), method) })
	}
}

func testDirectCheckoutRefund(t *testing.T, f directCheckoutFixture, method string) {
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
	stopped := stoppedDirectSalesContainer(t, f)
	service = stopped.OrderService
	records := kafudaworkflow.WithShopOrderTransitions(kafudaapp.New(f.store), f.options.OrderStore)
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
		if err := f.db.First(&stored, orderID).Error; err != nil || stored.Status != constants.OrderStatusFulfilling || !stored.RefundedAmount.IsZero() {
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
	if _, err := kafudaworkflow.ApplyResult(f.options.OrderStore, childID, id, result, now, now, 0); err != nil {
		t.Fatal(err)
	}
	assertDirectShopOrderStatus(t, f.options.OrderStore, order, constants.OrderStatusFulfilling, now)
	for _, input := range []struct{ orderID, attemptID uint }{{childID, id}, {order.ID, id + 1}, {order.ID, 0}} {
		if err := apply(input.orderID, input.attemptID, order.TotalAmount); !errors.Is(err, orderrefund.ErrOrderStatusInvalid) {
			t.Fatalf("invalid refund target accepted: %+v, %v", input, err)
		}
	}
	if err := apply(order.ID, id, amount); !errors.Is(err, orderrefund.ErrOrderStatusInvalid) {
		t.Fatalf("partial direct refund accepted: %v", err)
	}
	stopped.OrderRefundService = refunds
	handler := orderwiring.NewAdminRefundHandler(stopped)
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
	receivedAt := time.Now().UTC()
	if completed, err := kafudaworkflow.ApplyResult(f.options.OrderStore, childID, id, result, receivedAt, receivedAt, 0); completed || !errors.Is(err, orderapp.ErrOrderStatusInvalid) {
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
	t.Run("checking_lease_expires_before_lock", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) { testDirectCheckLeaseExpiresBeforeLock(t, newFixture(t)) })
	})
	t.Run("quote_expires_before_dispatch", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) { testDirectQuoteExpiresBeforeDispatch(t, newFixture(t), false) })
	})
	t.Run("quote_expires_after_recovery", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) { testDirectQuoteExpiresBeforeDispatch(t, newFixture(t), true) })
	})
	for _, scenario := range []struct {
		name       string
		stage      kafudadomain.Stage
		wantCall   bool
		wantReason string
	}{
		{"region_locked", kafudadomain.StageManualDelivery, false, kafudadomain.ReasonQuoteRegionLocked},
		{"quote_expired", kafudadomain.StageWaiting, false, kafudadomain.ReasonQuoteExpired},
		{"upstream_quote_expired", kafudadomain.StageWaiting, true, kafudadomain.ReasonQuoteExpired},
		{"upstream_quote_expired_after_recovery", kafudadomain.StageManualReview, true, "outcome_unknown"},
		{"upstream_balance_insufficient", kafudadomain.StageManualDelivery, true, kafudadomain.ReasonMerchantBalanceInsufficient},
		{"ambiguous_bad_request", kafudadomain.StageManualReview, true, "outcome_unknown"},
		{"quote_unavailable", kafudadomain.StageManualDelivery, false, kafudadomain.ReasonQuoteUnavailable},
		{"policy_rejected", kafudadomain.StageManualDelivery, false, kafudadomain.ReasonQuotePolicy},
		{"changed_snapshot_country", kafudadomain.StageManualDelivery, false, kafudadomain.ReasonQuotePolicy},
		{"changed_reservation_reference", kafudadomain.StageChecking, false, ""},
		{"processing", kafudadomain.StageProcessing, true, ""},
		{"completed", kafudadomain.StageCompleted, true, ""},
		{"completed_after_sales_disabled", kafudadomain.StageCompleted, true, ""},
		{"poll_after_sales_disabled", kafudadomain.StageProcessing, true, ""},
		{"response_lost", kafudadomain.StageManualReview, true, "outcome_unknown"},
		{"response_lost_submitting", kafudadomain.StageManualReview, true, "outcome_unknown"},
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
				policy := stoppedDirectSalesContainer(t, f).KafudaGPT.Commerce
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
			storedSnapshot, storedReference := reservation.Recharge.Snapshot, reservation.Attempt.Reference
			var initialAttempt *kafudadomain.Attempt
			quoteCosts := kafudadomain.QuoteCosts{ServiceFeeCents: 900, CardLoadCents: 100, TotalCents: 1000}
			check := kafudacontract.CheckResult{
				Country: "PH", PayMode: "auto_card", RegionLocked: scenario.name == "region_locked",
				Plans: []kafudacontract.Quote{{
					Plan: "plus", Status: "available", Kind: "new", QuoteID: "quote_checkout",
					PriceCents: 900, ExpiresAt: now.Add(time.Minute),
					AutoCard: &kafudacontract.AutoCard{ProductCode: "checkout-card", LoadCents: 100, TotalCents: 1000},
				}},
			}
			switch scenario.name {
			case "quote_expired":
				check.Plans[0].ExpiresAt = now
			case "quote_unavailable":
				check.Plans[0].Status = "not_eligible"
			case "policy_rejected":
				check.Plans[0].AutoCard.ProductCode = "wrong-card"
			case "changed_snapshot_country":
				// Matching a caller's modified snapshot must not override the paid country.
				reservation.Recharge.Snapshot.Country, check.Country = "CL", "CL"
			case "changed_reservation_reference":
				initialAttempt, err = f.store.GetAttempt(ctx, reservation.Attempt.ID)
				if err != nil || initialAttempt == nil {
					t.Fatalf("load original checking attempt: %+v, %v", initialAttempt, err)
				}
				reservation.Attempt.Reference = "replacement-reference"
			}
			calls := 0
			gateway := directCreateGateway{create: func(ctx context.Context, req kafudacontract.CreateOrderRequest) (*kafudacontract.OrderResult, error) {
				calls++
				attempt, err := f.store.GetAttempt(ctx, reservation.Attempt.ID)
				if err != nil || attempt == nil || attempt.Status != kafudadomain.AttemptSubmitting ||
					attempt.Reference != req.Reference || attempt.QuoteID != req.QuoteID ||
					req.Reference != reservation.Attempt.Reference || req.QuoteID != "quote_checkout" ||
					!reflect.DeepEqual(attempt.QuoteCosts, &quoteCosts) {
					t.Fatalf("create without committed attempt: %+v, %v", attempt, err)
				}
				result := &kafudacontract.OrderResult{ID: 100, Reference: req.Reference, Product: "gpt", Plan: "plus", Country: "PH", Status: "processing"}
				switch scenario.name {
				case "completed", "completed_after_sales_disabled":
					result.Status = "completed"
				case "response_lost", "response_lost_submitting":
					return nil, errors.New("supplier response lost")
				case "response_mismatch":
					result.Reference = "other-reference"
				case "upstream_quote_expired":
					return nil, kafudacontract.ErrCreateQuoteExpired
				case "upstream_quote_expired_after_recovery":
					if attempt.NextPollAt == nil {
						t.Fatal("committed submission has no recovery time")
					}
					recovery := kafudaworkflow.WithShopOrderTransitions(records, f.options.OrderStore)
					if err := recovery.RecoverUnidentifiedSubmissions(ctx, *attempt.NextPollAt, 1); err != nil {
						t.Fatal(err)
					}
					return nil, kafudacontract.ErrCreateQuoteExpired
				case "upstream_balance_insufficient":
					return nil, kafudacontract.ErrMerchantBalanceInsufficient
				case "ambiguous_bad_request":
					return nil, &kafudacontract.APIError{HTTPStatus: http.StatusBadRequest, Code: "upstream_error"}
				}
				return result, nil
			}, get: func(_ context.Context, id uint64) (*kafudacontract.OrderResult, error) {
				if id != 100 {
					t.Fatalf("polled unexpected upstream order %d", id)
				}
				return &kafudacontract.OrderResult{ID: id, Reference: reservation.Attempt.Reference, Product: "gpt", Plan: "plus", Country: "PH", Status: "completed"}, nil
			}}
			completed, err := kafudaworkflow.SubmitCheckedQuote(ctx, f.options.OrderStore, records, gateway, reservation, check, 10*time.Second)
			wantError := scenario.wantReason != "" || scenario.name == "changed_reservation_reference"
			if completed != (scenario.stage == kafudadomain.StageCompleted) || (err != nil) != wantError {
				t.Fatalf("unexpected outcome: completed=%v, err=%v", completed, err)
			}
			switch scenario.name {
			case "quote_expired", "upstream_quote_expired":
				if !errors.Is(err, kafudaapp.ErrQuoteExpired) {
					t.Fatalf("expected refreshable expired quote, got %v", err)
				}
			case "upstream_quote_expired_after_recovery":
				if !errors.Is(err, kafudadomain.ErrInvalidTransition) {
					t.Fatalf("expected late rejection to preserve the review state, got %v", err)
				}
			case "upstream_balance_insufficient":
				if !errors.Is(err, kafudacontract.ErrMerchantBalanceInsufficient) {
					t.Fatalf("expected merchant balance rejection, got %v", err)
				}
			case "changed_snapshot_country":
				if !errors.Is(err, kafudaapp.ErrQuotePolicy) {
					t.Fatalf("expected rejection of a quote for the wrong paid country, got %v", err)
				}
			case "changed_reservation_reference":
				if !errors.Is(err, kafudadomain.ErrConflict) {
					t.Fatalf("expected changed reference to conflict, got %v", err)
				}
			}
			wantReplayErr := kafudadomain.ErrInvalidTransition
			if scenario.stage == kafudadomain.StageCompleted {
				wantReplayErr = orderapp.ErrOrderStatusInvalid
			}
			if scenario.name == "changed_reservation_reference" {
				wantReplayErr = kafudadomain.ErrConflict
			}
			replayCheck := check
			if scenario.wantCall {
				quote := check.Plans[0]
				quote.QuoteID = "replacement_quote"
				quote.AutoCard = &kafudacontract.AutoCard{ProductCode: "checkout-card", OpenFeeCents: 25, LoadCents: 25, TotalCents: 950}
				replayCheck.Plans = []kafudacontract.Quote{quote}
			}
			if _, err := kafudaworkflow.SubmitCheckedQuote(ctx, f.options.OrderStore, records, gateway, reservation, replayCheck, 10*time.Second); !errors.Is(err, wantReplayErr) {
				t.Fatalf("stale submission replay: %v", err)
			}
			if (scenario.wantCall && calls != 1) || (!scenario.wantCall && calls != 0) {
				t.Fatalf("create called %d times", calls)
			}
			var wantCosts *kafudadomain.QuoteCosts
			if scenario.wantCall {
				wantCosts = &quoteCosts
			}
			original, err := f.store.GetAttempt(ctx, reservation.Attempt.ID)
			if err != nil || original == nil || original.Reference != storedReference || !reflect.DeepEqual(original.QuoteCosts, wantCosts) {
				t.Fatalf("submission or replay changed quote costs: %+v, %v", original, err)
			}
			if initialAttempt != nil && !reflect.DeepEqual(original, initialAttempt) {
				t.Fatalf("conflicting reference changed the live check: %+v", original)
			}
			record, err := f.store.GetRecharge(ctx, childID)
			if err != nil || record == nil || record.Stage != scenario.stage || record.ReasonCode != scenario.wantReason ||
				!reflect.DeepEqual(record.Snapshot, storedSnapshot) {
				t.Fatalf("unexpected persisted recharge: %+v, %v", record, err)
			}
			wantStatus := constants.OrderStatusPaid
			switch scenario.stage {
			case kafudadomain.StageManualReview, kafudadomain.StageManualDelivery:
				wantStatus = constants.OrderStatusFulfilling
			case kafudadomain.StageCompleted:
				wantStatus = constants.OrderStatusCompleted
			}
			assertDirectShopOrderStatus(t, f.options.OrderStore, order, wantStatus, now)
			if scenario.wantReason == kafudadomain.ReasonQuoteExpired || scenario.wantReason == kafudadomain.ReasonMerchantBalanceInsufficient {
				previous, err := f.store.GetAttempt(ctx, reservation.Attempt.ID)
				var wantQuoteID string
				if scenario.wantCall {
					wantQuoteID = "quote_checkout"
				}
				if err != nil || previous == nil || previous.Status != kafudadomain.AttemptFailed || previous.ActiveOrderID != nil ||
					previous.QuoteID != wantQuoteID || previous.NextPollAt != nil || previous.UpstreamOrderID != nil || previous.Reference != reservation.Attempt.Reference ||
					record.CurrentAttemptID == nil || *record.CurrentAttemptID != previous.ID {
					t.Fatalf("rejected quote left an active attempt or lost history: %+v, %v", previous, err)
				}
				if scenario.wantReason == kafudadomain.ReasonMerchantBalanceInsufficient {
					if !kafudacontract.CanResolveManualAttempt(record, previous) {
						t.Fatal("definitive merchant rejection did not open controlled after-sales")
					}
					return
				}
				retryAt := time.Now().UTC()
				retry, err := kafudaworkflow.ReserveBuyerAttempt(f.options.OrderStore, ordercontract.TenantScope{}, order.OrderNo, order.UserID, retryAt, retryAt.Add(time.Minute))
				if err != nil || retry == nil || !retry.Created || retry.Attempt == nil || retry.Attempt.Sequence != previous.Sequence+1 || retry.Attempt.Reference == previous.Reference || retry.Attempt.QuoteCosts != nil {
					t.Fatalf("expired pre-submission quote prevented a new attempt: %+v, %v", retry, err)
				}
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
				result, err := records.PollKnownOrder(ctx, ctx, gateway, attempt.ID, at, at.Add(time.Minute), 0)
				if err != nil {
					t.Fatal(err)
				}
				completed, err = kafudaworkflow.ApplyResult(f.options.OrderStore, childID, attempt.ID, *result, at, at, time.Minute)
				if err != nil || !completed {
					t.Fatalf("polled order completion: completed=%t, err=%v", completed, err)
				}
				finished, err := f.store.GetAttempt(ctx, attempt.ID)
				if err != nil || finished == nil || !reflect.DeepEqual(finished.QuoteCosts, &quoteCosts) {
					t.Fatalf("polled completion changed quote costs: %+v, %v", finished, err)
				}
			}
			if completed {
				assertDirectShopOrderStatus(t, f.options.OrderStore, order, constants.OrderStatusCompleted, now)
			}
			if scenario.stage == kafudadomain.StageManualReview {
				attempt, err := f.store.GetAttempt(ctx, reservation.Attempt.ID)
				if err != nil || attempt == nil || attempt.ActiveOrderID == nil || attempt.UpstreamOrderID != nil {
					t.Fatalf("unknown outcome lost active attempt: %+v, %v", attempt, err)
				}
			}
			switch scenario.name {
			case "response_lost":
				testReconcileUnknownDirectOrder(t, f, order, now, reservation.Attempt, kafudadomain.AttemptProcessing)
			case "response_lost_submitting":
				testReconcileUnknownDirectOrder(t, f, order, now, reservation.Attempt, kafudadomain.AttemptSubmitting)
			}
		})
	}
}

type directSubmitStore struct {
	ordercontract.Store
	beforeTransaction func()
	afterCommit       func()
}

func (s *directSubmitStore) WithinTransaction(run func(ordercontract.Transaction) error) error {
	if s.beforeTransaction != nil {
		beforeTransaction := s.beforeTransaction
		s.beforeTransaction = nil
		beforeTransaction()
	}
	if err := s.Store.WithinTransaction(run); err != nil {
		return err
	}
	if s.afterCommit != nil {
		afterCommit := s.afterCommit
		s.afterCommit = nil
		afterCommit()
	}
	return nil
}

func testDirectCheckLeaseExpiresBeforeLock(t *testing.T, f directCheckoutFixture) {
	t.Helper()
	if err := f.db.AutoMigrate(&kafudadomain.Attempt{}); err != nil {
		t.Fatal(err)
	}
	order, err := orderapp.NewOrderService(f.options).CreateOrder(f.input)
	if err != nil {
		t.Fatal(err)
	}
	ctx, paidAt := t.Context(), time.Now().UTC()
	childID := order.Children[0].ID
	if err := f.db.Model(&orderdomain.Order{}).Where("id IN ?", []uint{order.ID, childID}).
		Updates(map[string]any{"status": constants.OrderStatusPaid, "paid_at": paidAt}).Error; err != nil {
		t.Fatal(err)
	}
	leaseUntil := paidAt.Add(time.Second)
	reservation, err := kafudaworkflow.ReserveBuyerAttempt(f.options.OrderStore, ordercontract.TenantScope{}, order.OrderNo, order.UserID, paidAt, leaseUntil)
	if err != nil || reservation == nil || !reservation.Created {
		t.Fatalf("reserve paid order for checking: %+v, %v", reservation, err)
	}
	check := kafudacontract.CheckResult{Country: "PH", PayMode: "auto_card", Plans: []kafudacontract.Quote{{
		Plan: "plus", Status: "available", Kind: "new", QuoteID: "stale_check_quote", ExpiresAt: paidAt.Add(time.Minute), PriceCents: 900,
		AutoCard: &kafudacontract.AutoCard{ProductCode: "checkout-card", LoadCents: 100, TotalCents: 1000},
	}}}
	store := &directSubmitStore{Store: f.options.OrderStore, beforeTransaction: func() {
		// The authorization result arrives while the check lease is live. The
		// lease reaches its deadline while the workflow is waiting for the lock.
		time.Sleep(time.Until(leaseUntil))
	}}
	records := kafudaapp.New(f.store)
	calls := 0
	gateway := directCreateGateway{create: func(_ context.Context, req kafudacontract.CreateOrderRequest) (*kafudacontract.OrderResult, error) {
		calls++
		return &kafudacontract.OrderResult{ID: 100, Reference: req.Reference, Product: "gpt", Plan: "plus", Country: "PH", Status: "processing"}, nil
	}}
	completed, err := kafudaworkflow.SubmitCheckedQuote(ctx, store, records, gateway, reservation, check, time.Second)
	if completed || !errors.Is(err, kafudadomain.ErrInvalidTransition) || calls != 0 {
		t.Fatalf("stale check dispatched after waiting for lock: completed=%v, calls=%d, err=%v", completed, calls, err)
	}
	attempt, err := f.store.GetAttempt(ctx, reservation.Attempt.ID)
	if err != nil || attempt == nil || attempt.Status != kafudadomain.AttemptChecking || attempt.Reference != reservation.Attempt.Reference ||
		attempt.ActiveOrderID == nil || *attempt.ActiveOrderID != childID || attempt.NextPollAt == nil || !attempt.NextPollAt.Equal(leaseUntil) ||
		attempt.QuoteID != "" || attempt.QuoteCosts != nil || attempt.UpstreamOrderID != nil {
		t.Fatalf("stale response changed the expired check before recovery: %+v, %v", attempt, err)
	}
	record, err := f.store.GetRecharge(ctx, childID)
	if err != nil || record == nil || record.Stage != kafudadomain.StageChecking || record.CurrentAttemptID == nil || *record.CurrentAttemptID != attempt.ID || record.ReasonCode != "" {
		t.Fatalf("stale response changed recharge state before recovery: %+v, %v", record, err)
	}
	assertDirectShopOrderStatus(t, store, order, constants.OrderStatusPaid, paidAt)

	if err := kafudaworkflow.WithShopOrderTransitions(records, store).RecoverExpiredChecks(ctx, time.Now().UTC(), 1); err != nil {
		t.Fatal(err)
	}
	retryAt := time.Now().UTC()
	retry, err := kafudaworkflow.ReserveBuyerAttempt(store, ordercontract.TenantScope{}, order.OrderNo, order.UserID, retryAt, retryAt.Add(time.Minute))
	if err != nil || retry == nil || !retry.Created || retry.Attempt == nil || retry.Attempt.Sequence != attempt.Sequence+1 || retry.Attempt.Reference == attempt.Reference {
		t.Fatalf("recovery did not make room for fresh authorization: %+v, %v", retry, err)
	}
	check.Plans[0].QuoteID, check.Plans[0].ExpiresAt = "fresh_check_quote", retryAt.Add(time.Minute)
	completed, err = kafudaworkflow.SubmitCheckedQuote(ctx, store, records, gateway, retry, check, time.Second)
	if completed || err != nil || calls != 1 {
		t.Fatalf("fresh authorization did not dispatch once: completed=%v, calls=%d, err=%v", completed, calls, err)
	}
	failed, err := f.store.GetAttempt(ctx, attempt.ID)
	if err != nil || failed == nil || failed.Status != kafudadomain.AttemptFailed || failed.Reference != attempt.Reference || failed.ActiveOrderID != nil || failed.QuoteID != "" {
		t.Fatalf("fresh authorization did not preserve the failed check history: %+v, %v", failed, err)
	}
	submitted, err := f.store.GetAttempt(ctx, retry.Attempt.ID)
	if err != nil || submitted == nil || submitted.Status != kafudadomain.AttemptProcessing || submitted.QuoteID != "fresh_check_quote" || submitted.Reference != retry.Attempt.Reference {
		t.Fatalf("fresh authorization did not persist the new attempt: %+v, %v", submitted, err)
	}
}

func testDirectQuoteExpiresBeforeDispatch(t *testing.T, f directCheckoutFixture, recoverBeforeDispatch bool) {
	t.Helper()
	if err := f.db.AutoMigrate(&kafudadomain.Attempt{}); err != nil {
		t.Fatal(err)
	}
	order, err := orderapp.NewOrderService(f.options).CreateOrder(f.input)
	if err != nil {
		t.Fatal(err)
	}
	ctx, paidAt := t.Context(), time.Now().UTC()
	childID := order.Children[0].ID
	if err := f.db.Model(&orderdomain.Order{}).Where("id IN ?", []uint{order.ID, childID}).
		Updates(map[string]any{"status": constants.OrderStatusPaid, "paid_at": paidAt}).Error; err != nil {
		t.Fatal(err)
	}
	reservation, err := kafudaworkflow.ReserveBuyerAttempt(f.options.OrderStore, ordercontract.TenantScope{}, order.OrderNo, order.UserID, paidAt, paidAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	const quoteID = "quote_expires_during_submission"
	expiresAt := paidAt.Add(time.Second)
	quoteCosts := kafudadomain.QuoteCosts{ServiceFeeCents: 900, CardOpenFeeCents: 50, CardLoadCents: 50, TotalCents: 1000}
	check := kafudacontract.CheckResult{Country: "PH", PayMode: "auto_card", Plans: []kafudacontract.Quote{{
		Plan: "plus", Status: "available", Kind: "new", QuoteID: quoteID, ExpiresAt: expiresAt, PriceCents: 900,
		AutoCard: &kafudacontract.AutoCard{ProductCode: "checkout-card", OpenFeeCents: 50, LoadCents: 50, TotalCents: 1000},
	}}}
	records := kafudaapp.New(f.store)
	store := &directSubmitStore{Store: f.options.OrderStore, afterCommit: func() {
		attempt, err := f.store.GetAttempt(ctx, reservation.Attempt.ID)
		if err != nil || attempt == nil || attempt.Status != kafudadomain.AttemptSubmitting || attempt.QuoteID != quoteID || attempt.NextPollAt == nil ||
			!reflect.DeepEqual(attempt.QuoteCosts, &quoteCosts) {
			t.Fatalf("expected committed submit before expiry: %+v, %v", attempt, err)
		}
		if recoverBeforeDispatch {
			time.Sleep(time.Until(*attempt.NextPollAt))
			recovery := kafudaworkflow.WithShopOrderTransitions(records, f.options.OrderStore)
			if err := recovery.RecoverUnidentifiedSubmissions(ctx, time.Now().UTC(), 1); err != nil {
				t.Fatal(err)
			}
			return
		}
		time.Sleep(time.Until(expiresAt))
	}}
	calls := 0
	gateway := directCreateGateway{create: func(ctx context.Context, _ kafudacontract.CreateOrderRequest) (*kafudacontract.OrderResult, error) {
		calls++
		return nil, ctx.Err()
	}}
	completed, err := kafudaworkflow.SubmitCheckedQuote(ctx, store, records, gateway, reservation, check, time.Second)
	wantError := kafudaapp.ErrQuoteExpired
	if recoverBeforeDispatch {
		wantError = kafudadomain.ErrInvalidTransition
	}
	if completed || !errors.Is(err, wantError) || calls != 0 {
		t.Fatalf("expired quote dispatch: completed=%v, calls=%d, err=%v", completed, calls, err)
	}
	attempt, err := f.store.GetAttempt(ctx, reservation.Attempt.ID)
	if err != nil || attempt == nil || attempt.Reference != reservation.Attempt.Reference || attempt.QuoteID != quoteID || attempt.UpstreamOrderID != nil ||
		!reflect.DeepEqual(attempt.QuoteCosts, &quoteCosts) {
		t.Fatalf("expired quote lost attempt history: %+v, %v", attempt, err)
	}
	record, err := f.store.GetRecharge(ctx, childID)
	if err != nil || record == nil || record.CurrentAttemptID == nil || *record.CurrentAttemptID != attempt.ID ||
		!reflect.DeepEqual(record.Snapshot, reservation.Recharge.Snapshot) {
		t.Fatalf("expired quote changed recharge identity: %+v, %v", record, err)
	}
	retryAt := time.Now().UTC()
	if recoverBeforeDispatch {
		if record.Stage != kafudadomain.StageManualReview || record.ReasonCode != "outcome_unknown" ||
			attempt.Status != kafudadomain.AttemptReview || attempt.ActiveOrderID == nil || *attempt.ActiveOrderID != childID || attempt.NextPollAt == nil {
			t.Fatalf("expired quote overwrote recovery: recharge=%+v attempt=%+v", record, attempt)
		}
		assertDirectShopOrderStatus(t, store, order, constants.OrderStatusFulfilling, paidAt)
		replay, err := kafudaworkflow.ReserveBuyerAttempt(store, ordercontract.TenantScope{}, order.OrderNo, order.UserID, retryAt, retryAt.Add(time.Minute))
		if err != nil || replay == nil || replay.Created || replay.Attempt == nil || replay.Attempt.ID != attempt.ID {
			t.Fatalf("recovered attempt was replaced: %+v, %v", replay, err)
		}
		return
	}
	if record.Stage != kafudadomain.StageWaiting || record.ReasonCode != kafudadomain.ReasonQuoteExpired ||
		attempt.Status != kafudadomain.AttemptFailed || attempt.ActiveOrderID != nil || attempt.NextPollAt != nil {
		t.Fatalf("expired quote kept its slot: recharge=%+v attempt=%+v", record, attempt)
	}
	assertDirectShopOrderStatus(t, store, order, constants.OrderStatusPaid, paidAt)
	retry, err := kafudaworkflow.ReserveBuyerAttempt(store, ordercontract.TenantScope{}, order.OrderNo, order.UserID, retryAt, retryAt.Add(time.Minute))
	if err != nil || retry == nil || !retry.Created || retry.Attempt == nil ||
		retry.Attempt.Sequence != attempt.Sequence+1 || retry.Attempt.Reference == attempt.Reference {
		t.Fatalf("expired quote prevented a new attempt: %+v, %v", retry, err)
	}
}

func testReconcileUnknownDirectOrder(t *testing.T, f directCheckoutFixture, order *orderdomain.Order, paidAt time.Time, attempt *kafudadomain.Attempt, supplierStatus kafudadomain.AttemptStatus) {
	t.Helper()
	ctx := t.Context()
	childID := order.Children[0].ID
	const upstreamID = uint64(9_007_199_254_740_993)
	const pollInterval = 10 * time.Second
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
		status := supplierStatus
		if calls >= 4 {
			status = kafudadomain.AttemptCompleted
		}
		return &kafudacontract.OrderResult{
			ID: id, Reference: reference, Product: "gpt", Plan: "plus", Country: "PH", Status: string(status),
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
	original, err := f.store.GetAttempt(ctx, attempt.ID)
	if err != nil || original == nil || original.UpstreamOrderID != nil || original.Status != kafudadomain.AttemptReview {
		t.Fatalf("mismatch changed the original attempt: %+v, %v", original, err)
	}
	assertDirectShopOrderStatus(t, f.options.OrderStore, order, constants.OrderStatusFulfilling, paidAt)
	request(attempt.ID, true)
	request(attempt.ID, false)
	wantStatus := supplierStatus
	if supplierStatus == kafudadomain.AttemptSubmitting {
		wantStatus = kafudadomain.AttemptReview
	}
	assertPendingLookup := func(wantCalls int) *kafudadomain.Attempt {
		t.Helper()
		stored, err := f.store.GetAttempt(ctx, attempt.ID)
		if err != nil || stored == nil || stored.UpstreamOrderID == nil || *stored.UpstreamOrderID != upstreamID ||
			stored.NextPollAt == nil || stored.NextPollAt.Sub(stored.UpdatedAt) != pollInterval ||
			stored.Status != wantStatus || stored.ActiveOrderID == nil || *stored.ActiveOrderID != childID ||
			stored.Reference != original.Reference || stored.QuoteID != original.QuoteID ||
			!reflect.DeepEqual(stored.QuoteCosts, original.QuoteCosts) || calls != wantCalls {
			t.Fatalf("verified ID did not preserve the active attempt and schedule: %+v, %v, calls=%d", stored, err, calls)
		}
		recharge, err := f.store.GetRecharge(ctx, childID)
		if err != nil || recharge == nil || recharge.Stage != kafudadomain.StageManualReview || recharge.ReasonCode != "outcome_unknown" ||
			recharge.CurrentAttemptID == nil || *recharge.CurrentAttemptID != attempt.ID {
			t.Fatalf("verified intermediate result did not preserve the review: %+v, %v", recharge, err)
		}
		assertDirectShopOrderStatus(t, f.options.OrderStore, order, constants.OrderStatusFulfilling, paidAt)
		return stored
	}
	stored := assertPendingLookup(2)
	records := kafudaapp.New(f.store)
	lookupAt := *stored.NextPollAt
	result, err := records.PollKnownOrder(ctx, ctx, gateway, attempt.ID, lookupAt, lookupAt.Add(time.Minute), 0)
	if err != nil || result == nil || result.Status != string(supplierStatus) {
		t.Fatalf("subsequent intermediate result not accepted: %+v, %v", result, err)
	}
	if completed, err := kafudaworkflow.ApplyResult(f.options.OrderStore, childID, attempt.ID, *result, lookupAt, lookupAt, pollInterval); err != nil || completed {
		t.Fatalf("apply intermediate result: completed=%t, %v", completed, err)
	}
	stored = assertPendingLookup(3)
	lookupAt = *stored.NextPollAt
	result, err = records.PollKnownOrder(ctx, ctx, gateway, attempt.ID, lookupAt, lookupAt.Add(time.Minute), 0)
	if err != nil || result == nil || result.Status != string(kafudadomain.AttemptCompleted) || calls != 4 {
		t.Fatalf("terminal result not found: %+v, %v, calls=%d", result, err, calls)
	}
	for _, wantCompleted := range []bool{true, false} {
		completed, err := kafudaworkflow.ApplyResult(f.options.OrderStore, childID, attempt.ID, *result, lookupAt, lookupAt, pollInterval)
		if err != nil || completed != wantCompleted {
			t.Fatalf("late completion: completed=%t, %v; want %t", completed, err, wantCompleted)
		}
	}
	assertDirectShopOrderStatus(t, f.options.OrderStore, order, constants.OrderStatusCompleted, paidAt)
	var deliveries int64
	if err := f.db.Model(&fulfillmentdomain.Fulfillment{}).Where("order_id = ? AND type = ? AND status = ?",
		childID, constants.FulfillmentTypeKafudaGPT, constants.FulfillmentStatusDelivered).Count(&deliveries).Error; err != nil || deliveries != 1 {
		t.Fatalf("unexpected delivery count after applying the same terminal result twice: %d, %v", deliveries, err)
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
	ids := []uint{order.ID, childID}
	setStatus := func(status string) {
		t.Helper()
		if err := f.db.Model(&orderdomain.Order{}).Where("id IN ?", ids).Update("status", status).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, stage := range []kafudadomain.Stage{
		kafudadomain.StageWaiting, kafudadomain.StageChecking, kafudadomain.StageSubmitting, kafudadomain.StageProcessing,
		kafudadomain.StageManualReview, kafudadomain.StageManualDelivery, kafudadomain.StageCompleted,
	} {
		if stage == kafudadomain.StageCompleted {
			setStatus(constants.OrderStatusCompleted)
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
		if wantStage == kafudadomain.StageManualReview {
			setStatus(constants.OrderStatusFulfilling)
			request(order.OrderNo, ownerToken, main, 0, string(wantStage), "")
			request(order.OrderNo, otherToken, main, response.CodeNotFound, "", "")
			if err := f.db.Model(&orderdomain.Order{}).Where("id = ?", childID).Update("status", constants.OrderStatusPaid).Error; err != nil {
				t.Fatal(err)
			}
			request(order.OrderNo, ownerToken, main, response.CodeNotFound, "", "")
			if err := f.db.Model(&orderdomain.Order{}).Where("id = ?", childID).
				Updates(map[string]any{"status": constants.OrderStatusFulfilling, "paid_at": nil}).Error; err != nil {
				t.Fatal(err)
			}
			request(order.OrderNo, ownerToken, main, response.CodeNotFound, "", "")
			if err := f.db.Model(&orderdomain.Order{}).Where("id = ?", childID).Update("paid_at", now).Error; err != nil {
				t.Fatal(err)
			}
			setStatus(constants.OrderStatusPaid)
		}
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
	cardCharge := int64(1546)
	attempt := kafudadomain.Attempt{
		OrderID: childID, Sequence: 1, Reference: "kgpt-renewal-test", Status: kafudadomain.AttemptCompleted,
		UpstreamOrderID: &upstreamID, RenewalCancelled: &renewalCancelled, ChargedUSDCents: &cardCharge,
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
	t.Run("http", func(t *testing.T) {
		testControlledDirectManualAction(t, newDirectCheckoutFixture(t), "direct-complete", false)
	})
	t.Run("notification", func(t *testing.T) {
		testControlledDirectManualAction(t, newDirectCheckoutFixture(t), "direct-complete", true)
	})
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
	testControlledDirectManualAction(t, newDirectCheckoutFixture(t), "direct-release", false)
}

func testControlledDirectManualAction(t *testing.T, f directCheckoutFixture, action string, notify bool) {
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
	stopped := stoppedDirectSalesContainer(t, f)
	records := kafudaworkflow.WithShopOrderTransitions(kafudaapp.New(f.store), f.options.OrderStore)
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
	assertDirectShopOrderStatus(t, f.options.OrderStore, order, constants.OrderStatusFulfilling, now)
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
	if _, err := kafudaworkflow.ApplyResult(f.options.OrderStore, childID, id, result, now, now, 0); err != nil {
		t.Fatal(err)
	}
	assertDirectShopOrderStatus(t, f.options.OrderStore, order, constants.OrderStatusFulfilling, now)
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
		handler := orderwiring.New(stopped).Admin
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
		assertDirectShopOrderStatus(t, f.options.OrderStore, order, constants.OrderStatusFulfilling, now)
		var deliveries int64
		if err := f.db.Model(&fulfillmentdomain.Fulfillment{}).Where("order_id = ?", childID).Count(&deliveries).Error; err != nil || deliveries != 0 {
			t.Fatalf("release created delivery: %d, %v", deliveries, err)
		}
		retryAt := time.Now().UTC()
		retry, err := kafudaworkflow.ReserveBuyerAttempt(f.options.OrderStore, ordercontract.TenantScope{}, order.OrderNo, order.UserID, retryAt, retryAt.Add(time.Minute))
		if err != nil || retry == nil || !retry.Created || retry.Attempt.Sequence != 2 || retry.Attempt.Reference == reserved.Attempt.Reference {
			t.Fatalf("release did not allow a fresh attempt: %+v, %v", retry, err)
		}
		assertDirectShopOrderStatus(t, f.options.OrderStore, order, constants.OrderStatusFulfilling, now)
		return
	}
	if notify && emailQueue.calls != 1 {
		t.Fatalf("manual completion and replay queued %d emails, want one", emailQueue.calls)
	}
	var delivery fulfillmentdomain.Fulfillment
	if err := f.db.Where("order_id = ?", childID).First(&delivery).Error; err != nil || delivery.DeliveredBy == nil || *delivery.DeliveredBy != adminID || delivery.Payload != "" {
		t.Fatalf("invalid manual delivery audit: %+v, %v", delivery, err)
	}
	assertDirectShopOrderStatus(t, f.options.OrderStore, order, constants.OrderStatusCompleted, now)
	record, err := f.store.GetRecharge(ctx, childID)
	if err != nil || record == nil || record.Stage != kafudadomain.StageCompleted || record.CompletedAt == nil {
		t.Fatalf("manual completion did not update recharge: %+v, %v", record, err)
	}
}

func assertDirectShopOrderStatus(t *testing.T, orders ordercontract.Store, original *orderdomain.Order, status string, paidAt time.Time) {
	t.Helper()
	parent, err := orders.GetByID(original.ID)
	if err != nil || parent == nil || len(parent.Children) != 1 || parent.Children[0].ID != original.Children[0].ID {
		t.Fatalf("direct order lost its child association: %+v, %v", parent, err)
	}
	child, originalChild := parent.Children[0], original.Children[0]
	for i, row := range []orderdomain.Order{*parent, child} {
		before := []orderdomain.Order{*original, originalChild}[i]
		if row.Status != status || row.PaidAt == nil || !row.PaidAt.Truncate(time.Microsecond).Equal(paidAt.Truncate(time.Microsecond)) ||
			!row.TotalAmount.Decimal.Equal(before.TotalAmount.Decimal) || row.Currency != before.Currency || !row.RefundedAmount.IsZero() {
			t.Fatalf("order %d did not preserve payment facts in status %s: %+v", row.ID, status, row)
		}
	}
	if len(child.Items) != 1 || child.Items[0].FulfillmentType != constants.FulfillmentTypeKafudaGPT ||
		child.Items[0].SKUID != originalChild.Items[0].SKUID || child.Items[0].ProductID != originalChild.Items[0].ProductID {
		t.Fatalf("direct order changed its original fulfillment item: %+v", child.Items)
	}
}

func TestDirectManualOutcomeUpdatesParentAndChildAtomically(t *testing.T) {
	for _, test := range []struct {
		name  string
		stage kafudadomain.Stage
	}{
		{"unidentified_result", kafudadomain.StageManualReview},
		{"needs_review", kafudadomain.StageManualReview},
		{"failed", kafudadomain.StageManualDelivery},
		{"cancelled", kafudadomain.StageManualDelivery},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newDirectCheckoutFixture(t)
			if err := f.db.AutoMigrate(&kafudadomain.Attempt{}); err != nil {
				t.Fatal(err)
			}
			order, err := orderapp.NewOrderService(f.options).CreateOrder(f.input)
			if err != nil || order == nil || len(order.Children) != 1 {
				t.Fatalf("create checkout: %+v, %v", order, err)
			}
			childID := order.Children[0].ID
			orders, ctx, now := f.options.OrderStore, context.Background(), time.Now().UTC()
			if err := f.db.Model(&orderdomain.Order{}).Where("id IN ?", []uint{order.ID, childID}).
				Updates(map[string]any{"status": constants.OrderStatusPaid, "paid_at": now}).Error; err != nil {
				t.Fatal(err)
			}
			reservation, err := kafudaworkflow.ReserveBuyerAttempt(orders, ordercontract.TenantScope{}, order.OrderNo, order.UserID, now, now.Add(time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			id := reservation.Attempt.ID
			records := kafudaworkflow.WithShopOrderTransitions(kafudaapp.New(f.store), orders)
			if err := records.MarkSubmitting(ctx, id, "test-quote", now, now.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			before, err := f.store.GetRecharge(ctx, childID)
			if err != nil || before == nil {
				t.Fatalf("load original recharge: %+v, %v", before, err)
			}
			originalAttempt, err := f.store.GetAttempt(ctx, id)
			if err != nil || originalAttempt == nil {
				t.Fatalf("load original attempt: %+v, %v", originalAttempt, err)
			}
			observedAt := now.Add(time.Second)
			apply := func() error {
				if test.name == "unidentified_result" {
					return records.MarkUnknown(ctx, id, observedAt, observedAt.Add(time.Minute))
				}
				result := kafudacontract.OrderResult{ID: 101, Reference: originalAttempt.Reference, Product: "gpt", Plan: "plus", Country: "PH", Status: test.name}
				completed, err := kafudaworkflow.ApplyResult(orders, childID, id, result, observedAt, observedAt, time.Minute)
				if completed {
					t.Fatal("manual outcome completed the order")
				}
				return err
			}
			trigger := fmt.Sprintf("CREATE TRIGGER reject_manual_parent BEFORE UPDATE ON orders WHEN OLD.id = %d AND NEW.status = 'fulfilling' BEGIN SELECT RAISE(ABORT, 'manual parent update failed'); END", order.ID)
			if err := f.db.Exec(trigger).Error; err != nil {
				t.Fatal(err)
			}
			if err := apply(); err == nil || !strings.Contains(err.Error(), "manual parent update failed") {
				t.Fatalf("expected the parent status write error, got %v", err)
			}
			assertDirectShopOrderStatus(t, orders, order, constants.OrderStatusPaid, now)
			record, err := f.store.GetRecharge(ctx, childID)
			if err != nil || !reflect.DeepEqual(record, before) {
				t.Fatalf("failed transition did not roll back recharge: %+v, %v", record, err)
			}
			attempt, err := f.store.GetAttempt(ctx, id)
			if err != nil || !reflect.DeepEqual(attempt, originalAttempt) {
				t.Fatalf("failed transition did not roll back attempt: %+v, %v", attempt, err)
			}
			if err := f.db.Exec("DROP TRIGGER reject_manual_parent").Error; err != nil {
				t.Fatal(err)
			}
			if err := apply(); err != nil {
				t.Fatalf("retry manual transition: %v", err)
			}
			assertDirectShopOrderStatus(t, orders, order, constants.OrderStatusFulfilling, now)
			record, err = f.store.GetRecharge(ctx, childID)
			if err != nil || record == nil || record.Stage != test.stage || record.CurrentAttemptID == nil || *record.CurrentAttemptID != id ||
				!reflect.DeepEqual(record.Snapshot, before.Snapshot) {
				t.Fatalf("manual transition lost the original recharge data: %+v, %v", record, err)
			}
			attempt, err = f.store.GetAttempt(ctx, id)
			if err != nil || attempt == nil || attempt.Reference != originalAttempt.Reference || attempt.QuoteID != originalAttempt.QuoteID {
				t.Fatalf("manual transition lost the original attempt data: %+v, %v", attempt, err)
			}
		})
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
		return kafudaworkflow.ApplyResult(f.options.OrderStore, childID, attempt.ID, result, now, now, 0)
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
	testDirectCheckoutDiscountsMatchPreviewAndSnapshot(t, newDirectCheckoutFixture)
}

func testDirectCheckoutDiscountsMatchPreviewAndSnapshot(t *testing.T, fixture func(*testing.T) directCheckoutFixture) {
	t.Helper()
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
			f := fixture(t)
			amount := func(value int64) money.Amount { return money.FromDecimal(decimal.NewFromInt(value)) }
			if err := f.db.AutoMigrate(&memberdomain.MemberLevel{}, &memberdomain.MemberLevelPrice{}, &memberdomain.UserProductPrice{}); err != nil {
				t.Fatal(err)
			}
			promotions := promotionapp.NewAdminService(promotionstore.New(f.db))
			if _, err := promotions.Create(promotionapp.CreatePromotionInput{
				Name: "direct discount", ScopeRefID: f.product.ID,
				Type: constants.PromotionTypeFixed, Value: amount(scenario.promotion),
			}); err != nil {
				t.Fatal(err)
			}
			if scenario.wholesale > 0 {
				products := productadmin.NewAdminService(productadmin.Options{Products: productstore.NewProductStore(f.db)})
				tiers := []productdomain.WholesalePriceInput{{
					SKUID: f.sku.ID, MinQuantity: int(scenario.floor), UnitPrice: decimal.NewFromInt(scenario.wholesale),
				}}
				if _, err := products.UpdateWholesalePrices(fmt.Sprint(f.product.ID), tiers); err != nil {
					t.Fatal(err)
				}
			}
			members := memberapp.NewService(memberstore.NewLevelStore(f.db), memberstore.NewPriceStore(f.db), memberstore.NewUserStore(f.db), memberstore.NewUserPriceStore(f.db))
			if scenario.memberRate > 0 {
				level := &memberdomain.MemberLevel{Slug: "direct-member", NameJSON: jsonmap.JSON{"en-US": "Member"}, DiscountRate: amount(scenario.memberRate), IsActive: true}
				if err := members.CreateLevel(level); err != nil {
					t.Fatal(err)
				}
				if err := members.SetUserLevel(f.users[0].ID, level.ID); err != nil {
					t.Fatal(err)
				}
			}
			if scenario.exclusive > 0 {
				price := memberdomain.UserProductPrice{UserID: f.users[0].ID, ProductID: f.product.ID, SKUID: f.sku.ID, PriceAmount: amount(scenario.exclusive)}
				if err := members.BatchUpsertUserPrices([]memberdomain.UserProductPrice{price}); err != nil {
					t.Fatal(err)
				}
			}
			f.options.MemberLevelService = members
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

func stoppedDirectSalesContainer(t *testing.T, f directCheckoutFixture) *container.Container {
	t.Helper()
	cfg := &config.Config{KafudaGPT: config.KafudaGPTConfig{SalesEnabled: false}}
	commerce := directCheckoutCommerce(f.store, cfg.KafudaGPT.SalesEnabled)
	options := f.options
	options.DirectCatalog = commerce
	orders := orderapp.NewOrderService(options)
	if _, err := orders.PreviewOrder(f.input); !errors.Is(err, orderapp.ErrProductNotAvailable) {
		t.Fatalf("preview after sales stopped: %v", err)
	}
	if _, err := orders.CreateOrder(f.input); !errors.Is(err, orderapp.ErrProductNotAvailable) {
		t.Fatalf("checkout after sales stopped: %v", err)
	}
	return &container.Container{
		Config:     cfg,
		OrderStore: options.OrderStore, OrderService: orders,
		KafudaGPT: &kafudawiring.Module{Store: f.store, Commerce: commerce},
	}
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
