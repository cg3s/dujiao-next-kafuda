package orderhttp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	orderdomain "github.com/dujiao-next/internal/modules/order/domain"

	orderpresenter "github.com/dujiao-next/internal/modules/order/transport/presenter"
	quefacontract "github.com/dujiao-next/internal/modules/quefa/contract"
	reseller "github.com/dujiao-next/internal/modules/reseller/contract"
	"github.com/dujiao-next/internal/platform/http/ginutil"
	"github.com/dujiao-next/internal/platform/http/response"

	"github.com/gin-gonic/gin"
)

type GuestDirectStatusReader func(ctx context.Context, orderNo, email, password string) (*DirectStatus, error)
type GuestDirectAuthorizer func(ctx context.Context, orderNo, email, password, session string) error
type GuestHostedRechargeStatusReader func(ctx context.Context, orderNo, email, password string) (*quefacontract.BuyerStatus, error)
type GuestHostedRechargeEntryOpener func(ctx context.Context, orderNo, email, password string) (*quefacontract.FulfillmentEntryResult, error)

// GuestOrderQuery 游客订单只读端口。
type GuestOrderQuery interface {
	ListOrdersByGuestForTenant(tenant reseller.TenantContext, email, password string, page, pageSize int) ([]orderdomain.Order, int64, error)
	GetOrderByGuestOrderNoForTenant(tenant reseller.TenantContext, orderNo, email, password string) (*orderdomain.Order, error)
	GetAnyOrderByGuestOrderNoForTenant(tenant reseller.TenantContext, orderNo, email, password string) (*orderdomain.Order, error)
}

// GuestHandler 处理前台游客订单只读 HTTP。
type GuestHandler struct {
	orders          GuestOrderQuery
	payments        PaymentChannelPolicy
	refunds         RefundRecordDirectory
	direct          GuestDirectStatusReader
	authorizeDirect GuestDirectAuthorizer
	hostedStatus    GuestHostedRechargeStatusReader
	hostedEntry     GuestHostedRechargeEntryOpener
}

func (h *GuestHandler) SetHostedRecharge(status GuestHostedRechargeStatusReader, entry GuestHostedRechargeEntryOpener) {
	if h != nil {
		h.hostedStatus = status
		h.hostedEntry = entry
	}
}

func (h *GuestHandler) GetGuestHostedRechargeStatus(c *gin.Context) {
	email, password, ok := ginutil.GetGuestCredentials(c)
	if !ok || email == "" {
		ginutil.RespondError(c, response.CodeBadRequest, "error.guest_email_required", nil)
		return
	}
	if password == "" {
		ginutil.RespondError(c, response.CodeBadRequest, "error.guest_password_required", nil)
		return
	}
	tenant := tenantFromRequest(c)
	orderNo := strings.TrimSpace(c.Param("order_no"))
	if h.hostedStatus == nil || orderNo == "" || !tenant.IsMain || tenant.ResellerID != nil || tenant.Unavailable {
		ginutil.RespondError(c, response.CodeNotFound, "error.guest_order_not_found", nil)
		return
	}
	st, err := h.hostedStatus(c.Request.Context(), orderNo, email, password)
	if err != nil {
		ginutil.RespondError(c, response.CodeInternal, "error.order_fetch_failed", nil)
		return
	}
	if st == nil {
		ginutil.RespondError(c, response.CodeNotFound, "error.guest_order_not_found", nil)
		return
	}
	response.Success(c, st)
}

func (h *GuestHandler) OpenGuestHostedRechargeEntry(c *gin.Context) {
	email, password, ok := ginutil.GetGuestCredentials(c)
	if !ok || email == "" {
		ginutil.RespondError(c, response.CodeBadRequest, "error.guest_email_required", nil)
		return
	}
	if password == "" {
		ginutil.RespondError(c, response.CodeBadRequest, "error.guest_password_required", nil)
		return
	}
	tenant := tenantFromRequest(c)
	orderNo := strings.TrimSpace(c.Param("order_no"))
	if h.hostedEntry == nil || orderNo == "" || !tenant.IsMain || tenant.ResellerID != nil || tenant.Unavailable {
		ginutil.RespondError(c, response.CodeNotFound, "error.guest_order_not_found", nil)
		return
	}
	c.Header("Cache-Control", "no-store")
	entry, err := h.hostedEntry(c.Request.Context(), orderNo, email, password)
	if err != nil {
		if errors.Is(err, ErrOrderNotFound) || errors.Is(err, ErrGuestOrderNotFound) {
			ginutil.RespondError(c, response.CodeNotFound, "error.guest_order_not_found", nil)
			return
		}
		ginutil.RespondError(c, response.CodeBadRequest, "error.hosted_recharge_entry_unavailable", nil)
		return
	}
	response.Success(c, entry)
}

