package productwrite

import (
	"context"
	"sort"

	productcontract "github.com/dujiao-next/internal/modules/catalog/product/contract"
	productdomain "github.com/dujiao-next/internal/modules/catalog/product/domain"
	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	"github.com/shopspring/decimal"
)

func validateDirectTypeChange(product *productdomain.Product, repositories TransactionRepositories) error {
	if product.IsMapped || product.ManualStockTotal != 0 || product.ManualStockLocked != 0 || product.ManualStockSold != 0 || repositories.Orders == nil || repositories.SKUs == nil || repositories.CardSecrets == nil {
		return productcontract.ErrFulfillmentInvalid
	}
	count, err := repositories.Orders.CountOrderItemsByProduct(product.ID)
	if err != nil {
		return err
	}
	if count != 0 {
		return productcontract.ErrFulfillmentInvalid
	}
	skus, err := repositories.SKUs.ListByProduct(product.ID, false)
	if err != nil {
		return err
	}
	for _, sku := range skus {
		if sku.ManualStockTotal != 0 || sku.ManualStockLocked != 0 || sku.ManualStockSold != 0 {
			return productcontract.ErrFulfillmentInvalid
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

func prepareDirectSKUs(inputs []ProductSKUInput, existing map[uint]productdomain.ProductSKU, options []kafudacontract.SKUOption) ([]ProductSKUInput, error) {
	oldByCode := make(map[string]productdomain.ProductSKU, len(existing))
	for _, sku := range existing {
		oldByCode[sku.SKUCode] = sku
	}
	provided := make(map[string]ProductSKUInput, len(inputs))
	for _, input := range inputs {
		if _, duplicate := provided[input.SKUCode]; duplicate {
			return nil, productcontract.ErrProductSKUInvalid
		}
		if input.ID != 0 {
			old, ok := existing[input.ID]
			if !ok || old.SKUCode != input.SKUCode {
				return nil, productcontract.ErrProductSKUInvalid
			}
		}
		provided[input.SKUCode] = input
	}
	result := make([]ProductSKUInput, 0, len(options)+len(existing))
	seen := make(map[string]bool)
	for _, option := range options {
		seen[option.SKUCode] = true
		active := false
		item := ProductSKUInput{SKUCode: option.SKUCode, PriceAmount: decimal.Zero, CostPriceAmount: decimal.Zero, IsActive: &active}
		if old, ok := oldByCode[option.SKUCode]; ok {
			active = old.IsActive && old.DirectCatalogRevision == option.CatalogRevision
			item.ID, item.PriceAmount, item.CostPriceAmount = old.ID, old.PriceAmount.Decimal, old.CostPriceAmount.Decimal
			item.CostPriceProvided, item.SortOrder = old.DirectCostConfigured, old.SortOrder
		}
		if input, ok := provided[option.SKUCode]; ok {
			if item.ID != 0 && input.ID != 0 && item.ID != input.ID {
				return nil, productcontract.ErrProductSKUInvalid
			}
			item.PriceAmount, item.SortOrder = input.PriceAmount, input.SortOrder
			if input.CostPriceProvided {
				item.CostPriceAmount, item.CostPriceProvided = input.CostPriceAmount, true
			}
			if input.IsActive != nil {
				active = *input.IsActive
			}
		}
		if !option.Available {
			active = false
		}
		if input, supplied := provided[option.SKUCode]; active && supplied {
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
		item.SpecValuesJSON = map[string]interface{}{"plan": option.Plan, "country": option.Country}
		result = append(result, item)
	}
	// Disappeared entries stay attached to their historical IDs and prices.
	for code, old := range oldByCode {
		if seen[code] {
			continue
		}
		active := false
		revision := old.DirectCatalogRevision
		result = append(result, ProductSKUInput{ID: old.ID, SKUCode: code, SpecValuesJSON: old.SpecValuesJSON, PriceAmount: old.PriceAmount.Decimal, CostPriceAmount: old.CostPriceAmount.Decimal, CostPriceProvided: old.DirectCostConfigured, DirectCatalogRevision: &revision, IsActive: &active, SortOrder: old.SortOrder})
		seen[code] = true
	}
	for code := range provided {
		if !seen[code] {
			return nil, productcontract.ErrProductSKUInvalid
		}
	}
	if len(result) == 0 {
		return nil, productcontract.ErrProductSKUInvalid
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].SKUCode < result[j].SKUCode })
	return result, nil
}
