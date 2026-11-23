package orderwiring

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/dujiao-next/internal/app/container"
	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"

	paymentapp "github.com/dujiao-next/internal/modules/payment/application"
	paymentcontract "github.com/dujiao-next/internal/modules/payment/contract"

	paymentdomain "github.com/dujiao-next/internal/modules/payment/domain"

	orderapp "github.com/dujiao-next/internal/modules/order/application"
	orderrefund "github.com/dujiao-next/internal/modules/order/application/refund"
	ordercontract "github.com/dujiao-next/internal/modules/order/contract"
	orderdomain "github.com/dujiao-next/internal/modules/order/domain"

	walletdomain "github.com/dujiao-next/internal/modules/wallet/domain"

	coupondomain "github.com/dujiao-next/internal/modules/coupon/domain"

	promotiondomain "github.com/dujiao-next/internal/modules/promotion/domain"

	usercontract "github.com/dujiao-next/internal/modules/identity/user/contract"

	userdomain "github.com/dujiao-next/internal/modules/identity/user/domain"

	"github.com/dujiao-next/internal/constants"
	"github.com/dujiao-next/internal/logger"
	captchaapp "github.com/dujiao-next/internal/modules/captcha/application"
	captchahttp "github.com/dujiao-next/internal/modules/captcha/transport/http"
	couponcontract "github.com/dujiao-next/internal/modules/coupon/contract"
	ordertransport "github.com/dujiao-next/internal/modules/order/transport/http"
	orderriskcontract "github.com/dujiao-next/internal/modules/orderrisk/contract"
	promotioncontract "github.com/dujiao-next/internal/modules/promotion/contract"
	reseller "github.com/dujiao-next/internal/modules/reseller/contract"
	settingsstorefront "github.com/dujiao-next/internal/modules/settings/schema/storefront"
	walletcontract "github.com/dujiao-next/internal/modules/wallet/contract"
	"github.com/dujiao-next/internal/queue"
	"github.com/dujiao-next/internal/shared/money"
	"github.com/shopspring/decimal"
)

func newDirectStatusReader(c *container.Container) ordertransport.DirectStatusReader {
	if c.OrderStore == nil || c.KafudaGPT == nil || c.KafudaGPT.Store == nil {
		return nil
	}
	return func(ctx context.Context, orderNo string, userID uint) (*ordertransport.DirectStatus, error) {
		if userID == 0 {
			return nil, nil
		}
		order, err := c.OrderStore.GetByOrderNoAndUserScoped(orderNo, userID, ordercontract.TenantScope{})
		if err != nil || order == nil || order.UserID != userID {
			return nil, err
		}
		return readDirectStatusForOrder(ctx, c.KafudaGPT.Store, order)
	}
}

func newGuestDirectStatusReader(c *container.Container) ordertransport.GuestDirectStatusReader {
	if c.OrderStore == nil || c.KafudaGPT == nil || c.KafudaGPT.Store == nil {
		return nil
	}
	return func(ctx context.Context, orderNo, email, password string) (*ordertransport.DirectStatus, error) {
		order, err := c.OrderStore.GetByOrderNoAndGuestScoped(orderNo, email, password, ordercontract.TenantScope{})
		if err != nil || order == nil || order.UserID != 0 {
			return nil, err
		}
		return readDirectStatusForOrder(ctx, c.KafudaGPT.Store, order)
	}
}

