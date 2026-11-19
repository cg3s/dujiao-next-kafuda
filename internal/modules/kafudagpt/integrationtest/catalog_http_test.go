package integrationtest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
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

type singleReadCatalogStore struct {
	kafudacontract.Store
	reads int
}

func (s *singleReadCatalogStore) GetCatalog(ctx context.Context, profile string) (*kafudadomain.Catalog, error) {
	s.reads++
	if s.reads > 1 {
		return nil, errors.New("catalog unavailable on second read")
	}
	return s.Store.GetCatalog(ctx, profile)
}

func recordedCatalogData(t *testing.T) json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "docs", "gpt_catalog_data.json"))
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct{ Data json.RawMessage }
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatal(err)
	}
	return envelope.Data
}

func TestAdminCatalogRefreshUsesPersistedSnapshotWithoutSecondRead(t *testing.T) {
	_, store, _ := database(t)
	// The first valid read comes from the committed cache and must also be
	// sufficient to build the response if the database is briefly unavailable.
	reads := &singleReadCatalogStore{Store: store}
	service := kafudaapp.NewCatalogService(reads, failingCatalogGateway{data: recordedCatalogData(t)}, "default", time.Hour)
	router := gin.New()
	commerce := kafudaapp.NewCommerceService(kafudaapp.CommerceOptions{
		Catalog: service, SalesEnabled: true, ReferenceLookupVerified: true,
		ProductCode: "test-card", MaxCostCents: 1000, AllowedKinds: []string{"new"},
	})
	kafudahttp.RegisterAdminRoutes(router.Group("/admin"), &kafudahttp.AdminHandler{
		Catalog: service, Configured: true, SalesBlockReason: commerce.CatalogSalesBlockReason,
	})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/admin/kafuda-gpt/catalog/sync", nil))
	var body struct {
		StatusCode int `json:"status_code"`
		Data       struct {
			Options          []kafudacontract.SKUOption `json:"options"`
			Cards            []kafudadomain.CardProduct `json:"cards"`
			Enabled          bool                       `json:"enabled"`
			Stale            bool                       `json:"stale"`
			SyncedAt         time.Time                  `json:"synced_at"`
			ExpiresAt        time.Time                  `json:"expires_at"`
			SalesBlockReason string                     `json:"sales_block_reason"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusOK || body.StatusCode != 0 || reads.reads != 1 || len(body.Data.Options) != 60 || len(body.Data.Cards) != 11 {
		t.Fatalf("catalog refresh failed or re-read the cache: reads=%d, response=%s", reads.reads, recorder.Body.String())
	}
	if body.Data.SalesBlockReason != "fulfillment_not_wired" {
		t.Fatalf("missing sales status: %q", body.Data.SalesBlockReason)
	}
	saved, err := store.GetCatalog(context.Background(), "default")
	if err != nil || saved == nil {
		t.Fatalf("refreshed catalog was not persisted: %v", err)
	}
	if !body.Data.SyncedAt.Equal(saved.SyncedAt) || !body.Data.ExpiresAt.Equal(saved.ExpiresAt) || body.Data.Stale || body.Data.Enabled != saved.Enabled {
		t.Fatal("response metadata does not match the saved catalog")
	}
	for i, option := range body.Data.Options {
		entry := saved.Options[i]
		if option.Plan != entry.Plan || option.Country != entry.Country || option.Available != (saved.Enabled && entry.Available) || option.CatalogRevision != saved.Revision(entry.Plan, entry.Country) {
			t.Fatalf("response option does not match the saved catalog: %+v", option)
		}
	}
}

func TestAdminCatalogExplainsSnapshotSalesBlocks(t *testing.T) {
	const cardCode = "deployment-card"
	now := time.Now().UTC()
	ready := kafudadomain.Catalog{
		Profile: "default", Enabled: true, SyncedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour),
		Options: []kafudadomain.CatalogOption{{Plan: "plus", Country: "PH", Product: "gpt", Available: true}},
		Cards:   []kafudadomain.CardProduct{{ProductCode: cardCode}},
	}
	stale, disabled, missingCard, restrictedCard := ready, ready, ready, ready
	stale.ExpiresAt = now.Add(-time.Minute)
	disabled.Enabled = false
	missingCard.Cards = []kafudadomain.CardProduct{{ProductCode: "alternate-card"}}
	restrictedCard.Cards = []kafudadomain.CardProduct{{ProductCode: cardCode, Restricted: true}}
	for _, tc := range []struct {
		name, reason string
		catalog      *kafudadomain.Catalog
		workerReady  bool
		stale        bool
	}{
		{"no_cache", "catalog_stale", nil, true, true},
		{"expired", "catalog_stale", &stale, true, true},
		{"disabled", "catalog_disabled", &disabled, true, false},
		{"missing_card", "card_product_unavailable", &missingCard, true, false},
		{"restricted_card", "card_product_unavailable", &restrictedCard, true, false},
		{"worker_block_takes_precedence", "worker_not_ready", &stale, false, true},
		{"ready", "", &ready, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, store, _ := database(t)
			if tc.catalog != nil {
				if err := store.SaveCatalog(t.Context(), tc.catalog); err != nil {
					t.Fatal(err)
				}
			}
			reads := &singleReadCatalogStore{Store: store}
			catalog := kafudaapp.NewCatalogService(reads, nil, "default", time.Hour)
			commerce := kafudaapp.NewCommerceService(kafudaapp.CommerceOptions{
				Catalog: catalog, SalesEnabled: true, ReferenceLookupVerified: true,
				ProductCode: cardCode, MaxCostCents: 1000, AllowedKinds: []string{"new"},
				WorkerReady: func(context.Context) bool { return tc.workerReady },
			})
			router := gin.New()
			kafudahttp.RegisterAdminRoutes(router.Group("/admin"), &kafudahttp.AdminHandler{
				Catalog: catalog, Configured: true, SalesBlockReason: commerce.CatalogSalesBlockReason,
			})
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/admin/kafuda-gpt/catalog", nil))
			var body struct {
				StatusCode int `json:"status_code"`
				Data       struct {
					Reason string `json:"sales_block_reason"`
					Stale  bool   `json:"stale"`
				} `json:"data"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil || body.StatusCode != response.CodeOK ||
				body.Data.Reason != tc.reason || body.Data.Stale != tc.stale || reads.reads != 1 {
				t.Fatalf("want reason %q and stale=%t from one snapshot, got: %s (%v)", tc.reason, tc.stale, recorder.Body.String(), err)
			}
		})
	}
}

