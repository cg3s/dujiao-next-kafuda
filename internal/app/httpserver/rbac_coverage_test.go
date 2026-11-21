package httpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dujiao-next/internal/app/container"
	"github.com/dujiao-next/internal/authz"
	"github.com/dujiao-next/internal/bootstrap/database/migrations"
	databasemigrations "github.com/dujiao-next/internal/bootstrap/database/migrations"
	"github.com/dujiao-next/internal/config"
	"github.com/dujiao-next/internal/constants"
	"github.com/dujiao-next/internal/i18n"
	"github.com/dujiao-next/internal/logger"
	categorydomain "github.com/dujiao-next/internal/modules/catalog/category/domain"
	productdomain "github.com/dujiao-next/internal/modules/catalog/product/domain"
	productpresenter "github.com/dujiao-next/internal/modules/catalog/product/transport/presenter"
	admindomain "github.com/dujiao-next/internal/modules/identity/admin/domain"
	tokenapp "github.com/dujiao-next/internal/modules/identity/adminapitoken/application"
	userdomain "github.com/dujiao-next/internal/modules/identity/user/domain"
	kafudaapp "github.com/dujiao-next/internal/modules/kafudagpt/application"
	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
	kafudastore "github.com/dujiao-next/internal/modules/kafudagpt/infrastructure/gormstore"
	orderdomain "github.com/dujiao-next/internal/modules/order/domain"
	paymentdomain "github.com/dujiao-next/internal/modules/payment/domain"
	walletdomain "github.com/dujiao-next/internal/modules/wallet/domain"
	"github.com/dujiao-next/internal/platform/database/gormdb"
	"github.com/dujiao-next/internal/platform/http/response"
	"github.com/dujiao-next/internal/shared/jsonmap"
	"github.com/dujiao-next/internal/shared/money"

	"github.com/casbin/casbin/v3/util"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// TestAllAdminRoutesCoveredByBuiltinRoles 校验 admin 路由文件里的每条路由
// 都被 authz.BuiltinRoleSeeds() 中至少一条角色策略覆盖。
//
// 目的：避免新增 admin 接口时忘记同步 RBAC 预置角色，导致非超管角色无法通过
// 角色分配获得该权限（catalog UI 上能看到，但任何角色都拿不到）。
//
// 实现：静态扫描 routes_admin*.go、internal/modules/**/routes.go
// 以及 internal/platform/http/**/routes.go，
// 提取 admin/authorized/paymentProtected.METHOD("/path", ...) 调用，
// 与 builtin role seeds 用 keyMatch2 比对（与运行时 Casbin 模型一致）。
func TestAllAdminRoutesCoveredByBuiltinRoles(t *testing.T) {
	routes, err := extractAdminRoutesFromSource()
	if err != nil {
		t.Fatalf("extract admin routes: %v", err)
	}
	if len(routes) == 0 {
		t.Fatalf("no admin routes extracted; parser or source layout changed?")
	}
	t.Logf("validating RBAC coverage for %d admin routes", len(routes))

	seeds := authz.BuiltinRoleSeeds()
	if len(seeds) == 0 {
		t.Fatalf("no builtin role seeds")
	}

	type policy struct {
		object string
		action string
	}
	var policies []policy
	for _, seed := range seeds {
		for _, p := range seed.Policies {
			policies = append(policies, policy{
				object: authz.NormalizeObject(p.Object),
				action: authz.NormalizeAction(p.Action),
			})
		}
	}

	var uncovered []adminRoute
	for _, r := range routes {
		matched := false
		for _, p := range policies {
			if p.action != "*" && p.action != r.method {
				continue
			}
			if util.KeyMatch2(r.object, p.object) {
				matched = true
				break
			}
		}
		if !matched {
			uncovered = append(uncovered, r)
		}
	}

	if len(uncovered) > 0 {
		var lines []string
		for _, r := range uncovered {
			lines = append(lines, "  "+r.method+" "+r.object)
		}
		t.Fatalf("the following admin routes are not covered by any builtin role seed in authz.BuiltinRoleSeeds() — add them to the appropriate role in api/internal/authz/bootstrap.go:\n%s",
			strings.Join(lines, "\n"))
	}
}

