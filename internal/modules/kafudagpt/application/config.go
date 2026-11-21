package application

import (
	"context"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/dujiao-next/internal/crypto"
	"github.com/dujiao-next/internal/logger"
	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
)

const (
	ConfigSourceConfigFile = "config_file"
	ConfigSourceDatabase   = "database"
	ConfigSourceError      = "error"

	DefaultAPIBaseURL                    = "https://kafuda.com"
	DefaultCatalogTTLSeconds             = 3600
	DefaultCatalogRefreshIntervalSeconds = 0
	DefaultPollReviewAfterSeconds        = 86400
	DefaultPollIntervalSeconds           = 10

	defaultConfigReloadCheckInterval = 5 * time.Second
)

var (
	ErrConfigInvalid          = errors.New("kafuda config invalid")
	ErrConfigBaseURLInvalid   = errors.New("kafuda api base url must be an HTTPS origin")
	ErrConfigAPIKeyInvalid    = errors.New("kafuda api key invalid")
	ErrConfigAPIKeyRequired   = errors.New("kafuda api key required")
	ErrConfigStoreUnavailable = errors.New("kafuda config store unavailable")
	ErrConfigLoadFailed       = errors.New("kafuda config load failed")
	ErrConfigSaveFailed       = errors.New("kafuda config save failed")

	safeKindPattern = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)
)

// GatewayFactory constructs a supplier API gateway from a validated base URL and API key.
type GatewayFactory func(baseURL, apiKey string) (kafudacontract.Gateway, error)

// RuntimeConfig holds the effective runtime parameters for Kafuda GPT direct recharge.
type RuntimeConfig struct {
	APIBaseURL                    string
	APIKey                        string
	ProductCode                   string
	MaxCostCents                  int64
	AllowedKinds                  []string
	SalesEnabled                  bool
	ReferenceLookupVerified       bool
	CatalogTTLSeconds             int
	CatalogRefreshIntervalSeconds int
	PollReviewAfterSeconds        int
	PollIntervalSeconds           int
}

// ConfigPatch represents an admin update payload for Kafuda GPT configuration.
type ConfigPatch struct {
	APIBaseURL                    *string   `json:"api_base_url"`
	APIKey                        *string   `json:"api_key"`
	ClearAPIKey                   bool      `json:"clear_api_key"`
	ProductCode                   *string   `json:"product_code"`
	MaxCostCents                  *int64    `json:"max_cost_cents"`
	AllowedKinds                  *[]string `json:"allowed_kinds"`
	SalesEnabled                  *bool     `json:"sales_enabled"`
	ReferenceLookupVerified       *bool     `json:"reference_lookup_verified"`
	CatalogTTLSeconds             *int      `json:"catalog_ttl_seconds"`
	CatalogRefreshIntervalSeconds *int      `json:"catalog_refresh_interval_seconds"`
	PollReviewAfterSeconds        *int      `json:"poll_review_after_seconds"`
	PollIntervalSeconds           *int      `json:"poll_interval_seconds"`
}

// ConfigSnapshot is an immutable point-in-time view of Kafuda runtime state.
type ConfigSnapshot struct {
	Config               RuntimeConfig
	Source               string
	Version              int64
	UpdatedAt            time.Time
	Gateway              kafudacontract.Gateway
	Configured           bool
	GatewayConfigInvalid bool
	LoadError            bool
}

// Clone returns a copy with an independent AllowedKinds slice.
func (c RuntimeConfig) Clone() RuntimeConfig {
	out := c
	if c.AllowedKinds != nil {
		out.AllowedKinds = append([]string(nil), c.AllowedKinds...)
	}
	return out
}

// Normalize trims string inputs and deduplicates allowed kinds.
func (c RuntimeConfig) Normalize() RuntimeConfig {
	out := c.Clone()
	out.APIBaseURL = strings.TrimRight(strings.TrimSpace(out.APIBaseURL), "/")
	if out.APIBaseURL == "" {
		out.APIBaseURL = DefaultAPIBaseURL
	}
	out.APIKey = strings.TrimSpace(out.APIKey)
	out.ProductCode = strings.TrimSpace(out.ProductCode)
	seen := make(map[string]struct{}, len(out.AllowedKinds))
	kinds := make([]string, 0, len(out.AllowedKinds))
	for _, raw := range out.AllowedKinds {
		kind := strings.ToLower(strings.TrimSpace(raw))
		if kind == "" {
			continue
		}
		if _, dup := seen[kind]; dup {
			continue
		}
		seen[kind] = struct{}{}
		kinds = append(kinds, kind)
	}
	out.AllowedKinds = kinds
	return out
}

