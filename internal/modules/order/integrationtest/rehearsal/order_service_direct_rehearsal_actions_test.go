package rehearsal_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dujiao-next/internal/constants"
	"github.com/dujiao-next/internal/i18n"
	kafudaapp "github.com/dujiao-next/internal/modules/kafudagpt/application"
	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
	orderapp "github.com/dujiao-next/internal/modules/order/application"
	ordercontract "github.com/dujiao-next/internal/modules/order/contract"
	orderdomain "github.com/dujiao-next/internal/modules/order/domain"
	resellercontract "github.com/dujiao-next/internal/modules/reseller/contract"
	"github.com/dujiao-next/internal/platform/http/ginutil"
	"github.com/dujiao-next/internal/platform/http/response"
	kafudaworkflow "github.com/dujiao-next/internal/workflows/kafudagpt/application"

	"github.com/gin-gonic/gin"
)

const directRehearsalPollInterval = 10 * time.Millisecond

type directRehearsalActions struct {
	fixture             directCheckoutFixture
	records             *kafudaapp.Service
	allowManualDelivery bool
	renewalCancelled    *bool
	mu                  sync.Mutex // Serializes mock supplier advances for this fixture.
	results             map[uint]string
}

type directRehearsalGateway struct {
	kafudacontract.Gateway
	check  func(context.Context, kafudacontract.CheckRequest) (*kafudacontract.CheckResult, error)
	create func(context.Context, kafudacontract.CreateOrderRequest) (*kafudacontract.OrderResult, error)
	get    func(context.Context, uint64) (*kafudacontract.OrderResult, error)
}

func (g directRehearsalGateway) Check(ctx context.Context, req kafudacontract.CheckRequest) (*kafudacontract.CheckResult, error) {
	return g.check(ctx, req)
}

const directRehearsalSession = "local-rehearsal-session"

func newDirectRehearsalBuyerGateway(f directCheckoutFixture) kafudacontract.Gateway {
	plan := f.sku.SpecValuesJSON["plan"].(string)
	country := f.sku.SpecValuesJSON["country"].(string)
	product := "gpt"
	if plan == "credit250" {
		product = "gpt_credits"
	}
	const quoteID = "local-buyer-quote"
	var orderSequence atomic.Uint64
	return directRehearsalGateway{
		check: func(_ context.Context, req kafudacontract.CheckRequest) (*kafudacontract.CheckResult, error) {
			if req.Session != directRehearsalSession {
				return nil, kafudacontract.ErrCheckSessionInvalid
			}
			if req.Country != country || req.PayMode != "auto_card" || req.ProductCode != directRehearsalCard {
				return nil, kafudadomain.ErrInvalidTransition
			}
			return &kafudacontract.CheckResult{Country: req.Country, PayMode: req.PayMode, Plans: []kafudacontract.Quote{{
				Plan: plan, Status: "available", Kind: "new", QuoteID: quoteID, PriceCents: 900,
				ExpiresAt: time.Now().Add(time.Minute),
				AutoCard:  &kafudacontract.AutoCard{ProductCode: directRehearsalCard, LoadCents: 100, TotalCents: 1000},
			}}}, nil
		},
		create: func(_ context.Context, req kafudacontract.CreateOrderRequest) (*kafudacontract.OrderResult, error) {
			if req.QuoteID != quoteID || req.Reference == "" {
				return nil, kafudadomain.ErrInvalidTransition
			}
			return &kafudacontract.OrderResult{ID: 1000 + orderSequence.Add(1), Reference: req.Reference, Product: product, Plan: plan, Country: country, Status: "completed"}, nil
		},
	}
}

func (g directRehearsalGateway) CreateOrder(ctx context.Context, req kafudacontract.CreateOrderRequest) (*kafudacontract.OrderResult, error) {
	return g.create(ctx, req)
}

func (g directRehearsalGateway) GetOrder(ctx context.Context, id uint64) (*kafudacontract.OrderResult, error) {
	return g.get(ctx, id)
}

func newDirectRehearsalActions(f directCheckoutFixture, allowManualDelivery bool, renewalCancelled *bool) *directRehearsalActions {
	return &directRehearsalActions{
		fixture:             f,
		records:             kafudaapp.New(f.store),
		allowManualDelivery: allowManualDelivery,
		renewalCancelled:    renewalCancelled,
		results:             make(map[uint]string),
	}
}

func (a *directRehearsalActions) register(member *gin.RouterGroup) {
	member.POST("/rehearsal/orders/:order_no/authorize", a.authorize)
	member.POST("/rehearsal/orders/:order_no/advance", a.advance)
}

