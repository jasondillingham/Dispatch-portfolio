// Package poscan extracts P21 reference numbers (POs first, items later)
// from email text. Require a nearby keyword so random 7-digit numbers
// (credit-card fragments, invoice totals, tracking numbers) don't generate
// false-positive lookups.
package poscan

import (
	"regexp"
	"strconv"
)

// poRe matches phrases like:
//   PO 1234904
//   PO# 1234904
//   PO #1234904
//   P.O. 1234904
//   P.O.#1234904
//   Purchase Order 1234904
//   Order # 1234904
//   order: 1234904
//   Purchase Order No:\n1234904    (common in PDF text extraction)
//   PO Number\n1234904
//   Customer P.O   Ship Via   Terms\n  1234507  SampleShipper  (column layout)
//
// 6-8 digit numbers only (internal POs are 7 digits; leave some headroom).
// [^\d]{0,400} lets the number be separated from the keyword by labels,
// whitespace, newlines, AND several intervening column headers — a
// pdftotext -layout dump of a tabular invoice puts "Customer P.O" on a
// header row and the value on the next row with 4-6 column headers in
// between. P21 validation catches any false positives from the widened
// window.
var poRe = regexp.MustCompile(`(?is)\b(?:P\.?\s*O\.?|purchase\s+order|customer\s+po|cust\s*\.?\s*po|order)[^\d]{0,400}(\d{6,8})\b`)

// bareSubjectRe matches 7-digit standalone numbers — used only on subject
// lines where the "Re: <po> <vendor>" internal-reply convention is common.
// The result is low-confidence; callers must validate via P21 lookup before
// acting. 7 digits exactly (not 6-8) because that's the internal PO format and
// going wider hits more false positives (phone numbers, invoice counts).
var bareSubjectRe = regexp.MustCompile(`\b\d{7}\b`)

// poFilenameRe matches PO numbers embedded in attachment filenames.
// Patterns we catch:
//   PO1235840.pdf / po-1235840.pdf / po_1235840.pdf
//   Invoice_1235840.pdf / INV-1235840.pdf
//   Acme Supply - 1234742.pdf  (standalone 7-digit number at a boundary)
//
// Separate from poRe because filenames are keyword-poor ("PO1235840" has
// no surrounding text to match). Accept looser matching — P21 validation
// downstream kills any false positives.
var poFilenameRe = regexp.MustCompile(`(?i)(?:^|[^0-9])(?:p\.?o\.?[-_ ]?#?|order[-_ ]?#?|inv(?:oice)?[-_ ]?#?)?[-_ ]?(\d{7})(?:[^0-9]|$)`)

// invNumFilenameRe matches vendor invoice numbers embedded in filenames,
// including alphanumeric patterns like "INV18860098" or "0907518891".
// Emits the raw string (callers lowercase/validate as needed).
// Matches ABC123, INV-12345, or just long runs of digits in suggestive
// contexts. Used for metadata enrichment, not PO lookup.
var invNumFilenameRe = regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9])inv(?:oice)?[-_]?([A-Z0-9]{5,20})`)

// ExtractPOsFromFilename returns PO numbers found in an attachment's
// filename. This is the cheapest possible lookup path — zero Graph
// calls, zero pdftotext, zero vision. Callers must still validate each
// candidate via the P21 PO lookup because 7-digit numbers are noisy.
func ExtractPOsFromFilename(filename string) []int64 {
	seen := map[int64]bool{}
	var out []int64
	for _, m := range poFilenameRe.FindAllStringSubmatch(filename, -1) {
		n, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil || n == 0 {
			continue
		}
		if seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

// ExtractInvoiceNumberFromFilename returns a vendor invoice number string
// if one is embedded in the filename. Empty string on no match. First
// match wins — we don't expect more than one invoice# per filename.
func ExtractInvoiceNumberFromFilename(filename string) string {
	m := invNumFilenameRe.FindStringSubmatch(filename)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// ExtractBareSubjectNumbers returns unique 7-digit numbers in subject.
// Use ONLY when keyword-matched ExtractPOs returned nothing, and only on
// subject text (body has too much ambient noise — tracking numbers, zip codes).
func ExtractBareSubjectNumbers(subject string) []int64 {
	seen := map[int64]bool{}
	var out []int64
	for _, m := range bareSubjectRe.FindAllString(subject, -1) {
		n, err := strconv.ParseInt(m, 10, 64)
		if err != nil || n == 0 {
			continue
		}
		if seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

// ExtractPOs returns unique PO numbers found across all provided text snippets.
// Pass subject, bodyPreview, and optionally the full body. Order preserved
// (first occurrence wins), capped at max to keep lookup volume predictable.
func ExtractPOs(max int, texts ...string) []int64 {
	seen := map[int64]bool{}
	out := []int64{}
	for _, t := range texts {
		for _, m := range poRe.FindAllStringSubmatch(t, -1) {
			n, err := strconv.ParseInt(m[1], 10, 64)
			if err != nil || n == 0 {
				continue
			}
			if seen[n] {
				continue
			}
			seen[n] = true
			out = append(out, n)
			if max > 0 && len(out) >= max {
				return out
			}
		}
	}
	return out
}
