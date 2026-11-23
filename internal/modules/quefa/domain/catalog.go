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
	QuoteRequired   bool            `json:"quote_required"`
	Currency        string          `json:"currency"`
	FulfillmentMode string          `json:"fulfillment_mode,omitempty"`
	PaymentCountry  string          `json:"payment_country,omitempty"`
	PaymentCurrency string          `json:"payment_currency,omitempty"`
	MaxQuantity     int             `json:"max_quantity,omitempty"`
	CollectionModes []string        `json:"collection_modes"`
	DeliveryModes   []string        `json:"delivery_modes"`
	Available       bool            `json:"available"`
}

// SupportsMode checks whether this catalog product supports fixed-price sales for the given collection and delivery modes.
func (p CatalogProduct) SupportsMode(collectionMode, deliveryMode string) bool {
	if !p.Available || p.QuoteRequired || p.SupplyPrice.LessThanOrEqual(decimal.Zero) {
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
		for _, key := range []string{"products", "items", "list", "catalog", "records", "results", "data"} {
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
			for _, key := range []string{"products", "items", "list", "catalog", "records", "results", "data"} {
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

// DescribePayloadShape returns a secret-free structural summary (JSON keys only) for diagnostics.
func DescribePayloadShape(raw []byte) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return "empty"
	}
	if strings.HasPrefix(trimmed, "[") {
		var list []json.RawMessage
		if err := json.Unmarshal(raw, &list); err != nil {
			return "array(invalid)"
		}
		if len(list) == 0 {
			return "array(len=0)"
		}
		return "array(len=" + decimal.NewFromInt(int64(len(list))).String() + ", item_keys=[" + strings.Join(objectKeys(list[0]), ",") + "])"
	}
	if strings.HasPrefix(trimmed, "{") {
		keys := objectKeys(raw)
		var env map[string]json.RawMessage
		if err := json.Unmarshal(raw, &env); err == nil {
			for _, k := range []string{"products", "items", "list", "catalog", "records", "results", "data"} {
				if sub, ok := env[k]; ok {
					st := strings.TrimSpace(string(sub))
					if strings.HasPrefix(st, "[") {
						var list []json.RawMessage
						if json.Unmarshal(sub, &list) == nil && len(list) > 0 {
							return "object(keys=[" + strings.Join(keys, ",") + "], " + k + "_item_keys=[" + strings.Join(objectKeys(list[0]), ",") + "])"
						}
					} else if strings.HasPrefix(st, "{") {
						return "object(keys=[" + strings.Join(keys, ",") + "], " + k + "_keys=[" + strings.Join(objectKeys(sub), ",") + "])"
					}
				}
			}
		}
		return "object(keys=[" + strings.Join(keys, ",") + "])"
	}
	return "non-json"
}

func objectKeys(raw json.RawMessage) []string {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) > 20 {
		keys = keys[:20]
	}
	return keys
}

func parseCatalogProduct(raw json.RawMessage) (CatalogProduct, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return CatalogProduct{}, ErrCatalogInvalid
	}
	// Flatten optional nested pricing/price_info objects as fallback lookup sources.
	lookup := make(map[string]json.RawMessage, len(fields)+8)
	for _, nestedKey := range []string{"pricing", "price_info", "price_detail"} {
		if sub, ok := fields[nestedKey]; ok && len(sub) > 0 {
			var subMap map[string]json.RawMessage
			if json.Unmarshal(sub, &subMap) == nil {
				for k, v := range subMap {
					if _, exists := fields[k]; !exists {
						lookup[k] = v
					}
				}
			}
		}
	}
	for k, v := range fields {
		lookup[k] = v
	}

	code := firstNonEmptyString(lookup, "product_code", "code", "sku_code", "sku", "product_id", "id")
	if code == "" || len(code) > 128 {
		return CatalogProduct{}, ErrCatalogInvalid
	}
	title := firstNonEmptyString(lookup, "title", "name", "product_name", "display_name")
	if title == "" {
		title = code
	}

	available := resolveProductAvailability(lookup)
	quoteRequired, _ := readBoolField(lookup, "quote_required")
	fulfillmentMode := strings.ToLower(firstNonEmptyString(lookup, "fulfillment_mode"))
	paymentCountry := strings.ToUpper(firstNonEmptyString(lookup, "payment_country"))
	paymentCurrency := strings.ToUpper(firstNonEmptyString(lookup, "payment_currency"))
	var maxQuantity int
	if mqRaw, ok := lookup["max_quantity"]; ok && len(mqRaw) > 0 {
		_ = json.Unmarshal(mqRaw, &maxQuantity)
	}

	supplyPrice, ok := firstDecimal(lookup,
		"supply_price", "cost_price", "wholesale_price", "agent_price", "partner_price",
		"supply_amount", "cost_amount", "unit_price", "price", "sale_price", "sell_price", "base_price",
	)
	if !ok {
		if cents, centsOK := firstDecimal(lookup,
			"supply_price_cents", "cost_price_cents", "wholesale_price_cents", "agent_price_cents",
			"unit_price_cents", "price_cents", "cost_cents",
		); centsOK {
			supplyPrice = cents.Div(decimal.NewFromInt(100))
			ok = true
		}
	}
	if !ok || supplyPrice.IsNegative() {
		if (!available || quoteRequired) && !ok {
			supplyPrice = decimal.Zero
		} else {
			return CatalogProduct{}, ErrCatalogInvalid
		}
	}
	if supplyPrice.IsZero() && available && !quoteRequired {
		return CatalogProduct{}, ErrCatalogInvalid
	}

	minSalePrice, minOK := firstDecimal(lookup,
		"min_sale_price", "floor_price", "suggested_min_price", "min_sell_price", "min_price",
	)
	if !minOK {
		if cents, centsOK := firstDecimal(lookup, "min_sale_price_cents", "floor_price_cents", "min_price_cents"); centsOK {
			minSalePrice = cents.Div(decimal.NewFromInt(100))
		}
	}
	if minSalePrice.IsNegative() {
		return CatalogProduct{}, ErrCatalogInvalid
	}

	currency := strings.ToUpper(firstNonEmptyString(lookup, "currency", "settlement_currency"))
	if currency == "" {
		currency = "CNY"
	}

	collectionModes := normalizeModeSlice(lookup, "collection_modes", "supported_collection_modes", "collection_mode")
	if len(collectionModes) == 0 {
		collectionModes = inferCollectionModesFromFlags(lookup)
	}
	deliveryModes := normalizeModeSlice(lookup, "delivery_modes", "supported_delivery_modes", "delivery_mode")
	if len(deliveryModes) == 0 {
		deliveryModes = inferDeliveryModesFromFlags(lookup)
	}

	return CatalogProduct{
		ProductCode:     code,
		Title:           title,
		SupplyPrice:     supplyPrice.Round(2),
		MinSalePrice:    minSalePrice.Round(2),
		QuoteRequired:   quoteRequired,
		Currency:        currency,
		FulfillmentMode: fulfillmentMode,
		PaymentCountry:  paymentCountry,
		PaymentCurrency: paymentCurrency,
		MaxQuantity:     maxQuantity,
		CollectionModes: collectionModes,
		DeliveryModes:   deliveryModes,
		Available:       available,
	}, nil
}

