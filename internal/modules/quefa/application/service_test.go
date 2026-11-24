package application

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	quefacontract "github.com/dujiao-next/internal/modules/quefa/contract"
	quefadomain "github.com/dujiao-next/internal/modules/quefa/domain"
	"github.com/dujiao-next/internal/modules/quefa/infrastructure/gormstore"
	"github.com/dujiao-next/internal/shared/money"
	"github.com/glebarez/sqlite"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

type stubGateway struct {
	productsRaw        json.RawMessage
	productsErr        error
	productsCalls      int
	createOrderFn      func(ctx context.Context, req quefacontract.CreateOrderRequest, idem string) (*quefacontract.OrderSnapshot, error)
	getOrderFn         func(ctx context.Context, id string) (*quefacontract.OrderSnapshot, error)
	lookupMerchantFn   func(ctx context.Context, no string) (*quefacontract.OrderSnapshot, error)
	createRedemptionFn func(ctx context.Context, req quefacontract.CreateRedemptionRequest, idem string) (*quefacontract.RedemptionSnapshot, error)
	getRedemptionFn    func(ctx context.Context, id string) (*quefacontract.RedemptionSnapshot, error)
}

func (g *stubGateway) GetProducts(ctx context.Context) (json.RawMessage, quefacontract.PollHints, error) {
	g.productsCalls++
	return g.productsRaw, quefacontract.PollHints{}, g.productsErr
}

func (g *stubGateway) CreateOrder(ctx context.Context, req quefacontract.CreateOrderRequest, idem string) (*quefacontract.OrderSnapshot, error) {
	return g.createOrderFn(ctx, req, idem)
}

func (g *stubGateway) CreateOrderPaymentCode(ctx context.Context, id, idem string) (*quefacontract.PaymentCodeSnapshot, error) {
	return nil, &quefacontract.APIError{StatusCode: 403, Code: "direct_payment_code_disabled", Category: "forbidden"}
}

func (g *stubGateway) GetOrder(ctx context.Context, id string) (*quefacontract.OrderSnapshot, error) {
	return g.getOrderFn(ctx, id)
}

func (g *stubGateway) LookupOrderByMerchantNo(ctx context.Context, no string) (*quefacontract.OrderSnapshot, error) {
	if g.lookupMerchantFn == nil {
		return nil, &quefacontract.APIError{StatusCode: 404, Category: "not_found"}
	}
	return g.lookupMerchantFn(ctx, no)
}

func (g *stubGateway) CreateRedemption(ctx context.Context, req quefacontract.CreateRedemptionRequest, idem string) (*quefacontract.RedemptionSnapshot, error) {
	if g.createRedemptionFn == nil {
		return nil, &quefacontract.APIError{StatusCode: 500, Category: "upstream_error"}
	}
	return g.createRedemptionFn(ctx, req, idem)
}

func (g *stubGateway) GetRedemption(ctx context.Context, id string) (*quefacontract.RedemptionSnapshot, error) {
	if g.getRedemptionFn == nil {
		return nil, &quefacontract.APIError{StatusCode: 404, Category: "not_found"}
	}
	return g.getRedemptionFn(ctx, id)
}

func setupAppTest(t *testing.T, gw *stubGateway) (*gormstore.Store, *ConfigService, *CatalogService, *OrderService) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	if err := db.AutoMigrate(
		&quefadomain.Setting{},
		&quefadomain.Catalog{},
		&quefadomain.Order{},
		&quefadomain.WebhookEvent{},
	); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	store := gormstore.New(db)
	cfgSvc := NewConfigService(context.Background(), ConfigServiceOptions{
		Store:        store,
		Profile:      quefadomain.DefaultProfile,
		AppSecretKey: "test-app-secret-key-32-bytes-ok!",
		Fallback: RuntimeConfig{
			APIBaseURL:        "https://tibo.ink",
			PartnerID:         "p1",
			KeyID:             "k1",
			ClientSecret:      "sec1",
			WebhookSecret:     "whsec1",
			CollectionMode:    quefadomain.CollectionModePlatformCollect,
			SalesEnabled:      true,
			AllowedPayDomains: []string{"pay.myshop.com", "*.alipay.com"},
			CatalogTTLSeconds: 3600,
		},
		GatewayFactory: func(baseURL, partnerID, keyID, clientSecret string) (quefacontract.Gateway, error) {
			return gw, nil
		},
	})
	catSvc := NewCatalogService(store, cfgSvc, quefadomain.DefaultProfile, func(context.Context) bool { return true })
	ordSvc := NewOrderService(store, cfgSvc)
	return store, cfgSvc, catSvc, ordSvc
}

