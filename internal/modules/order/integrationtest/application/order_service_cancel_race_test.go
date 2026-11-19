package application_test

import (
	"errors"
	"testing"
	"time"

	"github.com/dujiao-next/internal/constants"
	categorydomain "github.com/dujiao-next/internal/modules/catalog/category/domain"
	productdomain "github.com/dujiao-next/internal/modules/catalog/product/domain"
	productgormstore "github.com/dujiao-next/internal/modules/catalog/product/store/gormstore"
	orderapp "github.com/dujiao-next/internal/modules/order/application"
	orderdomain "github.com/dujiao-next/internal/modules/order/domain"
	ordergormstore "github.com/dujiao-next/internal/modules/order/infrastructure/gormstore"
	paymentapp "github.com/dujiao-next/internal/modules/payment/application"
	paymentgormstore "github.com/dujiao-next/internal/modules/payment/infrastructure/gormstore"
	"github.com/dujiao-next/internal/shared/jsonmap"

	"gorm.io/gorm"
)

func TestAdminSingleOrderCancelKeepsCommittedPayment(t *testing.T) {
	testAdminSingleOrderCancelKeepsCommittedPayment(t, func(t *testing.T) *gorm.DB {
		db := setupCancelPaymentTestDB(t, "admin_cancel_race")
		pool, err := db.DB()
		if err != nil {
			t.Fatal(err)
		}
		pool.SetMaxOpenConns(1)
		t.Cleanup(func() { _ = pool.Close() })
		return db
	})
}

func testAdminSingleOrderCancelKeepsCommittedPayment(t *testing.T, setup func(*testing.T) *gorm.DB) {
	t.Helper()
	for _, inventory := range []string{"sku", "legacy_product"} {
		t.Run(inventory, func(t *testing.T) {
			db := setup(t)
			now := time.Now().UTC()
			category := categorydomain.Category{Slug: "admin-cancel-race", NameJSON: jsonmap.JSON{"zh-CN": "测试分类"}}
			if err := db.Create(&category).Error; err != nil {
				t.Fatal(err)
			}
			product := productdomain.Product{
				CategoryID: category.ID, Slug: "admin-cancel-race", TitleJSON: jsonmap.JSON{"zh-CN": "人工商品"},
				FulfillmentType: constants.FulfillmentTypeManual, IsActive: true,
			}
			if inventory == "legacy_product" {
				product.ManualStockTotal, product.ManualStockLocked = 1, 2
			}
			if err := db.Create(&product).Error; err != nil {
				t.Fatal(err)
			}
			var stockModel any = &productdomain.Product{}
			stockID, skuID := product.ID, uint(0)
			if inventory == "sku" {
				sku := productdomain.ProductSKU{
					ProductID: product.ID, SKUCode: "manual", IsActive: true,
					ManualStockTotal: 1, ManualStockLocked: 2,
				}
				if err := db.Create(&sku).Error; err != nil {
					t.Fatal(err)
				}
				stockModel, stockID, skuID = &productdomain.ProductSKU{}, sku.ID, sku.ID
			}

			order := newPendingOrderForCancel("ADMIN-CANCEL-RACE-PAID", 0, nil, now)
			other := newPendingOrderForCancel("ADMIN-CANCEL-RACE-OTHER", 0, nil, now)
			for _, pending := range []*orderdomain.Order{order, other} {
				if err := db.Create(pending).Error; err != nil {
					t.Fatal(err)
				}
				item := orderdomain.OrderItem{
					OrderID: pending.ID, ProductID: product.ID, SKUID: skuID, TitleJSON: product.TitleJSON,
					Quantity: 1, UnitPrice: pending.TotalAmount, TotalPrice: pending.TotalAmount, FulfillmentType: constants.FulfillmentTypeManual,
				}
				if err := db.Create(&item).Error; err != nil {
					t.Fatal(err)
				}
			}
			payment := newPaymentForOrder(order.ID, constants.PaymentStatusPending, now)
			if err := db.Create(payment).Error; err != nil {
				t.Fatal(err)
			}

			const credentialSecret = "test-guest-credential-secret-with-32-bytes"
			orders := ordergormstore.New(db, credentialSecret)
			products := productgormstore.NewProductStore(db)
			skus := productgormstore.NewSKUStore(db)
			payments := paymentgormstore.New(db, credentialSecret)
			paymentService := paymentapp.NewPaymentService(paymentapp.PaymentServiceOptions{
				OrderStore: orders, ProductRepo: products, ProductSKURepo: skus, PaymentStore: payments,
			})
			orderService := orderapp.NewOrderService(orderapp.OrderServiceOptions{
				OrderStore: orderProductChangeStore{Store: orders, before: func() {
					paid, err := paymentService.HandleCallback(paymentapp.PaymentCallbackInput{
						PaymentID: payment.ID, OrderNo: order.OrderNo, ChannelID: payment.ChannelID,
						Status: constants.PaymentStatusSuccess, Currency: payment.Currency, Amount: payment.Amount,
					})
					if err != nil || paid == nil || paid.Status != constants.PaymentStatusSuccess || paid.PaidAt == nil {
						t.Fatalf("payment did not commit before cancellation: %+v, %v", paid, err)
					}
				}},
				ProductStore: products, ProductSKUStore: skus,
			})
			if canceled, err := orderService.UpdateOrderStatus(order.ID, constants.OrderStatusCanceled); canceled != nil || !errors.Is(err, orderapp.ErrOrderStatusInvalid) {
				t.Errorf("expected stale cancellation to be rejected: %+v, %v", canceled, err)
			}
			stored, err := orders.GetByID(order.ID)
			if err != nil || stored == nil || stored.Status != constants.OrderStatusPaid || stored.PaidAt == nil || stored.CanceledAt != nil {
				t.Errorf("cancellation changed the paid order: %+v, %v", stored, err)
			}
			storedPayment, err := payments.GetByID(payment.ID)
			if err != nil || storedPayment == nil || storedPayment.Status != constants.PaymentStatusSuccess || storedPayment.PaidAt == nil || storedPayment.ExpiredAt != nil {
				t.Errorf("cancellation changed the successful payment: %+v, %v", storedPayment, err)
			}
			var stock struct{ ManualStockTotal, ManualStockLocked, ManualStockSold int }
			if err := db.Model(stockModel).Where("id = ?", stockID).Select("manual_stock_total, manual_stock_locked, manual_stock_sold").Scan(&stock).Error; err != nil {
				t.Fatal(err)
			}
			if stock.ManualStockTotal != 1 || stock.ManualStockLocked != 1 || stock.ManualStockSold != 1 {
				t.Errorf("cancellation changed stock or released the other order's reservation: %+v", stock)
			}
		})
	}
}
