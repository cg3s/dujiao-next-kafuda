package integrationtest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	kafudaapp "github.com/dujiao-next/internal/modules/kafudagpt/application"
	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
	kafudahttp "github.com/dujiao-next/internal/modules/kafudagpt/transport/http"
	"github.com/gin-gonic/gin"
)

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