func TestConfigEncryptionAndForbiddenDomainRejected(t *testing.T) {
	gw := &stubGateway{productsRaw: json.RawMessage(`{"products":[{"product_code":"plus_1m","supply_price":"100.00","currency":"CNY","available":true}]}`)}
	_, cfgSvc, _, _ := setupAppTest(t, gw)
	ctx := context.Background()

	// Forbidden domain tibo.ink must be rejected on save.
	badDomains := []string{"tibo.ink"}
	if _, err := cfgSvc.Update(ctx, ConfigPatch{AllowedPayDomains: &badDomains}); err != quefadomain.ErrPaymentDomainForbidden {
		t.Fatalf("expected ErrPaymentDomainForbidden, got %v", err)
	}

	// Secret encryption round-trip and AdminView redaction.
	newSecret := "super-secret-value"
	goodDomains := []string{"cashier.brand.com"}
	view, err := cfgSvc.Update(ctx, ConfigPatch{
		ClientSecret:      &newSecret,
		AllowedPayDomains: &goodDomains,
	})
	if err != nil {
		t.Fatalf("Update failed: %v", err)
	}
	if !view.ClientSecretConfigured || len(view.AllowedPayDomains) != 1 || view.AllowedPayDomains[0] != "cashier.brand.com" {
		t.Fatalf("unexpected admin view: %+v", view)
	}
	if cfgSvc.Snapshot().Config.ClientSecret != newSecret {
		t.Fatalf("expected secret decrypted in snapshot")
	}
}

func TestCatalogSyncPreservesLastValidOnFailureAndComputesCostFloor(t *testing.T) {
	gw := &stubGateway{
		productsRaw: json.RawMessage(`{"products":[{"product_code":"chatgpt_plus_1m","title":"Plus 1M","supply_price":"105.00","min_sale_price":"110.00","currency":"CNY","collection_modes":["platform_collect"],"delivery_modes":["auto_recharge"],"available":true}]}`),
	}
	_, _, catSvc, _ := setupAppTest(t, gw)
	ctx := context.Background()

	view, err := catSvc.Sync(ctx)
	if err != nil || len(view.Products) != 1 || view.Stale {
		t.Fatalf("expected clean sync, got view=%+v err=%v", view, err)
	}

	// Next sync fails -> previous valid snapshot must remain intact.
	gw.productsErr = &quefacontract.APIError{StatusCode: 503, Category: "upstream_error"}
	if _, err := catSvc.Sync(ctx); err == nil {
		t.Fatalf("expected sync error")
	}
	after, err := catSvc.Read(ctx)
	if err != nil || len(after.Products) != 1 || after.LastErrorCode != "upstream_error" {
		t.Fatalf("expected previous valid snapshot preserved, got %+v err=%v", after, err)
	}

	// Cost floor = max(adminCost, supplyFloor)
	terms, err := catSvc.ResolvePurchaseTerms(ctx, "chatgpt_plus_1m", decimal.NewFromInt(120), "CNY", false)
	if err != nil {
		t.Fatalf("ResolvePurchaseTerms failed: %v", err)
	}
	if terms.CostFloor.StringFixed(2) != "120.00" {
		t.Fatalf("expected cost floor 120.00, got %s", terms.CostFloor.StringFixed(2))
	}
	terms2, err := catSvc.ResolvePurchaseTerms(ctx, "chatgpt_plus_1m", decimal.NewFromInt(90), "CNY", false)
	if err != nil || terms2.CostFloor.StringFixed(2) != "110.00" {
		t.Fatalf("expected supply floor 110.00, got %+v err=%v", terms2, err)
	}
	// Wallet-only mode must block purchase.
	if _, err := catSvc.ResolvePurchaseTerms(ctx, "chatgpt_plus_1m", decimal.NewFromInt(120), "CNY", true); err != quefacontract.ErrWalletOnlyBlocked {
		t.Fatalf("expected ErrWalletOnlyBlocked, got %v", err)
	}
	// Currency mismatch must block purchase.
	if _, err := catSvc.ResolvePurchaseTerms(ctx, "chatgpt_plus_1m", decimal.NewFromInt(120), "USD", false); err != quefacontract.ErrCurrencyMismatch {
		t.Fatalf("expected ErrCurrencyMismatch, got %v", err)
	}
}