func readDirectStatusForOrder(ctx context.Context, store kafudacontract.Store, order *orderdomain.Order) (*ordertransport.DirectStatus, error) {
	if order == nil || order.ParentID != nil || order.ResellerID != nil || order.PaidAt == nil ||
		len(order.Children) != 1 {
		return nil, nil
	}
	child := order.Children[0]
	if child.ParentID == nil || *child.ParentID != order.ID || child.UserID != order.UserID || child.ResellerID != nil ||
		child.PaidAt == nil || len(child.Items) != 1 ||
		child.Items[0].FulfillmentType != constants.FulfillmentTypeKafudaGPT || child.Status != order.Status {
		return nil, nil
	}
	if order.Status == constants.OrderStatusRefunded && order.RefundedAmount.IsPositive() {
		return &ordertransport.DirectStatus{Stage: constants.OrderStatusRefunded, UpdatedAt: order.UpdatedAt}, nil
	}
	if (order.Status != constants.OrderStatusPaid && order.Status != constants.OrderStatusFulfilling && order.Status != constants.OrderStatusCompleted) ||
		!order.RefundedAmount.IsZero() || !child.RefundedAmount.IsZero() {
		return nil, nil
	}
	record, err := store.GetRecharge(ctx, child.ID)
	if err != nil || record == nil {
		return nil, err
	}
	stage := record.Stage
	switch stage {
	case kafudadomain.StageWaiting, kafudadomain.StageChecking, kafudadomain.StageSubmitting,
		kafudadomain.StageProcessing, kafudadomain.StageManualReview, kafudadomain.StageCompleted:
	case kafudadomain.StageManualDelivery:
		stage = kafudadomain.StageManualReview
	default:
		return nil, nil
	}
	status := &ordertransport.DirectStatus{Stage: string(stage), UpdatedAt: record.UpdatedAt}
	// Completion commits the recharge, fulfillment and shop order statuses
	// together. The order read above may have occurred before that commit.
	if stage == kafudadomain.StageCompleted && record.Snapshot.Product == "gpt" && record.CurrentAttemptID != nil {
		attempt, err := store.GetAttempt(ctx, *record.CurrentAttemptID)
		if err != nil {
			return nil, err
		}
		if attempt != nil && attempt.OrderID == child.ID && attempt.Status == kafudadomain.AttemptCompleted && attempt.UpstreamOrderID != nil {
			status.RenewalCancelled = attempt.RenewalCancelled
		}
	}
	return status, nil
}

type orderAdminQueryAdapter struct {
	orders *orderapp.OrderService
}

func (a orderAdminQueryAdapter) ListOrdersForAdmin(filter ordertransport.OrderListFilter) ([]orderdomain.Order, int64, error) {
	return a.orders.ListOrdersForAdmin(ordercontract.ListFilter{
		Page:           filter.Page,
		PageSize:       filter.PageSize,
		UserID:         filter.UserID,
		UserKeyword:    filter.UserKeyword,
		Status:         filter.Status,
		OrderNo:        filter.OrderNo,
		GuestEmail:     filter.GuestEmail,
		ProductKeyword: filter.ProductKeyword,
		CreatedFrom:    filter.CreatedFrom,
		CreatedTo:      filter.CreatedTo,
		SortBy:         filter.SortBy,
		SortOrder:      filter.SortOrder,
		DirectReview:   filter.DirectReview,
	})
}

