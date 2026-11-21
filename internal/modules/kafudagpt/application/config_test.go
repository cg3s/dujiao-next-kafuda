package application_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	kafudaapp "github.com/dujiao-next/internal/modules/kafudagpt/application"
	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
	"github.com/dujiao-next/internal/modules/kafudagpt/infrastructure/gormstore"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

type stubConfigGateway struct {
	kafudacontract.Gateway
	baseURL string
	apiKey  string
	data    json.RawMessage
	err     error
}

func (g *stubConfigGateway) Catalog(context.Context) (json.RawMessage, error) {
	if g.err != nil {
		return nil, g.err
	}
	if len(g.data) > 0 {
		return g.data, nil
	}
	return json.RawMessage(`{"enabled":true,"plans":[],"auto_card_products":[{"product_code":"CARD1","name":"Card 1","restriction_reason":null,"gpt_recommended":true,"open_fee_cents":100,"min_amount_cents":500}]}`), nil
}

type failingSettingStore struct {
	kafudacontract.SettingStore
	getErr  error
	saveErr error
}

func (s *failingSettingStore) GetSetting(ctx context.Context, profile string) (*kafudadomain.Setting, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	return s.SettingStore.GetSetting(ctx, profile)
}

func (s *failingSettingStore) SaveSetting(ctx context.Context, setting *kafudadomain.Setting) error {
	if s.saveErr != nil {
		return s.saveErr
	}
	return s.SettingStore.SaveSetting(ctx, setting)
}

func openSettingTestStore(t *testing.T) *gormstore.Store {
	t.Helper()
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
	if err := db.AutoMigrate(&kafudadomain.Setting{}, &kafudadomain.Catalog{}); err != nil {
		t.Fatal(err)
	}
	return gormstore.New(db)
}

func strPtr(v string) *string      { return &v }
func intPtr(v int) *int            { return &v }
func int64Ptr(v int64) *int64      { return &v }
func boolPtr(v bool) *bool         { return &v }
func strsPtr(v []string) *[]string { return &v }

func TestConfigServiceFallbackEncryptSaveAndCrossProcessReload(t *testing.T) {
	store := openSettingTestStore(t)
	ctx := context.Background()
	const appSecret = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	var applied []kafudaapp.ConfigSnapshot
	var mu sync.Mutex
	factory := func(baseURL, apiKey string) (kafudacontract.Gateway, error) {
		return &stubConfigGateway{baseURL: baseURL, apiKey: apiKey}, nil
	}

	fallback := kafudaapp.RuntimeConfig{
		APIBaseURL:             "https://kafuda.com",
		AllowedKinds:           []string{"new"},
		CatalogTTLSeconds:      3600,
		PollIntervalSeconds:    10,
		PollReviewAfterSeconds: 86400,
	}

	apiSvc := kafudaapp.NewConfigService(ctx, kafudaapp.ConfigServiceOptions{
		Store:          store,
		AppSecretKey:   appSecret,
		Fallback:       fallback,
		GatewayFactory: factory,
		OnApply: func(snap kafudaapp.ConfigSnapshot) {
			mu.Lock()
			applied = append(applied, snap)
			mu.Unlock()
		},
	})
	workerSvc := kafudaapp.NewConfigService(ctx, kafudaapp.ConfigServiceOptions{
		Store:               store,
		AppSecretKey:        appSecret,
		Fallback:            fallback,
		GatewayFactory:      factory,
		ReloadCheckInterval: time.Millisecond,
	})

	if snap := apiSvc.Snapshot(); snap.Source != kafudaapp.ConfigSourceConfigFile || snap.Configured {
		t.Fatalf("unexpected initial snapshot: %+v", snap)
	}

	const rawAPIKey = "kf_live_super_secret_merchant_key_123"
	updated, err := apiSvc.Update(ctx, kafudaapp.ConfigPatch{
		APIBaseURL:              strPtr("https://api.kafuda.example/"),
		APIKey:                  strPtr(rawAPIKey),
		ProductCode:             strPtr("XL537872"),
		MaxCostCents:            int64Ptr(2500),
		AllowedKinds:            strsPtr([]string{"NEW", "new", "  "}),
		SalesEnabled:            boolPtr(true),
		ReferenceLookupVerified: boolPtr(true),
		CatalogTTLSeconds:       intPtr(1800),
		PollIntervalSeconds:     intPtr(15),
	})
	if err != nil {
		t.Fatalf("Update failed: %v", err)
	}
	if !updated.Configured || updated.Source != kafudaapp.ConfigSourceDatabase || updated.Version != 1 {
		t.Fatalf("unexpected updated snapshot: %+v", updated)
	}
	if updated.Config.APIBaseURL != "https://api.kafuda.example" || updated.Config.APIKey != rawAPIKey {
		t.Fatalf("unexpected normalized config: %+v", updated.Config)
	}
	if len(updated.Config.AllowedKinds) != 1 || updated.Config.AllowedKinds[0] != "new" {
		t.Fatalf("expected deduplicated allowed_kinds, got %+v", updated.Config.AllowedKinds)
	}

	row, err := store.GetSetting(ctx, kafudadomain.DefaultProfile)
	if err != nil || row == nil {
		t.Fatalf("persisted row missing: %v", err)
	}
	if row.APIKeyEncrypted == "" || strings.Contains(row.APIKeyEncrypted, rawAPIKey) {
		t.Fatalf("API key was not encrypted in database")
	}

	// Empty APIKey patch must preserve existing encrypted key.
	preserved, err := apiSvc.Update(ctx, kafudaapp.ConfigPatch{
		APIKey:       strPtr(""),
		MaxCostCents: int64Ptr(3000),
	})
	if err != nil {
		t.Fatalf("Update preserving key failed: %v", err)
	}
	if preserved.Config.APIKey != rawAPIKey || preserved.Config.MaxCostCents != 3000 || preserved.Version != 2 {
		t.Fatalf("expected key preserved and version 2, got %+v", preserved)
	}

	// Separate worker instance picks up version 2 automatically on RefreshIfStale.
	time.Sleep(2 * time.Millisecond)
	workerSnap := workerSvc.RefreshIfStale(ctx)
	if !workerSnap.Configured || workerSnap.Version != 2 || workerSnap.Config.MaxCostCents != 3000 || workerSnap.Config.APIKey != rawAPIKey {
		t.Fatalf("worker did not reload updated DB setting: %+v", workerSnap)
	}

	// Explicit clear removes the key and disables gateway.
	cleared, err := apiSvc.Update(ctx, kafudaapp.ConfigPatch{ClearAPIKey: true})
	if err != nil {
		t.Fatalf("Update clearing key failed: %v", err)
	}
	if cleared.Configured || cleared.Config.APIKey != "" || cleared.Gateway != nil {
		t.Fatalf("expected cleared gateway, got %+v", cleared)
	}
	mu.Lock()
	applyCount := len(applied)
	mu.Unlock()
	if applyCount != 3 {
		t.Fatalf("expected 3 OnApply callbacks, got %d", applyCount)
	}
}

