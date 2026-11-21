package http

import (
	"context"
	"errors"
	"io"
	"time"

	kafudaapp "github.com/dujiao-next/internal/modules/kafudagpt/application"
	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
	"github.com/dujiao-next/internal/platform/http/ginutil"
	"github.com/dujiao-next/internal/platform/http/response"
	"github.com/gin-gonic/gin"
)

type CatalogUseCase interface {
	Cached(context.Context) (*kafudadomain.Catalog, error)
	Sync(context.Context) (*kafudadomain.Catalog, error)
}

type ConfigUseCase interface {
	Get(context.Context) (kafudaapp.ConfigSnapshot, error)
	Update(context.Context, kafudaapp.ConfigPatch) (kafudaapp.ConfigSnapshot, error)
	TestConnection(context.Context, *kafudaapp.ConfigPatch) (*kafudadomain.Catalog, error)
}

type AdminHandler struct {
	Catalog                CatalogUseCase
	Config                 ConfigUseCase
	FXRates                kafudacontract.FXRateProvider
	Configured             bool
	GatewayConfigInvalid   bool
	IsConfigured           func() bool
	IsGatewayConfigInvalid func() bool
	WorkerReady            func(context.Context) bool
	SalesBlockReason       func(context.Context, *kafudadomain.Catalog) string
}

func (h *AdminHandler) GetCatalog(c *gin.Context)  { h.catalog(c, false) }
func (h *AdminHandler) SyncCatalog(c *gin.Context) { h.catalog(c, true) }

func (h *AdminHandler) isConfigured() bool {
	if h == nil {
		return false
	}
	if h.IsConfigured != nil {
		return h.IsConfigured()
	}
	return h.Configured
}

func (h *AdminHandler) isGatewayConfigInvalid() bool {
	if h == nil {
		return false
	}
	if h.IsGatewayConfigInvalid != nil {
		return h.IsGatewayConfigInvalid()
	}
	return h.GatewayConfigInvalid
}

func (h *AdminHandler) GetConfig(c *gin.Context) {
	if h == nil || h.Config == nil {
		ginutil.RespondError(c, response.CodeBadRequest, "error.kafuda_not_configured", nil)
		return
	}
	snap, err := h.Config.Get(c.Request.Context())
	if err != nil && !snap.LoadError {
		ginutil.RequestLog(c).Warnw("kafuda_config_get_failed")
		ginutil.RespondError(c, response.CodeInternal, "error.kafuda_config_load_failed", nil)
		return
	}
	response.Success(c, h.buildConfigResponse(c.Request.Context(), snap))
}

func (h *AdminHandler) UpdateConfig(c *gin.Context) {
	if h == nil || h.Config == nil {
		ginutil.RespondError(c, response.CodeBadRequest, "error.kafuda_not_configured", nil)
		return
	}
	var patch kafudaapp.ConfigPatch
	if err := c.ShouldBindJSON(&patch); err != nil {
		ginutil.RespondError(c, response.CodeBadRequest, "error.bad_request", nil)
		return
	}
	snap, err := h.Config.Update(c.Request.Context(), patch)
	if err != nil {
		respondConfigFailure(c, err)
		return
	}
	response.Success(c, h.buildConfigResponse(c.Request.Context(), snap))
}

func (h *AdminHandler) TestConfig(c *gin.Context) {
	if h == nil || h.Config == nil {
		ginutil.RespondError(c, response.CodeBadRequest, "error.kafuda_not_configured", nil)
		return
	}
	var patch *kafudaapp.ConfigPatch
	if c.Request.Body != nil {
		var body kafudaapp.ConfigPatch
		if err := c.ShouldBindJSON(&body); err != nil && !errors.Is(err, io.EOF) {
			ginutil.RespondError(c, response.CodeBadRequest, "error.bad_request", nil)
			return
		} else if err == nil {
			patch = &body
		}
	}
	catalog, err := h.Config.TestConnection(c.Request.Context(), patch)
	if err != nil {
		if isConfigValidationError(err) {
			respondConfigFailure(c, err)
			return
		}
		respondCatalogFailure(c, err)
		return
	}
	data := gin.H{
		"ok":            true,
		"enabled":       catalog.Enabled,
		"options_count": len(catalog.Options),
		"cards":         catalog.Cards,
	}
	response.Success(c, data)
}

