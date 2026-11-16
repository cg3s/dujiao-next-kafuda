package kafuda

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
)

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
		fmt.Fprint(w, `{"data":{"check_id":"chk_1","pay_mode":"auto_card","country":"PH","region_lock":null,"account":{"email":"buyer@example.com"},"plans":[{"plan":"plus","status":"available","kind":"new","quote_id":"quo_1","expires_at":"2026-11-16T10:00:00Z","auto_card":{"product_code":"P5378XB","open_fee_cents":50,"load_cents":1700,"total_cents":1780}}]}}`)
	})
	input := kafudacontract.CheckRequest{Country: "PH", PayMode: "auto_card", ProductCode: "P5378XB", Session: session}
	got, err := c.Check(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if got.MaskedEmail != "b***@***" || got.RegionLocked || len(got.Plans) != 1 || got.Plans[0].AutoCard.TotalCents != 1780 {
		t.Fatalf("unexpected normalized check result: %+v", got)
	}
	if strings.Contains(fmt.Sprintf("%+v %#v", input, input), "test-only-session") {
		t.Fatal("formatting a check request exposed Session")
	}
	if strings.Contains(fmt.Sprintf("%+v %#v", c, c), "test-only-token") {
		t.Fatal("formatting the client exposed its merchant token")
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

func TestOrderFailureIsNotRetriedAndDoesNotExposePII(t *testing.T) {
	calls := 0
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, `{"error":{"code":"service_unavailable","message":"buyer@example.com test-only-session","details":{"session":"test-only-session"}}}`)
	})
	_, err := c.CreateOrder(context.Background(), kafudacontract.CreateOrderRequest{QuoteID: "quo_1", Reference: "order-1"})
	var apiErr *kafudacontract.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "service_unavailable" || calls != 1 {
		t.Fatalf("unexpected error or retry: %v, calls=%d", err, calls)
	}
	if strings.Contains(err.Error(), "buyer") || strings.Contains(err.Error(), "session") {
		t.Fatal("upstream PII escaped through error")
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

func TestCatalogAndNullableResultFields(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/gpt/catalog" {
			fmt.Fprint(w, `{"data":{"plans":[],"regions":[]}}`)
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