func newAdminDirectStatusReader(c *container.Container) ordertransport.AdminDirectStatusReader {
	if c.KafudaGPT == nil || c.KafudaGPT.Store == nil {
		return nil
	}
	store := c.KafudaGPT.Store
	return func(ctx context.Context, order *orderdomain.Order) (*ordertransport.AdminDirectStatus, error) {
		child := order
		if order.ParentID == nil {
			if len(order.Children) != 1 || order.Children[0].ParentID == nil || *order.Children[0].ParentID != order.ID {
				return nil, nil
			}
			child = &order.Children[0]
		}
		if len(child.Items) != 1 || !child.HasDirectRecharge() {
			return nil, nil
		}
		record, err := store.GetRecharge(ctx, child.ID)
		if err != nil || record == nil {
			return nil, err
		}
		switch record.Stage {
		case kafudadomain.StageWaiting, kafudadomain.StageChecking, kafudadomain.StageSubmitting, kafudadomain.StageProcessing,
			kafudadomain.StageManualReview, kafudadomain.StageManualDelivery, kafudadomain.StageCompleted:
		default:
			return nil, nil
		}
		status := &ordertransport.AdminDirectStatus{ChildID: child.ID, Stage: string(record.Stage), UpdatedAt: record.UpdatedAt}
		switch record.ReasonCode {
		case "check_rejected", "session_invalid", "outcome_unknown", "upstream_review", "upstream_failed", "upstream_cancelled",
			kafudadomain.ReasonQuoteRegionLocked, kafudadomain.ReasonQuoteExpired, kafudadomain.ReasonQuoteUnavailable, kafudadomain.ReasonQuotePolicy,
			kafudadomain.ReasonCostLimitExceeded, "lookup_deadline_exceeded", kafudadomain.ReasonProcessingDeadlineExceeded,
			"check_interrupted", kafudadomain.ReasonSubmissionInterrupted, kafudadomain.ReasonResultApplyFailed, kafudadomain.ReasonMerchantBalanceInsufficient:
			status.Reason = record.ReasonCode
		}
		if record.ReasonCode == kafudadomain.ReasonCostLimitExceeded && record.Snapshot.MaxCostCents > 0 {
			limit := usdFromCents(record.Snapshot.MaxCostCents)
			status.CostLimitUSD = &limit
		}
		if record.CurrentAttemptID == nil {
			return status, nil
		}
		attempt, err := store.GetAttempt(ctx, *record.CurrentAttemptID)
		if err != nil {
			return nil, err
		}
		if attempt == nil || attempt.OrderID != child.ID {
			return status, nil
		}
		info := &ordertransport.AdminDirectAttempt{ID: attempt.ID, Sequence: attempt.Sequence, Active: attempt.Status.Active()}
		validAttempt := attempt.Validate() == nil
		if validAttempt {
			info.Reference = attempt.Reference
			if costs := attempt.QuoteCosts; costs != nil {
				info.QuoteCosts = &ordertransport.AdminDirectQuoteCosts{
					ServiceFeeUSD:  usdFromCents(costs.ServiceFeeCents),
					CardOpenFeeUSD: usdFromCents(costs.CardOpenFeeCents),
					CardLoadUSD:    usdFromCents(costs.CardLoadCents),
					TotalUSD:       usdFromCents(costs.TotalCents),
				}
			}
		}
		if attempt.UpstreamOrderID != nil && *attempt.UpstreamOrderID > 0 {
			info.UpstreamOrderID = strconv.FormatUint(*attempt.UpstreamOrderID, 10)
			if validAttempt && attempt.ChargedUSDCents != nil {
				charge := usdFromCents(*attempt.ChargedUSDCents)
				info.CardChargedUSD = &charge
			}
		}
		if kafudadomain.IsMaskedAccountEmail(attempt.MaskedEmail) {
			info.MaskedEmail = attempt.MaskedEmail
		}
		status.Attempt = info
		status.CanConfirmUpstreamCompletion = child.Fulfillment == nil && order.Status == child.Status && order.UserID == child.UserID &&
			isOpenPaidAdminDirectOrder(order) && isOpenPaidAdminDirectOrder(child) && kafudacontract.CanConfirmUpstreamCompletion(record, attempt)
		return status, nil
	}
}

func isOpenPaidAdminDirectOrder(order *orderdomain.Order) bool {
	return order.ResellerID == nil && order.UserID != 0 && order.PaidAt != nil && order.RefundedAmount.IsZero() &&
		(order.Status == constants.OrderStatusPaid || order.Status == constants.OrderStatusFulfilling)
}

func usdFromCents(cents int64) money.Amount {
	return money.FromDecimal(decimal.New(cents, -2))
}

func (a orderAdminQueryAdapter) GetOrderForAdmin(orderID uint) (*orderdomain.Order, error) {
	order, err := a.orders.GetOrderForAdmin(orderID)
	return order, mapOrderTransportError(err)
}

func (a orderAdminQueryAdapter) UpdateOrderStatus(orderID uint, status string) (*orderdomain.Order, error) {
	order, err := a.orders.UpdateOrderStatus(orderID, status)
	return order, mapOrderTransportError(err)
}

type orderAdminUserAdapter struct {
	users usercontract.Store
}

func (a orderAdminUserAdapter) ListByIDs(ids []uint) ([]userdomain.User, error) {
	return a.users.ListByIDs(ids)
}

func (a orderAdminUserAdapter) GetByID(id uint) (*userdomain.User, error) {
	return a.users.GetByID(id)
}

type orderAdminCouponAdapter struct {
	coupons couponcontract.Repository
}

func (a orderAdminCouponAdapter) GetByID(id uint) (*coupondomain.Coupon, error) {
	return a.coupons.GetByID(id)
}

type orderAdminPromotionAdapter struct {
	promotions promotioncontract.Repository
}

