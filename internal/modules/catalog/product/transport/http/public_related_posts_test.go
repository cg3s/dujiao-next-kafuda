package producthttp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dujiao-next/internal/constants"
	productdomain "github.com/dujiao-next/internal/modules/catalog/product/domain"
	productpresenter "github.com/dujiao-next/internal/modules/catalog/product/transport/presenter"
	contentcontract "github.com/dujiao-next/internal/modules/content/contract"
	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	reseller "github.com/dujiao-next/internal/modules/reseller/contract"
	"github.com/dujiao-next/internal/shared/jsonmap"
	"github.com/dujiao-next/internal/shared/money"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
)

type relatedPostReaderStub struct {
	receivedContext context.Context
	productID       uint
	limit           int
	posts           []contentcontract.RelatedPost
	err             error
}

var _ RelatedPostReader = (*relatedPostReaderStub)(nil)

func (s *relatedPostReaderStub) ListPostsForProduct(ctx context.Context, productID uint, limit int) ([]contentcontract.RelatedPost, error) {
	s.receivedContext = ctx
	s.productID = productID
	s.limit = limit
	return s.posts, s.err
}

func TestLoadRelatedPostCardsUsesConsumerOwnedReaderAndRequestContext(t *testing.T) {
	t.Parallel()

	type contextKey struct{}
	requestContext := context.WithValue(context.Background(), contextKey{}, "product-request")
	reader := &relatedPostReaderStub{posts: []contentcontract.RelatedPost{{Slug: "guide"}}}
	handler := &PublicHandler{relatedPosts: reader}

	cards, err := handler.loadRelatedPostCards(requestContext, 42)
	if err != nil {
		t.Fatalf("loadRelatedPostCards() error = %v", err)
	}
	if reader.receivedContext.Value(contextKey{}) != "product-request" {
		t.Fatal("request context was not propagated")
	}
	if reader.productID != 42 || reader.limit != publicRelatedPostsLimit {
		t.Fatalf("reader arguments = (%d, %d)", reader.productID, reader.limit)
	}
	if len(cards) != 1 || cards[0].Slug != "guide" {
		t.Fatalf("cards mismatch: %#v", cards)
	}
}

func TestLoadRelatedPostCardsReturnsReaderErrorForBestEffortCaller(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("content unavailable")
	handler := &PublicHandler{relatedPosts: &relatedPostReaderStub{err: wantErr}}
	if _, err := handler.loadRelatedPostCards(context.Background(), 42); !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want %v", err, wantErr)
	}
}

type stubPublicProductQueries struct {
	products []productdomain.Product
	bySlug   map[string]productdomain.Product
}

func (s *stubPublicProductQueries) ListPublicForTenant(reseller.TenantContext, string, string, int, int) ([]productdomain.Product, int64, error) {
	out := make([]productdomain.Product, len(s.products))
	copy(out, s.products)
	return out, int64(len(out)), nil
}

func (s *stubPublicProductQueries) GetPublicBySlugForTenant(_ reseller.TenantContext, slug string) (*productdomain.Product, error) {
	p := s.bySlug[slug]
	return &p, nil
}

func (s *stubPublicProductQueries) ApplyAutoStockCounts([]productdomain.Product) error {
	return nil
}

type stubDirectAvailability struct {
	events       []string
	syncErr      error
	revisions    map[string]uint64
	afterSyncRev map[string]uint64
	syncCalls    int
}

func (s *stubDirectAvailability) SyncCatalog(ctx context.Context) error {
	s.syncCalls++
	s.events = append(s.events, "sync")
	if _, ok := ctx.Deadline(); !ok {
		return errors.New("expected bounded sync context deadline")
	}
	if s.syncErr != nil {
		return s.syncErr
	}
	if s.afterSyncRev != nil {
		s.revisions = s.afterSyncRev
	}
	return nil
}

func (s *stubDirectAvailability) PurchasableSKURevisions(context.Context) (map[string]uint64, error) {
	s.events = append(s.events, "revisions")
	return s.revisions, nil
}