// Validate checks admin-edited configuration values before persistence.
func (c RuntimeConfig) Validate() error {
	if err := validateHTTPSOrigin(c.APIBaseURL); err != nil {
		return err
	}
	if strings.ContainsAny(c.APIKey, "\r\n") {
		return ErrConfigAPIKeyInvalid
	}
	if len(c.ProductCode) > 128 || strings.ContainsAny(c.ProductCode, "\r\n") {
		return ErrConfigInvalid
	}
	if c.MaxCostCents < 0 || c.MaxCostCents > 100_000_000 {
		return ErrConfigInvalid
	}
	for _, kind := range c.AllowedKinds {
		if !safeKindPattern.MatchString(kind) {
			return ErrConfigInvalid
		}
	}
	if c.CatalogTTLSeconds < 10 || c.CatalogTTLSeconds > 86400 {
		return ErrConfigInvalid
	}
	if c.CatalogRefreshIntervalSeconds < -1 || (c.CatalogRefreshIntervalSeconds > 0 && (c.CatalogRefreshIntervalSeconds < 5 || c.CatalogRefreshIntervalSeconds > 86400)) {
		return ErrConfigInvalid
	}
	if c.PollIntervalSeconds < 5 || c.PollIntervalSeconds > 900 {
		return ErrConfigInvalid
	}
	if c.PollReviewAfterSeconds < 60 || c.PollReviewAfterSeconds > 604800 {
		return ErrConfigInvalid
	}
	return nil
}

func validateHTTPSOrigin(raw string) error {
	trimmed := strings.TrimSpace(raw)
	u, err := url.Parse(trimmed)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return ErrConfigBaseURLInvalid
	}
	return nil
}

// ApplyPatch merges non-nil fields from patch onto a copy of c.
func (c RuntimeConfig) ApplyPatch(patch ConfigPatch) RuntimeConfig {
	next := c.Clone()
	if patch.APIBaseURL != nil {
		next.APIBaseURL = strings.TrimSpace(*patch.APIBaseURL)
	}
	if patch.ClearAPIKey {
		next.APIKey = ""
	} else if patch.APIKey != nil {
		if v := strings.TrimSpace(*patch.APIKey); v != "" {
			next.APIKey = v
		}
	}
	if patch.ProductCode != nil {
		next.ProductCode = strings.TrimSpace(*patch.ProductCode)
	}
	if patch.MaxCostCents != nil {
		next.MaxCostCents = *patch.MaxCostCents
	}
	if patch.AllowedKinds != nil {
		next.AllowedKinds = append([]string(nil), (*patch.AllowedKinds)...)
	}
	if patch.SalesEnabled != nil {
		next.SalesEnabled = *patch.SalesEnabled
	}
	if patch.ReferenceLookupVerified != nil {
		next.ReferenceLookupVerified = *patch.ReferenceLookupVerified
	}
	if patch.CatalogTTLSeconds != nil {
		next.CatalogTTLSeconds = *patch.CatalogTTLSeconds
	}
	if patch.CatalogRefreshIntervalSeconds != nil {
		next.CatalogRefreshIntervalSeconds = *patch.CatalogRefreshIntervalSeconds
	}
	if patch.PollReviewAfterSeconds != nil {
		next.PollReviewAfterSeconds = *patch.PollReviewAfterSeconds
	}
	if patch.PollIntervalSeconds != nil {
		next.PollIntervalSeconds = *patch.PollIntervalSeconds
	}
	return next
}

// CatalogTTL returns the configured catalog cache duration.
func (c RuntimeConfig) CatalogTTL() time.Duration {
	if c.CatalogTTLSeconds <= 0 {
		return 0
	}
	return time.Duration(c.CatalogTTLSeconds) * time.Second
}

// PollInterval returns the clamped normal polling interval (matching config.Config.KafudaPollInterval).
func (c RuntimeConfig) PollInterval() time.Duration {
	if c.PollIntervalSeconds <= 0 {
		return time.Minute
	}
	seconds := max(5, min(900, c.PollIntervalSeconds))
	return time.Duration(seconds) * time.Second
}

