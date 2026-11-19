package kafuda

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	kafudaapp "github.com/dujiao-next/internal/modules/kafudagpt/application"
	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
)

func TestCatalogHTTPAndParserAcceptRepositorySupplierResponse(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "..", "docs", "gpt_catalog_data.json"))
	if err != nil {
		t.Fatal(err)
	}
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/gpt/catalog" || r.Header.Get("Authorization") != "Bearer test-only-token" {
			t.Error("incorrect catalog request")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	})
	payload, err := c.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	catalog, err := kafudaapp.ParseCatalog(payload, "default", now, now.Add(time.Hour))
	if err != nil || len(catalog.Options) != 60 || len(catalog.Cards) != 11 || !catalog.Available("plus", "PH", now) {
		t.Fatalf("supplier fixture was rejected: %+v, %v", catalog, err)
	}
}

func testClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	c, err := New(server.URL, "test-only-token")
	if err != nil {
		t.Fatal(err)
	}
	transport := server.Client().Transport
	c.http.Transport = transport
	return c
}

func TestCheckUsesEphemeralBodyAndMasksAccount(t *testing.T) {
	session := `{"sessionToken":"test-only-session","user":{"email":"buyer@example.com"}}`
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/gpt/check" || r.URL.RawQuery != "" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-only-token" || r.Header.Get("Idempotency-Key") != "" {
			t.Error("unexpected check authentication or idempotency header")
		}
		var body kafudacontract.CheckRequest
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.Session != session || body.Country != "PH" || body.ProductCode != "P5378XB" || body.PayMode != "auto_card" {
			t.Error("check body does not match input")
		}
		fmt.Fprint(w, `{"data":{"check_id":"chk_1","pay_mode":"auto_card","country":"PH","region_lock":null,"account":{"email":"buyer@example.com"},"plans":[{"plan":"plus","status":"available","kind":"new","quote_id":"quo_1","expires_at":"2026-11-16T10:00:00Z","price_cents":30,"auto_card":{"product_code":"P5378XB","open_fee_cents":50,"load_cents":1700,"total_cents":1780}}]}}`)
	})
	input := kafudacontract.CheckRequest{Country: "PH", PayMode: "auto_card", ProductCode: "P5378XB", Session: session}
	got, err := c.Check(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if got.MaskedEmail != "b***@***" || got.RegionLocked || len(got.Plans) != 1 || got.Plans[0].PriceCents != 30 ||
		got.Plans[0].AutoCard == nil || got.Plans[0].AutoCard.TotalCents != 1780 {
		t.Fatalf("unexpected normalized check result: %+v", got)
	}
	if strings.Contains(fmt.Sprintf("%+v %#v", input, input), "test-only-session") {
		t.Fatal("formatting a check request exposed Session")
	}
	if strings.Contains(fmt.Sprintf("%+v %#v", c, c), "test-only-token") {
		t.Fatal("formatting the client exposed its merchant token")
	}
}

func TestCheckDistinguishesZeroAndMissingQuotedAmounts(t *testing.T) {
	const quote = `{"plan":"plus","status":"available","quote_id":"quote","price_cents":0,"auto_card":{"product_code":"P5378XB","open_fee_cents":100,"load_cents":0,"total_cents":100}}`
	for _, tc := range []struct {
		name, remove, replace string
		complete              bool
	}{
		{name: "explicit_zero", complete: true},
		{name: "missing_service_fee", remove: `"price_cents":0,`},
		{name: "null_service_fee", remove: `"price_cents":0`, replace: `"price_cents":null`},
		{name: "missing_open_fee", remove: `"open_fee_cents":100,`},
		{name: "missing_card_load", remove: `"load_cents":0,`},
		{name: "missing_total", remove: `,"total_cents":100`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprintf(w, `{"data":{"check_id":"check","pay_mode":"auto_card","country":"PH","plans":[%s]}}`,
					strings.Replace(quote, tc.remove, tc.replace, 1))
			})
			got, err := c.Check(t.Context(), kafudacontract.CheckRequest{Country: "PH", PayMode: "auto_card"})
			if err != nil || got == nil || len(got.Plans) != 1 || got.Plans[0].Plan != "plus" || got.Plans[0].QuoteID != "quote" {
				t.Fatalf("unexpected check result: %+v, %v", got, err)
			}
			plan := got.Plans[0]
			if (plan.AutoCard != nil) != tc.complete {
				t.Fatalf("complete quote amounts = %v, want %v", plan.AutoCard != nil, tc.complete)
			}
			if tc.complete && (plan.PriceCents != 0 || plan.AutoCard.LoadCents != 0 || plan.AutoCard.OpenFeeCents != 100 || plan.AutoCard.TotalCents != 100) {
				t.Fatal("explicit fee amounts did not round-trip")
			}
		})
	}
}

