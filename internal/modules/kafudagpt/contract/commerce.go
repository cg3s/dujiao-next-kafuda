package contract

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
)

type SKUOption struct {
	SKUCode   string `json:"sku_code"`
	Plan      string `json:"plan"`
	Country   string `json:"country"`
	Label     string `json:"label"`
	Available bool   `json:"available"`
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
	PurchaseTerms(context.Context, string, string) (*PurchaseTerms, error)
}

func SKUCode(plan, country string) string {
	sum := sha256.Sum256([]byte(plan + "\x00" + country))
	return "KGPT_" + hex.EncodeToString(sum[:28])
}