func TestPlatformCollectDomainBlockTimeoutRecoveryAndMonotonicCompletion(t *testing.T) {
	var createdCalls int
	gw := &stubGateway{}
	store, _, _, ordSvc := setupAppTest(t, gw)
	ctx := context.Background()
	now := time.Now().UTC()

	var paidCount, successCount int
	ordSvc.SetHooks(LifecycleHooks{
		OnPlatformOrderPaid: func(ctx context.Context, record *quefadomain.Order, snap *quefacontract.OrderSnapshot) error {
			paidCount++
			return nil
		},
		OnOrderSucceeded: func(ctx context.Context, record *quefadomain.Order) error {
			successCount++
			return nil
		},
	})

	err := store.CreateCheckout(quefacontract.CheckoutInput{
		OrderID:            201,
		ParentOrderID:      200,
		ProductID:          1,
		SKUID:              2,
		ProductCode:        "chatgpt_plus_1m",
		CollectionMode:     quefadomain.CollectionModePlatformCollect,
		DeliveryMode:       quefadomain.DeliveryModeAutoRecharge,
		CostFloorAmount:    money.FromDecimal(decimal.NewFromInt(110)),
		SettlementCurrency: "CNY",
		MerchantOrderNo:    "QF-200",
		CreatedAt:          now,
	})
	if err != nil {
		t.Fatalf("CreateCheckout failed: %v", err)
	}

	// Below cost floor must be rejected before calling upstream.
	if _, err := ordSvc.CreatePlatformCollectPayment(ctx, 200, decimal.NewFromInt(100), "https://shop.com/return"); err != quefacontract.ErrBelowCostFloor {
		t.Fatalf("expected ErrBelowCostFloor, got %v", err)
	}

	// Simulate timeout on CreateOrder followed by successful merchant_order_no recovery!
	gw.createOrderFn = func(ctx context.Context, req quefacontract.CreateOrderRequest, idem string) (*quefacontract.OrderSnapshot, error) {
		createdCalls++
		return nil, &quefacontract.APIError{Category: "network_error", Retryable: true}
	}
	gw.lookupMerchantFn = func(ctx context.Context, no string) (*quefacontract.OrderSnapshot, error) {
		return &quefacontract.OrderSnapshot{
			OrderID:         "qf_200",
			MerchantOrderNo: no,
			Status:          "pending",
			PayURL:          "https://pay.myshop.com/checkout/200",
			Currency:        "CNY",
		}, nil
	}

	out, err := ordSvc.CreatePlatformCollectPayment(ctx, 200, decimal.NewFromInt(135), "https://shop.com/return")
	if err != nil || out.PayURL != "https://pay.myshop.com/checkout/200" || out.UpstreamOrderID != "qf_200" {
		t.Fatalf("expected timeout recovery success, got out=%+v err=%v", out, err)
	}
	if createdCalls != 1 {
		t.Fatalf("expected 1 create call, got %d", createdCalls)
	}

	// Second call must reuse existing payment without calling upstream CreateOrder again.
	out2, err := ordSvc.CreatePlatformCollectPayment(ctx, 200, decimal.NewFromInt(135), "https://shop.com/return")
	if err != nil || out2.UpstreamOrderID != "qf_200" || createdCalls != 1 {
		t.Fatalf("expected idempotent reuse, got out=%+v calls=%d err=%v", out2, createdCalls, err)
	}

	// Cancel must be blocked while upstream is pending.
	gw.getOrderFn = func(ctx context.Context, id string) (*quefacontract.OrderSnapshot, error) {
		return &quefacontract.OrderSnapshot{OrderID: id, Status: "pending", Currency: "CNY"}, nil
	}
	eval, err := ordSvc.EvaluateCancelOrExpire(ctx, 200)
	if err != quefadomain.ErrCancelBlockedByActive || eval.CanClose {
		t.Fatalf("expected cancel blocked while pending, got eval=%+v err=%v", eval, err)
	}

	// Sync paid -> opens fulfillment entry.
	sale := decimal.NewFromInt(135)
	gw.getOrderFn = func(ctx context.Context, id string) (*quefacontract.OrderSnapshot, error) {
		return &quefacontract.OrderSnapshot{
			OrderID:        id,
			Status:         "paid",
			PaidAmount:     &sale,
			Currency:       "CNY",
			FulfillmentURL: "https://tibo.ink/redeem/token-200",
		}, nil
	}
	if err := ordSvc.SyncOrder(ctx, 201); err != nil {
		t.Fatalf("SyncOrder paid failed: %v", err)
	}
	if paidCount != 1 {
		t.Fatalf("expected OnPlatformOrderPaid called once, got %d", paidCount)
	}
	entry, err := ordSvc.OpenFulfillmentEntry(ctx, 200)
	if err != nil || entry.FulfillmentURL != "https://tibo.ink/redeem/token-200" {
		t.Fatalf("expected fulfillment entry, got entry=%+v err=%v", entry, err)
	}

	// Sync succeeded -> triggers completion once; subsequent stale running event must NOT regress!
	gw.getOrderFn = func(ctx context.Context, id string) (*quefacontract.OrderSnapshot, error) {
		return &quefacontract.OrderSnapshot{
			OrderID:         id,
			Status:          "succeeded",
			PaidAmount:      &sale,
			Currency:        "CNY",
			AttemptNo:       1,
			ProgressVersion: 5,
		}, nil
	}
	if err := ordSvc.SyncOrder(ctx, 201); err != nil {
		t.Fatalf("SyncOrder succeeded failed: %v", err)
	}
	if successCount != 1 {
		t.Fatalf("expected OnOrderSucceeded called once, got %d", successCount)
	}

	// Replay stale running snapshot -> terminal state must NOT regress and hooks must not fire again.
	gw.getOrderFn = func(ctx context.Context, id string) (*quefacontract.OrderSnapshot, error) {
		return &quefacontract.OrderSnapshot{
			OrderID:         id,
			Status:          "running",
			AttemptNo:       1,
			ProgressVersion: 2,
		}, nil
	}
	if err := ordSvc.SyncOrder(ctx, 201); err != nil {
		t.Fatalf("SyncOrder replay failed: %v", err)
	}
	st, _ := ordSvc.GetBuyerStatusByParentOrder(ctx, 200)
	if st == nil || st.Stage != string(quefadomain.StageSucceeded) || st.CanOpenFulfillment {
		t.Fatalf("expected terminal succeeded state preserved, got %+v", st)
	}
}

