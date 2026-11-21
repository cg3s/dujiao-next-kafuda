package contract

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

var (
	ErrCatalogReadFailed  = errors.New("kafuda catalog cache read failed")
	ErrCatalogWriteFailed = errors.New("kafuda catalog cache write failed")
)

type SKUOption struct {
	CatalogRevision     uint64   `json:"catalog_revision"`
	SKUCode             string   `json:"sku_code"`
	Plan                string   `json:"plan"`
	Country             string   `json:"country"`
	Label               string   `json:"label"`
	Currency            string   `json:"currency,omitempty"`
	EstimatedLocalMinor *int64   `json:"estimated_local_minor,omitempty"`
	LocalExponent       *int     `json:"local_exponent,omitempty"`
	EstimatedCostCNY    *float64 `json:"estimated_cost_cny,omitempty"`
	// Available reflects supplier listing state. Purchasing also requires a fresh catalog.
	Available bool `json:"available"`
}

// FXRateProvider supplies exchange rates converting foreign currencies to CNY
// (each map value is CNY per 1 unit of the keyed ISO-4217 currency).
type FXRateProvider interface {
	RatesToCNY(ctx context.Context) (map[string]float64, time.Time, error)
}

type PurchaseTerms struct {
	Plan           string   `json:"plan"`
	Country        string   `json:"country"`
	Product        string   `json:"product"`
	PayMode        string   `json:"pay_mode"`
	ProductCode    string   `json:"product_code"`
	PaymentProfile string   `json:"payment_profile"`
	MaxCostCents   int64    `json:"max_cost_cents"`
	AllowedKinds   []string `json:"allowed_kinds"`
}

type CommercePolicy interface {
	SKUOptions(context.Context) ([]SKUOption, error)
	PurchaseTerms(context.Context, string, string, uint64) (*PurchaseTerms, error)
}

func OptionKey(plan, country string) string {
	return strings.TrimSpace(plan) + "\x00" + strings.TrimSpace(country)
}

func sanitizeSKUCodeSegment(raw string) string {
	var b strings.Builder
	lastSep := false
	for _, r := range strings.ToUpper(strings.TrimSpace(raw)) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastSep = false
		} else if !lastSep && b.Len() > 0 {
			b.WriteByte('-')
			lastSep = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func SKUCode(plan, country string) string {
	parts := []string{"GPT"}
	if p := sanitizeSKUCodeSegment(plan); p != "" {
		parts = append(parts, p)
	}
	if c := sanitizeSKUCodeSegment(country); c != "" {
		parts = append(parts, c)
	}
	if len(parts) > 1 {
		if code := strings.Join(parts, "-"); len(code) <= 64 {
			return code
		}
	}
	sum := sha256.Sum256([]byte(plan + "\x00" + country))
	return "KGPT_" + hex.EncodeToString(sum[:28])
}
