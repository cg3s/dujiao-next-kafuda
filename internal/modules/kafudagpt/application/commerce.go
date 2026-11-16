package application

import (
	"context"
	"time"

	kafudacontract "github.com/dujiao-next/internal/modules/kafudagpt/contract"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
)

type CommerceOptions struct {
	Catalog                 *CatalogService
	SalesEnabled            bool
	ReferenceLookupVerified bool
	ProductCode             string
	MaxCostCents            int64
	AllowedKinds            []string
	WorkerReady             func(context.Context) bool
}

type CommerceService struct{ options CommerceOptions }

func NewCommerceService(options CommerceOptions) *CommerceService {
	return &CommerceService{options: options}
}

func (s *CommerceService) SKUOptions(ctx context.Context) ([]kafudacontract.SKUOption, error) {
	catalog, err := s.options.Catalog.Cached(ctx)
	if err != nil {
		return nil, err
	}
	if catalog == nil {
		return nil, kafudadomain.ErrInvalidCatalog
	}
	result := make([]kafudacontract.SKUOption, 0, len(catalog.Options))
	for _, entry := range catalog.Options {
		result = append(result, kafudacontract.SKUOption{SKUCode: kafudacontract.SKUCode(entry.Plan, entry.Country), Plan: entry.Plan, Country: entry.Country, Label: entry.Label, Available: catalog.Available(entry.Plan, entry.Country, time.Now())})
	}
	return result, nil
}

func (s *CommerceService) PurchaseTerms(ctx context.Context, plan, country string) (*kafudacontract.PurchaseTerms, error) {
	opt := s.options
	if !opt.SalesEnabled || !opt.ReferenceLookupVerified || opt.ProductCode == "" || opt.MaxCostCents <= 0 || len(opt.AllowedKinds) == 0 || opt.WorkerReady == nil || !opt.WorkerReady(ctx) {
		return nil, ErrQuoteUnavailable
	}
	catalog, err := opt.Catalog.Cached(ctx)
	if err != nil {
		return nil, err
	}
	if catalog == nil || !catalog.Available(plan, country, time.Now()) {
		return nil, ErrQuoteUnavailable
	}
	cardAllowed := false
	for _, card := range catalog.Cards {
		if card.ProductCode == opt.ProductCode && !card.Restricted {
			cardAllowed = true
		}
	}
	if !cardAllowed {
		return nil, ErrQuotePolicy
	}
	for _, entry := range catalog.Options {
		if entry.Plan == plan && entry.Country == country {
			return &kafudacontract.PurchaseTerms{Plan: plan, Country: country, Product: entry.Product, PayMode: "auto_card", ProductCode: opt.ProductCode, PaymentProfile: catalog.Profile, MaxCostCents: opt.MaxCostCents, AllowedKinds: append([]string(nil), opt.AllowedKinds...)}, nil
		}
	}
	return nil, ErrQuoteUnavailable
}
