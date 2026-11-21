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
	"testing"
	"time"

	"github.com/dujiao-next/internal/app/container"
	catalogproductbootstrap "github.com/dujiao-next/internal/bootstrap/catalogproduct"
	orderwiring "github.com/dujiao-next/internal/bootstrap/order"
	"github.com/dujiao-next/internal/constants"
	"github.com/dujiao-next/internal/i18n"
	categorystore "github.com/dujiao-next/internal/modules/catalog/category/infrastructure/gormstore"
	productapp "github.com/dujiao-next/internal/modules/catalog/product/application"
	productwrite "github.com/dujiao-next/internal/modules/catalog/product/application/write"
	productdomain "github.com/dujiao-next/internal/modules/catalog/product/domain"
	productstore "github.com/dujiao-next/internal/modules/catalog/product/store/gormstore"
	producthttp "github.com/dujiao-next/internal/modules/catalog/product/transport/http"
	productpresenter "github.com/dujiao-next/internal/modules/catalog/product/transport/presenter"
	contentcontract "github.com/dujiao-next/internal/modules/content/contract"
	coupondomain "github.com/dujiao-next/internal/modules/coupon/domain"
	userstore "github.com/dujiao-next/internal/modules/identity/user/infrastructure/gormstore"
	kafudaapp "github.com/dujiao-next/internal/modules/kafudagpt/application"
	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
	kafudastore "github.com/dujiao-next/internal/modules/kafudagpt/infrastructure/gormstore"
	orderapp "github.com/dujiao-next/internal/modules/order/application"
	ordercontract "github.com/dujiao-next/internal/modules/order/contract"
	orderdomain "github.com/dujiao-next/internal/modules/order/domain"
	orderstore "github.com/dujiao-next/internal/modules/order/infrastructure/gormstore"
	paymentapp "github.com/dujiao-next/internal/modules/payment/application"
	paymentdomain "github.com/dujiao-next/internal/modules/payment/domain"
	paymentstore "github.com/dujiao-next/internal/modules/payment/infrastructure/gormstore"
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
)

type previewPolicySpy struct {
	kafudacontract.CommercePolicy
	mode     string
	calls    int
	revision uint64
}

func (p *previewPolicySpy) PurchaseTerms(ctx context.Context, plan, country string, revision uint64) (*kafudacontract.PurchaseTerms, error) {
	p.calls++
	p.revision = revision
	if p.mode == "policy_error" {
		return nil, errors.New("merchant-sensitive-error")
	}
	if p.mode == "nil_terms" {
		return nil, nil
	}
	terms, err := p.CommercePolicy.PurchaseTerms(ctx, plan, country, revision)
	if err == nil && p.mode == "wrong_terms" {
		terms.Country = "US"
	}
	return terms, err
}

