package productwrite

import (
	"context"
	"fmt"
	"sort"
	"strings"

	productcontract "github.com/dujiao-next/internal/modules/catalog/product/contract"
	productdomain "github.com/dujiao-next/internal/modules/catalog/product/domain"
	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	"github.com/shopspring/decimal"
)

func validateDirectTypeChange(product *productdomain.Product, repositories TransactionRepositories) error {
	if product.IsMapped {
		return fmt.Errorf("%w: mapped product cannot change fulfillment type", productcontract.ErrFulfillmentInvalid)
	}
	if product.ManualStockTotal != 0 || product.ManualStockLocked != 0 || product.ManualStockSold != 0 {
		return fmt.Errorf("%w: product manual stock must be 0 (total=%d locked=%d sold=%d)", productcontract.ErrFulfillmentInvalid, product.ManualStockTotal, product.ManualStockLocked, product.ManualStockSold)
	}
	if repositories.Orders == nil || repositories.SKUs == nil || repositories.CardSecrets == nil {
		return fmt.Errorf("%w: required repositories unavailable", productcontract.ErrFulfillmentInvalid)
	}
	count, err := repositories.Orders.CountOrderItemsByProduct(product.ID)
	if err != nil {
		return err
	}
	if count != 0 {
		return fmt.Errorf("%w: product has %d historical order items", productcontract.ErrFulfillmentInvalid, count)
	}
	skus, err := repositories.SKUs.ListByProduct(product.ID, false)
	if err != nil {
		return err
	}
	for _, sku := range skus {
		if sku.ManualStockTotal != 0 || sku.ManualStockLocked != 0 || sku.ManualStockSold != 0 {
			return fmt.Errorf("%w: sku %d manual stock must be 0 (total=%d locked=%d sold=%d)", productcontract.ErrFulfillmentInvalid, sku.ID, sku.ManualStockTotal, sku.ManualStockLocked, sku.ManualStockSold)
		}
	}
	// Card secrets remain associated with their original product even if a
	// previous edit changed its fulfillment type. Inspect every source type.
	total, available, used, err := repositories.CardSecrets.CountByProduct(product.ID, 0)
	if err != nil {
		return err
	}
	if total != 0 || available != 0 || used != 0 {
		return productcontract.ErrProductSKUHasCardSecretStock
	}
	return nil
}

type directCatalogForUpdate interface {
	SKUOptionsForUpdate(kafudacontract.CatalogForUpdate) ([]kafudacontract.SKUOption, error)
}

// directSKUOptions is the pre-transaction reader for creation and policies
// without a transaction-bound catalog.
func (s *WriteService) directSKUOptions() ([]kafudacontract.SKUOption, error) {
	if s.directCatalog == nil {
		return nil, productcontract.ErrProductSKUInvalid
	}
	options, err := s.directCatalog.SKUOptions(context.Background())
	if err != nil {
		return nil, productcontract.ErrProductSKUInvalid
	}
	return options, nil
}

var directSpecLocales = []string{"zh-CN", "zh-TW", "en-US"}

func directOptionKeyFromSpec(spec map[string]interface{}) (string, bool) {
	if spec == nil {
		return "", false
	}
	plan, _ := spec["plan"].(string)
	country, _ := spec["country"].(string)
	plan = strings.TrimSpace(plan)
	country = strings.TrimSpace(country)
	if plan == "" || country == "" {
		return "", false
	}
	return kafudacontract.OptionKey(plan, country), true
}

func buildDirectSpecValues(plan, country, defaultLabel string, oldSpec, inputSpec map[string]interface{}, hasOld bool) map[string]interface{} {
	spec := map[string]interface{}{"plan": plan, "country": country}
	source := oldSpec
	if inputSpec != nil {
		source = inputSpec
	}
	hasLocale := false
	for _, lang := range directSpecLocales {
		if text, ok := source[lang].(string); ok {
			if trimmed := strings.TrimSpace(text); trimmed != "" {
				spec[lang] = trimmed
				hasLocale = true
			}
		}
	}
	if !hasLocale && !hasOld && inputSpec == nil {
		if label := strings.TrimSpace(defaultLabel); label != "" {
			for _, lang := range directSpecLocales {
				spec[lang] = label
			}
		}
	}
	return spec
}

