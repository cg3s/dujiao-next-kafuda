package client

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	quefacontract "github.com/dujiao-next/internal/modules/quefa/contract"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func newMockResponse(status int, body string, headers map[string]string) *http.Response {
	h := make(http.Header)
	h.Set("Content-Type", "application/json")
	for k, v := range headers {
		h.Set(k, v)
	}
	return &http.Response{
		StatusCode: status,
		Header:     h,
		Body:       io.NopCloser(bytes.NewBufferString(body)),
	}
}

func TestBuildCanonicalStringAndHMACSignature(t *testing.T) {
	body := []byte(`{"merchant_order_no":"SHOP-1","product_code":"plus_1m","quantity":1}`)
	q := url.Values{"b": {"2"}, "a": {"1 2"}}
	canonicalQuery := CanonicalQuery(q)
	if canonicalQuery != "a=1%202&b=2" {
		t.Fatalf("unexpected canonical query: %q", canonicalQuery)
	}

	canonical := BuildCanonicalString("post", "/v1/orders", canonicalQuery, "1700000000", "nonce-1", "key-1", "idem-1", body)
	lines := strings.Split(canonical, "\n")
	if len(lines) != 8 {
		t.Fatalf("expected 8 canonical lines, got %d", len(lines))
	}
	if lines[0] != "POST" || lines[1] != "/v1/orders" || lines[2] != "a=1%202&b=2" || lines[3] != "1700000000" || lines[4] != "nonce-1" || lines[5] != "key-1" || lines[6] != "idem-1" {
		t.Fatalf("unexpected canonical lines: %#v", lines)
	}
	sig := SignRequest("super-secret", canonical)
	if len(sig) != 64 || sig != strings.ToLower(sig) {
		t.Fatalf("expected 64-char lowercase hex signature, got %q", sig)
	}
}

func TestVerifyWebhookSignature(t *testing.T) {
	secret := "whsec_test_123"
	raw := []byte(`{"event_id":"evt_1","event_type":"order.paid","order_id":"qf_1"}`)
	now := time.Unix(1700000000, 0)
	ts := strconv.FormatInt(now.Unix(), 10)

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "."))
	mac.Write(raw)
	validSig := hex.EncodeToString(mac.Sum(nil))

	if err := VerifyWebhookSignature(secret, ts, raw, validSig, now.Add(2*time.Minute), 5*time.Minute); err != nil {
		t.Fatalf("expected valid webhook signature, got %v", err)
	}
	if err := VerifyWebhookSignature(secret, ts, raw, "sha256="+validSig, now, 5*time.Minute); err != nil {
		t.Fatalf("expected valid sha256= prefixed webhook signature, got %v", err)
	}
	if err := VerifyWebhookSignature(secret, ts, raw, validSig, now.Add(10*time.Minute), 5*time.Minute); err != ErrWebhookTimestampInvalid {
		t.Fatalf("expected ErrWebhookTimestampInvalid, got %v", err)
	}
	if err := VerifyWebhookSignature(secret, ts, raw, strings.Repeat("0", 64), now, 5*time.Minute); err != ErrWebhookSignatureInvalid {
		t.Fatalf("expected ErrWebhookSignatureInvalid, got %v", err)
	}
}