func resolveProductAvailability(fields map[string]json.RawMessage) bool {
	for _, key := range []string{"available", "is_available", "sellable", "can_order", "can_purchase", "in_stock", "is_active", "enabled"} {
		if raw, ok := fields[key]; ok {
			var b bool
			if err := json.Unmarshal(raw, &b); err == nil {
				return b
			}
		}
	}
	if status := strings.ToLower(firstNonEmptyString(fields, "status", "sale_status")); status != "" {
		return status == "active" || status == "available" || status == "on_sale" || status == "enabled" || status == "published" || status == "online" || status == "normal"
	}
	return true
}

func inferCollectionModesFromFlags(fields map[string]json.RawMessage) []string {
	var modes []string
	if b, ok := readBoolField(fields, "supports_platform_collect", "platform_collect_enabled"); ok && b {
		modes = append(modes, CollectionModePlatformCollect)
	}
	if b, ok := readBoolField(fields, "supports_agent_collect", "agent_collect_enabled"); ok && b {
		modes = append(modes, CollectionModeAgentCollect)
	}
	return modes
}

func inferDeliveryModesFromFlags(fields map[string]json.RawMessage) []string {
	var modes []string
	if b, ok := readBoolField(fields, "supports_auto_recharge", "auto_recharge_enabled"); ok && b {
		modes = append(modes, DeliveryModeAutoRecharge)
	}
	if b, ok := readBoolField(fields, "supports_cdk", "cdk_enabled"); ok && b {
		modes = append(modes, "cdk")
	}
	return modes
}

func readBoolField(fields map[string]json.RawMessage, keys ...string) (bool, bool) {
	for _, key := range keys {
		raw, ok := fields[key]
		if !ok || len(raw) == 0 {
			continue
		}
		var b bool
		if err := json.Unmarshal(raw, &b); err == nil {
			return b, true
		}
	}
	return false, false
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
