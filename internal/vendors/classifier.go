package vendors

import "strings"

// SenderClass categorizes a sender before we even try vendor lookup.
// Only ClassVendor should be fed to Resolve() — other classes have different workflows
// (Internal replies shouldn't be tagged; Relays hide the real vendor in the body;
// Logistics/Bank have their own queue semantics).
type SenderClass string

const (
	ClassInternal  SenderClass = "internal"  // @example.com — AP clerk reply, internal forward
	ClassRelay     SenderClass = "relay"     // e.g. quickbooks@intuit, vendor invoice delivered via a service
	ClassLogistics SenderClass = "logistics" // Carrier-A, Carrier-B, Carrier-C — shipment/tracking mail
	ClassBank      SenderClass = "bank"      // payment-confirmation mail from banks/processors
	ClassVendor    SenderClass = "vendor"    // candidate for vendor lookup
)

// Hardcoded domain lists. Short and obvious-first — expand as the test mailbox reveals more
// patterns. Kept here rather than in a config file because they rarely change
// and are part of the product's judgment, not an operator setting.
var (
	relayDomains = map[string]bool{
		"notification.intuit.com": true, // QuickBooks invoice relays
		"intuit.com":              true,
		"docusign.net":            true,
		"docusign.com":            true,
		"pandadoc.net":            true,
		"pandadoc.com":            true,
		"bill.com":                true,
		"billtrust.com":           true,
		"netsuite.com":            true, // catches sent-via.netsuite.com via suffix match
	}
	logisticsDomains = map[string]bool{
		"ups.com":            true,
		"upsbilling.ups.com": true,
		"fedex.com":          true,
		"usps.com":           true,
		"tforcefreight.com":  true,
		"yrcfreight.com":     true,
		"saia.com":           true,
		"xpo.com":            true,
		"odfl.com":           true,
	}
	bankDomains = map[string]bool{
		"usbank.com":     true,
		"chase.com":      true,
		"bofa.com":       true,
		"wellsfargo.com": true,
		"elavon.com":     true, // payment processor, like a bank for our purposes
		"stripe.com":     true,
		"square.com":     true,
		"paypal.com":     true,
	}
)

// Classify returns the sender class for a raw email address.
// Matches are on exact domain and suffix (so foo.intuit.com counts as intuit.com).
func Classify(senderEmail string) SenderClass {
	e := strings.ToLower(strings.TrimSpace(senderEmail))
	at := strings.LastIndex(e, "@")
	if at < 0 || at == len(e)-1 {
		return ClassVendor // no domain, fall through to vendor lookup → Unknown
	}
	domain := e[at+1:]
	if domain == "example.com" || strings.HasSuffix(domain, ".example.com") {
		return ClassInternal
	}
	if domainMatch(domain, relayDomains) {
		return ClassRelay
	}
	if domainMatch(domain, logisticsDomains) {
		return ClassLogistics
	}
	if domainMatch(domain, bankDomains) {
		return ClassBank
	}
	return ClassVendor
}

func domainMatch(domain string, set map[string]bool) bool {
	if set[domain] {
		return true
	}
	// match foo.bar.com against bar.com
	for d := range set {
		if strings.HasSuffix(domain, "."+d) {
			return true
		}
	}
	return false
}