func (a orderAdminPromotionAdapter) GetByID(id uint) (*promotiondomain.Promotion, error) {
	return a.promotions.GetByID(id)
}

type orderAdminPaymentAdapter struct {
	payments paymentcontract.Store
}

func (a orderAdminPaymentAdapter) ListByOrderID(orderID uint) ([]paymentdomain.Payment, error) {
	return a.payments.ListByOrderID(orderID)
}

type orderAdminPaymentChannelAdapter struct {
	channels paymentcontract.ChannelStore
}

func (a orderAdminPaymentChannelAdapter) ListByIDs(ids []uint) ([]paymentdomain.PaymentChannel, error) {
	return a.channels.ListByIDs(ids)
}

// orderAdminDashboardSettingReader 是订单适配器读取仪表盘统计口径所需的最小设置接口。
type orderAdminDashboardSettingReader interface {
	GetDashboardSetting() (settingsstorefront.DashboardSetting, error)
}

type orderAdminAccountingAdapter struct {
	settings orderAdminDashboardSettingReader
}

// RefundReversesCost 读取仪表盘“退款冲回成本”开关；读取失败时按默认（关闭）处理，不影响订单详情。
func (a orderAdminAccountingAdapter) RefundReversesCost() bool {
	setting, err := a.settings.GetDashboardSetting()
	if err != nil {
		logger.Warnw("order_admin_load_dashboard_setting_failed", "error", err)
		return false
	}
	return setting.Accounting.RefundReversesCost
}

type orderUserQueryAdapter struct {
	orders *orderapp.OrderService
}

func (a orderUserQueryAdapter) ListOrdersByUserForTenant(tenant reseller.TenantContext, filter ordertransport.UserOrderListFilter) ([]orderdomain.Order, int64, error) {
	return a.orders.ListOrdersByUserForTenant(tenant, ordercontract.ListFilter{
		Page:     filter.Page,
		PageSize: filter.PageSize,
		UserID:   filter.UserID,
		Status:   filter.Status,
		OrderNo:  filter.OrderNo,
	})
}

func (a orderUserQueryAdapter) StatsOrdersByUserForTenant(tenant reseller.TenantContext, filter ordertransport.UserOrderListFilter) (map[string]int64, error) {
	return a.orders.StatsOrdersByUserForTenant(tenant, ordercontract.ListFilter{
		UserID:  filter.UserID,
		OrderNo: filter.OrderNo,
	})
}

func (a orderUserQueryAdapter) GetOrderByUserOrderNoForTenant(tenant reseller.TenantContext, orderNo string, userID uint) (*orderdomain.Order, error) {
	order, err := a.orders.GetOrderByUserOrderNoForTenant(tenant, orderNo, userID)
	return order, mapOrderTransportError(err)
}

func (a orderUserQueryAdapter) GetAnyOrderByUserOrderNoForTenant(tenant reseller.TenantContext, orderNo string, userID uint) (*orderdomain.Order, error) {
	order, err := a.orders.GetAnyOrderByUserOrderNoForTenant(tenant, orderNo, userID)
	return order, mapOrderTransportError(err)
}

func (a orderUserQueryAdapter) CancelOrder(orderID uint, userID uint) (*orderdomain.Order, error) {
	order, err := a.orders.CancelOrder(orderID, userID)
	return order, mapOrderTransportError(err)
}

type orderUserPaymentChannelAdapter struct {
	payments *paymentapp.PaymentService
}

func (a orderUserPaymentChannelAdapter) GetAllowedChannelIDsForOrder(items []orderdomain.OrderItem) []uint {
	if a.payments == nil {
		return nil
	}
	return a.payments.GetAllowedChannelIDsForOrder(items)
}

func (a orderUserPaymentChannelAdapter) GetAvailableChannels(filter ordertransport.AvailablePaymentChannelFilter) ([]map[string]interface{}, error) {
	if a.payments == nil {
		return nil, nil
	}
	return a.payments.GetAvailableChannels(paymentapp.AvailablePaymentChannelFilter{
		TargetAmount: filter.TargetAmount,
		User:         filter.User,
		PaymentType:  filter.PaymentType,
	})
}

type orderUserLookupAdapter struct {
	users usercontract.Store
}

