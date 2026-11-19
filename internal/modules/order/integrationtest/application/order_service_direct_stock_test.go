package application_test

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/dujiao-next/internal/constants"
	cardsecretapp "github.com/dujiao-next/internal/modules/cardsecret/application"
	cardsecretcontract "github.com/dujiao-next/internal/modules/cardsecret/contract"
	cardsecretdomain "github.com/dujiao-next/internal/modules/cardsecret/domain"
	cardsecretstore "github.com/dujiao-next/internal/modules/cardsecret/infrastructure/gormstore"
	productstore "github.com/dujiao-next/internal/modules/catalog/product/store/gormstore"
	fulfillmentapp "github.com/dujiao-next/internal/modules/fulfillment/application"
	fulfillmentstore "github.com/dujiao-next/internal/modules/fulfillment/infrastructure/gormstore"
	orderapp "github.com/dujiao-next/internal/modules/order/application"
	ordercontract "github.com/dujiao-next/internal/modules/order/contract"
	orderdomain "github.com/dujiao-next/internal/modules/order/domain"
	orderstore "github.com/dujiao-next/internal/modules/order/infrastructure/gormstore"
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

func TestAdminCardSecretMutationsPreserveCheckoutReservation(t *testing.T) {
	testAdminCardSecretMutationsPreserveCheckoutReservation(t, func(t *testing.T) couponLimitRaceFixture {
		return newCouponLimitRaceFixture(t, "card-admin-reservation", 0, 0)
	})
}