func TestDirectOrderPreviewEnforcesStorefrontAndCatalogRules(t *testing.T) {
	cases := []string{"valid", "zero_cost", "guest", "channel", "unknown_tenant", "reseller", "unavailable_tenant", "mixed", "duplicate", "quantity", "missing_sku", "wrong_sku_owner", "sku_disabled", "product_disabled", "unconfirmed_cost", "negative_cost", "zero_price", "mapped", "bad_code", "bad_spec", "expired", "unlisted", "old_revision", "sales_disabled", "worker_down", "lookup_unverified", "nil_policy", "policy_error", "nil_terms", "wrong_terms", "create_unknown_source"}
	for _, scenario := range cases {
		t.Run(scenario, func(t *testing.T) {
			f := newCouponLimitRaceFixture(t, "direct-preview-"+scenario, 0, 0)
			if err := f.db.AutoMigrate(&kafudadomain.Catalog{}); err != nil {
				t.Fatal(err)
			}
			ctx, now := context.Background(), time.Now().UTC()
			store := kafudastore.New(f.db)
			catalog := &kafudadomain.Catalog{Profile: "merchant-profile", Enabled: true, SyncedAt: now.Add(-3 * time.Hour), ExpiresAt: now.Add(time.Hour), Options: []kafudadomain.CatalogOption{{Plan: "plus", Country: "PH", Product: "gpt", Available: true}}, Cards: []kafudadomain.CardProduct{{ProductCode: "merchant-card"}}}
			if err := store.SaveCatalog(ctx, catalog); err != nil {
				t.Fatal(err)
			}
			removed := *catalog
			removed.Options = []kafudadomain.CatalogOption{}
			removed.SyncedAt = now.Add(-2 * time.Hour)
			if err := store.SaveCatalog(ctx, &removed); err != nil {
				t.Fatal(err)
			}
			catalog.SyncedAt = now.Add(-time.Hour)
			if scenario == "expired" {
				catalog.ExpiresAt = now.Add(-time.Minute)
			}
			if scenario == "unlisted" {
				catalog.Options[0].Available = false
			}
			if err := store.SaveCatalog(ctx, catalog); err != nil {
				t.Fatal(err)
			}
			productFields := map[string]interface{}{"fulfillment_type": constants.FulfillmentTypeKafudaGPT, "manual_stock_total": 0}
			if scenario == "mapped" {
				productFields["is_mapped"] = true
			}
			if scenario == "product_disabled" {
				productFields["is_active"] = false
			}
			if err := f.db.Model(&f.product).Updates(productFields).Error; err != nil {
				t.Fatal(err)
			}
			fields := map[string]interface{}{"sku_code": kafudacontract.SKUCode("plus", "PH"), "spec_values_json": jsonmap.JSON{"plan": "plus", "country": "PH", "merchant_cost": "must-not-leak"}, "direct_catalog_revision": 1, "direct_cost_configured": true, "cost_price_amount": "45", "manual_stock_total": 0}
			switch scenario {
			case "zero_cost":
				fields["cost_price_amount"] = "0"
			case "negative_cost":
				fields["cost_price_amount"] = "-1"
			case "zero_price":
				fields["price_amount"] = "0"
			case "unconfirmed_cost":
				fields["direct_cost_configured"] = false
			case "sku_disabled":
				fields["is_active"] = false
			case "wrong_sku_owner":
				fields["product_id"] = f.product.ID + 99
			case "bad_code":
				fields["sku_code"] = ""
			case "bad_spec":
				fields["spec_values_json"] = jsonmap.JSON{"plan": 42, "country": "PH"}
			case "old_revision":
				fields["direct_catalog_revision"] = 0
			}
			if err := f.db.Model(&f.sku).Updates(fields).Error; err != nil {
				t.Fatal(err)
			}
			options := kafudaapp.CommerceOptions{Catalog: kafudaapp.NewCatalogService(store, nil, "merchant-profile", time.Hour), SalesEnabled: scenario != "sales_disabled", ReferenceLookupVerified: scenario != "lookup_unverified", ProductCode: "merchant-card", MaxCostCents: 2500, AllowedKinds: []string{"new"}, WorkerReady: func(context.Context) bool { return scenario != "worker_down" }}
			spy := &previewPolicySpy{CommercePolicy: kafudaapp.NewCommerceService(options), mode: scenario}
			var policy kafudacontract.CommercePolicy = spy
			if scenario == "nil_policy" {
				policy = nil
			}
			svc := orderapp.NewOrderService(orderapp.OrderServiceOptions{DirectCatalog: policy, OrderStore: orderstore.New(f.db, "test-guest-credential-secret-with-32-bytes"), UserStore: userstore.New(f.db), ProductStore: productstore.NewProductStore(f.db), ProductSKUStore: productstore.NewSKUStore(f.db), CouponStore: f.couponRepo, CouponUsageStore: f.usageRepo, PromotionRepo: promotionstore.New(f.db), Queue: f.queue})
			input := f.input(f.users[0].ID)
			input.CouponCode = ""
			input.StorefrontRequest = true
			input.Tenant = resellercontract.MainTenantContext("shop.example")
			// Request fulfillment values must never replace the database type.
			input.Items[0].FulfillmentType = constants.FulfillmentTypeManual
			switch scenario {
			case "channel":
				input.StorefrontRequest = false
			case "unknown_tenant":
				input.Tenant = resellercontract.TenantContext{}
			case "reseller":
				input.Tenant = resellercontract.ResellerTenantContext("reseller.example", 7, 1, "")
			case "unavailable_tenant":
				input.Tenant.Unavailable = true
			case "mixed":
				input.Items = append(input.Items, orderapp.CreateOrderItem{ProductID: 99, SKUID: 99, Quantity: 1})
			case "duplicate":
				input.Items = append(input.Items, input.Items[0])
			case "quantity":
				input.Items[0].Quantity = 2
			case "missing_sku":
				input.Items[0].SKUID = 0
			}
			var preview *orderapp.OrderPreview
			var err error
			if scenario == "create_unknown_source" {
				input.StorefrontRequest = false
			}
			if scenario == "guest" {
				preview, err = svc.PreviewGuestOrder(orderapp.CreateGuestOrderInput{Email: "guest@example.test", OrderPassword: "password123", Tenant: input.Tenant, Items: input.Items})
			} else if scenario == "create_unknown_source" {
				_, err = svc.CreateOrder(input)
			} else {
				preview, err = svc.PreviewOrder(input)
			}
			if scenario == "valid" || scenario == "zero_cost" {
				if err != nil || preview == nil || len(preview.Items) != 1 {
					t.Fatalf("valid Direct preview failed: %+v, %v", preview, err)
				}
				if preview.Items[0].FulfillmentType != constants.FulfillmentTypeKafudaGPT || preview.TotalAmount.String() != "100.00" || spy.calls != 1 || spy.revision != 1 {
					t.Fatalf("incorrect preview or review version: %+v, calls=%d rev=%d", preview, spy.calls, spy.revision)
				}
				encoded, _ := json.Marshal(preview)
				for _, secret := range []string{"merchant-profile", "merchant-card", "merchant_cost", "must-not-leak", "cost_price", "max_cost", "45.00"} {
					if strings.Contains(string(encoded), secret) {
						t.Fatalf("preview leaked %s: %s", secret, encoded)
					}
				}
			} else if err == nil || preview != nil {
				t.Fatalf("invalid Direct preview accepted: %+v, %v", preview, err)
			}
			if scenario != "valid" && scenario != "zero_cost" {
				want := orderapp.ErrProductNotAvailable
				switch scenario {
				case "guest", "channel", "unknown_tenant", "reseller", "unavailable_tenant", "mixed", "duplicate", "quantity", "create_unknown_source":
					want = orderapp.ErrProductPurchaseNotAllowed
				case "missing_sku":
					want = orderapp.ErrProductSKURequired
				case "wrong_sku_owner", "sku_disabled", "bad_code", "bad_spec":
					want = orderapp.ErrProductSKUInvalid
				case "unconfirmed_cost", "negative_cost", "zero_price":
					want = orderapp.ErrProductPriceInvalid
				}
				if !errors.Is(err, want) {
					t.Fatalf("expected %v, got %v", want, err)
				}
			}
			if scenario == "valid" {
				assertDirectPreviewHTTP(t, svc, input)
			}
			if scenario == "policy_error" && strings.Contains(fmt.Sprint(err), "merchant-sensitive") {
				t.Fatal("provider error leaked")
			}
			if scenario == "old_revision" && (spy.calls != 1 || spy.revision != 0) {
				t.Fatal("SKU review version was not passed to purchase policy")
			}
			for _, model := range []interface{}{&orderdomain.Order{}, &orderdomain.OrderItem{}, &coupondomain.CouponUsage{}} {
				var count int64
				if err := f.db.Model(model).Count(&count).Error; err != nil {
					t.Fatal(err)
				}
				if count != 0 {
					t.Fatalf("read-only preview persisted %T", model)
				}
			}
			if f.queue.enqueued.Load() != 0 {
				t.Fatal("preview enqueued order work")
			}
		})
	}
}

