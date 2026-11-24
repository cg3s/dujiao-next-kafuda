package application

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/dujiao-next/internal/crypto"
	"github.com/dujiao-next/internal/logger"
	quefacontract "github.com/dujiao-next/internal/modules/quefa/contract"
	quefadomain "github.com/dujiao-next/internal/modules/quefa/domain"
)

const (
	ConfigSourceConfigFile = "config_file"
	ConfigSourceDatabase   = "database"
	ConfigSourceError      = "error"

	DefaultCatalogTTLSeconds      = 3600
	DefaultPollIntervalSeconds    = 10
	DefaultPollReviewAfterSeconds = 86400
	DefaultWebhookMaxSkewSeconds  = 300

	defaultConfigReloadCheckInterval = 5 * time.Second
)

var (
	ErrConfigInvalid          = errors.New("quefa config invalid")
	ErrConfigBaseURLInvalid   = errors.New("quefa api base url must be an HTTPS origin")
	ErrConfigStoreUnavailable = errors.New("quefa config store unavailable")
	ErrConfigSaveFailed       = errors.New("quefa config save failed")
	ErrGatewayNotConfigured   = errors.New("quefa gateway not configured")
)

// GatewayFactory constructs an outbound Quefa API client from validated runtime parameters.
type GatewayFactory func(baseURL, partnerID, keyID, clientSecret string) (quefacontract.Gateway, error)

// RuntimeConfig holds the effective runtime parameters for Quefa hosted direct recharge.
type RuntimeConfig struct {
	APIBaseURL                    string
	PartnerID                     string
	KeyID                         string
	ClientSecret                  string
	WebhookSecret                 string
	CollectionMode                string
	SalesEnabled                  bool
	AllowedPayDomains             []string
	CatalogTTLSeconds             int
	CatalogRefreshIntervalSeconds int
	PollIntervalSeconds           int
	PollReviewAfterSeconds        int
	WebhookMaxSkewSeconds         int
}

// ConfigPatch represents an admin update payload for global Quefa settings.
type ConfigPatch struct {
	APIBaseURL                    *string   `json:"api_base_url"`
	PartnerID                     *string   `json:"partner_id"`
	KeyID                         *string   `json:"key_id"`
	ClientSecret                  *string   `json:"client_secret"`
	ClearClientSecret             bool      `json:"clear_client_secret"`
	WebhookSecret                 *string   `json:"webhook_secret"`
	ClearWebhookSecret            bool      `json:"clear_webhook_secret"`
	CollectionMode                *string   `json:"collection_mode"`
	SalesEnabled                  *bool     `json:"sales_enabled"`
	AllowedPayDomains             *[]string `json:"allowed_pay_domains"`
	CatalogTTLSeconds             *int      `json:"catalog_ttl_seconds"`
	CatalogRefreshIntervalSeconds *int      `json:"catalog_refresh_interval_seconds"`
	PollIntervalSeconds           *int      `json:"poll_interval_seconds"`
	PollReviewAfterSeconds        *int      `json:"poll_review_after_seconds"`
	WebhookMaxSkewSeconds         *int      `json:"webhook_max_skew_seconds"`
}

// ConfigSnapshot is an immutable view of active Quefa configuration.
type ConfigSnapshot struct {
	Config               RuntimeConfig
	Source               string
	Version              int64
	UpdatedAt            time.Time
	Gateway              quefacontract.Gateway
	Configured           bool
	GatewayConfigInvalid bool
	LoadError            bool
}