func TestClientSignsHeadersAndParsesPollHints(t *testing.T) {
	secret := "client_secret_xyz"
	var seenHeaders http.Header
	var seenBody []byte

	httpClient := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			seenHeaders = r.Header.Clone()
			seenBody, _ = io.ReadAll(r.Body)
			return newMockResponse(
				http.StatusAccepted,
				`{"order_id":"qf_ord_9","merchant_order_no":"ORD-9","status":"pending","pay_url":"https://pay.myshop.com/p/9","currency":"CNY"}`,
				map[string]string{
					"X-Poll-After":          "15",
					"X-RateLimit-Remaining": "42",
				},
			), nil
		}),
	}

	c, err := NewForTest("https://tibo.ink", "partner_1", "key_1", secret, httpClient)
	if err != nil {
		t.Fatalf("NewForTest failed: %v", err)
	}

	snap, err := c.CreateOrder(context.Background(), quefacontract.CreateOrderRequest{
		MerchantOrderNo: "ORD-9",
		ProductCode:     "plus_1m",
		Quantity:        1,
		SaleAmount:      "135.00",
		CollectionMode:  "platform_collect",
		DeliveryMode:    "auto_recharge",
	}, "idem-ord-9")
	if err != nil {
		t.Fatalf("CreateOrder failed: %v", err)
	}
	if !snap.AcceptedAsync || snap.OrderID != "qf_ord_9" || snap.Hints.PollAfter != 15*time.Second || snap.Hints.RateLimitRemaining != 42 {
		t.Fatalf("unexpected snapshot: %+v", snap)
	}

	canonical := BuildCanonicalString(
		http.MethodPost,
		"/v1/orders",
		"",
		seenHeaders.Get("X-Timestamp"),
		seenHeaders.Get("X-Nonce"),
		seenHeaders.Get("X-Key-Id"),
		seenHeaders.Get("Idempotency-Key"),
		seenBody,
	)
	if SignRequest(secret, canonical) != seenHeaders.Get("X-Signature") {
		t.Fatalf("signature header did not match expected HMAC")
	}
}

func TestClientClassifies409And429AndBlocksRedirect(t *testing.T) {
	httpClient := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			switch r.URL.Path {
			case "/v1/orders":
				return newMockResponse(http.StatusConflict, `{"code":"idempotency_in_progress"}`, nil), nil
			case "/v1/products":
				return newMockResponse(http.StatusTooManyRequests, `{"code":"rate_limited"}`, map[string]string{"Retry-After": "30"}), nil
			default:
				return newMockResponse(http.StatusFound, ``, map[string]string{"Location": "https://evil.example/steal"}), nil
			}
		}),
	}
	c, err := NewForTest("https://tibo.ink", "p1", "k1", "s1", httpClient)
	if err != nil {
		t.Fatalf("NewForTest failed: %v", err)
	}

	if _, err := c.CreateOrder(context.Background(), quefacontract.CreateOrderRequest{MerchantOrderNo: "M1"}, "idem-1"); !quefacontract.IsAPIErrorCategory(err, "idempotency_in_progress") {
		t.Fatalf("expected idempotency_in_progress error, got %v", err)
	}
	if _, hints, err := c.GetProducts(context.Background()); !quefacontract.IsAPIErrorCategory(err, "rate_limited") || hints.RetryAfter != 30*time.Second {
		t.Fatalf("expected rate_limited with 30s Retry-After, got err=%v hints=%+v", err, hints)
	}
	if _, err := c.GetOrder(context.Background(), "ord_redir"); !quefacontract.IsAPIErrorCategory(err, "redirect_blocked") {
		t.Fatalf("expected redirect_blocked, got %v", err)
	}
}