func assertDirectPreviewHTTP(t *testing.T, svc *orderapp.OrderService, input orderapp.CreateOrderInput) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	handlers := orderwiring.New(&container.Container{OrderService: svc})
	for _, locale := range []string{"zh-CN", "zh-TW", "en-US"} {
		for _, mode := range []string{"main_preview", "reseller_preview", "unavailable_preview"} {
			t.Run(locale+"/"+mode, func(t *testing.T) {
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				body := fmt.Sprintf(`{"items":[{"product_id":%d,"sku_id":%d,"quantity":1,"fulfillment_type":"manual"}],"storefront_request":true}`, input.Items[0].ProductID, input.Items[0].SKUID)
				c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/orders/preview", strings.NewReader(body))
				c.Request.Header.Set("Content-Type", "application/json")
				c.Request.Header.Set("X-Lang", locale)
				c.Set("user_id", input.UserID)
				if mode == "reseller_preview" {
					c.Request = c.Request.WithContext(resellercontract.WithTenantContext(c.Request.Context(), resellercontract.ResellerTenantContext("reseller.example", 7, 1, "")))
				}
				if mode == "unavailable_preview" {
					c.Request = c.Request.WithContext(resellercontract.WithTenantContext(c.Request.Context(), resellercontract.UnavailableTenantContext("unknown.example", "unknown")))
				}
				handlers.Preview.PreviewOrder(c)
				var result response.Response
				if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				if mode == "main_preview" {
					if result.StatusCode != 0 || !strings.Contains(recorder.Body.String(), `"fulfillment_type":"gpt_direct"`) {
						t.Fatalf("main adapter did not enable preview: %s", recorder.Body.String())
					}
				} else if result.StatusCode != response.CodeBadRequest || result.Msg != i18n.T(locale, "error.product_purchase_not_allowed") {
					t.Fatalf("forged preview flag bypassed endpoint restriction: %s", recorder.Body.String())
				}
				for _, secret := range []string{"kafuda", "merchant-profile", "merchant-card", "merchant_cost", "must-not-leak", "cost_price", "max_cost", "45.00"} {
					if strings.Contains(recorder.Body.String(), secret) {
						t.Fatalf("HTTP preview leaked %s", secret)
					}
				}
			})
		}
	}
}

