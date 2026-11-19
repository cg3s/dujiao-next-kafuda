package rehearsal_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dujiao-next/internal/constants"
	fulfillmentdomain "github.com/dujiao-next/internal/modules/fulfillment/domain"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
	orderdomain "github.com/dujiao-next/internal/modules/order/domain"
	paymentdomain "github.com/dujiao-next/internal/modules/payment/domain"
	walletdomain "github.com/dujiao-next/internal/modules/wallet/domain"
)

func TestDirectBuyerOnlinePaymentRehearsal(t *testing.T) {
	const origin = "http://127.0.0.1:5180"
	const price = "100.00"
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>fixture storefront</html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, router, buyer, other := newDirectRehearsalRouter(t, dir, origin)
	request := func(method, path, body, contentType, token string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, origin+path, strings.NewReader(body)).WithContext(t.Context())
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res := httptest.NewRecorder()
		router.ServeHTTP(res, req)
		return res
	}
	readJSON := func(method, path, body, token string, wantOK bool, data any) {
		t.Helper()
		res := request(method, path, body, "application/json", token)
		var reply struct {
			StatusCode int             `json:"status_code"`
			Data       json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(res.Body.Bytes(), &reply); err != nil {
			t.Fatalf("%s %s returned HTTP %d: %v", method, path, res.Code, err)
		}
		ok := res.Code >= 200 && res.Code < 300 && reply.StatusCode == 0
		if ok != wantOK {
			t.Fatalf("%s %s returned HTTP %d, API status %d", method, path, res.Code, reply.StatusCode)
		}
		if data != nil {
			if err := json.Unmarshal(reply.Data, data); err != nil {
				t.Fatalf("decode %s %s data: %v", method, path, err)
			}
		}
	}

	var product struct {
		ID              uint   `json:"id"`
		FulfillmentType string `json:"fulfillment_type"`
		IsSoldOut       bool   `json:"is_sold_out"`
		SKUs            []struct {
			ID uint `json:"id"`
		} `json:"skus"`
	}
	readJSON(http.MethodGet, "/api/v1/public/products/"+f.product.Slug, "", "", true, &product)
	if product.ID != f.product.ID || product.FulfillmentType != constants.FulfillmentTypeKafudaGPT || product.IsSoldOut ||
		len(product.SKUs) != 1 || product.SKUs[0].ID != f.sku.ID {
		t.Fatalf("online rehearsal product is not available: %+v", product)
	}
	items := fmt.Sprintf(`"items":[{"product_id":%d,"sku_id":%d,"quantity":1}]`, product.ID, product.SKUs[0].ID)
	var preview struct {
		TotalAmount string `json:"total_amount"`
	}
	readJSON(http.MethodPost, "/api/v1/orders/preview", "{"+items+"}", buyer, true, &preview)
	if preview.TotalAmount != price {
		t.Fatalf("online preview amount = %q, want %q", preview.TotalAmount, price)
	}
	var channels []struct {
		ID           uint   `json:"id"`
		ProviderType string `json:"provider_type"`
	}
	readJSON(http.MethodPost, "/api/v1/order/payment-channels", "{"+items+`,"amount":"100.00"}`, buyer, true, &channels)
	if len(channels) != 1 || channels[0].ID == 0 || channels[0].ProviderType != constants.PaymentProviderEpay {
		t.Fatalf("online rehearsal channel is not available: %+v", channels)
	}

	var created struct {
		OrderNo          string `json:"order_no"`
		OrderPaid        bool   `json:"order_paid"`
		PaymentID        uint   `json:"payment_id"`
		PaymentError     string `json:"payment_error"`
		OnlinePayAmount  string `json:"online_pay_amount"`
		WalletPaidAmount string `json:"wallet_paid_amount"`
		PayURL           string `json:"pay_url"`
	}
	payload := fmt.Sprintf(`{%s,"channel_id":%d,"use_balance":false}`, items, channels[0].ID)
	readJSON(http.MethodPost, "/api/v1/orders/create-and-pay", payload, buyer, true, &created)
	if created.OrderNo == "" || created.OrderPaid || created.PaymentID == 0 || created.PaymentError != "" ||
		created.OnlinePayAmount != price || created.WalletPaidAmount != "0.00" {
		t.Fatalf("online checkout did not await callback: order=%q paid=%t id=%d online=%q wallet=%q",
			created.OrderNo, created.OrderPaid, created.PaymentID, created.OnlinePayAmount, created.WalletPaidAmount)
	}
	cashier, err := url.Parse(created.PayURL)
	if err != nil || cashier.Scheme != "http" || cashier.Host != "127.0.0.1:5180" || cashier.Path != rehearsalEpayBasePath+"/submit.php" {
		t.Fatalf("checkout did not create a local EPay redirect: %v", err)
	}
	orderPath := "/api/v1/orders/" + url.PathEscape(created.OrderNo)
	var latest struct {
		PaymentID     uint   `json:"payment_id"`
		OrderNo       string `json:"order_no"`
		PayableAmount string `json:"payable_amount"`
		PayURL        string `json:"pay_url"`
	}
	readJSON(http.MethodGet, "/api/v1/payments/latest?order_no="+url.QueryEscape(created.OrderNo), "", buyer, true, &latest)
	if latest.PaymentID != created.PaymentID || latest.OrderNo != created.OrderNo ||
		latest.PayableAmount != price || latest.PayURL != created.PayURL {
		t.Fatal("pending checkout did not preserve its payment and local cashier URL")
	}

	loadOrderAndPayment := func() (*orderdomain.Order, paymentdomain.Payment) {
		t.Helper()
		var payment paymentdomain.Payment
		if err := f.db.First(&payment, created.PaymentID).Error; err != nil {
			t.Fatal(err)
		}
		order, err := f.options.OrderStore.GetByID(payment.OrderID)
		if err != nil || order == nil || order.OrderNo != created.OrderNo || len(order.Children) != 1 {
			t.Fatalf("load online checkout %q: %v", created.OrderNo, err)
		}
		return order, payment
	}
	originalOrder, originalPayment := loadOrderAndPayment()
	childID := originalOrder.Children[0].ID
	originalRecharge, err := f.store.GetRecharge(t.Context(), childID)
	if err != nil || originalRecharge == nil || originalRecharge.Stage != kafudadomain.StageWaiting ||
		originalRecharge.Snapshot.SellPrice.String() != price || originalRecharge.CurrentAttemptID != nil {
		t.Fatalf("unexpected Direct checkout snapshot before payment: %+v, %v", originalRecharge, err)
	}
	assertNoWalletOrFulfillment := func() {
		t.Helper()
		var wallet struct {
			Balance string `json:"balance"`
		}
		readJSON(http.MethodGet, "/api/v1/wallet", "", buyer, true, &wallet)
		if wallet.Balance != "200.00" {
			t.Fatalf("online checkout changed wallet balance: %s", wallet.Balance)
		}
		for _, model := range []any{&walletdomain.Transaction{}, &kafudadomain.Attempt{}, &fulfillmentdomain.Fulfillment{}} {
			var count int64
			if err := f.db.Model(model).Count(&count).Error; err != nil || count != 0 {
				t.Fatalf("online checkout created unexpected %T rows: %d, %v", model, count, err)
			}
		}
		recharge, err := f.store.GetRecharge(t.Context(), childID)
		if err != nil || !reflect.DeepEqual(recharge, originalRecharge) {
			t.Fatalf("payment changed the checkout recharge: %+v, %v", recharge, err)
		}
	}
	assertPending := func() {
		t.Helper()
		order, payment := loadOrderAndPayment()
		child := order.Children[0]
		if order.Status != constants.OrderStatusPendingPayment || order.PaidAt != nil ||
			child.Status != constants.OrderStatusPendingPayment || child.PaidAt != nil ||
			payment.Status != constants.PaymentStatusPending || payment.PaidAt != nil || payment.CallbackAt != nil ||
			payment.ProviderRef != originalPayment.ProviderRef || !payment.UpdatedAt.Equal(originalPayment.UpdatedAt) ||
			!order.WalletPaidAmount.IsZero() || order.OnlinePaidAmount.String() != price ||
			payment.Amount.String() != price || payment.Currency != "CNY" || payment.GatewayOrderNo == "" {
			t.Fatalf("unconfirmed checkout changed payment state: order=%s child=%s payment=%s", order.Status, child.Status, payment.Status)
		}
		assertNoWalletOrFulfillment()
	}
	assertPending()

	var returnPath string
	for range 2 {
		res := request(http.MethodGet, cashier.RequestURI(), "", "", "")
		returned, err := url.Parse(res.Header().Get("Location"))
		if err != nil || res.Code != http.StatusSeeOther || returned.Path != "/pay" {
			t.Fatalf("cashier did not redirect back to checkout: HTTP %d, %v, gateway order matches=%t, quote=%q, stored amount=%s, has expiry=%t",
				res.Code, err, cashier.Query().Get("out_trade_no") == originalPayment.GatewayOrderNo,
				cashier.Query().Get("money"), originalPayment.Amount, originalPayment.ExpiredAt != nil)
		}
		target := cashier.ResolveReference(returned)
		query := target.Query()
		if target.Scheme != cashier.Scheme || target.Host != cashier.Host || query.Get("order_no") != created.OrderNo || query.Get("epay_return") != "1" {
			t.Fatalf("cashier did not preserve the local order return")
		}
		if returnPath != "" && target.RequestURI() != returnPath {
			t.Fatal("cashier replay changed the return URL")
		}
		returnPath = target.RequestURI()
		assertPending()
	}
	readJSON(http.MethodGet, orderPath+"/direct-status", "", buyer, false, nil)

	var channel paymentdomain.PaymentChannel
	if err := f.db.First(&channel, channels[0].ID).Error; err != nil {
		t.Fatal(err)
	}
	merchantID, _ := channel.ConfigJSON["merchant_id"].(string)
	if merchantID == "" || originalPayment.GatewayOrderNo == "" {
		t.Fatal("missing synthetic EPay callback identity")
	}
	badSignature := url.Values{
		"pid": {merchantID}, "out_trade_no": {originalPayment.GatewayOrderNo}, "trade_no": {"rehearsal-invalid"},
		"money": {originalPayment.Amount.String()}, "trade_status": {constants.EpayTradeStatusSuccess},
		"sign_type": {"MD5"}, "sign": {"invalid-rehearsal-signature"},
	}
	if res := request(http.MethodPost, "/api/v1/payments/callback", badSignature.Encode(), "application/x-www-form-urlencoded", ""); res.Code != http.StatusOK || strings.TrimSpace(res.Body.String()) != constants.EpayCallbackFail {
		t.Fatalf("invalid EPay signature not rejected: HTTP %d", res.Code)
	}
	assertPending()

	confirmPath := "/api/v1/rehearsal/orders/" + url.PathEscape(created.OrderNo) + "/confirm-payment"
	readJSON(http.MethodPost, confirmPath, "", other, false, nil)
	assertPending()
	type stages struct {
		OrderNo string `json:"order_no"`
		Status  string `json:"status"`
		Stage   string `json:"stage"`
	}
	var settledOrder *orderdomain.Order
	var settledPayment paymentdomain.Payment
	for range 2 {
		var confirmed stages
		readJSON(http.MethodPost, confirmPath, "", buyer, true, &confirmed)
		if confirmed.OrderNo != created.OrderNo || confirmed.Status != constants.OrderStatusPaid || confirmed.Stage != string(kafudadomain.StageWaiting) {
			t.Fatalf("callback did not return a paid order awaiting authorization: %+v", confirmed)
		}
		order, payment := loadOrderAndPayment()
		child := order.Children[0]
		if order.Status != constants.OrderStatusPaid || child.Status != constants.OrderStatusPaid ||
			order.PaidAt == nil || child.PaidAt == nil || payment.Status != constants.PaymentStatusSuccess ||
			payment.PaidAt == nil || payment.ProviderRef == "" || payment.Amount.String() != price || payment.Currency != "CNY" ||
			order.OnlinePaidAmount.String() != price || !order.WalletPaidAmount.IsZero() {
			t.Fatalf("verified payment did not settle just the online amount: order=%s child=%s payment=%s", order.Status, child.Status, payment.Status)
		}
		if settledOrder != nil {
			if !order.PaidAt.Equal(*settledOrder.PaidAt) || !child.PaidAt.Equal(*settledOrder.Children[0].PaidAt) ||
				!payment.PaidAt.Equal(*settledPayment.PaidAt) || payment.ProviderRef != settledPayment.ProviderRef {
				t.Fatal("callback replay changed original payment facts")
			}
		}
		settledOrder, settledPayment = order, payment
		assertNoWalletOrFulfillment()
	}
	var status stages
	readJSON(http.MethodGet, orderPath+"/direct-status", "", buyer, true, &status)
	if status.Stage != string(kafudadomain.StageWaiting) {
		t.Fatalf("buyer progress after online payment = %q", status.Stage)
	}
}
