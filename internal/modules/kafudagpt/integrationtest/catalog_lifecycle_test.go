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

func TestCatalogProductChangeRequiresReapproval(t *testing.T) {
	db, store, _ := database(t)
	ctx, now := t.Context(), time.Now().UTC()
	const plan, country = "plus", "PH"
	initial := kafudadomain.Catalog{
		Profile: "default", Enabled: true, SyncedAt: now, ExpiresAt: now.Add(time.Hour),
		Options: []kafudadomain.CatalogOption{
			{Plan: plan, Country: country, Product: "gpt", Label: "Plus", Available: true},
			{Plan: "go", Country: country, Product: "gpt", Label: "Go", Available: true},
		},
		Cards: []kafudadomain.CardProduct{{ProductCode: "checkout-card"}},
	}
	if err := store.SaveCatalog(ctx, &initial); err != nil {
		t.Fatal(err)
	}
	commerce := kafudaapp.NewCommerceService(kafudaapp.CommerceOptions{
		Catalog:      kafudaapp.NewCatalogService(kafudastore.New(db), nil, initial.Profile, time.Hour),
		SalesEnabled: true, ReferenceLookupVerified: true, ProductCode: "checkout-card", MaxCostCents: 2500,
		AllowedKinds: []string{"new"}, WorkerReady: func(context.Context) bool { return true },
	})
	assertTerms := func(plan, product string, revision uint64) {
		t.Helper()
		terms, err := commerce.PurchaseTerms(ctx, plan, country, revision)
		if err != nil || terms == nil || terms.Product != product {
			t.Fatalf("purchase terms for %s revision %d = %+v, %v; want %s", plan, revision, terms, err, product)
		}
	}
	assertTerms(plan, "gpt", 0)
	for index, change := range []struct {
		product, label string
		revision       uint64
	}{
		{product: "gpt_credits", label: "Credits", revision: 1},
		{product: "gpt_credits", label: "Credits updated", revision: 1},
		{product: "gpt", label: "Plus", revision: 2},
	} {
		snapshot := initial
		snapshot.SyncedAt = now.Add(time.Duration(index+1) * time.Second)
		snapshot.Options = append([]kafudadomain.CatalogOption(nil), initial.Options...)
		snapshot.Options[0].Product, snapshot.Options[0].Label = change.product, change.label
		if err := store.SaveCatalog(ctx, &snapshot); err != nil {
			t.Fatal(err)
		}
		current, err := store.GetCatalog(ctx, initial.Profile)
		if err != nil || current == nil || current.Revision(plan, country) != change.revision || current.Revision("go", country) != 0 {
			t.Fatalf("refresh %d did not preserve the expected review revisions: %+v, %v", index, current, err)
		}
		if terms, err := commerce.PurchaseTerms(ctx, plan, country, change.revision-1); terms != nil || !errors.Is(err, kafudaapp.ErrQuoteUnavailable) {
			t.Fatalf("old approval issued terms after a product change: %+v, %v", terms, err)
		}
		assertTerms(plan, change.product, change.revision)
		assertTerms("go", "gpt", 0)
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

func TestScheduledCatalogRefreshKeepsReviewedSKUAvailable(t *testing.T) {
	_, store, _ := database(t)
	now := time.Now().UTC()
	const plan, country, card = "plus", "PH", "checkout-card"
	initial := &kafudadomain.Catalog{Profile: "default", Enabled: true, SyncedAt: now.Add(-time.Hour), ExpiresAt: now.Add(15 * time.Second),
		Options: []kafudadomain.CatalogOption{{Plan: plan, Country: country, Product: "gpt", Label: "Plus", Available: true}},
		Cards:   []kafudadomain.CardProduct{{ProductCode: card}},
	}
	if err := store.SaveCatalog(t.Context(), initial); err != nil {
		t.Fatal(err)
	}
	gateway := failingCatalogGateway{data: json.RawMessage(`{"enabled":true,"plans":[{"plan":"plus","label":"Plus","credit":false,"regions":[{"country":"PH","currency":"PHP","estimated_plan_usd_cents":1000,"estimated_local_minor":55000}]}],"auto_card_products":[{"product_code":"checkout-card","restriction_reason":null}]}`)}
	service := kafudaapp.NewCatalogService(store, gateway, initial.Profile, time.Hour)
	if err := service.RefreshBeforeExpiry(t.Context(), 10*time.Second); err != nil {
		t.Fatal(err)
	}
	fresh, err := service.Cached(t.Context())
	if err != nil || fresh == nil || !fresh.Available(plan, country, initial.ExpiresAt) ||
		fresh.Revision(plan, country) != initial.Revision(plan, country) {
		t.Fatalf("scheduled refresh did not preserve reviewed availability: %+v (%v)", fresh, err)
	}
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
