package contract

import (
	"strings"
	"testing"
)

func TestSKUCodeDefaultFormat(t *testing.T) {
	if got := SKUCode("plus", "US"); got != "GPT-PLUS-US" {
		t.Fatalf("SKUCode(plus, US) = %q, want GPT-PLUS-US", got)
	}
	if got := SKUCode("pro_monthly", "ph"); got != "GPT-PRO-MONTHLY-PH" {
		t.Fatalf("SKUCode(pro_monthly, ph) = %q, want GPT-PRO-MONTHLY-PH", got)
	}
	longPlan := strings.Repeat("plan", 20)
	gotLong := SKUCode(longPlan, "US")
	if len(gotLong) > 64 || !strings.HasPrefix(gotLong, "KGPT_") {
		t.Fatalf("SKUCode(longPlan, US) = %q (len=%d), want KGPT_ hash <= 64 chars", gotLong, len(gotLong))
	}
}
