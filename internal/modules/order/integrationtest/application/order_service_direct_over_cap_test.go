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
	"sync"
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
	userauthapp "github.com/dujiao-next/internal/modules/identity/userauth/application"
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
	gateway := newDirectTLSGateway(t, supplierToken, func(w http.ResponseWriter, r *http.Request) {
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
	})
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

func TestDirectSupplierRecovery(t *testing.T) {
	testDirectSupplierRecovery(t, newDirectCheckoutFixture)
}

func testDirectSupplierRecovery(t *testing.T, newFixture func(*testing.T) directCheckoutFixture) {
	t.Helper()
	zero, overCap, lower := int64(0), int64(2_501), int64(2_000)
	for _, scenario := range []struct {
		name          string
		terminal      kafudadomain.AttemptStatus
		verifiedDebit *int64
		retryDebit    *int64
		replaceLease  bool
	}{
		{name: "completed", terminal: kafudadomain.AttemptCompleted},
		{name: "cancelled", terminal: kafudadomain.AttemptCancelled},
		{name: "verified_zero_after_rollback", terminal: kafudadomain.AttemptCompleted, verifiedDebit: &zero},
		{name: "verified_over_cap_after_rollback", terminal: kafudadomain.AttemptCompleted, verifiedDebit: &overCap, retryDebit: &lower},
		{name: "replaced_lease", terminal: kafudadomain.AttemptCompleted, replaceLease: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			terminal := scenario.terminal
			f := newFixture(t)
			if err := f.db.AutoMigrate(&kafudadomain.Attempt{}); err != nil {
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
			initial, err := f.store.GetRecharge(ctx, childID)
			if err != nil || initial == nil {
				t.Fatalf("load paid order snapshot: %v", err)
			}
			snapshot := initial.Snapshot
			records := kafudaworkflow.WithShopOrderTransitions(kafudaapp.New(f.store), orders)
			const upstreamID = uint64(9_007_199_254_740_993)
			const merchantToken = "direct-recovery-fixture-token"
			const buyerSession = "direct-recovery-fixture-session"
			const originalQuoteID = "recovery-quote-1"
			var checks, submissions, lookups atomic.Int32
			var originalRequest, retryRequest atomic.Pointer[kafudacontract.CreateOrderRequest]
			firstLookupStarted := make(chan struct{})
			finishFirstLookup := make(chan struct{})
			supplierOrder := func(id uint64, reference string, status kafudadomain.AttemptStatus) map[string]any {
				return map[string]any{"id": id, "reference": reference, "product": snapshot.Product, "plan": snapshot.Plan, "country": snapshot.Country, "status": status}
			}
			gateway := newDirectTLSGateway(t, merchantToken, func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer "+merchantToken {
					t.Error("missing supplier authentication")
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/api/v1/gpt/check":
					var input kafudacontract.CheckRequest
					if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.Session != buyerSession ||
						input.Country != snapshot.Country || input.PayMode != snapshot.PayMode || input.ProductCode != snapshot.ProductCode {
						t.Error("eligibility check did not use the buyer session and paid snapshot")
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					quote := kafudacontract.Quote{QuoteID: fmt.Sprintf("recovery-quote-%d", checks.Add(1)), Plan: snapshot.Plan,
						Status: "available", Kind: "new", ExpiresAt: time.Now().Add(time.Minute), PriceCents: 900,
						AutoCard: &kafudacontract.AutoCard{ProductCode: snapshot.ProductCode, LoadCents: 100, TotalCents: 1000}}
					_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
						"check_id": "recovery-check", "country": snapshot.Country, "pay_mode": snapshot.PayMode, "region_lock": nil, "plans": []kafudacontract.Quote{quote},
					}})
					return
				case r.Method == http.MethodPost && r.URL.Path == "/api/v1/gpt/orders":
					var input kafudacontract.CreateOrderRequest
					decoder := json.NewDecoder(r.Body)
					decoder.DisallowUnknownFields()
					call := submissions.Add(1)
					if err := decoder.Decode(&input); err != nil || input.QuoteID != fmt.Sprintf("recovery-quote-%d", call) ||
						input.Reference == "" || r.Header.Get("Idempotency-Key") != input.Reference {
						t.Error("unexpected supplier order request")
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					if call == 1 {
						originalRequest.Store(&input)
						w.WriteHeader(http.StatusBadGateway)
						return
					}
					retryRequest.Store(&input)
					_ = json.NewEncoder(w).Encode(map[string]any{"data": supplierOrder(upstreamID+1, input.Reference, kafudadomain.AttemptProcessing)})
					return
				}
				original := originalRequest.Load()
				if r.Method != http.MethodGet || r.URL.RequestURI() != fmt.Sprintf("/api/v1/gpt/orders/%d", upstreamID) || original == nil {
					t.Error("unexpected supplier request during recovery")
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				status := terminal
				var debit *int64
				switch lookups.Add(1) {
				case 1:
					close(firstLookupStarted)
					select {
					case <-finishFirstLookup:
					case <-r.Context().Done():
					}
					if !scenario.replaceLease {
						w.WriteHeader(http.StatusBadGateway)
						return
					}
				case 3:
					w.WriteHeader(http.StatusBadGateway)
					return
				case 2:
					if scenario.verifiedDebit == nil {
						status = kafudadomain.AttemptProcessing
					} else {
						debit = scenario.verifiedDebit
					}
				case 4:
					debit = scenario.retryDebit
				default:
					t.Error("unexpected supplier lookup after settlement")
				}
				data := supplierOrder(upstreamID, original.Reference, status)
				if debit != nil {
					data["charged_usd_cents"] = *debit
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
			})
			deps := stoppedDirectSalesContainer(t, f)
			deps.KafudaGPT.Gateway = gateway
			deps.KafudaGPT.Records = records
			deps.UserStore = f.options.UserStore
			operatorStore := &directSubmitStore{Store: orders}
			deps.OrderStore = operatorStore
			router, supportToken, _ := newDirectSupportRouter(t, f, deps)
			buyerSecret := rand.Text()
			buyerAuth := userauthapp.NewService(&config.Config{UserJWT: config.JWTConfig{SecretKey: buyerSecret}}, f.options.UserStore, nil, nil, nil, nil, nil)
			buyerToken, _, err := buyerAuth.GenerateUserJWT(&f.users[0], 1)
			if err != nil {
				t.Fatal(err)
			}
			buyerRoutes := router.Group("/api/v1", middleware.UserJWTAuthMiddleware(buyerSecret, f.options.UserStore))
			userHandler := orderwiring.New(deps).User
			orderhttp.RegisterUserReadRoutes(buyerRoutes, userHandler)
			orderhttp.RegisterUserDirectAuthorizationRoute(buyerRoutes, userHandler)
			send := func(method, path, body, token string) []byte {
				req := httptest.NewRequestWithContext(ctx, method, path, strings.NewReader(body))
				req.Header.Set("Authorization", "Bearer "+token)
				req.Header.Set("Content-Type", "application/json")
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, req)
				return rec.Body.Bytes()
			}
			assertCode := func(raw []byte, wantCode int) {
				t.Helper()
				var envelope response.Response
				if err := json.Unmarshal(raw, &envelope); err != nil || envelope.StatusCode != wantCode {
					t.Fatalf("request returned code %d, want %d, decode error %v: %s", envelope.StatusCode, wantCode, err, raw)
				}
			}
			request := func(method, path, body string, wantCode int) []byte {
				t.Helper()
				raw := send(method, directAdminRoutePrefix+path, body, supportToken)
				assertCode(raw, wantCode)
				return raw
			}
			buyerOrderPath := "/api/v1/orders/" + order.OrderNo
			buyerRequest := func(method, operation, body string, wantCode int) []byte {
				t.Helper()
				raw := send(method, buyerOrderPath+operation, body, buyerToken)
				assertCode(raw, wantCode)
				return raw
			}
			submit := func(wantCode int) {
				t.Helper()
				buyerRequest(http.MethodPost, "/direct-authorize", fmt.Sprintf(`{"session":%q}`, buyerSession), wantCode)
			}
			assertBuyerStage := func(want kafudadomain.Stage) {
				t.Helper()
				raw := buyerRequest(http.MethodGet, "/direct-status", "", response.CodeOK)
				var envelope struct{ Data orderhttp.DirectStatus }
				if err := json.Unmarshal(raw, &envelope); err != nil || envelope.Data.Stage != string(want) {
					t.Fatalf("buyer sees %q, want %q: %v", envelope.Data.Stage, want, err)
				}
			}
			submit(response.CodeInternal)
			original := originalRequest.Load()
			review, err := f.store.GetRecharge(ctx, childID)
			if err != nil || original == nil || review == nil || review.CurrentAttemptID == nil || review.AttemptCount != 1 || !reflect.DeepEqual(review.Snapshot, snapshot) {
				t.Fatalf("buyer submission did not retain the original paid attempt: %v", err)
			}
			attemptID, reference := *review.CurrentAttemptID, original.Reference
			loadAttempt := func() *kafudadomain.Attempt {
				t.Helper()
				attempt, err := f.store.GetAttempt(ctx, attemptID)
				if err != nil || attempt == nil || attempt.Reference != reference || attempt.QuoteID != originalQuoteID ||
					attempt.QuoteCosts == nil || attempt.QuoteCosts.TotalCents != 1000 {
					t.Fatalf("lost original submission: %+v, %v", attempt, err)
				}
				return attempt
			}
			unknown := loadAttempt()
			if unknown.Status != kafudadomain.AttemptReview || unknown.ActiveOrderID == nil || *unknown.ActiveOrderID != childID || unknown.UpstreamOrderID != nil {
				t.Fatalf("uncertain submission did not keep the original hold: %+v", unknown)
			}
			assertBuyerStage(kafudadomain.StageManualReview)
			submit(response.CodeOK)
			if checks.Load() != 1 || submissions.Load() != 1 {
				t.Fatalf("buyer replay reached the supplier: checks=%d, submissions=%d", checks.Load(), submissions.Load())
			}
			assertReviewList := func(want int) {
				t.Helper()
				body := request(http.MethodGet, "/orders?direct_review=true", "", response.CodeOK)
				var page struct {
					Data       []struct{ ID uint }
					Pagination response.Pagination
				}
				if err := json.Unmarshal(body, &page); err != nil || page.Pagination.Total != int64(want) || len(page.Data) != want || (want == 1 && page.Data[0].ID != order.ID) {
					t.Fatalf("review list should contain %d matching parents: %s, %v", want, body, err)
				}
			}
			release := func(confirmed bool, wantCode int) {
				t.Helper()
				request(http.MethodPost, fmt.Sprintf("/orders/%d/direct-release", childID), fmt.Sprintf(`{"attempt_id":%d,"confirmed":%t}`, attemptID, confirmed), wantCode)
			}
			reconcilePath := fmt.Sprintf("/orders/%d/direct-reconcile", childID)
			lookupBody := fmt.Sprintf(`{"attempt_id":%d,"upstream_order_id":"%d"}`, attemptID, upstreamID)
			assertReviewList(1)
			release(true, response.CodeBadRequest)
			// A lease left by an interrupted lookup must allow recovery at expiry.
			if err := f.db.Model(&kafudadomain.Attempt{}).Where("id = ?", attemptID).
				UpdateColumn("known_order_poll_lease_until", paidAt).Error; err != nil {
				t.Fatal(err)
			}
			firstResponse := make(chan []byte, 1)
			go func() {
				firstResponse <- send(http.MethodPost, directAdminRoutePrefix+reconcilePath, lookupBody, supportToken)
			}()
			unblockLookup := sync.OnceFunc(func() { close(finishFirstLookup) })
			defer unblockLookup()
			select {
			case <-firstLookupStarted:
			case early := <-firstResponse:
				t.Fatalf("first reconciliation returned before contacting the supplier: %s", early)
			case <-time.After(5 * time.Second):
				t.Fatal("first reconciliation did not reach supplier")
			}
			duplicateResponse := make(chan []byte, 1)
			go func() {
				duplicateResponse <- send(http.MethodPost, directAdminRoutePrefix+reconcilePath, lookupBody, supportToken)
			}()
			var duplicate []byte
			select {
			case duplicate = <-duplicateResponse:
			case <-time.After(5 * time.Second):
				t.Fatal("duplicate reconciliation blocked behind supplier HTTP")
			}
			var replacementLease time.Time
			firstCode := response.CodeInternal
			if scenario.replaceLease {
				firstCode = response.CodeBadRequest
				replacementLease = time.Now().UTC().Add(2 * time.Minute).Truncate(time.Microsecond)
				if err := f.db.Model(&kafudadomain.Attempt{}).Where("id = ?", attemptID).
					UpdateColumn("known_order_poll_lease_until", replacementLease).Error; err != nil {
					t.Fatal(err)
				}
			}
			unblockLookup()
			assertCode(<-firstResponse, firstCode)
			assertCode(duplicate, response.CodeBadRequest)
			if got := lookups.Load(); got != 1 {
				t.Fatalf("concurrent reconciliation issued %d supplier lookups, want one", got)
			}
			stored := loadAttempt()
			if scenario.replaceLease {
				if stored.KnownOrderPollLeaseUntil == nil || !stored.KnownOrderPollLeaseUntil.Equal(replacementLease) {
					t.Fatalf("stale response released the newer lookup lease: %+v", stored)
				}
				stored.KnownOrderPollLeaseUntil = nil
				if err := f.db.Model(&kafudadomain.Attempt{}).Where("id = ?", attemptID).
					UpdateColumn("known_order_poll_lease_until", paidAt).Error; err != nil {
					t.Fatal(err)
				}
			}
			if !reflect.DeepEqual(stored, unknown) {
				t.Fatalf("failed reconciliation changed the hold: %+v", stored)
			}
			wantPendingStatus := kafudadomain.AttemptProcessing
			wantReviewReason := "outcome_unknown"
			secondCode := response.CodeOK
			costHold := scenario.verifiedDebit != nil && *scenario.verifiedDebit > snapshot.MaxCostCents
			if scenario.verifiedDebit != nil {
				secondCode = response.CodeInternal
				wantPendingStatus = kafudadomain.AttemptReview
				wantReviewReason = kafudadomain.ReasonResultApplyFailed
				if costHold {
					wantReviewReason = kafudadomain.ReasonCostLimitExceeded
				}
				operatorStore.afterCommit = func() { operatorStore.rollbackErr = errors.New("rollback reconciliation apply") }
			}
			request(http.MethodPost, reconcilePath, lookupBody, secondCode)
			request(http.MethodPost, reconcilePath, lookupBody, response.CodeBadRequest)
			if stored := loadAttempt(); stored.Status != wantPendingStatus || stored.ActiveOrderID == nil || *stored.ActiveOrderID != childID ||
				stored.UpstreamOrderID == nil || *stored.UpstreamOrderID != upstreamID || stored.NextPollAt == nil ||
				stored.KnownOrderPollLeaseUntil != nil || !reflect.DeepEqual(stored.ChargedUSDCents, scenario.verifiedDebit) || lookups.Load() != 2 {
				t.Fatalf("numeric reconciliation did not retain the original hold and schedule: %+v, lookups=%d", stored, lookups.Load())
			}
			review, err = f.store.GetRecharge(ctx, childID)
			if err != nil || review == nil || review.Stage != kafudadomain.StageManualReview || review.ReasonCode != wantReviewReason ||
				review.CompletedAt != nil || review.CompletionEmailPending || !reflect.DeepEqual(review.Snapshot, snapshot) {
				t.Fatalf("unexpected recharge after verification: %+v, %v", review, err)
			}
			assertDirectShopOrderStatus(t, orders, order, constants.OrderStatusFulfilling, paidAt)
			var deliveries int64
			if err := f.db.Model(&fulfillmentdomain.Fulfillment{}).Where("order_id = ?", childID).Count(&deliveries).Error; err != nil || deliveries != 0 {
				t.Fatalf("initial supplier verification left %d delivery records: %v", deliveries, err)
			}
			poll := func() (*kafudaapp.KnownOrderPoll, time.Time, error) {
				t.Helper()
				// Make the scheduled lookup due without sleeping, while keeping the
				// real timestamps used by the support HTTP routes.
				startedAt := time.Now().UTC().Truncate(time.Microsecond)
				if err := f.db.Model(&kafudadomain.Attempt{}).Where("id = ?", attemptID).UpdateColumn("next_poll_at", startedAt).Error; err != nil {
					t.Fatal(err)
				}
				result, err := records.PollKnownOrder(ctx, ctx, gateway, attemptID, startedAt, deps.Config.KafudaPollInterval(), 0)
				return result, startedAt, err
			}
			if result, _, err := poll(); result != nil || !errors.Is(err, kafudaapp.ErrKnownOrderLookupFailed) {
				t.Fatalf("expected retryable supplier lookup failure: %+v, %v", result, err)
			}
			stored = loadAttempt()
			if stored.Status != wantPendingStatus || stored.ActiveOrderID == nil || stored.NextPollAt == nil ||
				!stored.NextPollAt.After(stored.UpdatedAt) || stored.KnownOrderPollLeaseUntil != nil {
				t.Fatalf("failed numeric lookup did not retain and reschedule the hold: %+v", stored)
			}
			assertReviewList(1)
			release(true, response.CodeBadRequest)
			result, startedAt, err := poll()
			if err != nil || result == nil || result.Status != string(terminal) || lookups.Load() != 4 {
				t.Fatalf("confirmed terminal supplier lookup: %+v, lookups=%d, %v", result, lookups.Load(), err)
			}
			receivedAt, interval := time.Now().UTC(), deps.Config.KafudaPollInterval()
			completed, err := kafudaworkflow.ApplyPolledResult(orders, childID, attemptID, *result, startedAt, receivedAt, interval, 0)
			if err != nil || completed != (terminal == kafudadomain.AttemptCompleted && !costHold) {
				t.Fatalf("apply supplier result: completed=%t, %v", completed, err)
			}
			settled := loadAttempt()
			if settled.Status != terminal || settled.ActiveOrderID != nil || settled.NextPollAt != nil || settled.UpstreamOrderID == nil || *settled.UpstreamOrderID != upstreamID {
				t.Fatalf("original attempt did not settle: %+v", settled)
			}
			wantDebit := scenario.verifiedDebit
			if scenario.retryDebit != nil {
				wantDebit = scenario.retryDebit
			}
			if !reflect.DeepEqual(settled.ChargedUSDCents, wantDebit) {
				t.Fatalf("known lookup lost the verified debit: %+v", settled)
			}
			wantSubmissions := int32(1)
			if costHold {
				assertDirectShopOrderStatus(t, orders, order, constants.OrderStatusFulfilling, paidAt)
				review, err := f.store.GetRecharge(ctx, childID)
				if err != nil || !kafudacontract.CanConfirmUpstreamCompletion(review, settled) {
					t.Fatalf("lower debit removed the cost hold: %+v, %v", review, err)
				}
				release(true, response.CodeBadRequest)
			} else if completed {
				assertDirectShopOrderStatus(t, orders, order, constants.OrderStatusCompleted, paidAt)
				assertBuyerStage(kafudadomain.StageCompleted)
				submit(response.CodeBadRequest)
				release(true, response.CodeBadRequest)
				if replay, err := kafudaworkflow.ApplyPolledResult(orders, childID, attemptID, *result, startedAt, receivedAt, interval, 0); err != nil || replay {
					t.Fatalf("completion replay: completed=%t, %v", replay, err)
				}
			} else {
				assertReviewList(1)
				submit(response.CodeBadRequest)
				release(false, response.CodeBadRequest)
				release(true, response.CodeOK)
				release(true, response.CodeBadRequest)
				assertBuyerStage(kafudadomain.StageWaiting)
				submit(response.CodeOK)
				wantSubmissions = 2
				fresh, err := f.store.GetRecharge(ctx, childID)
				if err != nil || fresh == nil || fresh.CurrentAttemptID == nil || *fresh.CurrentAttemptID == attemptID || fresh.AttemptCount != 2 ||
					!reflect.DeepEqual(fresh.Snapshot, snapshot) {
					t.Fatalf("confirmed release did not allow a fresh attempt for the paid snapshot: %+v, %v", fresh, err)
				}
				retry, err := f.store.GetAttempt(ctx, *fresh.CurrentAttemptID)
				newRequest := retryRequest.Load()
				if err != nil || retry == nil || newRequest == nil || retry.Reference == reference || retry.Reference != newRequest.Reference ||
					retry.QuoteID != newRequest.QuoteID || retry.Status != kafudadomain.AttemptProcessing {
					t.Fatalf("buyer retry did not submit a new quote and reference: %+v, %v", retry, err)
				}
				stale := *result
				stale.Status = string(kafudadomain.AttemptCompleted)
				if replay, err := kafudaworkflow.ApplyPolledResult(orders, childID, attemptID, stale, startedAt, receivedAt, interval, 0); replay || !errors.Is(err, kafudadomain.ErrConflict) {
					t.Fatalf("old supplier result after replacement: completed=%t, %v", replay, err)
				}
				assertDirectShopOrderStatus(t, orders, order, constants.OrderStatusFulfilling, paidAt)
			}
			if checks.Load() != wantSubmissions || submissions.Load() != wantSubmissions {
				t.Fatalf("unexpected supplier calls: checks=%d, submissions=%d, want %d each", checks.Load(), submissions.Load(), wantSubmissions)
			}
			if costHold {
				assertReviewList(1)
			} else {
				assertReviewList(0)
			}
			if historical := loadAttempt(); !reflect.DeepEqual(historical, settled) {
				t.Fatalf("replay or release changed terminal history: %+v", historical)
			}
			var wantDeliveries int64
			if completed {
				wantDeliveries = 1
			}
			if err := f.db.Model(&fulfillmentdomain.Fulfillment{}).Where("order_id = ?", childID).Count(&deliveries).Error; err != nil || deliveries != wantDeliveries {
				t.Fatalf("found %d deliveries, want %d: %v", deliveries, wantDeliveries, err)
			}
		})
	}
}

func newDirectTLSGateway(t *testing.T, token string, handler http.HandlerFunc) *kafudaclient.Client {
	t.Helper()
	supplier := httptest.NewTLSServer(handler)
	t.Cleanup(supplier.Close)
	// Route only this loopback origin through the transport that trusts the
	// fixture certificate. The caller must run serially.
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
	gateway, err := kafudaclient.New(supplier.URL, token)
	if err != nil {
		t.Fatal(err)
	}
	return gateway
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
