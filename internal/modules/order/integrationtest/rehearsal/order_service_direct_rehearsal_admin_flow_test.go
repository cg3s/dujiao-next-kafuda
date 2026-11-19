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
	"time"

	"github.com/dujiao-next/internal/constants"
	"github.com/dujiao-next/internal/i18n"
	productdomain "github.com/dujiao-next/internal/modules/catalog/product/domain"
	productpresenter "github.com/dujiao-next/internal/modules/catalog/product/transport/presenter"
	fulfillmentdomain "github.com/dujiao-next/internal/modules/fulfillment/domain"
	admindomain "github.com/dujiao-next/internal/modules/identity/admin/domain"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
	ordercontract "github.com/dujiao-next/internal/modules/order/contract"
	orderdomain "github.com/dujiao-next/internal/modules/order/domain"
	orderhttp "github.com/dujiao-next/internal/modules/order/transport/http"
	orderpresenter "github.com/dujiao-next/internal/modules/order/transport/presenter"
	paymentdomain "github.com/dujiao-next/internal/modules/payment/domain"
	settingsapp "github.com/dujiao-next/internal/modules/settings/application"
	settingsstore "github.com/dujiao-next/internal/modules/settings/infrastructure/gormstore"
	walletdomain "github.com/dujiao-next/internal/modules/wallet/domain"
	"github.com/dujiao-next/internal/platform/http/response"
	"github.com/dujiao-next/internal/shared/jsonmap"
)

func directRehearsalHTMLFixtures(t *testing.T) (string, string) {
	t.Helper()
	storefrontDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(storefrontDir, "index.html"), []byte("<html>fixture storefront</html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	adminDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(adminDir, "index.html"), []byte(`<html><base href="__DJ_ADMIN_BASE__/"></html>`), 0o600); err != nil {
		t.Fatal(err)
	}
	return storefrontDir, adminDir
}