// PollReviewAfter returns the duration after which an unresolved known order moves to manual review.
func (c RuntimeConfig) PollReviewAfter() time.Duration {
	if c.PollReviewAfterSeconds <= 0 {
		return DefaultPollReviewAfter
	}
	if int64(c.PollReviewAfterSeconds) > int64((1<<63-1)/time.Second) {
		return DefaultPollReviewAfter
	}
	return time.Duration(c.PollReviewAfterSeconds) * time.Second
}

// CatalogRefreshInterval computes the background catalog refresh cadence (matching config.Config.KafudaCatalogRefreshInterval).
func (c RuntimeConfig) CatalogRefreshInterval() time.Duration {
	if c.CatalogTTLSeconds <= 0 || c.CatalogRefreshIntervalSeconds < 0 {
		return 0
	}
	if c.CatalogRefreshIntervalSeconds > 0 {
		seconds := max(5, min(86400, c.CatalogRefreshIntervalSeconds))
		return time.Duration(seconds) * time.Second
	}
	seconds := max(1, min(60, c.CatalogTTLSeconds/4))
	return time.Duration(seconds) * time.Second
}

// CatalogFixedRefresh reports whether catalog refresh runs on an explicit fixed schedule.
func (c RuntimeConfig) CatalogFixedRefresh() bool {
	return c.CatalogTTLSeconds > 0 && c.CatalogRefreshIntervalSeconds > 0
}

// ConfigServiceOptions configures ConfigService.
type ConfigServiceOptions struct {
	Store               kafudacontract.SettingStore
	Profile             string
	AppSecretKey        string
	Fallback            RuntimeConfig
	GatewayFactory      GatewayFactory
	OnApply             func(ConfigSnapshot)
	ReloadCheckInterval time.Duration
}

// ConfigService manages persisted encrypted Kafuda configuration and runtime hot-reload.
type ConfigService struct {
	store               kafudacontract.SettingStore
	profile             string
	encKey              []byte
	fallback            RuntimeConfig
	gatewayFactory      GatewayFactory
	onApply             func(ConfigSnapshot)
	reloadCheckInterval time.Duration

	mu        sync.RWMutex
	snapshot  ConfigSnapshot
	lastCheck time.Time
}

// NewConfigService loads the initial snapshot from DB (falling back to config.yml when no DB row exists).
// If DB read or API key decryption fails, it fails closed with sales disabled and no gateway.
func NewConfigService(ctx context.Context, opts ConfigServiceOptions) *ConfigService {
	profile := strings.TrimSpace(opts.Profile)
	if profile == "" {
		profile = kafudadomain.DefaultProfile
	}
	interval := opts.ReloadCheckInterval
	if interval <= 0 {
		interval = defaultConfigReloadCheckInterval
	}
	svc := &ConfigService{
		store:               opts.Store,
		profile:             profile,
		encKey:              crypto.DeriveKey(opts.AppSecretKey),
		fallback:            opts.Fallback.Clone(),
		gatewayFactory:      opts.GatewayFactory,
		onApply:             opts.OnApply,
		reloadCheckInterval: interval,
	}
	svc.snapshot = svc.loadInitial(ctx)
	svc.lastCheck = time.Now()
	return svc
}

// SetOnApply registers the runtime apply hook invoked whenever configuration changes.
func (s *ConfigService) SetOnApply(fn func(ConfigSnapshot)) {
	s.mu.Lock()
	s.onApply = fn
	s.mu.Unlock()
}

// Snapshot returns the current in-memory configuration snapshot without database I/O.
func (s *ConfigService) Snapshot() ConfigSnapshot {
	if s == nil {
		return ConfigSnapshot{}
	}
	s.mu.RLock()
	snap := s.snapshot
	s.mu.RUnlock()
	snap.Config = snap.Config.Clone()
	return snap
}

