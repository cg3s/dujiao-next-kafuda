package domain

import (
	"testing"
)

func TestValidatePaymentEntryURL(t *testing.T) {
	allowed := []string{"pay.myshop.com", "*.alipay.com"}

	valid := []string{
		"https://pay.myshop.com/cashier/123",
		"https://qr.alipay.com/bax123",
	}
	for _, u := range valid {
		if err := ValidatePaymentEntryURL(u, allowed); err != nil {
			t.Fatalf("expected valid payment url %q, got %v", u, err)
		}
	}

	forbidden := []string{
		"https://tibo.ink/pay/123",
		"https://pay.tibo.ink/pay/123",
		"https://quefa.example.com/pay/123",
	}
	for _, u := range forbidden {
		if err := ValidatePaymentEntryURL(u, append(allowed, "tibo.ink", "*.tibo.ink", "*.example.com")); err != ErrPaymentDomainForbidden {
			t.Fatalf("expected ErrPaymentDomainForbidden for %q, got %v", u, err)
		}
	}

	if err := ValidatePaymentEntryURL("https://other.com/pay", allowed); err != ErrPaymentDomainNotAllowed {
		t.Fatalf("expected ErrPaymentDomainNotAllowed, got %v", err)
	}
	if err := ValidatePaymentEntryURL("http://pay.myshop.com/pay", allowed); err != ErrPaymentURLInvalid {
		t.Fatalf("expected ErrPaymentURLInvalid for http, got %v", err)
	}
}

func TestValidatePaymentQRCode(t *testing.T) {
	allowed := []string{"qr.alipay.com", "pay.myshop.com"}

	if err := ValidatePaymentQRCode("weixin://wxpay/bizpayurl?pr=abc123", allowed); err != nil {
		t.Fatalf("expected native weixin qr valid, got %v", err)
	}
	if err := ValidatePaymentQRCode("https://qr.alipay.com/bax123", allowed); err != nil {
		t.Fatalf("expected allowed https qr valid, got %v", err)
	}
	if err := ValidatePaymentQRCode("https://tibo.ink/qr/123", allowed); err != ErrPaymentDomainForbidden {
		t.Fatalf("expected ErrPaymentDomainForbidden for tibo.ink qr, got %v", err)
	}
	if err := ValidatePaymentQRCode("weixin://wxpay/bizpayurl?cb=https://quefa.io", allowed); err != ErrPaymentDomainForbidden {
		t.Fatalf("expected ErrPaymentDomainForbidden for embedded quefa qr, got %v", err)
	}
}

