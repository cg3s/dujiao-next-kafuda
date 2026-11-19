package rehearsal_test

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dujiao-next/internal/app/container"
	paymentwiring "github.com/dujiao-next/internal/bootstrap/payment"
	"github.com/dujiao-next/internal/constants"
	ordercontract "github.com/dujiao-next/internal/modules/order/contract"
	orderdomain "github.com/dujiao-next/internal/modules/order/domain"
	paymentcontract "github.com/dujiao-next/internal/modules/payment/contract"
	paymentdomain "github.com/dujiao-next/internal/modules/payment/domain"
	epayadapter "github.com/dujiao-next/internal/modules/payment/infrastructure/gateway/adapters/epay"
	"github.com/dujiao-next/internal/modules/payment/infrastructure/gateway/epay"
	"github.com/dujiao-next/internal/modules/payment/infrastructure/gateway/provider"
	paymentstore "github.com/dujiao-next/internal/modules/payment/infrastructure/gormstore"
	paymenthttp "github.com/dujiao-next/internal/modules/payment/transport/http"
	paymentcallback "github.com/dujiao-next/internal/modules/payment/transport/http/callback"
	"github.com/dujiao-next/internal/platform/http/ginutil"
	"github.com/dujiao-next/internal/platform/http/response"
	"github.com/dujiao-next/internal/shared/jsonmap"

	"github.com/gin-gonic/gin"
)

const (
	rehearsalEpayBasePath    = "/rehearsal/epay"
	rehearsalPaymentCallback = "/api/v1/payments/callback"
)

type directRehearsalOnlinePayment struct {
	fixture   directCheckoutFixture
	payments  *paymentstore.Store
	channelID uint
	origin    *url.URL
	config    *epay.Config
}

func newDirectRehearsalOnlinePayment(t *testing.T, f directCheckoutFixture, payments *paymentstore.Store, origin string) (*directRehearsalOnlinePayment, paymentcontract.GatewayRegistry) {
	t.Helper()
	parsed, err := url.Parse(origin)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil || port < 1 || port > 65535 || origin != "http://127.0.0.1:"+strconv.Itoa(port) {
		t.Fatal("online rehearsal requires a fixed IPv4 loopback origin")
	}
	channel := paymentdomain.PaymentChannel{
		Name: "模拟线上支付", ProviderType: constants.PaymentProviderEpay, ChannelType: constants.PaymentChannelTypeAlipay,
		InteractionMode: constants.PaymentInteractionRedirect, IsActive: true,
		ConfigJSON: jsonmap.JSON{
			"epay_version": epay.VersionV1, "merchant_id": "rehearsal-epay", "merchant_key": rand.Text(),
			"gateway_url": origin + rehearsalEpayBasePath, "notify_url": origin + rehearsalPaymentCallback,
			"return_url": origin + "/pay",
		},
	}
	if err := f.db.Create(&channel).Error; err != nil {
		t.Fatal(err)
	}
	cfg, err := epay.ParseConfig(channel.ConfigJSON)
	if err != nil {
		t.Fatal(err)
	}
	registry := provider.NewRegistry()
	registry.Register(constants.PaymentProviderEpay, "", epayadapter.NewEpayAdapter())
	return &directRehearsalOnlinePayment{fixture: f, payments: payments, channelID: channel.ID, origin: parsed, config: cfg}, registry
}

func (a *directRehearsalOnlinePayment) register(router *gin.Engine, member *gin.RouterGroup, deps *container.Container) {
	handlers := paymentwiring.New(deps)
	paymentcallback.RegisterRoutes(router.Group("/api/v1"), handlers.Callback)
	paymenthttp.RegisterUserLatestRoute(member, handlers.Latest)
	router.GET(rehearsalEpayBasePath+"/submit.php", a.returnFromCashier)
	member.POST("/rehearsal/orders/:order_no/confirm-payment", func(c *gin.Context) {
		a.confirmPayment(c, router)
	})
}

func (a *directRehearsalOnlinePayment) eligiblePayment(payment *paymentdomain.Payment) bool {
	if payment == nil || payment.ChannelID != a.channelID || payment.ProviderType != constants.PaymentProviderEpay ||
		payment.GatewayOrderNo == "" || !payment.Amount.Decimal.IsPositive() || payment.SupersededAt != nil {
		return false
	}
	return payment.Status == constants.PaymentStatusSuccess ||
		(payment.Status == constants.PaymentStatusPending && payment.ExpiredAt == nil)
}

func pendingRehearsalOnlineOrder(order *orderdomain.Order) bool {
	return order.Status == constants.OrderStatusPendingPayment && (order.ExpiresAt == nil || time.Now().Before(*order.ExpiresAt))
}