func TestProcurementRefundedDoesNotTriggerBuyerRefund(t *testing.T) {
	gw := &stubGateway{}
	store, _, _, ordSvc := setupAppTest(t, gw)
	ctx := context.Background()
	now := time.Now().UTC()

	refundedHookCalled := 0
	ordSvc.SetHooks(LifecycleHooks{
		OnPlatformOrderRefunded: func(ctx context.Context, record *quefadomain.Order, refundRef string) error {
			refundedHookCalled++
			return nil
		},
	})

	_ = store.CreateCheckout(quefacontract.CheckoutInput{
		OrderID:            301,
		ParentOrderID:      300,
		ProductID:          1,
		SKUID:              2,
		ProductCode:        "chatgpt_plus_1m",
		CollectionMode:     quefadomain.CollectionModeAgentCollect,
		DeliveryMode:       quefadomain.DeliveryModeAutoRecharge,
		CostFloorAmount:    money.FromDecimal(decimal.NewFromInt(110)),
		SettlementCurrency: "CNY",
		MerchantOrderNo:    "QF-300",
		CreatedAt:          now,
	})

	dup, childID, err := ordSvc.RecordWebhookEvent(ctx, quefacontract.WebhookEnvelope{
		EventID:         "evt_proc_ref_1",
		EventType:       "procurement.refunded",
		MerchantOrderNo: "QF-300",
		RefundRef:       "proc_ref_999",
	})
	if err != nil || dup || childID != 301 {
		t.Fatalf("RecordWebhookEvent failed: dup=%v child=%d err=%v", dup, childID, err)
	}
	// Duplicate webhook must be ignored.
	dup2, _, err := ordSvc.RecordWebhookEvent(ctx, quefacontract.WebhookEnvelope{
		EventID:         "evt_proc_ref_1",
		EventType:       "procurement.refunded",
		MerchantOrderNo: "QF-300",
		RefundRef:       "proc_ref_999",
	})
	if err != nil || !dup2 {
		t.Fatalf("expected duplicate webhook ignored, got dup=%v err=%v", dup2, err)
	}

	adminView, _ := ordSvc.GetAdminStatusByParentOrder(ctx, 300)
	if adminView.ProcurementRefundRef != "proc_ref_999" || adminView.Stage == string(quefadomain.StageRefunded) || refundedHookCalled != 0 {
		t.Fatalf("procurement.refunded must only record procurement ref, not refund buyer: view=%+v hook=%d", adminView, refundedHookCalled)
	}
}

