package application_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dujiao-next/internal/constants"
	fulfillmentdomain "github.com/dujiao-next/internal/modules/fulfillment/domain"
	kafudaapp "github.com/dujiao-next/internal/modules/kafudagpt/application"
	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
	orderapp "github.com/dujiao-next/internal/modules/order/application"
	orderrefund "github.com/dujiao-next/internal/modules/order/application/refund"
	ordercontract "github.com/dujiao-next/internal/modules/order/contract"
	orderdomain "github.com/dujiao-next/internal/modules/order/domain"
	walletapp "github.com/dujiao-next/internal/modules/wallet/application"
	walletdomain "github.com/dujiao-next/internal/modules/wallet/domain"
	walletstore "github.com/dujiao-next/internal/modules/wallet/infrastructure/gormstore"
	kafudaworkflow "github.com/dujiao-next/internal/workflows/kafudagpt/application"
)

func TestDirectAfterSalesCompetingResolutions(t *testing.T) {
	testDirectAfterSalesCompetingResolutions(t, newDirectCheckoutFixture)
}

func testDirectAfterSalesCompetingResolutions(t *testing.T, fixture func(*testing.T) directCheckoutFixture) {
	t.Helper()
	for _, alternative := range []string{"wallet_refund", "manual_delivery", "release_and_reserve", "late_supplier_success", "late_success_vs_manual_delivery"} {
		t.Run(alternative, func(t *testing.T) {
			f := fixture(t)
			if err := f.db.AutoMigrate(&kafudadomain.Attempt{}, &orderdomain.OrderRefundRecord{}, &walletdomain.Account{}, &walletdomain.Transaction{}); err != nil {
				t.Fatal(err)
			}
			order, err := orderapp.NewOrderService(f.options).CreateOrder(f.input)
			if err != nil {
				t.Fatal(err)
			}
			orders, ctx := f.options.OrderStore, context.Background()
			childID := order.Children[0].ID
			now := time.Now().UTC()
			if err := f.db.Model(&orderdomain.Order{}).Where("id IN ?", []uint{order.ID, childID}).
				Updates(map[string]any{"status": constants.OrderStatusPaid, "paid_at": now}).Error; err != nil {
				t.Fatal(err)
			}
			reserve := func(at time.Time) (*kafudaapp.Reservation, error) {
				return kafudaworkflow.ReserveBuyerAttempt(orders, ordercontract.TenantScope{}, order.OrderNo, order.UserID, at, at.Add(time.Minute))
			}
			reservation, err := reserve(now)
			if err != nil {
				t.Fatal(err)
			}
			records := kafudaworkflow.WithShopOrderTransitions(kafudaapp.New(f.store), orders)
			attemptID := reservation.Attempt.ID
			if err := records.MarkSubmitting(ctx, attemptID, "quote", now, now.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			if err := records.MarkUnknown(ctx, attemptID, now, now.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			assertDirectShopOrderStatus(t, orders, order, constants.OrderStatusFulfilling, now)
			result := kafudacontract.OrderResult{ID: 173, Reference: reservation.Attempt.Reference, Product: "gpt", Plan: "plus", Country: "PH", Status: "failed"}
			if alternative != "late_supplier_success" && alternative != "late_success_vs_manual_delivery" {
				if _, err := kafudaworkflow.ApplyResult(orders, childID, attemptID, result, now, now, 0); err != nil {
					t.Fatal(err)
				}
			}
			wallets := walletstore.New(f.db)
			refunds := orderrefund.New(orders, nil, nil, nil, walletapp.NewService(walletapp.Options{Repository: wallets, Transactions: wallets}), nil)
			refund := func() error {
				_, _, _, err := refunds.AdminRefundToWallet(orderrefund.AdminRefundToWalletInput{OrderID: order.ID, Amount: order.TotalAmount, DirectAttemptID: attemptID})
				return err
			}
			const adminID = 9
			completeManual := func() error {
				return kafudaworkflow.CompleteManual(orders, childID, attemptID, adminID, true, time.Now().UTC())
			}
			firstAction := refund
			if alternative == "late_success_vs_manual_delivery" {
				firstAction = completeManual
			}
			var competingAction func() error
			switch alternative {
			case "wallet_refund":
				competingAction = refund
			case "manual_delivery":
				competingAction = completeManual
			case "release_and_reserve":
				competingAction = func() error {
					if err := kafudaworkflow.ReleaseManual(orders, childID, attemptID, adminID, true, time.Now().UTC()); err != nil {
						return err
					}
					retry, err := reserve(time.Now().UTC())
					if err == nil && (!retry.Created || retry.Attempt.Sequence != 2 || retry.Attempt.Reference == reservation.Attempt.Reference) {
						t.Errorf("unexpected replacement attempt: %+v", retry)
					}
					return err
				}
			case "late_supplier_success", "late_success_vs_manual_delivery":
				competingAction = func() error {
					result.Status = "completed"
					receivedAt := time.Now().UTC()
					completed, err := kafudaworkflow.ApplyResult(orders, childID, attemptID, result, receivedAt, receivedAt, 0)
					if err == nil && !completed {
						t.Error("late success did not complete the order")
					}
					return err
				}
			}

			start := make(chan struct{})
			firstResult, competingResult := make(chan error, 1), make(chan error, 1)
			go func() {
				<-start
				firstResult <- firstAction()
			}()
			go func() {
				<-start
				competingResult <- competingAction()
			}()
			close(start)
			firstErr, competingErr := <-firstResult, <-competingResult
			for _, err := range []error{firstErr, competingErr} {
				if err != nil && !errors.Is(err, orderrefund.ErrOrderStatusInvalid) && !errors.Is(err, orderapp.ErrOrderStatusInvalid) {
					t.Fatalf("unexpected concurrent error: %v", err)
				}
			}
			if (firstErr == nil) == (competingErr == nil) {
				t.Fatalf("expected exactly one success, got first=%v competing=%v", firstErr, competingErr)
			}

			wantStatus, wantStage := constants.OrderStatusRefunded, kafudadomain.StageManualDelivery
			var wantRefunds, wantDeliveries int64 = 1, 0
			if firstErr != nil && alternative != "wallet_refund" {
				wantRefunds = 0
				switch alternative {
				case "manual_delivery":
					wantStatus, wantStage, wantDeliveries = constants.OrderStatusCompleted, kafudadomain.StageCompleted, 1
				case "release_and_reserve":
					wantStatus, wantStage = constants.OrderStatusFulfilling, kafudadomain.StageChecking
				case "late_supplier_success", "late_success_vs_manual_delivery":
					wantStatus, wantStage, wantDeliveries = constants.OrderStatusCompleted, kafudadomain.StageCompleted, 1
				}
			}
			for _, id := range []uint{order.ID, childID} {
				var stored orderdomain.Order
				if err := f.db.First(&stored, id).Error; err != nil || stored.Status != wantStatus {
					t.Fatalf("order %d state: %s, %v, want %s", id, stored.Status, err, wantStatus)
				}
				if id == order.ID && (wantRefunds == 1 && !stored.RefundedAmount.Decimal.Equal(order.TotalAmount.Decimal) ||
					wantRefunds == 0 && !stored.RefundedAmount.IsZero()) {
					t.Fatalf("parent refund amount: %s", stored.RefundedAmount.String())
				}
			}
			for _, check := range []struct {
				model any
				want  int64
			}{
				{&orderdomain.OrderRefundRecord{}, wantRefunds},
				{&walletdomain.Transaction{}, wantRefunds},
				{&fulfillmentdomain.Fulfillment{}, wantDeliveries},
			} {
				var count int64
				if err := f.db.Model(check.model).Count(&count).Error; err != nil || count != check.want {
					t.Fatalf("%T count: %d, %v, want %d", check.model, count, err, check.want)
				}
			}
			recharge, err := f.store.GetRecharge(ctx, childID)
			if err != nil || recharge == nil || recharge.Stage != wantStage {
				t.Fatalf("recharge state: %+v, %v, want %s", recharge, err, wantStage)
			}
		})
	}
}
