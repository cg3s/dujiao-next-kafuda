package domain

import (
	"strings"
	"time"
)

// CatalogOption is our normalized representation of a confirmed sellable pair,
// not a wire schema for the supplier's catalog endpoint. No Cartesian expansion
// of independent plan and region lists is allowed.
type CatalogOption struct {
	Plan              string `json:"plan"`
	Country           string `json:"country"`
	Product           string `json:"product"`
	Label             string `json:"label"`
	Currency          string `json:"currency"`
	EstimatedUSDCents *int64 `json:"estimated_usd_cents"`
	Available         bool   `json:"available"`
}

type CardProduct struct {
	ProductCode    string `json:"product_code"`
	Name           string `json:"name"`
	Restricted     bool   `json:"restricted"`
	GPTRecommended bool   `json:"gpt_recommended"`
	OpenFeeCents   int64  `json:"open_fee_cents"`
	MinAmountCents int64  `json:"min_amount_cents"`
}

type Catalog struct {
	Profile   string          `gorm:"primaryKey;type:varchar(64)" json:"-"`
	Options   []CatalogOption `gorm:"serializer:json;type:json;not null" json:"-"`
	Cards     []CardProduct   `gorm:"serializer:json;type:json;not null" json:"-"`
	Enabled   bool            `gorm:"not null;default:false" json:"-"`
	SyncedAt  time.Time       `json:"-"`
	ExpiresAt time.Time       `gorm:"not null" json:"-"`
}

func (Catalog) TableName() string { return "kafuda_gpt_catalogs" }

func (c Catalog) Validate() error {
	if c.Profile == "" || len(c.Profile) > 64 || c.SyncedAt.IsZero() || !c.ExpiresAt.After(c.SyncedAt) || c.Options == nil {
		return ErrInvalidCatalog
	}
	seen := make(map[string]bool, len(c.Options))
	for _, option := range c.Options {
		key := option.Plan + "\x00" + option.Country
		if option.Plan == "" || option.Country == "" || strings.TrimSpace(option.Plan) != option.Plan || strings.TrimSpace(option.Country) != option.Country || strings.ContainsAny(option.Plan+option.Country, "\x00") || seen[key] {
			return ErrInvalidCatalog
		}
		seen[key] = true
	}
	return nil
}

func (c Catalog) Available(plan, country string, now time.Time) bool {
	if !c.Enabled || !now.Before(c.ExpiresAt) {
		return false
	}
	for _, option := range c.Options {
		if option.Plan == plan && option.Country == country && option.Available {
			return true
		}
	}
	return false
}
