package consumer

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/dujiao-next/internal/app/container"
	kafudawiring "github.com/dujiao-next/internal/bootstrap/kafudagpt"
	"github.com/dujiao-next/internal/config"
	kafudaapp "github.com/dujiao-next/internal/modules/kafudagpt/application"
	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
	"github.com/dujiao-next/internal/queue"
	"github.com/hibiken/asynq"
)

type memoryCatalogStore struct {
	kafudacontract.Store
	catalog *kafudadomain.Catalog
}

func (s *memoryCatalogStore) GetCatalog(context.Context, string) (*kafudadomain.Catalog, error) {
	return s.catalog, nil
}

func (s *memoryCatalogStore) SaveCatalog(_ context.Context, catalog *kafudadomain.Catalog) error {
	s.catalog = catalog
	return nil
}

type countingCatalogGateway struct {
	kafudacontract.Gateway
	data  json.RawMessage
	calls int
}

func (g *countingCatalogGateway) Catalog(context.Context) (json.RawMessage, error) {
	g.calls++
	return g.data, nil
}

func TestHandleKafudaRefreshCatalogRunsWithoutSalesEnabled(t *testing.T) {
	payload := json.RawMessage(`{"enabled":true,"plans":[{"plan":"plus","label":"Plus","credit":false,"regions":[{"country":"PH","currency":"PHP","estimated_plan_usd_cents":1000,"estimated_local_minor":55000}]}],"auto_card_products":[{"product_code":"test-card","restriction_reason":null}]}`)

	t.Run("auto mode refreshes empty cache when sales_enabled is false", func(t *testing.T) {
		store := &memoryCatalogStore{}
		gateway := &countingCatalogGateway{data: payload}
		c := New(&container.Container{
			Config: &config.Config{KafudaGPT: config.KafudaGPTConfig{
				SalesEnabled:                  false,
				CatalogTTLSeconds:             3600,
				CatalogRefreshIntervalSeconds: 0,
			}},
			KafudaGPT: &kafudawiring.Module{
				Gateway: gateway,
				Catalog: kafudaapp.NewCatalogService(store, gateway, "default", time.Hour),
			},
		})
		mux := asynq.NewServeMux()
		c.Register(mux)
		if err := mux.ProcessTask(t.Context(), queue.NewKafudaRefreshCatalogTask()); err != nil {
			t.Fatalf("ProcessTask() error = %v", err)
		}
		if gateway.calls != 1 || store.catalog == nil || len(store.catalog.Options) != 1 {
			t.Fatalf("expected initial catalog sync, calls=%d catalog=%+v", gateway.calls, store.catalog)
		}
	})

	t.Run("fixed interval mode refreshes when synced_at is older than interval", func(t *testing.T) {
		now := time.Now().UTC()
		staleByInterval, err := kafudaapp.ParseCatalog(payload, "default", now.Add(-10*time.Minute), now.Add(50*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		store := &memoryCatalogStore{catalog: staleByInterval}
		gateway := &countingCatalogGateway{data: payload}
		c := New(&container.Container{
			Config: &config.Config{KafudaGPT: config.KafudaGPTConfig{
				SalesEnabled:                  false,
				CatalogTTLSeconds:             3600,
				CatalogRefreshIntervalSeconds: 300,
			}},
			KafudaGPT: &kafudawiring.Module{
				Gateway: gateway,
				Catalog: kafudaapp.NewCatalogService(store, gateway, "default", time.Hour),
			},
		})
		mux := asynq.NewServeMux()
		c.Register(mux)
		if err := mux.ProcessTask(t.Context(), queue.NewKafudaRefreshCatalogTask()); err != nil {
			t.Fatalf("ProcessTask() error = %v", err)
		}
		if gateway.calls != 1 || !store.catalog.SyncedAt.After(staleByInterval.SyncedAt) {
			t.Fatalf("expected fixed-interval sync, calls=%d synced_at=%v", gateway.calls, store.catalog.SyncedAt)
		}
	})

	t.Run("negative interval disables refresh", func(t *testing.T) {
		store := &memoryCatalogStore{}
		gateway := &countingCatalogGateway{data: payload}
		c := New(&container.Container{
			Config: &config.Config{KafudaGPT: config.KafudaGPTConfig{
				SalesEnabled:                  true,
				CatalogTTLSeconds:             3600,
				CatalogRefreshIntervalSeconds: -1,
			}},
			KafudaGPT: &kafudawiring.Module{
				Gateway: gateway,
				Catalog: kafudaapp.NewCatalogService(store, gateway, "default", time.Hour),
			},
		})
		mux := asynq.NewServeMux()
		c.Register(mux)
		if err := mux.ProcessTask(t.Context(), queue.NewKafudaRefreshCatalogTask()); err != nil {
			t.Fatalf("ProcessTask() error = %v", err)
		}
		if gateway.calls != 0 || store.catalog != nil {
			t.Fatalf("expected disabled refresh to skip sync, calls=%d catalog=%+v", gateway.calls, store.catalog)
		}
	})
}

func TestClassifyKafudaCatalogError(t *testing.T) {
	reason, status := classifyKafudaCatalogError(&kafudacontract.APIError{HTTPStatus: http.StatusTooManyRequests})
	if reason != "rate_limited" || status != http.StatusTooManyRequests {
		t.Fatalf("got (%q, %d), want (rate_limited, 429)", reason, status)
	}
	reason, status = classifyKafudaCatalogError(kafudadomain.ErrInvalidCatalog)
	if reason != "invalid_response" || status != 0 {
		t.Fatalf("got (%q, %d), want (invalid_response, 0)", reason, status)
	}
}