func (a orderUserLookupAdapter) GetByID(id uint) (*userdomain.User, error) {
	return a.users.GetByID(id)
}

type orderUserRefundRecordAdapter struct {
	records ordercontract.Store
}

func (a orderUserRefundRecordAdapter) ListByOrderIDs(orderIDs []uint) ([]orderdomain.OrderRefundRecord, error) {
	return a.records.ListRefundRecordsByOrderIDs(orderIDs)
}

type orderGuestQueryAdapter struct {
	orders *orderapp.OrderService
}

func (a orderGuestQueryAdapter) ListOrdersByGuestForTenant(tenant reseller.TenantContext, email, password string, page, pageSize int) ([]orderdomain.Order, int64, error) {
	return a.orders.ListOrdersByGuestForTenant(tenant, email, password, page, pageSize)
}

func (a orderGuestQueryAdapter) GetOrderByGuestOrderNoForTenant(tenant reseller.TenantContext, orderNo, email, password string) (*orderdomain.Order, error) {
	order, err := a.orders.GetOrderByGuestOrderNoForTenant(tenant, orderNo, email, password)
	return order, mapOrderTransportError(err)
}

func (a orderGuestQueryAdapter) GetAnyOrderByGuestOrderNoForTenant(tenant reseller.TenantContext, orderNo, email, password string) (*orderdomain.Order, error) {
	order, err := a.orders.GetAnyOrderByGuestOrderNoForTenant(tenant, orderNo, email, password)
	return order, mapOrderTransportError(err)
}

type orderAdminRefundAdapter struct {
	refunds *orderrefund.Service
}

func (a orderAdminRefundAdapter) ListAdminRefundItems(query ordertransport.AdminRefundListQuery) ([]ordertransport.AdminRefundItem, int64, error) {
	items, total, err := a.refunds.ListAdminRefundItems(orderrefund.AdminOrderRefundListQuery{
		Page:           query.Page,
		PageSize:       query.PageSize,
		UserID:         query.UserID,
		UserKeyword:    query.UserKeyword,
		OrderNo:        query.OrderNo,
		GuestEmail:     query.GuestEmail,
		ProductKeyword: query.ProductKeyword,
		ProductName:    query.ProductName,
		CreatedFrom:    query.CreatedFrom,
		CreatedTo:      query.CreatedTo,
	})
	if err != nil {
		return nil, 0, mapOrderTransportError(err)
	}
	return mapAdminRefundItems(items), total, nil
}

func (a orderAdminRefundAdapter) GetAdminRefundItem(id uint) (*ordertransport.AdminRefundItem, error) {
	item, err := a.refunds.GetAdminRefundItem(id)
	if err != nil {
		return nil, mapOrderTransportError(err)
	}
	mapped := mapAdminRefundItem(*item)
	return &mapped, nil
}

func (a orderAdminRefundAdapter) ParseRefundAmount(raw string) (money.Amount, error) {
	amount, err := a.refunds.ParseRefundAmount(raw)
	return amount, mapOrderTransportError(err)
}

func (a orderAdminRefundAdapter) AdminManualRefund(input ordertransport.AdminManualRefundInput) (*orderdomain.Order, *orderdomain.OrderRefundRecord, error) {
	order, record, err := a.refunds.AdminManualRefund(orderrefund.AdminManualRefundInput{
		DirectAttemptID:    input.DirectAttemptID,
		OrderID:            input.OrderID,
		Amount:             input.Amount,
		Remark:             input.Remark,
		PaymentFeeRefunded: input.PaymentFeeRefunded,
	})
	return order, record, mapOrderTransportError(err)
}

func (a orderAdminRefundAdapter) UpdatePaymentFeeRefunded(input ordertransport.AdminUpdateRefundPaymentFeeInput) (*orderdomain.OrderRefundRecord, error) {
	record, err := a.refunds.UpdatePaymentFeeRefunded(orderrefund.UpdatePaymentFeeRefundedInput{
		RefundRecordID:     input.RefundRecordID,
		PaymentFeeRefunded: input.PaymentFeeRefunded,
	})
	return record, mapOrderTransportError(err)
}

type orderAdminWalletRefundAdapter struct {
	refunds *orderrefund.Service
}