func readDirectRehearsalJSON(t *testing.T, router http.Handler, method, path, body, token string, wantCode int, data any) string {
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

func TestDirectSalesStopPreservesPaidOrderAndSKURehearsal(t *testing.T) {
	forEachDirectRehearsalDatabase(t, testDirectSalesStopPreservesPaidOrderAndSKURehearsal)
}

func testDirectSalesStopPreservesPaidOrderAndSKURehearsal(t *testing.T, postgresDSN string) {
	storefrontDir, adminDir := directRehearsalHTMLFixtures(t)
	f, selling, buyer, _ := newConfiguredDirectRehearsalRouter(t, storefrontDir, "", "", directRehearsalOptions{postgresDSN: postgresDSN})
	productPath := "/api/v1/public/products/" + f.product.Slug
	assertPublicProduct := func(product productpresenter.Product, soldOut bool) {
		t.Helper()
		if product.ID != f.product.ID || product.FulfillmentType != constants.FulfillmentTypeKafudaGPT || product.IsSoldOut != soldOut ||
			len(product.SKUs) != 1 || product.SKUs[0].ID != f.sku.ID || product.SKUs[0].SKUCode != f.sku.SKUCode ||
			product.SKUs[0].IsSoldOut != soldOut || !product.SKUs[0].IsActive {
			t.Fatalf("expected the original product and SKU with sold_out=%t: %+v", soldOut, product)
		}
	}
	var product productpresenter.Product
	readDirectRehearsalJSON(t, selling, http.MethodGet, productPath, "", "", 0, &product)
	assertPublicProduct(product, false)

	payload := fmt.Sprintf(`{"items":[{"product_id":%d,"sku_id":%d,"quantity":1}],"use_balance":true}`, f.product.ID, f.sku.ID)
	var created struct {
		OrderNo   string `json:"order_no"`
		OrderPaid bool   `json:"order_paid"`
	}
	readDirectRehearsalJSON(t, selling, http.MethodPost, "/api/v1/orders/create-and-pay", payload, buyer, 0, &created)
	if created.OrderNo == "" || !created.OrderPaid {
		t.Fatalf("wallet payment did not settle the checkout: %+v", created)
	}
	paid, err := f.options.OrderStore.GetByOrderNoAndUserScoped(created.OrderNo, f.users[0].ID, ordercontract.TenantScope{})
	if err != nil || paid == nil || len(paid.Children) != 1 || paid.PaidAt == nil || paid.Children[0].PaidAt == nil ||
		paid.Status != constants.OrderStatusPaid || paid.Children[0].Status != constants.OrderStatusPaid ||
		!paid.WalletPaidAmount.Equal(f.sku.PriceAmount.Decimal) || !paid.OnlinePaidAmount.IsZero() ||
		len(paid.Children[0].Items) != 1 || paid.Children[0].Items[0].SKUID != f.sku.ID {
		t.Fatalf("load paid checkout: %+v, %v", paid, err)
	}
	childID := paid.Children[0].ID
	loadRecharge := func() *kafudadomain.Recharge {
		t.Helper()
		saved, err := f.store.GetRecharge(t.Context(), childID)
		if err != nil || saved == nil {
			t.Fatalf("load recharge for child %d: %v", childID, err)
		}
		return saved
	}
	initialRecharge := loadRecharge()
	if initialRecharge.Stage != kafudadomain.StageWaiting || initialRecharge.CurrentAttemptID != nil {
		t.Fatalf("payment did not leave a fresh checkout awaiting authorization: %+v", initialRecharge)
	}
	type paidRows struct {
		Account  walletdomain.Account
		Ledger   []walletdomain.Transaction
		Payments []paymentdomain.Payment
	}
	loadPaidRows := func() paidRows {
		t.Helper()
		var saved paidRows
		if err := f.db.Where("user_id = ?", paid.UserID).First(&saved.Account).Error; err != nil {
			t.Fatal(err)
		}
		if err := f.db.Where("user_id = ?", paid.UserID).Find(&saved.Ledger).Error; err != nil || len(saved.Ledger) != 1 ||
			saved.Ledger[0].OrderID == nil || *saved.Ledger[0].OrderID != paid.ID || saved.Ledger[0].Type != constants.WalletTxnTypeOrderPay {
			t.Fatalf("expected exactly one wallet payment ledger: %+v, %v", saved.Ledger, err)
		}
		if err := f.db.Find(&saved.Payments).Error; err != nil || len(saved.Payments) != 1 || saved.Payments[0].OrderID != paid.ID ||
			saved.Payments[0].Status != constants.PaymentStatusSuccess || saved.Payments[0].ProviderType != constants.PaymentProviderWallet {
			t.Fatalf("expected exactly one successful payment: %+v, %v", saved.Payments, err)
		}
		return saved
	}
	initialPayment := loadPaidRows()
	var originalSKU productdomain.ProductSKU
	if err := f.db.First(&originalSKU, f.sku.ID).Error; err != nil {
		t.Fatal(err)
	}
	// Rebuild the handlers and commerce policy around the same paid checkout and SKU.
	stopped, router, buyer, other := assembleDirectRehearsalRouter(t, f.withSalesEnabled(false), storefrontDir, "", adminDir, nil)
	if reason := stopped.commerce.SalesBlockReason(t.Context()); reason != "sales_disabled" {
		t.Fatalf("replacement server did not stop new sales: %q", reason)
	}
	readJSON := func(method, path, body, token string, wantCode int, data any) string {
		t.Helper()
		return readDirectRehearsalJSON(t, router, method, path, body, token, wantCode, data)
	}
	product = productpresenter.Product{}
	readJSON(http.MethodGet, productPath, "", "", 0, &product)
	assertPublicProduct(product, true)
	var listed []productpresenter.Product
	readJSON(http.MethodGet, "/api/v1/public/products", "", "", 0, &listed)
	if len(listed) != 1 {
		t.Fatalf("public list lost the stopped product: %+v", listed)
	}
	assertPublicProduct(listed[0], true)
	for _, path := range []string{"/api/v1/orders/preview", "/api/v1/orders", "/api/v1/orders/create-and-pay"} {
		msg := readJSON(http.MethodPost, path, payload, buyer, response.CodeBadRequest, nil)
		if msg != i18n.T("en-US", "error.product_not_available") {
			t.Fatalf("%s rejected the stopped purchase for an unexpected reason: %q", path, msg)
		}
	}
	support, operations := stopped.adminTokens["support"], stopped.adminTokens["operations"]
	var adminProduct productdomain.Product
	readJSON(http.MethodGet, fmt.Sprintf("/api/v1/admin/products/%d", f.product.ID), "", operations, 0, &adminProduct)
	if adminProduct.ID != f.product.ID || !adminProduct.IsActive || len(adminProduct.SKUs) != 1 || adminProduct.SKUs[0].ID != originalSKU.ID ||
		!adminProduct.SKUs[0].IsActive || !adminProduct.SKUs[0].DirectCostConfigured || adminProduct.SKUs[0].DirectCatalogRevision != originalSKU.DirectCatalogRevision ||
		!adminProduct.SKUs[0].CostPriceAmount.Equal(originalSKU.CostPriceAmount.Decimal) {
		t.Fatalf("admin lost the stored product configuration: %+v", adminProduct.SKUs)
	}
	orderPath := "/api/v1/orders/" + url.PathEscape(paid.OrderNo)
	assertPaidDetail := func(wantStatus string) {
		t.Helper()
		var detail orderpresenter.OrderDetail
		readJSON(http.MethodGet, orderPath, "", buyer, 0, &detail)
		if detail.OrderNo != paid.OrderNo || detail.Status != wantStatus || detail.PaidAt == nil || !detail.PaidAt.Equal(*paid.PaidAt) ||
			!detail.TotalAmount.Equal(paid.TotalAmount.Decimal) || !detail.WalletPaidAmount.Equal(paid.WalletPaidAmount.Decimal) ||
			!detail.OnlinePaidAmount.IsZero() || !detail.RefundedAmount.IsZero() || len(detail.Children) != 1 ||
			detail.Children[0].OrderNo != paid.Children[0].OrderNo || detail.Children[0].Status != wantStatus || detail.Children[0].PaidAt == nil ||
			!detail.Children[0].PaidAt.Equal(*paid.Children[0].PaidAt) || len(detail.Children[0].Items) != 1 ||
			detail.Children[0].Items[0].FulfillmentType != constants.FulfillmentTypeKafudaGPT ||
			!reflect.DeepEqual(detail.Children[0].Items[0].SKUSnapshot, paid.Children[0].Items[0].SKUSnapshotJSON) {
			t.Fatalf("buyer order detail changed the original purchase: %+v", detail)
		}
	}
	assertStage := func(want kafudadomain.Stage) {
		t.Helper()
		var status struct {
			Stage kafudadomain.Stage `json:"stage"`
		}
		readJSON(http.MethodGet, orderPath+"/direct-status", "", buyer, 0, &status)
		if status.Stage != want {
			t.Fatalf("buyer direct stage = %q, want %q", status.Stage, want)
		}
	}
	assertPaidDetail(constants.OrderStatusPaid)
	assertStage(kafudadomain.StageWaiting)
	readJSON(http.MethodGet, orderPath+"/direct-status", "", other, response.CodeNotFound, nil)
	currentOrder, err := f.options.OrderStore.GetByOrderNoAndUserScoped(paid.OrderNo, paid.UserID, ordercontract.TenantScope{})
	if err != nil || !reflect.DeepEqual(currentOrder, paid) {
		t.Fatalf("stop-sale or rejected checkout changed the existing order: %+v, %v", currentOrder, err)
	}
	if current := loadRecharge(); !reflect.DeepEqual(current, initialRecharge) {
		t.Fatalf("stop-sale or rejected checkout changed the old recharge: %+v", current)
	}
	for _, check := range []struct {
		model any
		want  int64
	}{{&orderdomain.Order{}, 2}, {&orderdomain.OrderItem{}, 1}, {&kafudadomain.Recharge{}, 1}, {&kafudadomain.Attempt{}, 0}} {
		var count int64
		if err := f.db.Model(check.model).Count(&count).Error; err != nil || count != check.want {
			t.Fatalf("stopped checkout left %d %T rows, want %d: %v", count, check.model, check.want, err)
		}
	}
	if current := loadPaidRows(); !reflect.DeepEqual(current, initialPayment) {
		t.Fatal("stop-sale or rejected checkout changed the settled payment or wallet ledger")
	}

	actionPath := "/api/v1/rehearsal/orders/" + url.PathEscape(paid.OrderNo)
	readJSON(http.MethodPost, actionPath+"/authorize", `{"result":"manual_delivery"}`, buyer, 0, nil)
	assertStage(kafudadomain.StageProcessing)
	readJSON(http.MethodPost, actionPath+"/advance", "", buyer, 0, nil)
	assertStage(kafudadomain.StageManualReview)
	var detail struct {
		DirectRecharge *orderhttp.AdminDirectStatus `json:"direct_recharge"`
	}
	readJSON(http.MethodGet, fmt.Sprintf("/api/v1/admin/orders/%d", paid.ID), "", support, 0, &detail)
	review := detail.DirectRecharge
	if review == nil || review.ChildID != childID || review.Stage != string(kafudadomain.StageManualDelivery) || review.Attempt == nil || review.Attempt.Active {
		t.Fatalf("support did not see the original attempt ready for manual delivery: %+v", review)
	}
	completePath := fmt.Sprintf("/api/v1/admin/orders/%d/direct-complete", childID)
	confirm := fmt.Sprintf(`{"attempt_id":%d,"confirmed":true}`, review.Attempt.ID)
	readJSON(http.MethodPost, completePath, confirm, operations, response.CodeForbidden, nil)
	readJSON(http.MethodPost, completePath, confirm, support, 0, nil)
	assertStage(kafudadomain.StageCompleted)
	assertPaidDetail(constants.OrderStatusCompleted)
	var supportAccount admindomain.Admin
	if err := f.db.Where("username = ?", "support").First(&supportAccount).Error; err != nil || supportAccount.IsSuper {
		t.Fatalf("support must be a scoped administrator: %v", err)
	}
	var deliveries []fulfillmentdomain.Fulfillment
	if err := f.db.Where("order_id = ?", childID).Find(&deliveries).Error; err != nil || len(deliveries) != 1 ||
		deliveries[0].Type != constants.FulfillmentTypeManual || deliveries[0].Status != constants.FulfillmentStatusDelivered ||
		deliveries[0].DeliveredBy == nil || *deliveries[0].DeliveredBy != supportAccount.ID {
		t.Fatalf("the paid order did not keep one authenticated manual delivery: %+v, %v", deliveries, err)
	}
	if current := loadRecharge(); !reflect.DeepEqual(current.Snapshot, initialRecharge.Snapshot) || current.CurrentAttemptID == nil ||
		*current.CurrentAttemptID != review.Attempt.ID || current.AttemptCount != 1 || current.CompletedAt == nil {
		t.Fatalf("manual completion lost the original snapshot or attempt: %+v", current)
	}
	var currentSKU productdomain.ProductSKU
	if err := f.db.First(&currentSKU, f.sku.ID).Error; err != nil || !reflect.DeepEqual(currentSKU, originalSKU) {
		t.Fatalf("sales stop or manual resolution changed the SKU: %+v, %v", currentSKU, err)
	}
	if current := loadPaidRows(); !reflect.DeepEqual(current, initialPayment) {
		t.Fatal("manual resolution changed the settled payment or wallet ledger")
	}
}

func TestDirectBuyerAdminManualDeliveryRehearsal(t *testing.T) {
	storefrontDir, adminDir := directRehearsalHTMLFixtures(t)
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
		return readDirectRehearsalJSON(t, router, method, path, body, token, wantCode, data)
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
