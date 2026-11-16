package integrationtest

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	kafudaapp "github.com/dujiao-next/internal/modules/kafudagpt/application"
	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
	kafudastore "github.com/dujiao-next/internal/modules/kafudagpt/infrastructure/gormstore"
)

func TestCatalogRemovalRevisionSurvivesConcurrentSyncAndRestart(t *testing.T) {
	db, store, _ := database(t)
	ctx, now := context.Background(), time.Now().UTC()
	initial := &kafudadomain.Catalog{Profile: "default", Enabled: true, SyncedAt: now, ExpiresAt: now.Add(time.Hour), Options: []kafudadomain.CatalogOption{{Plan: "plus", Country: "PH", Available: true}}}
	if err := store.SaveCatalog(ctx, initial); err != nil {
		t.Fatal(err)
	}
	removed := *initial
	removed.SyncedAt, removed.Options = now.Add(time.Second), []kafudadomain.CatalogOption{}
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			if err := store.SaveCatalog(ctx, &removed); err != nil {
				t.Error(err)
			}
		})
	}
	workers.Wait()
	store = kafudastore.New(db)
	current, err := store.GetCatalog(ctx, "default")
	if err != nil || current.Revision("plus", "PH") != 1 || len(current.Options) != 0 {
		t.Fatalf("removal history lost or counted twice: %+v, %v", current, err)
	}
	restored := *initial
	restored.SyncedAt = now.Add(2 * time.Second)
	if err := store.SaveCatalog(ctx, &restored); err != nil {
		t.Fatal(err)
	}
	// An old delayed response must not erase the revision or replace the catalog.
	if err := store.SaveCatalog(ctx, initial); err != nil {
		t.Fatal(err)
	}
	current, err = store.GetCatalog(ctx, "default")
	if err != nil || current.Revision("plus", "PH") != 1 || !current.Available("plus", "PH", now) || !current.SyncedAt.Equal(restored.SyncedAt) {
		t.Fatalf("restoration forgot prior removal: %+v, %v", current, err)
	}
	disabled := restored
	disabled.Enabled, disabled.SyncedAt = false, now.Add(3*time.Second)
	for range 2 {
		if err := store.SaveCatalog(ctx, &disabled); err != nil {
			t.Fatal(err)
		}
	}
	current, err = store.GetCatalog(ctx, "default")
	if err != nil || current.Revision("plus", "PH") != 2 || current.Enabled {
		t.Fatalf("global disable or duplicate sync mishandled: %+v, %v", current, err)
	}
}

type failingCatalogGateway struct {
	kafudacontract.Gateway
	data json.RawMessage
	err  error
}

func (g failingCatalogGateway) Catalog(context.Context) (json.RawMessage, error) {
	return g.data, g.err
}

func TestFailedCatalogSyncPreservesLastValidHistory(t *testing.T) {
	_, store, _ := database(t)
	ctx, now := context.Background(), time.Now().UTC()
	initial := &kafudadomain.Catalog{Profile: "default", Enabled: true, SyncedAt: now, ExpiresAt: now.Add(time.Hour), Options: []kafudadomain.CatalogOption{{Plan: "plus", Country: "PH", Available: true}}}
	if err := store.SaveCatalog(ctx, initial); err != nil {
		t.Fatal(err)
	}
	for _, gateway := range []failingCatalogGateway{{err: errors.New("upstream unavailable")}, {data: json.RawMessage(`{"enabled":true,"plans":null,"auto_card_products":[]}`)}} {
		service := kafudaapp.NewCatalogService(store, gateway, "default", time.Hour)
		if _, err := service.Sync(ctx); err == nil {
			t.Fatal("failed or invalid sync succeeded")
		}
		current, err := store.GetCatalog(ctx, "default")
		if err != nil || !current.SyncedAt.Equal(initial.SyncedAt) || !current.Available("plus", "PH", now) || current.Revision("plus", "PH") != 0 {
			t.Fatalf("failed sync overwrote valid catalog: %+v, %v", current, err)
		}
	}
}
