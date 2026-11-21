package http

import (
	"context"
	"errors"
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

type AdminHandler struct {
	Catalog              CatalogUseCase
	FXRates              kafudacontract.FXRateProvider
	Configured           bool
	GatewayConfigInvalid bool
	SalesBlockReason     func(context.Context, *kafudadomain.Catalog) string
}

func (h *AdminHandler) GetCatalog(c *gin.Context)  { h.catalog(c, false) }
func (h *AdminHandler) SyncCatalog(c *gin.Context) { h.catalog(c, true) }

func (h *AdminHandler) catalog(c *gin.Context, sync bool) {
	if h == nil || h.Catalog == nil {
		ginutil.RespondError(c, response.CodeBadRequest, "error.kafuda_not_configured", nil)
		return
	}
	if sync && !h.Configured {
		key := "error.kafuda_not_configured"
		if h.GatewayConfigInvalid {
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
	data := gin.H{"options": []kafudacontract.SKUOption{}, "cards": []kafudadomain.CardProduct{}, "stale": true, "configured": h.Configured}
	if h.GatewayConfigInvalid {
		data["sales_block_reason"] = "gateway_config_invalid"
	} else if !h.Configured {
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
