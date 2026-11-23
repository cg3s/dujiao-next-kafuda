package http

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/dujiao-next/internal/i18n"
	quefaapp "github.com/dujiao-next/internal/modules/quefa/application"
	quefacontract "github.com/dujiao-next/internal/modules/quefa/contract"
	quefadomain "github.com/dujiao-next/internal/modules/quefa/domain"
	"github.com/dujiao-next/internal/platform/http/ginutil"
	"github.com/dujiao-next/internal/platform/http/response"
	"github.com/gin-gonic/gin"
)

type AdminConfigPort interface {
	AdminView(ctx context.Context) quefaapp.AdminConfigView
	Update(ctx context.Context, patch quefaapp.ConfigPatch) (quefaapp.AdminConfigView, error)
	TestConnection(ctx context.Context, patch *quefaapp.ConfigPatch) (int, error)
}

type AdminCatalogPort interface {
	Read(ctx context.Context) (*quefaapp.CatalogView, error)
	Sync(ctx context.Context) (*quefaapp.CatalogView, error)
	SalesReadiness(ctx context.Context) quefacontract.SalesReadiness
}

type AdminOrderPort interface {
	GetAdminStatusByParentOrder(ctx context.Context, parentOrderID uint) (*quefacontract.AdminOrderStatus, error)
	SyncOrder(ctx context.Context, childOrderID uint) error
	MarkManualReview(ctx context.Context, parentOrderID uint, remark string) (*quefacontract.AdminOrderStatus, error)
}

type AdminHandler struct {
	Config  AdminConfigPort
	Catalog AdminCatalogPort
	Orders  AdminOrderPort
}

func (h *AdminHandler) GetConfig(c *gin.Context) {
	if h == nil || h.Config == nil {
		ginutil.RespondError(c, response.CodeBadRequest, "error.quefa_not_configured", nil)
		return
	}
	view := h.Config.AdminView(c.Request.Context())
	payload := gin.H{
		"config": view,
	}
	if h.Catalog != nil {
		payload["readiness"] = h.Catalog.SalesReadiness(c.Request.Context())
	}
	response.Success(c, payload)
}

func (h *AdminHandler) UpdateConfig(c *gin.Context) {
	if h == nil || h.Config == nil {
		ginutil.RespondError(c, response.CodeBadRequest, "error.quefa_not_configured", nil)
		return
	}
	var patch quefaapp.ConfigPatch
	if err := c.ShouldBindJSON(&patch); err != nil {
		ginutil.RespondError(c, response.CodeBadRequest, "error.bad_request", nil)
		return
	}
	view, err := h.Config.Update(c.Request.Context(), patch)
	if err != nil {
		respondQuefaConfigError(c, err)
		return
	}
	payload := gin.H{
		"config": view,
	}
	if h.Catalog != nil {
		payload["readiness"] = h.Catalog.SalesReadiness(c.Request.Context())
	}
	response.Success(c, payload)
}

func (h *AdminHandler) TestConfig(c *gin.Context) {
	if h == nil || h.Config == nil {
		ginutil.RespondError(c, response.CodeBadRequest, "error.quefa_not_configured", nil)
		return
	}
	var patch *quefaapp.ConfigPatch
	var body quefaapp.ConfigPatch
	if err := c.ShouldBindJSON(&body); err == nil {
		patch = &body
	} else if !errors.Is(err, io.EOF) {
		ginutil.RespondError(c, response.CodeBadRequest, "error.bad_request", nil)
		return
	}
	count, err := h.Config.TestConnection(c.Request.Context(), patch)
	if err != nil {
		respondQuefaUpstreamError(c, err)
		return
	}
	response.Success(c, gin.H{"ok": true, "product_count": count})
}

func (h *AdminHandler) GetCatalog(c *gin.Context) {
	if h == nil || h.Catalog == nil {
		ginutil.RespondError(c, response.CodeBadRequest, "error.quefa_not_configured", nil)
		return
	}
	view, err := h.Catalog.Read(c.Request.Context())
	if err != nil {
		ginutil.RespondError(c, response.CodeInternal, "error.quefa_catalog_fetch_failed", nil)
		return
	}
	response.Success(c, view)
}

func (h *AdminHandler) SyncCatalog(c *gin.Context) {
	if h == nil || h.Catalog == nil {
		ginutil.RespondError(c, response.CodeBadRequest, "error.quefa_not_configured", nil)
		return
	}
	view, err := h.Catalog.Sync(c.Request.Context())
	if err != nil {
		respondQuefaUpstreamError(c, err)
		return
	}
	response.Success(c, view)
}

func (h *AdminHandler) GetOrderStatus(c *gin.Context) {
	if h == nil || h.Orders == nil {
		ginutil.RespondError(c, response.CodeNotFound, "error.order_not_found", nil)
		return
	}
	orderID, ok := parseUintParam(c, "id")
	if !ok {
		ginutil.RespondError(c, response.CodeBadRequest, "error.bad_request", nil)
		return
	}
	status, err := h.Orders.GetAdminStatusByParentOrder(c.Request.Context(), orderID)
	if err != nil {
		ginutil.RespondError(c, response.CodeInternal, "error.order_fetch_failed", nil)
		return
	}
	if status == nil {
		ginutil.RespondError(c, response.CodeNotFound, "error.order_not_found", nil)
		return
	}
	response.Success(c, status)
}