func TestDirectRegionalSKUStopPreservesPaidSnapshot(t *testing.T) {
	testDirectRegionalSKUStopPreservesPaidSnapshot(t, newDirectCheckoutFixture(t))
}

func testDirectRegionalSKUStopPreservesPaidSnapshot(t *testing.T, f directCheckoutFixture) {
	t.Helper()
	ctx := t.Context()
	const siblingCountry, replacementCard = "US", "replacement-card"
	f.catalog.Options = append(f.catalog.Options, kafudadomain.CatalogOption{Plan: "plus", Country: siblingCountry, Product: "gpt", Available: true})
	f.catalog.Cards = append(f.catalog.Cards, kafudadomain.CardProduct{ProductCode: replacementCard})
	f.catalog.SyncedAt = time.Now().UTC()
	if err := f.store.SaveCatalog(ctx, f.catalog); err != nil {
		t.Fatal(err)
	}
	sibling := productdomain.ProductSKU{
		ProductID: f.product.ID, SKUCode: kafudacontract.SKUCode("plus", siblingCountry),
		SpecValuesJSON: jsonmap.JSON{"plan": "plus", "country": siblingCountry},
		PriceAmount:    money.FromDecimal(decimal.NewFromInt(160)), CostPriceAmount: money.FromDecimal(decimal.NewFromInt(60)),
		DirectCostConfigured: true, IsActive: true,
	}
	if err := productstore.NewSKUStore(f.db).Create(&sibling); err != nil {
		t.Fatal(err)
	}
	orders := f.options.OrderStore
	assertDirectRegionalPublicSKUs(t, f, directCheckoutCommerce(f.store, true), f.sku.ID, sibling.ID)
	checkout, err := orderapp.NewOrderService(f.options).CreateOrder(f.input)
	if err != nil || checkout == nil || len(checkout.Children) != 1 {
		t.Fatalf("create original region order: %v", err)
	}
	if err := f.db.AutoMigrate(&kafudadomain.Attempt{}, &paymentdomain.Payment{}, &paymentdomain.PaymentChannel{}, &walletdomain.Account{}, &walletdomain.Transaction{}); err != nil {
		t.Fatal(err)
	}
	wallets := walletstore.New(f.db)
	if err := f.db.Create(&walletdomain.Account{UserID: checkout.UserID, Balance: checkout.TotalAmount}).Error; err != nil {
		t.Fatal(err)
	}
	payments := paymentapp.NewPaymentService(paymentapp.PaymentServiceOptions{
		OrderStore: orders, PaymentStore: paymentstore.New(f.db, "test-guest-credential-secret-with-32-bytes"), WalletRepo: wallets,
		WalletService: walletapp.NewService(walletapp.Options{Repository: wallets, Transactions: wallets}),
	})
	settled, err := payments.CreatePayment(paymentapp.CreatePaymentInput{OrderID: checkout.ID, UseBalance: true, Context: ctx})
	if err != nil || settled == nil || !settled.OrderPaid || !settled.WalletPaidAmount.Equal(checkout.TotalAmount.Decimal) || !settled.OnlinePayAmount.IsZero() {
		t.Fatalf("settle original region order: %v", err)
	}
	paid, err := orders.GetByID(checkout.ID)
	if err != nil || paid == nil || paid.PaidAt == nil {
		t.Fatalf("load paid original order: %v", err)
	}
	loadPayment := func() paymentdomain.Payment {
		t.Helper()
		var rows []paymentdomain.Payment
		if err := f.db.Where("order_id = ?", checkout.ID).Find(&rows).Error; err != nil || len(rows) != 1 ||
			rows[0].Status != constants.PaymentStatusSuccess || rows[0].ProviderType != constants.PaymentProviderWallet ||
			rows[0].Currency != paid.Currency || !rows[0].Amount.Equal(paid.TotalAmount.Decimal) {
			t.Fatalf("expected one successful wallet payment for original order: %v", err)
		}
		return rows[0]
	}
	originalPayment := loadPayment()
	assertDirectShopOrderStatus(t, orders, checkout, constants.OrderStatusPaid, *paid.PaidAt)
	childID := checkout.Children[0].ID
	initial, err := f.store.GetRecharge(ctx, childID)
	if err != nil || initial == nil || initial.Stage != kafudadomain.StageWaiting || initial.Snapshot.Country != "PH" ||
		initial.Snapshot.ProductCode != "checkout-card" || initial.Snapshot.SellPrice.String() != "100.00" || initial.Snapshot.Currency != checkout.Currency {
		t.Fatalf("unexpected original snapshot: %+v, %v", initial, err)
	}
	updatedCommerce := kafudaapp.NewCommerceService(kafudaapp.CommerceOptions{
		Catalog: kafudaapp.NewCatalogService(f.store, nil, f.catalog.Profile, time.Hour), SalesEnabled: true,
		ReferenceLookupVerified: true, ProductCode: replacementCard, MaxCostCents: 2500, AllowedKinds: []string{"new"},
		WorkerReady: func(context.Context) bool { return true },
	})
	writer := productwrite.NewWriteService(productwrite.Options{
		Products: productstore.NewProductStore(f.db), SKUs: productstore.NewSKUStore(f.db),
		Categories: categorystore.NewCategoryStore(f.db), Orders: orders,
	})
	writer.SetDirectCatalog(updatedCommerce)
	inactive := false
	edited, err := writer.Update(fmt.Sprint(f.product.ID), productwrite.CreateProductInput{
		CategoryID: f.product.CategoryID, Slug: f.product.Slug, TitleJSON: f.product.TitleJSON,
		FulfillmentType: constants.FulfillmentTypeKafudaGPT,
		SKUs: []productwrite.ProductSKUInput{{
			ID: f.sku.ID, SKUCode: kafudacontract.SKUCode("plus", "PH"), IsActive: &inactive,
			PriceAmount: decimal.NewFromInt(130), CostPriceAmount: decimal.NewFromInt(55), CostPriceProvided: true,
		}},
	})
	if err != nil || edited == nil || !edited.IsActive || len(edited.SKUs) != 1 || edited.SKUs[0].ID != sibling.ID {
		t.Fatalf("retire original SKU via product edit: %v", err)
	}
	retired, err := f.options.ProductSKUStore.GetByID(f.sku.ID)
	if err != nil || retired == nil || retired.IsActive || retired.PriceAmount.String() != "130.00" || retired.CostPriceAmount.String() != "55.00" {
		t.Fatalf("retired SKU configuration was not retained: %+v, %v", retired, err)
	}
	assertDirectRegionalPublicSKUs(t, f, updatedCommerce, sibling.ID)
	f.options.DirectCatalog = updatedCommerce
	checkoutService := orderapp.NewOrderService(f.options)
	if preview, err := checkoutService.PreviewOrder(f.input); preview != nil || !errors.Is(err, orderapp.ErrProductSKUInvalid) {
		t.Fatalf("retired region remains previewable: %v", err)
	}
	if order, err := checkoutService.CreateOrder(f.input); order != nil || !errors.Is(err, orderapp.ErrProductSKUInvalid) {
		t.Fatalf("retired region remains purchasable from a stale SKU ID: %v", err)
	}
	var count int64
	if err := f.db.Model(&orderdomain.Order{}).Count(&count).Error; err != nil || count != 2 {
		t.Fatalf("rejected order changed persisted checkout count: %d, %v", count, err)
	}
	otherInput := f.input
	otherInput.Items = []orderapp.CreateOrderItem{{ProductID: f.product.ID, SKUID: sibling.ID, Quantity: 1}}
	if preview, err := checkoutService.PreviewOrder(otherInput); err != nil || preview == nil || !preview.TotalAmount.Equal(sibling.PriceAmount.Decimal) {
		t.Fatalf("sibling region preview lost its own price: %v", err)
	}
	otherOrder, err := checkoutService.CreateOrder(otherInput)
	if err != nil || otherOrder == nil || len(otherOrder.Children) != 1 {
		t.Fatalf("sibling region checkout: %v", err)
	}
	other, err := f.store.GetRecharge(ctx, otherOrder.Children[0].ID)
	if err != nil || other == nil || other.Snapshot.SKUID != sibling.ID || other.Snapshot.Country != siblingCountry ||
		other.Snapshot.ProductCode != replacementCard || !other.Snapshot.SellPrice.Equal(sibling.PriceAmount.Decimal) {
		t.Fatalf("unexpected sibling snapshot: %+v, %v", other, err)
	}

	now := time.Now().UTC()
	reservation, err := kafudaworkflow.ReserveBuyerAttempt(orders, ordercontract.TenantScope{}, checkout.OrderNo, checkout.UserID, now, now.Add(time.Minute))
	if err != nil || reservation == nil || !reservation.Created || !reflect.DeepEqual(reservation.Recharge.Snapshot, initial.Snapshot) {
		t.Fatalf("old order could not reserve the original snapshot: %v", err)
	}
	snapshot := reservation.Recharge.Snapshot
	quote := kafudacontract.Quote{Plan: snapshot.Plan, Status: "available", Kind: "new", QuoteID: "original-region-quote",
		PriceCents: 900, ExpiresAt: now.Add(time.Minute), AutoCard: &kafudacontract.AutoCard{ProductCode: snapshot.ProductCode, LoadCents: 100, TotalCents: 1000}}
	check := kafudacontract.CheckResult{Country: siblingCountry, PayMode: snapshot.PayMode, Plans: []kafudacontract.Quote{quote}}
	if selected, err := kafudaapp.SelectQuote(check, snapshot, now); selected != nil || !errors.Is(err, kafudaapp.ErrQuotePolicy) {
		t.Fatalf("old order accepted a quote for the sibling region: %v", err)
	}
	check.Country = snapshot.Country
	check.Plans[0].AutoCard = &kafudacontract.AutoCard{ProductCode: replacementCard, LoadCents: 100, TotalCents: 1000}
	if selected, err := kafudaapp.SelectQuote(check, snapshot, now); selected != nil || !errors.Is(err, kafudaapp.ErrQuotePolicy) {
		t.Fatalf("old order accepted a quote using the replacement card: %v", err)
	}
	check.Plans[0] = quote
	calls := 0
	gateway := directCreateGateway{create: func(_ context.Context, req kafudacontract.CreateOrderRequest) (*kafudacontract.OrderResult, error) {
		calls++
		if req.QuoteID != quote.QuoteID || req.Reference != reservation.Attempt.Reference {
			t.Fatalf("old order submitted with unexpected quote or reference: %+v", req)
		}
		return &kafudacontract.OrderResult{ID: 701, Reference: req.Reference, Product: snapshot.Product, Plan: snapshot.Plan, Country: snapshot.Country, Status: "completed"}, nil
	}}
	completed, err := kafudaworkflow.SubmitCheckedQuote(ctx, orders, kafudaapp.New(f.store), gateway, reservation, check, time.Minute)
	if err != nil || !completed || calls != 1 {
		t.Fatalf("retired region order could not fulfill from its original quote: calls=%d, %v", calls, err)
	}
	assertDirectShopOrderStatus(t, orders, checkout, constants.OrderStatusCompleted, *paid.PaidAt)
	final, err := f.store.GetRecharge(ctx, childID)
	if err != nil || final == nil || final.Stage != kafudadomain.StageCompleted || !reflect.DeepEqual(final.Snapshot, initial.Snapshot) {
		t.Fatalf("retired region fulfillment changed the original snapshot: %+v, %v", final, err)
	}
	if payment := loadPayment(); !reflect.DeepEqual(payment, originalPayment) {
		t.Fatal("retired region fulfillment changed payment facts")
	}
}