func TestCreateOrderHasStableIdempotencyAndNoCredential(t *testing.T) {
	calls := 0
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Idempotency-Key") != "kgpt.order-1" {
			t.Error("missing original idempotency key")
		}
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil || len(body) != 2 || body["quote_id"] != "quo_1" || body["reference"] != "kgpt.order-1" {
			t.Error("unexpected order body")
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"data":{"id":100001,"reference":"kgpt.order-1","product":"gpt","plan":"plus","country":"PH","status":"processing","account_email":"buyer@example.com"}}`)
	})
	got, err := c.CreateOrder(context.Background(), kafudacontract.CreateOrderRequest{QuoteID: "quo_1", Reference: "kgpt.order-1"})
	if err != nil || got.ID != 100001 || got.MaskedEmail != "b***@***" || calls != 1 {
		t.Fatalf("create = (%+v, %v), calls=%d", got, err, calls)
	}
}

func TestCreateOrderDoesNotReplayAfterDroppedResponse(t *testing.T) {
	input := kafudacontract.CreateOrderRequest{QuoteID: "quo_1", Reference: "kgpt.order-1"}
	var calls atomic.Int32
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/gpt/catalog" {
			fmt.Fprint(w, `{"data":{}}`)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != orderPath || r.Header.Get("Idempotency-Key") != input.Reference {
			t.Error("unexpected order request")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		data, err := io.ReadAll(r.Body)
		var body kafudacontract.CreateOrderRequest
		if err != nil || json.Unmarshal(data, &body) != nil || body != input {
			t.Error("supplier did not receive the complete order body")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if calls.Add(1) != 1 {
			w.WriteHeader(http.StatusConflict)
			return
		}
		// Accept the request and disconnect before sending any response headers.
		conn, _, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Errorf("hijack supplier connection: %v", err)
			return
		}
		_ = conn.Close()
	})
	if _, err := c.Catalog(t.Context()); err != nil {
		t.Fatal(err)
	}
	var reused atomic.Bool
	ctx := httptrace.WithClientTrace(t.Context(), &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
		if info.Reused {
			reused.Store(true)
		}
	}})
	got, err := c.CreateOrder(ctx, input)
	if !reused.Load() {
		t.Fatal("create did not use the warmed connection")
	}
	var apiErr *kafudacontract.APIError
	if got != nil || !errors.As(err, &apiErr) || apiErr.Code != "transport_error" || calls.Load() != 1 {
		t.Fatalf("create = (%+v, %v), supplier calls=%d", got, err, calls.Load())
	}
}

func TestOrderFailureIsNotRetriedAndDoesNotExposePII(t *testing.T) {
	calls := 0
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, `{"error":{"code":"opaque_secret_that_looks_like_a_code","message":"buyer@example.com test-only-session","details":{"session":"test-only-session"}}}`)
	})
	_, err := c.CreateOrder(context.Background(), kafudacontract.CreateOrderRequest{QuoteID: "quo_1", Reference: "order-1"})
	var apiErr *kafudacontract.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "service_unavailable" || calls != 1 {
		t.Fatalf("unexpected error or retry: %v, calls=%d", err, calls)
	}
	if strings.Contains(err.Error(), "buyer") || strings.Contains(err.Error(), "session") || strings.Contains(err.Error(), "opaque_secret") {
		t.Fatal("upstream PII escaped through error")
	}
}

func TestCreateOrderClassifiesDefinitiveRejections(t *testing.T) {
	const expired = `{"error":{"code":"quote_expired","message":"buyer@example.test","details":{"text":"private_fixture"}}}`
	for _, scenario := range []struct {
		name, body string
		status     int
		lookup     bool
		want       error
	}{
		{name: "expired_quote", body: expired, status: 400, want: kafudacontract.ErrCreateQuoteExpired},
		{name: "merchant_balance", body: `{"error":{"code":"insufficient_balance","message":"buyer@example.test"}}`, status: 400, want: kafudacontract.ErrMerchantBalanceInsufficient},
		{name: "quote_used", body: `{"error":{"code":"quote_used"}}`, status: 400},
		{name: "unknown_code", body: `{"error":{"code":"private_fixture","message":"quote_expired"}}`, status: 400},
		{name: "contradictory_data", body: `{"error":{"code":"quote_expired"},"data":{"id":1}}`, status: 400},
		{name: "truncated", body: expired[:len(expired)-1], status: 400},
		{name: "oversized", body: expired + strings.Repeat(" ", 8192), status: 400},
		{name: "different_status", body: expired, status: 409},
		{name: "lookup", body: expired, status: 400, lookup: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var calls atomic.Int32
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(scenario.status)
				fmt.Fprint(w, scenario.body)
			})
			var err error
			if scenario.lookup {
				_, err = c.GetOrder(t.Context(), 1)
			} else {
				_, err = c.CreateOrder(t.Context(), kafudacontract.CreateOrderRequest{QuoteID: "quo_1", Reference: "order-1"})
			}
			if scenario.want != nil {
				if !errors.Is(err, scenario.want) {
					t.Fatalf("order rejection = %v, want %v", err, scenario.want)
				}
			} else {
				var apiErr *kafudacontract.APIError
				if !errors.As(err, &apiErr) || apiErr.Code != "upstream_error" || apiErr.HTTPStatus != scenario.status {
					t.Fatalf("ambiguous response classified as definitive: %v", err)
				}
			}
			if calls.Load() != 1 || strings.Contains(err.Error(), "private_fixture") || strings.Contains(err.Error(), "buyer@example.test") {
				t.Fatalf("unexpected retry or upstream details in error: %v, calls=%d", err, calls.Load())
			}
		})
	}
}

func TestRejectsRedirectsWithoutForwardingSession(t *testing.T) {
	forwarded := 0
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forwarded++ }))
	defer target.Close()
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	})
	_, err := c.Check(context.Background(), kafudacontract.CheckRequest{Session: "test-only-session"})
	if err == nil || forwarded != 0 {
		t.Fatalf("redirect followed: err=%v, forwarded=%d", err, forwarded)
	}
}

func TestRejectsMalformedOversizedOrMismatchedResponses(t *testing.T) {
	for _, body := range []string{
		`{"data":null}`, `{}`, `{"data":{"id":0,"status":"completed"}}`,
		`{"data":{"id":2,"status":"completed"}}`,
		`{"data":{"id":1,"status":"unexpected","account_email":"buyer@example.com"}}`,
		strings.Repeat(" ", maxResponseBytes+1),
	} {
		t.Run(fmt.Sprintf("body_size_%d", len(body)), func(t *testing.T) {
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) })
			if _, err := c.GetOrder(context.Background(), 1); err == nil {
				t.Fatal("invalid response accepted")
			}
		})
	}
}

func TestRejectsContradictorySuccessResponses(t *testing.T) {
	const supplierError = `"error":{"code":"quote_expired","message":"supplier-private-fixture"}`
	const order = `{"id":1,"reference":"order-1","product":"gpt","plan":"plus","country":"PH","status":"completed"}`
	for _, scenario := range []struct {
		name, data string
		request    func(context.Context, *Client) error
	}{
		{"catalog", `{"enabled":false,"plans":[],"auto_card_products":[]}`, func(ctx context.Context, client *Client) error {
			_, err := client.Catalog(ctx)
			return err
		}},
		{"create", order, func(ctx context.Context, client *Client) error {
			_, err := client.CreateOrder(ctx, kafudacontract.CreateOrderRequest{QuoteID: "quo_1", Reference: "order-1"})
			return err
		}},
		{"lookup", order, func(ctx context.Context, client *Client) error {
			_, err := client.GetOrder(ctx, 1)
			return err
		}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var calls atomic.Int32
			c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				fmt.Fprintf(w, `{"data":%s,%s}`, scenario.data, supplierError)
			})
			err := scenario.request(t.Context(), c)
			var apiErr *kafudacontract.APIError
			if !errors.As(err, &apiErr) || apiErr.Code != "invalid_response" || apiErr.HTTPStatus != http.StatusOK ||
				errors.Is(err, kafudacontract.ErrCreateQuoteExpired) || strings.Contains(err.Error(), "supplier-private-fixture") || calls.Load() != 1 {
				t.Fatalf("contradictory envelope was not rejected safely: %v, calls=%d", err, calls.Load())
			}
		})
	}
}

func TestCatalogAndNullableResultFields(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/gpt/catalog" {
			fmt.Fprint(w, `{"data":{"plans":[],"regions":[]},"error":null}`)
			return
		}
		fmt.Fprint(w, `{"data":{"id":1,"status":"completed","account_email":"buyer@example.com","charged_usd_cents":null,"renewal_cancelled":false,"completed_at":"2026-11-16T10:00:00Z"}}`)
	})
	if got, err := c.Catalog(context.Background()); err != nil || !json.Valid(got) {
		t.Fatalf("catalog = (%s, %v)", got, err)
	}
	got, err := c.GetOrder(context.Background(), 1)
	if err != nil || got.ChargedUSDCents != nil || got.RenewalCancelled == nil || *got.RenewalCancelled || got.CompletedAt == nil {
		t.Fatalf("nullable fields lost: %+v, %v", got, err)
	}
}

func TestRejectsUnsafeConfigurationAndReferenceBeforeSending(t *testing.T) {
	for _, origin := range []string{"http://kafuda.com", "https://user:pass@kafuda.com", "https://kafuda.com?token=secret", "https://kafuda.com/path", "https://kafuda.com#fragment"} {
		if _, err := New(origin, "test-only-token"); err == nil {
			t.Errorf("unsafe origin accepted: %s", origin)
		}
	}
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) { t.Error("invalid input sent to upstream") })
	for _, ref := range []string{"", strings.Repeat("a", 81), "order\r\nAuthorization: secret", "order?token=secret"} {
		if _, err := c.CreateOrder(context.Background(), kafudacontract.CreateOrderRequest{QuoteID: "quo_1", Reference: ref}); err == nil {
			t.Error("unsafe reference accepted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.GetOrder(ctx, 1)
	var apiErr *kafudacontract.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "transport_error" {
		t.Fatalf("context cancellation not sanitized: %v", err)
	}
}

func TestRateLimitDelayIsBounded(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-ratelimit-reset", fmt.Sprint(time.Now().Add(20*time.Second).Unix()))
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"code":"rate_limited"}}`)
	})
	_, err := c.Catalog(context.Background())
	var apiErr *kafudacontract.APIError
	if !errors.As(err, &apiErr) || apiErr.RetryAfter <= 0 || apiErr.RetryAfter > 20*time.Second {
		t.Fatalf("unexpected rate limit delay: %v", err)
	}
}