func TestConfigServiceValidationRejectsInvalidInputs(t *testing.T) {
	store := openSettingTestStore(t)
	ctx := context.Background()
	svc := kafudaapp.NewConfigService(ctx, kafudaapp.ConfigServiceOptions{
		Store:        store,
		AppSecretKey: "test-secret-key-0123456789abcdef",
		Fallback: kafudaapp.RuntimeConfig{
			APIBaseURL:             "https://kafuda.com",
			AllowedKinds:           []string{"new"},
			CatalogTTLSeconds:      3600,
			PollIntervalSeconds:    10,
			PollReviewAfterSeconds: 86400,
		},
	})

	cases := []struct {
		name    string
		patch   kafudaapp.ConfigPatch
		wantErr error
	}{
		{"http_url", kafudaapp.ConfigPatch{APIBaseURL: strPtr("http://kafuda.com")}, kafudaapp.ErrConfigBaseURLInvalid},
		{"url_with_path", kafudaapp.ConfigPatch{APIBaseURL: strPtr("https://kafuda.com/api")}, kafudaapp.ErrConfigBaseURLInvalid},
		{"url_with_query", kafudaapp.ConfigPatch{APIBaseURL: strPtr("https://kafuda.com?a=1")}, kafudaapp.ErrConfigBaseURLInvalid},
		{"url_with_userinfo", kafudaapp.ConfigPatch{APIBaseURL: strPtr("https://user@kafuda.com")}, kafudaapp.ErrConfigBaseURLInvalid},
		{"key_newline", kafudaapp.ConfigPatch{APIKey: strPtr("abc\ndef")}, kafudaapp.ErrConfigAPIKeyInvalid},
		{"negative_cost", kafudaapp.ConfigPatch{MaxCostCents: int64Ptr(-1)}, kafudaapp.ErrConfigInvalid},
		{"ttl_too_small", kafudaapp.ConfigPatch{CatalogTTLSeconds: intPtr(5)}, kafudaapp.ErrConfigInvalid},
		{"refresh_too_small", kafudaapp.ConfigPatch{CatalogRefreshIntervalSeconds: intPtr(3)}, kafudaapp.ErrConfigInvalid},
		{"poll_too_small", kafudaapp.ConfigPatch{PollIntervalSeconds: intPtr(2)}, kafudaapp.ErrConfigInvalid},
		{"poll_review_too_small", kafudaapp.ConfigPatch{PollReviewAfterSeconds: intPtr(30)}, kafudaapp.ErrConfigInvalid},
		{"bad_kind", kafudaapp.ConfigPatch{AllowedKinds: strsPtr([]string{"bad kind!"})}, kafudaapp.ErrConfigInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := svc.Update(ctx, tc.patch); !errors.Is(err, tc.wantErr) {
				t.Fatalf("Update(%s) err = %v, want %v", tc.name, err, tc.wantErr)
			}
		})
	}
}

