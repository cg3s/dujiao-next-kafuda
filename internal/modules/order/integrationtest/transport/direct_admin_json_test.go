package transport_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dujiao-next/internal/i18n"
	"github.com/dujiao-next/internal/logger"
	orderdomain "github.com/dujiao-next/internal/modules/order/domain"
	orderhttp "github.com/dujiao-next/internal/modules/order/transport/http"
	walletdomain "github.com/dujiao-next/internal/modules/wallet/domain"
	"github.com/dujiao-next/internal/platform/http/response"
	"github.com/dujiao-next/internal/shared/money"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

type directAdminJSONCall struct {
	action                      string
	orderID, attemptID, adminID uint
	upstreamID                  uint64
	confirmed                   bool
	amount, remark              string
}

type directAdminJSONRefundSpy struct {
	orderhttp.AdminRefundWriter
	record     func(directAdminJSONCall)
	parseCalls int
}

func (s *directAdminJSONRefundSpy) ParseRefundAmount(raw string) (money.Amount, error) {
	s.parseCalls++
	var amount money.Amount
	err := amount.UnmarshalJSON([]byte(raw))
	return amount, err
}

func (s *directAdminJSONRefundSpy) AdminRefundToWallet(input orderhttp.AdminRefundToWalletInput) (*orderdomain.Order, *walletdomain.Transaction, *orderdomain.OrderRefundRecord, error) {
	s.record(directAdminJSONCall{action: "refund-to-wallet", orderID: input.OrderID, attemptID: input.DirectAttemptID, amount: input.Amount.String(), remark: input.Remark})
	return nil, nil, nil, nil
}

func (s *directAdminJSONRefundSpy) AdminManualRefund(input orderhttp.AdminManualRefundInput) (*orderdomain.Order, *orderdomain.OrderRefundRecord, error) {
	s.record(directAdminJSONCall{action: "manual-refund", orderID: input.OrderID, attemptID: input.DirectAttemptID, amount: input.Amount.String(), remark: input.Remark})
	return nil, nil, nil
}