func (a *directRehearsalOnlinePayment) returnFromCashier(c *gin.Context) {
	query, err := url.ParseQuery(c.Request.URL.RawQuery)
	if err != nil {
		directRehearsalError(c, http.StatusBadRequest, "error.bad_request")
		return
	}
	for _, values := range query {
		if len(values) != 1 {
			directRehearsalError(c, http.StatusBadRequest, "error.bad_request")
			return
		}
	}
	if epay.VerifyCallback(a.config, query) != nil || epay.VerifyCallbackOwnership(a.config, query) != nil ||
		query.Get("notify_url") != a.config.NotifyURL {
		directRehearsalError(c, http.StatusBadRequest, "error.bad_request")
		return
	}
	returnURL, err := url.Parse(query.Get("return_url"))
	if err != nil || returnURL.Scheme != a.origin.Scheme || returnURL.Host != a.origin.Host ||
		returnURL.Path != "/pay" || returnURL.User != nil || returnURL.Fragment != "" {
		directRehearsalError(c, http.StatusBadRequest, "error.bad_request")
		return
	}
	returnQuery := returnURL.Query()
	if len(returnQuery) != 3 || returnQuery.Get("biz_type") != "order" ||
		returnQuery.Get("order_no") == "" || returnQuery.Get("epay_return") != "1" {
		directRehearsalError(c, http.StatusBadRequest, "error.bad_request")
		return
	}
	payment, err := a.payments.GetByGatewayOrderNo(query.Get("out_trade_no"))
	if err != nil || !a.eligiblePayment(payment) || query.Get("money") != payment.Amount.Decimal.String() {
		directRehearsalError(c, http.StatusNotFound, "error.order_not_found")
		return
	}
	order, err := a.fixture.options.OrderStore.GetByID(payment.OrderID)
	if err != nil || order == nil || order.OrderNo != returnQuery.Get("order_no") || !order.HasDirectRecharge() ||
		(payment.Status == constants.PaymentStatusPending && !pendingRehearsalOnlineOrder(order)) {
		directRehearsalError(c, http.StatusNotFound, "error.order_not_found")
		return
	}
	// The buyer can return before the provider sends the asynchronous callback.
	c.Header("Referrer-Policy", "no-referrer")
	c.Redirect(http.StatusSeeOther, returnURL.RequestURI())
}

func (a *directRehearsalOnlinePayment) confirmPayment(c *gin.Context, callbackHandler http.Handler) {
	if c.Request.Body != nil {
		var first [1]byte
		if _, err := io.ReadFull(c.Request.Body, first[:]); !errors.Is(err, io.EOF) {
			directRehearsalError(c, http.StatusBadRequest, "error.bad_request")
			return
		}
	}
	userID, ok := ginutil.GetUserID(c)
	if !ok {
		return
	}
	orders := a.fixture.options.OrderStore
	order, err := orders.GetByOrderNoAndUserScoped(c.Param("order_no"), userID, ordercontract.TenantScope{})
	if err != nil || order == nil || order.ParentID != nil || len(order.Children) != 1 || !order.HasDirectRecharge() {
		directRehearsalError(c, http.StatusNotFound, "error.order_not_found")
		return
	}
	payments, err := a.payments.ListByOrderID(order.ID)
	if err != nil {
		directRehearsalError(c, http.StatusInternalServerError, "error.order_fetch_failed")
		return
	}
	var payment *paymentdomain.Payment
	for i := range payments {
		if payments[i].ChannelID == a.channelID {
			payment = &payments[i]
			break
		}
	}
	if !a.eligiblePayment(payment) || (payment.Status == constants.PaymentStatusPending && !pendingRehearsalOnlineOrder(order)) {
		directRehearsalError(c, http.StatusConflict, "error.order_status_invalid")
		return
	}
	form := a.callbackForm(payment)
	req := httptest.NewRequest(http.MethodPost, rehearsalPaymentCallback, strings.NewReader(form.Encode())).WithContext(c.Request.Context())
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	result := httptest.NewRecorder()
	callbackHandler.ServeHTTP(result, req)
	if result.Code != http.StatusOK || result.Body.String() != constants.EpayCallbackSuccess {
		directRehearsalError(c, http.StatusInternalServerError, "error.order_fetch_failed")
		return
	}
	settled, err := orders.GetByID(order.ID)
	if err != nil || settled == nil || settled.PaidAt == nil || len(settled.Children) != 1 {
		directRehearsalError(c, http.StatusInternalServerError, "error.order_fetch_failed")
		return
	}
	record, err := a.fixture.store.GetRecharge(c.Request.Context(), settled.Children[0].ID)
	if err != nil || record == nil {
		directRehearsalError(c, http.StatusInternalServerError, "error.order_fetch_failed")
		return
	}
	response.Success(c, gin.H{"order_no": settled.OrderNo, "status": settled.Status, "stage": record.Stage})
}

func (a *directRehearsalOnlinePayment) callbackForm(payment *paymentdomain.Payment) url.Values {
	form := url.Values{
		"pid": {a.config.MerchantID}, "out_trade_no": {payment.GatewayOrderNo},
		"trade_no": {"rehearsal-" + strconv.FormatUint(uint64(payment.ID), 10)},
		"money":    {payment.Amount.String()}, "trade_status": {constants.EpayTradeStatusSuccess},
	}
	keys := make([]string, 0, len(form))
	for key := range form {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	pairs := make([]string, 0, len(keys))
	for _, key := range keys {
		pairs = append(pairs, key+"="+form.Get(key))
	}
	// EPay v1 uses its legacy MD5 wire signature. The key is unique to this fixture.
	signature := md5.Sum([]byte(strings.Join(pairs, "&") + a.config.MerchantKey))
	form.Set("sign", hex.EncodeToString(signature[:]))
	form.Set("sign_type", a.config.SignType)
	return form
}