// RefreshIfStale polls the database at most once per reloadCheckInterval so separate
// API and worker processes pick up admin configuration updates automatically.
func (s *ConfigService) RefreshIfStale(ctx context.Context) ConfigSnapshot {
	if s == nil || s.store == nil {
		return s.Snapshot()
	}
	now := time.Now()
	s.mu.RLock()
	current := s.snapshot
	stale := now.Sub(s.lastCheck) >= s.reloadCheckInterval
	s.mu.RUnlock()
	if !stale {
		current.Config = current.Config.Clone()
		return current
	}
	row, err := s.store.GetSetting(ctx, s.profile)
	if err != nil {
		logger.Warnw("kafuda_setting_reload_failed")
		s.mu.Lock()
		s.lastCheck = time.Now()
		snap := s.snapshot
		s.mu.Unlock()
		snap.Config = snap.Config.Clone()
		return snap
	}
	if row == nil {
		s.mu.Lock()
		s.lastCheck = time.Now()
		if s.snapshot.Source == ConfigSourceConfigFile {
			snap := s.snapshot
			s.mu.Unlock()
			snap.Config = snap.Config.Clone()
			return snap
		}
		snap := s.buildSnapshot(s.fallback, ConfigSourceConfigFile, 0, time.Time{})
		s.snapshot = snap
		onApply := s.onApply
		s.mu.Unlock()
		if onApply != nil {
			onApply(snap)
		}
		snap.Config = snap.Config.Clone()
		return snap
	}
	if current.Source == ConfigSourceDatabase && current.Version == row.Version && !current.LoadError {
		s.mu.Lock()
		s.lastCheck = time.Now()
		s.mu.Unlock()
		current.Config = current.Config.Clone()
		return current
	}
	snap, decErr := s.snapshotFromRow(row)
	if decErr != nil {
		logger.Warnw("kafuda_setting_decrypt_failed", "fail_closed", true)
	}
	s.mu.Lock()
	s.snapshot = snap
	s.lastCheck = time.Now()
	onApply := s.onApply
	s.mu.Unlock()
	if onApply != nil {
		onApply(snap)
	}
	snap.Config = snap.Config.Clone()
	return snap
}

// Get loads the latest configuration state from storage for the admin UI.
func (s *ConfigService) Get(ctx context.Context) (ConfigSnapshot, error) {
	if s == nil {
		return ConfigSnapshot{}, ErrConfigStoreUnavailable
	}
	if s.store == nil {
		return s.Snapshot(), nil
	}
	row, err := s.store.GetSetting(ctx, s.profile)
	if err != nil {
		return ConfigSnapshot{}, ErrConfigLoadFailed
	}
	if row == nil {
		snap := s.buildSnapshot(s.fallback, ConfigSourceConfigFile, 0, time.Time{})
		s.mu.Lock()
		changed := s.snapshot.Source != ConfigSourceConfigFile
		if changed {
			s.snapshot = snap
		}
		s.lastCheck = time.Now()
		onApply := s.onApply
		s.mu.Unlock()
		if changed && onApply != nil {
			onApply(snap)
		}
		snap.Config = snap.Config.Clone()
		return snap, nil
	}
	snap, decErr := s.snapshotFromRow(row)
	if decErr != nil {
		logger.Warnw("kafuda_setting_decrypt_failed", "fail_closed", true)
		s.mu.Lock()
		s.snapshot = snap
		s.lastCheck = time.Now()
		onApply := s.onApply
		s.mu.Unlock()
		if onApply != nil {
			onApply(snap)
		}
		return snap, ErrConfigLoadFailed
	}
	s.mu.Lock()
	changed := s.snapshot.Source != ConfigSourceDatabase || s.snapshot.Version != snap.Version || s.snapshot.LoadError
	if changed {
		s.snapshot = snap
	}
	s.lastCheck = time.Now()
	onApply := s.onApply
	s.mu.Unlock()
	if changed && onApply != nil {
		onApply(snap)
	}
	snap.Config = snap.Config.Clone()
	return snap, nil
}