func (a orderAdminWalletRefundAdapter) AdminRefundToWallet(input ordertransport.AdminRefundToWalletInput) (*orderdomain.Order, *walletdomain.Transaction, *orderdomain.OrderRefundRecord, error) {
	order, txn, record, err := a.refunds.AdminRefundToWallet(orderrefund.AdminRefundToWalletInput{
		DirectAttemptID: input.DirectAttemptID,
		OrderID:         input.OrderID,
		Amount:          input.Amount,
		Remark:          input.Remark,
	})
	return order, txn, record, mapOrderTransportError(err)
}

type orderAdminOrderLookupAdapter struct {
	orders ordercontract.Store
}

func (a orderAdminOrderLookupAdapter) GetByID(id uint) (*orderdomain.Order, error) {
	return a.orders.GetByID(id)
}

type orderAdminStatusEmailAdapter struct {
	queue *queue.Client
}

func (a orderAdminStatusEmailAdapter) EnqueueOrderStatusEmail(orderID uint, status string, refundRecordID uint) error {
	if a.queue == nil {
		return nil
	}
	return a.queue.EnqueueOrderStatusEmail(queue.OrderStatusEmailPayload{
		OrderID:        orderID,
		Status:         status,
		RefundRecordID: refundRecordID,
	})
}

func mapAdminRefundItems(items []orderrefund.AdminOrderRefundItem) []ordertransport.AdminRefundItem {
	out := make([]ordertransport.AdminRefundItem, 0, len(items))
	for _, item := range items {
		out = append(out, mapAdminRefundItem(item))
	}
	return out
}

func mapAdminRefundItem(item orderrefund.AdminOrderRefundItem) ordertransport.AdminRefundItem {
	return ordertransport.AdminRefundItem{
		OrderRefundRecord: item.OrderRefundRecord,
		OrderNo:           item.OrderNo,
		GuestLocale:       item.GuestLocale,
		Items:             item.Items,
		UserEmail:         item.UserEmail,
		UserDisplayName:   item.UserDisplayName,
		RefundTypeLabel:   item.RefundTypeLabel,
	}
}

type orderPreviewAdapter struct {
	orders *orderapp.OrderService
}

type orderCreateAdapter struct {
	orders *orderapp.OrderService
}

func (a orderCreateAdapter) CreateOrder(input ordertransport.CreateOrderInput) (*orderdomain.Order, error) {
	order, err := a.orders.CreateOrder(orderapp.CreateOrderInput{
		StorefrontRequest:   true,
		UserID:              input.UserID,
		Tenant:              input.Tenant,
		Items:               mapServiceOrderItems(input.Items),
		CouponCode:          input.CouponCode,
		AffiliateCode:       input.AffiliateCode,
		AffiliateVisitorKey: input.AffiliateVisitorKey,
		ClientIP:            input.ClientIP,
		ManualFormData:      input.ManualFormData,
	})
	return order, mapOrderTransportError(err)
}

func (a orderCreateAdapter) CreateGuestOrder(input ordertransport.CreateGuestOrderInput) (*orderdomain.Order, error) {
	order, err := a.orders.CreateGuestOrder(orderapp.CreateGuestOrderInput{
		StorefrontRequest:   true,
		Email:               input.Email,
		OrderPassword:       input.OrderPassword,
		Locale:              input.Locale,
		Tenant:              input.Tenant,
		Items:               mapServiceOrderItems(input.Items),
		CouponCode:          input.CouponCode,
		AffiliateCode:       input.AffiliateCode,
		AffiliateVisitorKey: input.AffiliateVisitorKey,
		ClientIP:            input.ClientIP,
		ManualFormData:      input.ManualFormData,
	})
	return order, mapOrderTransportError(err)
}

type orderGuestCreateCaptchaAdapter struct {
	captcha *captchaapp.Service
}

func (a orderGuestCreateCaptchaAdapter) VerifyGuestCreateOrder(payload captchahttp.CaptchaPayloadRequest, clientIP string) error {
	if a.captcha == nil {
		return nil
	}
	return mapOrderTransportError(a.captcha.Verify(constants.CaptchaSceneGuestCreateOrder, payload.ToCaptchaPayload(), clientIP))
}