func (h *AdminHandler) SyncOrder(c *gin.Context) {
	if h == nil || h.Orders == nil {
		ginutil.RespondError(c, response.CodeNotFound, "error.order_not_found", nil)
		return
	}
	orderID, ok := parseUintParam(c, "id")
	if !ok {
		ginutil.RespondError(c, response.CodeBadRequest, "error.bad_request", nil)
		return
	}
	status, err := h.Orders.GetAdminStatusByParentOrder(c.Request.Context(), orderID)
	if err != nil {
		ginutil.RespondError(c, response.CodeInternal, "error.order_fetch_failed", nil)
		return
	}
	if status == nil {
		ginutil.RespondError(c, response.CodeNotFound, "error.order_not_found", nil)
		return
	}
	if err := h.Orders.SyncOrder(c.Request.Context(), status.OrderID); err != nil {
		respondQuefaUpstreamError(c, err)
		return
	}
	refreshed, err := h.Orders.GetAdminStatusByParentOrder(c.Request.Context(), orderID)
	if err != nil || refreshed == nil {
		ginutil.RespondError(c, response.CodeInternal, "error.order_fetch_failed", nil)
		return
	}
	response.Success(c, refreshed)
}

func (h *AdminHandler) MarkOrderReview(c *gin.Context) {
	if h == nil || h.Orders == nil {
		ginutil.RespondError(c, response.CodeNotFound, "error.order_not_found", nil)
		return
	}
	orderID, ok := parseUintParam(c, "id")
	if !ok {
		ginutil.RespondError(c, response.CodeBadRequest, "error.bad_request", nil)
		return
	}
	var req struct {
		Remark string `json:"remark"`
	}
	if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
		ginutil.RespondError(c, response.CodeBadRequest, "error.bad_request", nil)
		return
	}
	updated, err := h.Orders.MarkManualReview(c.Request.Context(), orderID, req.Remark)
	if err != nil {
		if errors.Is(err, quefadomain.ErrOrderNotFound) {
			ginutil.RespondError(c, response.CodeNotFound, "error.order_not_found", nil)
			return
		}
		ginutil.RespondError(c, response.CodeInternal, "error.order_update_failed", nil)
		return
	}
	response.Success(c, updated)
}

func parseUintParam(c *gin.Context, name string) (uint, bool) {
	raw := strings.TrimSpace(c.Param(name))
	v, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || v == 0 {
		return 0, false
	}
	return uint(v), true
}

func respondQuefaConfigError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, quefadomain.ErrPaymentDomainForbidden):
		ginutil.RespondError(c, response.CodeBadRequest, "error.quefa_payment_domain_forbidden", nil)
	case errors.Is(err, quefadomain.ErrPaymentDomainNotAllowed):
		ginutil.RespondError(c, response.CodeBadRequest, "error.quefa_payment_domain_invalid", nil)
	case errors.Is(err, quefaapp.ErrConfigBaseURLInvalid), errors.Is(err, quefaapp.ErrConfigInvalid):
		ginutil.RespondError(c, response.CodeBadRequest, "error.quefa_config_invalid", nil)
	default:
		ginutil.RespondError(c, response.CodeInternal, "error.quefa_config_save_failed", nil)
	}
}

func respondQuefaUpstreamError(c *gin.Context, err error) {
	locale := i18n.ResolveLocale(c)
	switch {
	case errors.Is(err, quefaapp.ErrGatewayNotConfigured):
		ginutil.RespondError(c, response.CodeBadRequest, "error.quefa_not_configured", err)
	case errors.Is(err, quefaapp.ErrConfigBaseURLInvalid), errors.Is(err, quefaapp.ErrConfigInvalid):
		ginutil.RespondError(c, response.CodeBadRequest, "error.quefa_config_invalid", err)
	case quefacontract.IsAPIErrorCategory(err, "auth"), quefacontract.IsAPIErrorCategory(err, "forbidden"):
		base := i18n.T(locale, "error.quefa_upstream_auth_failed")
		if detail := formatAdminUpstreamDetail(err); detail != "" {
			ginutil.RespondErrorWithMsg(c, response.CodeBadRequest, base+" ("+detail+")", err)
			return
		}
		ginutil.RespondError(c, response.CodeBadRequest, "error.quefa_upstream_auth_failed", err)
	case quefacontract.IsAPIErrorCategory(err, "rate_limited"):
		ginutil.RespondError(c, response.CodeTooManyRequests, "error.quefa_rate_limited", err)
	default:
		base := i18n.T(locale, "error.quefa_upstream_failed")
		if detail := formatAdminUpstreamDetail(err); detail != "" {
			ginutil.RespondErrorWithMsg(c, response.CodeInternal, base+" ("+detail+")", err)
			return
		}
		ginutil.RespondError(c, response.CodeInternal, "error.quefa_upstream_failed", err)
	}
}

func formatAdminUpstreamDetail(err error) string {
	if err == nil {
		return ""
	}
	var apiErr *quefacontract.APIError
	if errors.As(err, &apiErr) {
		parts := make([]string, 0, 3)
		if apiErr.StatusCode > 0 {
			parts = append(parts, fmt.Sprintf("HTTP %d", apiErr.StatusCode))
		} else if apiErr.Category != "" {
			parts = append(parts, apiErr.Category)
		}
		if apiErr.Code != "" {
			parts = append(parts, apiErr.Code)
		}
		if apiErr.Message != "" {
			parts = append(parts, apiErr.Message)
		}
		return strings.Join(parts, ": ")
	}
	var parseErr *quefaapp.CatalogParseError
	if errors.As(err, &parseErr) {
		return parseErr.Error()
	}
	if errors.Is(err, quefadomain.ErrCatalogEmpty) || errors.Is(err, quefadomain.ErrCatalogInvalid) {
		return err.Error()
	}
	return strings.TrimSpace(err.Error())
}