func TestAdminAPITokenAllowlistMatchesRegisteredRoutes(t *testing.T) {
	originalDB, originalLog := gormdb.DB, logger.L
	t.Cleanup(func() { gormdb.DB, logger.L = originalDB, originalLog })
	logger.L = zap.NewNop()
	require.NoError(t, gormdb.InitDB("sqlite", ":memory:", gormdb.DBPoolConfig{MaxOpenConns: 1, MaxIdleConns: 1}, "release"))
	db, err := gormdb.DB.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, migrations.AutoMigrate())

	cfg := &config.Config{}
	cfg.App.SecretKey = t.Name()
	cfg.Web.AdminPath = "/admin"
	c, err := container.NewContainer(cfg)
	require.NoError(t, err)
	engine := SetupRouter(cfg, c)
	registered := make(map[string]bool)
	for _, route := range engine.Routes() {
		registered[route.Method+" "+route.Path] = true
	}
	for _, route := range tokenapp.AllowedRoutes() {
		if !registered[route] {
			t.Errorf("admin API token allowlist refers to unregistered route %s", route)
		}
	}
}

type adminRoute struct {
	method string
	object string // 例如 "/admin/users/:id"
}

var adminRouteMethods = map[string]struct{}{
	"GET":    {},
	"POST":   {},
	"PUT":    {},
	"PATCH":  {},
	"DELETE": {},
}

var adminRouteReceivers = map[string]struct{}{
	"admin":            {},
	"authorized":       {},
	"paymentProtected": {},
}

var publicAdminRoutes = map[string]struct{}{
	"POST /admin/login":            {},
	"POST /admin/login/verify-2fa": {},
}

// extractAdminRoutesFromSource 从应用 admin 路由、模块路由和平台 HTTP 路由文件中读取调用。
// 方法范围：GET / POST / PUT / PATCH / DELETE。HEAD/OPTIONS 不参与 RBAC。
func extractAdminRoutesFromSource() ([]adminRoute, error) {
	_, thisFile, _, _ := runtime.Caller(0)
	routerDirectory := filepath.Dir(thisFile)

	sources, err := discoverAdminRouteSources(routerDirectory)
	if err != nil {
		return nil, err
	}
	if len(sources) == 0 {
		return nil, os.ErrNotExist
	}

	seen := make(map[string]struct{})
	var out []adminRoute
	for _, source := range sources {
		routes, err := extractAdminRoutesFromFile(source)
		if err != nil {
			return nil, err
		}
		for _, route := range routes {
			key := route.method + " " + route.object
			if _, public := publicAdminRoutes[key]; public {
				continue
			}
			if _, duplicate := seen[key]; duplicate {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, route)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].object == out[j].object {
			return out[i].method < out[j].method
		}
		return out[i].object < out[j].object
	})
	return out, nil
}