func TestValidateAllowedPayDomainsRejectsQuefa(t *testing.T) {
	if _, err := ValidateAllowedPayDomains([]string{"tibo.ink"}); err != ErrPaymentDomainForbidden {
		t.Fatalf("expected ErrPaymentDomainForbidden for tibo.ink, got %v", err)
	}
	if _, err := ValidateAllowedPayDomains([]string{"*.quefa-pay.com"}); err != ErrPaymentDomainForbidden {
		t.Fatalf("expected ErrPaymentDomainForbidden for quefa wildcard, got %v", err)
	}
	got, err := ValidateAllowedPayDomains([]string{"Pay.MyShop.com", "*.alipay.com", "pay.myshop.com"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 || got[0] != "pay.myshop.com" || got[1] != "*.alipay.com" {
		t.Fatalf("unexpected normalized domains: %#v", got)
	}
}

func TestParseCatalogPayload(t *testing.T) {
	raw := []byte(`{"products":[{"product_code":"chatgpt_plus_1m","title":"ChatGPT Plus 1M","supply_price":"110.50","min_sale_price":"120.00","currency":"CNY","collection_modes":["platform_collect","agent_collect"],"delivery_modes":["auto_recharge"],"available":true}]}`)
	products, err := ParseCatalogPayload(raw)
	if err != nil {
		t.Fatalf("ParseCatalogPayload failed: %v", err)
	}
	if len(products) != 1 {
		t.Fatalf("want 1 product, got %d", len(products))
	}
	if products[0].EffectiveSupplyFloor().StringFixed(2) != "120.00" {
		t.Fatalf("want floor 120.00, got %s", products[0].EffectiveSupplyFloor().StringFixed(2))
	}
	if !products[0].SupportsMode(CollectionModePlatformCollect, DeliveryModeAutoRecharge) {
		t.Fatalf("expected mode support")
	}
}

func TestParseCatalogPayloadVariants(t *testing.T) {
	raw := []byte(`{
		"data": {
			"items": [
				{
					"code": "gpt_plus_1m",
					"name": "Plus 1M",
					"pricing": {"supply_price_cents": 11500, "min_sale_price_cents": 12500, "currency": "CNY"},
					"supports_platform_collect": true,
					"supports_auto_recharge": true,
					"status": "published"
				},
				{
					"product_code": "paused_sku",
					"title": "Paused",
					"supply_price": null,
					"status": "disabled"
				}
			]
		}
	}`)
	products, err := ParseCatalogPayload(raw)
	if err != nil {
		t.Fatalf("ParseCatalogPayload variants failed: %v", err)
	}
	if len(products) != 2 {
		t.Fatalf("want 2 products, got %d", len(products))
	}
	if products[0].ProductCode != "gpt_plus_1m" || products[0].SupplyPrice.StringFixed(2) != "115.00" || products[0].MinSalePrice.StringFixed(2) != "125.00" {
		t.Fatalf("unexpected parsed product[0]: %+v", products[0])
	}
	if !products[0].SupportsMode(CollectionModePlatformCollect, DeliveryModeAutoRecharge) {
		t.Fatalf("expected inferred mode support on product[0]")
	}
	if products[1].Available {
		t.Fatalf("expected paused_sku to be unavailable")
	}
	shape := DescribePayloadShape([]byte(`{"ok":true,"catalog":[{"id":"x"}]}`))
	if shape == "" || shape == "empty" || shape == "non-json" {
		t.Fatalf("unexpected shape summary: %q", shape)
	}

	// Official OpenAPI 1.4.3 GET /v1/products payload including quote_required=true with 0.00 placeholder price.
	official := []byte(`{
		"data": [
			{
				"product_code": "chatgpt_plus_cdk_1m",
				"name": "ChatGPT Plus 月卡",
				"supply_price": "110.00",
				"max_sale_price": "159.00",
				"currency": "CNY",
				"max_quantity": 1,
				"available": true,
				"fulfillment_mode": "cdk",
				"quote_required": false,
				"delivery_modes": ["auto_recharge", "cdk"],
				"payment_country": "PH",
				"payment_currency": "PHP"
			},
			{
				"product_code": "chatgpt_plus_us_direct_1m",
				"name": "ChatGPT Plus US 直充",
				"supply_price": "0.00",
				"max_sale_price": "0.00",
				"currency": "CNY",
				"max_quantity": 1,
				"available": true,
				"fulfillment_mode": "direct",
				"quote_required": true,
				"delivery_modes": ["auto_recharge"],
				"payment_country": "US",
				"payment_currency": "USD"
			}
		]
	}`)
	officialProducts, err := ParseCatalogPayload(official)
	if err != nil {
		t.Fatalf("official OpenAPI catalog parse failed: %v", err)
	}
	if len(officialProducts) != 2 {
		t.Fatalf("want 2 products, got %d", len(officialProducts))
	}
	if !officialProducts[0].SupportsMode(CollectionModePlatformCollect, DeliveryModeAutoRecharge) {
		t.Fatalf("expected fixed-price product to support platform_collect + auto_recharge")
	}
	if !officialProducts[1].QuoteRequired || officialProducts[1].SupportsMode(CollectionModePlatformCollect, DeliveryModeAutoRecharge) {
		t.Fatalf("expected quote_required=true product to parse cleanly but remain blocked from fixed-price checkout: %+v", officialProducts[1])
	}
}