func TestSubmitRedemptionLifecycleAndRetry(t *testing.T) {
	var seenIdem []string
	var callCount int
	gw := &stubGateway{
		createRedemptionFn: func(ctx context.Context, req quefacontract.CreateRedemptionRequest, idem string) (*quefacontract.RedemptionSnapshot, error) {
			callCount++
			seenIdem = append(seenIdem, idem)
			if callCount == 1 {
				return nil, &quefacontract.APIError{StatusCode: 422, Code: "session_invalid", Message: "登录凭据已失效，请重新获取"}
			}
			return &quefacontract.RedemptionSnapshot{
				RedemptionID:       "ful_attempt_2",
				OrderID:            req.OrderID,
				AttemptNo:          2,
				Status:             "queued",
				ProgressStage:      "queued",
				ProgressMessage:    "已受理，等待处理",
				AccountEmailMasked: "b***@example.com",
			}, nil
		},
	}
	store, _, _, ordSvc := setupAppTest(t, gw)
	ctx := context.Background()
	now := time.Now().UTC()

	_ = store.CreateCheckout(quefacontract.CheckoutInput{
		OrderID:            401,
		ParentOrderID:      400,
		ProductID:          1,
		SKUID:              2,
		ProductCode:        "chatgpt_plus_cdk_1m",
		CollectionMode:     quefadomain.CollectionModePlatformCollect,
		DeliveryMode:       quefadomain.DeliveryModeAutoRecharge,
		CostFloorAmount:    money.FromDecimal(decimal.NewFromInt(110)),
		SettlementCurrency: "CNY",
		MerchantOrderNo:    "QF-400",
		CreatedAt:          now,
	})
	upID := "QF000000400"
	_ = store.WithinTransaction(ctx, func(tx quefacontract.Transaction) error {
		rec, err := tx.GetOrderByOrderIDForUpdate(401)
		if err != nil {
			return err
		}
		rec.UpstreamOrderID = &upID
		rec.Stage = quefadomain.StageWaitingSubmit
		return tx.SaveOrder(rec)
	})

	// 1. Missing confirmation is rejected locally.
	if _, err := ordSvc.SubmitRedemption(ctx, 400, quefacontract.SubmitRedemptionInput{
		CredentialMode:         "session",
		Session:                `{"accessToken":"tok"}`,
		CustomerConfirmedEmail: false,
	}); err == nil {
		t.Fatalf("expected error when customer_confirmed_email is false")
	}

	// 2. Upstream 422 rolls back to waiting_submit so buyer can fix & resubmit with a fresh Idempotency-Key.
	_, err := ordSvc.SubmitRedemption(ctx, 400, quefacontract.SubmitRedemptionInput{
		CredentialMode:         "session",
		Session:                `{"accessToken":"expired"}`,
		CustomerConfirmedEmail: true,
	})
	if err == nil {
		t.Fatalf("expected 422 rejection error")
	}
	st, _ := ordSvc.GetBuyerStatusByParentOrder(ctx, 400)
	if st == nil || st.Stage != string(quefadomain.StageWaitingSubmit) || !st.CanSubmitRedemption {
		t.Fatalf("expected rollback to waiting_submit after 422, got %+v", st)
	}

	// 3. Valid retry transitions to recharging with fresh Idempotency-Key and stores masked email.
	st2, err := ordSvc.SubmitRedemption(ctx, 400, quefacontract.SubmitRedemptionInput{
		CredentialMode:         "session",
		Session:                `{"accessToken":"valid"}`,
		CustomerConfirmedEmail: true,
	})
	if err != nil {
		t.Fatalf("expected retry submit success, got %v", err)
	}
	if st2.Stage != string(quefadomain.StageRecharging) || st2.CanSubmitRedemption || st2.AccountEmailMasked != "b***@example.com" {
		t.Fatalf("unexpected buyer status after submit: %+v", st2)
	}
	if len(seenIdem) != 2 || seenIdem[0] == seenIdem[1] {
		t.Fatalf("expected distinct idempotency keys across attempts, got %v", seenIdem)
	}
}

