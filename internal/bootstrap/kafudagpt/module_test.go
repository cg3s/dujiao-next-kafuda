package kafudawiring

import (
	"context"
	"testing"
	"time"

	"github.com/dujiao-next/internal/config"
	"github.com/dujiao-next/internal/constants"
	fulfillmentdomain "github.com/dujiao-next/internal/modules/fulfillment/domain"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
	orderdomain "github.com/dujiao-next/internal/modules/order/domain"
	orderstore "github.com/dujiao-next/internal/modules/order/infrastructure/gormstore"
	"github.com/dujiao-next/internal/shared/jsonmap"
	"github.com/dujiao-next/internal/shared/money"
	"github.com/glebarez/sqlite"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

func TestNewBackfillsLegacyManualOrderStatuses(t *testing.T) {
	for _, scenario := range []struct {
		name, status, fulfillmentType string
		stage                         kafudadomain.Stage
		wantFulfilling                bool
	}{
		{"manual_review", constants.OrderStatusPaid, constants.FulfillmentTypeKafudaGPT, kafudadomain.StageManualReview, true},
		{"manual_delivery", constants.OrderStatusPaid, constants.FulfillmentTypeKafudaGPT, kafudadomain.StageManualDelivery, true},
		{"waiting", constants.OrderStatusPaid, constants.FulfillmentTypeKafudaGPT, kafudadomain.StageWaiting, false},
		{"refunded", constants.OrderStatusRefunded, constants.FulfillmentTypeKafudaGPT, kafudadomain.StageManualDelivery, false},
		{"other_fulfillment", constants.OrderStatusPaid, constants.FulfillmentTypeManual, kafudadomain.StageManualReview, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
			if err != nil {
				t.Fatal(err)
			}
			pool, err := db.DB()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = pool.Close() })
			pool.SetMaxOpenConns(1)
			if err := db.AutoMigrate(&orderdomain.Order{}, &orderdomain.OrderItem{}, &fulfillmentdomain.Fulfillment{}, &kafudadomain.Recharge{}); err != nil {
				t.Fatal(err)
			}
			paidAt := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
			price := money.FromDecimal(decimal.NewFromInt(100))
			parent := orderdomain.Order{OrderNo: "legacy-direct-parent", UserID: 1, Currency: "CNY", Status: scenario.status,
				PaidAt: &paidAt, TotalAmount: price, OnlinePaidAmount: price, UpdatedAt: paidAt}
			if scenario.status == constants.OrderStatusRefunded {
				parent.RefundedAmount = price
			}
			if err := db.Create(&parent).Error; err != nil {
				t.Fatal(err)
			}
			child := orderdomain.Order{OrderNo: "legacy-direct-child", ParentID: &parent.ID, UserID: parent.UserID,
				Currency: parent.Currency, Status: scenario.status, PaidAt: &paidAt, TotalAmount: price, UpdatedAt: paidAt}
			if err := db.Create(&child).Error; err != nil {
				t.Fatal(err)
			}
			item := orderdomain.OrderItem{OrderID: child.ID, ProductID: 1, SKUID: 2, Quantity: 1,
				FulfillmentType: scenario.fulfillmentType, TitleJSON: jsonmap.JSON{"en-US": "GPT"}}
			recharge := kafudadomain.Recharge{OrderID: child.ID, Stage: scenario.stage, Version: 4, UpdatedAt: paidAt,
				Snapshot: kafudadomain.Snapshot{ProductID: item.ProductID, SKUID: item.SKUID, Plan: "plus", SellPrice: price}}
			for _, row := range []any{&item, &recharge} {
				if err := db.Create(row).Error; err != nil {
					t.Fatal(err)
				}
			}
			store := orderstore.New(db, "legacy-direct-order-test-credential-secret")
			wantStatus := scenario.status
			if scenario.wantFulfilling {
				wantStatus = constants.OrderStatusFulfilling
			}
			var previousUpdates [2]time.Time
			for i := range 2 {
				module, err := New(db, config.KafudaGPTConfig{}, store)
				if err != nil {
					t.Fatalf("initialize module %d: %v", i+1, err)
				}
				stored, err := store.GetByID(parent.ID)
				if err != nil || stored == nil || len(stored.Children) != 1 || len(stored.Children[0].Items) != 1 {
					t.Fatalf("load parent and child: %+v, %v", stored, err)
				}
				for j, order := range []*orderdomain.Order{stored, &stored.Children[0]} {
					if order.Status != wantStatus || order.PaidAt == nil || !order.PaidAt.Equal(paidAt) || !order.TotalAmount.Decimal.Equal(price.Decimal) {
						t.Fatalf("backfill changed payment details or status for %s: %+v", order.OrderNo, order)
					}
					if !scenario.wantFulfilling && !order.UpdatedAt.Equal(paidAt) || i == 1 && !order.UpdatedAt.Equal(previousUpdates[j]) {
						t.Fatalf("backfill made an unnecessary write for %s", order.OrderNo)
					}
					previousUpdates[j] = order.UpdatedAt
				}
				if !stored.RefundedAmount.Decimal.Equal(parent.RefundedAmount.Decimal) ||
					!stored.OnlinePaidAmount.Decimal.Equal(parent.OnlinePaidAmount.Decimal) ||
					!stored.Children[0].RefundedAmount.IsZero() || stored.Children[0].Items[0].FulfillmentType != scenario.fulfillmentType {
					t.Fatalf("backfill changed payment amounts or fulfillment type: %+v", stored)
				}
				latest, err := module.Store.GetRecharge(context.Background(), child.ID)
				if err != nil || latest == nil || latest.Stage != recharge.Stage || latest.Version != recharge.Version ||
					!latest.UpdatedAt.Equal(paidAt) || latest.Snapshot.SKUID != item.SKUID || !latest.Snapshot.SellPrice.Decimal.Equal(price.Decimal) {
					t.Fatalf("backfill changed the recharge or snapshot: %+v, %v", latest, err)
				}
			}
		})
	}
}
