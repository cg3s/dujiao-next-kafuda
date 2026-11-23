package integrationtest

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/dujiao-next/internal/constants"
	fulfillmentdomain "github.com/dujiao-next/internal/modules/fulfillment/domain"
	orderapp "github.com/dujiao-next/internal/modules/order/application"
	ordercontract "github.com/dujiao-next/internal/modules/order/contract"
	orderdomain "github.com/dujiao-next/internal/modules/order/domain"
	ordergormstore "github.com/dujiao-next/internal/modules/order/infrastructure/gormstore"
	quefaapp "github.com/dujiao-next/internal/modules/quefa/application"
	quefacontract "github.com/dujiao-next/internal/modules/quefa/contract"
	quefadomain "github.com/dujiao-next/internal/modules/quefa/domain"
	quefaclient "github.com/dujiao-next/internal/modules/quefa/infrastructure/client"
	quefagormstore "github.com/dujiao-next/internal/modules/quefa/infrastructure/gormstore"
	quefahttp "github.com/dujiao-next/internal/modules/quefa/transport/http"
	walletapp "github.com/dujiao-next/internal/modules/wallet/application"
	walletdomain "github.com/dujiao-next/internal/modules/wallet/domain"
	walletgormstore "github.com/dujiao-next/internal/modules/wallet/infrastructure/gormstore"
	"github.com/dujiao-next/internal/shared/jsonmap"
	"github.com/dujiao-next/internal/shared/money"
	quefaworkflow "github.com/dujiao-next/internal/workflows/quefa/application"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

func openIntegrationDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	if err := db.AutoMigrate(
		&orderdomain.Order{},
		&orderdomain.OrderItem{},
		&orderdomain.OrderRefundRecord{},
		&fulfillmentdomain.Fulfillment{},
		&walletdomain.Account{},
		&walletdomain.Transaction{},
		&quefadomain.Setting{},
		&quefadomain.Catalog{},
		&quefadomain.Order{},
		&quefadomain.WebhookEvent{},
	); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	return db
}

