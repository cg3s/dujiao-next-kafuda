package domain

import (
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

var (
	ErrCatalogInvalid = errors.New("quefa catalog payload invalid")
	ErrCatalogEmpty   = errors.New("quefa catalog has no products")
)

// Catalog persists the latest valid Quefa product catalog snapshot.
type Catalog struct {
	Profile       string     `gorm:"primaryKey;type:varchar(64)" json:"-"`
	RawPayload    string     `gorm:"type:text;not null;default:''" json:"-"`
	ParsedJSON    string     `gorm:"type:text;not null;default:''" json:"-"`
	SyncedAt      *time.Time `gorm:"index" json:"-"`
	Revision      int64      `gorm:"not null;default:0" json:"-"`
	LastErrorCode string     `gorm:"type:varchar(64);not null;default:''" json:"-"`
	LastErrorAt   *time.Time `json:"-"`
	UpdatedAt     time.Time  `gorm:"not null" json:"-"`
}

func (Catalog) TableName() string { return "quefa_catalog_cache" }

// CatalogProduct represents one sellable or paused product from Quefa GET /v1/products.
type CatalogProduct struct {
	ProductCode     string          `json:"product_code"`
	Title           string          `json:"title"`
	SupplyPrice     decimal.Decimal `json:"supply_price"`
	MinSalePrice    decimal.Decimal `json:"min_sale_price"`
	Currency        string          `json:"currency"`
	CollectionModes []string        `json:"collection_modes"`
	DeliveryModes   []string        `json:"delivery_modes"`
	Available       bool            `json:"available"`
}

// SupportsMode checks whether this catalog product supports the given collection and delivery modes.
func (p CatalogProduct) SupportsMode(collectionMode, deliveryMode string) bool {
	if !p.Available {
		return false
	}
	if len(p.CollectionModes) > 0 && !containsFold(p.CollectionModes, collectionMode) {
		return false
	}
	if len(p.DeliveryModes) > 0 && !containsFold(p.DeliveryModes, deliveryMode) {
		return false
	}
	return true
}

// EffectiveSupplyFloor returns the higher of supply_price and min_sale_price.
func (p CatalogProduct) EffectiveSupplyFloor() decimal.Decimal {
	floor := p.SupplyPrice.Round(2)
	if p.MinSalePrice.GreaterThan(floor) {
		floor = p.MinSalePrice.Round(2)
	}
	return floor
}

// ParsedCatalog is the normalized view of a cached catalog snapshot.
type ParsedCatalog struct {
	Products []CatalogProduct `json:"products"`
	Revision int64            `json:"revision"`
	SyncedAt *time.Time       `json:"synced_at,omitempty"`
}

// FindProduct returns the catalog product matching productCode.
func (c *ParsedCatalog) FindProduct(productCode string) (CatalogProduct, bool) {
	if c == nil {
		return CatalogProduct{}, false
	}
	target := strings.TrimSpace(productCode)
	for _, p := range c.Products {
		if strings.EqualFold(p.ProductCode, target) {
			return p, true
		}
	}
	return CatalogProduct{}, false
}

// ParseCatalogPayload parses Quefa GET /v1/products payloads into normalized products.
// It accepts array responses or envelope objects with "data", "products", or "items".
func ParseCatalogPayload(raw []byte) ([]CatalogProduct, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, ErrCatalogInvalid
	}

	var listRaw []json.RawMessage
	if strings.HasPrefix(trimmed, "[") {
		if err := json.Unmarshal(raw, &listRaw); err != nil {
			return nil, ErrCatalogInvalid
		}
	} else if strings.HasPrefix(trimmed, "{") {
		var env map[string]json.RawMessage
		if err := json.Unmarshal(raw, &env); err != nil {
			return nil, ErrCatalogInvalid
		}
		var nested json.RawMessage
		for _, key := range []string{"products", "items", "data"} {
			if candidate, ok := env[key]; ok && len(candidate) > 0 {
				nested = candidate
				break
			}
		}
		if len(nested) == 0 {
			return nil, ErrCatalogInvalid
		}
		nestedTrim := strings.TrimSpace(string(nested))
		if strings.HasPrefix(nestedTrim, "{") {
			var inner map[string]json.RawMessage
			if err := json.Unmarshal(nested, &inner); err != nil {
				return nil, ErrCatalogInvalid
			}
			for _, key := range []string{"products", "items", "list"} {
				if candidate, ok := inner[key]; ok && len(candidate) > 0 {
					nested = candidate
					break
				}
			}
		}
		if err := json.Unmarshal(nested, &listRaw); err != nil {
			return nil, ErrCatalogInvalid
		}
	} else {
		return nil, ErrCatalogInvalid
	}

	if len(listRaw) == 0 {
		return nil, ErrCatalogEmpty
	}

	seen := make(map[string]struct{}, len(listRaw))
	out := make([]CatalogProduct, 0, len(listRaw))
	for _, item := range listRaw {
		p, err := parseCatalogProduct(item)
		if err != nil {
			return nil, err
		}
		key := strings.ToLower(p.ProductCode)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].ProductCode < out[j].ProductCode
	})
	return out, nil
}

