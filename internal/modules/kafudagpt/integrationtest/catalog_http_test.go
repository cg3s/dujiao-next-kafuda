package integrationtest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dujiao-next/internal/i18n"
	"github.com/dujiao-next/internal/logger"
	kafudaapp "github.com/dujiao-next/internal/modules/kafudagpt/application"
	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
	kafudahttp "github.com/dujiao-next/internal/modules/kafudagpt/transport/http"
	"github.com/dujiao-next/internal/platform/http/response"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

type catalogFailureStore struct {
	kafudacontract.Store
	readErr, writeErr error
}

func (s catalogFailureStore) GetCatalog(ctx context.Context, profile string) (*kafudadomain.Catalog, error) {
	if s.readErr != nil {
		return nil, s.readErr
	}
	return s.Store.GetCatalog(ctx, profile)
}
func (s catalogFailureStore) SaveCatalog(ctx context.Context, catalog *kafudadomain.Catalog) error {
	if s.writeErr != nil {
		return s.writeErr
	}
	return s.Store.SaveCatalog(ctx, catalog)
}

func TestCatalogFailuresAreActionableAndDoNotLeakUpstreamData(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	previous := logger.L
	logger.L = zap.New(core)
	t.Cleanup(func() { logger.L = previous })
	for _, locale := range []string{"zh-CN", "zh-TW", "en-US"} {
		for _, scenario := range []struct {
			name, key string
			status    int
		}{
			{"auth", "auth_failed", 401}, {"permission", "forbidden", 403}, {"endpoint", "endpoint_missing", 404}, {"rate", "rate_limited", 429},
			{"redirect", "redirect", 307}, {"upstream", "upstream_unavailable", 503}, {"body", "invalid_response", 200}, {"network", "network_failed", 0},
			{"schema", "invalid_response", 0}, {"read", "cache_read_failed", 0}, {"write", "cache_write_failed", 0}, {"unknown", "failed", 0}, {"untrusted_code", "failed", 200},
		} {
			t.Run(locale+"/"+scenario.name, func(t *testing.T) {
				_, store, _ := database(t)
				now := time.Now().Add(-time.Hour)
				initial := &kafudadomain.Catalog{Profile: "default", Enabled: true, SyncedAt: now, ExpiresAt: now.Add(2 * time.Hour), Options: []kafudadomain.CatalogOption{{Plan: "plus", Country: "PH", Product: "gpt", Available: true}}, Cards: []kafudadomain.CardProduct{}}
				if err := store.SaveCatalog(context.Background(), initial); err != nil {
					t.Fatal(err)
				}
				secret := "merchant-token-and-session-must-not-leak"
				gateway := failingCatalogGateway{err: &kafudacontract.APIError{HTTPStatus: scenario.status, Code: secret}}
				wrapped := catalogFailureStore{Store: store}
				path, method := "/admin/kafuda-gpt/catalog/sync", http.MethodPost
				switch scenario.name {
				case "network":
					gateway.err = &kafudacontract.APIError{Code: "transport_error"}
				case "body":
					gateway.err = &kafudacontract.APIError{HTTPStatus: 200, Code: "invalid_response"}
				case "schema":
					gateway = failingCatalogGateway{data: json.RawMessage(`{"enabled":true,"plans":null,"auto_card_products":[]}`)}
				case "read":
					wrapped.readErr = errors.New(secret)
					path, method = "/admin/kafuda-gpt/catalog", http.MethodGet
				case "write":
					wrapped.writeErr = errors.New(secret)
					gateway = failingCatalogGateway{data: json.RawMessage(`{"enabled":false,"plans":[],"auto_card_products":[]}`)}
				case "unknown":
					gateway.err = errors.New(secret)
				}
				service := kafudaapp.NewCatalogService(wrapped, gateway, "default", time.Hour)
				router := gin.New()
				router.Use(func(c *gin.Context) { c.Set("request_id", "catalog-test-request"); c.Next() })
				kafudahttp.RegisterAdminRoutes(router.Group("/admin"), &kafudahttp.AdminHandler{Catalog: service, Policy: kafudaapp.NewCommerceService(kafudaapp.CommerceOptions{Catalog: service}), Configured: true})
				request := httptest.NewRequest(method, path, nil)
				request.Header.Set("X-Lang", locale)
				recorder := httptest.NewRecorder()
				logs.TakeAll()
				router.ServeHTTP(recorder, request)
				var result response.Response
				if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				key := "error.kafuda_catalog_" + scenario.key
				if recorder.Code != http.StatusOK || result.StatusCode != response.CodeInternal || result.Msg != i18n.T(locale, key) || result.Msg == key {
					t.Fatalf("incorrect catalog diagnostic: %s", recorder.Body.String())
				}
				entries := logs.TakeAll()
				if len(entries) != 1 || entries[0].Message != "kafuda_catalog_failed" || entries[0].ContextMap()["reason"] != key || entries[0].ContextMap()["request_id"] != "catalog-test-request" {
					t.Fatalf("missing safe diagnostic: %+v", entries)
				}
				if fmt.Sprint(entries[0].ContextMap()["upstream_http_status"]) != fmt.Sprint(scenario.status) {
					t.Fatal("upstream status was lost in diagnostic logs")
				}
				if strings.Contains(recorder.Body.String()+fmt.Sprint(entries), secret) {
					t.Fatal("raw upstream error reached response or logs")
				}
				current, err := store.GetCatalog(context.Background(), "default")
				if err != nil || !current.SyncedAt.Equal(initial.SyncedAt) || !current.Available("plus", "PH", time.Now()) {
					t.Fatal("failed refresh overwrote last valid catalog")
				}
			})
		}
	}
}

func TestAdminExpiredCatalogKeepsListingStateAndReportsStaleness(t *testing.T) {
	_, store, _ := database(t)
	now := time.Now().UTC()
	catalog := &kafudadomain.Catalog{
		Profile: "default", Enabled: true, SyncedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Hour),
		Options: []kafudadomain.CatalogOption{{Plan: "plus", Country: "PH", Product: "gpt", Label: "Plus", Available: true}},
	}
	if err := store.SaveCatalog(context.Background(), catalog); err != nil {
		t.Fatal(err)
	}
	catalogService := kafudaapp.NewCatalogService(store, nil, "default", time.Hour)
	policy := kafudaapp.NewCommerceService(kafudaapp.CommerceOptions{Catalog: catalogService})
	router := gin.New()
	kafudahttp.RegisterAdminRoutes(router.Group("/admin"), &kafudahttp.AdminHandler{Catalog: catalogService, Policy: policy})
	request := httptest.NewRequest(http.MethodGet, "/admin/kafuda-gpt/catalog", nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("catalog request failed: %d %s", response.Code, response.Body.String())
	}
	var body struct {
		Data struct {
			Stale   bool                       `json:"stale"`
			Options []kafudacontract.SKUOption `json:"options"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.Data.Stale || len(body.Data.Options) != 1 || !body.Data.Options[0].Available {
		t.Fatalf("staleness replaced supplier listing state: %+v", body.Data)
	}
}
