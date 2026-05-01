package aiclass

import (
	"strings"
	"testing"
)

// TestSelectInvoicePrompt_DefaultsToGeneric confirms vendors without an
// override get the standard prompt — the safe fall-through behavior.
func TestSelectInvoicePrompt_DefaultsToGeneric(t *testing.T) {
	cases := []string{
		"",
		"Some Random Vendor LLC",
		"Acme Industrial",
		"Unknown",
	}
	for _, v := range cases {
		got := selectInvoicePrompt(v)
		if got != genericInvoicePrompt {
			t.Errorf("selectInvoicePrompt(%q): expected genericInvoicePrompt, got specialized variant", v)
		}
	}
}

// TestSelectInvoicePrompt_SampleHVAC confirms the registered
// vendor-specific prompt fires on resolved-vendor variations. Substring
// match is case-insensitive so "Sample-HVAC - Units" and
// "Sample-HVAC Products" both hit the same entry.
func TestSelectInvoicePrompt_SampleHVAC(t *testing.T) {
	cases := []string{
		"Sample-HVAC - Units",
		"Sample-HVAC Products",
		"sample-hvac",
		"SAMPLE-HVAC",
	}
	for _, v := range cases {
		got := selectInvoicePrompt(v)
		if got == genericInvoicePrompt {
			t.Errorf("selectInvoicePrompt(%q): expected vendor-specific prompt, got generic", v)
		}
		if !strings.Contains(got, "Sample-HVAC / Subsidiary / HVAC-Vendor") {
			t.Errorf("selectInvoicePrompt(%q): vendor-specific prompt missing expected marker", v)
		}
	}
}

// TestSelectInvoicePrompt_NoCrossMatch protects against substring false
// positives in the vendor key set. "International Tools Inc" must not
// match the "sample-hvac" key just because both share the word
// "international."
func TestSelectInvoicePrompt_NoCrossMatch(t *testing.T) {
	cases := []string{
		"International Tools Inc",
		"International Paper",
		"Comfort Industries Inc", // "comfort" alone shouldn't match "sample-hvac"
	}
	for _, v := range cases {
		got := selectInvoicePrompt(v)
		if got != genericInvoicePrompt {
			t.Errorf("selectInvoicePrompt(%q): substring false positive — got vendor-specific prompt", v)
		}
	}
}

// TestGenericInvoicePrompt_HasTotalGuidance pins the strong total-finding
// rules in the generic prompt. The prompt was previously barebones
// ("<number or null>") which caused most vision extractions to come back
// with invoice_total=0; this test guards against accidental regression.
func TestGenericInvoicePrompt_HasTotalGuidance(t *testing.T) {
	requiredFragments := []string{
		"Total Due",
		"Amount Due",
		"Net Amount",
		"Grand Total",
		"NEVER 0 unless invoice genuinely has zero total",
		"Never leave this as 0 when there are line items",
	}
	for _, frag := range requiredFragments {
		if !strings.Contains(genericInvoicePrompt, frag) {
			t.Errorf("genericInvoicePrompt missing required total-finding fragment: %q", frag)
		}
	}
}