func TestGetProductBySlugSyncsDirectCatalogBeforeReadingRevisions(t *testing.T) {
	gin.SetMode(gin.TestMode)

	skuCode := kafudacontract.SKUCode("plus", "PH")
	directProduct := productdomain.Product{
		ID:              10,
		Slug:            "gpt-plus",
		FulfillmentType: constants.FulfillmentTypeKafudaGPT,
		IsActive:        true,
		PriceAmount:     money.FromDecimal(decimal.NewFromInt(100)),
		SKUs: []productdomain.ProductSKU{
			{
				ID:                    101,
				ProductID:             10,
				SKUCode:               skuCode,
				IsActive:              true,
				PriceAmount:           money.FromDecimal(decimal.NewFromInt(100)),
				CostPriceAmount:       money.FromDecimal(decimal.NewFromInt(80)),
				DirectCostConfigured:  true,
				SpecValuesJSON:        jsonmap.JSON{"plan": "plus", "country": "PH"},
				DirectCatalogRevision: 2,
			},
		},
	}
	normalProduct := productdomain.Product{
		ID:              11,
		Slug:            "normal-auto",
		FulfillmentType: constants.FulfillmentTypeAuto,
		IsActive:        true,
		PriceAmount:     money.FromDecimal(decimal.NewFromInt(50)),
	}

	direct := &stubDirectAvailability{
		revisions:    map[string]uint64{skuCode: 1},
		afterSyncRev: map[string]uint64{skuCode: 2},
	}
	handler := NewPublicHandler(
		&stubPublicProductQueries{
			products: []productdomain.Product{directProduct, normalProduct},
			bySlug: map[string]productdomain.Product{
				directProduct.Slug: directProduct,
				normalProduct.Slug: normalProduct,
			},
		},
		nil, nil, nil, nil, nil,
		&relatedPostReaderStub{},
		direct,
	)

	router := gin.New()
	RegisterPublicRoutes(router.Group("/api/v1/public"), handler)

	// 1. Visiting a kafuda_gpt_direct product syncs upstream catalog before reading revisions.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/public/products/gpt-plus", nil)
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET direct product HTTP %d: %s", rec.Code, rec.Body.String())
	}
	var detailEnvelope struct {
		StatusCode int                      `json:"status_code"`
		Data       productpresenter.Product `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detailEnvelope); err != nil || detailEnvelope.StatusCode != 0 {
		t.Fatalf("decode direct product response: %v, body=%s", err, rec.Body.String())
	}
	if direct.syncCalls != 1 || len(direct.events) != 2 || direct.events[0] != "sync" || direct.events[1] != "revisions" {
		t.Fatalf("unexpected direct call sequence: calls=%d events=%v", direct.syncCalls, direct.events)
	}
	if detailEnvelope.Data.IsSoldOut {
		t.Fatalf("expected direct product to be in stock after on-visit sync, got sold out")
	}

	// 2. Sync failure degrades gracefully and still returns HTTP 200 using cached revisions.
	direct.events = nil
	direct.syncErr = errors.New("upstream timeout")
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/public/products/gpt-plus", nil)
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET direct product on sync failure HTTP %d: %s", rec.Code, rec.Body.String())
	}
	if direct.syncCalls != 2 {
		t.Fatalf("expected second sync attempt, got %d", direct.syncCalls)
	}

	// 3. Visiting a non-direct product does not trigger catalog sync.
	direct.syncErr = nil
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/public/products/normal-auto", nil)
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET normal product HTTP %d: %s", rec.Code, rec.Body.String())
	}
	if direct.syncCalls != 2 {
		t.Fatalf("non-direct product visit should not trigger sync, got %d calls", direct.syncCalls)
	}

	// 4. Listing products via GetProducts does not trigger catalog sync.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/public/products", nil)
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET product list HTTP %d: %s", rec.Code, rec.Body.String())
	}
	if direct.syncCalls != 2 {
		t.Fatalf("product list should not trigger sync, got %d calls", direct.syncCalls)
	}

	// 5. Reseller tenant visiting a direct product does not trigger catalog sync.
	rec = httptest.NewRecorder()
	resellerTenant := reseller.ResellerTenantContext("shop.example.test", 10, 99, "shop.example.test")
	req = httptest.NewRequest(http.MethodGet, "/api/v1/public/products/gpt-plus", nil).
		WithContext(reseller.WithTenantContext(context.Background(), resellerTenant))
	router.ServeHTTP(rec, req)
	if direct.syncCalls != 2 {
		t.Fatalf("reseller tenant visit should not trigger sync, got %d calls", direct.syncCalls)
	}
}