func TestConfigServiceFailClosedOnLoadOrDecryptError(t *testing.T) {
	store := openSettingTestStore(t)
	ctx := context.Background()
	factory := func(baseURL, apiKey string) (kafudacontract.Gateway, error) {
		return &stubConfigGateway{baseURL: baseURL, apiKey: apiKey}, nil
	}
	yamlEnabled := kafudaapp.RuntimeConfig{
		APIBaseURL:              "https://kafuda.com",
		APIKey:                  "yaml-fallback-key",
		ProductCode:             "CARD1",
		MaxCostCents:            2000,
		AllowedKinds:            []string{"new"},
		SalesEnabled:            true,
		ReferenceLookupVerified: true,
		CatalogTTLSeconds:       3600,
		PollIntervalSeconds:     10,
		PollReviewAfterSeconds:  86400,
	}

	// Case 1: DB read failure at startup must fail closed and NOT resurrect YAML sales_enabled.
	brokenStore := &failingSettingStore{SettingStore: store, getErr: errors.New("db unavailable")}
	svcBroken := kafudaapp.NewConfigService(ctx, kafudaapp.ConfigServiceOptions{
		Store:          brokenStore,
		AppSecretKey:   "secret-a",
		Fallback:       yamlEnabled,
		GatewayFactory: factory,
	})
	if snap := svcBroken.Snapshot(); !snap.LoadError || !snap.GatewayConfigInvalid || snap.Configured || snap.Config.SalesEnabled {
		t.Fatalf("expected fail-closed snapshot on DB error, got %+v", snap)
	}

	// Case 2: Decryption failure (rotated app.secret_key) must fail closed, then allow admin recovery.
	writer := kafudaapp.NewConfigService(ctx, kafudaapp.ConfigServiceOptions{
		Store:          store,
		AppSecretKey:   "original-secret-key",
		Fallback:       yamlEnabled,
		GatewayFactory: factory,
	})
	if _, err := writer.Update(ctx, kafudaapp.ConfigPatch{
		APIKey:       strPtr("persisted-key"),
		SalesEnabled: boolPtr(true),
	}); err != nil {
		t.Fatal(err)
	}

	rotated := kafudaapp.NewConfigService(ctx, kafudaapp.ConfigServiceOptions{
		Store:          store,
		AppSecretKey:   "rotated-different-secret-key",
		Fallback:       yamlEnabled,
		GatewayFactory: factory,
	})
	if snap := rotated.Snapshot(); !snap.LoadError || !snap.GatewayConfigInvalid || snap.Configured || snap.Config.SalesEnabled {
		t.Fatalf("expected fail-closed snapshot on decrypt failure, got %+v", snap)
	}
	recovered, err := rotated.Update(ctx, kafudaapp.ConfigPatch{APIKey: strPtr("reentered-key")})
	if err != nil {
		t.Fatalf("expected admin recovery save to succeed: %v", err)
	}
	if !recovered.Configured || recovered.LoadError || recovered.Config.APIKey != "reentered-key" {
		t.Fatalf("expected recovered active config, got %+v", recovered)
	}
}

func TestConfigServiceTestConnectionUsesPatchOrSavedKeyWithoutSaving(t *testing.T) {
	store := openSettingTestStore(t)
	ctx := context.Background()
	var seenBaseURL, seenKey string
	factory := func(baseURL, apiKey string) (kafudacontract.Gateway, error) {
		seenBaseURL, seenKey = baseURL, apiKey
		return &stubConfigGateway{baseURL: baseURL, apiKey: apiKey}, nil
	}
	svc := kafudaapp.NewConfigService(ctx, kafudaapp.ConfigServiceOptions{
		Store:          store,
		AppSecretKey:   "test-secret",
		Fallback:       kafudaapp.RuntimeConfig{APIBaseURL: "https://kafuda.com", CatalogTTLSeconds: 3600, PollIntervalSeconds: 10, PollReviewAfterSeconds: 86400},
		GatewayFactory: factory,
	})

	if _, err := svc.TestConnection(ctx, nil); !errors.Is(err, kafudaapp.ErrConfigAPIKeyRequired) {
		t.Fatalf("expected ErrConfigAPIKeyRequired without key, got %v", err)
	}

	catalog, err := svc.TestConnection(ctx, &kafudaapp.ConfigPatch{
		APIBaseURL: strPtr("https://staging.kafuda.example"),
		APIKey:     strPtr("ephemeral-test-key"),
	})
	if err != nil {
		t.Fatalf("TestConnection with patch failed: %v", err)
	}
	if seenBaseURL != "https://staging.kafuda.example" || seenKey != "ephemeral-test-key" || len(catalog.Cards) != 1 {
		t.Fatalf("unexpected test result: base=%q key=%q cards=%d", seenBaseURL, seenKey, len(catalog.Cards))
	}
	if row, _ := store.GetSetting(ctx, kafudadomain.DefaultProfile); row != nil {
		t.Fatalf("TestConnection must not persist settings")
	}
}
