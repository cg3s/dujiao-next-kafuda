package client

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	quefacontract "github.com/dujiao-next/internal/modules/quefa/contract"
	"github.com/shopspring/decimal"
)

const (
	maxResponseBytes = 2 << 20
	productsPath     = "/v1/products"
	ordersPath       = "/v1/orders"
	redemptionsPath  = "/v1/redemptions"
)

var (
	ErrWebhookSignatureMissing = errors.New("quefa webhook signature or timestamp missing")
	ErrWebhookTimestampInvalid = errors.New("quefa webhook timestamp invalid or expired")
	ErrWebhookSignatureInvalid = errors.New("quefa webhook signature mismatch")
)

// Client is an authenticated HTTP client for the Quefa Partner API V1.
type Client struct {
	baseURL      string
	partnerID    string
	keyID        string
	clientSecret string
	http         *http.Client
	now          func() time.Time
	nonce        func() string
}

var _ quefacontract.Gateway = (*Client)(nil)

func (c *Client) String() string   { return "Quefa Partner API client [credentials redacted]" }
func (c *Client) GoString() string { return c.String() }

// New creates a Quefa client. Base URL must be an HTTPS origin without path/query/fragment.
// Redirects are disabled so signed requests are never forwarded across origins.
func New(baseURL, partnerID, keyID, clientSecret string) (*Client, error) {
	return newClient(baseURL, partnerID, keyID, clientSecret, nil, false)
}

// NewForTest constructs a client allowing custom http.Client (e.g. httptest.NewTLSServer / loopback http).
func NewForTest(baseURL, partnerID, keyID, clientSecret string, httpClient *http.Client) (*Client, error) {
	return newClient(baseURL, partnerID, keyID, clientSecret, httpClient, true)
}

func newClient(baseURL, partnerID, keyID, clientSecret string, httpClient *http.Client, allowHTTP bool) (*Client, error) {
	raw := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("quefa base URL must be a valid HTTPS origin")
	}
	if !allowHTTP && !strings.EqualFold(u.Scheme, "https") {
		return nil, errors.New("quefa base URL must use HTTPS")
	}
	if allowHTTP && !strings.EqualFold(u.Scheme, "https") && !strings.EqualFold(u.Scheme, "http") {
		return nil, errors.New("quefa base URL must use HTTP or HTTPS")
	}
	// Strip optional trailing /v1 if the operator pasted https://tibo.ink/v1.
	cleanPath := strings.TrimRight(u.Path, "/")
	if cleanPath != "" && cleanPath != "/v1" {
		return nil, errors.New("quefa base URL must not contain extra path segments")
	}
	u.Path = ""
	if strings.TrimSpace(partnerID) == "" || strings.ContainsAny(partnerID, "\r\n") {
		return nil, errors.New("quefa partner_id is required")
	}
	if strings.TrimSpace(keyID) == "" || strings.ContainsAny(keyID, "\r\n") {
		return nil, errors.New("quefa key_id is required")
	}
	if strings.TrimSpace(clientSecret) == "" || strings.ContainsAny(clientSecret, "\r\n") {
		return nil, errors.New("quefa client_secret is required")
	}
	if httpClient == nil {
		httpClient = &http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	return &Client{
		baseURL:      strings.TrimRight(u.String(), "/"),
		partnerID:    strings.TrimSpace(partnerID),
		keyID:        strings.TrimSpace(keyID),
		clientSecret: strings.TrimSpace(clientSecret),
		http:         httpClient,
		now:          time.Now,
		nonce:        generateNonce,
	}, nil
}

// CanonicalQuery builds a deterministic sorted RFC3986 query string for signing.
func CanonicalQuery(values url.Values) string {
	if len(values) == 0 {
		return ""
	}
	type pair struct{ k, v string }
	pairs := make([]pair, 0, len(values))
	for k, list := range values {
		ek := encodeRFC3986(k)
		if len(list) == 0 {
			pairs = append(pairs, pair{k: ek, v: ""})
			continue
		}
		for _, v := range list {
			pairs = append(pairs, pair{k: ek, v: encodeRFC3986(v)})
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].k == pairs[j].k {
			return pairs[i].v < pairs[j].v
		}
		return pairs[i].k < pairs[j].k
	})
	var b strings.Builder
	for i, p := range pairs {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(p.k)
		b.WriteByte('=')
		b.WriteString(p.v)
	}
	return b.String()
}

