package consumer

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/dujiao-next/internal/app/container"
	quefawiring "github.com/dujiao-next/internal/bootstrap/quefa"
	"github.com/dujiao-next/internal/config"
	quefaapp "github.com/dujiao-next/internal/modules/quefa/application"
	quefacontract "github.com/dujiao-next/internal/modules/quefa/contract"
	quefadomain "github.com/dujiao-next/internal/modules/quefa/domain"
	"github.com/dujiao-next/internal/modules/quefa/infrastructure/gormstore"
	"github.com/dujiao-next/internal/queue"
	"github.com/glebarez/sqlite"
	"github.com/hibiken/asynq"
	"gorm.io/gorm"
)

type countingQuefaGateway struct {
	quefacontract.Gateway
	data  json.RawMessage
	calls int
}

func (g *countingQuefaGateway) GetProducts(context.Context) (json.RawMessage, quefacontract.PollHints, error) {
	g.calls++
	return g.data, quefacontract.PollHints{}, nil
}

func newTestQuefaStore(t *testing.T) *gormstore.Store {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	if err := db.AutoMigrate(&quefadomain.Setting{}, &quefadomain.Catalog{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	return gormstore.New(db)
}

func TestHandleQuefaRefreshCatalogRunsWithoutSalesEnabled(t *testing.T) {
	payload := json.RawMessage(`{"products":[{"product_code":"chatgpt_plus_1m","title":"Plus 1M","supply_price":"100.00","min_sale_price":"110.00","currency":"CNY","collection_modes":["platform_collect"],"delivery_modes":["auto_recharge"],"available":true}]}`)

	t.Run("auto mode refreshes empty cache when sales_enabled is false", func(t *testing.T) {
		store := newTestQuefaStore(t)
		gateway := &countingQuefaGateway{data: payload}
		cfgSvc := quefaapp.NewConfigService(t.Context(), quefaapp.ConfigServiceOptions{
			Store:        store,
			Profile:      quefadomain.DefaultProfile,
			AppSecretKey: "test-app-secret-key-32-bytes-ok!",
			Fallback: quefaapp.RuntimeConfig{
				APIBaseURL:                    "https://tibo.ink",
				PartnerID:                     "p1",
				KeyID:                         "k1",
				ClientSecret:                  "sec1",
				SalesEnabled:                  false,
				CatalogTTLSeconds:             3600,
				CatalogRefreshIntervalSeconds: 0,
			},
			GatewayFactory: func(string, string, string, string) (quefacontract.Gateway, error) {
				return gateway, nil
			},
		})
		c := New(&container.Container{
			Config: &config.Config{Quefa: config.QuefaConfig{
				SalesEnabled:                  false,
				CatalogTTLSeconds:             3600,
				CatalogRefreshIntervalSeconds: 0,
			}},
			Quefa: &quefawiring.Module{
				Store:   store,
				Config:  cfgSvc,
				Catalog: quefaapp.NewCatalogService(store, cfgSvc, quefadomain.DefaultProfile, nil),
			},
		})
		mux := asynq.NewServeMux()
		c.Register(mux)
		if err := mux.ProcessTask(t.Context(), queue.NewQuefaRefreshCatalogTask()); err != nil {
			t.Fatalf("ProcessTask() error = %v", err)
		}
		cat, err := store.GetCatalog(t.Context(), quefadomain.DefaultProfile)
		if err != nil || gateway.calls != 1 || cat == nil || cat.SyncedAt == nil {
			t.Fatalf("expected initial catalog sync, calls=%d cat=%+v err=%v", gateway.calls, cat, err)
		}
	})

	t.Run("fixed interval mode refreshes when synced_at is older than interval", func(t *testing.T) {
		store := newTestQuefaStore(t)
		oldTime := time.Now().UTC().Add(-10 * time.Minute)
		products, err := quefadomain.ParseCatalogPayload(payload)
		if err != nil {
			t.Fatal(err)
		}
		parsedJSON, _ := json.Marshal(products)
		if err := store.SaveCatalog(&quefadomain.Catalog{
			Profile:    quefadomain.DefaultProfile,
			RawPayload: string(payload),
			ParsedJSON: string(parsedJSON),
			SyncedAt:   &oldTime,
			Revision:   1,
			UpdatedAt:  oldTime,
		}); err != nil {
			t.Fatal(err)
		}
		gateway := &countingQuefaGateway{data: payload}
		cfgSvc := quefaapp.NewConfigService(t.Context(), quefaapp.ConfigServiceOptions{
			Store:        store,
			Profile:      quefadomain.DefaultProfile,
			AppSecretKey: "test-app-secret-key-32-bytes-ok!",
			Fallback: quefaapp.RuntimeConfig{
				APIBaseURL:                    "https://tibo.ink",
				PartnerID:                     "p1",
				KeyID:                         "k1",
				ClientSecret:                  "sec1",
				SalesEnabled:                  false,
				CatalogTTLSeconds:             3600,
				CatalogRefreshIntervalSeconds: 300,
			},
			GatewayFactory: func(string, string, string, string) (quefacontract.Gateway, error) {
				return gateway, nil
			},
		})
		c := New(&container.Container{
			Config: &config.Config{Quefa: config.QuefaConfig{
				SalesEnabled:                  false,
				CatalogTTLSeconds:             3600,
				CatalogRefreshIntervalSeconds: 300,
			}},
			Quefa: &quefawiring.Module{
				Store:   store,
				Config:  cfgSvc,
				Catalog: quefaapp.NewCatalogService(store, cfgSvc, quefadomain.DefaultProfile, nil),
			},
		})
		mux := asynq.NewServeMux()
		c.Register(mux)
		if err := mux.ProcessTask(t.Context(), queue.NewQuefaRefreshCatalogTask()); err != nil {
			t.Fatalf("ProcessTask() error = %v", err)
		}
		cat, err := store.GetCatalog(t.Context(), quefadomain.DefaultProfile)
		if err != nil || gateway.calls != 1 || cat == nil || cat.SyncedAt == nil || !cat.SyncedAt.After(oldTime) {
			t.Fatalf("expected fixed-interval sync, calls=%d cat=%+v err=%v", gateway.calls, cat, err)
		}
	})

	t.Run("negative interval disables refresh", func(t *testing.T) {
		store := newTestQuefaStore(t)
		gateway := &countingQuefaGateway{data: payload}
		cfgSvc := quefaapp.NewConfigService(t.Context(), quefaapp.ConfigServiceOptions{
			Store:        store,
			Profile:      quefadomain.DefaultProfile,
			AppSecretKey: "test-app-secret-key-32-bytes-ok!",
			Fallback: quefaapp.RuntimeConfig{
				APIBaseURL:                    "https://tibo.ink",
				PartnerID:                     "p1",
				KeyID:                         "k1",
				ClientSecret:                  "sec1",
				SalesEnabled:                  true,
				CatalogTTLSeconds:             3600,
				CatalogRefreshIntervalSeconds: -1,
			},
			GatewayFactory: func(string, string, string, string) (quefacontract.Gateway, error) {
				return gateway, nil
			},
		})
		c := New(&container.Container{
			Config: &config.Config{Quefa: config.QuefaConfig{
				SalesEnabled:                  true,
				CatalogTTLSeconds:             3600,
				CatalogRefreshIntervalSeconds: -1,
			}},
			Quefa: &quefawiring.Module{
				Store:   store,
				Config:  cfgSvc,
				Catalog: quefaapp.NewCatalogService(store, cfgSvc, quefadomain.DefaultProfile, nil),
			},
		})
		mux := asynq.NewServeMux()
		c.Register(mux)
		if err := mux.ProcessTask(t.Context(), queue.NewQuefaRefreshCatalogTask()); err != nil {
			t.Fatalf("ProcessTask() error = %v", err)
		}
		cat, _ := store.GetCatalog(t.Context(), quefadomain.DefaultProfile)
		if gateway.calls != 0 || cat != nil {
			t.Fatalf("expected disabled refresh to skip sync, calls=%d cat=%+v", gateway.calls, cat)
		}
	})
}

func TestClassifyQuefaCatalogError(t *testing.T) {
	reason, status := classifyQuefaCatalogError(&quefacontract.APIError{StatusCode: http.StatusTooManyRequests, Category: "rate_limited"})
	if reason != "rate_limited" || status != http.StatusTooManyRequests {
		t.Fatalf("got (%q, %d), want (rate_limited, 429)", reason, status)
	}
	reason, status = classifyQuefaCatalogError(&quefaapp.CatalogParseError{Err: quefadomain.ErrCatalogInvalid, Shape: "empty"})
	if reason != "invalid_response" || status != 0 {
		t.Fatalf("got (%q, %d), want (invalid_response, 0)", reason, status)
	}
}
