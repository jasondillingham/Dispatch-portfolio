package aiclass

import (
	"encoding/json"
	"testing"
)

// TestStripCurrency_NewUnits documents the unit suffixes added to handle
// real-world vendor invoices that were previously crashing extraction.
// Each case is a real example pulled from a failed extraction in cache.
func TestStripCurrency_NewUnits(t *testing.T) {
	cases := []struct {
		in   string
		want string
		desc string
	}{
		// Real Resideo case — cable per thousand feet.
		{"1940.00000 MFT", "1940.00000", "MFT (thousand feet, cable)"},
		{"1.50 MFT", "1.50", "MFT lowercase length"},
		// Linear feet variants.
		{"12.50 LF", "12.50", "LF (linear feet)"},
		{"3.25 lin ft", "3.25", "lin ft variant"},
		{"100 ft", "100", "ft (feet)"},
		// Weight.
		{"45.5 LB", "45.5", "LB"},
		{"45.5 lbs", "45.5", "lbs plural"},
		{"2.5 KG", "2.5", "KG"},
		// Volume.
		{"5 GAL", "5", "GAL"},
		{"32 oz", "32", "oz"},
		// Counts / packaging.
		{"100 ROLL", "100", "ROLL"},
		{"50 doz", "50", "doz"},
		{"1 case", "1", "case"},
		{"3 sets", "3", "sets"},
		{"6 bundle", "6", "bundle"},
		// Time/labor.
		{"40 hr", "40", "hr"},
		{"2 day", "2", "day"},
		// Compound: currency + unit.
		{"1.50 USD ROLL", "1.50", "USD then ROLL"},
		{"$25.00 each", "25.00", "$ + each"},
		// Percentages.
		{"15%", "15", "percent"},
		// Existing behavior we don't want to break.
		{"$1,234.56", "1234.56", "currency + commas"},
		{"25.00 U.S.D.", "25.00", "u.s.d. with dots"},
		{"1 EA", "1", "ea (existing)"},
		// Things that should stay non-numeric.
		{"THA", "THA", "model hallucination — no number to extract"},
		{"N/A", "N/A", "literal N/A"},
		{"abc def", "abc def", "no numeric core"},
	}
	for _, c := range cases {
		got := stripCurrency(c.in)
		if got != c.want {
			t.Errorf("%s: stripCurrency(%q) = %q, want %q", c.desc, c.in, got, c.want)
		}
	}
}

// TestCoerceStringNums_NullFallback documents the safety-net behavior:
// when a numeric field has a non-numeric string after currency-stripping,
// we emit JSON null (which unmarshals to zero) instead of leaving the
// original string (which crashes unmarshal). The line stays in the
// extraction; recon flags it as a discrepancy because zero won't match.
func TestCoerceStringNums_NullFallback(t *testing.T) {
	// Power King-style: model emitted "THA" for unit_price.
	in := `{"item_id": "AX35", "qty": 5, "unit_price": "THA", "extended": 5}`
	out := coerceStringNums(in)

	// Should now parse cleanly into InvoiceLine.
	var line InvoiceLine
	if err := json.Unmarshal([]byte(out), &line); err != nil {
		t.Fatalf("expected coerced JSON to unmarshal cleanly, got: %v\noutput: %s", err, out)
	}
	if line.ItemID != "AX35" {
		t.Errorf("ItemID lost: got %q", line.ItemID)
	}
	if line.Qty != 5 {
		t.Errorf("Qty lost: got %v", line.Qty)
	}
	if line.UnitPrice != 0 {
		t.Errorf("UnitPrice should fall back to 0, got %v", line.UnitPrice)
	}
}

// TestCoerceStringNums_MFTRecovery documents the Resideo case end-to-end:
// a cable line with "1940.00000 MFT" as unit_price now parses successfully
// with the value 1940 (per-MFT pricing — recon will reconcile correctly
// because the ERP PO lines also store per-MFT for cable items).
func TestCoerceStringNums_MFTRecovery(t *testing.T) {
	in := `{"item_id": "400-01XHHW-AL-BN", "qty": 100, "unit_price": "1940.00000 MFT", "extended": 19400}`
	out := coerceStringNums(in)

	var line InvoiceLine
	if err := json.Unmarshal([]byte(out), &line); err != nil {
		t.Fatalf("expected MFT value to coerce + unmarshal, got: %v\noutput: %s", err, out)
	}
	if line.UnitPrice != 1940 {
		t.Errorf("UnitPrice should be 1940 (MFT stripped), got %v", line.UnitPrice)
	}
	if line.Qty != 100 {
		t.Errorf("Qty: got %v", line.Qty)
	}
	if line.Extended != 19400 {
		t.Errorf("Extended: got %v", line.Extended)
	}
}

// TestCoerceStringNums_PreservesCleanInput confirms the coercion is a
// no-op for already-numeric JSON values (the common case).
func TestCoerceStringNums_PreservesCleanInput(t *testing.T) {
	in := `{"item_id": "X1", "qty": 5, "unit_price": 12.5, "extended": 62.5, "description": "Widget"}`
	out := coerceStringNums(in)
	var line InvoiceLine
	if err := json.Unmarshal([]byte(out), &line); err != nil {
		t.Fatalf("expected clean input to round-trip, got: %v", err)
	}
	if line.UnitPrice != 12.5 || line.Qty != 5 || line.Extended != 62.5 {
		t.Errorf("unexpected drift: %+v", line)
	}
}