func encodeRFC3986(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}

// BuildCanonicalString constructs the exact 8-line Quefa Partner API V1 signing string.
func BuildCanonicalString(method, path, canonicalQuery, timestamp, nonce, keyID, idempotencyKey string, rawBody []byte) string {
	bodySum := sha256.Sum256(rawBody)
	bodyHex := hex.EncodeToString(bodySum[:])
	return strings.Join([]string{
		strings.ToUpper(strings.TrimSpace(method)),
		path,
		canonicalQuery,
		timestamp,
		nonce,
		keyID,
		idempotencyKey,
		bodyHex,
	}, "\n")
}

// SignRequest computes the 64-char lowercase hex HMAC-SHA256 signature.
func SignRequest(secret, canonical string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(canonical))
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifyWebhookSignature validates HMAC_SHA256(webhook_secret, timestamp + "." + RAW_BODY)
// with a replay-protection timestamp skew check.
// Supports the official Quefa header format "t=<timestamp>,v1=<hex>" (requiring t == timestamp)
// as well as plain hex / "sha256=<hex>".
func VerifyWebhookSignature(secret, timestamp string, rawBody []byte, signature string, now time.Time, maxSkew time.Duration) error {
	secret = strings.TrimSpace(secret)
	timestamp = strings.TrimSpace(timestamp)
	rawSig := strings.TrimSpace(signature)
	if secret == "" || timestamp == "" || rawSig == "" {
		return ErrWebhookSignatureMissing
	}

	var sigHex string
	if strings.Contains(rawSig, "v1=") {
		var sigTS string
		for _, part := range strings.Split(rawSig, ",") {
			kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
			if len(kv) != 2 {
				continue
			}
			k := strings.ToLower(strings.TrimSpace(kv[0]))
			v := strings.TrimSpace(kv[1])
			switch k {
			case "t":
				sigTS = v
			case "v1":
				sigHex = strings.ToLower(v)
			}
		}
		if sigHex == "" || (sigTS != "" && sigTS != timestamp) {
			return ErrWebhookSignatureInvalid
		}
	} else {
		sigHex = strings.ToLower(strings.TrimPrefix(rawSig, "sha256="))
	}
	if sigHex == "" {
		return ErrWebhookSignatureMissing
	}

	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil || ts <= 0 {
		return ErrWebhookTimestampInvalid
	}
	if maxSkew <= 0 {
		maxSkew = 5 * time.Minute
	}
	diff := now.Sub(time.Unix(ts, 0))
	if diff < 0 {
		diff = -diff
	}
	if diff > maxSkew {
		return ErrWebhookTimestampInvalid
	}

	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(timestamp))
	_, _ = mac.Write([]byte("."))
	_, _ = mac.Write(rawBody)
	expected := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(sigHex)) {
		return ErrWebhookSignatureInvalid
	}
	return nil
}

func generateNonce() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(buf[:])
}

func (c *Client) GetProducts(ctx context.Context) (json.RawMessage, quefacontract.PollHints, error) {
	body, _, hints, err := c.do(ctx, http.MethodGet, productsPath, nil, nil, "")
	if err != nil {
		return nil, hints, err
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, hints, &quefacontract.APIError{Category: "invalid_response", Hints: hints}
	}
	return json.RawMessage(trimmed), hints, nil
}

func (c *Client) CreateOrder(ctx context.Context, req quefacontract.CreateOrderRequest, idempotencyKey string) (*quefacontract.OrderSnapshot, error) {
	raw, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	body, statusCode, hints, err := c.do(ctx, http.MethodPost, ordersPath, nil, raw, idempotencyKey)
	if err != nil {
		return nil, err
	}
	snap, err := parseOrderSnapshot(body)
	if err != nil {
		return nil, &quefacontract.APIError{StatusCode: statusCode, Category: "invalid_response", Hints: hints}
	}
	snap.AcceptedAsync = statusCode == http.StatusAccepted
	snap.Hints = hints
	return snap, nil
}