func TestConfigCatalogRefreshIntervalPersistenceAndValidation(t *testing.T) {
	gw := &stubGateway{productsRaw: json.RawMessage(`{"products":[{"product_code":"plus_1m","supply_price":"100.00","currency":"CNY","available":true}]}`)}
	store, cfgSvc, _, _ := setupAppTest(t, gw)
	ctx := context.Background()

	// Invalid refresh intervals must be rejected.
	for _, bad := range []int{-2, 1, 4, 86401} {
		v := bad
		if _, err := cfgSvc.Update(ctx, ConfigPatch{CatalogRefreshIntervalSeconds: &v}); !errors.Is(err, ErrConfigInvalid) {
			t.Fatalf("expected ErrConfigInvalid for %d, got %v", bad, err)
		}
	}

	// Fixed interval saves and reloads cleanly.
	fixed := 300
	view, err := cfgSvc.Update(ctx, ConfigPatch{CatalogRefreshIntervalSeconds: &fixed})
	if err != nil {
		t.Fatalf("Update fixed interval failed: %v", err)
	}
	if view.CatalogRefreshIntervalSeconds != 300 {
		t.Fatalf("expected 300 in AdminView, got %d", view.CatalogRefreshIntervalSeconds)
	}
	snap := cfgSvc.Snapshot()
	if snap.Config.CatalogRefreshInterval() != 5*time.Minute || !snap.Config.CatalogFixedRefresh() {
		t.Fatalf("unexpected fixed schedule: interval=%v fixed=%v", snap.Config.CatalogRefreshInterval(), snap.Config.CatalogFixedRefresh())
	}

	// Verify DB reload preserves the persisted value.
	reloaded := NewConfigService(ctx, ConfigServiceOptions{
		Store:        store,
		Profile:      quefadomain.DefaultProfile,
		AppSecretKey: "test-app-secret-key-32-bytes-ok!",
	})
	if reloaded.Snapshot().Config.CatalogRefreshIntervalSeconds != 300 {
		t.Fatalf("expected reloaded value 300, got %d", reloaded.Snapshot().Config.CatalogRefreshIntervalSeconds)
	}

	// -1 disables auto-refresh cleanly.
	disabled := -1
	view, err = cfgSvc.Update(ctx, ConfigPatch{CatalogRefreshIntervalSeconds: &disabled})
	if err != nil || view.CatalogRefreshIntervalSeconds != -1 {
		t.Fatalf("Update disabled interval failed: view=%+v err=%v", view, err)
	}
	if cfgSvc.Snapshot().Config.CatalogRefreshInterval() != 0 || cfgSvc.Snapshot().Config.CatalogFixedRefresh() {
		t.Fatalf("expected disabled refresh schedule")
	}
}