func discoverAdminRouteSources(routerDirectory string) ([]string, error) {
	adminSources, err := filepath.Glob(filepath.Join(routerDirectory, "routes_admin*.go"))
	if err != nil {
		return nil, err
	}

	sources := make([]string, 0, len(adminSources)+32)
	for _, path := range adminSources {
		if !strings.HasSuffix(path, "_test.go") {
			sources = append(sources, path)
		}
	}

	for _, directory := range []string{
		filepath.Join(routerDirectory, "..", "..", "modules"),
		filepath.Join(routerDirectory, "..", "..", "platform", "http"),
	} {
		if err := filepath.WalkDir(directory, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if !entry.IsDir() && entry.Name() == "routes.go" {
				sources = append(sources, path)
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}

	sort.Strings(sources)
	return sources, nil
}

func TestExtractAdminRoutesIncludesPlatformHTTPRoutes(t *testing.T) {
	routes, err := extractAdminRoutesFromSource()
	if err != nil {
		t.Fatalf("extract admin routes: %v", err)
	}
	for _, route := range routes {
		if route.method == "GET" && route.object == "/admin/system/version/check" {
			return
		}
	}
	t.Fatal("platform HTTP route GET /admin/system/version/check was not discovered")
}

type kafudaCatalogAccessGateway struct {
	kafudacontract.Gateway
	calls int
}

func (g *kafudaCatalogAccessGateway) Catalog(context.Context) (json.RawMessage, error) {
	g.calls++
	return json.RawMessage(`{"enabled":true,"plans":[],"auto_card_products":[]}`), nil
}

func openKafudaHTTPTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	gin.SetMode(gin.TestMode)
	previousLogger := logger.L
	logger.L = zap.NewNop()
	t.Cleanup(func() { logger.L = previousLogger })

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
	previousDB := gormdb.DB
	gormdb.DB = db
	t.Cleanup(func() { gormdb.DB = previousDB })
	if err := databasemigrations.AutoMigrate(); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestKafudaAdminRoutesEnforceScopedPermissions(t *testing.T) {
	const jwtSecret = "kafuda-catalog-test-admin-jwt-secret"
	openKafudaHTTPTestDB(t)

	cfg := &config.Config{
		App: config.AppConfig{SecretKey: jwtSecret}, JWT: config.JWTConfig{SecretKey: jwtSecret}, Server: config.ServerConfig{Mode: gin.TestMode},
	}
	deps, err := container.NewContainer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	tokens := make(map[string]string, 3)
	for _, role := range []string{"support", "operations", "integration"} {
		admin := &admindomain.Admin{Username: role, PasswordHash: "test-hash"}
		if err := deps.AdminStore.Create(admin); err != nil {
			t.Fatal(err)
		}
		if err := deps.AuthzService.SetAdminRoles(admin.ID, []string{role}); err != nil {
			t.Fatal(err)
		}
		tokens[role] = signAdminRouteToken(t, jwtSecret, admin)
	}

	gateway := &kafudaCatalogAccessGateway{}
	store := deps.KafudaGPT.Store
	deps.KafudaGPT.Gateway = gateway
	deps.KafudaGPT.Catalog = kafudaapp.NewCatalogService(store, gateway, "default", time.Hour)
	deps.KafudaGPT.Commerce = kafudaapp.NewCommerceService(kafudaapp.CommerceOptions{Catalog: deps.KafudaGPT.Catalog})
	router := SetupRouter(cfg, deps)

	routes := []adminRoute{{method: http.MethodGet, object: "/admin/kafuda-gpt/catalog"}, {method: http.MethodPost, object: "/admin/kafuda-gpt/catalog/sync"}}
	discovered, err := extractAdminRoutesFromSource()
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range routes {
		if !slices.Contains(discovered, route) {
			t.Errorf("RBAC coverage did not discover %s %s", route.method, route.object)
		}
	}
	for _, actor := range []struct {
		name string
		want int
	}{
		{name: "anonymous", want: response.CodeUnauthorized},
		{name: "support", want: response.CodeForbidden},
		{name: "operations", want: response.CodeOK},
	} {
		for _, route := range routes {
			t.Run(actor.name+"/"+route.method, func(t *testing.T) {
				request := httptest.NewRequest(route.method, "/api/v1"+route.object, nil)
				if bearer := tokens[actor.name]; bearer != "" {
					request.Header.Set("Authorization", "Bearer "+bearer)
				}
				beforeCalls := gateway.calls
				recorder := httptest.NewRecorder()
				router.ServeHTTP(recorder, request)
				var body struct {
					StatusCode int `json:"status_code"`
					Data       struct {
						Configured bool `json:"configured"`
					} `json:"data"`
				}
				if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil || body.StatusCode != actor.want {
					t.Fatalf("expected business status %d, got %s (%v)", actor.want, recorder.Body.String(), err)
				}
				wantCalls := 0
				if actor.want == response.CodeOK {
					if !body.Data.Configured {
						t.Fatal("operations request did not reach the configured catalog")
					}
					if route.method == http.MethodPost {
						wantCalls = 1
					}
				}
				if calls := gateway.calls - beforeCalls; calls != wantCalls {
					t.Fatalf("expected %d upstream requests, got %d", wantCalls, calls)
				}
			})
		}
	}
	catalog, err := store.GetCatalog(t.Context(), "default")
	if err != nil || catalog == nil || !catalog.Enabled {
		t.Fatalf("authorized refresh did not persist the upstream response: %+v, %v", catalog, err)
	}

	configRoutes := []struct {
		route adminRoute
		body  string
	}{
		{route: adminRoute{method: http.MethodGet, object: "/admin/kafuda-gpt/config"}},
		{route: adminRoute{method: http.MethodPut, object: "/admin/kafuda-gpt/config"}, body: `{"sales_enabled":false}`},
		{route: adminRoute{method: http.MethodPost, object: "/admin/kafuda-gpt/config/test"}, body: `{}`},
	}
	for _, item := range configRoutes {
		if !slices.Contains(discovered, item.route) {
			t.Errorf("RBAC coverage did not discover %s %s", item.route.method, item.route.object)
		}
	}
	for _, actor := range []struct {
		name     string
		wantGet  int
		wantPut  int
		wantTest int
	}{
		{name: "anonymous", wantGet: response.CodeUnauthorized, wantPut: response.CodeUnauthorized, wantTest: response.CodeUnauthorized},
		{name: "support", wantGet: response.CodeForbidden, wantPut: response.CodeForbidden, wantTest: response.CodeForbidden},
		{name: "operations", wantGet: response.CodeForbidden, wantPut: response.CodeForbidden, wantTest: response.CodeForbidden},
		{name: "integration", wantGet: response.CodeOK, wantPut: response.CodeOK, wantTest: response.CodeBadRequest},
	} {
		for _, item := range configRoutes {
			t.Run("config/"+actor.name+"/"+item.route.method, func(t *testing.T) {
				req := httptest.NewRequest(item.route.method, "/api/v1"+item.route.object, strings.NewReader(item.body))
				if item.body != "" {
					req.Header.Set("Content-Type", "application/json")
				}
				if bearer := tokens[actor.name]; bearer != "" {
					req.Header.Set("Authorization", "Bearer "+bearer)
				}
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, req)
				var body response.Response
				want := actor.wantGet
				if item.route.method == http.MethodPut {
					want = actor.wantPut
				} else if item.route.method == http.MethodPost {
					want = actor.wantTest
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.StatusCode != want {
					t.Fatalf("expected business status %d, got %s (%v)", want, rec.Body.String(), err)
				}
			})
		}
	}

	confirmation := adminRoute{method: http.MethodPost, object: "/admin/orders/:id/direct-confirm-upstream"}
	if !slices.Contains(discovered, confirmation) {
		t.Fatal("RBAC coverage did not discover upstream confirmation")
	}
	for _, actor := range []struct {
		name string
		want int
	}{
		{name: "anonymous", want: response.CodeUnauthorized},
		{name: "support", want: response.CodeBadRequest},
		{name: "operations", want: response.CodeForbidden},
	} {
		t.Run("confirm_upstream/"+actor.name, func(t *testing.T) {
			// A missing confirmation reaches the handler only for authorized staff.
			request := httptest.NewRequest(confirmation.method, "/api/v1/admin/orders/101/direct-confirm-upstream", strings.NewReader(`{"attempt_id":1}`))
			request.Header.Set("Content-Type", "application/json")
			if bearer := tokens[actor.name]; bearer != "" {
				request.Header.Set("Authorization", "Bearer "+bearer)
			}
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			var body response.Response
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil || body.StatusCode != actor.want {
				t.Fatalf("expected business status %d, got %s (%v)", actor.want, recorder.Body.String(), err)
			}
		})
	}
}

func TestKafudaProductionStartupBlocksDirectCheckoutWithoutWorker(t *testing.T) {
	testKafudaProductionStartupDirectGate(t, "https://supplier.example.test", "test-catalog-api-key", "worker_not_ready")
}

func TestKafudaProductionStartupKeepsShopAvailableWithInvalidGateway(t *testing.T) {
	for _, tc := range []struct{ name, origin, apiKey string }{
		{"insecure_origin", "http://supplier.example.test", "test-catalog-api-key"},
		{"invalid_key", "https://supplier.example.test", "test-catalog\nkey"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testKafudaProductionStartupDirectGate(t, tc.origin, tc.apiKey, "gateway_config_invalid")
		})
	}
}

func testKafudaProductionStartupDirectGate(t *testing.T, apiURL, apiKey, wantReason string) {
	t.Helper()
	db := openKafudaHTTPTestDB(t)
	// Order creation requires an enabled queue. A blocked checkout never connects to it.
	endpoint, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := endpoint.Addr().(*net.TCPAddr).Port
	if err := endpoint.Close(); err != nil {
		t.Fatal(err)
	}
	const jwtSecret, cardCode = "kafuda-startup-test-jwt-secret-only", "catalog-test-card"
	cfg := &config.Config{
		App: config.AppConfig{SecretKey: jwtSecret}, JWT: config.JWTConfig{SecretKey: jwtSecret}, UserJWT: config.JWTConfig{SecretKey: jwtSecret},
		Server: config.ServerConfig{Mode: gin.TestMode}, Web: config.WebConfig{AdminPath: "/admin"},
		Queue: config.QueueConfig{Enabled: true, Host: "127.0.0.1", Port: port},
		KafudaGPT: config.KafudaGPTConfig{
			APIBaseURL: apiURL, APIKey: apiKey, ProductCode: cardCode,
			SalesEnabled: true, ReferenceLookupVerified: true, MaxCostCents: 2500, AllowedKinds: []string{"new"}, CatalogTTLSeconds: 3600,
		},
	}
	const plan, country = "plus", "PH"
	now := time.Now().UTC()
	if err := kafudastore.New(db).SaveCatalog(t.Context(), &kafudadomain.Catalog{
		Profile: "default", Enabled: true, SyncedAt: now, ExpiresAt: now.Add(time.Hour),
		Options: []kafudadomain.CatalogOption{{Plan: plan, Country: country, Product: "gpt", Available: true}},
		Cards:   []kafudadomain.CardProduct{{ProductCode: cardCode}},
	}); err != nil {
		t.Fatal(err)
	}
	deps, err := container.NewContainer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if deps.QueueClient == nil || !deps.QueueClient.Enabled() {
		t.Fatal("startup did not enable the queue client")
	}
	t.Cleanup(func() { _ = deps.QueueClient.Close() })
	operator := &admindomain.Admin{Username: "catalog-startup-operator", PasswordHash: "fixture"}
	if err := deps.AdminStore.Create(operator); err != nil {
		t.Fatal(err)
	}
	if err := deps.AuthzService.SetAdminRoles(operator.ID, []string{"operations"}); err != nil {
		t.Fatal(err)
	}
	operatorToken := signAdminRouteToken(t, jwtSecret, operator)
	buyer := &userdomain.User{Email: "catalog-startup-buyer@example.test", PasswordHash: "fixture", Status: constants.UserStatusActive}
	if err := deps.UserStore.Create(buyer); err != nil {
		t.Fatal(err)
	}
	buyerToken, _, err := deps.UserAuthService.GenerateUserJWT(buyer, 1)
	if err != nil {
		t.Fatal(err)
	}
	category := &categorydomain.Category{Slug: "startup-catalog", NameJSON: jsonmap.JSON{"en-US": "Startup catalog"}, IsActive: true}
	if err := deps.CategoryRepo.Create(category); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&walletdomain.Account{UserID: buyer.ID, Balance: money.FromDecimal(decimal.NewFromInt(200))}).Error; err != nil {
		t.Fatal(err)
	}
	loadWallet := func() walletdomain.Account {
		t.Helper()
		var wallet walletdomain.Account
		if err := db.Where("user_id = ?", buyer.ID).First(&wallet).Error; err != nil {
			t.Fatal(err)
		}
		return wallet
	}
	beforeWallet := loadWallet()
	router := SetupRouter(cfg, deps)
	request := func(method, path, token, body string, wantCode int, data any) string {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(t.Context())
		req.Header.Set("Accept-Language", "en-US")
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, req)
		var res struct {
			StatusCode int             `json:"status_code"`
			Message    string          `json:"msg"`
			Data       json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &res); err != nil || recorder.Code != http.StatusOK || res.StatusCode != wantCode {
			t.Fatalf("%s %s: HTTP %d, API %d, want API %d: %s (%v)", method, path, recorder.Code, res.StatusCode, wantCode, recorder.Body.String(), err)
		}
		if data != nil {
			if err := json.Unmarshal(res.Data, data); err != nil {
				t.Fatalf("decode %s data: %v", path, err)
			}
		}
		return res.Message
	}
	type catalogView struct {
		Configured bool                       `json:"configured"`
		Enabled    bool                       `json:"enabled"`
		Stale      bool                       `json:"stale"`
		Reason     string                     `json:"sales_block_reason"`
		Options    []kafudacontract.SKUOption `json:"options"`
	}
	var catalog catalogView
	request(http.MethodGet, "/api/v1/admin/kafuda-gpt/catalog", operatorToken, "", response.CodeOK, &catalog)
	wantConfigured := wantReason != "gateway_config_invalid"
	if catalog.Configured != wantConfigured || !catalog.Enabled || catalog.Stale || catalog.Reason != wantReason ||
		len(catalog.Options) != 1 || !catalog.Options[0].Available {
		t.Fatalf("production startup did not isolate its fulfillment gate: %+v", catalog)
	}
	if !wantConfigured {
		if deps.KafudaGPT.Gateway != nil {
			t.Fatal("invalid configuration created an upstream gateway")
		}
		msg := request(http.MethodPost, "/api/v1/admin/kafuda-gpt/catalog/sync", operatorToken, "", response.CodeBadRequest, nil)
		if msg != i18n.T("en-US", "error.kafuda_config_invalid") {
			t.Fatalf("invalid gateway refresh returned an unexpected message: %q", msg)
		}
		var after catalogView
		request(http.MethodGet, "/api/v1/admin/kafuda-gpt/catalog", operatorToken, "", response.CodeOK, &after)
		if !reflect.DeepEqual(catalog, after) {
			t.Fatal("invalid gateway refresh changed the cached catalog")
		}
	}
	const adminProducts = "/api/v1/admin/products"
	var direct productdomain.Product
	request(http.MethodPost, adminProducts, operatorToken, fmt.Sprintf(`{
		"category_id":%d,"slug":"startup-direct","title":{"en-US":"Direct"},"fulfillment_type":"kafuda_gpt_direct",
		"purchase_type":"member","stock_display_mode":"hidden","is_active":true,
		"skus":[{"sku_code":%q,"direct_catalog_revision":%d,"price_amount":100,"cost_price_amount":45,"is_active":true}]
	}`, category.ID, catalog.Options[0].SKUCode, catalog.Options[0].CatalogRevision), response.CodeOK, &direct)
	var savedSKU productdomain.ProductSKU
	if err := db.Where("product_id = ?", direct.ID).First(&savedSKU).Error; err != nil || !direct.IsActive || !savedSKU.IsActive ||
		!savedSKU.DirectCostConfigured || savedSKU.DirectCatalogRevision != catalog.Options[0].CatalogRevision || savedSKU.PriceAmount.String() != "100.00" {
		t.Fatalf("expected an enabled, fully configured direct SKU: %+v (%v)", savedSKU, err)
	}
	var public productpresenter.Product
	request(http.MethodGet, "/api/v1/public/products/"+direct.Slug, "", "", response.CodeOK, &public)
	if public.ID != direct.ID || public.FulfillmentType != constants.FulfillmentTypeGPTDirect || !public.IsSoldOut ||
		len(public.SKUs) != 1 || public.SKUs[0].ID != savedSKU.ID || !public.SKUs[0].IsSoldOut || !public.SKUs[0].IsActive {
		t.Fatalf("storefront did not keep the configured SKU visible and sold out: %+v", public)
	}
	const previewPath = "/api/v1/orders/preview"
	items := fmt.Sprintf(`{"items":[{"product_id":%d,"sku_id":%d,"quantity":1}]}`, direct.ID, savedSKU.ID)
	purchase := strings.TrimSuffix(items, "}") + `,"use_balance":true}`
	for _, check := range []struct{ path, body string }{{previewPath, items}, {"/api/v1/orders/create-and-pay", purchase}} {
		if msg := request(http.MethodPost, check.path, buyerToken, check.body, response.CodeBadRequest, nil); msg != i18n.T("en-US", "error.product_not_available") {
			t.Fatalf("%s was rejected for a different reason: %q", check.path, msg)
		}
	}

	var ordinary productdomain.Product
	request(http.MethodPost, adminProducts, operatorToken, fmt.Sprintf(`{
		"category_id":%d,"slug":"startup-ordinary","title":{"en-US":"Ordinary"},"fulfillment_type":"manual",
		"purchase_type":"member","stock_display_mode":"exact","price_amount":20,"manual_stock_total":2,"is_active":true
	}`, category.ID), response.CodeOK, &ordinary)
	var ordinarySKU productdomain.ProductSKU
	if err := db.Where("product_id = ?", ordinary.ID).First(&ordinarySKU).Error; err != nil {
		t.Fatal(err)
	}
	items = fmt.Sprintf(`{"items":[{"product_id":%d,"sku_id":%d,"quantity":1}]}`, ordinary.ID, ordinarySKU.ID)
	var ordinaryPreview struct {
		Total money.Amount `json:"total_amount"`
	}
	request(http.MethodPost, previewPath, buyerToken, items, response.CodeOK, &ordinaryPreview)
	if ordinaryPreview.Total.String() != "20.00" {
		t.Fatalf("ordinary checkout preview amount = %s, want 20.00", ordinaryPreview.Total)
	}
	for _, model := range []any{&orderdomain.Order{}, &orderdomain.OrderItem{}, &paymentdomain.Payment{}, &kafudadomain.Recharge{}, &kafudadomain.Attempt{}, &walletdomain.Transaction{}} {
		var count int64
		if err := db.Model(model).Count(&count).Error; err != nil || count != 0 {
			t.Fatalf("startup purchase rejection left %d %T rows: %v", count, model, err)
		}
	}
	if current := loadWallet(); !reflect.DeepEqual(current, beforeWallet) {
		t.Fatal("startup purchase rejection changed the buyer wallet")
	}
}

