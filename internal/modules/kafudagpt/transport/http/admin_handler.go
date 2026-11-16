package http

import (
	"context"
	"time"

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
	Catalog    CatalogUseCase
	Policy     kafudacontract.CommercePolicy
	Configured bool
}

func (h *AdminHandler) GetCatalog(c *gin.Context)  { h.catalog(c, false) }
func (h *AdminHandler) SyncCatalog(c *gin.Context) { h.catalog(c, true) }

func (h *AdminHandler) catalog(c *gin.Context, sync bool) {
	if h == nil || h.Catalog == nil || (sync && !h.Configured) {
		ginutil.RespondError(c, response.CodeBadRequest, "error.kafuda_not_configured", nil)
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
		ginutil.RespondError(c, response.CodeInternal, "error.kafuda_catalog_failed", nil)
		return
	}
	if catalog == nil {
		response.Success(c, gin.H{"options": []kafudacontract.SKUOption{}, "cards": []kafudadomain.CardProduct{}, "stale": true, "configured": h.Configured})
		return
	}
	options, err := h.Policy.SKUOptions(c.Request.Context())
	if err != nil {
		ginutil.RespondError(c, response.CodeInternal, "error.kafuda_catalog_failed", nil)
		return
	}
	response.Success(c, gin.H{"options": options, "cards": catalog.Cards, "enabled": catalog.Enabled, "synced_at": catalog.SyncedAt, "expires_at": catalog.ExpiresAt, "stale": !time.Now().Before(catalog.ExpiresAt), "configured": h.Configured})
}

func RegisterAdminRoutes(group gin.IRoutes, handler *AdminHandler) {
	group.GET("/kafuda-gpt/catalog", handler.GetCatalog)
	group.POST("/kafuda-gpt/catalog/sync", handler.SyncCatalog)
}