func NewGuestHandler(orders GuestOrderQuery, payments PaymentChannelPolicy, refunds RefundRecordDirectory) *GuestHandler {
	if orders == nil {
		panic("order guest handler: orders is nil")
	}
	return &GuestHandler{orders: orders, payments: payments, refunds: refunds}
}

func (h *GuestHandler) SetDirectStatusReader(reader GuestDirectStatusReader) {
	h.direct = reader
}

func (h *GuestHandler) SetDirectAuthorizer(authorize GuestDirectAuthorizer) {
	h.authorizeDirect = authorize
}

func (h *GuestHandler) GetGuestDirectStatus(c *gin.Context) {
	email, password, ok := ginutil.GetGuestCredentials(c)
	if !ok || email == "" {
		ginutil.RespondError(c, response.CodeBadRequest, "error.guest_email_required", nil)
		return
	}
	if password == "" {
		ginutil.RespondError(c, response.CodeBadRequest, "error.guest_password_required", nil)
		return
	}
	tenant := tenantFromRequest(c)
	orderNo := strings.TrimSpace(c.Param("order_no"))
	if h.direct == nil || orderNo == "" || !tenant.IsMain || tenant.ResellerID != nil || tenant.Unavailable {
		ginutil.RespondError(c, response.CodeNotFound, "error.guest_order_not_found", nil)
		return
	}
	status, err := h.direct(c.Request.Context(), orderNo, email, password)
	if err != nil {
		ginutil.RespondError(c, response.CodeInternal, "error.order_fetch_failed", nil)
		return
	}
	if status == nil {
		ginutil.RespondError(c, response.CodeNotFound, "error.guest_order_not_found", nil)
		return
	}
	response.Success(c, status)
}

func (h *GuestHandler) AuthorizeGuestDirect(c *gin.Context) {
	email, password, ok := ginutil.GetGuestCredentials(c)
	if !ok || email == "" {
		ginutil.RespondError(c, response.CodeBadRequest, "error.guest_email_required", nil)
		return
	}
	if password == "" {
		ginutil.RespondError(c, response.CodeBadRequest, "error.guest_password_required", nil)
		return
	}
	tenant := tenantFromRequest(c)
	orderNo := strings.TrimSpace(c.Param("order_no"))
	if h.authorizeDirect == nil || orderNo == "" || len(orderNo) > 64 || !tenant.IsMain || tenant.ResellerID != nil || tenant.Unavailable {
		ginutil.RespondError(c, response.CodeNotFound, "error.guest_order_not_found", nil)
		return
	}
	c.Header("Cache-Control", "no-store")
	decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 32<<10))
	decoder.DisallowUnknownFields()
	var input struct {
		Session string `json:"session"`
	}
	decodeErr := decoder.Decode(&input)
	if decodeErr == nil {
		decodeErr = decoder.Decode(new(json.RawMessage))
	} else if decodeErr == io.EOF {
		decodeErr = io.ErrUnexpectedEOF
	}
	if decodeErr != io.EOF {
		key := "error.bad_request"
		var oversized *http.MaxBytesError
		if errors.As(decodeErr, &oversized) {
			key = "error.request_too_large"
		}
		ginutil.RespondError(c, response.CodeBadRequest, key, nil)
		return
	}
	session := buyerSessionToken(input.Session)
	if session == "" {
		ginutil.RespondError(c, response.CodeBadRequest, "error.direct_session_invalid", nil)
		return
	}
	err := h.authorizeDirect(c.Request.Context(), orderNo, email, password, session)
	switch {
	case err == nil:
		response.Success(c, nil)
	case errors.Is(err, ErrOrderNotFound), errors.Is(err, ErrGuestOrderNotFound):
		ginutil.RespondError(c, response.CodeNotFound, "error.guest_order_not_found", nil)
	case errors.Is(err, ErrOrderStatusInvalid):
		ginutil.RespondError(c, response.CodeBadRequest, "error.order_status_invalid", nil)
	case errors.Is(err, ErrDirectSessionInvalid):
		ginutil.RespondError(c, response.CodeBadRequest, "error.direct_session_invalid", nil)
	case errors.Is(err, ErrDirectRetry):
		ginutil.RespondError(c, response.CodeBadRequest, "error.direct_retry", nil)
	default:
		ginutil.RespondError(c, response.CodeInternal, "error.direct_check_progress", nil)
	}
}

