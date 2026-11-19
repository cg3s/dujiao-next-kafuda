package application

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
)

func TestCatalogUsesPerPlanRegionsAndKeepsUnknownPairsUnavailable(t *testing.T) {
	now := time.Now().UTC()
	data := json.RawMessage(`{
		"enabled":true,
		"plans":[
			{"plan":"plus","label":"Plus","credit":false,"regions":[{"country":"PH","currency":"PHP","estimated_plan_usd_cents":1537,"estimated_local_minor":98214}]},
			{"plan":"credit250","label":"Codex 250","credit":true,"regions":[{"country":"PH","currency":"PHP","estimated_plan_usd_cents":800,"estimated_local_minor":50000},{"country":"CL","currency":"CLP","estimated_plan_usd_cents":null,"estimated_local_minor":null}]}
		],
		"regions":[{"country":"PH"},{"country":"CL"},{"country":"US"}],
		"auto_card_products":[{"product_code":"P5378XB","name":"test","restriction_reason":null,"gpt_recommended":true,"open_fee_cents":100,"min_amount_cents":2000},{"product_code":"restricted","name":"test","restriction_reason":"not supported","gpt_recommended":false,"open_fee_cents":100,"min_amount_cents":2000},{"product_code":"explicit-empty","restriction_reason":""}]
	}`)
	got, err := ParseCatalog(data, "default", now, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Options) != 3 || !got.Available("plus", "PH", now) || got.Available("plus", "CL", now) || got.Available("credit250", "CL", now) || got.Available("plus", "US", now) {
		t.Fatalf("catalog incorrectly expanded or enabled regions: %+v", got)
	}
	if got.Options[0].Product != "gpt" || got.Options[1].Product != "gpt_credits" || len(got.Cards) != 3 ||
		got.Cards[0].Restricted || !got.Cards[1].Restricted || got.Cards[2].Restricted {
		t.Fatalf("lost product or card restriction: %+v", got)
	}
}

func TestInvalidCatalogCannotBeMistakenForConfirmedRemoval(t *testing.T) {
	now := time.Now().UTC()
	for _, data := range []string{
		`{}`, `null`, `{"enabled":true,"plans":null,"auto_card_products":[]}`,
		`{"enabled":true,"plans":[],"auto_card_products":null}`,
		`{"enabled":true,"plans":[{"plan":"plus","label":"Plus","credit":false,"regions":[{"country":"PH","currency":"PHP","estimated_plan_usd_cents":-1}]}],"auto_card_products":[]}`,
		`{"enabled":true,"plans":[{"plan":"plus","label":"Plus","credit":false,"regions":[{"country":"PH","currency":"PHP"},{"country":"PH","currency":"PHP"}]}],"auto_card_products":[]}`,
		`{"enabled":true,"plans":[{"plan":"credit250","label":"Codex 250","regions":[]}],"auto_card_products":[]}`,
		`{"enabled":true,"plans":[{"plan":"credit250","label":"Codex 250","credit":null,"regions":[]}],"auto_card_products":[]}`,
		`{"enabled":true,"plans":[],"auto_card_products":[{"product_code":"restricted"}]}`,
		`{"enabled":true,"plans":[],"auto_card_products":[{"product_code":"restricted","restriction_reason":false}]}`,
	} {
		if _, err := ParseCatalog(json.RawMessage(data), "default", now, now.Add(time.Hour)); !errors.Is(err, kafudadomain.ErrInvalidCatalog) {
			t.Fatalf("invalid catalog accepted: %s, %v", data, err)
		}
	}
	got, err := ParseCatalog(json.RawMessage(`{"enabled":false,"plans":[],"auto_card_products":[]}`), "default", now, now.Add(time.Hour))
	if err != nil || got.Enabled || len(got.Options) != 0 {
		t.Fatalf("confirmed disabled catalog rejected: %+v, %v", got, err)
	}
}
