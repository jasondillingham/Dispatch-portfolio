// Package vendors resolves a sender email or domain to a the ERP vendor.
//
// Matching order:
//  1. Exact email match (from vendor_emails.json)
//  2. Unambiguous domain match (domain maps to exactly one vendor)
//  3. Unknown
//
// Ambiguous domains (freemail.example.com, freemail.example.com, sales-rep agencies like agency.example.com
// that represent many vendor lines) deliberately fall through to Unknown
// rather than guess a wrong vendor.
package vendors

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

type MatchType string

const (
	MatchExact   MatchType = "exact"
	MatchDomain  MatchType = "domain"
	MatchStem    MatchType = "stem"  // domain matched after stripping a mail-service subdomain prefix
	MatchName    MatchType = "name"  // domain label matched a vendor name token
	MatchBrand   MatchType = "brand" // ambiguous domain whose label appears in N>1 candidate vendor names — emits the brand name with no specific VendorID
	MatchUnknown MatchType = "unknown"
)

type Vendor struct {
	VendorID   string
	VendorName string
}

type Match struct {
	Type   MatchType
	Vendor Vendor // zero value if Unknown
}

type Resolver struct {
	byEmail              map[string]Vendor
	byDomain             map[string]Vendor   // only populated for unambiguous domains
	byNameToken          map[string]Vendor   // normalized vendor-name tokens → vendor, unambiguous only
	ambiguousDoms        map[string]int      // domain → vendor count, for reporting
	ambiguousDomVendors  map[string][]Vendor // domain → all candidate vendors (drives the brand-name fallback)
}

// stripPrefixes are common mail-service subdomain prefixes. When an exact
// domain lookup fails, we strip these and retry: notification.example.com
// becomes example.com. Kept small and obvious.
var stripPrefixes = map[string]bool{
	"notification":  true,
	"notifications": true,
	"email":         true,
	"emails":        true,
	"emailalerts":   true,
	"mail":          true,
	"mailer":        true,
	"noreply":       true,
	"no-reply":      true,
	"donotreply":    true,
	"do-not-reply":  true,
	"em":            true,
	"smtp":          true,
	"reply":         true,
	"alert":         true,
	"alerts":        true,
	"delivery":      true,
	"sent":          true,
	"sent-via":      true,
	"sentvia":       true,
}

// nameTokenStopwords are generic terms that appear in many vendor names and
// would generate false-positive matches if used as name tokens. Keep narrow —
// only things that are genuinely meaningless on their own as vendor identifiers.
var nameTokenStopwords = map[string]bool{
	"inc": true, "corp": true, "corporation": true, "company": true, "co": true,
	"llc": true, "ltd": true, "limited": true, "group": true, "the": true,
	"and": true, "of": true, "services": true, "service": true, "supply": true,
	"supplies": true, "equipment": true, "solutions": true, "systems": true,
	"products": true, "industries": true, "international": true, "intl": true,
	"global": true, "usa": true, "us": true, "america": true, "american": true,
	"north": true, "south": true, "east": true, "west": true, "div": true,
	"division": true, "enterprises": true, "holdings": true, "associates": true,
}

// minNameTokenLen is the minimum length for a vendor-name token to be indexed.
// Shorter tokens (3-4 chars) generate too many false-positive domain matches
// (e.g. "rab" could match any domain starting with rab).
const minNameTokenLen = 5

type emailRow struct {
	VendorID   string `json:"vendor_id"`
	VendorName string `json:"vendor_name"`
	Email      string `json:"email"`
	Domain     string `json:"domain"`
	Source     string `json:"source"`
}

type domainRow struct {
	Domain      string `json:"domain"`
	VendorCount int    `json:"vendor_count"`
	EmailCount  int    `json:"email_count"`
	Vendors     []struct {
		VendorID   string `json:"vendor_id"`
		VendorName string `json:"vendor_name"`
	} `json:"vendors"`
}

