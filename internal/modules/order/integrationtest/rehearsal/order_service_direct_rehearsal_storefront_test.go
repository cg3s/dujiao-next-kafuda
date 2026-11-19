package rehearsal_test

import (
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	catalogproductbootstrap "github.com/dujiao-next/internal/bootstrap/catalogproduct"
	"github.com/dujiao-next/internal/constants"
	categorystore "github.com/dujiao-next/internal/modules/catalog/category/infrastructure/gormstore"
	productapp "github.com/dujiao-next/internal/modules/catalog/product/application"
	producthttp "github.com/dujiao-next/internal/modules/catalog/product/transport/http"
	contentcontract "github.com/dujiao-next/internal/modules/content/contract"
	resellercontract "github.com/dujiao-next/internal/modules/reseller/contract"
	"github.com/dujiao-next/internal/platform/http/response"

	"github.com/gin-gonic/gin"
)

const rehearsalStorefrontPolicy = "default-src 'self'; connect-src 'self'; img-src 'self' data: blob:; style-src 'self' 'unsafe-inline'; font-src 'self' data:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'"

type rehearsalRelatedPosts struct{}

func (rehearsalRelatedPosts) ListPostsForProduct(context.Context, uint, int) ([]contentcontract.RelatedPost, error) {
	return nil, nil
}

func registerDirectRehearsalStorefront(t *testing.T, router *gin.Engine, f directCheckoutFixture, dir string) {
	t.Helper()
	if !filepath.IsAbs(dir) {
		t.Fatal("the rehearsal storefront directory must be absolute")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	assets := root.FS()
	indexInfo, err := fs.Stat(assets, "index.html")
	if err != nil || !indexInfo.Mode().IsRegular() {
		t.Fatalf("expected a built storefront index.html: %v", err)
	}
	index, err := fs.ReadFile(assets, "index.html")
	if err != nil {
		t.Fatal(err)
	}

	serveIndex := func(c *gin.Context) {
		// Keep the rehearsal same-origin even when an asset embeds an API URL.
		c.Header("Content-Security-Policy", rehearsalStorefrontPolicy)
		c.Data(http.StatusOK, "text/html; charset=utf-8", index)
	}
	for _, route := range []string{"/", "/products", "/products/:slug", "/checkout", "/pay", "/orders", "/orders/:order_no"} {
		router.GET(route, serveIndex)
	}
	files := http.FileServer(http.FS(assets))
	serveAsset := func(c *gin.Context) {
		name := strings.TrimPrefix(c.Request.URL.Path, "/")
		info, err := fs.Stat(assets, name)
		if err != nil || !info.Mode().IsRegular() {
			c.Status(http.StatusNotFound)
			return
		}
		files.ServeHTTP(c.Writer, c.Request)
	}
	router.GET("/assets/*asset", serveAsset)
	router.GET("/dj.svg", serveAsset)

	public := router.Group("/api/v1/public", func(c *gin.Context) {
		c.Request = c.Request.WithContext(resellercontract.WithTenantContext(c.Request.Context(), f.tenant))
	})
	public.GET("/config", func(c *gin.Context) {
		response.Success(c, gin.H{
			"site_name": "Direct rehearsal", "site_currency": "CNY", "storefront_template": "classic",
			"wallet_only_payment": true, "payment_channels": []any{},
		})
	})
	producthttp.RegisterPublicRoutes(public, catalogproductbootstrap.NewPublicHTTP(catalogproductbootstrap.PublicHTTPDependencies{
		Products: productapp.NewService(productapp.Options{
			Products: f.options.ProductStore, Categories: categorystore.NewCategoryStore(f.db),
		}),
		RelatedPosts: rehearsalRelatedPosts{}, Direct: f.commerce,
	}))
	router.NoRoute(func(c *gin.Context) {
		if c.Request.URL.Path == "/api" || strings.HasPrefix(c.Request.URL.Path, "/api/") {
			response.ErrorWithHTTPStatus(c, http.StatusNotFound, http.StatusNotFound, http.StatusText(http.StatusNotFound))
			return
		}
		c.Status(http.StatusNotFound)
	})
}

func TestDirectBuyerRehearsalStorefrontRoutes(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "assets"), 0o700); err != nil {
		t.Fatal(err)
	}
	for filename, content := range map[string]string{
		"index.html":    "<html>fixture storefront</html>",
		"assets/app.js": "export const test = true",
		"unserved.txt":  "file outside of assets",
	} {
		if err := os.WriteFile(filepath.Join(dir, filename), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	f, router, buyer, _ := newDirectRehearsalRouter(t, dir)
	request := func(path string, wantStatus int) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil).WithContext(t.Context())
		res := httptest.NewRecorder()
		router.ServeHTTP(res, req)
		if res.Code != wantStatus {
			t.Fatalf("%s returned HTTP %d, want %d: %s", path, res.Code, wantStatus, res.Body.String())
		}
		return res
	}

	for _, path := range []string{"/", "/products/" + f.product.Slug, "/checkout", "/pay", "/orders/test-order"} {
		res := request(path, http.StatusOK)
		if res.Body.String() != "<html>fixture storefront</html>" || res.Header().Get("Content-Security-Policy") != rehearsalStorefrontPolicy {
			t.Fatalf("%s did not serve the same-origin storefront: %s", path, res.Body.String())
		}
	}
	if res := request("/assets/app.js", http.StatusOK); res.Body.String() != "export const test = true" {
		t.Fatalf("asset body = %q", res.Body.String())
	}
	request("/assets/missing.js", http.StatusNotFound)
	request("/assets/", http.StatusNotFound)
	request("/unserved.txt", http.StatusNotFound)
	missing := request("/api/v1/rehearsal/not-found", http.StatusNotFound)
	var missingEnvelope response.Response
	if err := json.Unmarshal(missing.Body.Bytes(), &missingEnvelope); err != nil || missingEnvelope.StatusCode != http.StatusNotFound {
		t.Fatalf("missing API must return a JSON 404: %s (%v)", missing.Body.String(), err)
	}
	config := request("/api/v1/public/config", http.StatusOK)
	if !strings.Contains(config.Body.String(), `"wallet_only_payment":true`) {
		t.Fatalf("expected wallet-only config: %s", config.Body.String())
	}
	product := request("/api/v1/public/products/"+f.product.Slug, http.StatusOK)
	var item struct {
		StatusCode int `json:"status_code"`
		Data       struct {
			ID              uint   `json:"id"`
			FulfillmentType string `json:"fulfillment_type"`
			IsSoldOut       bool   `json:"is_sold_out"`
		} `json:"data"`
	}
	if err := json.Unmarshal(product.Body.Bytes(), &item); err != nil || item.StatusCode != 0 || item.Data.ID != f.product.ID ||
		item.Data.FulfillmentType != constants.FulfillmentTypeKafudaGPT || item.Data.IsSoldOut {
		t.Fatalf("real catalog route did not expose a purchasable fixture: %s (%v)", product.Body.String(), err)
	}
	console := request("/rehearsal", http.StatusOK)
	if !strings.Contains(console.Body.String(), buyer) || !strings.Contains(console.Body.String(), f.product.Slug) {
		t.Fatal("console did not include the synthetic storefront launcher")
	}
}
