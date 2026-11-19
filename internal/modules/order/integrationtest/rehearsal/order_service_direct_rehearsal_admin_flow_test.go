package rehearsal_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dujiao-next/internal/constants"
	"github.com/dujiao-next/internal/i18n"
	fulfillmentdomain "github.com/dujiao-next/internal/modules/fulfillment/domain"
	admindomain "github.com/dujiao-next/internal/modules/identity/admin/domain"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
	ordercontract "github.com/dujiao-next/internal/modules/order/contract"
	orderdomain "github.com/dujiao-next/internal/modules/order/domain"
	orderhttp "github.com/dujiao-next/internal/modules/order/transport/http"
	settingsapp "github.com/dujiao-next/internal/modules/settings/application"
	settingsstore "github.com/dujiao-next/internal/modules/settings/infrastructure/gormstore"
	walletdomain "github.com/dujiao-next/internal/modules/wallet/domain"
	"github.com/dujiao-next/internal/platform/http/response"
	"github.com/dujiao-next/internal/shared/jsonmap"
)

func TestDirectBuyerAdminManualDeliveryRehearsal(t *testing.T) {
	storefrontDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(storefrontDir, "index.html"), []byte("<html>fixture storefront</html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	adminDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(adminDir, "index.html"), []byte(`<html><base href="__DJ_ADMIN_BASE__/"></html>`), 0o600); err != nil {
		t.Fatal(err)
	}
	f, router, buyer, _ := newDirectRehearsalRouter(t, storefrontDir, "", adminDir)
	support, operations := f.adminTokens["support"], f.adminTokens["operations"]
	if support == "" || operations == "" {
		t.Fatal("missing synthetic staff authorization")
	}
	var admin admindomain.Admin
	if err := f.db.Where("username = ?", "support").First(&admin).Error; err != nil || admin.ID == 0 || admin.IsSuper {
		t.Fatalf("support account fixture is invalid: %v", err)
	}
	readJSON := func(method, path, body, token string, wantCode int, data any) string {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(t.Context())
		req.Header.Set("Accept-Language", "en-US")
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res := httptest.NewRecorder()
		router.ServeHTTP(res, req)
		var reply struct {
			StatusCode int             `json:"status_code"`
			Message    string          `json:"msg"`
			Data       json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(res.Body.Bytes(), &reply); err != nil {
			t.Fatalf("%s %s returned HTTP %d: %v", method, path, res.Code, err)
		}
		if res.Code != http.StatusOK || reply.StatusCode != wantCode {
			t.Fatalf("%s %s returned HTTP %d, API status %d, want API status %d in HTTP 200 envelope", method, path, res.Code, reply.StatusCode, wantCode)
		}
		if data != nil {
			if err := json.Unmarshal(reply.Data, data); err != nil {
				t.Fatalf("decode %s %s data: %v", method, path, err)
			}
		}
		return reply.Message
	}
	var created struct {
		OrderNo          string `json:"order_no"`
		OrderPaid        bool   `json:"order_paid"`
		WalletPaidAmount string `json:"wallet_paid_amount"`
		OnlinePayAmount  string `json:"online_pay_amount"`
	}
	payload := fmt.Sprintf(`{"items":[{"product_id":%d,"sku_id":%d,"quantity":1}],"use_balance":true}`, f.product.ID, f.sku.ID)
	readJSON(http.MethodPost, "/api/v1/orders/create-and-pay", payload, buyer, 0, &created)
	if created.OrderNo == "" || !created.OrderPaid || created.WalletPaidAmount != "100.00" || created.OnlinePayAmount != "0.00" {
		t.Fatalf("wallet payment did not settle the rehearsal order: %+v", created)
	}
	loadOrder := func() *orderdomain.Order {
		t.Helper()
		order, err := f.options.OrderStore.GetByOrderNoAndUserScoped(created.OrderNo, f.users[0].ID, ordercontract.TenantScope{})
		if err != nil || order == nil || len(order.Children) != 1 {
			t.Fatalf("load the buyer checkout order: %v", err)
		}
		return order
	}
	paid := loadOrder()
	childID := paid.Children[0].ID
	if paid.Status != constants.OrderStatusPaid || paid.PaidAt == nil ||
		paid.Children[0].Status != constants.OrderStatusPaid || paid.Children[0].PaidAt == nil {
		t.Fatal("wallet payment did not mark both orders paid")
	}
	historicPaidAt := paid.PaidAt.AddDate(0, 0, -2)
	if err := f.db.Model(&orderdomain.Order{}).Where("id IN ?", []uint{paid.ID, childID}).UpdateColumns(map[string]any{
		"created_at": historicPaidAt.Add(-time.Minute),
		"paid_at":    historicPaidAt,
	}).Error; err != nil {
		t.Fatal(err)
	}
	paid = loadOrder()

	walletLedger := func() walletdomain.Transaction {
		t.Helper()
		var wallet struct {
			Balance string `json:"balance"`
		}
		readJSON(http.MethodGet, "/api/v1/wallet", "", buyer, 0, &wallet)
		if wallet.Balance != "100.00" {
			t.Fatalf("unexpected rehearsal wallet balance: %s", wallet.Balance)
		}
		var ledger []walletdomain.Transaction
		if err := f.db.Where("user_id = ?", f.users[0].ID).Find(&ledger).Error; err != nil || len(ledger) != 1 {
			t.Fatalf("wallet should have exactly one payment ledger row: count=%d, %v", len(ledger), err)
		}
		row := ledger[0]
		if row.OrderID == nil || *row.OrderID != paid.ID || row.Type != constants.WalletTxnTypeOrderPay ||
			row.Direction != constants.WalletTxnDirectionOut || row.Amount.String() != "100.00" || row.BalanceAfter.String() != "100.00" {
			t.Fatalf("unexpected wallet ledger facts: type=%s direction=%s amount=%s balance=%s", row.Type, row.Direction, row.Amount, row.BalanceAfter)
		}
		return row
	}
	initialLedger := walletLedger()
	buyerPath := "/api/v1/orders/" + url.PathEscape(created.OrderNo)
	assertBuyerStage := func(want kafudadomain.Stage) {
		t.Helper()
		var got struct {
			Stage string `json:"stage"`
		}
		readJSON(http.MethodGet, buyerPath+"/direct-status", "", buyer, 0, &got)
		if got.Stage != string(want) {
			t.Fatalf("buyer sees stage %q, want %q", got.Stage, want)
		}
	}
	assertBuyerStage(kafudadomain.StageWaiting)
	actionPath := "/api/v1/rehearsal/orders/" + url.PathEscape(created.OrderNo)
	readJSON(http.MethodPost, actionPath+"/authorize", `{"result":"manual_delivery"}`, buyer, 0, nil)
	assertBuyerStage(kafudadomain.StageProcessing)

	parentDetail := fmt.Sprintf("/api/v1/admin/orders/%d", paid.ID)
	loadAdminStatus := func(path string) *orderhttp.AdminDirectStatus {
		t.Helper()
		var detail struct {
			DirectRecharge *orderhttp.AdminDirectStatus `json:"direct_recharge"`
		}
		readJSON(http.MethodGet, path, "", support, 0, &detail)
		if detail.DirectRecharge == nil || detail.DirectRecharge.Attempt == nil ||
			detail.DirectRecharge.ChildID != childID || detail.DirectRecharge.Attempt.ID == 0 {
			t.Fatalf("admin order did not project the current direct attempt: %+v", detail.DirectRecharge)
		}
		return detail.DirectRecharge
	}
	inFlight := loadAdminStatus(parentDetail)
	if inFlight.Stage != string(kafudadomain.StageProcessing) || !inFlight.Attempt.Active {
		t.Fatalf("supplier request should be in progress: %+v", inFlight)
	}
	completePath := fmt.Sprintf("/api/v1/admin/orders/%d/direct-complete", childID)
	confirm := fmt.Sprintf(`{"attempt_id":%d,"confirmed":true,"admin_id":999}`, inFlight.Attempt.ID)
	readJSON(http.MethodPost, completePath, confirm, support, response.CodeBadRequest, nil)
	assertBuyerStage(kafudadomain.StageProcessing)

	readJSON(http.MethodPost, actionPath+"/advance", "", buyer, 0, nil)
	assertBuyerStage(kafudadomain.StageManualReview)
	adminStatus := loadAdminStatus(parentDetail)
	if adminStatus.Stage != string(kafudadomain.StageManualDelivery) || adminStatus.Reason != "upstream_failed" ||
		adminStatus.Attempt.Active || adminStatus.Attempt.ID != inFlight.Attempt.ID {
		t.Fatalf("failed supplier result did not enter controlled manual delivery: %+v", adminStatus)
	}

	settings := settingsstore.New(f.db)
	setRefundDays := func(days int) {
		t.Helper()
		if _, err := settings.Upsert(constants.SettingKeyOrderConfig, jsonmap.JSON{settingsapp.OrderConfigFieldMaxRefundDays: days}); err != nil {
			t.Fatal(err)
		}
	}
	assertWindowExpired := func(want bool) {
		t.Helper()
		var detail struct {
			Expired *bool `json:"refund_window_expired"`
		}
		readJSON(http.MethodGet, parentDetail, "", support, 0, &detail)
		if detail.Expired == nil || *detail.Expired != want {
			t.Fatalf("admin order details should include refund_window_expired=%t", want)
		}
	}
	readJSON(http.MethodGet, "/api/v1/admin/settings?key="+constants.SettingKeyOrderConfig, "", support, response.CodeForbidden, nil)
	setRefundDays(1)
	assertWindowExpired(true)
	refund := fmt.Sprintf(`{"attempt_id":%d,"confirmed":true,"amount":"100.00"}`, adminStatus.Attempt.ID)
	for _, endpoint := range []string{"direct-refund-to-wallet", "direct-manual-refund"} {
		path := fmt.Sprintf("/api/v1/admin/orders/%d/%s", paid.ID, endpoint)
		message := readJSON(http.MethodPost, path, refund, support, response.CodeBadRequest, nil)
		if message != i18n.T("en-US", "error.order_refund_expired") {
			t.Fatalf("%s did not reject the expired refund window: %q", endpoint, message)
		}
	}
	assertBuyerStage(kafudadomain.StageManualReview)
	walletLedger()
	setRefundDays(0)
	assertWindowExpired(false)

	const reviewList = "/api/v1/admin/orders?direct_review=true"
	var queue []struct {
		ID      uint   `json:"id"`
		OrderNo string `json:"order_no"`
		Status  string `json:"status"`
	}
	readJSON(http.MethodGet, reviewList, "", support, 0, &queue)
	if len(queue) != 1 || queue[0].ID != paid.ID || queue[0].OrderNo != created.OrderNo || queue[0].Status != constants.OrderStatusFulfilling {
		t.Fatalf("support queue did not include the pending manual order: %+v", queue)
	}

	readJSON(http.MethodPost, completePath, confirm, "", response.CodeUnauthorized, nil)
	readJSON(http.MethodPost, completePath, confirm, buyer, response.CodeUnauthorized, nil)
	readJSON(http.MethodPost, completePath, confirm, operations, response.CodeForbidden, nil)
	assertBuyerStage(kafudadomain.StageManualReview)
	var beforeDeliveries int64
	if err := f.db.Model(&fulfillmentdomain.Fulfillment{}).Where("order_id = ?", childID).Count(&beforeDeliveries).Error; err != nil || beforeDeliveries != 0 {
		t.Fatalf("rejected completion left delivery records: %d, %v", beforeDeliveries, err)
	}

	readJSON(http.MethodPost, completePath, confirm, support, 0, nil)
	readJSON(http.MethodPost, completePath, confirm, support, response.CodeBadRequest, nil)
	assertBuyerStage(kafudadomain.StageCompleted)
	readJSON(http.MethodGet, reviewList, "", support, 0, &queue)
	if len(queue) != 0 {
		t.Fatalf("completed order remained in the support queue: %+v", queue)
	}
	finished := loadOrder()
	if finished.Status != constants.OrderStatusCompleted || finished.Children[0].Status != constants.OrderStatusCompleted ||
		finished.PaidAt == nil || !finished.PaidAt.Equal(*paid.PaidAt) ||
		finished.Children[0].PaidAt == nil || !finished.Children[0].PaidAt.Equal(*paid.Children[0].PaidAt) ||
		!finished.TotalAmount.Equal(paid.TotalAmount.Decimal) || !finished.WalletPaidAmount.Equal(paid.WalletPaidAmount.Decimal) ||
		!finished.OnlinePaidAmount.IsZero() || !finished.RefundedAmount.IsZero() {
		t.Fatal("manual completion did not preserve and finish the paid order")
	}
	var deliveries []fulfillmentdomain.Fulfillment
	if err := f.db.Where("order_id = ?", childID).Find(&deliveries).Error; err != nil || len(deliveries) != 1 {
		t.Fatalf("expected exactly one manual fulfillment: count=%d, %v", len(deliveries), err)
	}
	delivery := deliveries[0]
	if delivery.Type != constants.FulfillmentTypeManual || delivery.Status != constants.FulfillmentStatusDelivered ||
		delivery.DeliveredBy == nil || *delivery.DeliveredBy != admin.ID || delivery.DeliveredAt == nil || delivery.Payload != "" {
		t.Fatalf("manual fulfillment lost the authenticated support actor: type=%s status=%s", delivery.Type, delivery.Status)
	}
	if finalLedger := walletLedger(); finalLedger.ID != initialLedger.ID || !finalLedger.CreatedAt.Equal(initialLedger.CreatedAt) {
		t.Fatal("manual completion changed the wallet payment ledger")
	}
}