func TestOrderLookupSharesRateLimitAcrossDifferentOrders(t *testing.T) {
	var lookups atomic.Int32
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/gpt/catalog" {
			fmt.Fprint(w, `{"data":{"plans":[],"regions":[]}}`)
			return
		}
		if lookups.Add(1) == 1 {
			w.Header().Set("x-ratelimit-reset", fmt.Sprint(time.Now().Add(time.Minute).Unix()))
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		fmt.Fprint(w, `{"data":{"id":2,"status":"processing"}}`)
	})
	ctx := context.Background()
	for _, id := range []uint64{1, 2} {
		_, err := c.GetOrder(ctx, id)
		var limit *kafudacontract.APIError
		if !errors.As(err, &limit) || limit.HTTPStatus != http.StatusTooManyRequests || limit.RetryAfter <= 0 {
			t.Fatalf("lookup %d did not honor rate limit: %v", id, err)
		}
	}
	if calls := lookups.Load(); calls != 1 {
		t.Fatalf("second lookup sent an HTTP request during cooldown: %d requests", calls)
	}
	if _, err := c.Catalog(ctx); err != nil {
		t.Fatalf("numeric lookup limit blocked a catalog request: %v", err)
	}
	c.orderLookupMu.Lock()
	c.orderLookupResumeAt = time.Now().Add(-time.Second)
	c.orderLookupMu.Unlock()
	got, err := c.GetOrder(ctx, 2)
	if err != nil || got.ID != 2 || lookups.Load() != 2 {
		t.Fatalf("lookup did not resume: %+v, %v, %d requests", got, err, lookups.Load())
	}
}