func Load(emailsPath, domainsPath string) (*Resolver, error) {
	emailsRaw, err := os.ReadFile(emailsPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", emailsPath, err)
	}
	var emails []emailRow
	if err := json.Unmarshal(emailsRaw, &emails); err != nil {
		return nil, fmt.Errorf("parse %s: %w", emailsPath, err)
	}

	domainsRaw, err := os.ReadFile(domainsPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", domainsPath, err)
	}
	var domains []domainRow
	if err := json.Unmarshal(domainsRaw, &domains); err != nil {
		return nil, fmt.Errorf("parse %s: %w", domainsPath, err)
	}

	r := &Resolver{
		byEmail:             make(map[string]Vendor, len(emails)),
		byDomain:            make(map[string]Vendor),
		byNameToken:         make(map[string]Vendor),
		ambiguousDoms:       make(map[string]int),
		ambiguousDomVendors: make(map[string][]Vendor),
	}
	for _, e := range emails {
		r.byEmail[strings.ToLower(e.Email)] = Vendor{VendorID: e.VendorID, VendorName: e.VendorName}
	}
	for _, d := range domains {
		dom := strings.ToLower(d.Domain)
		if d.VendorCount == 1 && len(d.Vendors) == 1 {
			r.byDomain[dom] = Vendor{
				VendorID:   d.Vendors[0].VendorID,
				VendorName: d.Vendors[0].VendorName,
			}
		} else {
			r.ambiguousDoms[dom] = d.VendorCount
			cands := make([]Vendor, 0, len(d.Vendors))
			for _, v := range d.Vendors {
				cands = append(cands, Vendor{VendorID: v.VendorID, VendorName: v.VendorName})
			}
			r.ambiguousDomVendors[dom] = cands
		}
	}

	// Build name-token index. First collect all vendors (dedupe by vendor_id).
	// Walk both emails and domains; each entry may reference the same vendor multiple times.
	vendorSet := map[string]Vendor{}
	for _, e := range emails {
		vendorSet[e.VendorID] = Vendor{VendorID: e.VendorID, VendorName: e.VendorName}
	}
	for _, d := range domains {
		for _, v := range d.Vendors {
			vendorSet[v.VendorID] = Vendor{VendorID: v.VendorID, VendorName: v.VendorName}
		}
	}

	// For each vendor, emit its name tokens. Track ambiguous tokens (appearing
	// for more than one vendor) and drop them.
	candidateTokens := map[string]map[string]bool{} // token → set of vendor IDs
	for _, v := range vendorSet {
		for _, tok := range extractNameTokens(v.VendorName) {
			if _, ok := candidateTokens[tok]; !ok {
				candidateTokens[tok] = map[string]bool{}
			}
			candidateTokens[tok][v.VendorID] = true
		}
	}
	for tok, ids := range candidateTokens {
		if len(ids) != 1 {
			continue // ambiguous — more than one vendor shares this token
		}
		for id := range ids {
			r.byNameToken[tok] = vendorSet[id]
		}
	}

	return r, nil
}

// extractNameTokens returns all meaningful tokens from a vendor name suitable
// for domain-label matching. Includes:
//  - the full normalized name (all alphanumeric, lowercase)
//  - each word-level token (lowercase, stripped of non-alphanumeric)
//  - any parenthetical alias (treated as a full name)
// Filters out stopwords and tokens shorter than minNameTokenLen.
func extractNameTokens(name string) []string {
	out := []string{}
	add := func(s string) {
		s = normalizeName(s)
		if len(s) >= minNameTokenLen && !nameTokenStopwords[s] {
			out = append(out, s)
		}
	}

	// Pull out parenthetical aliases first, and also consider the stripped version
	stripped := name
	paren := ""
	if i := strings.Index(name, "("); i >= 0 {
		if j := strings.Index(name[i:], ")"); j > 0 {
			paren = name[i+1 : i+j]
			stripped = strings.TrimSpace(name[:i] + name[i+j+1:])
		}
	}

	// Whole normalized names
	add(stripped)
	if paren != "" {
		add(paren)
	}

	// Individual word tokens
	for _, w := range strings.Fields(stripped) {
		add(w)
	}
	for _, w := range strings.Fields(paren) {
		add(w)
	}
	return out
}