// AdminConfigView is the safe response returned to admin endpoints (never includes plaintext secrets).
type AdminConfigView struct {
	APIBaseURL                    string    `json:"api_base_url"`
	PartnerID                     string    `json:"partner_id"`
	KeyID                         string    `json:"key_id"`
	ClientSecretConfigured        bool      `json:"client_secret_configured"`
	WebhookSecretConfigured       bool      `json:"webhook_secret_configured"`
	CollectionMode                string    `json:"collection_mode"`
	SalesEnabled                  bool      `json:"sales_enabled"`
	AllowedPayDomains             []string  `json:"allowed_pay_domains"`
	CatalogTTLSeconds             int       `json:"catalog_ttl_seconds"`
	CatalogRefreshIntervalSeconds int       `json:"catalog_refresh_interval_seconds"`
	PollIntervalSeconds           int       `json:"poll_interval_seconds"`
	PollReviewAfterSeconds        int       `json:"poll_review_after_seconds"`
	WebhookMaxSkewSeconds         int       `json:"webhook_max_skew_seconds"`
	Source                        string    `json:"source"`
	Version                       int64     `json:"version"`
	UpdatedAt                     time.Time `json:"updated_at"`
}

func (c RuntimeConfig) Clone() RuntimeConfig {
	out := c
	if c.AllowedPayDomains != nil {
		out.AllowedPayDomains = append([]string(nil), c.AllowedPayDomains...)
	}
	return out
}