func TestOfficialQuefaSigningVectorsAndWebhookFormat(t *testing.T) {
	secret := "tibo_demo_secret_never_use_in_production"
	keyID := "key_demo_01"
	chineseBody := []byte(`{"merchant_order_no":"DEMO-中文-001","product_code":"chatgpt_plus_cdk_1m","quantity":1,"sale_amount":"135.00","collection_mode":"platform_collect","delivery_mode":"auto_recharge"}`)

	// Vector 1: Chinese JSON body POST
	c1 := BuildCanonicalString("POST", "/v1/orders", "", "1790323200", "demo-nonce-0001", keyID, "demo-order-0001", chineseBody)
	if got := SignRequest(secret, c1); got != "82844463eb516b0891b81956465ee7e4c240e5fec403da12f0c4ea5e5d7cd1e0" {
		t.Fatalf("vector 1 mismatch: got %s", got)
	}

	// Vector 2: GET with UTF-8 query params and empty body
	q := url.Values{}
	q.Set("search", "测试")
	q.Set("limit", "20")
	q.Set("payment_status", "paid")
	c2 := BuildCanonicalString("GET", "/v1/orders", CanonicalQuery(q), "1790323260", "demo-nonce-0002", keyID, "", nil)
	if got := SignRequest(secret, c2); got != "9913c8b36f8354f1a15d6c8a875fa9f45b846a1ea33b054212d30c9e30dd3496" {
		t.Fatalf("vector 2 mismatch: got %s", got)
	}

	// Vector 3: Retransmission with same idempotency key and updated timestamp/nonce
	c3 := BuildCanonicalString("POST", "/v1/orders", "", "1790323320", "demo-nonce-0003", keyID, "demo-order-0001", chineseBody)
	if got := SignRequest(secret, c3); got != "4688e31de9214846fc167d1756a863d83837238ebdc4f394b39e84a6bdb6f207" {
		t.Fatalf("vector 3 mismatch: got %s", got)
	}

	// Official Webhook signature format: t=<timestamp>,v1=<hex>
	whSecret := "whsec_official_test"
	whTS := "1790323200"
	whBody := []byte(`{"event_id":"evt_1","event":"order.paid","data":{"order_id":"QF0001"}}`)
	expectedHex := SignRequest(whSecret, whTS+"."+string(whBody))
	now := time.Unix(1790323210, 0)
	if err := VerifyWebhookSignature(whSecret, whTS, whBody, "t="+whTS+",v1="+expectedHex, now, 5*time.Minute); err != nil {
		t.Fatalf("official t=,v1= webhook signature failed: %v", err)
	}
	if err := VerifyWebhookSignature(whSecret, whTS, whBody, "t=1790323201,v1="+expectedHex, now, 5*time.Minute); err == nil {
		t.Fatalf("expected error when t= disagrees with X-Quefa-Timestamp")
	}
}

func TestClientPaymentCodeAndOpenAPIOrderSnapshot(t *testing.T) {
	httpClient := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			switch r.URL.Path {
			case "/v1/orders/QF0001/payment-code":
				body, _ := io.ReadAll(r.Body)
				if string(body) != "{}" {
					t.Fatalf("expected empty JSON object body {}, got %q", string(body))
				}
				return newMockResponse(http.StatusOK, `{"data":{"order_id":"QF0001","merchant_order_no":"M1","amount":"135.00","currency":"CNY","payment_status":"pending","payment_code_type":"alipay_precreate","payment_code":"https://qr.alipay.com/bax123","qr_image_data_url":"data:image/png;base64,AAAA"}}`, nil), nil
			case "/v1/orders/QF0001":
				return newMockResponse(http.StatusOK, `{"data":{"order_id":"QF0001","merchant_order_no":"M1","product_code":"chatgpt_plus_cdk_1m","payment_status":"paid","fulfillment_status":"running","fulfillment_message":"处理中","fulfillment_next_action":"wait","qr_payload":"https://tibo.ink/pay/QF0001","auto_recharge":{"status":"submitted","redemption_id":"ful_99"}}}`, nil), nil
			default:
				return newMockResponse(http.StatusNotFound, `{"error":{"code":"not_found"}}`, nil), nil
			}
		}),
	}
	c, err := NewForTest("https://tibo.ink", "pt_1", "key_1", "sec_1", httpClient)
	if err != nil {
		t.Fatalf("NewForTest: %v", err)
	}
	pc, err := c.CreateOrderPaymentCode(context.Background(), "QF0001", "idem-pc-1")
	if err != nil {
		t.Fatalf("CreateOrderPaymentCode failed: %v", err)
	}
	if pc.PaymentCode != "https://qr.alipay.com/bax123" || pc.PaymentCodeType != "alipay_precreate" {
		t.Fatalf("unexpected payment code snapshot: %+v", pc)
	}
	ord, err := c.GetOrder(context.Background(), "QF0001")
	if err != nil {
		t.Fatalf("GetOrder failed: %v", err)
	}
	if ord.PaymentStatus != "paid" || ord.FulfillmentStatus != "running" || ord.Status != "running" || ord.LatestRedemptionID != "ful_99" {
		t.Fatalf("unexpected OpenAPI order snapshot: %+v", ord)
	}
}
