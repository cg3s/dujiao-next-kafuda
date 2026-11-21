package productwrite

import (
	"testing"

	productdomain "github.com/dujiao-next/internal/modules/catalog/product/domain"
	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	"github.com/dujiao-next/internal/shared/jsonmap"
	"github.com/dujiao-next/internal/shared/money"
	"github.com/shopspring/decimal"
)

func TestPrepareDirectSKUsDefaultsAndCustomEdits(t *testing.T) {
	option := kafudacontract.SKUOption{
		CatalogRevision: 2,
		SKUCode:         kafudacontract.SKUCode("plus", "US"),
		Plan:            "plus",
		Country:         "US",
		Label:           "ChatGPT Plus (US)",
		Available:       true,
	}

	// 1. Create with no explicit inputs: defaults SKUCode to GPT-PLUS-US and localized spec names to option.Label.
	created, err := prepareDirectSKUs(nil, nil, []kafudacontract.SKUOption{option})
	if err != nil {
		t.Fatalf("prepareDirectSKUs create failed: %v", err)
	}
	if len(created) != 1 {
		t.Fatalf("expected 1 SKU, got %d", len(created))
	}
	if created[0].SKUCode != "GPT-PLUS-US" {
		t.Fatalf("default SKUCode = %q, want GPT-PLUS-US", created[0].SKUCode)
	}
	if created[0].SpecValuesJSON["zh-CN"] != "ChatGPT Plus (US)" ||
		created[0].SpecValuesJSON["zh-TW"] != "ChatGPT Plus (US)" ||
		created[0].SpecValuesJSON["en-US"] != "ChatGPT Plus (US)" ||
		created[0].SpecValuesJSON["plan"] != "plus" ||
		created[0].SpecValuesJSON["country"] != "US" {
		t.Fatalf("unexpected default SpecValuesJSON: %#v", created[0].SpecValuesJSON)
	}

	// 2. Edit an existing legacy KGPT_ SKU: allow custom SKUCode and custom localized spec names,
	// while preserving plan/country and dropping unknown private keys.
	existing := map[uint]productdomain.ProductSKU{
		7: {
			ID:                    7,
			SKUCode:               "KGPT_legacyhash",
			SpecValuesJSON:        jsonmap.JSON{"plan": "plus", "country": "US"},
			PriceAmount:           money.FromDecimal(decimal.NewFromInt(120)),
			CostPriceAmount:       money.FromDecimal(decimal.NewFromInt(80)),
			DirectCostConfigured:  true,
			DirectCatalogRevision: 2,
			IsActive:              true,
		},
	}
	active := true
	rev := uint64(2)
	updated, err := prepareDirectSKUs([]ProductSKUInput{
		{
			ID:                    7,
			SKUCode:               "MY-GPT-PLUS-US",
			SpecValuesJSON:        map[string]interface{}{"plan": "plus", "country": "US", "zh-CN": "美区 Plus 月卡", "en-US": "US Plus Monthly", "merchant_cost": "secret"},
			PriceAmount:           decimal.NewFromInt(120),
			CostPriceAmount:       decimal.NewFromInt(80),
			CostPriceProvided:     true,
			DirectCatalogRevision: &rev,
			IsActive:              &active,
		},
	}, existing, []kafudacontract.SKUOption{option})
	if err != nil {
		t.Fatalf("prepareDirectSKUs update failed: %v", err)
	}
	if len(updated) != 1 {
		t.Fatalf("expected 1 SKU, got %d", len(updated))
	}
	if updated[0].ID != 7 || updated[0].SKUCode != "MY-GPT-PLUS-US" {
		t.Fatalf("updated SKU = (%d, %q), want (7, MY-GPT-PLUS-US)", updated[0].ID, updated[0].SKUCode)
	}
	if updated[0].SpecValuesJSON["zh-CN"] != "美区 Plus 月卡" || updated[0].SpecValuesJSON["en-US"] != "US Plus Monthly" {
		t.Fatalf("localized spec names not preserved: %#v", updated[0].SpecValuesJSON)
	}
	if _, leaked := updated[0].SpecValuesJSON["merchant_cost"]; leaked {
		t.Fatalf("private key leaked into SpecValuesJSON: %#v", updated[0].SpecValuesJSON)
	}
}