// CreateOrderPaymentCode calls POST /v1/orders/{order_id}/payment-code with empty JSON object "{}"
// to obtain the official white-label Alipay payment_code and qr_image_data_url without exposing tibo.ink.
func (c *Client) CreateOrderPaymentCode(ctx context.Context, upstreamOrderID string, idempotencyKey string) (*quefacontract.PaymentCodeSnapshot, error) {
	id := strings.TrimSpace(upstreamOrderID)
	if id == "" {
		return nil, &quefacontract.APIError{Category: "invalid_response"}
	}
	path := ordersPath + "/" + url.PathEscape(id) + "/payment-code"
	body, statusCode, hints, err := c.do(ctx, http.MethodPost, path, nil, []byte("{}"), idempotencyKey)
	if err != nil {
		return nil, err
	}
	m, err := unwrapData(body)
	if err != nil {
		return nil, &quefacontract.APIError{StatusCode: statusCode, Category: "invalid_response", Hints: hints}
	}
	payCode := readString(m, "payment_code")
	qrDataURL := readString(m, "qr_image_data_url")
	if payCode == "" && qrDataURL == "" {
		return nil, &quefacontract.APIError{StatusCode: statusCode, Category: "invalid_response", Hints: hints}
	}
	return &quefacontract.PaymentCodeSnapshot{
		OrderID:         readString(m, "order_id"),
		MerchantOrderNo: readString(m, "merchant_order_no"),
		Amount:          readDecimalPtr(m, "amount"),
		Currency:        strings.ToUpper(readString(m, "currency")),
		PaymentStatus:   strings.ToLower(readString(m, "payment_status")),
		PaymentCodeType: strings.ToLower(readString(m, "payment_code_type")),
		PaymentCode:     payCode,
		QRImageDataURL:  qrDataURL,
		ExpiresAt:       readTimePtr(m, "expires_at"),
		Hints:           hints,
	}, nil
}

func (c *Client) GetOrder(ctx context.Context, upstreamOrderID string) (*quefacontract.OrderSnapshot, error) {
	id := strings.TrimSpace(upstreamOrderID)
	if id == "" {
		return nil, &quefacontract.APIError{Category: "invalid_response"}
	}
	path := ordersPath + "/" + url.PathEscape(id)
	body, statusCode, hints, err := c.do(ctx, http.MethodGet, path, nil, nil, "")
	if err != nil {
		return nil, err
	}
	snap, err := parseOrderSnapshot(body)
	if err != nil {
		return nil, &quefacontract.APIError{StatusCode: statusCode, Category: "invalid_response", Hints: hints}
	}
	snap.Hints = hints
	return snap, nil
}

func (c *Client) LookupOrderByMerchantNo(ctx context.Context, merchantOrderNo string) (*quefacontract.OrderSnapshot, error) {
	no := strings.TrimSpace(merchantOrderNo)
	if no == "" {
		return nil, &quefacontract.APIError{Category: "invalid_response"}
	}
	q := url.Values{}
	q.Set("search", no)
	q.Set("limit", "20")
	body, statusCode, hints, err := c.do(ctx, http.MethodGet, ordersPath, q, nil, "")
	if err != nil {
		return nil, err
	}
	snap, err := parseOrderLookupSnapshot(body, no)
	if err != nil {
		return nil, &quefacontract.APIError{StatusCode: statusCode, Category: "invalid_response", Hints: hints}
	}
	if snap == nil {
		return nil, &quefacontract.APIError{StatusCode: http.StatusNotFound, Category: "not_found", Hints: hints}
	}
	snap.Hints = hints
	return snap, nil
}

