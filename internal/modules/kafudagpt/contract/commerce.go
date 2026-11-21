package contract

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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

func SKUCode(plan, country string) string {
	sum := sha256.Sum256([]byte(plan + "\x00" + country))
	return "KGPT_" + hex.EncodeToString(sum[:28])
}