// ListGuestOrders 获取游客订单列表
func (h *GuestHandler) ListGuestOrders(c *gin.Context) {
	email, password, ok := ginutil.GetGuestCredentials(c)
	orderNo := strings.TrimSpace(c.Query("order_no"))
	if !ok || email == "" {
		ginutil.RespondError(c, response.CodeBadRequest, "error.guest_email_required", nil)
		return
	}
	if password == "" {
		ginutil.RespondError(c, response.CodeBadRequest, "error.guest_password_required", nil)
		return
	}

	if orderNo != "" {
		order, err := h.orders.GetOrderByGuestOrderNoForTenant(tenantFromRequest(c), orderNo, email, password)
		if err != nil {
			if errors.Is(err, ErrGuestOrderNotFound) {
				pagination := response.Pagination{
					Page:      1,
					PageSize:  1,
					Total:     0,
					TotalPage: 1,
				}
				response.SuccessWithPage(c, []orderdomain.Order{}, pagination)
				return
			}
			ginutil.RespondError(c, response.CodeInternal, "error.order_fetch_failed", err)
			return
		}
		pagination := response.Pagination{
			Page:      1,
			PageSize:  1,
			Total:     1,
			TotalPage: 1,
		}
		response.SuccessWithPage(c, orderpresenter.NewOrderSummaryList([]orderdomain.Order{*order}), pagination)
		return
	}

	page, pageSize := ginutil.ParsePagination(c)

	orders, total, err := h.orders.ListOrdersByGuestForTenant(tenantFromRequest(c), email, password, page, pageSize)
	if err != nil {
		ginutil.RespondError(c, response.CodeInternal, "error.order_fetch_failed", err)
		return
	}
	pagination := response.BuildPagination(page, pageSize, total)
	response.SuccessWithPage(c, orderpresenter.NewOrderSummaryList(orders), pagination)
}

// GetGuestOrderByOrderNo 按订单号获取游客订单详情
func (h *GuestHandler) GetGuestOrderByOrderNo(c *gin.Context) {
	email, password, ok := ginutil.GetGuestCredentials(c)
	if !ok || email == "" {
		ginutil.RespondError(c, response.CodeBadRequest, "error.guest_email_required", nil)
		return
	}
	if password == "" {
		ginutil.RespondError(c, response.CodeBadRequest, "error.guest_password_required", nil)
		return
	}
	orderNo := strings.TrimSpace(c.Param("order_no"))
	if orderNo == "" {
		ginutil.RespondError(c, response.CodeBadRequest, "error.order_item_invalid", nil)
		return
	}
	order, err := h.orders.GetOrderByGuestOrderNoForTenant(tenantFromRequest(c), orderNo, email, password)
	if err != nil {
		if errors.Is(err, ErrGuestOrderNotFound) {
			ginutil.RespondError(c, response.CodeNotFound, "error.guest_order_not_found", nil)
			return
		}
		ginutil.RespondError(c, response.CodeInternal, "error.order_fetch_failed", err)
		return
	}
	orderDetail := orderpresenter.NewOrderDetailTruncated(order)
	enrichOrderWithAllowedChannels(h.payments, order, &orderDetail)
	enrichOrderWithRefundRecords(h.refunds, order, &orderDetail)
	response.Success(c, orderDetail)
}

// DownloadGuestFulfillment 下载订单交付内容（游客）
// 支持父订单或子订单的 order_no
func (h *GuestHandler) DownloadGuestFulfillment(c *gin.Context) {
	email, password, ok := ginutil.GetGuestCredentials(c)
	if !ok || email == "" || password == "" {
		ginutil.RespondError(c, response.CodeBadRequest, "error.guest_email_required", nil)
		return
	}
	orderNo := strings.TrimSpace(c.Param("order_no"))
	if orderNo == "" {
		ginutil.RespondError(c, response.CodeBadRequest, "error.order_item_invalid", nil)
		return
	}
	order, err := h.orders.GetAnyOrderByGuestOrderNoForTenant(tenantFromRequest(c), orderNo, email, password)
	if err != nil {
		if errors.Is(err, ErrGuestOrderNotFound) {
			ginutil.RespondError(c, response.CodeNotFound, "error.guest_order_not_found", nil)
			return
		}
		ginutil.RespondError(c, response.CodeInternal, "error.order_fetch_failed", err)
		return
	}
	if order == nil {
		ginutil.RespondError(c, response.CodeNotFound, "error.guest_order_not_found", nil)
		return
	}
	respondFulfillmentDownload(c, order)
}