func (c *Client) GetRedemption(ctx context.Context, redemptionID string) (*quefacontract.RedemptionSnapshot, error) {
	id := strings.TrimSpace(redemptionID)
	if id == "" {
		return nil, &quefacontract.APIError{Category: "invalid_response"}
	}
	path := redemptionsPath + "/" + url.PathEscape(id)
	body, statusCode, hints, err := c.do(ctx, http.MethodGet, path, nil, nil, "")
	if err != nil {
		return nil, err
	}
	snap, err := parseRedemptionSnapshot(body)
	if err != nil {
		return nil, &quefacontract.APIError{StatusCode: statusCode, Category: "invalid_response", Hints: hints}
	}
	snap.Hints = hints
	return snap, nil
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, rawBody []byte, idempotencyKey string) ([]byte, int, quefacontract.PollHints, error) {
	canonicalQuery := CanonicalQuery(query)
	targetURL := c.baseURL + path
	if canonicalQuery != "" {
		targetURL += "?" + canonicalQuery
	}

	var bodyReader io.Reader
	if len(rawBody) > 0 {
		bodyReader = bytes.NewReader(rawBody)
	}
	req, err := http.NewRequestWithContext(ctx, method, targetURL, bodyReader)
	if err != nil {
		return nil, 0, quefacontract.PollHints{}, &quefacontract.APIError{Category: "network_error", Retryable: true}
	}

	ts := strconv.FormatInt(c.now().Unix(), 10)
	nonce := c.nonce()
	idem := strings.TrimSpace(idempotencyKey)
	canonical := BuildCanonicalString(method, path, canonicalQuery, ts, nonce, c.keyID, idem, rawBody)
	sig := SignRequest(c.clientSecret, canonical)

	req.Header.Set("Accept", "application/json")
	if len(rawBody) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Partner-Id", c.partnerID)
	req.Header.Set("X-Key-Id", c.keyID)
	req.Header.Set("X-Timestamp", ts)
	req.Header.Set("X-Nonce", nonce)
	req.Header.Set("X-Signature", sig)
	if idem != "" {
		req.Header.Set("Idempotency-Key", idem)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, quefacontract.PollHints{}, &quefacontract.APIError{
			Category:  "network_error",
			Message:   sanitizeDiag(err.Error()),
			Retryable: true,
		}
	}
	defer resp.Body.Close()

	hints := parsePollHints(resp.Header, c.now())
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		loc := sanitizeDiag(resp.Header.Get("Location"))
		return nil, resp.StatusCode, hints, &quefacontract.APIError{
			StatusCode: resp.StatusCode,
			Category:   "redirect_blocked",
			Message:    loc,
			Hints:      hints,
		}
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil || len(data) > maxResponseBytes {
		msg := "response body too large"
		if err != nil {
			msg = sanitizeDiag(err.Error())
		}
		return nil, resp.StatusCode, hints, &quefacontract.APIError{
			StatusCode: resp.StatusCode,
			Category:   "invalid_response",
			Message:    msg,
			Hints:      hints,
		}
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.StatusCode, hints, classifyError(resp.StatusCode, data, hints)
	}
	return data, resp.StatusCode, hints, nil
}

func parsePollHints(h http.Header, now time.Time) quefacontract.PollHints {
	var hints quefacontract.PollHints
	if v := strings.TrimSpace(h.Get("Retry-After")); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
			hints.RetryAfter = time.Duration(secs) * time.Second
		} else if t, err := http.ParseTime(v); err == nil && t.After(now) {
			hints.RetryAfter = t.Sub(now)
		}
	}
	if v := strings.TrimSpace(h.Get("X-Poll-After")); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
			hints.PollAfter = time.Duration(secs) * time.Second
		}
	}
	if v := strings.ToLower(strings.TrimSpace(h.Get("X-Poll-Stop"))); v == "1" || v == "true" || v == "yes" {
		hints.PollStop = true
	}
	if v, err := strconv.Atoi(strings.TrimSpace(h.Get("X-RateLimit-Limit"))); err == nil && v >= 0 {
		hints.RateLimitLimit = v
	}
	if v, err := strconv.Atoi(strings.TrimSpace(h.Get("X-RateLimit-Remaining"))); err == nil && v >= 0 {
		hints.RateLimitRemaining = v
	}
	if v, err := strconv.ParseInt(strings.TrimSpace(h.Get("X-RateLimit-Reset")), 10, 64); err == nil && v > 0 {
		t := time.Unix(v, 0)
		hints.RateLimitReset = &t
	}
	return hints
}

