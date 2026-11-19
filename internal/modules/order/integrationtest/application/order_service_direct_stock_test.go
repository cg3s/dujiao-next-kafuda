package application_test

import (
	"testing"
	"time"

	"github.com/dujiao-next/internal/constants"
	cardsecretdomain "github.com/dujiao-next/internal/modules/cardsecret/domain"
	orderapp "github.com/dujiao-next/internal/modules/order/application"
	ordercontract "github.com/dujiao-next/internal/modules/order/contract"
	orderdomain "github.com/dujiao-next/internal/modules/order/domain"
	paymentdomain "github.com/dujiao-next/internal/modules/payment/domain"
)

func TestDirectCheckoutCancelAndReopenDoNotAccessCardSecrets(t *testing.T) {
	for _, path := range []string{"buyer", "admin", "expired"} {
		t.Run(path, func(t *testing.T) {
			f := newDirectCheckoutFixture(t)
			// The fixture intentionally has no card_secrets table. Any release
			// or re-reservation of card inventory must fail the transaction.
			if err := f.db.AutoMigrate(&paymentdomain.Payment{}); err != nil {
				t.Fatal(err)
			}
			if f.db.Migrator().HasTable(&cardsecretdomain.Secret{}) {
				t.Fatal("direct checkout fixture must not have a card_secrets table")
			}
			service := orderapp.NewOrderService(f.options)
			order, err := service.CreateOrder(f.input)
			if err != nil {
				t.Fatal(err)
			}
			childID := order.Children[0].ID
			var canceled *orderdomain.Order
			switch path {
			case "buyer":
				canceled, err = service.CancelOrder(order.ID, order.UserID)
			case "admin":
				canceled, err = service.UpdateOrderStatus(order.ID, constants.OrderStatusCanceled)
			case "expired":
				if err := f.db.Model(order).Update("expires_at", time.Now().Add(-time.Minute)).Error; err != nil {
					t.Fatal(err)
				}
				canceled, err = service.CancelExpiredOrder(order.ID)
			}
			if err != nil || canceled == nil || canceled.Status != constants.OrderStatusCanceled {
				t.Fatalf("cancel direct checkout: %+v, %v", canceled, err)
			}
			child, err := f.options.OrderStore.GetByID(childID)
			if err != nil || child == nil || child.Status != constants.OrderStatusCanceled {
				t.Fatalf("cancel direct child: %+v, %v", child, err)
			}
			if err := f.options.OrderStore.WithinTransaction(func(tx ordercontract.Transaction) error {
				locked, err := tx.Orders().GetByIDForUpdateWithChildren(order.ID)
				if err != nil {
					return err
				}
				return orderapp.ReopenCanceledOrderInTx(tx, locked, time.Now().UTC())
			}); err != nil {
				t.Fatalf("reopen after late payment: %v", err)
			}
			for _, id := range []uint{order.ID, childID} {
				stored, err := f.options.OrderStore.GetByID(id)
				if err != nil || stored == nil || stored.Status != constants.OrderStatusPendingPayment || stored.CanceledAt != nil {
					t.Fatalf("order %d was not reopened: %+v, %v", id, stored, err)
				}
			}
		})
	}
}

func TestAutoCheckoutCancelStillReleasesCardSecrets(t *testing.T) {
	f := newCouponLimitRaceFixture(t, "auto-stock-cancel", 0, 0)
	if err := f.db.AutoMigrate(&cardsecretdomain.Secret{}, &paymentdomain.Payment{}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Model(&f.product).Update("fulfillment_type", constants.FulfillmentTypeAuto).Error; err != nil {
		t.Fatal(err)
	}
	card := cardsecretdomain.Secret{
		ProductID: f.product.ID, SKUID: f.sku.ID, Secret: "test-card",
		Status: cardsecretdomain.StatusAvailable,
	}
	if err := f.db.Create(&card).Error; err != nil {
		t.Fatal(err)
	}
	service := f.service(f.couponRepo, f.usageRepo)
	order, err := service.CreateOrder(f.input(f.users[0].ID))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.First(&card, card.ID).Error; err != nil || card.Status != cardsecretdomain.StatusReserved {
		t.Fatalf("auto checkout did not reserve card: %s, %v", card.Status, err)
	}
	if _, err := service.CancelOrder(order.ID, order.UserID); err != nil {
		t.Fatal(err)
	}
	var released cardsecretdomain.Secret
	if err := f.db.First(&released, card.ID).Error; err != nil || released.Status != cardsecretdomain.StatusAvailable || released.OrderID != nil {
		t.Fatalf("auto checkout did not release card: %s, %v", released.Status, err)
	}
}