type orderPaymentCreatorAdapter struct {
	payments *paymentapp.PaymentService
}

func (a orderPaymentCreatorAdapter) CreatePayment(input ordertransport.CreatePaymentInput) (*ordertransport.CreatePaymentResult, error) {
	if a.payments == nil {
		return nil, paymentapp.ErrPaymentInvalid
	}
	result, err := a.payments.CreatePayment(paymentapp.CreatePaymentInput{
		OrderID:       input.OrderID,
		ChannelID:     input.ChannelID,
		UseBalance:    input.UseBalance,
		ClientIP:      input.ClientIP,
		Context:       input.Context,
		RequestScheme: input.RequestScheme,
	})
	if err != nil {
		// create-and-pay returns payment_error as plain string; keep original error text.
		return nil, err
	}
	if result == nil {
		return nil, nil
	}
	return &ordertransport.CreatePaymentResult{
		Payment:          result.Payment,
		OrderPaid:        result.OrderPaid,
		WalletPaidAmount: result.WalletPaidAmount,
		OnlinePayAmount:  result.OnlinePayAmount,
	}, nil
}

func (a orderPreviewAdapter) PreviewOrder(input ordertransport.CreateOrderInput) (*ordertransport.OrderPreview, error) {
	preview, err := a.orders.PreviewOrder(orderapp.CreateOrderInput{
		StorefrontRequest:   true,
		UserID:              input.UserID,
		Tenant:              input.Tenant,
		Items:               mapServiceOrderItems(input.Items),
		CouponCode:          input.CouponCode,
		AffiliateCode:       input.AffiliateCode,
		AffiliateVisitorKey: input.AffiliateVisitorKey,
		ClientIP:            input.ClientIP,
		ManualFormData:      input.ManualFormData,
	})
	if err != nil {
		return nil, mapOrderTransportError(err)
	}
	return mapOrderPreview(preview), nil
}

func (a orderPreviewAdapter) PreviewGuestOrder(input ordertransport.CreateGuestOrderInput) (*ordertransport.OrderPreview, error) {
	preview, err := a.orders.PreviewGuestOrder(orderapp.CreateGuestOrderInput{
		StorefrontRequest:   true,
		Email:               input.Email,
		OrderPassword:       input.OrderPassword,
		Locale:              input.Locale,
		Tenant:              input.Tenant,
		Items:               mapServiceOrderItems(input.Items),
		CouponCode:          input.CouponCode,
		AffiliateCode:       input.AffiliateCode,
		AffiliateVisitorKey: input.AffiliateVisitorKey,
		ClientIP:            input.ClientIP,
		ManualFormData:      input.ManualFormData,
	})
	if err != nil {
		return nil, mapOrderTransportError(err)
	}
	return mapOrderPreview(preview), nil
}

func mapServiceOrderItems(items []ordertransport.CreateOrderItem) []orderapp.CreateOrderItem {
	out := make([]orderapp.CreateOrderItem, 0, len(items))
	for _, item := range items {
		out = append(out, orderapp.CreateOrderItem{
			ProductID:       item.ProductID,
			SKUID:           item.SKUID,
			Quantity:        item.Quantity,
			FulfillmentType: item.FulfillmentType,
		})
	}
	return out
}

func mapOrderPreview(preview *orderapp.OrderPreview) *ordertransport.OrderPreview {
	if preview == nil {
		return nil
	}
	items := make([]ordertransport.OrderPreviewItem, 0, len(preview.Items))
	for _, item := range preview.Items {
		items = append(items, ordertransport.OrderPreviewItem{
			ProductID:          item.ProductID,
			SKUID:              item.SKUID,
			TitleJSON:          item.TitleJSON,
			SKUSnapshotJSON:    item.SKUSnapshotJSON,
			Tags:               item.Tags,
			OriginalUnitPrice:  item.OriginalUnitPrice,
			UnitPrice:          item.UnitPrice,
			Quantity:           item.Quantity,
			OriginalTotalPrice: item.OriginalTotalPrice,
			TotalPrice:         item.TotalPrice,
			MemberDiscount:     item.MemberDiscount,
			CouponDiscount:     item.CouponDiscount,
			PromotionDiscount:  item.PromotionDiscount,
			WholesaleDiscount:  item.WholesaleDiscount,
			FulfillmentType:    constants.BuyerFulfillmentType(item.FulfillmentType),
		})
	}
	return &ordertransport.OrderPreview{
		Currency:                preview.Currency,
		OriginalAmount:          preview.OriginalAmount,
		MemberDiscountAmount:    preview.MemberDiscountAmount,
		DiscountAmount:          preview.DiscountAmount,
		PromotionDiscountAmount: preview.PromotionDiscountAmount,
		WholesaleDiscountAmount: preview.WholesaleDiscountAmount,
		TotalAmount:             preview.TotalAmount,
		Items:                   items,
	}
}