func classifyError(status int, body []byte, hints quefacontract.PollHints) *quefacontract.APIError {
	code, msg := extractErrorCodeAndMessage(body)
	lowerCode := strings.ToLower(code)
	apiErr := &quefacontract.APIError{
		StatusCode: status,
		Code:       code,
		Message:    sanitizeDiag(msg),
		Hints:      hints,
	}
	switch {
	case status == http.StatusUnauthorized:
		apiErr.Category = "auth"
	case status == http.StatusForbidden:
		apiErr.Category = "forbidden"
	case status == http.StatusNotFound:
		apiErr.Category = "not_found"
	case status == http.StatusConflict && (lowerCode == "idempotency_in_progress" || strings.Contains(lowerCode, "idempotency")):
		apiErr.Category = "idempotency_in_progress"
		apiErr.Retryable = true
	case status == http.StatusTooManyRequests:
		apiErr.Category = "rate_limited"
		apiErr.Retryable = true
	case strings.Contains(lowerCode, "insufficient_balance") || strings.Contains(lowerCode, "balance_insufficient") || status == http.StatusPaymentRequired:
		apiErr.Category = "insufficient_balance"
	case status == http.StatusServiceUnavailable || status >= 500:
		apiErr.Category = "upstream_error"
		apiErr.Retryable = true
	default:
		apiErr.Category = "upstream_error"
	}
	return apiErr
}

func extractErrorCodeAndMessage(body []byte) (string, string) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return "", ""
	}
	var code, msg string
	for _, key := range []string{"code", "error_code", "result_code", "error"} {
		if v, ok := raw[key]; ok {
			switch val := v.(type) {
			case string:
				if code == "" {
					if trimmed := strings.TrimSpace(val); trimmed != "" {
						code = trimmed
					}
				}
			case map[string]any:
				if code == "" {
					if nested, ok := val["code"].(string); ok && strings.TrimSpace(nested) != "" {
						code = strings.TrimSpace(nested)
					}
				}
				if msg == "" {
					if nested, ok := val["message"].(string); ok && strings.TrimSpace(nested) != "" {
						msg = strings.TrimSpace(nested)
					}
				}
			}
		}
	}
	if msg == "" {
		for _, key := range []string{"message", "msg", "detail"} {
			if s, ok := raw[key].(string); ok && strings.TrimSpace(s) != "" {
				msg = strings.TrimSpace(s)
				break
			}
		}
	}
	return code, msg
}

func sanitizeDiag(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(s, "\r", " "), "\n", " "))
	if len(s) > 200 {
		return s[:200]
	}
	return s
}

func unwrapData(raw []byte) (map[string]json.RawMessage, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, err
	}
	if dataRaw, ok := top["data"]; ok && len(bytes.TrimSpace(dataRaw)) > 0 && bytes.TrimSpace(dataRaw)[0] == '{' {
		var nested map[string]json.RawMessage
		if err := json.Unmarshal(dataRaw, &nested); err == nil {
			return nested, nil
		}
	}
	if orderRaw, ok := top["order"]; ok && len(bytes.TrimSpace(orderRaw)) > 0 && bytes.TrimSpace(orderRaw)[0] == '{' {
		var nested map[string]json.RawMessage
		if err := json.Unmarshal(orderRaw, &nested); err == nil {
			return nested, nil
		}
	}
	return top, nil
}