func TestCatalogRefreshBeforeExpiryAndRateLimitBackoff(t *testing.T) {
	gw := &stubGateway{
		productsRaw: json.RawMessage(`{"products":[{"product_code":"chatgpt_plus_1m","title":"Plus 1M","supply_price":"105.00","min_sale_price":"110.00","currency":"CNY","collection_modes":["platform_collect"],"delivery_modes":["auto_recharge"],"available":true}]}`),
	}
	_, _, catSvc, _ := setupAppTest(t, gw)
	ctx := context.Background()

	now := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	catSvc.now = func() time.Time { return now }

	// 1. 429 Retry-After sets backoff and skips upstream calls during backoff window.
	rateErr := &quefacontract.APIError{
		StatusCode: http.StatusTooManyRequests,
		Category:   "rate_limited",
		Hints:      quefacontract.PollHints{RetryAfter: 5 * time.Minute},
	}
	gw.productsErr = rateErr
	if synced, err := catSvc.RefreshBeforeExpiry(ctx, time.Minute); synced != nil || !errors.Is(err, rateErr) {
		t.Fatalf("expected 429 error, got synced=%v err=%v", synced, err)
	}
	if gw.productsCalls != 1 {
		t.Fatalf("expected 1 upstream call, got %d", gw.productsCalls)
	}

	// Still inside 5m Retry-After window -> must skip without calling upstream.
	now = now.Add(2 * time.Minute)
	gw.productsErr = nil
	if synced, err := catSvc.RefreshBeforeExpiry(ctx, time.Minute); synced != nil || err != nil || gw.productsCalls != 1 {
		t.Fatalf("expected backoff skip, synced=%v err=%v calls=%d", synced, err, gw.productsCalls)
	}

	// 2. Once backoff expires, empty cache triggers sync.
	now = now.Add(4 * time.Minute)
	synced, err := catSvc.RefreshBeforeExpiry(ctx, time.Minute)
	if err != nil || synced == nil || len(synced.Products) != 1 || gw.productsCalls != 2 {
		t.Fatalf("expected sync after backoff, synced=%v err=%v calls=%d", synced, err, gw.productsCalls)
	}

	// 3. Fresh snapshot skips refresh.
	now = now.Add(10 * time.Minute)
	if skipped, err := catSvc.RefreshBeforeExpiry(ctx, time.Minute); skipped != nil || err != nil || gw.productsCalls != 2 {
		t.Fatalf("expected fresh snapshot skip, skipped=%v err=%v calls=%d", skipped, err, gw.productsCalls)
	}

	// 4. Near TTL expiry (3600s TTL, 2m lead -> 59m elapsed) triggers refresh.
	now = now.Add(49 * time.Minute)
	refreshed, err := catSvc.RefreshBeforeExpiry(ctx, time.Minute)
	if err != nil || refreshed == nil || gw.productsCalls != 3 {
		t.Fatalf("expected near-expiry refresh, refreshed=%v err=%v calls=%d", refreshed, err, gw.productsCalls)
	}
}

func TestCatalogRefreshOnIntervalAndResetSchedule(t *testing.T) {
	gw := &stubGateway{
		productsRaw: json.RawMessage(`{"products":[{"product_code":"chatgpt_plus_1m","title":"Plus 1M","supply_price":"105.00","min_sale_price":"110.00","currency":"CNY","collection_modes":["platform_collect"],"delivery_modes":["auto_recharge"],"available":true}]}`),
	}
	_, _, catSvc, _ := setupAppTest(t, gw)
	ctx := context.Background()

	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	catSvc.now = func() time.Time { return now }

	// Initial empty cache syncs immediately on fixed interval.
	synced, err := catSvc.RefreshOnInterval(ctx, 5*time.Minute)
	if err != nil || synced == nil || gw.productsCalls != 1 {
		t.Fatalf("expected initial fixed sync, synced=%v err=%v calls=%d", synced, err, gw.productsCalls)
	}

	// Within 5m interval -> skips.
	now = now.Add(2 * time.Minute)
	if skipped, err := catSvc.RefreshOnInterval(ctx, 5*time.Minute); skipped != nil || err != nil || gw.productsCalls != 1 {
		t.Fatalf("expected skip within fixed interval, skipped=%v err=%v calls=%d", skipped, err, gw.productsCalls)
	}

	// After 5m interval elapses -> syncs again.
	now = now.Add(3 * time.Minute)
	refreshed, err := catSvc.RefreshOnInterval(ctx, 5*time.Minute)
	if err != nil || refreshed == nil || gw.productsCalls != 2 {
		t.Fatalf("expected refresh after fixed interval, refreshed=%v err=%v calls=%d", refreshed, err, gw.productsCalls)
	}

	// Shortening interval + ResetRefreshSchedule allows immediate refresh once new interval elapsed.
	now = now.Add(1 * time.Minute)
	catSvc.ResetRefreshSchedule()
	refreshed2, err := catSvc.RefreshOnInterval(ctx, 30*time.Second)
	if err != nil || refreshed2 == nil || gw.productsCalls != 3 {
		t.Fatalf("expected refresh after schedule reset, refreshed=%v err=%v calls=%d", refreshed2, err, gw.productsCalls)
	}
}