func directRehearsalError(c *gin.Context, status int, key string) {
	response.ErrorWithHTTPStatus(c, status, status, i18n.T(i18n.ResolveLocale(c), key))
}

func readDirectRehearsalOutcome(c *gin.Context, allowManualDelivery bool) (string, bool) {
	if c.Request.Body == nil {
		return "", false
	}
	decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 128))
	// Read the exact shape so extra fields, duplicate keys and trailing input
	// cannot become part of the rehearsal request.
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return "", false
	}
	key, err := decoder.Token()
	if err != nil || key != "result" {
		return "", false
	}
	var result string
	if err := decoder.Decode(&result); err != nil {
		return "", false
	}
	if result != "success" && result != "manual_review" && !(allowManualDelivery && result == "manual_delivery") {
		return "", false
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') {
		return "", false
	}
	_, err = decoder.Token()
	return result, errors.Is(err, io.EOF)
}

func (a *directRehearsalActions) paidOrder(c *gin.Context) (*orderdomain.Order, *kafudadomain.Recharge) {
	userID, ok := ginutil.GetUserID(c)
	if !ok {
		return nil, nil
	}
	ctx := c.Request.Context()
	tenant := resellercontract.TenantOrMain(ctx)
	if !tenant.IsMain || tenant.Unavailable || tenant.ResellerID != nil {
		directRehearsalError(c, http.StatusNotFound, "error.order_not_found")
		return nil, nil
	}
	order, err := a.fixture.options.OrderStore.GetByOrderNoAndUserScoped(c.Param("order_no"), userID, ordercontract.TenantScope{})
	if err != nil {
		directRehearsalError(c, http.StatusInternalServerError, "error.order_fetch_failed")
		return nil, nil
	}
	if order == nil || order.ParentID != nil || len(order.Children) != 1 {
		directRehearsalError(c, http.StatusNotFound, "error.order_not_found")
		return nil, nil
	}
	child := order.Children[0]
	if order.PaidAt == nil || child.PaidAt == nil || order.RefundedAmount.IsPositive() || child.RefundedAmount.IsPositive() ||
		(order.Status != constants.OrderStatusPaid && order.Status != constants.OrderStatusFulfilling && order.Status != constants.OrderStatusCompleted) {
		directRehearsalError(c, http.StatusConflict, "error.order_status_invalid")
		return nil, nil
	}
	record, err := a.fixture.store.GetRecharge(ctx, child.ID)
	if err != nil {
		directRehearsalError(c, http.StatusInternalServerError, "error.order_fetch_failed")
		return nil, nil
	}
	if record == nil {
		directRehearsalError(c, http.StatusNotFound, "error.order_not_found")
		return nil, nil
	}
	return order, record
}