func extractAdminRoutesFromFile(path string) ([]adminRoute, error) {
	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, path, nil, 0)
	if err != nil {
		return nil, err
	}

	var routes []adminRoute
	var routeErr error
	ast.Inspect(file, func(node ast.Node) bool {
		if routeErr != nil {
			return false
		}
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		receiver, ok := selector.X.(*ast.Ident)
		if !ok {
			return true
		}
		if _, ok := adminRouteReceivers[receiver.Name]; !ok {
			return true
		}
		if _, ok := adminRouteMethods[selector.Sel.Name]; !ok {
			return true
		}
		pathLiteral, ok := call.Args[0].(*ast.BasicLit)
		if !ok || pathLiteral.Kind != token.STRING {
			routeErr = fmt.Errorf("%s:%d: admin route path must be a string literal", path, fileSet.Position(call.Args[0].Pos()).Line)
			return false
		}
		routePath, err := strconv.Unquote(pathLiteral.Value)
		if err != nil {
			routeErr = fmt.Errorf("%s:%d: decode admin route path: %w", path, fileSet.Position(pathLiteral.Pos()).Line, err)
			return false
		}
		routes = append(routes, adminRoute{
			method: selector.Sel.Name,
			object: authz.NormalizeObject("/admin" + routePath),
		})
		return true
	})
	if routeErr != nil {
		return nil, routeErr
	}
	return routes, nil
}