func parseCatalogProduct(raw json.RawMessage) (CatalogProduct, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return CatalogProduct{}, ErrCatalogInvalid
	}

	code := firstNonEmptyString(fields, "product_code", "code", "sku_code")
	if code == "" || len(code) > 128 {
		return CatalogProduct{}, ErrCatalogInvalid
	}
	title := firstNonEmptyString(fields, "title", "name", "product_name")
	if title == "" {
		title = code
	}

	supplyPrice, ok := firstDecimal(fields, "supply_price", "cost_price", "wholesale_price", "price")
	if !ok || supplyPrice.LessThanOrEqual(decimal.Zero) {
		return CatalogProduct{}, ErrCatalogInvalid
	}
	minSalePrice, _ := firstDecimal(fields, "min_sale_price", "floor_price", "suggested_min_price")
	if minSalePrice.IsNegative() {
		return CatalogProduct{}, ErrCatalogInvalid
	}

	currency := strings.ToUpper(firstNonEmptyString(fields, "currency", "settlement_currency"))
	if currency == "" {
		currency = "CNY"
	}

	collectionModes := normalizeModeSlice(fields, "collection_modes", "supported_collection_modes", "collection_mode")
	deliveryModes := normalizeModeSlice(fields, "delivery_modes", "supported_delivery_modes", "delivery_mode")

	available := true
	if rawAvail, ok := fields["available"]; ok {
		var b bool
		if err := json.Unmarshal(rawAvail, &b); err == nil {
			available = b
		}
	} else if rawSellable, ok := fields["sellable"]; ok {
		var b bool
		if err := json.Unmarshal(rawSellable, &b); err == nil {
			available = b
		}
	} else if rawInStock, ok := fields["in_stock"]; ok {
		var b bool
		if err := json.Unmarshal(rawInStock, &b); err == nil {
			available = b
		}
	} else if status := strings.ToLower(firstNonEmptyString(fields, "status")); status != "" {
		available = status == "active" || status == "available" || status == "on_sale" || status == "enabled"
	}

	return CatalogProduct{
		ProductCode:     code,
		Title:           title,
		SupplyPrice:     supplyPrice.Round(2),
		MinSalePrice:    minSalePrice.Round(2),
		Currency:        currency,
		CollectionModes: collectionModes,
		DeliveryModes:   deliveryModes,
		Available:       available,
	}, nil
}

func firstNonEmptyString(fields map[string]json.RawMessage, keys ...string) string {
	for _, key := range keys {
		raw, ok := fields[key]
		if !ok || len(raw) == 0 {
			continue
		}
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			if trimmed := strings.TrimSpace(s); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

func firstDecimal(fields map[string]json.RawMessage, keys ...string) (decimal.Decimal, bool) {
	for _, key := range keys {
		raw, ok := fields[key]
		if !ok || len(raw) == 0 {
			continue
		}
		trimmed := strings.TrimSpace(string(raw))
		if trimmed == "" || trimmed == "null" {
			continue
		}
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			if d, err := decimal.NewFromString(strings.TrimSpace(s)); err == nil {
				return d, true
			}
		}
		if d, err := decimal.NewFromString(trimmed); err == nil {
			return d, true
		}
	}
	return decimal.Zero, false
}

func normalizeModeSlice(fields map[string]json.RawMessage, keys ...string) []string {
	for _, key := range keys {
		raw, ok := fields[key]
		if !ok || len(raw) == 0 {
			continue
		}
		var arr []string
		if err := json.Unmarshal(raw, &arr); err == nil {
			out := make([]string, 0, len(arr))
			for _, v := range arr {
				if s := strings.ToLower(strings.TrimSpace(v)); s != "" {
					out = append(out, s)
				}
			}
			return out
		}
		var single string
		if err := json.Unmarshal(raw, &single); err == nil {
			if s := strings.ToLower(strings.TrimSpace(single)); s != "" {
				return []string{s}
			}
		}
	}
	return nil
}

func containsFold(items []string, target string) bool {
	target = strings.TrimSpace(target)
	for _, item := range items {
		if strings.EqualFold(strings.TrimSpace(item), target) {
			return true
		}
	}
	return false
}