func (c RuntimeConfig) Normalize() RuntimeConfig {
	out := c.Clone()
	out.APIBaseURL = strings.TrimRight(strings.TrimSpace(out.APIBaseURL), "/")
	if out.APIBaseURL == "" {
		out.APIBaseURL = quefadomain.DefaultAPIBaseURL
	}
	out.PartnerID = strings.TrimSpace(out.PartnerID)
	out.KeyID = strings.TrimSpace(out.KeyID)
	out.ClientSecret = strings.TrimSpace(out.ClientSecret)
	out.WebhookSecret = strings.TrimSpace(out.WebhookSecret)
	out.CollectionMode = strings.ToLower(strings.TrimSpace(out.CollectionMode))
	if out.CollectionMode == "" {
		out.CollectionMode = quefadomain.CollectionModePlatformCollect
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
	if out.WebhookMaxSkewSeconds <= 0 {
		out.WebhookMaxSkewSeconds = DefaultWebhookMaxSkewSeconds
	}
	if out.AllowedPayDomains == nil {
		out.AllowedPayDomains = []string{}
	}
	return out
}

func (c RuntimeConfig) Validate() error {
	if err := validateQuefaOrigin(c.APIBaseURL); err != nil {
		return err
	}
	if strings.ContainsAny(c.PartnerID, "\r\n") || len(c.PartnerID) > 128 {
		return ErrConfigInvalid
	}
	if strings.ContainsAny(c.KeyID, "\r\n") || len(c.KeyID) > 128 {
		return ErrConfigInvalid
	}
	if strings.ContainsAny(c.ClientSecret, "\r\n") || strings.ContainsAny(c.WebhookSecret, "\r\n") {
		return ErrConfigInvalid
	}
	if !quefadomain.IsValidCollectionMode(c.CollectionMode) {
		return ErrConfigInvalid
	}
	if _, err := quefadomain.ValidateAllowedPayDomains(c.AllowedPayDomains); err != nil {
		return err
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
	if c.WebhookMaxSkewSeconds < 30 || c.WebhookMaxSkewSeconds > 3600 {
		return ErrConfigInvalid
	}
	return nil
}

func (c RuntimeConfig) CatalogTTL() time.Duration {
	if c.CatalogTTLSeconds <= 0 {
		return time.Duration(DefaultCatalogTTLSeconds) * time.Second
	}
	return time.Duration(c.CatalogTTLSeconds) * time.Second
}

// CatalogRefreshInterval computes the background catalog refresh cadence (matching config.Config.QuefaCatalogRefreshInterval).
func (c RuntimeConfig) CatalogRefreshInterval() time.Duration {
	ttlSeconds := c.CatalogTTLSeconds
	if ttlSeconds <= 0 {
		ttlSeconds = DefaultCatalogTTLSeconds
	}
	if c.CatalogRefreshIntervalSeconds < 0 {
		return 0
	}
	if c.CatalogRefreshIntervalSeconds > 0 {
		seconds := max(5, min(86400, c.CatalogRefreshIntervalSeconds))
		return time.Duration(seconds) * time.Second
	}
	seconds := max(1, min(60, ttlSeconds/4))
	return time.Duration(seconds) * time.Second
}

// CatalogFixedRefresh reports whether catalog refresh runs on an explicit fixed schedule.
func (c RuntimeConfig) CatalogFixedRefresh() bool {
	return c.CatalogRefreshIntervalSeconds > 0
}

func (c RuntimeConfig) PollInterval() time.Duration {
	if c.PollIntervalSeconds <= 0 {
		return time.Duration(DefaultPollIntervalSeconds) * time.Second
	}
	seconds := max(5, min(900, c.PollIntervalSeconds))
	return time.Duration(seconds) * time.Second
}

func (c RuntimeConfig) WebhookMaxSkew() time.Duration {
	if c.WebhookMaxSkewSeconds <= 0 {
		return time.Duration(DefaultWebhookMaxSkewSeconds) * time.Second
	}
	return time.Duration(c.WebhookMaxSkewSeconds) * time.Second
}

func validateQuefaOrigin(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !strings.EqualFold(u.Scheme, "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return ErrConfigBaseURLInvalid
	}
	cleanPath := strings.TrimRight(u.Path, "/")
	if cleanPath != "" && cleanPath != "/v1" {
		return ErrConfigBaseURLInvalid
	}
	return nil
}

type ConfigServiceOptions struct {
	Store          quefacontract.SettingStore
	Profile        string
	AppSecretKey   string
	Fallback       RuntimeConfig
	GatewayFactory GatewayFactory
}

type ConfigService struct {
	mu          sync.RWMutex
	store       quefacontract.SettingStore
	profile     string
	aesKey      []byte
	fallback    RuntimeConfig
	factory     GatewayFactory
	snapshot    ConfigSnapshot
	onApply     func(ConfigSnapshot)
	lastChecked time.Time
	now         func() time.Time
}

func NewConfigService(ctx context.Context, opts ConfigServiceOptions) *ConfigService {
	profile := strings.TrimSpace(opts.Profile)
	if profile == "" {
		profile = quefadomain.DefaultProfile
	}
	var key []byte
	if strings.TrimSpace(opts.AppSecretKey) != "" {
		key = crypto.DeriveKey(opts.AppSecretKey)
	}
	svc := &ConfigService{
		store:    opts.Store,
		profile:  profile,
		aesKey:   key,
		fallback: opts.Fallback.Normalize(),
		factory:  opts.GatewayFactory,
		now:      time.Now,
	}
	svc.snapshot = svc.loadInitial(ctx)
	svc.lastChecked = svc.now()
	return svc
}

func (s *ConfigService) SetOnApply(fn func(ConfigSnapshot)) {
	s.mu.Lock()
	s.onApply = fn
	s.mu.Unlock()
}

func (s *ConfigService) Snapshot() ConfigSnapshot {
	s.mu.RLock()
	snap := s.snapshot
	snap.Config = snap.Config.Clone()
	s.mu.RUnlock()
	return snap
}

func (s *ConfigService) WebhookSecretAndSkew(ctx context.Context) (string, time.Duration) {
	s.RefreshIfStale(ctx)
	snap := s.Snapshot()
	return snap.Config.WebhookSecret, snap.Config.WebhookMaxSkew()
}

func (s *ConfigService) AdminView(ctx context.Context) AdminConfigView {
	s.RefreshIfStale(ctx)
	snap := s.Snapshot()
	return AdminConfigView{
		APIBaseURL:                    snap.Config.APIBaseURL,
		PartnerID:                     snap.Config.PartnerID,
		KeyID:                         snap.Config.KeyID,
		ClientSecretConfigured:        snap.Config.ClientSecret != "",
		WebhookSecretConfigured:       snap.Config.WebhookSecret != "",
		CollectionMode:                snap.Config.CollectionMode,
		SalesEnabled:                  snap.Config.SalesEnabled,
		AllowedPayDomains:             append([]string(nil), snap.Config.AllowedPayDomains...),
		CatalogTTLSeconds:             snap.Config.CatalogTTLSeconds,
		CatalogRefreshIntervalSeconds: snap.Config.CatalogRefreshIntervalSeconds,
		PollIntervalSeconds:           snap.Config.PollIntervalSeconds,
		PollReviewAfterSeconds:        snap.Config.PollReviewAfterSeconds,
		WebhookMaxSkewSeconds:         snap.Config.WebhookMaxSkewSeconds,
		Source:                        snap.Source,
		Version:                       snap.Version,
		UpdatedAt:                     snap.UpdatedAt,
	}
}

func (s *ConfigService) RefreshIfStale(ctx context.Context) {
	if s == nil || s.store == nil {
		return
	}
	s.mu.RLock()
	stale := s.now().Sub(s.lastChecked) >= defaultConfigReloadCheckInterval
	currentVersion := s.snapshot.Version
	currentSource := s.snapshot.Source
	s.mu.RUnlock()
	if !stale {
		return
	}
	row, err := s.store.GetSetting(ctx, s.profile)
	s.mu.Lock()
	s.lastChecked = s.now()
	s.mu.Unlock()
	if err != nil {
		return
	}
	if row == nil {
		if currentSource == ConfigSourceDatabase {
			snap := s.buildSnapshot(s.fallback, ConfigSourceConfigFile, 0, time.Time{})
			s.commitSnapshot(snap)
		}
		return
	}
	if currentSource == ConfigSourceDatabase && row.Version == currentVersion {
		return
	}
	cfg, err := s.decodeSetting(row)
	if err != nil {
		logger.Warnw("quefa_config_reload_decrypt_failed", "error", err)
		return
	}
	snap := s.buildSnapshot(cfg, ConfigSourceDatabase, row.Version, row.UpdatedAt)
	s.commitSnapshot(snap)
}

func (s *ConfigService) Update(ctx context.Context, patch ConfigPatch) (AdminConfigView, error) {
	if s.store == nil {
		return AdminConfigView{}, ErrConfigStoreUnavailable
	}
	current := s.Snapshot()
	next := current.Config.Clone()
	if patch.APIBaseURL != nil {
		next.APIBaseURL = *patch.APIBaseURL
	}
	if patch.PartnerID != nil {
		next.PartnerID = *patch.PartnerID
	}
	if patch.KeyID != nil {
		next.KeyID = *patch.KeyID
	}
	if patch.ClearClientSecret {
		next.ClientSecret = ""
	} else if patch.ClientSecret != nil && strings.TrimSpace(*patch.ClientSecret) != "" {
		next.ClientSecret = strings.TrimSpace(*patch.ClientSecret)
	}
	if patch.ClearWebhookSecret {
		next.WebhookSecret = ""
	} else if patch.WebhookSecret != nil && strings.TrimSpace(*patch.WebhookSecret) != "" {
		next.WebhookSecret = strings.TrimSpace(*patch.WebhookSecret)
	}
	if patch.CollectionMode != nil {
		next.CollectionMode = *patch.CollectionMode
	}
	if patch.SalesEnabled != nil {
		next.SalesEnabled = *patch.SalesEnabled
	}
	if patch.AllowedPayDomains != nil {
		next.AllowedPayDomains = append([]string(nil), (*patch.AllowedPayDomains)...)
	}
	if patch.CatalogTTLSeconds != nil {
		next.CatalogTTLSeconds = *patch.CatalogTTLSeconds
	}
	if patch.CatalogRefreshIntervalSeconds != nil {
		next.CatalogRefreshIntervalSeconds = *patch.CatalogRefreshIntervalSeconds
	}
	if patch.PollIntervalSeconds != nil {
		next.PollIntervalSeconds = *patch.PollIntervalSeconds
	}
	if patch.PollReviewAfterSeconds != nil {
		next.PollReviewAfterSeconds = *patch.PollReviewAfterSeconds
	}
	if patch.WebhookMaxSkewSeconds != nil {
		next.WebhookMaxSkewSeconds = *patch.WebhookMaxSkewSeconds
	}

	next = next.Normalize()
	domains, err := quefadomain.ValidateAllowedPayDomains(next.AllowedPayDomains)
	if err != nil {
		return AdminConfigView{}, err
	}
	next.AllowedPayDomains = domains
	if err := next.Validate(); err != nil {
		return AdminConfigView{}, err
	}

	encClient, err := s.encryptSecret(next.ClientSecret)
	if err != nil {
		return AdminConfigView{}, ErrConfigSaveFailed
	}
	encWebhook, err := s.encryptSecret(next.WebhookSecret)
	if err != nil {
		return AdminConfigView{}, ErrConfigSaveFailed
	}

	now := s.now().UTC()
	nextVersion := current.Version + 1
	if nextVersion <= 0 {
		nextVersion = 1
	}
	row := &quefadomain.Setting{
		Profile:                       s.profile,
		APIBaseURL:                    next.APIBaseURL,
		PartnerID:                     next.PartnerID,
		KeyID:                         next.KeyID,
		ClientSecretEncrypted:         encClient,
		WebhookSecretEncrypted:        encWebhook,
		CollectionMode:                next.CollectionMode,
		SalesEnabled:                  next.SalesEnabled,
		AllowedPayDomains:             next.AllowedPayDomains,
		CatalogTTLSeconds:             next.CatalogTTLSeconds,
		CatalogRefreshIntervalSeconds: next.CatalogRefreshIntervalSeconds,
		PollIntervalSeconds:           next.PollIntervalSeconds,
		PollReviewAfterSeconds:        next.PollReviewAfterSeconds,
		WebhookMaxSkewSeconds:         next.WebhookMaxSkewSeconds,
		Version:                       nextVersion,
		UpdatedAt:                     now,
	}
	if err := s.store.SaveSetting(ctx, row); err != nil {
		return AdminConfigView{}, ErrConfigSaveFailed
	}

	snap := s.buildSnapshot(next, ConfigSourceDatabase, nextVersion, now)
	s.commitSnapshot(snap)
	return s.AdminView(ctx), nil
}

// TestConnection performs a read-only GET /v1/products call using current saved config
// (or optional patch overrides without saving) and parses the response.
func (s *ConfigService) TestConnection(ctx context.Context, patch *ConfigPatch) (int, error) {
	cfg := s.Snapshot().Config.Clone()
	if patch != nil {
		if patch.APIBaseURL != nil && strings.TrimSpace(*patch.APIBaseURL) != "" {
			cfg.APIBaseURL = *patch.APIBaseURL
		}
		if patch.PartnerID != nil && strings.TrimSpace(*patch.PartnerID) != "" {
			cfg.PartnerID = *patch.PartnerID
		}
		if patch.KeyID != nil && strings.TrimSpace(*patch.KeyID) != "" {
			cfg.KeyID = *patch.KeyID
		}
		if patch.ClientSecret != nil && strings.TrimSpace(*patch.ClientSecret) != "" {
			cfg.ClientSecret = *patch.ClientSecret
		}
	}
	cfg = cfg.Normalize()
	if s.factory == nil || cfg.PartnerID == "" || cfg.KeyID == "" || cfg.ClientSecret == "" {
		return 0, ErrGatewayNotConfigured
	}
	gw, err := s.factory(cfg.APIBaseURL, cfg.PartnerID, cfg.KeyID, cfg.ClientSecret)
	if err != nil {
		return 0, err
	}
	raw, _, err := gw.GetProducts(ctx)
	if err != nil {
		logger.Warnw("quefa_test_connection_failed", "base_url", cfg.APIBaseURL, "error", err)
		return 0, err
	}
	products, err := quefadomain.ParseCatalogPayload(raw)
	if err != nil {
		shape := quefadomain.DescribePayloadShape(raw)
		logger.Warnw("quefa_test_catalog_parse_failed", "base_url", cfg.APIBaseURL, "shape", shape, "error", err)
		return 0, &CatalogParseError{Err: err, Shape: shape}
	}
	return len(products), nil
}

func (s *ConfigService) loadInitial(ctx context.Context) ConfigSnapshot {
	if s.store != nil {
		row, err := s.store.GetSetting(ctx, s.profile)
		if err != nil {
			logger.Warnw("quefa_config_initial_load_failed", "error", err)
			snap := s.buildSnapshot(s.fallback, ConfigSourceError, 0, time.Time{})
			snap.LoadError = true
			return snap
		}
		if row != nil {
			cfg, err := s.decodeSetting(row)
			if err != nil {
				logger.Warnw("quefa_config_initial_decrypt_failed", "error", err)
				snap := s.buildSnapshot(s.fallback, ConfigSourceError, row.Version, row.UpdatedAt)
				snap.LoadError = true
				return snap
			}
			return s.buildSnapshot(cfg, ConfigSourceDatabase, row.Version, row.UpdatedAt)
		}
	}
	return s.buildSnapshot(s.fallback, ConfigSourceConfigFile, 0, time.Time{})
}

func (s *ConfigService) buildSnapshot(cfg RuntimeConfig, source string, version int64, updatedAt time.Time) ConfigSnapshot {
	norm := cfg.Normalize()
	if domains, err := quefadomain.ValidateAllowedPayDomains(norm.AllowedPayDomains); err == nil {
		norm.AllowedPayDomains = domains
	}
	snap := ConfigSnapshot{
		Config:    norm,
		Source:    source,
		Version:   version,
		UpdatedAt: updatedAt,
	}
	if norm.PartnerID != "" && norm.KeyID != "" && norm.ClientSecret != "" {
		snap.Configured = true
		if s.factory != nil {
			gw, err := s.factory(norm.APIBaseURL, norm.PartnerID, norm.KeyID, norm.ClientSecret)
			if err != nil {
				snap.GatewayConfigInvalid = true
			} else {
				snap.Gateway = gw
			}
		}
	}
	return snap
}

func (s *ConfigService) commitSnapshot(snap ConfigSnapshot) {
	s.mu.Lock()
	s.snapshot = snap
	s.lastChecked = s.now()
	cb := s.onApply
	s.mu.Unlock()
	if cb != nil {
		cb(snap)
	}
}

func (s *ConfigService) encryptSecret(plaintext string) (string, error) {
	if strings.TrimSpace(plaintext) == "" {
		return "", nil
	}
	if len(s.aesKey) == 0 {
		return "", errors.New("app secret key required to encrypt quefa secret")
	}
	return crypto.Encrypt(s.aesKey, plaintext)
}

func (s *ConfigService) decryptSecret(ciphertext string) (string, error) {
	if strings.TrimSpace(ciphertext) == "" {
		return "", nil
	}
	if len(s.aesKey) == 0 {
		return "", errors.New("app secret key required to decrypt quefa secret")
	}
	return crypto.Decrypt(s.aesKey, ciphertext)
}

func (s *ConfigService) decodeSetting(row *quefadomain.Setting) (RuntimeConfig, error) {
	clientSecret, err := s.decryptSecret(row.ClientSecretEncrypted)
	if err != nil {
		return RuntimeConfig{}, err
	}
	webhookSecret, err := s.decryptSecret(row.WebhookSecretEncrypted)
	if err != nil {
		return RuntimeConfig{}, err
	}
	return RuntimeConfig{
		APIBaseURL:                    row.APIBaseURL,
		PartnerID:                     row.PartnerID,
		KeyID:                         row.KeyID,
		ClientSecret:                  clientSecret,
		WebhookSecret:                 webhookSecret,
		CollectionMode:                row.CollectionMode,
		SalesEnabled:                  row.SalesEnabled,
		AllowedPayDomains:             append([]string(nil), row.AllowedPayDomains...),
		CatalogTTLSeconds:             row.CatalogTTLSeconds,
		CatalogRefreshIntervalSeconds: row.CatalogRefreshIntervalSeconds,
		PollIntervalSeconds:           row.PollIntervalSeconds,
		PollReviewAfterSeconds:        row.PollReviewAfterSeconds,
		WebhookMaxSkewSeconds:         row.WebhookMaxSkewSeconds,
	}.Normalize(), nil
}