type directRegionalRelatedPosts struct{}

func (directRegionalRelatedPosts) ListPostsForProduct(context.Context, uint, int) ([]contentcontract.RelatedPost, error) {
	return nil, nil
}

func assertDirectRegionalPublicSKUs(t *testing.T, f directCheckoutFixture, commerce *kafudaapp.CommerceService, wantIDs ...uint) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	producthttp.RegisterPublicRoutes(router.Group("/api/v1/public"), catalogproductbootstrap.NewPublicHTTP(catalogproductbootstrap.PublicHTTPDependencies{
		Products:     productapp.NewService(productapp.Options{Products: productstore.NewProductStore(f.db), Categories: categorystore.NewCategoryStore(f.db)}),
		RelatedPosts: directRegionalRelatedPosts{}, Direct: commerce,
	}))
	request := func(path string, data any) {
		t.Helper()
		ctx := resellercontract.WithTenantContext(t.Context(), f.input.Tenant)
		result := httptest.NewRecorder()
		router.ServeHTTP(result, httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx))
		var envelope struct {
			Code int             `json:"status_code"`
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(result.Body.Bytes(), &envelope); err != nil || result.Code != http.StatusOK || envelope.Code != 0 {
			t.Fatalf("read public route %s: %s, %v", path, result.Body.String(), err)
		}
		if err := json.Unmarshal(envelope.Data, data); err != nil {
			t.Fatalf("decode public route %s: %v", path, err)
		}
	}
	var products []productpresenter.Product
	request("/api/v1/public/products", &products)
	if len(products) != 1 {
		t.Fatalf("public list contains %d products, want one Direct product", len(products))
	}
	var detail productpresenter.Product
	request("/api/v1/public/products/"+f.product.Slug, &detail)
	for _, product := range []productpresenter.Product{products[0], detail} {
		if product.ID != f.product.ID || product.IsSoldOut || len(product.SKUs) != len(wantIDs) {
			t.Fatalf("unexpected public Direct availability: %+v", product)
		}
		for i, sku := range product.SKUs {
			if sku.ID != wantIDs[i] || sku.IsSoldOut || !sku.IsActive {
				t.Fatalf("expected active public SKU %d, got %+v", wantIDs[i], sku)
			}
		}
	}
}
