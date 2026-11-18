package application

import (
	"context"
	"errors"
	"testing"
)

func TestCommerceWithoutFulfillmentCannotSell(t *testing.T) {
	ctx := context.Background()
	service := NewCommerceService(CommerceOptions{
		Catalog: &CatalogService{}, SalesEnabled: true, ReferenceLookupVerified: true,
		ProductCode: "test-card", MaxCostCents: 1000, AllowedKinds: []string{"new"},
	})
	if reason := service.SalesBlockReason(ctx); reason != "fulfillment_not_wired" {
		t.Fatalf("sales block reason = %q", reason)
	}
	if _, err := service.PurchasableSKURevisions(ctx); !errors.Is(err, ErrQuoteUnavailable) {
		t.Fatalf("browsing reported sellable terms: %v", err)
	}
	if _, err := service.PurchaseTerms(ctx, "plus", "PH", 1); !errors.Is(err, ErrQuoteUnavailable) {
		t.Fatalf("checkout reported sellable terms: %v", err)
	}
}