func TestWebhookHTTPVerificationDedupAndMixedPaymentRefundWorkflow(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := openIntegrationDB(t)
	now := time.Now().UTC().Truncate(time.Second)

	orderStore := ordergormstore.New(db, "guest-test-secret")
	walletStore := walletgormstore.New(db)
	walletSvc := walletapp.NewService(walletapp.Options{
		Repository:   walletStore,
		Transactions: walletStore,
	})
	qStore := quefagormstore.New(db)

	cfgSvc := quefaapp.NewConfigService(context.Background(), quefaapp.ConfigServiceOptions{
		Store:        qStore,
		Profile:      quefadomain.DefaultProfile,
		AppSecretKey: "test-secret-key-for-quefa-e2e!!!",
		Fallback: quefaapp.RuntimeConfig{
			APIBaseURL:            "https://tibo.ink",
			PartnerID:             "partner_e2e",
			KeyID:                 "key_e2e",
			ClientSecret:          "csec_e2e",
			WebhookSecret:         "whsec_e2e",
			CollectionMode:        quefadomain.CollectionModePlatformCollect,
			SalesEnabled:          true,
			AllowedPayDomains:     []string{"pay.mybrand.com"},
			WebhookMaxSkewSeconds: 300,
		},
	})
	ordSvc := quefaapp.NewOrderService(qStore, cfgSvc)

	// Seed paid parent + child order (mixed payment: 30 wallet + 110 online = 140 total, cost floor 110).
	paidAt := now
	parent := &orderdomain.Order{
		OrderNo:          "DJ-QF-E2E-1",
		UserID:           77,
		Status:           constants.OrderStatusPaid,
		Currency:         "CNY",
		OriginalAmount:   money.FromDecimal(decimal.NewFromInt(140)),
		TotalAmount:      money.FromDecimal(decimal.NewFromInt(140)),
		WalletPaidAmount: money.FromDecimal(decimal.NewFromInt(30)),
		OnlinePaidAmount: money.FromDecimal(decimal.NewFromInt(110)),
		PaidAt:           &paidAt,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if err := orderStore.Create(parent, nil); err != nil {
		t.Fatalf("create parent order: %v", err)
	}
	child := &orderdomain.Order{
		OrderNo:          "DJ-QF-E2E-1-01",
		ParentID:         &parent.ID,
		UserID:           77,
		Status:           constants.OrderStatusPaid,
		Currency:         "CNY",
		OriginalAmount:   money.FromDecimal(decimal.NewFromInt(140)),
		TotalAmount:      money.FromDecimal(decimal.NewFromInt(140)),
		OnlinePaidAmount: money.FromDecimal(decimal.NewFromInt(140)),
		PaidAt:           &paidAt,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if err := orderStore.Create(child, []orderdomain.OrderItem{{
		ProductID:       10,
		SKUID:           20,
		TitleJSON:       jsonmap.JSON{"en-US": "Hosted Plan"},
		Quantity:        1,
		FulfillmentType: constants.FulfillmentTypeQuefaHosted,
		UnitPrice:       money.FromDecimal(decimal.NewFromInt(140)),
		TotalPrice:      money.FromDecimal(decimal.NewFromInt(140)),
		CostPrice:       money.FromDecimal(decimal.NewFromInt(110)),
	}}); err != nil {
		t.Fatalf("create child order: %v", err)
	}
	if err := qStore.CreateCheckout(quefacontract.CheckoutInput{
		OrderID:            child.ID,
		ParentOrderID:      parent.ID,
		ProductID:          10,
		SKUID:              20,
		ProductCode:        "chatgpt_plus_1m",
		CollectionMode:     quefadomain.CollectionModePlatformCollect,
		DeliveryMode:       quefadomain.DeliveryModeAutoRecharge,
		CostFloorAmount:    money.FromDecimal(decimal.NewFromInt(110)),
		SettlementCurrency: "CNY",
		MerchantOrderNo:    child.OrderNo,
		CreatedAt:          now,
	}); err != nil {
		t.Fatalf("create quefa checkout: %v", err)
	}

	// 1. Test Webhook HTTP handler (reject bad signature, accept valid signature, dedup second delivery).
	r := gin.New()
	quefahttp.RegisterWebhookRoutes(r, &quefahttp.WebhookHandler{
		Config:   cfgSvc,
		Verifier: quefaclient.VerifyWebhookSignature,
		Ingestor: ordSvc,
		Now:      func() time.Time { return now },
	})

	payload := []byte(`{"event_id":"evt_e2e_1","event_type":"fulfillment.updated","merchant_order_no":"DJ-QF-E2E-1-01"}`)
	ts := strconv.FormatInt(now.Unix(), 10)
	mac := hmac.New(sha256.New, []byte("whsec_e2e"))
	mac.Write([]byte(ts + "."))
	mac.Write(payload)
	validSig := hex.EncodeToString(mac.Sum(nil))

	// Invalid signature -> 401
	wBad := httptest.NewRecorder()
	reqBad := httptest.NewRequest(http.MethodPost, "/webhooks/quefa", bytes.NewReader(payload))
	reqBad.Header.Set("X-Quefa-Timestamp", ts)
	reqBad.Header.Set("X-Quefa-Signature", "deadbeef")
	r.ServeHTTP(wBad, reqBad)
	if wBad.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for bad webhook sig, got %d", wBad.Code)
	}

	// Valid signature -> 200 duplicate=false
	w1 := httptest.NewRecorder()
	req1 := httptest.NewRequest(http.MethodPost, "/webhooks/quefa", bytes.NewReader(payload))
	req1.Header.Set("X-Quefa-Timestamp", ts)
	req1.Header.Set("X-Quefa-Signature", validSig)
	r.ServeHTTP(w1, req1)
	if w1.Code != http.StatusOK {
		t.Fatalf("expected 200 for valid webhook, got %d body=%s", w1.Code, w1.Body.String())
	}
	var resp1 map[string]any
	_ = json.Unmarshal(w1.Body.Bytes(), &resp1)
	if resp1["duplicate"] != false {
		t.Fatalf("expected duplicate=false on first webhook, got %v", resp1)
	}

	// Replay same event_id -> 200 duplicate=true
	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/webhooks/quefa", bytes.NewReader(payload))
	req2.Header.Set("X-Quefa-Timestamp", ts)
	req2.Header.Set("X-Quefa-Signature", validSig)
	r.ServeHTTP(w2, req2)
	var resp2 map[string]any
	_ = json.Unmarshal(w2.Body.Bytes(), &resp2)
	if resp2["duplicate"] != true {
		t.Fatalf("expected duplicate=true on replay, got %v", resp2)
	}

	// 2. Test mixed-payment platform_collect refund workflow (credits 30 back to wallet + records 110 external refund).
	qRec, _ := qStore.GetOrderByOrderID(context.Background(), child.ID)
	if err := quefaworkflow.ApplyPlatformCollectRefunded(orderStore, walletSvc, qRec, "qf_ref_123"); err != nil {
		t.Fatalf("ApplyPlatformCollectRefunded failed: %v", err)
	}
	// Replay must be idempotent.
	if err := quefaworkflow.ApplyPlatformCollectRefunded(orderStore, walletSvc, qRec, "qf_ref_123"); err != nil {
		t.Fatalf("ApplyPlatformCollectRefunded replay failed: %v", err)
	}

	reloadedParent, _ := orderStore.GetByID(parent.ID)
	if reloadedParent.Status != constants.OrderStatusRefunded || reloadedParent.RefundedAmount.Decimal.StringFixed(2) != "140.00" {
		t.Fatalf("expected refunded parent 140.00, got status=%s refunded=%s", reloadedParent.Status, reloadedParent.RefundedAmount.String())
	}
	acct, err := walletStore.GetAccountByUserID(77)
	if err != nil || acct == nil || acct.Balance.Decimal.StringFixed(2) != "30.00" {
		t.Fatalf("expected 30.00 wallet portion credited back once, got acct=%+v err=%v", acct, err)
	}
}

func TestAtomicCompletionProducesSingleDelivery(t *testing.T) {
	db := openIntegrationDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	orderStore := ordergormstore.New(db, "guest-test-secret")
	paidAt := now

	parent := &orderdomain.Order{
		OrderNo: "DJ-QF-DONE-1", UserID: 88, Status: constants.OrderStatusPaid,
		Currency: "CNY", TotalAmount: money.FromDecimal(decimal.NewFromInt(135)),
		PaidAt: &paidAt, CreatedAt: now, UpdatedAt: now,
	}
	_ = orderStore.Create(parent, nil)
	child := &orderdomain.Order{
		OrderNo: "DJ-QF-DONE-1-01", ParentID: &parent.ID, UserID: 88, Status: constants.OrderStatusPaid,
		Currency: "CNY", TotalAmount: money.FromDecimal(decimal.NewFromInt(135)),
		PaidAt: &paidAt, CreatedAt: now, UpdatedAt: now,
	}
	_ = orderStore.Create(child, []orderdomain.OrderItem{{
		ProductID: 1, SKUID: 2, TitleJSON: jsonmap.JSON{"en-US": "Hosted Plan"}, Quantity: 1, FulfillmentType: constants.FulfillmentTypeQuefaHosted,
		UnitPrice: money.FromDecimal(decimal.NewFromInt(135)), TotalPrice: money.FromDecimal(decimal.NewFromInt(135)),
	}})

	completed, err := quefaworkflow.CompleteOrder(orderStore, child.ID, now)
	if err != nil || !completed {
		t.Fatalf("CompleteOrder failed: completed=%v err=%v", completed, err)
	}
	// Second invocation must be idempotent and return completed=false.
	completedAgain, err := quefaworkflow.CompleteOrder(orderStore, child.ID, now.Add(time.Second))
	if err != nil || completedAgain {
		t.Fatalf("expected idempotent replay, got completed=%v err=%v", completedAgain, err)
	}

	var deliveryCount int64
	db.Model(&fulfillmentdomain.Fulfillment{}).Where("order_id = ?", child.ID).Count(&deliveryCount)
	if deliveryCount != 1 {
		t.Fatalf("expected exactly 1 fulfillment record, got %d", deliveryCount)
	}
	reloadedParent, _ := orderStore.GetByID(parent.ID)
	if reloadedParent.Status != constants.OrderStatusCompleted {
		t.Fatalf("expected parent completed, got %s", reloadedParent.Status)
	}
}

func TestWalletBalanceCapProtectsCostFloor(t *testing.T) {
	db := openIntegrationDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	orderStore := ordergormstore.New(db, "guest-test-secret")
	walletStore := walletgormstore.New(db)
	walletSvc := walletapp.NewService(walletapp.Options{
		Repository:   walletStore,
		Transactions: walletStore,
	})

	// Give user 99 a wallet balance of 200.00 (more than order total 140.00).
	if err := db.Create(&walletdomain.Account{
		UserID:    99,
		Balance:   money.FromDecimal(decimal.NewFromInt(200)),
		CreatedAt: now,
		UpdatedAt: now,
	}).Error; err != nil {
		t.Fatalf("seed wallet: %v", err)
	}

	// Order total = 140.00, cost floor = 110.00 -> max wallet deduct = 30.00, online pay must remain 110.00.
	order := &orderdomain.Order{
		OrderNo:          "DJ-QF-CAP-1",
		UserID:           99,
		Status:           constants.OrderStatusPendingPayment,
		Currency:         "CNY",
		TotalAmount:      money.FromDecimal(decimal.NewFromInt(140)),
		OnlinePaidAmount: money.FromDecimal(decimal.NewFromInt(140)),
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if err := orderStore.Create(order, nil); err != nil {
		t.Fatalf("create order: %v", err)
	}

	capAmt := money.FromDecimal(decimal.NewFromInt(30))
	err := orderStore.WithinTransaction(func(tx ordercontract.Transaction) error {
		deducted, err := orderapp.ApplyWalletBalanceWithCap(walletSvc, tx, order, true, &capAmt)
		if err != nil {
			return err
		}
		if deducted.StringFixed(2) != "30.00" {
			t.Fatalf("expected capped wallet deduction 30.00, got %s", deducted.StringFixed(2))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ApplyWalletBalanceWithCap failed: %v", err)
	}

	reloaded, _ := orderStore.GetByID(order.ID)
	if reloaded.WalletPaidAmount.Decimal.StringFixed(2) != "30.00" || reloaded.OnlinePaidAmount.Decimal.StringFixed(2) != "110.00" {
		t.Fatalf("expected wallet=30.00 online=110.00, got wallet=%s online=%s", reloaded.WalletPaidAmount.String(), reloaded.OnlinePaidAmount.String())
	}
	acct, _ := walletStore.GetAccountByUserID(99)
	if acct.Balance.Decimal.StringFixed(2) != "170.00" {
		t.Fatalf("expected remaining wallet balance 170.00, got %s", acct.Balance.String())
	}
}