func prepareDirectSKUs(inputs []ProductSKUInput, existing map[uint]productdomain.ProductSKU, options []kafudacontract.SKUOption) ([]ProductSKUInput, error) {
	oldByKey := make(map[string]productdomain.ProductSKU, len(existing))
	oldByCode := make(map[string]productdomain.ProductSKU, len(existing))
	for _, sku := range existing {
		if plan, country, valid := sku.DirectSelection(); valid {
			oldByKey[kafudacontract.OptionKey(plan, country)] = sku
		}
		if code := strings.TrimSpace(sku.SKUCode); code != "" {
			oldByCode[code] = sku
		}
	}
	optionByDefaultCode := make(map[string]kafudacontract.SKUOption, len(options))
	for _, option := range options {
		if code := strings.TrimSpace(option.SKUCode); code != "" {
			optionByDefaultCode[code] = option
		}
	}
	provided := make(map[string]ProductSKUInput, len(inputs))
	for _, input := range inputs {
		trimmedCode := strings.TrimSpace(input.SKUCode)
		specKey, hasSpecKey := directOptionKeyFromSpec(input.SpecValuesJSON)
		var key string
		if input.ID != 0 {
			old, ok := existing[input.ID]
			if !ok {
				return nil, productcontract.ErrProductSKUInvalid
			}
			oldPlan, oldCountry, oldValid := old.DirectSelection()
			if !oldValid {
				return nil, productcontract.ErrProductSKUInvalid
			}
			oldKey := kafudacontract.OptionKey(oldPlan, oldCountry)
			if hasSpecKey && specKey != oldKey {
				return nil, productcontract.ErrProductSKUInvalid
			}
			key = oldKey
		} else if hasSpecKey {
			key = specKey
		} else if opt, ok := optionByDefaultCode[trimmedCode]; ok {
			key = kafudacontract.OptionKey(opt.Plan, opt.Country)
		} else if old, ok := oldByCode[trimmedCode]; ok {
			if oldPlan, oldCountry, oldValid := old.DirectSelection(); oldValid {
				key = kafudacontract.OptionKey(oldPlan, oldCountry)
			}
		}
		if key == "" {
			return nil, productcontract.ErrProductSKUInvalid
		}
		if _, duplicate := provided[key]; duplicate {
			return nil, productcontract.ErrProductSKUInvalid
		}
		provided[key] = input
	}
	result := make([]ProductSKUInput, 0, len(options)+len(existing))
	seen := make(map[string]bool)
	for _, option := range options {
		key := kafudacontract.OptionKey(option.Plan, option.Country)
		seen[key] = true
		active := false
		defaultCode := strings.TrimSpace(option.SKUCode)
		if defaultCode == "" {
			defaultCode = kafudacontract.SKUCode(option.Plan, option.Country)
		}
		item := ProductSKUInput{SKUCode: defaultCode, PriceAmount: decimal.Zero, CostPriceAmount: decimal.Zero, IsActive: &active}
		old, hasOld := oldByKey[key]
		if !hasOld {
			if legacyOld, ok := oldByCode[defaultCode]; ok {
				old, hasOld = legacyOld, true
			}
		}
		if hasOld {
			active = old.IsActive && old.DirectCatalogRevision == option.CatalogRevision
			item.ID, item.SKUCode, item.PriceAmount, item.CostPriceAmount = old.ID, old.SKUCode, old.PriceAmount.Decimal, old.CostPriceAmount.Decimal
			item.CostPriceProvided, item.SortOrder = old.DirectCostConfigured, old.SortOrder
		}
		var inputSpec map[string]interface{}
		if input, ok := provided[key]; ok {
			if item.ID != 0 && input.ID != 0 && item.ID != input.ID {
				return nil, productcontract.ErrProductSKUInvalid
			}
			if trimmedCode := strings.TrimSpace(input.SKUCode); trimmedCode != "" {
				item.SKUCode = trimmedCode
			}
			if !input.KeepPriceAmount {
				item.PriceAmount = input.PriceAmount
			}
			if !input.KeepSortOrder {
				item.SortOrder = input.SortOrder
			}
			if input.CostPriceProvided {
				item.CostPriceAmount, item.CostPriceProvided = input.CostPriceAmount, true
			}
			if input.IsActive != nil {
				active = *input.IsActive
			}
			item.AffiliateRatePercent, item.AffiliateRatePercentSet = input.AffiliateRatePercent, input.AffiliateRatePercentSet
			inputSpec = input.SpecValuesJSON
		}
		if !option.Available {
			active = false
		}
		if input, supplied := provided[key]; active && supplied {
			if (option.CatalogRevision > 0 && input.DirectCatalogRevision == nil) ||
				(input.DirectCatalogRevision != nil && *input.DirectCatalogRevision != option.CatalogRevision) {
				return nil, productcontract.ErrProductSKUInvalid
			}
		}
		revision := option.CatalogRevision
		item.DirectCatalogRevision = &revision
		if active && !item.CostPriceProvided {
			return nil, productcontract.ErrProductPriceInvalid
		}
		var oldSpec map[string]interface{}
		if hasOld && old.SpecValuesJSON != nil {
			oldSpec = map[string]interface{}(old.SpecValuesJSON)
		}
		item.SpecValuesJSON = buildDirectSpecValues(option.Plan, option.Country, option.Label, oldSpec, inputSpec, hasOld)
		result = append(result, item)
	}
	// Disappeared entries stay attached to their historical IDs and prices.
	for key, old := range oldByKey {
		if seen[key] {
			continue
		}
		active := false
		revision := old.DirectCatalogRevision
		code := old.SKUCode
		spec := map[string]interface{}(old.SpecValuesJSON)
		if input, ok := provided[key]; ok {
			if trimmedCode := strings.TrimSpace(input.SKUCode); trimmedCode != "" {
				code = trimmedCode
			}
			if plan, country, valid := old.DirectSelection(); valid && input.SpecValuesJSON != nil {
				spec = buildDirectSpecValues(plan, country, "", map[string]interface{}(old.SpecValuesJSON), input.SpecValuesJSON, true)
			}
		}
		input := provided[key]
		result = append(result, ProductSKUInput{AffiliateRatePercent: input.AffiliateRatePercent, AffiliateRatePercentSet: input.AffiliateRatePercentSet, ID: old.ID, SKUCode: code, SpecValuesJSON: spec, PriceAmount: old.PriceAmount.Decimal, CostPriceAmount: old.CostPriceAmount.Decimal, CostPriceProvided: old.DirectCostConfigured, DirectCatalogRevision: &revision, IsActive: &active, SortOrder: old.SortOrder})
		seen[key] = true
	}
	for key := range provided {
		if !seen[key] {
			return nil, productcontract.ErrProductSKUInvalid
		}
	}
	if len(result) == 0 {
		return nil, productcontract.ErrProductSKUInvalid
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].SKUCode < result[j].SKUCode })
	return result, nil
}
