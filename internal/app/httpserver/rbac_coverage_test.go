package httpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dujiao-next/internal/app/container"
	"github.com/dujiao-next/internal/authz"
	databasemigrations "github.com/dujiao-next/internal/bootstrap/database/migrations"
	"github.com/dujiao-next/internal/config"
	"github.com/dujiao-next/internal/logger"
	admindomain "github.com/dujiao-next/internal/modules/identity/admin/domain"
	kafudaapp "github.com/dujiao-next/internal/modules/kafudagpt/application"
	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	"github.com/dujiao-next/internal/platform/database/gormdb"
	"github.com/dujiao-next/internal/platform/http/response"

	"github.com/casbin/casbin/v3/util"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
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

func TestKafudaCatalogRoutesRequireOperationsPermission(t *testing.T) {
	const jwtSecret = "kafuda-catalog-test-admin-jwt-secret"
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

	cfg := &config.Config{
		App: config.AppConfig{SecretKey: jwtSecret}, JWT: config.JWTConfig{SecretKey: jwtSecret}, Server: config.ServerConfig{Mode: gin.TestMode},
	}
	deps, err := container.NewContainer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	tokens := make(map[string]string, 2)
	for _, role := range []string{"support", "operations"} {
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