func (h *AdminHandler) buildConfigResponse(ctx context.Context, snap kafudaapp.ConfigSnapshot) gin.H {
	cfg := snap.Config
	allowedKinds := cfg.AllowedKinds
	if allowedKinds == nil {
		allowedKinds = []string{}
	}
	configured := snap.Configured
	if h.IsConfigured != nil {
		configured = h.IsConfigured()
	}
	gatewayInvalid := snap.GatewayConfigInvalid
	if h.IsGatewayConfigInvalid != nil {
		gatewayInvalid = h.IsGatewayConfigInvalid()
	}
	workerReady := false
	if h.WorkerReady != nil {
		workerReady = h.WorkerReady(ctx)
	}
	var catalog *kafudadomain.Catalog
	if h.Catalog != nil {
		if cached, err := h.Catalog.Cached(ctx); err == nil {
			catalog = cached
		}
	}
	reason := ""
	switch {
	case gatewayInvalid:
		reason = "gateway_config_invalid"
	case !configured:
		reason = "gateway_not_configured"
	case h.SalesBlockReason != nil:
		reason = h.SalesBlockReason(ctx, catalog)
	}
	data := gin.H{
		"api_base_url":                     cfg.APIBaseURL,
		"has_api_key":                      cfg.APIKey != "",
		"product_code":                     cfg.ProductCode,
		"max_cost_cents":                   cfg.MaxCostCents,
		"allowed_kinds":                    allowedKinds,
		"sales_enabled":                    cfg.SalesEnabled,
		"reference_lookup_verified":        cfg.ReferenceLookupVerified,
		"catalog_ttl_seconds":              cfg.CatalogTTLSeconds,
		"catalog_refresh_interval_seconds": cfg.CatalogRefreshIntervalSeconds,
		"poll_review_after_seconds":        cfg.PollReviewAfterSeconds,
		"poll_interval_seconds":            cfg.PollIntervalSeconds,
		"config_source":                    snap.Source,
		"version":                          snap.Version,
		"configured":                       configured,
		"gateway_config_invalid":           gatewayInvalid,
		"worker_ready":                     workerReady,
		"sales_block_reason":               reason,
		"catalog_stale":                    true,
		"catalog_enabled":                  false,
		"catalog_options_count":            0,
		"cards":                            []kafudadomain.CardProduct{},
	}
	if !snap.UpdatedAt.IsZero() {
		data["updated_at"] = snap.UpdatedAt
	}
	if catalog != nil {
		data["catalog_enabled"] = catalog.Enabled
		data["catalog_synced_at"] = catalog.SyncedAt
		data["catalog_expires_at"] = catalog.ExpiresAt
		data["catalog_stale"] = !time.Now().Before(catalog.ExpiresAt)
		data["catalog_options_count"] = len(catalog.Options)
		data["cards"] = catalog.Cards
	}
	return data
}

func isConfigValidationError(err error) bool {
	return errors.Is(err, kafudaapp.ErrConfigBaseURLInvalid) ||
		errors.Is(err, kafudaapp.ErrConfigAPIKeyInvalid) ||
		errors.Is(err, kafudaapp.ErrConfigAPIKeyRequired) ||
		errors.Is(err, kafudaapp.ErrConfigInvalid) ||
		errors.Is(err, kafudaapp.ErrConfigStoreUnavailable)
}

func respondConfigFailure(c *gin.Context, err error) {
	switch {
	case errors.Is(err, kafudaapp.ErrConfigBaseURLInvalid):
		ginutil.RespondError(c, response.CodeBadRequest, "error.kafuda_config_base_url_invalid", nil)
	case errors.Is(err, kafudaapp.ErrConfigAPIKeyInvalid):
		ginutil.RespondError(c, response.CodeBadRequest, "error.kafuda_config_api_key_invalid", nil)
	case errors.Is(err, kafudaapp.ErrConfigAPIKeyRequired), errors.Is(err, kafudaapp.ErrConfigStoreUnavailable):
		ginutil.RespondError(c, response.CodeBadRequest, "error.kafuda_not_configured", nil)
	case errors.Is(err, kafudaapp.ErrConfigInvalid):
		ginutil.RespondError(c, response.CodeBadRequest, "error.kafuda_config_invalid", nil)
	case errors.Is(err, kafudaapp.ErrConfigLoadFailed):
		ginutil.RequestLog(c).Warnw("kafuda_config_load_failed")
		ginutil.RespondError(c, response.CodeInternal, "error.kafuda_config_load_failed", nil)
	default:
		ginutil.RequestLog(c).Warnw("kafuda_config_save_failed")
		ginutil.RespondError(c, response.CodeInternal, "error.kafuda_config_save_failed", nil)
	}
}