func parseOrderSnapshot(raw []byte) (*quefacontract.OrderSnapshot, error) {
	m, err := unwrapData(raw)
	if err != nil {
		return nil, err
	}
	orderID := readString(m, "order_id", "id")
	paymentStatus := strings.ToLower(readString(m, "payment_status"))
	fulfillmentStatus := strings.ToLower(readString(m, "fulfillment_status"))
	rawStatus := strings.ToLower(readString(m, "status"))
	if orderID == "" && paymentStatus == "" && rawStatus == "" {
		return nil, fmt.Errorf("missing order_id and status")
	}

	// Derive effective lifecycle status from official OpenAPI payment_status + fulfillment_status.
	status := rawStatus
	if paymentStatus != "" {
		switch paymentStatus {
		case "refunded", "partially_refunded", "expired", "closed", "pending":
			status = paymentStatus
		case "paid":
			if fulfillmentStatus != "" {
				status = fulfillmentStatus
			} else {
				status = "paid"
			}
		default:
			if status == "" {
				status = paymentStatus
			}
		}
	}

	latestRedemption := readString(m, "latest_redemption_id", "redemption_id")
	if latestRedemption == "" {
		if arRaw, ok := m["auto_recharge"]; ok && len(bytes.TrimSpace(arRaw)) > 0 && bytes.TrimSpace(arRaw)[0] == '{' {
			var ar map[string]json.RawMessage
			if json.Unmarshal(arRaw, &ar) == nil {
				latestRedemption = readString(ar, "redemption_id")
			}
		}
	}

	snap := &quefacontract.OrderSnapshot{
		OrderID:                   orderID,
		MerchantOrderNo:           readString(m, "merchant_order_no"),
		ProductCode:               readString(m, "product_code"),
		Status:                    status,
		PaymentStatus:             paymentStatus,
		FulfillmentStatus:         fulfillmentStatus,
		FulfillmentPaused:         readBool(m, "fulfillment_paused"),
		SyncMark:                  strings.ToLower(readString(m, "sync_mark")),
		FallbackRechargeAvailable: readBool(m, "fallback_recharge_available"),
		ResultCode:                readString(m, "result_code", "fulfillment_failure_code"),
		CollectionMode:            strings.ToLower(readString(m, "collection_mode")),
		DeliveryMode:              strings.ToLower(readString(m, "delivery_mode")),
		SaleAmount:                readDecimalPtr(m, "sale_amount"),
		PaidAmount:                readDecimalPtr(m, "paid_amount", "pay_amount"),
		Currency:                  strings.ToUpper(readString(m, "currency", "settlement_currency")),
		PayURL:                    readString(m, "pay_url", "payment_url", "cashier_url", "qr_payload"),
		QRCode:                    readString(m, "payment_code", "qr_code", "qrcode", "payment_qr", "qr_image_data_url"),
		PayExpiresAt:              readTimePtr(m, "pay_expires_at", "expires_at"),
		FulfillmentURL:            readString(m, "fulfillment_url"),
		LatestRedemptionID:        latestRedemption,
		AttemptNo:                 readInt(m, "attempt_no"),
		ProgressVersion:           readInt64(m, "progress_version"),
		ProgressStage:             readString(m, "progress_stage"),
		ProgressMessage:           readString(m, "progress_message", "fulfillment_message", "message"),
		ProgressUpdatedAt:         readTimePtr(m, "progress_updated_at"),
		RetryAllowed:              readBool(m, "retry_allowed"),
		NextAction:                strings.ToLower(readString(m, "next_action", "fulfillment_next_action")),
		RecoveryAction:            strings.ToLower(readString(m, "recovery_action")),
		RefundRef:                 readString(m, "refund_ref", "refund_id"),
		ProcurementRefundRef:      readString(m, "procurement_refund_ref"),
	}
	return snap, nil
}

func parseOrderLookupSnapshot(raw []byte, merchantOrderNo string) (*quefacontract.OrderSnapshot, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("empty body")
	}
	if trimmed[0] == '[' {
		return pickMatchingOrder(trimmed, merchantOrderNo)
	}
	var env map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &env); err != nil {
		return nil, err
	}
	for _, key := range []string{"orders", "items", "list", "data"} {
		candidate, ok := env[key]
		if !ok || len(bytes.TrimSpace(candidate)) == 0 {
			continue
		}
		ct := bytes.TrimSpace(candidate)
		if ct[0] == '[' {
			return pickMatchingOrder(ct, merchantOrderNo)
		}
		if ct[0] == '{' && key == "data" {
			var inner map[string]json.RawMessage
			if err := json.Unmarshal(ct, &inner); err == nil {
				for _, ik := range []string{"orders", "items", "list"} {
					if ic, ok := inner[ik]; ok && len(bytes.TrimSpace(ic)) > 0 && bytes.TrimSpace(ic)[0] == '[' {
						return pickMatchingOrder(ic, merchantOrderNo)
					}
				}
			}
		}
	}
	return parseOrderSnapshot(trimmed)
}