// Update validates, encrypts, persists, and hot-applies a configuration patch.
func (s *ConfigService) Update(ctx context.Context, patch ConfigPatch) (ConfigSnapshot, error) {
	if s == nil || s.store == nil {
		return ConfigSnapshot{}, ErrConfigStoreUnavailable
	}
	base, err := s.currentEditableConfig(ctx)
	if err != nil {
		return ConfigSnapshot{}, err
	}
	next := base.ApplyPatch(patch).Normalize()
	if err := next.Validate(); err != nil {
		return ConfigSnapshot{}, err
	}
	var gateway kafudacontract.Gateway
	if next.APIKey != "" && s.gatewayFactory != nil {
		gw, gwErr := s.gatewayFactory(next.APIBaseURL, next.APIKey)
		if gwErr != nil {
			return ConfigSnapshot{}, ErrConfigInvalid
		}
		gateway = gw
	}
	encryptedKey := ""
	if next.APIKey != "" {
		enc, encErr := crypto.Encrypt(s.encKey, next.APIKey)
		if encErr != nil {
			return ConfigSnapshot{}, ErrConfigSaveFailed
		}
		encryptedKey = enc
	}
	now := time.Now().UTC()
	row := &kafudadomain.Setting{
		Profile:                       s.profile,
		APIBaseURL:                    next.APIBaseURL,
		APIKeyEncrypted:               encryptedKey,
		ProductCode:                   next.ProductCode,
		MaxCostCents:                  next.MaxCostCents,
		AllowedKinds:                  append([]string(nil), next.AllowedKinds...),
		SalesEnabled:                  next.SalesEnabled,
		ReferenceLookupVerified:       next.ReferenceLookupVerified,
		CatalogTTLSeconds:             next.CatalogTTLSeconds,
		CatalogRefreshIntervalSeconds: next.CatalogRefreshIntervalSeconds,
		PollReviewAfterSeconds:        next.PollReviewAfterSeconds,
		PollIntervalSeconds:           next.PollIntervalSeconds,
		UpdatedAt:                     now,
	}
	if err := s.store.SaveSetting(ctx, row); err != nil {
		return ConfigSnapshot{}, ErrConfigSaveFailed
	}
	snap := ConfigSnapshot{
		Config:               next.Clone(),
		Source:               ConfigSourceDatabase,
		Version:              row.Version,
		UpdatedAt:            row.UpdatedAt,
		Gateway:              gateway,
		Configured:           gateway != nil,
		GatewayConfigInvalid: false,
	}
	s.mu.Lock()
	s.snapshot = snap
	s.lastCheck = time.Now()
	onApply := s.onApply
	s.mu.Unlock()
	if onApply != nil {
		onApply(snap)
	}
	snap.Config = snap.Config.Clone()
	return snap, nil
}

// TestConnection verifies upstream HTTPS connectivity and API key authentication
// without persisting configuration changes or mutating the cached catalog.
func (s *ConfigService) TestConnection(ctx context.Context, patch *ConfigPatch) (*kafudadomain.Catalog, error) {
	if s == nil || s.gatewayFactory == nil {
		return nil, ErrConfigStoreUnavailable
	}
	base := s.Snapshot().Config
	if s.store != nil {
		if latest, err := s.currentEditableConfig(ctx); err == nil {
			base = latest
		}
	}
	candidate := base.Clone()
	if patch != nil {
		candidate = candidate.ApplyPatch(*patch)
	}
	candidate = candidate.Normalize()
	if err := validateHTTPSOrigin(candidate.APIBaseURL); err != nil {
		return nil, err
	}
	if strings.ContainsAny(candidate.APIKey, "\r\n") {
		return nil, ErrConfigAPIKeyInvalid
	}
	if candidate.APIKey == "" {
		return nil, ErrConfigAPIKeyRequired
	}
	gw, err := s.gatewayFactory(candidate.APIBaseURL, candidate.APIKey)
	if err != nil {
		return nil, ErrConfigInvalid
	}
	started := time.Now().UTC()
	data, err := gw.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	ttl := candidate.CatalogTTL()
	if ttl <= 0 {
		ttl = DefaultCatalogTTLSeconds * time.Second
	}
	return ParseCatalog(data, s.profile, started, started.Add(ttl))
}

func (s *ConfigService) loadInitial(ctx context.Context) ConfigSnapshot {
	if s.store == nil {
		return s.buildSnapshot(s.fallback, ConfigSourceConfigFile, 0, time.Time{})
	}
	row, err := s.store.GetSetting(ctx, s.profile)
	if err != nil {
		// Fail closed: a database read error must not silently resurrect an
		// enabled YAML fallback after an operator changed settings in the DB.
		logger.Warnw("kafuda_setting_load_failed", "fail_closed", true)
		return failClosedSnapshot()
	}
	if row == nil {
		return s.buildSnapshot(s.fallback, ConfigSourceConfigFile, 0, time.Time{})
	}
	snap, decErr := s.snapshotFromRow(row)
	if decErr != nil {
		logger.Warnw("kafuda_setting_decrypt_failed", "fail_closed", true)
	}
	return snap
}