func (h *AdminHandler) catalog(c *gin.Context, sync bool) {
	if h == nil || h.Catalog == nil {
		ginutil.RespondError(c, response.CodeBadRequest, "error.kafuda_not_configured", nil)
		return
	}
	configured := h.isConfigured()
	gatewayInvalid := h.isGatewayConfigInvalid()
	if sync && !configured {
		key := "error.kafuda_not_configured"
		if gatewayInvalid {
			key = "error.kafuda_config_invalid"
		}
		ginutil.RespondError(c, response.CodeBadRequest, key, nil)
		return
	}
	var catalog *kafudadomain.Catalog
	var err error
	if sync {
		catalog, err = h.Catalog.Sync(c.Request.Context())
	} else {
		catalog, err = h.Catalog.Cached(c.Request.Context())
	}
	if err != nil {
		respondCatalogFailure(c, err)
		return
	}
	data := gin.H{"options": []kafudacontract.SKUOption{}, "cards": []kafudadomain.CardProduct{}, "stale": true, "configured": configured}
	if gatewayInvalid {
		data["sales_block_reason"] = "gateway_config_invalid"
	} else if !configured {
		data["sales_block_reason"] = "gateway_not_configured"
	} else if h.SalesBlockReason != nil {
		if reason := h.SalesBlockReason(c.Request.Context(), catalog); reason != "" {
			data["sales_block_reason"] = reason
		}
	}
	if catalog != nil {
		options := kafudaapp.CatalogSKUOptions(catalog)
		if h.FXRates != nil && catalogNeedsFX(options) {
			rates, updatedAt, fxErr := h.FXRates.RatesToCNY(c.Request.Context())
			if fxErr != nil {
				data["fx_unavailable"] = true
				ginutil.RequestLog(c).Warnw("kafuda_catalog_fx_failed")
			} else {
				for i := range options {
					if options[i].EstimatedLocalMinor == nil {
						continue
					}
					if costCNY, ok := kafudaapp.EstimateCostCNY(options[i].Currency, *options[i].EstimatedLocalMinor, options[i].LocalExponent, rates); ok {
						val := costCNY
						options[i].EstimatedCostCNY = &val
					}
				}
				if !updatedAt.IsZero() {
					data["fx_updated_at"] = updatedAt
				}
			}
		}
		data["options"] = options
		data["cards"] = catalog.Cards
		data["enabled"] = catalog.Enabled
		data["synced_at"] = catalog.SyncedAt
		data["expires_at"] = catalog.ExpiresAt
		data["stale"] = !time.Now().Before(catalog.ExpiresAt)
	}
	response.Success(c, data)
}

func catalogNeedsFX(options []kafudacontract.SKUOption) bool {
	for _, opt := range options {
		if opt.EstimatedLocalMinor != nil && *opt.EstimatedLocalMinor > 0 && opt.Currency != "" {
			return true
		}
	}
	return false
}

func respondCatalogFailure(c *gin.Context, err error) {
	key := "error.kafuda_catalog_failed"
	status := 0
	var upstream *kafudacontract.APIError
	switch {
	case errors.As(err, &upstream):
		status = upstream.HTTPStatus
		if status < 100 || status > 599 {
			status = 0
		}
		switch {
		case status == 401:
			key = "error.kafuda_catalog_auth_failed"
		case status == 403:
			key = "error.kafuda_catalog_forbidden"
		case status == 404:
			key = "error.kafuda_catalog_endpoint_missing"
		case status == 429:
			key = "error.kafuda_catalog_rate_limited"
		case status >= 300 && status < 400:
			key = "error.kafuda_catalog_redirect"
		case status >= 500:
			key = "error.kafuda_catalog_upstream_unavailable"
		case upstream.Code == "invalid_response":
			key = "error.kafuda_catalog_invalid_response"
		case status == 0:
			key = "error.kafuda_catalog_network_failed"
		}
	case errors.Is(err, kafudadomain.ErrInvalidCatalog):
		key = "error.kafuda_catalog_invalid_response"
	case errors.Is(err, kafudacontract.ErrCatalogReadFailed):
		key = "error.kafuda_catalog_cache_read_failed"
	case errors.Is(err, kafudacontract.ErrCatalogWriteFailed):
		key = "error.kafuda_catalog_cache_write_failed"
	}
	// Never log upstream bodies, error codes, URLs, or raw database errors.
	// Even a syntactically safe upstream error code can contain a credential.
	ginutil.RequestLog(c).Warnw("kafuda_catalog_failed", "reason", key, "upstream_http_status", status)
	// An upstream 401 is not an expired administrator login.
	ginutil.RespondError(c, response.CodeInternal, key, nil)
}
