package application_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/dujiao-next/internal/constants"
	cardsecretcontract "github.com/dujiao-next/internal/modules/cardsecret/contract"
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
	for _, alternative := range []string{"wallet_refund", "manual_delivery", "release_and_reserve", "late_supplier_success", "late_success_vs_manual_delivery", "stale_poll_vs_manual_delivery"} {
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
			switch alternative {
			case "late_success_vs_manual_delivery", "stale_poll_vs_manual_delivery":
				testDirectCompetingCompletions(t, f, order, reservation, records, result, now, alternative == "stale_poll_vs_manual_delivery")
				return
			case "late_supplier_success":
				// Keep the outcome unknown until the late supplier result arrives.
			default:
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
			case "late_supplier_success":
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
				case "late_supplier_success":
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

type directCompletionStore struct {
	ordercontract.Store
	t *testing.T
}

func (s directCompletionStore) WithinTransaction(run func(ordercontract.Transaction) error) error {
	return s.Store.WithinTransaction(func(tx ordercontract.Transaction) error {
		return run(directCompletionTransaction{Transaction: tx, t: s.t})
	})
}

type directCompletionTransaction struct {
	ordercontract.Transaction
	t *testing.T
}

func (tx directCompletionTransaction) CardSecrets() cardsecretcontract.Repository {
	tx.t.Error("direct completion must not access card secret inventory")
	return tx.Transaction.CardSecrets()
}

func testDirectCompetingCompletions(t *testing.T, f directCheckoutFixture, order *orderdomain.Order, reservation *kafudaapp.Reservation,
	records *kafudaapp.Service, failed kafudacontract.OrderResult, paidAt time.Time, supplierFailed bool,
) {
	t.Helper()
	ctx := t.Context()
	orders := directCompletionStore{Store: f.options.OrderStore, t: t}
	childID, attemptID := order.Children[0].ID, reservation.Attempt.ID
	const operatorID = 9
	queue := &overCapCompletionQueue{store: orders, parentID: order.ID}
	complete := failed
	complete.Status = string(kafudadomain.AttemptCompleted)
	var poll *kafudaapp.KnownOrderPoll
	startedAt, receivedAt := paidAt, paidAt.Add(time.Second)
	manualAt := receivedAt
	if supplierFailed {
		processing := failed
		processing.Status = string(kafudadomain.AttemptProcessing)
		if completed, err := kafudaworkflow.ApplyResult(orders, childID, attemptID, processing, paidAt, paidAt, time.Second); completed || err != nil {
			t.Fatalf("record supplier processing: completed=%t, err=%v", completed, err)
		}
		pollResult := func(result kafudacontract.OrderResult, at time.Time) *kafudaapp.KnownOrderPoll {
			gateway := directCreateGateway{get: func(_ context.Context, id uint64) (*kafudacontract.OrderResult, error) {
				if id != result.ID {
					t.Errorf("poll requested upstream order %d, want %d", id, result.ID)
				}
				return &result, nil
			}}
			lookup, err := records.PollKnownOrder(ctx, ctx, gateway, attemptID, at, time.Second, kafudaapp.DefaultPollReviewAfter)
			if err != nil || lookup == nil {
				t.Fatalf("capture leased supplier response %s: %+v, %v", result.Status, lookup, err)
			}
			return lookup
		}
		startedAt = paidAt.Add(2 * time.Second)
		receivedAt = startedAt.Add(time.Second)
		poll = pollResult(complete, startedAt)
		// A later worker sees a terminal failure after the first lease expires,
		// while the older worker still has a captured success waiting to apply.
		failedAt := poll.Lease.Until.Add(time.Second)
		failure := pollResult(failed, failedAt)
		if completed, err := kafudaworkflow.ApplyPolledResult(orders, childID, attemptID, *failure, failedAt, failedAt, time.Second, kafudaapp.DefaultPollReviewAfter); completed || err != nil {
			t.Fatalf("record terminal supplier failure: completed=%t, err=%v", completed, err)
		}
		manualAt = failedAt.Add(time.Second)
	}

	manual := func() error {
		return kafudaworkflow.CompleteManualAndNotify(ctx, orders, f.store, queue, nil, nil, childID, attemptID, operatorID, true, manualAt)
	}
	supplier := func() (bool, error) {
		var completed bool
		var err error
		if poll == nil {
			completed, err = kafudaworkflow.ApplyResult(orders, childID, attemptID, complete, startedAt, receivedAt, time.Second)
		} else {
			completed, err = kafudaworkflow.ApplyPolledResult(orders, childID, attemptID, *poll, startedAt, receivedAt, time.Second, kafudaapp.DefaultPollReviewAfter)
		}
		if completed && err == nil {
			kafudaworkflow.EnqueueCompletionEmail(ctx, orders, f.store, queue, nil, nil, childID)
		}
		return completed, err
	}
	start := make(chan struct{})
	manualResult := make(chan error, 1)
	go func() {
		<-start
		manualResult <- manual()
	}()
	close(start)
	completed, supplierErr := supplier()
	manualErr := <-manualResult
	wantType, wantAttemptStatus := constants.FulfillmentTypeKafudaGPT, kafudadomain.AttemptCompleted
	var wantOperator *uint
	if supplierFailed {
		if completed || !errors.Is(supplierErr, kafudadomain.ErrInvalidTransition) || manualErr != nil {
			t.Fatalf("manual delivery should win after terminal failure: completed=%t, supplier=%v, manual=%v", completed, supplierErr, manualErr)
		}
		wantType, wantAttemptStatus = constants.FulfillmentTypeManual, kafudadomain.AttemptFailed
		operator := uint(operatorID)
		wantOperator = &operator
	} else if !completed || supplierErr != nil || !errors.Is(manualErr, orderapp.ErrOrderStatusInvalid) {
		t.Fatalf("late supplier should win while the result is unknown: completed=%t, supplier=%v, manual=%v", completed, supplierErr, manualErr)
	}

	loadCompletion := func() (fulfillmentdomain.Fulfillment, *kafudadomain.Recharge, *kafudadomain.Attempt) {
		t.Helper()
		var deliveries []fulfillmentdomain.Fulfillment
		if err := f.db.Find(&deliveries).Error; err != nil || len(deliveries) != 1 || deliveries[0].OrderID != childID {
			t.Fatalf("expected one child delivery: got %d, %v", len(deliveries), err)
		}
		record, err := f.store.GetRecharge(ctx, childID)
		if err != nil || record == nil {
			t.Fatalf("load completed recharge: %+v, %v", record, err)
		}
		attempt, err := f.store.GetAttempt(ctx, attemptID)
		if err != nil || attempt == nil {
			t.Fatalf("load original attempt: %+v, %v", attempt, err)
		}
		return deliveries[0], record, attempt
	}
	delivery, record, attempt := loadCompletion()
	if delivery.Type != wantType || delivery.Status != constants.FulfillmentStatusDelivered ||
		!reflect.DeepEqual(delivery.DeliveredBy, wantOperator) || delivery.DeliveredAt == nil {
		t.Fatalf("delivery did not preserve its source and operator: %+v", delivery)
	}
	if record.Stage != kafudadomain.StageCompleted || record.CompletedAt == nil || !record.CompletedAt.Equal(*delivery.DeliveredAt) ||
		record.CurrentAttemptID == nil || *record.CurrentAttemptID != attemptID || record.AttemptCount != 1 ||
		!reflect.DeepEqual(record.Snapshot, reservation.Recharge.Snapshot) {
		t.Fatalf("completion lost its original recharge: %+v", record)
	}
	if attempt.Status != wantAttemptStatus || attempt.ActiveOrderID != nil || attempt.NextPollAt != nil || attempt.KnownOrderPollLeaseUntil != nil ||
		attempt.Reference != failed.Reference || attempt.UpstreamOrderID == nil || *attempt.UpstreamOrderID != failed.ID || attempt.QuoteID != "quote" {
		t.Fatalf("completion changed the original attempt outcome or identity: %+v", attempt)
	}
	if queue.calls.Load() != 1 || record.CompletionEmailPending || record.CompletionEmailRetryAt != nil {
		t.Fatalf("expected one acknowledged completion email: calls=%d pending=%t retry=%v", queue.calls.Load(), record.CompletionEmailPending, record.CompletionEmailRetryAt)
	}
	assertDirectShopOrderStatus(t, orders, order, constants.OrderStatusCompleted, paidAt)
	loadShopOrder := func() *orderdomain.Order {
		t.Helper()
		stored, err := orders.GetByID(order.ID)
		if err != nil || stored == nil {
			t.Fatalf("load completed storefront order: %v", err)
		}
		return stored
	}
	shopOrder := loadShopOrder()

	if err := manual(); !errors.Is(err, orderapp.ErrOrderStatusInvalid) {
		t.Fatalf("replayed manual completion: %v", err)
	}
	wantReplayErr := error(nil)
	if supplierFailed {
		wantReplayErr = kafudadomain.ErrInvalidTransition
	}
	if completed, err := supplier(); completed || !errors.Is(err, wantReplayErr) {
		t.Fatalf("replayed supplier result: completed=%t, err=%v", completed, err)
	}
	if !supplierFailed {
		for _, status := range []kafudadomain.AttemptStatus{kafudadomain.AttemptProcessing, kafudadomain.AttemptFailed} {
			late := failed
			late.Status = string(status)
			if completed, err := kafudaworkflow.ApplyResult(orders, childID, attemptID, late, startedAt, receivedAt.Add(time.Second), time.Second); completed || !errors.Is(err, kafudadomain.ErrInvalidTransition) {
				t.Fatalf("late supplier %s result after completion: completed=%t, err=%v", status, completed, err)
			}
		}
	}
	kafudaworkflow.EnqueueCompletionEmail(ctx, orders, f.store, queue, nil, nil, childID)
	afterDelivery, afterRecord, afterAttempt := loadCompletion()
	if queue.calls.Load() != 1 || !reflect.DeepEqual(afterDelivery, delivery) || !reflect.DeepEqual(afterRecord, record) ||
		!reflect.DeepEqual(afterAttempt, attempt) || !reflect.DeepEqual(loadShopOrder(), shopOrder) {
		t.Fatal("replayed or out-of-order results changed the completed order, delivery, recharge, attempt or notification")
	}
	assertDirectShopOrderStatus(t, orders, order, constants.OrderStatusCompleted, paidAt)
}