func (s *ConfigService) currentEditableConfig(ctx context.Context) (RuntimeConfig, error) {
	if s.store == nil {
		return s.fallback.Clone(), nil
	}
	row, err := s.store.GetSetting(ctx, s.profile)
	if err != nil {
		return RuntimeConfig{}, ErrConfigLoadFailed
	}
	if row == nil {
		return fillAdminDefaults(s.fallback), nil
	}
	cfg, decErr := s.decodeRow(row)
	if decErr != nil {
		// Allow an administrator to recover from a rotated app.secret_key by saving a new key,
		// while preserving non-secret fields from the existing DB row.
		cfg = rowToNonSecretConfig(row)
		return fillAdminDefaults(cfg), nil
	}
	return fillAdminDefaults(cfg), nil
}

func fillAdminDefaults(cfg RuntimeConfig) RuntimeConfig {
	out := cfg.Clone()
	if strings.TrimSpace(out.APIBaseURL) == "" {
		out.APIBaseURL = DefaultAPIBaseURL
	}
	if out.CatalogTTLSeconds <= 0 {
		out.CatalogTTLSeconds = DefaultCatalogTTLSeconds
	}
	if out.PollIntervalSeconds <= 0 {
		out.PollIntervalSeconds = DefaultPollIntervalSeconds
	}
	if out.PollReviewAfterSeconds <= 0 {
		out.PollReviewAfterSeconds = DefaultPollReviewAfterSeconds
	}
	if out.AllowedKinds == nil {
		out.AllowedKinds = []string{"new"}
	}
	return out
}

func rowToNonSecretConfig(row *kafudadomain.Setting) RuntimeConfig {
	return RuntimeConfig{
		APIBaseURL:                    row.APIBaseURL,
		ProductCode:                   row.ProductCode,
		MaxCostCents:                  row.MaxCostCents,
		AllowedKinds:                  append([]string(nil), row.AllowedKinds...),
		SalesEnabled:                  row.SalesEnabled,
		ReferenceLookupVerified:       row.ReferenceLookupVerified,
		CatalogTTLSeconds:             row.CatalogTTLSeconds,
		CatalogRefreshIntervalSeconds: row.CatalogRefreshIntervalSeconds,
		PollReviewAfterSeconds:        row.PollReviewAfterSeconds,
		PollIntervalSeconds:           row.PollIntervalSeconds,
	}
}

func (s *ConfigService) decodeRow(row *kafudadomain.Setting) (RuntimeConfig, error) {
	cfg := rowToNonSecretConfig(row)
	if row.APIKeyEncrypted != "" {
		plain, err := crypto.Decrypt(s.encKey, row.APIKeyEncrypted)
		if err != nil {
			return RuntimeConfig{}, err
		}
		cfg.APIKey = plain
	}
	return cfg, nil
}

func (s *ConfigService) snapshotFromRow(row *kafudadomain.Setting) (ConfigSnapshot, error) {
	cfg, err := s.decodeRow(row)
	if err != nil {
		snap := failClosedSnapshot()
		snap.Config = rowToNonSecretConfig(row)
		snap.Config.SalesEnabled = false
		snap.Version = row.Version
		snap.UpdatedAt = row.UpdatedAt
		return snap, err
	}
	return s.buildSnapshot(cfg, ConfigSourceDatabase, row.Version, row.UpdatedAt), nil
}

func (s *ConfigService) buildSnapshot(cfg RuntimeConfig, source string, version int64, updatedAt time.Time) ConfigSnapshot {
	snap := ConfigSnapshot{
		Config:    cfg.Clone(),
		Source:    source,
		Version:   version,
		UpdatedAt: updatedAt,
	}
	if cfg.APIKey != "" && s.gatewayFactory != nil {
		gw, err := s.gatewayFactory(cfg.APIBaseURL, cfg.APIKey)
		if err != nil {
			snap.GatewayConfigInvalid = true
			logger.Warnw("kafuda_gateway_config_invalid")
		} else {
			snap.Gateway = gw
			snap.Configured = true
		}
	}
	return snap
}

func failClosedSnapshot() ConfigSnapshot {
	return ConfigSnapshot{
		Source:               ConfigSourceError,
		GatewayConfigInvalid: true,
		LoadError:            true,
	}
}
