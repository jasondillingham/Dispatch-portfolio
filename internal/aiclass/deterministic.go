package aiclass

import (
	"regexp"
	"strings"
)

// DeterministicKind tries to classify a message using high-precision text rules
// before we spend an AI call on it. Returns one of the known Kind values when
// a confident rule matches; returns "" when no rule fires (let the AI handle).
//
// Rules are intentionally narrow: false positives propagate into Outlook
// categories which clerks see directly. When a pattern is ambiguous (e.g.
// "Invoice" appearing in an order acknowledgement subject), prefer the more
// specific Kind earlier in the chain or fall through to AI.
//
// Ordering matters — earlier matches win. The chain reads roughly from "most
// specific keyword" to "least specific keyword" so a subject like
// "Order Confirmation including Invoice #12345" classifies as OrderConfirmation
// (the email's purpose) rather than Invoice (the number it mentions).
func DeterministicKind(subject, sender, body string) string {
	subj := strings.ToLower(subject)
	snd := strings.ToLower(sender)
	bod := strings.ToLower(body)

	// Statement — narrow matches because "statement" appears in many vendor
	// emails as a generic word ("our statement of work…"). Exact subject
	// matches and "statement of account" are the safe wins.
	if reStatementSubject.MatchString(subj) {
		return "Statement"
	}

	// Order confirmation / acknowledgement — checked BEFORE Invoice because
	// Generic-Brand-style subjects ("PO# X / SO# Y") and NSI ("Your order has been
	// received!") commonly include numbers that look invoice-like but aren't.
	if reOrderConfirm.MatchString(subj) {
		return "OrderConfirmation"
	}

	// Credit memo — distinct enough subject keyword that we can pull it out.
	if reCredit.MatchString(subj) {
		return "Credit"
	}

	// Payment notifications — usually from us to the vendor (remittance advice,
	// payment scheduled). Subject keywords are reliable.
	if rePayment.MatchString(subj) {
		return "Payment"
	}

	// Webinar / training / lunch-and-learn — invitation style mail. Common
	// false positive: a vendor's "training opportunity" mail. We match liberally
	// because these all land in the same "non-actionable" bucket via
	// HideByDefault — getting Webinar vs Newsletter slightly wrong has no AP
	// consequence.
	if reWebinar.MatchString(subj) {
		return "Webinar"
	}
	if reNewsletter.MatchString(subj) {
		return "Newsletter"
	}
	// Marketing — sender side is the strongest signal (marketing@, promo@,
	// newsletter@), with subject keywords as backup.
	if reMarketingSender.MatchString(snd) {
		return "Marketing"
	}
	if reMarketingSubject.MatchString(subj) {
		return "Marketing"
	}

	// Invoice — last because "invoice" appears in many other contexts. We
	// require either an attached-invoice phrase ("invoice attached", "please
	// find your invoice") or a number-bearing pattern ("Invoice #12345",
	// "Invoice 12345"). Bare subjects like "Re: invoice" don't fire — those
	// can be replies, disputes, etc.
	if reInvoiceWithNumber.MatchString(subj) || reInvoiceAttached.MatchString(subj) || reInvoiceAttached.MatchString(bod) {
		return "Invoice"
	}

	return ""
}

var (
	// (^|word-boundary)(account|monthly|year-to-date)? statement(s)?($|word-boundary
	// to "of account") — bare "Statement" subjects, common from vendor
	// statement automations.
	reStatementSubject = regexp.MustCompile(
		`(?i)^(?:re:\s*|fwd?:\s*)*(account |monthly |annual |year-to-date |ytd )?statement(s)?(\s+of\s+account)?(\s*[-:|.][^a-z]|\s*$)`,
	)

	// "Order confirmation", "order acknowledgement/acknowledgment", "PO# X /
	// SO# Y" (Generic-Brand), "<your> ... order has been received" (NSI),
	// "order received", "thank you for your order", "order tracking".
	// "order has been" and "order is being" handle the case where vendor
	// name slips between "your" and "order" ("Your Sample Industries order…").
	reOrderConfirm = regexp.MustCompile(
		`(?i)\border\s+(confirmation|acknowledg(?:e?ment)|ack)\b|\bpo\s*#?\s*\d+\s*\/\s*so\s*#?|\border\s+(has\s+been|is\s+being|received)\b|\bthank(?:s|\s+you)\s+for\s+your\s+order\b|\border\s+tracking\b`,
	)

	// "credit memo", "credit note" — pretty specific.
	reCredit = regexp.MustCompile(
		`(?i)\bcredit\s+(memo|note)\b`,
	)

	// "remittance advice", "payment scheduled", "payment notification",
	// "your payment is/has", "payment received".
	rePayment = regexp.MustCompile(
		`(?i)\bremittance\s+advice\b|\bpayment\s+(scheduled|notification|received|advice|details)\b|\byour\s+payment\s+(is|has|will)\b|\bpayment\s+is\s+scheduled\b`,
	)

	// "webinar", "training", "lunch and learn", "lunch & learn", "live demo",
	// "register now".
	reWebinar = regexp.MustCompile(
		`(?i)\b(webinar|training\s+session|lunch\s*(and|&|&amp;)\s*learn|live\s+demo|register\s+now\b|class\s+invite)\b`,
	)

	// "newsletter", "industry update", "monthly digest".
	reNewsletter = regexp.MustCompile(
		`(?i)\b(newsletter|monthly\s+digest|weekly\s+digest|industry\s+update)\b`,
	)

	// Sender local-part contains marketing-style keywords.
	reMarketingSender = regexp.MustCompile(
		`(?i)^(marketing|promo(tions?)?|newsletter|news|updates?|hello|info|sales|deals|specials|offers)@`,
	)

	// Subject heuristics for marketing — "save N%", "limited time", "exclusive
	// offer", "only X days left", "new product launch".
	reMarketingSubject = regexp.MustCompile(
		`(?i)\b(save\s+\d|limited\s+time|exclusive\s+offer|new\s+product\s+launch|don['’]?t\s+miss|hurry|last\s+chance|special\s+offer|free\s+shipping\s+on)\b`,
	)

	// "Invoice 12345" or "Invoice #12345" or "Invoice No 12345" — number-
	// bearing. The number must come within a short window of "invoice" to
	// avoid matching "Re: Invoice question … reference 12345" style.
	reInvoiceWithNumber = regexp.MustCompile(
		`(?i)\binvoice\s*(no\.?|number|num|#)?\s*\d{3,}\b`,
	)

	// "invoice attached", "please find your invoice", "find attached invoice".
	// Intentionally does NOT include bare "new invoice" — too loose, fires on
	// announcements and unrelated subject lines. AI handles the ambiguous
	// invoice-keyword cases that don't include "attached" or a number.
	reInvoiceAttached = regexp.MustCompile(
		`(?i)invoice\s+attached\b|please\s+find.{0,30}\binvoice\b|attached\s+(is|find)\s+(your|the)\s+invoice`,
	)
)
