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
	"time"

	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
)

const maxResponseBytes = 2 << 20

var safeKey = regexp.MustCompile(`^[a-zA-Z0-9._:-]{1,80}$`)
var safeCode = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

type Client struct {
	baseURL string
	token   string
	http    *http.Client
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
		Plans []kafudacontract.Quote `json:"plans"`
	}
	if err := c.request(ctx, http.MethodPost, "/api/v1/gpt/check", input, "", &wire); err != nil {
		return nil, err
	}
	if wire.CheckID == "" || wire.Country != input.Country || wire.PayMode != input.PayMode || len(wire.Plans) == 0 {
		return nil, apiError(0, "invalid_response")
	}
	return &kafudacontract.CheckResult{
		CheckID: wire.CheckID, PayMode: wire.PayMode, Country: wire.Country,
		RegionLocked: len(wire.RegionLock) > 0 && !bytes.Equal(bytes.TrimSpace(wire.RegionLock), []byte("null")),
		MaskedEmail:  maskEmail(wire.Account.Email), Plans: wire.Plans,
	}, nil
}

func (c *Client) CreateOrder(ctx context.Context, input kafudacontract.CreateOrderRequest) (*kafudacontract.OrderResult, error) {
	if !safeKey.MatchString(input.Reference) || strings.TrimSpace(input.QuoteID) == "" {
		return nil, apiError(0, "invalid_request")
	}
	return c.order(ctx, http.MethodPost, "/api/v1/gpt/orders", input, input.Reference)
}

func (c *Client) GetOrder(ctx context.Context, id uint64) (*kafudacontract.OrderResult, error) {
	if id == 0 {
		return nil, apiError(0, "invalid_request")
	}
	result, err := c.order(ctx, http.MethodGet, "/api/v1/gpt/orders/"+strconv.FormatUint(id, 10), nil, "")
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
	resp, err := c.http.Do(req)
	if err != nil {
		return apiError(0, "transport_error")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil || len(data) > maxResponseBytes {
		return apiError(resp.StatusCode, "invalid_response")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var envelope struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		_ = json.Unmarshal(data, &envelope)
		e := apiError(resp.StatusCode, envelope.Error.Code)
		// Kafuda documents x-ratelimit-reset as Unix seconds.
		if reset, parseErr := strconv.ParseInt(resp.Header.Get("x-ratelimit-reset"), 10, 64); parseErr == nil {
			if delay := time.Until(time.Unix(reset, 0)); delay > 0 && delay <= time.Hour {
				e.RetryAfter = delay
			}
		}
		return e
	}
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(data, &envelope) != nil || len(envelope.Data) == 0 || bytes.Equal(bytes.TrimSpace(envelope.Data), []byte("null")) || json.Unmarshal(envelope.Data, target) != nil {
		return apiError(resp.StatusCode, "invalid_response")
	}
	return nil
}

func apiError(status int, code string) *kafudacontract.APIError {
	if !safeCode.MatchString(code) {
		code = "upstream_error"
	}
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
	return string([]rune(local)[:1]) + "***@***"
}