func pickMatchingOrder(listJSON []byte, merchantOrderNo string) (*quefacontract.OrderSnapshot, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(listJSON, &items); err != nil {
		return nil, err
	}
	for _, item := range items {
		snap, err := parseOrderSnapshot(item)
		if err != nil {
			continue
		}
		if snap.MerchantOrderNo == "" || strings.EqualFold(snap.MerchantOrderNo, merchantOrderNo) {
			return snap, nil
		}
	}
	return nil, nil
}

func parseRedemptionSnapshot(raw []byte) (*quefacontract.RedemptionSnapshot, error) {
	m, err := unwrapData(raw)
	if err != nil {
		return nil, err
	}
	redemptionID := readString(m, "redemption_id", "id")
	status := strings.ToLower(readString(m, "status"))
	if redemptionID == "" && status == "" {
		return nil, fmt.Errorf("missing redemption_id and status")
	}
	return &quefacontract.RedemptionSnapshot{
		RedemptionID:       redemptionID,
		OrderID:            readString(m, "order_id"),
		LatestRedemptionID: readString(m, "latest_redemption_id"),
		Status:             status,
		ResultCode:         readString(m, "result_code"),
		AttemptNo:          readInt(m, "attempt_no"),
		ProgressVersion:    readInt64(m, "progress_version"),
		ProgressStage:      readString(m, "progress_stage"),
		ProgressMessage:    readString(m, "progress_message", "message"),
		ProgressUpdatedAt:  readTimePtr(m, "progress_updated_at"),
		RetryAllowed:       readBool(m, "retry_allowed"),
		NextAction:         strings.ToLower(readString(m, "next_action")),
		RecoveryAction:     strings.ToLower(readString(m, "recovery_action")),
		FulfillmentURL:     readString(m, "fulfillment_url"),
	}, nil
}

func readString(m map[string]json.RawMessage, keys ...string) string {
	for _, k := range keys {
		raw, ok := m[k]
		if !ok || len(raw) == 0 {
			continue
		}
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			if trimmed := strings.TrimSpace(s); trimmed != "" {
				return trimmed
			}
		}
		var n json.Number
		if err := json.Unmarshal(raw, &n); err == nil {
			if trimmed := strings.TrimSpace(n.String()); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

func readDecimalPtr(m map[string]json.RawMessage, keys ...string) *decimal.Decimal {
	for _, k := range keys {
		raw, ok := m[k]
		if !ok || len(raw) == 0 {
			continue
		}
		trimmed := strings.TrimSpace(string(raw))
		if trimmed == "" || trimmed == "null" {
			continue
		}
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			if d, err := decimal.NewFromString(strings.TrimSpace(s)); err == nil {
				v := d.Round(2)
				return &v
			}
		}
		if d, err := decimal.NewFromString(trimmed); err == nil {
			v := d.Round(2)
			return &v
		}
	}
	return nil
}

func readInt(m map[string]json.RawMessage, keys ...string) int {
	for _, k := range keys {
		raw, ok := m[k]
		if !ok || len(raw) == 0 {
			continue
		}
		var v int
		if err := json.Unmarshal(raw, &v); err == nil {
			return v
		}
	}
	return 0
}

func readInt64(m map[string]json.RawMessage, keys ...string) int64 {
	for _, k := range keys {
		raw, ok := m[k]
		if !ok || len(raw) == 0 {
			continue
		}
		var v int64
		if err := json.Unmarshal(raw, &v); err == nil {
			return v
		}
	}
	return 0
}

func readBool(m map[string]json.RawMessage, keys ...string) bool {
	for _, k := range keys {
		raw, ok := m[k]
		if !ok || len(raw) == 0 {
			continue
		}
		var v bool
		if err := json.Unmarshal(raw, &v); err == nil {
			return v
		}
	}
	return false
}

func readTimePtr(m map[string]json.RawMessage, keys ...string) *time.Time {
	for _, k := range keys {
		raw, ok := m[k]
		if !ok || len(raw) == 0 {
			continue
		}
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			s = strings.TrimSpace(s)
			for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
				if t, err := time.Parse(layout, s); err == nil {
					return &t
				}
			}
		}
		var unix int64
		if err := json.Unmarshal(raw, &unix); err == nil && unix > 0 {
			t := time.Unix(unix, 0).UTC()
			return &t
		}
	}
	return nil
}