func mapOrderTransportError(err error) error {
	if err == nil {
		return nil
	}
	if retryAfter := orderriskcontract.GetRetryAfter(err); retryAfter > 0 {
		return ordertransport.WrapRiskRateLimited(retryAfter, err)
	}
	for _, mapping := range []struct {
		source error
		target error
	}{
		{orderapp.ErrOrderNotFound, ordertransport.ErrOrderNotFound},
		{orderapp.ErrOrderStatusInvalid, ordertransport.ErrOrderStatusInvalid},
		{orderapp.ErrOrderFetchFailed, ordertransport.ErrOrderFetchFailed},
		{orderapp.ErrGuestOrderNotFound, ordertransport.ErrGuestOrderNotFound},
		{orderapp.ErrOrderCancelNotAllowed, ordertransport.ErrOrderCancelNotAllowed},
		{orderapp.ErrOrderRefundExpired, ordertransport.ErrOrderRefundExpired},
		{walletcontract.ErrInvalidAmount, ordertransport.ErrWalletInvalidAmount},
		{walletcontract.ErrRefundExceeded, ordertransport.ErrWalletRefundExceeded},
		{walletcontract.ErrNotSupportedForGuest, ordertransport.ErrWalletNotSupportedForGuest},
		{orderapp.ErrProductSKURequired, ordertransport.ErrProductSKURequired},
		{orderapp.ErrInvalidOrderAmount, ordertransport.ErrInvalidOrderAmount},
		{orderapp.ErrGuestEmailRequired, ordertransport.ErrGuestEmailRequired},
		{orderapp.ErrGuestPasswordRequired, ordertransport.ErrGuestPasswordRequired},
		{orderapp.ErrGuestPasswordTooShort, ordertransport.ErrGuestPasswordTooShort},
		{orderapp.ErrInvalidEmail, ordertransport.ErrInvalidEmail},
		{orderapp.ErrProductPurchaseNotAllowed, ordertransport.ErrProductPurchaseNotAllowed},
		{orderapp.ErrQuefaBelowCostFloor, ordertransport.ErrQuefaBelowCostFloor},
		{orderapp.ErrManualStockInsufficient, ordertransport.ErrManualStockInsufficient},
		{orderapp.ErrOrderCurrencyMismatch, ordertransport.ErrOrderCurrencyMismatch},
		{orderapp.ErrProductNotAvailable, ordertransport.ErrProductNotAvailable},
		{orderapp.ErrResellerCouponNotAllowed, ordertransport.ErrResellerCouponNotAllowed},
		{orderapp.ErrQueueUnavailable, ordertransport.ErrQueueUnavailable},
		{orderriskcontract.ErrIPBlacklisted, ordertransport.ErrRiskIPBlacklisted},
		{orderriskcontract.ErrClientIPUnavailable, ordertransport.ErrRiskClientIPUnavailable},
		{orderriskcontract.ErrTooManyPendingOrders, ordertransport.ErrRiskTooManyPendingOrders},
		{orderriskcontract.ErrProductQuantityLimit, ordertransport.ErrRiskProductQuantityLimit},
		{orderriskcontract.ErrPendingProductQuantityLimit, ordertransport.ErrRiskPendingProductLimit},
		{orderriskcontract.ErrOrderRateLimited, ordertransport.ErrRiskOrderRateLimited},
	} {
		if errors.Is(err, mapping.source) {
			return fmt.Errorf("%w: %v", mapping.target, err)
		}
	}
	return err
}