func TestIncompleteCatalogRefreshPreservesLastValidEligibility(t *testing.T) {
	fixture := recordedCatalogData(t)
	for _, scenario := range []struct {
		name, collection, key, value, field string
		replacement                         json.RawMessage
	}{
		{name: "missing_credit", collection: "plans", key: "plan", value: "credit250", field: "credit"},
		{name: "null_credit", collection: "plans", key: "plan", value: "credit250", field: "credit", replacement: json.RawMessage(`null`)},
		{name: "missing_restriction", collection: "auto_card_products", key: "product_code", value: "PP5450RC", field: "restriction_reason"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var payload map[string]json.RawMessage
			if err := json.Unmarshal(fixture, &payload); err != nil {
				t.Fatal(err)
			}
			var entries []map[string]json.RawMessage
			if err := json.Unmarshal(payload[scenario.collection], &entries); err != nil {
				t.Fatal(err)
			}
			found := false
			for _, entry := range entries {
				var key string
				if err := json.Unmarshal(entry[scenario.key], &key); err != nil {
					t.Fatal(err)
				}
				if key != scenario.value {
					continue
				}
				if _, defined := entry[scenario.field]; !defined {
					t.Fatalf("fixture %s is already missing %s", key, scenario.field)
				}
				if scenario.replacement == nil {
					delete(entry, scenario.field)
				} else {
					entry[scenario.field] = scenario.replacement
				}
				found = true
				break
			}
			if !found {
				t.Fatalf("fixture does not include %s", scenario.value)
			}
			mutatedEntries, err := json.Marshal(entries)
			if err != nil {
				t.Fatal(err)
			}
			payload[scenario.collection] = mutatedEntries
			malformed, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}

			_, store, _ := database(t)
			ctx, syncedAt := t.Context(), time.Now().UTC().Add(-time.Minute)
			original, err := kafudaapp.ParseCatalog(fixture, "default", syncedAt, syncedAt.Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			if err := store.SaveCatalog(ctx, original); err != nil {
				t.Fatal(err)
			}
			service := kafudaapp.NewCatalogService(store, failingCatalogGateway{data: malformed}, original.Profile, time.Hour)
			before, err := service.Cached(ctx)
			if err != nil || before == nil {
				t.Fatalf("read original supplier catalog: %v", err)
			}
			options := kafudaapp.CommerceOptions{
				Catalog: service, SalesEnabled: true, ReferenceLookupVerified: true, ProductCode: "XL537872", MaxCostCents: 2500,
				AllowedKinds: []string{"new"}, WorkerReady: func(context.Context) bool { return true },
			}
			allowed := kafudaapp.NewCommerceService(options)
			options.ProductCode = "PP5450RC"
			restricted := kafudaapp.NewCommerceService(options)
			assertEligibility := func() {
				t.Helper()
				const country = "PH"
				for plan, product := range map[string]string{"credit250": "gpt_credits", "plus": "gpt"} {
					revision := before.Revision(plan, country)
					if terms, err := allowed.PurchaseTerms(ctx, plan, country, revision); err != nil || terms == nil || terms.Product != product {
						t.Fatalf("approved terms for %s changed from %s: %+v, %v", plan, product, terms, err)
					}
					if terms, err := restricted.PurchaseTerms(ctx, plan, country, revision); terms != nil || !errors.Is(err, kafudaapp.ErrQuotePolicy) {
						t.Fatalf("restricted card was eligible for %s: %+v, %v", plan, terms, err)
					}
				}
			}
			assertEligibility()

			router := gin.New()
			kafudahttp.RegisterAdminRoutes(router.Group("/admin"), &kafudahttp.AdminHandler{Catalog: service, Configured: true})
			req := httptest.NewRequest(http.MethodPost, "/admin/kafuda-gpt/catalog/sync", nil).WithContext(ctx)
			req.Header.Set("Accept-Language", "en-US")
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, req)
			var result response.Response
			if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil || recorder.Code != http.StatusOK ||
				result.StatusCode != response.CodeInternal || result.Msg != i18n.T("en-US", "error.kafuda_catalog_invalid_response") {
				t.Fatalf("refresh of incomplete catalog returned HTTP %d, API %d %q: %v", recorder.Code, result.StatusCode, result.Msg, err)
			}
			after, err := service.Cached(ctx)
			if err != nil || !reflect.DeepEqual(after, before) {
				t.Fatalf("incomplete refresh changed the last valid directory: %v", err)
			}
			assertEligibility()
		})
	}
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
				kafudahttp.RegisterAdminRoutes(router.Group("/admin"), &kafudahttp.AdminHandler{Catalog: service, Configured: true})
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
	router := gin.New()
	kafudahttp.RegisterAdminRoutes(router.Group("/admin"), &kafudahttp.AdminHandler{Catalog: catalogService})
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
