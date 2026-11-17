package application_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/dujiao-next/internal/app/container"
	orderwiring "github.com/dujiao-next/internal/bootstrap/order"
	"github.com/dujiao-next/internal/i18n"
	"github.com/dujiao-next/internal/platform/http/response"
	"github.com/gin-gonic/gin"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dujiao-next/internal/constants"
	productstore "github.com/dujiao-next/internal/modules/catalog/product/store/gormstore"
	coupondomain "github.com/dujiao-next/internal/modules/coupon/domain"
	userstore "github.com/dujiao-next/internal/modules/identity/user/infrastructure/gormstore"
	kafudaapp "github.com/dujiao-next/internal/modules/kafudagpt/application"
	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
	kafudastore "github.com/dujiao-next/internal/modules/kafudagpt/infrastructure/gormstore"
	orderapp "github.com/dujiao-next/internal/modules/order/application"
	orderdomain "github.com/dujiao-next/internal/modules/order/domain"
	orderstore "github.com/dujiao-next/internal/modules/order/infrastructure/gormstore"
	promotionstore "github.com/dujiao-next/internal/modules/promotion/infrastructure/gormstore"
	resellercontract "github.com/dujiao-next/internal/modules/reseller/contract"
	"github.com/dujiao-next/internal/shared/jsonmap"
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
				fields["sku_code"] = "client-code"
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
					if result.StatusCode != 0 || !strings.Contains(recorder.Body.String(), constants.FulfillmentTypeKafudaGPT) {
						t.Fatalf("main adapter did not enable preview: %s", recorder.Body.String())
					}
				} else if result.StatusCode != response.CodeBadRequest || result.Msg != i18n.T(locale, "error.product_purchase_not_allowed") {
					t.Fatalf("forged preview flag bypassed endpoint restriction: %s", recorder.Body.String())
				}
				for _, secret := range []string{"merchant-profile", "merchant-card", "merchant_cost", "must-not-leak", "cost_price", "max_cost", "45.00"} {
					if strings.Contains(recorder.Body.String(), secret) {
						t.Fatalf("HTTP preview leaked %s", secret)
					}
				}
			})
		}
	}
}
