package application_test

import (
	"crypto/rand"
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

	"github.com/dujiao-next/internal/app/container"
	"github.com/dujiao-next/internal/app/httpserver/middleware"
	"github.com/dujiao-next/internal/authz"
	orderwiring "github.com/dujiao-next/internal/bootstrap/order"
	"github.com/dujiao-next/internal/config"
	"github.com/dujiao-next/internal/constants"
	fulfillmentdomain "github.com/dujiao-next/internal/modules/fulfillment/domain"
	admindomain "github.com/dujiao-next/internal/modules/identity/admin/domain"
	adminstore "github.com/dujiao-next/internal/modules/identity/admin/infrastructure/gormstore"
	adminauthapp "github.com/dujiao-next/internal/modules/identity/adminauth/application"
	kafudaapp "github.com/dujiao-next/internal/modules/kafudagpt/application"
	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
	kafudaclient "github.com/dujiao-next/internal/modules/kafudagpt/infrastructure/kafuda"
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
	"github.com/shopspring/decimal"
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

const directAdminRoutePrefix = "/api/v1/admin"

func newDirectSupportRouter(t *testing.T, f directCheckoutFixture, deps *container.Container) (*gin.Engine, string, *admindomain.Admin) {
	t.Helper()
	if err := f.db.AutoMigrate(&admindomain.Admin{}); err != nil {
		t.Fatal(err)
	}
	admins := adminstore.New(f.db)
	admin := &admindomain.Admin{Username: "direct-support", PasswordHash: "fixture-login-disabled"}
	if err := admins.Create(admin); err != nil {
		t.Fatal(err)
	}
	permissions, err := authz.NewService(f.db)
	if err != nil {
		t.Fatal(err)
	}
	if err := permissions.BootstrapBuiltinRoles(); err != nil {
		t.Fatal(err)
	}
	if err := permissions.SetAdminRoles(admin.ID, []string{"support"}); err != nil {
		t.Fatal(err)
	}
	secret := rand.Text()
	adminAuth := adminauthapp.NewService(&config.Config{JWT: config.JWTConfig{SecretKey: secret, ExpireHours: 1}}, admins)
	token, _, err := adminAuth.GenerateJWT(admin)
	if err != nil {
		t.Fatal(err)
	}
	handlers := orderwiring.New(deps)
	router := gin.New()
	adminRoutes := router.Group(directAdminRoutePrefix, middleware.JWTAuthMiddleware(secret, admins), middleware.AdminRBACMiddleware(permissions))
	orderhttp.RegisterAdminRoutes(adminRoutes, handlers.Admin)
	orderhttp.RegisterAdminRefundWriteRoutes(adminRoutes, handlers.AdminRefund)
	return router, token, admin
}

func TestDirectSupplierPrivacyAndRefundBoundary(t *testing.T) {
	testDirectSupplierPrivacyAndRefundBoundary(t, newDirectCheckoutFixture(t))
}

func testDirectSupplierPrivacyAndRefundBoundary(t *testing.T, f directCheckoutFixture) {
	t.Helper()
	if err := f.db.AutoMigrate(&kafudadomain.Attempt{}, &paymentdomain.Payment{}, &orderdomain.OrderRefundRecord{}, &walletdomain.Account{}, &walletdomain.Transaction{}); err != nil {
		t.Fatal(err)
	}
	ctx, orders := t.Context(), f.options.OrderStore
	order, err := orderapp.NewOrderService(f.options).CreateOrder(f.input)
	if err != nil || order == nil || len(order.Children) != 1 {
		t.Fatalf("create direct checkout: %+v, %v", order, err)
	}
	childID, paidAt := order.Children[0].ID, time.Now().UTC().Truncate(time.Microsecond)
	if err := f.db.Model(&orderdomain.Order{}).Where("id IN ?", []uint{order.ID, childID}).
		Updates(map[string]any{"status": constants.OrderStatusPaid, "paid_at": paidAt}).Error; err != nil {
		t.Fatal(err)
	}
	records := kafudaworkflow.WithShopOrderTransitions(kafudaapp.New(f.store), orders)
	reserved, err := records.Reserve(ctx, childID, paidAt, paidAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	attemptID := reserved.Attempt.ID
	if err := records.MarkSubmitting(ctx, attemptID, "privacy-test-quote", paidAt, paidAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := records.MarkUnknown(ctx, attemptID, paidAt, paidAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	unknown, err := f.store.GetAttempt(ctx, attemptID)
	if err != nil || unknown == nil || unknown.Status != kafudadomain.AttemptReview {
		t.Fatalf("expected an unknown supplier attempt: %+v, %v", unknown, err)
	}
	initialBalance := money.FromDecimal(decimal.NewFromInt(7))
	account := walletdomain.Account{UserID: order.UserID, Balance: initialBalance}
	if err := f.db.Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	const upstreamID = uint64(9_007_199_254_740_993)
	const supplierToken = "privacy-test-supplier-token"
	const supplierEmail = "private-activation@example.test"
	const supplierSession = "privacy-test-supplier-session"
	const supplierError = "privacy-test-raw-upstream-error"
	const supplierRefundCents = 137
	const chargedCents = 923
	const maskedEmail = "p***@***"
	var lookups atomic.Int32
	supplier := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.RequestURI() != fmt.Sprintf("/api/v1/gpt/orders/%d", upstreamID) ||
			r.Header.Get("Authorization") != "Bearer "+supplierToken {
			t.Error("unexpected supplier lookup method, target or credential")
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		status := "failed"
		switch lookups.Add(1) {
		case 1:
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": supplierError, "message": supplierEmail + " " + supplierSession}})
			return
		case 2:
			status = "refunded"
		}
		snapshot := reserved.Recharge.Snapshot
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
			"id": upstreamID, "reference": reserved.Attempt.Reference, "product": snapshot.Product,
			"plan": snapshot.Plan, "country": snapshot.Country, "status": status,
			"account_email": supplierEmail, "session": supplierSession, "api_token": supplierToken,
			"failure_message": supplierError, "charged_usd_cents": chargedCents, "refund_usd_cents": supplierRefundCents,
		}})
	}))
	t.Cleanup(supplier.Close)
	// Keep production TLS verification and route only this exact loopback origin
	// through the transport that trusts the fixture certificate. This test is serial.
	originalTransport := http.DefaultTransport
	supplierTransport := supplier.Client().Transport
	supplierHost := strings.TrimPrefix(supplier.URL, "https://")
	http.DefaultTransport = directTestRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme == "https" && r.URL.Host == supplierHost {
			return supplierTransport.RoundTrip(r)
		}
		return originalTransport.RoundTrip(r)
	})
	t.Cleanup(func() { http.DefaultTransport = originalTransport })
	gateway, err := kafudaclient.New(supplier.URL, supplierToken)
	if err != nil {
		t.Fatal(err)
	}
	deps := stoppedDirectSalesContainer(t, f)
	deps.KafudaGPT.Gateway = gateway
	deps.UserStore = f.options.UserStore
	deps.PaymentStore = paymentstore.New(f.db, "test-guest-credential-secret-with-32-bytes")
	wallets := walletstore.New(f.db)
	deps.OrderRefundService = orderrefund.New(orders, nil, nil, nil, walletapp.NewService(walletapp.Options{Repository: wallets, Transactions: wallets}), nil)
	router, supportToken, _ := newDirectSupportRouter(t, f, deps)
	request := func(method, path, payload string, wantCode int) json.RawMessage {
		t.Helper()
		req := httptest.NewRequestWithContext(ctx, method, directAdminRoutePrefix+path, strings.NewReader(payload))
		req.Header.Set("Authorization", "Bearer "+supportToken)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		for _, secret := range []string{supplierEmail, supplierToken, supplierSession, supplierError, "session-must-not-persist", `"refund_usd_cents"`} {
			if strings.Contains(rec.Body.String(), secret) {
				t.Fatalf("supplier data escaped into %s %s", method, path)
			}
		}
		var envelope struct {
			Code int             `json:"status_code"`
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil || envelope.Code != wantCode {
			t.Fatalf("%s %s returned code %d, want %d, decode error %v", method, path, envelope.Code, wantCode, err)
		}
		return envelope.Data
	}
	assertUnrefunded := func() {
		t.Helper()
		assertDirectShopOrderStatus(t, orders, order, constants.OrderStatusFulfilling, paidAt)
		for _, model := range []any{&orderdomain.OrderRefundRecord{}, &walletdomain.Transaction{}, &fulfillmentdomain.Fulfillment{}} {
			var count int64
			if err := f.db.Model(model).Count(&count).Error; err != nil || count != 0 {
				t.Fatalf("unexpected %T rows after supplier lookup: %d, %v", model, count, err)
			}
		}
		var stored walletdomain.Account
		if err := f.db.First(&stored, account.ID).Error; err != nil || !stored.Balance.Equal(initialBalance.Decimal) {
			t.Fatalf("supplier lookup changed buyer balance: %s, %v", stored.Balance, err)
		}
	}
	reconcilePath := fmt.Sprintf("/orders/%d/direct-reconcile", childID)
	lookupBody := fmt.Sprintf(`{"attempt_id":%d,"upstream_order_id":"%d"}`, attemptID, upstreamID)
	for range 2 {
		request(http.MethodPost, reconcilePath, lookupBody, response.CodeInternal)
		assertUnrefunded()
		stored, err := f.store.GetAttempt(ctx, attemptID)
		if err != nil || !reflect.DeepEqual(stored, unknown) {
			t.Fatalf("rejected supplier response changed the unknown attempt: %v", err)
		}
	}
	request(http.MethodPost, reconcilePath, lookupBody, response.CodeOK)
	if lookups.Load() != 3 {
		t.Fatalf("supplier received %d numeric lookups, want 3", lookups.Load())
	}
	assertUnrefunded()
	for _, orderID := range []uint{order.ID, childID} {
		raw := request(http.MethodGet, fmt.Sprintf("/orders/%d", orderID), "", response.CodeOK)
		var detail orderhttp.AdminOrderDetail
		if err := json.Unmarshal(raw, &detail); err != nil {
			t.Fatal(err)
		}
		direct := detail.DirectRecharge
		if direct == nil || direct.Stage != string(kafudadomain.StageManualDelivery) || direct.Reason != "upstream_failed" ||
			direct.Attempt == nil || direct.Attempt.Active || direct.Attempt.MaskedEmail != maskedEmail ||
			direct.Attempt.UpstreamOrderID != fmt.Sprint(upstreamID) || direct.Attempt.CardChargedUSD == nil || direct.Attempt.CardChargedUSD.String() != "9.23" {
			t.Fatalf("incorrect sanitized supplier projection for order %d", orderID)
		}
	}
	failed, err := f.store.GetAttempt(ctx, attemptID)
	if err != nil || failed == nil || failed.Status != kafudadomain.AttemptFailed || failed.MaskedEmail != maskedEmail {
		t.Fatalf("supplier failure was not stored with the masked account: %v", err)
	}
	refundPath := fmt.Sprintf("/orders/%d/direct-refund-to-wallet", order.ID)
	refundBody := func(amount string) string {
		return fmt.Sprintf(`{"attempt_id":%d,"confirmed":true,"amount":%q}`, attemptID, amount)
	}
	request(http.MethodPost, refundPath, refundBody(decimal.New(supplierRefundCents, -2).StringFixed(2)), response.CodeBadRequest)
	assertUnrefunded()
	request(http.MethodPost, refundPath, refundBody(order.TotalAmount.String()), response.CodeOK)
	refunded, err := orders.GetByID(order.ID)
	if err != nil || refunded == nil || refunded.Status != constants.OrderStatusRefunded || len(refunded.Children) != 1 ||
		refunded.Children[0].Status != constants.OrderStatusRefunded || !refunded.RefundedAmount.Equal(order.TotalAmount.Decimal) {
		t.Fatalf("explicit shop refund did not settle both orders for the local amount: %v", err)
	}
	var refunds []orderdomain.OrderRefundRecord
	if err := f.db.Find(&refunds).Error; err != nil || len(refunds) != 1 || refunds[0].OrderID != order.ID || refunds[0].UserID != order.UserID ||
		refunds[0].Type != constants.OrderRefundTypeWallet || refunds[0].Currency != order.Currency || !refunds[0].Amount.Equal(order.TotalAmount.Decimal) {
		t.Fatalf("expected one shop refund record for the local paid amount: count=%d, %v", len(refunds), err)
	}
	var ledger []walletdomain.Transaction
	finalBalance := initialBalance.Add(order.TotalAmount.Decimal)
	if err := f.db.Find(&ledger).Error; err != nil || len(ledger) != 1 || ledger[0].UserID != order.UserID || ledger[0].OrderID == nil ||
		*ledger[0].OrderID != order.ID || ledger[0].Type != constants.WalletTxnTypeAdminRefund || ledger[0].Direction != constants.WalletTxnDirectionIn ||
		ledger[0].Currency != order.Currency || !ledger[0].Amount.Equal(order.TotalAmount.Decimal) || !ledger[0].BalanceAfter.Equal(finalBalance) {
		t.Fatalf("expected one wallet credit for the actual shop refund: count=%d, %v", len(ledger), err)
	}
	if err := f.db.First(&account, account.ID).Error; err != nil || !account.Balance.Equal(finalBalance) {
		t.Fatalf("shop refund did not credit the local paid amount: %s, %v", account.Balance, err)
	}
}

type directTestRoundTripper func(*http.Request) (*http.Response, error)

func (transport directTestRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return transport(r)
}

func testOverCapDirectCompletionHTTP(t *testing.T, f overCapDirectFixture) {
	t.Helper()
	if err := f.db.AutoMigrate(&paymentdomain.Payment{}, &orderdomain.OrderRefundRecord{}, &walletdomain.Account{}, &walletdomain.Transaction{}); err != nil {
		t.Fatal(err)
	}
	container := stoppedDirectSalesContainer(t, f.directCheckoutFixture)
	container.UserStore = f.options.UserStore
	container.PaymentStore = paymentstore.New(f.db, "test-guest-credential-secret-with-32-bytes")
	wallets := walletstore.New(f.db)
	container.OrderRefundService = orderrefund.New(f.options.OrderStore, nil, nil, nil, walletapp.NewService(walletapp.Options{Repository: wallets, Transactions: wallets}), nil)
	router, token, admin := newDirectSupportRouter(t, f.directCheckoutFixture, container)
	send := func(method, path, body string) response.Response {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, directAdminRoutePrefix+path, strings.NewReader(body))
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
