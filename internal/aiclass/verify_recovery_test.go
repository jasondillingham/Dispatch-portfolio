package aiclass

import (
	"encoding/json"
	"testing"
)

// TestVerifyStatusUnmarshal_Number documents the Signamax case: model emits
// "status": 1 instead of "status": "match" or similar. The custom unmarshal
// coerces numbers to empty string so the parent struct still parses.
func TestVerifyStatusUnmarshal_Number(t *testing.T) {
	cases := []struct {
		raw  string
		want VerifyStatus
	}{
		{`"match"`, "match"},
		{`"differs"`, "differs"},
		{`"not_found"`, "not_found"},
		{`""`, ""},
		{`null`, ""},
		{`1`, ""},
		{`0`, ""},
		{`true`, ""},
		{`false`, ""},
	}
	for _, c := range cases {
		var s VerifyStatus
		if err := json.Unmarshal([]byte(c.raw), &s); err != nil {
			t.Errorf("raw=%s: unexpected error: %v", c.raw, err)
			continue
		}
		if s != c.want {
			t.Errorf("raw=%s: want %q, got %q", c.raw, c.want, s)
		}
	}
}

// TestVerifyResult_StatusAsNumber documents the actual Signamax output: the
// second line item has "status": 1. Without the custom unmarshal the whole
// parse fails. With it, the line lands with empty status and recon flags it.
func TestVerifyResult_StatusAsNumber(t *testing.T) {
	raw := `{
	  "invoice_number": "917199089",
	  "invoice_date": "2026-04-04",
	  "invoice_total_observed": 151.46,
	  "lines": [
	    {"line_no": 1, "status": "match", "observed_qty": 1, "observed_unit_price": 119.1, "observed_extended": 119.1},
	    {"line_no": 2, "status": 1, "observed_qty": 1, "observed_unit_price": 119.1, "observed_extended": 119.1}
	  ]
	}`
	var r VerifyResult
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if len(r.Lines) != 2 {
		t.Fatalf("expected 2 lines, got %d", len(r.Lines))
	}
	if r.Lines[0].Status != "match" {
		t.Errorf("line 0: want 'match', got %q", r.Lines[0].Status)
	}
	if r.Lines[1].Status != "" {
		t.Errorf("line 1: want '' (coerced from numeric), got %q", r.Lines[1].Status)
	}
	// Comparison to untyped string constants still works (callsites do
	// `lr.Status == "match"`).
	if r.Lines[0].Status != "match" {
		t.Error("equality comparison to string literal failed")
	}
}

// TestKeepFirstTopLevelKeys_Sunbelt documents the model-repeated-itself case:
// "lines" appears three times. Standard json.Unmarshal would use the LAST
// occurrence (the broken truncated string); we want the FIRST (the actual
// array of line results).
func TestKeepFirstTopLevelKeys_Sunbelt(t *testing.T) {
	// Note: the third "lines" in the real bug was truncated mid-string. For
	// this test we use a closed-string version so JSON parses (the repair
	// step would close it in production); the dedupe semantics are the same
	// either way.
	raw := `{"invoice_number": "71018526-01", "invoice_total_observed": 3659.04, "lines": [{"line_no": 1, "status": "not_found"}], "lines": [{"line_no": 2, "status": "not_found"}], "lines": "fallback string"}`
	out, ok := keepFirstTopLevelKeys(raw)
	if !ok {
		t.Fatal("expected dedupe to fire")
	}
	var r VerifyResult
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("parse after dedupe failed: %v\noutput: %s", err, out)
	}
	if len(r.Lines) != 1 {
		t.Fatalf("expected 1 line (first occurrence), got %d", len(r.Lines))
	}
	if r.Lines[0].LineNo != 1 {
		t.Errorf("expected LineNo=1 from first 'lines' entry, got %d", r.Lines[0].LineNo)
	}
}

// TestKeepFirstTopLevelKeys_Idempotent confirms a clean object with no
// duplicates is left alone (returns false so we don't churn the string).
func TestKeepFirstTopLevelKeys_Idempotent(t *testing.T) {
	raw := `{"invoice_number": "1", "lines": [{"line_no": 1, "status": "match"}]}`
	_, ok := keepFirstTopLevelKeys(raw)
	if ok {
		t.Error("expected dedupe to be no-op on clean input")
	}
}

// TestKeepFirstTopLevelKeys_NotAnObject confirms we don't crash on inputs
// that aren't a JSON object (arrays, scalars, malformed).
func TestKeepFirstTopLevelKeys_NotAnObject(t *testing.T) {
	for _, raw := range []string{`[1,2,3]`, `"just a string"`, `null`, ``, `not json at all`} {
		_, ok := keepFirstTopLevelKeys(raw)
		if ok {
			t.Errorf("expected false for non-object input %q", raw)
		}
	}
}