// normalizeName strips everything except lowercase letters and digits.
// "Sample-Lighting Brand" → "samplelighting"; "Sample-Faucet Brand" → "samplefaucetbrand".
func normalizeName(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (r *Resolver) Resolve(senderEmail string) Match {
	e := strings.ToLower(strings.TrimSpace(senderEmail))
	if e == "" {
		return Match{Type: MatchUnknown}
	}
	if v, ok := r.byEmail[e]; ok {
		return Match{Type: MatchExact, Vendor: v}
	}
	at := strings.LastIndex(e, "@")
	if at < 0 || at == len(e)-1 {
		return Match{Type: MatchUnknown}
	}
	domain := e[at+1:]
	if v, ok := r.byDomain[domain]; ok {
		return Match{Type: MatchDomain, Vendor: v}
	}

	// Fallback 1: strip common mail-service subdomain prefixes and retry the
	// domain map. notification.example.com → example.com.
	if stemmed, ok := stripMailPrefix(domain); ok {
		if v, ok := r.byDomain[stemmed]; ok {
			return Match{Type: MatchStem, Vendor: v}
		}
	}

	// Fallback 2: match the domain's second-level label against vendor name tokens.
	// Catches direct-from-vendor mail when our the ERP data only knows their rep
	// agency's email (e.g. sample-lighting.example.com → "Sample-Lighting Brand" vendor, even though
	// the ERP's AP contact for them is marketing.example.com).
	label := domainLabel(domain)
	if label != "" && !nameTokenStopwords[label] {
		if v, ok := r.byNameToken[label]; ok {
			return Match{Type: MatchName, Vendor: v}
		}
	}

	// Fallback 3: brand match. Some big vendors have one corporate domain
	// (a single corporate domain) but many the ERP sub-accounts (Subsidiary A, Subsidiary
	// Lighting, Subsidiary C — all "@vendor-a.example.com" senders). We can't guess the
	// sub-account from sender alone, but we can confidently say the email is
	// from "Globex." Surface that as the brand so the row reads "Vendor: Globex"
	// instead of "Vendor: Unknown" — a real PO in the message will override
	// to the specific sub-vendor downstream via PO lookup.
	//
	// Only emit when the domain label appears in at least one candidate's
	// normalized name (so vendor-a.example.com → "Globex" works, but freemail.example.com → no
	// match because no vendor name contains "freemail").
	if v, ok := r.brandMatch(domain); ok {
		return Match{Type: MatchBrand, Vendor: v}
	}
	if stemmed, ok := stripMailPrefix(domain); ok {
		if v, ok := r.brandMatch(stemmed); ok {
			return Match{Type: MatchBrand, Vendor: v}
		}
	}

	return Match{Type: MatchUnknown}
}

// brandMatch returns a synthesized brand vendor if the domain is ambiguous AND
// its label appears as a whole word in at least one candidate's name. The
// returned Vendor has VendorID="" — downstream code that needs a specific ERP
// vendor (voucher lookup, PO match) should treat MatchBrand as "vendor known,
// sub-account unknown" and rely on the PO override or manual entry to
// disambiguate.
//
// We compare against word-tokenized vendor names rather than the full
// normalized string to avoid substring false positives — e.g. domain
// "vendor-a.example.com" should NOT brand-match a vendor "Maglobex Inc" even though
// "globex" is a substring of "maglobex".
func (r *Resolver) brandMatch(domain string) (Vendor, bool) {
	cands, ok := r.ambiguousDomVendors[domain]
	if !ok || len(cands) == 0 {
		return Vendor{}, false
	}
	label := domainLabel(domain)
	if label == "" || nameTokenStopwords[label] || len(label) < minNameTokenLen {
		return Vendor{}, false
	}
	for _, c := range cands {
		for _, tok := range strings.Fields(c.VendorName) {
			if normalizeName(tok) == label {
				return Vendor{VendorID: "", VendorName: capitalize(label)}, true
			}
		}
	}
	return Vendor{}, false
}

// capitalize uppercases the first letter of a normalized (lowercase) label.
// "globex" → "Globex".
func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// stripMailPrefix removes a leading mail-service subdomain if one is present.
// Returns the stripped domain and true on success; (original, false) otherwise.
func stripMailPrefix(domain string) (string, bool) {
	parts := strings.SplitN(domain, ".", 2)
	if len(parts) != 2 {
		return domain, false
	}
	if stripPrefixes[parts[0]] {
		return parts[1], true
	}
	return domain, false
}

// domainLabel returns the second-level domain label: "samplelighting" from
// "sample-lighting.example.com" or "notification.sample-lighting.example.com". Normalized the same
// way vendor-name tokens are normalized, so comparison is direct.
func domainLabel(domain string) string {
	parts := strings.Split(domain, ".")
	// If we have e.g. ["notification", "samplelighting", "com"], drop the first
	// if it's a known mail prefix.
	if len(parts) >= 3 && stripPrefixes[parts[0]] {
		parts = parts[1:]
	}
	if len(parts) < 2 {
		return ""
	}
	// second-to-last is the interesting label; ignore TLD
	return normalizeName(parts[len(parts)-2])
}

// Stats returns loaded-dataset sizes for reporting.
func (r *Resolver) Stats() (emails, unambiguousDomains, ambiguousDomains int) {
	return len(r.byEmail), len(r.byDomain), len(r.ambiguousDoms)
}

// NameTokenCount returns how many unambiguous vendor-name tokens are indexed.
// Useful for the reality-check header to confirm the name map is non-empty.
func (r *Resolver) NameTokenCount() int { return len(r.byNameToken) }

// IsAmbiguousDomain reports whether the sender's domain is known but ambiguous
// (maps to multiple vendors). Useful for the reality-check report.
func (r *Resolver) IsAmbiguousDomain(senderEmail string) (bool, int) {
	e := strings.ToLower(strings.TrimSpace(senderEmail))
	at := strings.LastIndex(e, "@")
	if at < 0 {
		return false, 0
	}
	n, ok := r.ambiguousDoms[e[at+1:]]
	return ok, n
}