func testAdminCardSecretMutationsPreserveCheckoutReservation(t *testing.T, fixture func(*testing.T) couponLimitRaceFixture) {
	t.Helper()
	for _, operation := range []struct {
		name, status  string
		batch, remove bool
	}{
		{name: "single_text"},
		{name: "single_used", status: cardsecretdomain.StatusUsed},
		{name: "single_restore", status: cardsecretdomain.StatusAvailable},
		{name: "batch_used", status: cardsecretdomain.StatusUsed, batch: true},
		{name: "batch_restore", status: cardsecretdomain.StatusAvailable, batch: true},
		{name: "batch_delete", batch: true, remove: true},
	} {
		t.Run(operation.name, func(t *testing.T) {
			f := fixture(t)
			if err := f.db.AutoMigrate(&cardsecretdomain.Batch{}, &cardsecretdomain.Secret{}); err != nil {
				t.Fatal(err)
			}
			if err := f.db.Model(&f.product).Update("fulfillment_type", constants.FulfillmentTypeAuto).Error; err != nil {
				t.Fatal(err)
			}
			card := cardsecretdomain.Secret{ProductID: f.product.ID, SKUID: f.sku.ID, Secret: "for-checkout", Status: cardsecretdomain.StatusAvailable}
			other := cardsecretdomain.Secret{ProductID: f.product.ID, SKUID: f.sku.ID, Secret: "available-other", Status: cardsecretdomain.StatusAvailable}
			for _, row := range []*cardsecretdomain.Secret{&card, &other} {
				if err := f.db.Create(row).Error; err != nil {
					t.Fatal(err)
				}
			}
			secrets := cardsecretstore.New(f.db)
			products, skus := productstore.NewProductStore(f.db), productstore.NewSKUStore(f.db)
			orders := f.service(f.couponRepo, f.usageRepo)
			var reserved *cardsecretdomain.Secret
			var checkout *orderdomain.Order
			selection := &cardAdminSelectionHook{Repository: secrets, afterSelection: func(ids []uint) {
				want := []uint{card.ID}
				if operation.batch {
					want = append(want, other.ID)
				}
				if !reflect.DeepEqual(ids, want) {
					t.Fatalf("admin selection = %v, want %v", ids, want)
				}
				// Complete checkout after the administrator's read has returned the available card.
				var err error
				checkout, err = orders.CreateOrder(f.input(f.users[0].ID))
				if err != nil || checkout == nil || len(checkout.Children) != 1 {
					t.Fatalf("checkout did not reserve its stock: %v", err)
				}
				reserved, err = secrets.GetByID(card.ID)
				if err != nil || reserved == nil || reserved.OrderID == nil || *reserved.OrderID != checkout.Children[0].ID ||
					reserved.Status != cardsecretdomain.StatusReserved || reserved.ReservedAt == nil {
					t.Fatalf("card was not reserved by the checkout child: %v", err)
				}
			}}
			admin := cardsecretapp.NewService(cardsecretapp.ServiceOptions{
				Secrets: selection, Batches: cardsecretstore.NewBatch(f.db), Products: products, ProductSKUs: skus,
				Transactions: secrets.WithProducts(products, skus),
			})
			applyBatch := func(ids []uint, filter cardsecretapp.ListCardSecretInput, want int64) {
				t.Helper()
				var affected int64
				var err error
				if operation.remove {
					affected, err = admin.BatchDeleteCardSecrets(ids, 0, filter)
				} else {
					affected, err = admin.BatchUpdateCardSecretStatus(ids, 0, filter, operation.status)
				}
				if err != nil || affected != want {
					t.Fatalf("batch affected = (%d, %v), want (%d, nil)", affected, err, want)
				}
			}
			if operation.batch {
				applyBatch(nil, cardsecretapp.ListCardSecretInput{ProductID: f.product.ID, Status: cardsecretdomain.StatusAvailable}, 1)
				applyBatch([]uint{card.ID}, cardsecretapp.ListCardSecretInput{}, 0)
			} else if item, err := admin.UpdateCardSecret(card.ID, "replacement", operation.status); item != nil || !errors.Is(err, cardsecretapp.ErrOrderReservation) {
				t.Fatalf("edit unexpectedly changed the order-owned card: item=%t, error=%v", item != nil, err)
			}
			stored, err := secrets.GetByID(card.ID)
			if reserved == nil || err != nil || !reflect.DeepEqual(stored, reserved) {
				t.Fatalf("admin mutation changed the committed reservation: %v", err)
			}

			childID := checkout.Children[0].ID
			if err := f.db.Model(&orderdomain.Order{}).Where("id IN ?", []uint{checkout.ID, childID}).
				Updates(map[string]any{"status": constants.OrderStatusPaid, "paid_at": time.Now()}).Error; err != nil {
				t.Fatal(err)
			}
			fulfillments := fulfillmentapp.New(fulfillmentapp.Options{
				OrderStore: orderstore.New(f.db, "test-guest-credential-secret-with-32-bytes"), FulfillmentStore: fulfillmentstore.New(f.db),
			})
			delivery, err := fulfillments.CreateAuto(childID)
			if err != nil || delivery == nil || delivery.Payload != card.Secret {
				t.Fatalf("the checkout could not deliver its exact reserved card: %v", err)
			}
			manual := cardsecretdomain.Secret{ProductID: f.product.ID, SKUID: f.sku.ID, Secret: "manual-reservation", Status: cardsecretdomain.StatusReserved}
			if err := f.db.Create(&manual).Error; err != nil {
				t.Fatal(err)
			}
			eligible := []uint{card.ID, manual.ID}
			if operation.batch {
				applyBatch(eligible, cardsecretapp.ListCardSecretInput{}, 2)
				for _, id := range eligible {
					item, err := secrets.GetByID(id)
					if err != nil || (operation.remove && item != nil) || (!operation.remove && (item == nil || item.Status != operation.status)) {
						t.Fatalf("eligible card %d did not receive the batch change: %v", id, err)
					}
				}
			} else {
				for _, id := range eligible {
					item, err := admin.UpdateCardSecret(id, "replacement", operation.status)
					if err != nil || item == nil || item.Secret != "replacement" || (operation.status != "" && item.Status != operation.status) {
						t.Fatalf("eligible card %d did not receive the edit: %v", id, err)
					}
				}
			}
		})
	}
}

type cardAdminSelectionHook struct {
	cardsecretcontract.Repository
	afterSelection func([]uint)
}

func (hook *cardAdminSelectionHook) ListIDs(filter cardsecretcontract.ListFilter) ([]uint, error) {
	ids, err := hook.Repository.ListIDs(filter)
	if err == nil {
		hook.runAfterSelection(ids)
	}
	return ids, err
}

func (hook *cardAdminSelectionHook) GetByID(id uint) (*cardsecretdomain.Secret, error) {
	item, err := hook.Repository.GetByID(id)
	if err == nil && item != nil {
		hook.runAfterSelection([]uint{id})
	}
	return item, err
}

func (hook *cardAdminSelectionHook) runAfterSelection(ids []uint) {
	if hook.afterSelection != nil {
		selected := hook.afterSelection
		hook.afterSelection = nil
		selected(ids)
	}
}
