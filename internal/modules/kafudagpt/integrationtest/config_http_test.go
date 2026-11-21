package integrationtest

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	kafudawiring "github.com/dujiao-next/internal/bootstrap/kafudagpt"
	"github.com/dujiao-next/internal/config"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
	kafudahttp "github.com/dujiao-next/internal/modules/kafudagpt/transport/http"
	orderstore "github.com/dujiao-next/internal/modules/order/infrastructure/gormstore"
	"github.com/dujiao-next/internal/platform/http/response"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestAdminConfigHotReloadsGatewayCatalogAndSalesReadiness(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const supplierOrigin = "https://supplier.kafuda.test"
	const apiKey = "kf_live_integration_secret_987654"
	const cardCode = "XL537872"

	previousTransport := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme+"://"+r.URL.Host != supplierOrigin {
			return &http.Response{
				StatusCode: http.StatusBadGateway,
				Body:       io.NopCloser(strings.NewReader("")),
				Header:     make(http.Header),
			}, nil
		}
		if r.Header.Get("Authorization") != "Bearer "+apiKey {
			return &http.Response{
				StatusCode: http.StatusUnauthorized,
				Body:       io.NopCloser(strings.NewReader(`{"code":"unauthorized"}`)),
				Header:     make(http.Header),
			}, nil
		}
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/gpt/catalog" {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body: io.NopCloser(strings.NewReader(
					`{"data":{"enabled":true,"plans":[{"plan":"plus","label":"Plus","credit":false,"regions":[{"country":"PH","currency":"PHP","estimated_plan_usd_cents":1000,"estimated_local_minor":55000}]}],"auto_card_products":[{"product_code":"XL537872","name":"Visa XL","restriction_reason":null,"gpt_recommended":true,"open_fee_cents":100,"min_amount_cents":500}]}}`,
				)),
				Header: http.Header{"Content-Type": []string{"application/json"}},
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusNotFound,
			Body:       io.NopCloser(strings.NewReader("")),
			Header:     make(http.Header),
		}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = previousTransport })

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = pool.Close() })
	if err := db.AutoMigrate(&kafudadomain.Setting{}, &kafudadomain.Catalog{}, &kafudadomain.Recharge{}, &kafudadomain.Attempt{}); err != nil {
		t.Fatal(err)
	}
	// Create minimal orders table expected by syncLegacyManualOrders join.
	if err := db.Exec(`CREATE TABLE orders (id INTEGER PRIMARY KEY, parent_id INTEGER, status TEXT, deleted_at DATETIME)`).Error; err != nil {
		t.Fatal(err)
	}

	orders := orderstore.New(db, "integration-order-secret")
	module, err := kafudawiring.NewWithSettings(
		db,
		config.KafudaGPTConfig{
			APIBaseURL:             "https://kafuda.com",
			AllowedKinds:           []string{"new"},
			CatalogTTLSeconds:      3600,
			PollIntervalSeconds:    10,
			PollReviewAfterSeconds: 86400,
		},
		"integration-app-secret-key-0123456789",
		orders,
		func(context.Context) bool { return true },
	)
	if err != nil {
		t.Fatal(err)
	}

	router := gin.New()
	kafudahttp.RegisterAdminRoutes(router.Group("/admin"), kafudawiring.NewAdminHandler(module))

	call := func(method, path, body string) (int, map[string]any, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Accept-Language", "en-US")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		raw := rec.Body.String()
		if strings.Contains(raw, apiKey) {
			t.Fatalf("%s %s leaked raw API key in response body", method, path)
		}
		var envelope struct {
			StatusCode int            `json:"status_code"`
			Data       map[string]any `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
			t.Fatalf("%s %s decode failed: %v (%s)", method, path, err, raw)
		}
		return envelope.StatusCode, envelope.Data, raw
	}

	// 1. Initially unconfigured.
	code, data, _ := call(http.MethodGet, "/admin/kafuda-gpt/config", "")
	if code != response.CodeOK || data["configured"] != false || data["has_api_key"] != false || data["config_source"] != "config_file" || data["sales_block_reason"] != "gateway_not_configured" {
		t.Fatalf("unexpected initial config response: code=%d data=%+v", code, data)
	}

	// 2. Test connection with unsaved credentials succeeds and returns cards.
	testPayload, _ := json.Marshal(map[string]any{
		"api_base_url": supplierOrigin,
		"api_key":      apiKey,
	})
	code, data, _ = call(http.MethodPost, "/admin/kafuda-gpt/config/test", string(testPayload))
	if code != response.CodeOK || data["ok"] != true || data["enabled"] != true {
		t.Fatalf("config test failed: code=%d data=%+v", code, data)
	}

	// 3. Save config via PUT -> hot-reloads Gateway and Commerce policy immediately.
	savePayload, _ := json.Marshal(map[string]any{
		"api_base_url":              supplierOrigin,
		"api_key":                   apiKey,
		"product_code":              cardCode,
		"max_cost_cents":            2500,
		"allowed_kinds":             []string{"new"},
		"sales_enabled":             true,
		"reference_lookup_verified": true,
		"catalog_ttl_seconds":       3600,
		"poll_interval_seconds":     10,
		"poll_review_after_seconds": 86400,
	})
	code, data, _ = call(http.MethodPut, "/admin/kafuda-gpt/config", string(savePayload))
	if code != response.CodeOK || data["configured"] != true || data["has_api_key"] != true || data["config_source"] != "database" || data["sales_block_reason"] != "catalog_stale" {
		t.Fatalf("unexpected save response: code=%d data=%+v", code, data)
	}
	if module.GatewayRef() == nil {
		t.Fatal("expected hot-reloaded gateway on module")
	}

	// 4. Sync catalog now works without restarting.
	code, data, _ = call(http.MethodPost, "/admin/kafuda-gpt/catalog/sync", "")
	if code != response.CodeOK || data["configured"] != true || data["stale"] != false {
		t.Fatalf("catalog sync after config save failed: code=%d data=%+v", code, data)
	}
	if reason, _ := data["sales_block_reason"].(string); reason != "" {
		t.Fatalf("expected sales ready after catalog sync, got reason %q", reason)
	}

	// 5. GET config now reports fully ready with cards.
	code, data, _ = call(http.MethodGet, "/admin/kafuda-gpt/config", "")
	if code != response.CodeOK || data["sales_block_reason"] != "" || data["catalog_stale"] != false {
		t.Fatalf("expected ready config status, got: %+v", data)
	}

	// 6. Clearing API key immediately disables gateway and blocks sales.
	code, data, _ = call(http.MethodPut, "/admin/kafuda-gpt/config", `{"clear_api_key":true}`)
	if code != response.CodeOK || data["configured"] != false || data["has_api_key"] != false || data["sales_block_reason"] != "gateway_not_configured" {
		t.Fatalf("expected cleared key to block sales, got: %+v", data)
	}
}
