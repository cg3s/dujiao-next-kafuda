package queueadapter

import (
	"errors"
	"fmt"
	"strings"
	"time"

	ordercontract "github.com/dujiao-next/internal/modules/order/contract"

	"github.com/dujiao-next/internal/queue"
	"github.com/hibiken/asynq"
)

// Queue 把平台异步队列适配为订单应用端口。
type Queue struct {
	client           *queue.Client
	directCompletion bool
}

var _ ordercontract.Queue = (*Queue)(nil)

func New(client *queue.Client) *Queue {
	return &Queue{client: client}
}

// NewDirectCompletion uses a stable Redis task identity for retried notices.
func NewDirectCompletion(client *queue.Client) *Queue {
	return &Queue{client: client, directCompletion: true}
}

func (q *Queue) Enabled() bool {
	return q != nil && q.client != nil && q.client.Enabled()
}

func (q *Queue) EnqueueTimeoutCancel(orderID uint, delay time.Duration) error {
	if q == nil || q.client == nil {
		return nil
	}
	return q.client.EnqueueOrderTimeoutCancel(queue.OrderTimeoutCancelPayload{OrderID: orderID}, delay)
}

func (q *Queue) EnqueueStatusEmail(orderID uint, status string) error {
	if q == nil || q.client == nil {
		return nil
	}
	payload := queue.OrderStatusEmailPayload{OrderID: orderID, Status: strings.TrimSpace(status)}
	if q.directCompletion {
		err := q.client.EnqueueOrderStatusEmail(payload, asynq.TaskID(fmt.Sprintf("kafuda-completed:%d", orderID)))
		if errors.Is(err, asynq.ErrTaskIDConflict) {
			return nil
		}
		return err
	}
	return q.client.EnqueueOrderStatusEmail(payload)
}
