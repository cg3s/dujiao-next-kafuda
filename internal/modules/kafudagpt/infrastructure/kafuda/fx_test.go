package kafuda

import (
	"math"
	"testing"
)

func TestParseFXCNYResponse(t *testing.T) {
	body := []byte(`{"result":"success","base_code":"CNY","time_last_update_unix":1700000000,"rates":{"CNY":1,"PHP":8.0,"JPY":20.0,"BAD":0}}`)
	rates, updatedAt, err := parseFXCNYResponse(body)
	if err != nil {
		t.Fatalf("parseFXCNYResponse() error = %v", err)
	}
	if updatedAt.Unix() != 1700000000 {
		t.Fatalf("updatedAt = %d, want 1700000000", updatedAt.Unix())
	}
	if math.Abs(rates["PHP"]-0.125) > 1e-9 {
		t.Fatalf("rates[PHP] = %v, want 0.125", rates["PHP"])
	}
	if math.Abs(rates["JPY"]-0.05) > 1e-9 {
		t.Fatalf("rates[JPY] = %v, want 0.05", rates["JPY"])
	}
	if rates["CNY"] != 1.0 {
		t.Fatalf("rates[CNY] = %v, want 1", rates["CNY"])
	}
	if _, ok := rates["BAD"]; ok {
		t.Fatalf("expected non-positive rate to be ignored")
	}

	if _, _, err := parseFXCNYResponse([]byte(`{"result":"error","base_code":"CNY","rates":{}}`)); err == nil {
		t.Fatal("expected error for non-success FX payload")
	}
}
