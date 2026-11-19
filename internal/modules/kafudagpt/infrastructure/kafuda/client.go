package kafuda

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
)

const (
	maxResponseBytes       = 2 << 20
	maxOrderRejectionBytes = 4 << 10
	orderPath              = "/api/v1/gpt/orders"
)

var safeKey = regexp.MustCompile(`^[a-zA-Z0-9._:-]{1,80}$`)

type Client struct {
	baseURL             string
	token               string
	http                *http.Client
	orderLookupMu       sync.Mutex
	orderLookupResumeAt time.Time
}

func (c *Client) String() string { return "Kafuda GPT API client [credentials redacted]" }

func (c *Client) GoString() string { return c.String() }

var _ kafudacontract.Gateway = (*Client)(nil)

// New requires HTTPS. Requests never follow redirects, including same-origin
// redirects, so a redirect cannot forward a buyer's Session to another endpoint.
func New(baseURL, token string) (*Client, error) {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("kafuda base URL must be an HTTPS origin")
	}
	if strings.TrimSpace(token) == "" || strings.ContainsAny(token, "\r\n") {
		return nil, errors.New("kafuda API token is required")
	}
	return &Client{
		baseURL: strings.TrimRight(u.String(), "/"), token: token,
		http: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}, nil
}

func (c *Client) Catalog(ctx context.Context) (json.RawMessage, error) {
	var result json.RawMessage
	if err := c.request(ctx, http.MethodGet, "/api/v1/gpt/catalog", nil, "", &result); err != nil {
		return nil, err
	}
	if len(result) == 0 || bytes.Equal(bytes.TrimSpace(result), []byte("null")) {
		return nil, apiError(0, "invalid_response")
	}
	return result, nil
}

func (c *Client) Check(ctx context.Context, input kafudacontract.CheckRequest) (*kafudacontract.CheckResult, error) {
	var wire struct {
		CheckID    string          `json:"check_id"`
		PayMode    string          `json:"pay_mode"`
		Country    string          `json:"country"`
		RegionLock json.RawMessage `json:"region_lock"`
		Account    struct {
			Email string `json:"email"`
		} `json:"account"`
		Plans []struct {
			kafudacontract.Quote
			PriceCents *int64 `json:"price_cents"`
			AutoCard   *struct {
				ProductCode  string `json:"product_code"`
				OpenFeeCents *int64 `json:"open_fee_cents"`
				LoadCents    *int64 `json:"load_cents"`
				TotalCents   *int64 `json:"total_cents"`
			} `json:"auto_card"`
		} `json:"plans"`
	}
	if err := c.request(ctx, http.MethodPost, "/api/v1/gpt/check", input, "", &wire); err != nil {
		return nil, err
	}
	// An explicit null means unlocked. An omitted result cannot establish eligibility.
	if wire.CheckID == "" || wire.Country != input.Country || wire.PayMode != input.PayMode || len(wire.RegionLock) == 0 || len(wire.Plans) == 0 {
		return nil, apiError(0, "invalid_response")
	}
	plans := make([]kafudacontract.Quote, len(wire.Plans))
	for i, plan := range wire.Plans {
		plans[i] = plan.Quote
		if plan.PriceCents == nil {
			continue
		}
		plans[i].PriceCents = *plan.PriceCents
		// A missing amount cannot become a known zero in the saved quote.
		if card := plan.AutoCard; card != nil && card.OpenFeeCents != nil && card.LoadCents != nil && card.TotalCents != nil {
			plans[i].AutoCard = &kafudacontract.AutoCard{
				ProductCode: card.ProductCode, OpenFeeCents: *card.OpenFeeCents,
				LoadCents: *card.LoadCents, TotalCents: *card.TotalCents,
			}
		}
	}
	return &kafudacontract.CheckResult{
		CheckID: wire.CheckID, PayMode: wire.PayMode, Country: wire.Country,
		RegionLocked: !bytes.Equal(bytes.TrimSpace(wire.RegionLock), []byte("null")),
		MaskedEmail:  maskEmail(wire.Account.Email), Plans: plans,
	}, nil
}

func (c *Client) CreateOrder(ctx context.Context, input kafudacontract.CreateOrderRequest) (*kafudacontract.OrderResult, error) {
	if !safeKey.MatchString(input.Reference) || strings.TrimSpace(input.QuoteID) == "" {
		return nil, apiError(0, "invalid_request")
	}
	return c.order(ctx, http.MethodPost, orderPath, input, input.Reference)
}

func (c *Client) GetOrder(ctx context.Context, id uint64) (*kafudacontract.OrderResult, error) {
	if id == 0 {
		return nil, apiError(0, "invalid_request")
	}
	// Subsequent worker batches may contain different orders while the
	// supplier's numeric lookup limit is still in effect.
	c.orderLookupMu.Lock()
	retryAfter := time.Until(c.orderLookupResumeAt)
	c.orderLookupMu.Unlock()
	if retryAfter > 0 {
		return nil, &kafudacontract.APIError{HTTPStatus: http.StatusTooManyRequests, Code: "rate_limited", RetryAfter: retryAfter}
	}
	result, err := c.order(ctx, http.MethodGet, orderPath+"/"+strconv.FormatUint(id, 10), nil, "")
	var limit *kafudacontract.APIError
	if errors.As(err, &limit) && limit.HTTPStatus == http.StatusTooManyRequests && limit.RetryAfter > 0 {
		resumeAt := time.Now().Add(limit.RetryAfter)
		c.orderLookupMu.Lock()
		if resumeAt.After(c.orderLookupResumeAt) {
			c.orderLookupResumeAt = resumeAt
		}
		c.orderLookupMu.Unlock()
	}
	if err == nil && result.ID != id {
		return nil, apiError(0, "invalid_response")
	}
	return result, err
}