func (a *directRehearsalActions) authorize(c *gin.Context) {
	outcome, ok := readDirectRehearsalOutcome(c, a.allowManualDelivery)
	if !ok {
		directRehearsalError(c, http.StatusBadRequest, "error.bad_request")
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	order, record := a.paidOrder(c)
	if order == nil {
		return
	}
	if record.CurrentAttemptID != nil {
		if a.results[*record.CurrentAttemptID] != outcome {
			directRehearsalError(c, http.StatusConflict, "error.order_status_invalid")
			return
		}
		response.Success(c, gin.H{"stage": record.Stage})
		return
	}
	ctx := c.Request.Context()
	now := time.Now().UTC()
	orders := a.fixture.options.OrderStore
	reserved, err := kafudaworkflow.ReserveBuyerAttempt(orders, ordercontract.TenantScope{}, order.OrderNo, order.UserID, now, now.Add(time.Minute))
	if errors.Is(err, orderapp.ErrOrderStatusInvalid) || errors.Is(err, kafudadomain.ErrInvalidTransition) {
		directRehearsalError(c, http.StatusConflict, "error.order_status_invalid")
		return
	}
	if err != nil || reserved == nil || !reserved.Created {
		directRehearsalError(c, http.StatusInternalServerError, "error.order_fetch_failed")
		return
	}
	const quoteID = "local-rehearsal-quote"
	snapshot := reserved.Recharge.Snapshot
	check := kafudacontract.CheckResult{Country: snapshot.Country, PayMode: snapshot.PayMode,
		Plans: []kafudacontract.Quote{{
			Plan: snapshot.Plan, Status: "available", Kind: "new", QuoteID: quoteID,
			PriceCents: 900, ExpiresAt: now.Add(time.Minute),
			AutoCard: &kafudacontract.AutoCard{ProductCode: snapshot.ProductCode, LoadCents: 100, TotalCents: 1000},
		}},
	}
	gateway := directRehearsalGateway{create: func(_ context.Context, req kafudacontract.CreateOrderRequest) (*kafudacontract.OrderResult, error) {
		if req.QuoteID != quoteID || req.Reference != reserved.Attempt.Reference {
			return nil, kafudadomain.ErrInvalidTransition
		}
		return directRehearsalSupplierOrder(reserved.Recharge, reserved.Attempt, "processing"), nil
	}}
	completed, err := kafudaworkflow.SubmitCheckedQuote(ctx, orders, a.records, gateway, reserved, check, directRehearsalPollInterval)
	if err != nil || completed {
		directRehearsalError(c, http.StatusInternalServerError, "error.order_fetch_failed")
		return
	}
	a.results[reserved.Attempt.ID] = outcome
	a.respondStage(c, record.OrderID)
}

func (a *directRehearsalActions) advance(c *gin.Context) {
	if c.Request.Body != nil {
		var first [1]byte
		if _, err := io.ReadFull(c.Request.Body, first[:]); !errors.Is(err, io.EOF) {
			directRehearsalError(c, http.StatusBadRequest, "error.bad_request")
			return
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	order, record := a.paidOrder(c)
	if order == nil {
		return
	}
	if record.CurrentAttemptID == nil || a.results[*record.CurrentAttemptID] == "" {
		directRehearsalError(c, http.StatusConflict, "error.order_status_invalid")
		return
	}
	if record.Stage == kafudadomain.StageCompleted || record.Stage == kafudadomain.StageManualReview ||
		record.Stage == kafudadomain.StageManualDelivery {
		response.Success(c, gin.H{"stage": record.Stage})
		return
	}
	ctx := c.Request.Context()
	attempt, err := a.fixture.store.GetAttempt(ctx, *record.CurrentAttemptID)
	if err != nil || attempt == nil || attempt.NextPollAt == nil {
		directRehearsalError(c, http.StatusInternalServerError, "error.order_fetch_failed")
		return
	}
	status := "completed"
	switch a.results[attempt.ID] {
	case "manual_review":
		status = "needs_review"
	case "manual_delivery":
		status = "failed"
	}
	// Wait for the short rehearsal lease so subsequent real-time admin
	// operations do not encounter a future fulfillment timestamp.
	if delay := time.Until(*attempt.NextPollAt); delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	}
	lookupStarted := time.Now()
	startedAt := lookupStarted.UTC()
	gateway := directRehearsalGateway{get: func(_ context.Context, upstreamID uint64) (*kafudacontract.OrderResult, error) {
		result := directRehearsalSupplierOrder(record, attempt, status)
		if upstreamID != result.ID {
			return nil, kafudadomain.ErrInvalidTransition
		}
		if status == "completed" {
			result.RenewalCancelled = a.renewalCancelled
		}
		return result, nil
	}}
	result, err := a.records.PollKnownOrder(ctx, ctx, gateway, attempt.ID, startedAt, directRehearsalPollInterval, kafudaapp.DefaultPollReviewAfter)
	if err != nil {
		directRehearsalError(c, http.StatusInternalServerError, "error.order_fetch_failed")
		return
	}
	receivedAt := startedAt.Add(time.Since(lookupStarted))
	if _, err := kafudaworkflow.ApplyPolledResult(a.fixture.options.OrderStore, record.OrderID, attempt.ID, *result, startedAt, receivedAt, directRehearsalPollInterval, kafudaapp.DefaultPollReviewAfter); err != nil {
		directRehearsalError(c, http.StatusInternalServerError, "error.order_fetch_failed")
		return
	}
	a.respondStage(c, record.OrderID)
}

func directRehearsalSupplierOrder(record *kafudadomain.Recharge, attempt *kafudadomain.Attempt, status string) *kafudacontract.OrderResult {
	return &kafudacontract.OrderResult{
		ID: uint64(attempt.ID), Reference: attempt.Reference, Product: record.Snapshot.Product,
		Plan: record.Snapshot.Plan, Country: record.Snapshot.Country, Status: status,
	}
}

func (a *directRehearsalActions) respondStage(c *gin.Context, childID uint) {
	record, err := a.fixture.store.GetRecharge(c.Request.Context(), childID)
	if err != nil || record == nil {
		directRehearsalError(c, http.StatusInternalServerError, "error.order_fetch_failed")
		return
	}
	response.Success(c, gin.H{"stage": record.Stage})
}
