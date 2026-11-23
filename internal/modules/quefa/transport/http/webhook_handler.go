package http

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	quefacontract "github.com/dujiao-next/internal/modules/quefa/contract"
	"github.com/gin-gonic/gin"
)

const maxWebhookBodyBytes = 1 << 20 // 1 MiB

type WebhookConfigReader interface {
	WebhookSecretAndSkew(ctx context.Context) (secret string, maxSkew time.Duration)
}

type WebhookVerifier func(secret, timestamp string, rawBody []byte, signature string, now time.Time, maxSkew time.Duration) error

type WebhookIngestor interface {
	RecordWebhookEvent(ctx context.Context, env quefacontract.WebhookEnvelope) (duplicate bool, childOrderID uint, err error)
}

type WebhookHandler struct {
	Config   WebhookConfigReader
	Verifier WebhookVerifier
	Ingestor WebhookIngestor
	Now      func() time.Time
}

func (h *WebhookHandler) Handle(c *gin.Context) {
	if h == nil || h.Config == nil || h.Verifier == nil || h.Ingestor == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"ok": false, "error": "webhook_unavailable"})
		return
	}
	rawBody, err := io.ReadAll(io.LimitReader(c.Request.Body, maxWebhookBodyBytes+1))
	if err != nil || len(rawBody) == 0 || len(rawBody) > maxWebhookBodyBytes {
		c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": "invalid_body"})
		return
	}

	secret, maxSkew := h.Config.WebhookSecretAndSkew(c.Request.Context())
	if strings.TrimSpace(secret) == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"ok": false, "error": "webhook_secret_not_configured"})
		return
	}

	ts := firstHeader(c, "X-Quefa-Timestamp", "X-Webhook-Timestamp", "X-Timestamp")
	sig := firstHeader(c, "X-Quefa-Signature", "X-Webhook-Signature", "X-Signature")
	now := time.Now()
	if h.Now != nil {
		now = h.Now()
	}
	if err := h.Verifier(secret, ts, rawBody, sig, now, maxSkew); err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"ok": false, "error": "invalid_signature"})
		return
	}

	var env quefacontract.WebhookEnvelope
	if err := json.Unmarshal(rawBody, &env); err != nil || strings.TrimSpace(env.EventID) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": "invalid_event_payload"})
		return
	}

	dup, _, err := h.Ingestor.RecordWebhookEvent(c.Request.Context(), env)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "error": "event_persist_failed"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "duplicate": dup})
}

func firstHeader(c *gin.Context, names ...string) string {
	for _, name := range names {
		if v := strings.TrimSpace(c.GetHeader(name)); v != "" {
			return v
		}
	}
	return ""
}
