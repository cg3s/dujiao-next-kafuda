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
	kafudawiring "github.com/dujiao-next/internal/bootstrap/kafudagpt"
	orderwiring "github.com/dujiao-next/internal/bootstrap/order"
	"github.com/dujiao-next/internal/constants"
	productdomain "github.com/dujiao-next/internal/modules/catalog/product/domain"
	productstore "github.com/dujiao-next/internal/modules/catalog/product/store/gormstore"
	coupondomain "github.com/dujiao-next/internal/modules/coupon/domain"
	fulfillmentapp "github.com/dujiao-next/internal/modules/fulfillment/application"
	fulfillmentdomain "github.com/dujiao-next/internal/modules/fulfillment/domain"
	userstore "github.com/dujiao-next/internal/modules/identity/user/infrastructure/gormstore"
	kafudaapp "github.com/dujiao-next/internal/modules/kafudagpt/application"
	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
	kafudastore "github.com/dujiao-next/internal/modules/kafudagpt/infrastructure/gormstore"
	memberapp "github.com/dujiao-next/internal/modules/memberlevel/application"
	memberdomain "github.com/dujiao-next/internal/modules/memberlevel/domain"
	memberstore "github.com/dujiao-next/internal/modules/memberlevel/infrastructure/gormstore"
	orderapp "github.com/dujiao-next/internal/modules/order/application"
	orderrefund "github.com/dujiao-next/internal/modules/order/application/refund"
	orderdomain "github.com/dujiao-next/internal/modules/order/domain"
	orderstore "github.com/dujiao-next/internal/modules/order/infrastructure/gormstore"
	orderhttp "github.com/dujiao-next/internal/modules/order/transport/http"
	orderpresenter "github.com/dujiao-next/internal/modules/order/transport/presenter"
	paymentapp "github.com/dujiao-next/internal/modules/payment/application"
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

func TestDirectCheckoutGenericAfterSalesCannotBypassReview(t *testing.T) {
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
	router := gin.New()
	userID := uint(0)
	router.Use(func(c *gin.Context) {
		if userID != 0 {
			c.Set("user_id", userID)
		}
	})
	orderhttp.RegisterUserReadRoutes(router, handler)
	request := func(orderNo string, uid uint, tenant resellercontract.TenantContext, wantCode int, wantStage string) {
		t.Helper()
		userID = uid
		ctx := resellercontract.WithTenantContext(context.Background(), tenant)
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/orders/"+orderNo+"/direct-status", nil).WithContext(ctx))
		var responseBody struct {
			Code int                        `json:"status_code"`
			Data map[string]json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &responseBody); err != nil || responseBody.Code != wantCode {
			t.Fatalf("status response: %s, %v; expected %d", recorder.Body.String(), err, wantCode)
		}
		if wantStage != "" {
			if len(responseBody.Data) != 2 || string(responseBody.Data["stage"]) != fmt.Sprintf("%q", wantStage) || len(responseBody.Data["updated_at"]) == 0 {
				t.Fatalf("unexpected buyer projection: %s", recorder.Body.String())
			}
		}
	}
	main := resellercontract.MainTenantContext("shop.example")
	request(order.OrderNo, order.UserID, main, response.CodeNotFound, "")
	now := time.Now().UTC()
	if err := f.db.Model(&orderdomain.Order{}).Where("id IN ?", []uint{order.ID, childID}).Updates(map[string]any{"paid_at": now, "status": constants.OrderStatusPaid}).Error; err != nil {
		t.Fatal(err)
	}
	request(order.OrderNo, 0, main, response.CodeUnauthorized, "")
	request(order.OrderNo, order.UserID+100, main, response.CodeNotFound, "")
	request(order.OrderNo, order.UserID, resellercontract.ResellerTenantContext("reseller.example", 1, 1, ""), response.CodeNotFound, "")
	request(order.Children[0].OrderNo, order.UserID, main, response.CodeNotFound, "")
	for _, stage := range []kafudadomain.Stage{kafudadomain.StageWaiting, kafudadomain.StageProcessing, kafudadomain.StageManualReview, kafudadomain.StageCompleted} {
		if stage == kafudadomain.StageCompleted {
			if err := f.db.Model(&orderdomain.Order{}).Where("id IN ?", []uint{order.ID, childID}).Update("status", constants.OrderStatusCompleted).Error; err != nil {
				t.Fatal(err)
			}
		}
		if err := f.db.Model(&kafudadomain.Recharge{}).Where("order_id = ?", childID).
			Updates(map[string]any{"stage": stage, "reason_code": "private-reason", "version": 5}).Error; err != nil {
			t.Fatal(err)
		}
		request(order.OrderNo, order.UserID, main, 0, string(stage))
		record, err := f.store.GetRecharge(context.Background(), childID)
		if err != nil || record == nil || record.Version != 5 {
			t.Fatalf("status read changed the recharge: %+v, %v", record, err)
		}
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
	if err := f.db.Exec("DROP TRIGGER reject_parent_completion").Error; err != nil {
		t.Fatal(err)
	}
	for _, want := range []bool{true, false} {
		if completed, err := apply(); completed != want || err != nil {
			t.Fatalf("fresh completion %t, %v; want %t", completed, err, want)
		}
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
	f := newCouponLimitRaceFixture(t, "direct-checkout", 0, 0)
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
	policy := kafudaapp.NewCommerceService(kafudaapp.CommerceOptions{Catalog: kafudaapp.NewCatalogService(store, nil, "checkout-profile", time.Hour), SalesEnabled: true, ReferenceLookupVerified: true, ProductCode: "checkout-card", MaxCostCents: 2500, AllowedKinds: []string{"new", "renew"}, WorkerReady: func(context.Context) bool { return true }})
	options := orderapp.OrderServiceOptions{DirectCatalog: policy, OrderStore: orderstore.New(f.db, "test-guest-credential-secret-with-32-bytes"), UserStore: userstore.New(f.db), ProductStore: productstore.NewProductStore(f.db), ProductSKUStore: productstore.NewSKUStore(f.db), CouponStore: f.couponRepo, CouponUsageStore: f.usageRepo, PromotionRepo: promotionstore.New(f.db), Queue: f.queue}
	input := f.input(f.users[0].ID)
	input.CouponCode = ""
	input.StorefrontRequest = true
	input.Tenant = resellercontract.MainTenantContext("shop.example")
	input.ManualFormData = map[string]jsonmap.JSON{fmt.Sprint(f.product.ID): {"session": "session-must-not-persist"}}
	return directCheckoutFixture{couponLimitRaceFixture: f, options: options, input: input, catalog: catalog, store: store}
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
