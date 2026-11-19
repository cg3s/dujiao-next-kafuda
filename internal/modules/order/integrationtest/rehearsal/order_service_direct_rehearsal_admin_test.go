package rehearsal_test

import (
	"bytes"
	"crypto/rand"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dujiao-next/internal/app/container"
	"github.com/dujiao-next/internal/app/httpserver/middleware"
	"github.com/dujiao-next/internal/authz"
	adminauthzwiring "github.com/dujiao-next/internal/bootstrap/adminauthz"
	orderwiring "github.com/dujiao-next/internal/bootstrap/order"
	"github.com/dujiao-next/internal/config"
	admindomain "github.com/dujiao-next/internal/modules/identity/admin/domain"
	adminstore "github.com/dujiao-next/internal/modules/identity/admin/infrastructure/gormstore"
	adminauthapp "github.com/dujiao-next/internal/modules/identity/adminauth/application"
	orderrefund "github.com/dujiao-next/internal/modules/order/application/refund"
	orderdomain "github.com/dujiao-next/internal/modules/order/domain"
	orderhttp "github.com/dujiao-next/internal/modules/order/transport/http"
	"github.com/dujiao-next/internal/web"

	"github.com/gin-gonic/gin"
)

const rehearsalAdminPath = "/rehearsal-admin"

func registerDirectRehearsalAdmin(t *testing.T, router *gin.Engine, f directCheckoutFixture, deps *container.Container, dir string) map[string]string {
	t.Helper()
	if !filepath.IsAbs(dir) {
		t.Fatal("the rehearsal admin directory must be absolute")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	assets := root.FS()
	info, err := fs.Stat(assets, "index.html")
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("expected a fullstack admin index.html: %v", err)
	}
	index, err := fs.ReadFile(assets, "index.html")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(index, []byte("__DJ_ADMIN_BASE__")) {
		t.Fatal("expected admin assets built with pnpm run build:fullstack")
	}
	// The production admin shell resolves its prefix using a same-origin base.
	// Its bundled vue-i18n compiler evaluates messages at runtime.
	adminPolicy := strings.Replace(rehearsalStorefrontPolicy, "base-uri 'none'", "base-uri 'self'", 1) + "; script-src 'self' 'unsafe-eval'"
	router.Use(func(c *gin.Context) {
		path := c.Request.URL.Path
		if path == rehearsalAdminPath || strings.HasPrefix(path, rehearsalAdminPath+"/") {
			c.Header("Content-Security-Policy", adminPolicy)
		}
	})
	if err := web.RegisterAdmin(router, rehearsalAdminPath, assets); err != nil {
		t.Fatal(err)
	}

	if err := f.db.AutoMigrate(&admindomain.Admin{}, &orderdomain.OrderRefundRecord{}); err != nil {
		t.Fatal(err)
	}
	deps.AdminStore = adminstore.New(f.db)
	deps.AuthzService, err = authz.NewService(f.db)
	if err != nil {
		t.Fatal(err)
	}
	if err := deps.AuthzService.BootstrapBuiltinRoles(); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{JWT: config.JWTConfig{SecretKey: rand.Text(), ExpireHours: 1}}
	deps.AuthService = adminauthapp.NewService(cfg, deps.AdminStore)
	deps.OrderRefundService = orderrefund.New(f.options.OrderStore, f.options.UserStore, nil, deps.SettingService, deps.WalletService, deps.PaymentStore)
	tokens := make(map[string]string, 2)
	for _, role := range []string{"support", "operations"} {
		admin := &admindomain.Admin{Username: role, PasswordHash: "fixture-login-disabled"}
		if err := deps.AdminStore.Create(admin); err != nil {
			t.Fatal(err)
		}
		if err := deps.AuthzService.SetAdminRoles(admin.ID, []string{role}); err != nil {
			t.Fatal(err)
		}
		token, _, err := deps.AuthService.GenerateJWT(admin)
		if err != nil {
			t.Fatal(err)
		}
		tokens[role] = token
	}
	admin := router.Group("/api/v1/admin",
		middleware.JWTAuthMiddleware(cfg.JWT.SecretKey, deps.AdminStore),
		middleware.AdminRBACMiddleware(deps.AuthzService),
	)
	admin.GET("/authz/me", adminauthzwiring.NewHandler(deps).GetAuthzMe)
	handlers := orderwiring.New(deps)
	orderhttp.RegisterAdminRoutes(admin, handlers.Admin)
	orderhttp.RegisterAdminRefundRoutes(admin, handlers.AdminRefund)
	orderhttp.RegisterAdminRefundWriteRoutes(admin, handlers.AdminRefund)
	return tokens
}
