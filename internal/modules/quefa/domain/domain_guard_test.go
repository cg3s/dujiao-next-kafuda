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
