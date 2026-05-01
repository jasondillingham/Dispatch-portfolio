package vendors

import "testing"

// TestBrandMatch_Acme documents the canonical brand-match case: the domain
// is ambiguous (multiple vendor sub-accounts share it) so direct domain
// lookup returns nothing, but the sender is unmistakably "from Globex."
// Brand match surfaces that without faking a specific VendorID.
func TestBrandMatch_Acme(t *testing.T) {
	r := &Resolver{
		byEmail:     map[string]Vendor{},
		byDomain:    map[string]Vendor{},
		byNameToken: map[string]Vendor{},
		ambiguousDoms: map[string]int{
			"globex.test": 4,
		},
		ambiguousDomVendors: map[string][]Vendor{
			"globex.test": {
				{VendorID: "100179", VendorName: "GLOBEX B-Line Systems"},
				{VendorID: "100397", VendorName: "Globex Lighting"},
				{VendorID: "100426", VendorName: "Subsidiary C"},
				{VendorID: "117816", VendorName: "Globex Industrial Lighting"},
			},
		},
	}
	got := r.Resolve("vendor.contact@globex.test")
	if got.Type != MatchBrand {
		t.Fatalf("expected MatchBrand, got %s", got.Type)
	}
	if got.Vendor.VendorName != "Globex" {
		t.Errorf("expected VendorName=\"Acme\", got %q", got.Vendor.VendorName)
	}
	if got.Vendor.VendorID != "" {
		t.Errorf("expected VendorID empty for brand match, got %q", got.Vendor.VendorID)
	}
}

// TestBrandMatch_GenericIgnored confirms we don't blindly emit a brand for
// any ambiguous domain — only when the label actually appears in a
// candidate's name. A free-mail-style domain is ambiguous but no real
// vendor name would contain its label.
func TestBrandMatch_GenericIgnored(t *testing.T) {
	r := &Resolver{
		byEmail:     map[string]Vendor{},
		byDomain:    map[string]Vendor{},
		byNameToken: map[string]Vendor{},
		ambiguousDoms: map[string]int{
			"webmail.test": 7,
		},
		ambiguousDomVendors: map[string][]Vendor{
			"webmail.test": {
				{VendorID: "1", VendorName: "Globex Lighting"},
				{VendorID: "2", VendorName: "Beta Supply"},
			},
		},
	}
	got := r.Resolve("someone@webmail.test")
	if got.Type != MatchUnknown {
		t.Fatalf("expected MatchUnknown for free-mail with no name overlap, got %s", got.Type)
	}
}

// TestBrandMatch_StemmedDomain confirms the stripped-prefix fallback also
// reaches the brand path: notification.globex.test → globex.test → "Globex".
func TestBrandMatch_StemmedDomain(t *testing.T) {
	r := &Resolver{
		byEmail:     map[string]Vendor{},
		byDomain:    map[string]Vendor{},
		byNameToken: map[string]Vendor{},
		ambiguousDoms: map[string]int{
			"globex.test": 2,
		},
		ambiguousDomVendors: map[string][]Vendor{
			"globex.test": {
				{VendorID: "1", VendorName: "Globex Industrial"},
				{VendorID: "2", VendorName: "Globex Lighting"},
			},
		},
	}
	got := r.Resolve("noreply@notification.globex.test")
	if got.Type != MatchBrand {
		t.Fatalf("expected MatchBrand via stem, got %s", got.Type)
	}
	if got.Vendor.VendorName != "Globex" {
		t.Errorf("expected VendorName=\"Acme\", got %q", got.Vendor.VendorName)
	}
}

// TestBrandMatch_DoesNotShadowExact confirms a known sender still resolves
// exactly even when the domain is ambiguous.
func TestBrandMatch_DoesNotShadowExact(t *testing.T) {
	r := &Resolver{
		byEmail: map[string]Vendor{
			"specific@globex.test": {VendorID: "100397", VendorName: "Globex Lighting"},
		},
		byDomain:    map[string]Vendor{},
		byNameToken: map[string]Vendor{},
		ambiguousDoms: map[string]int{
			"globex.test": 4,
		},
		ambiguousDomVendors: map[string][]Vendor{
			"globex.test": {
				{VendorID: "100397", VendorName: "Globex Lighting"},
				{VendorID: "117816", VendorName: "Globex Industrial Lighting"},
			},
		},
	}
	got := r.Resolve("specific@globex.test")
	if got.Type != MatchExact {
		t.Fatalf("expected MatchExact (known sender), got %s", got.Type)
	}
	if got.Vendor.VendorID != "100397" {
		t.Errorf("expected vendor 100397, got %q", got.Vendor.VendorID)
	}
}

// TestBrandMatch_NoSubstringFalsePositive guards against the substring-match
// bug: domain "globex.test" must not brand-match a vendor "Maglobex Inc"
// even though "globex" is a substring of "macme". The fix is to match against
// whole word tokens, not the raw normalized name.
func TestBrandMatch_NoSubstringFalsePositive(t *testing.T) {
	r := &Resolver{
		byEmail:     map[string]Vendor{},
		byDomain:    map[string]Vendor{},
		byNameToken: map[string]Vendor{},
		ambiguousDoms: map[string]int{
			"globex.test": 2,
		},
		ambiguousDomVendors: map[string][]Vendor{
			// Neither candidate has "Globex" as a standalone word — only as a
			// substring inside another word. Brand match should NOT fire.
			"globex.test": {
				{VendorID: "1", VendorName: "Maglobex Inc"},
				{VendorID: "2", VendorName: "Globexology LLC"},
			},
		},
	}
	got := r.Resolve("someone@globex.test")
	if got.Type != MatchUnknown {
		t.Fatalf("expected MatchUnknown when label is only a substring, got %s (vendor %q)", got.Type, got.Vendor.VendorName)
	}
}

// TestBrandMatch_RealWordMatch confirms a vendor with the label as a real
// word token still matches. Companion to the substring-false-positive test.
func TestBrandMatch_RealWordMatch(t *testing.T) {
	r := &Resolver{
		byEmail:     map[string]Vendor{},
		byDomain:    map[string]Vendor{},
		byNameToken: map[string]Vendor{},
		ambiguousDoms: map[string]int{
			"globex.test": 2,
		},
		ambiguousDomVendors: map[string][]Vendor{
			"globex.test": {
				{VendorID: "1", VendorName: "Maglobex Inc"},        // substring; should NOT trigger
				{VendorID: "2", VendorName: "Globex Industrial"}, // word match; SHOULD trigger
			},
		},
	}
	got := r.Resolve("someone@globex.test")
	if got.Type != MatchBrand {
		t.Fatalf("expected MatchBrand when at least one candidate has the label as a word, got %s", got.Type)
	}
	if got.Vendor.VendorName != "Globex" {
		t.Errorf("expected VendorName=\"Acme\", got %q", got.Vendor.VendorName)
	}
}