func (c *Client) order(ctx context.Context, method, path string, input any, key string) (*kafudacontract.OrderResult, error) {
	var wire struct {
		ID               uint64     `json:"id"`
		Reference        string     `json:"reference"`
		Product          string     `json:"product"`
		Plan             string     `json:"plan"`
		Country          string     `json:"country"`
		Status           string     `json:"status"`
		AccountEmail     string     `json:"account_email"`
		ChargedUSDCents  *int64     `json:"charged_usd_cents"`
		RenewalCancelled *bool      `json:"renewal_cancelled"`
		CompletedAt      *time.Time `json:"completed_at"`
	}
	if err := c.request(ctx, method, path, input, key, &wire); err != nil {
		return nil, err
	}
	if wire.ID == 0 || !validStatus(wire.Status) {
		return nil, apiError(0, "invalid_response")
	}
	return &kafudacontract.OrderResult{
		ID: wire.ID, Reference: wire.Reference, Product: wire.Product, Plan: wire.Plan, Country: wire.Country,
		Status: wire.Status, MaskedEmail: maskEmail(wire.AccountEmail), ChargedUSDCents: wire.ChargedUSDCents,
		RenewalCancelled: wire.RenewalCancelled, CompletedAt: wire.CompletedAt,
	}, nil
}

func (c *Client) request(ctx context.Context, method, path string, input any, key string, target any) error {
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return apiError(0, "invalid_request")
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return apiError(0, "invalid_request")
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	if method == http.MethodPost && path == orderPath {
		// Preserve the original key while preventing transport replay of a
		// potentially accepted order after a connection failure.
		req.GetBody = nil
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return apiError(0, "transport_error")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if method == http.MethodPost && path == orderPath && resp.StatusCode == http.StatusBadRequest {
			if rejection := readOrderRejection(resp.Body); rejection != nil {
				return rejection
			}
		}
		// No supplier error value is allowed to reach diagnostics.
		code := "upstream_error"
		switch {
		case resp.StatusCode == http.StatusTooManyRequests:
			code = "rate_limited"
		case resp.StatusCode >= 500:
			code = "service_unavailable"
		}
		e := apiError(resp.StatusCode, code)
		// Interpret the reset in Unix seconds and bound the accepted delay.
		if reset, parseErr := strconv.ParseInt(resp.Header.Get("x-ratelimit-reset"), 10, 64); parseErr == nil {
			if delay := time.Until(time.Unix(reset, 0)); delay > 0 && delay <= time.Hour {
				e.RetryAfter = delay
			}
		}
		return e
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil || len(data) > maxResponseBytes {
		return apiError(resp.StatusCode, "invalid_response")
	}
	var envelope struct {
		Data  json.RawMessage `json:"data"`
		Error json.RawMessage `json:"error"`
	}
	// A success status with an error payload cannot confirm a catalog or order result.
	if json.Unmarshal(data, &envelope) != nil ||
		(len(envelope.Error) != 0 && !bytes.Equal(bytes.TrimSpace(envelope.Error), []byte("null"))) ||
		len(envelope.Data) == 0 || bytes.Equal(bytes.TrimSpace(envelope.Data), []byte("null")) || json.Unmarshal(envelope.Data, target) != nil {
		return apiError(resp.StatusCode, "invalid_response")
	}
	return nil
}

// readOrderRejection recognizes only documented reasons that reject creation.
// Return a local sentinel, never the supplier's code, message, or details.
func readOrderRejection(body io.Reader) error {
	data, err := io.ReadAll(io.LimitReader(body, maxOrderRejectionBytes+1))
	if err != nil || len(data) > maxOrderRejectionBytes {
		return nil
	}
	var wire struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(data, &wire) != nil || len(wire.Data) > 0 && !bytes.Equal(wire.Data, []byte("null")) {
		return nil
	}
	switch wire.Error.Code {
	case "quote_expired":
		return kafudacontract.ErrCreateQuoteExpired
	case "insufficient_balance":
		return kafudacontract.ErrMerchantBalanceInsufficient
	default:
		return nil
	}
}

func apiError(status int, code string) *kafudacontract.APIError {
	return &kafudacontract.APIError{HTTPStatus: status, Code: code}
}

func validStatus(status string) bool {
	switch status {
	case "submitting", "processing", "completed", "failed", "cancelled", "needs_review":
		return true
	default:
		return false
	}
}

func maskEmail(email string) string {
	// Do not retain the full domain either: the API can return arbitrary PII.
	local, _, ok := strings.Cut(strings.TrimSpace(email), "@")
	if !ok || local == "" {
		return ""
	}
	masked := string([]rune(local)[:1]) + "***@***"
	if !kafudadomain.IsMaskedAccountEmail(masked) {
		return ""
	}
	return masked
}