func TestDirectAdminActionJSONRoutes(t *testing.T) {
	oldMode := gin.Mode()
	gin.SetMode(gin.TestMode)
	t.Cleanup(func() { gin.SetMode(oldMode) })
	core, logs := observer.New(zap.DebugLevel)
	originalLogger := logger.L
	logger.L = zap.New(core)
	t.Cleanup(func() { logger.L = originalLogger })

	const adminID = uint(19)
	const orderID = uint(23)
	const maxJSONBytes = 16 << 10
	const valid = `{"attempt_id":7,"confirmed":true,"upstream_order_id":"9007199254740993","admin_id":999,"amount":"12.34","remark":"checked"}`
	var calls []directAdminJSONCall
	record := func(call directAdminJSONCall) { calls = append(calls, call) }
	manualAction := func(action string) func(uint, uint, uint, bool) error {
		return func(orderID, attemptID, operatorID uint, confirmed bool) error {
			record(directAdminJSONCall{action: action, orderID: orderID, attemptID: attemptID, adminID: operatorID, confirmed: confirmed})
			return nil
		}
	}
	admin := orderhttp.NewAdminHandler(
		struct{ orderhttp.OrderQuery }{},
		struct{ orderhttp.UserDirectory }{},
		struct{ orderhttp.CouponLookup }{},
		struct{ orderhttp.PromotionLookup }{},
		struct{ orderhttp.PaymentDirectory }{},
		struct {
			orderhttp.PaymentChannelDirectory
		}{},
		struct{ orderhttp.AccountingPolicy }{},
		nil,
		nil,
		manualAction("complete"),
		manualAction("release"),
		func(_ context.Context, orderID, attemptID, operatorID uint, upstreamID uint64) error {
			record(directAdminJSONCall{action: "reconcile", orderID: orderID, attemptID: attemptID, adminID: operatorID, upstreamID: upstreamID})
			return nil
		},
		manualAction("confirm-upstream"),
	)
	refunds := &directAdminJSONRefundSpy{record: record}
	router := gin.New()
	routes := router.Group("/admin", func(c *gin.Context) {
		c.Set("admin_id", adminID)
	})
	orderhttp.RegisterAdminRoutes(routes, admin)
	orderhttp.RegisterAdminRefundWriteRoutes(routes, orderhttp.NewAdminRefundHandler(
		struct{ orderhttp.AdminRefundReader }{}, refunds, refunds, nil, nil,
	))
	request := func(t *testing.T, action, body, locale string, streamed bool) response.Response {
		t.Helper()
		calls = nil
		refunds.parseCalls = 0
		logs.TakeAll()
		req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/admin/orders/%d/%s", orderID, action), strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Lang", locale)
		if streamed {
			req.ContentLength = -1
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		var envelope response.Response
		if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		return envelope
	}

	atLimit := valid + strings.Repeat(" ", maxJSONBytes-len(valid))
	invalidBodies := []struct {
		name, body, locale string
		streamed           bool
	}{
		{name: "syntax", body: `{"attempt_id":private-fixture}`, locale: i18n.LocaleZH},
		{name: "wrong_field_type", body: `{"attempt_id":"private-fixture"}`, locale: i18n.LocaleTW},
		{name: "missing_required", body: "{}", locale: i18n.LocaleEN},
		{name: "second_value", body: valid + " {}", locale: i18n.LocaleZH},
		{name: "oversized_valid_object", body: `{"ignored":"` + strings.Repeat("x", maxJSONBytes) + `",` + valid[1:], locale: i18n.LocaleTW},
		{name: "oversized_after_valid_object", body: atLimit + " ", locale: i18n.LocaleEN, streamed: true},
	}

	for _, action := range []struct {
		name string
		want directAdminJSONCall
	}{
		{name: "reconcile", want: directAdminJSONCall{action: "reconcile", orderID: orderID, attemptID: 7, adminID: adminID, upstreamID: 9007199254740993}},
		{name: "complete", want: directAdminJSONCall{action: "complete", orderID: orderID, attemptID: 7, adminID: adminID, confirmed: true}},
		{name: "release", want: directAdminJSONCall{action: "release", orderID: orderID, attemptID: 7, adminID: adminID, confirmed: true}},
		{name: "confirm-upstream", want: directAdminJSONCall{action: "confirm-upstream", orderID: orderID, attemptID: 7, adminID: adminID, confirmed: true}},
		{name: "refund-to-wallet", want: directAdminJSONCall{action: "refund-to-wallet", orderID: orderID, attemptID: 7, amount: "12.34", remark: "checked"}},
		{name: "manual-refund", want: directAdminJSONCall{action: "manual-refund", orderID: orderID, attemptID: 7, amount: "12.34", remark: "checked"}},
	} {
		t.Run(action.name, func(t *testing.T) {
			for _, bad := range invalidBodies {
				t.Run(bad.name, func(t *testing.T) {
					got := request(t, "direct-"+action.name, bad.body, bad.locale, bad.streamed)
					if got.StatusCode != response.CodeBadRequest || got.Msg != i18n.T(bad.locale, "error.bad_request") {
						t.Fatalf("expected generic localized bad request, got %+v", got)
					}
					if len(calls) != 0 || refunds.parseCalls != 0 {
						t.Fatalf("rejected body reached a business action: calls=%+v, amount parses=%d", calls, refunds.parseCalls)
					}
					for _, entry := range logs.All() {
						if _, ok := entry.ContextMap()["error"]; ok {
							t.Errorf("invalid JSON logged a raw error: %s", entry.Message)
						}
					}
				})
			}
			t.Run("valid_at_limit", func(t *testing.T) {
				got := request(t, "direct-"+action.name, atLimit, i18n.LocaleZH, true)
				if got.StatusCode != response.CodeOK || len(calls) != 1 || calls[0] != action.want {
					t.Fatalf("valid request failed delivery: response=%+v, calls=%+v", got, calls)
				}
				if action.want.amount != "" {
					entries := logs.FilterMessage("kafuda_direct_refund_recorded").All()
					if len(entries) != 1 || fmt.Sprint(entries[0].ContextMap()["admin_id"]) != fmt.Sprint(adminID) {
						t.Fatalf("refund audit used an unexpected operator: %+v", entries)
					}
				}
			})
		})
	}

	t.Run("ordinary_refunds_keep_existing_binding", func(t *testing.T) {
		for _, action := range []string{"refund-to-wallet", "manual-refund"} {
			got := request(t, action, atLimit+" ", i18n.LocaleZH, true)
			if got.StatusCode != response.CodeOK || len(calls) != 1 || calls[0].action != action || calls[0].attemptID != 0 {
				t.Fatalf("ordinary %s binding changed: response=%+v, calls=%+v", action, got, calls)
			}
		}
	})
}
