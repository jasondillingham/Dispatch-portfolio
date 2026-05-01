// Package recon reconciles an AI-extracted invoice against a P21 PO.
//
// Line-matching is fuzzy on item_id because vendors drop our brand prefix
// (P21 stores "DEL T14235-BL", vendor invoice shows "T14235-BL"). Qty and
// price comparisons are exact — per the author, price tolerance is exact-penny.
//
// Output is pure data; rendering lives in the web template.
package recon

import (
	"math"
	"strings"

	"dispatch/internal/aiclass"
	"dispatch/internal/cache"
	"dispatch/internal/p21"
)

// PriceEpsilon is the max absolute difference for a price to count as
// matching. Set to 0.005 (half a cent) so rounding at 2-decimal display
// doesn't trip exact-penny comparisons. the author approved exact-penny.
const PriceEpsilon = 0.005

// QtyEpsilon is the max abs diff for quantity matching. P21 qty_ordered
// is decimal (can be fractional for weight-based items), so use a very
// small tolerance for floating-point sanity.
const QtyEpsilon = 0.001

// Verdict is the per-line comparison outcome.
type Verdict string

const (
	VerdictMatch           Verdict = "match"              // extended prices align (qty/unit may differ if UoM varies)
	VerdictExtendedMatch   Verdict = "extended_match"     // extended OK but qty or unit split differs (UoM variance — usually fine)
	VerdictPriceMismatch   Verdict = "price_mismatch"     // extended differs primarily due to unit price
	VerdictQtyMismatch     Verdict = "qty_mismatch"       // extended differs primarily due to quantity
	VerdictBothMismatch    Verdict = "both_mismatch"      // extended differs, can't attribute cleanly
	VerdictExtraOnInvoice  Verdict = "extra_on_invoice"   // invoice line has no PO counterpart and isn't recognizable as a shipping/tax fee
	VerdictShippingFee     Verdict = "shipping_fee"       // invoice line matches "shipping/freight/delivery" patterns; not on PO; needs buyer confirmation
	VerdictTaxFee          Verdict = "tax_fee"            // invoice line matches "tax" patterns; not on PO
	VerdictHandlingFee     Verdict = "handling_fee"       // invoice line matches "handling/processing/fuel surcharge" patterns
	VerdictMissingFromInv  Verdict = "missing_from_invoice" // PO line has no invoice counterpart (partial shipment, usually OK)
)

// IsFeeVerdict returns true for the shipping/tax/handling family — the
// "extra on the invoice that's plausibly a fee, not a missing PO line."
// Used by the UI to render these distinctly and by the section-level
// FeeOnlyDiscrepancy logic.
func IsFeeVerdict(v Verdict) bool {
	return v == VerdictShippingFee || v == VerdictTaxFee || v == VerdictHandlingFee
}

// LinePair is one row of the reconciliation report: paired invoice line
// and PO line (either may be nil for orphans), plus the verdict.
type LinePair struct {
	Invoice *cache.InvoiceLine `json:"invoice,omitempty"`
	PO      *p21.POLine        `json:"po,omitempty"`
	Verdict Verdict            `json:"verdict"`
	Note    string             `json:"note,omitempty"`
}

// Reconciliation is the full comparison snapshot cached per message.
type Reconciliation struct {
	PONo             int64      `json:"po_no"`
	POTotal          float64    `json:"po_total"`
	InvoiceTotal     float64    `json:"invoice_total"`
	TotalMatch       bool       `json:"total_match"`
	TotalDiff        float64    `json:"total_diff"` // invoice_total - po_total
	Lines            []LinePair `json:"lines"`
	AnyLineMismatch  bool       `json:"any_line_mismatch"`
	// Summary counts for the UI header
	LineMatches      int `json:"line_matches"`
	LineMismatches   int `json:"line_mismatches"`
	ExtraInvoice     int `json:"extra_on_invoice"`
	MissingFromInv   int `json:"missing_from_invoice"`
	FeeLines         int `json:"fee_lines"` // count of shipping/tax/handling extras

	// FeeOnlyDiscrepancy is true when EVERY problematic line is a fee
	// (shipping/tax/handling) and all PO lines themselves match. The UI
	// surfaces this distinctly because it's a common workflow: invoice
	// matches the PO except the vendor billed shipping that may or may
	// not be our responsibility — clerk confirms with buyer.
	FeeOnlyDiscrepancy bool `json:"fee_only_discrepancy,omitempty"`
}

// Compare runs the reconciliation. Pass both the AI-extracted invoice and the
// P21 PO lines; returns a Reconciliation with one LinePair per PO-or-invoice
// line (both, or either alone for orphans).
func Compare(poNo int64, invoice *cache.InvoiceData, poLines []p21.POLine) Reconciliation {
	r := Reconciliation{PONo: poNo, Lines: []LinePair{}}
	if invoice != nil {
		r.InvoiceTotal = invoice.InvoiceTotal
	}
	for _, pl := range poLines {
		r.POTotal += pl.Extended
	}
	r.TotalDiff = r.InvoiceTotal - r.POTotal
	r.TotalMatch = math.Abs(r.TotalDiff) <= PriceEpsilon

	if invoice == nil {
		// No AI data yet; just mirror PO lines as unmatched.
		for i := range poLines {
			r.Lines = append(r.Lines, LinePair{
				PO:      &poLines[i],
				Verdict: VerdictMissingFromInv,
				Note:    "no invoice extraction available",
			})
		}
		return r
	}

	// Track which PO lines have been paired so we don't reuse them.
	poUsed := make([]bool, len(poLines))

	for i := range invoice.Lines {
		inv := &invoice.Lines[i]
		bestIdx, score := findBestPOMatch(inv, poLines, poUsed)
		if bestIdx < 0 || score == 0 {
			// No PO match — but is it a recognizable fee line (shipping,
			// tax, handling)? Vendors routinely add freight that isn't on
			// the PO, and AP needs to confirm with the buyer rather than
			// treat it as a generic discrepancy. Classify into a fee
			// verdict so the UI can flag it distinctly.
			feeVerdict, feeNote := classifyFeeLine(inv)
			if feeVerdict != "" {
				r.Lines = append(r.Lines, LinePair{
					Invoice: inv,
					Verdict: feeVerdict,
					Note:    feeNote,
				})
				r.FeeLines++
				r.AnyLineMismatch = true
				continue
			}
			r.Lines = append(r.Lines, LinePair{
				Invoice: inv,
				Verdict: VerdictExtraOnInvoice,
				Note:    "no PO line matched by item ID or description",
			})
			r.ExtraInvoice++
			r.AnyLineMismatch = true
			continue
		}
		poUsed[bestIdx] = true
		pl := &poLines[bestIdx]

		// Extended-price is the economic truth for a line. Wire and other
		// distribution products commonly have a UoM split between the PO
		// (priced per foot) and the invoice (billed per reel). Same money,
		// different qty×unit_price presentation. If extended reconciles,
		// the line is fine regardless of that split.
		extendedOk := math.Abs(inv.Extended-pl.Extended) <= PriceEpsilon
		qtyOk := math.Abs(inv.Qty-pl.QtyOrdered) <= QtyEpsilon
		priceOk := math.Abs(inv.UnitPrice-pl.UnitPrice) <= PriceEpsilon

		var v Verdict
		var note string
		switch {
		case extendedOk && qtyOk && priceOk:
			v = VerdictMatch
		case extendedOk:
			// Same money, different UoM. Counts as a match for workflow purposes.
			v = VerdictExtendedMatch
			note = "line total matches; qty/unit split differs (UoM variance)"
		case qtyOk && !priceOk:
			v = VerdictPriceMismatch
			note = "unit price differs from PO"
		case !qtyOk && priceOk:
			v = VerdictQtyMismatch
			note = "qty differs from PO"
		default:
			v = VerdictBothMismatch
			note = "qty and unit price both differ; extended does not reconcile"
		}
		if v == VerdictMatch || v == VerdictExtendedMatch {
			r.LineMatches++
		} else {
			r.LineMismatches++
			r.AnyLineMismatch = true
		}
		r.Lines = append(r.Lines, LinePair{Invoice: inv, PO: pl, Verdict: v, Note: note})
	}
	// Emit any PO lines that weren't paired.
	for i, used := range poUsed {
		if used {
			continue
		}
		r.Lines = append(r.Lines, LinePair{
			PO:      &poLines[i],
			Verdict: VerdictMissingFromInv,
			Note:    "not shipped/billed on this invoice (may be partial)",
		})
		r.MissingFromInv++
	}

	// Fee-only-discrepancy summary: every problematic line is a fee
	// (shipping/tax/handling), no real line mismatches, and no extra
	// non-fee invoice lines. Lets the UI flag "shipping discrepancy —
	// confirm with buyer" instead of generic "discrepancy."
	if r.AnyLineMismatch && r.FeeLines > 0 && r.LineMismatches == 0 && r.ExtraInvoice == 0 {
		r.FeeOnlyDiscrepancy = true
	}

	return r
}

// classifyFeeLine returns a fee verdict + note when an unmatched invoice line
// looks like a shipping/tax/handling fee. Returns ("", "") if it doesn't.
// Conservative: matches against item_id and description with strict keyword
// patterns so we don't false-positive on real items that happen to mention
// "freight elevator" etc.
func classifyFeeLine(inv *cache.InvoiceLine) (Verdict, string) {
	id := strings.ToLower(strings.TrimSpace(inv.ItemID))
	desc := strings.ToLower(strings.TrimSpace(inv.Description))
	hay := id + " " + desc

	// Order matters: shipping is checked before tax because "freight tax"
	// (rare, but exists on some carriers) should classify as shipping.
	switch {
	case containsAnyWord(hay, "freight", "shipping", "delivery", "ship", "freight charge", "shipping charge"):
		return VerdictShippingFee, "shipping/freight on invoice but not on PO — confirm with buyer if we pay freight"
	case containsAnyWord(hay, "fuel surcharge", "fuel-surcharge", "fuelcharge"):
		return VerdictHandlingFee, "fuel surcharge — usually buyer's call whether to pay"
	case containsAnyWord(hay, "handling", "processing fee", "convenience fee", "service charge"):
		return VerdictHandlingFee, "handling/processing fee not on PO — confirm with buyer"
	case containsAnyWord(hay, "tax", "sales tax", "vat", "use tax"):
		return VerdictTaxFee, "tax line not on PO — typically expected, verify rate"
	}
	return "", ""
}

// containsAnyWord returns true if hay contains any of the needles as a
// whole-word match. Cheap; no regex. Lower-case input only.
func containsAnyWord(hay string, needles ...string) bool {
	for _, n := range needles {
		// Whole-word: bounded by space, start, or end. Cheap check via
		// padded lookups on a trimmed haystack.
		padded := " " + hay + " "
		if strings.Contains(padded, " "+n+" ") || strings.Contains(padded, " "+n+"s ") {
			return true
		}
	}
	return false
}

// findBestPOMatch picks the best-matching PO line for an invoice line.
// Returns (-1, 0) if no plausible candidate. Skips PO lines already used.
// Score is a rough quality signal: higher = better; caller doesn't need
// the number, just uses it to reject zero-score matches.
func findBestPOMatch(inv *cache.InvoiceLine, poLines []p21.POLine, used []bool) (int, int) {
	invItem := normalizeItemID(inv.ItemID)
	invDesc := strings.ToLower(inv.Description)

	bestIdx := -1
	bestScore := 0
	for i, pl := range poLines {
		if used[i] {
			continue
		}
		score := 0
		poItem := normalizeItemID(pl.ItemID)
		// Exact normalized item_id match is the strongest signal.
		if invItem != "" && invItem == poItem {
			score += 100
		}
		// Ends-with: invoice "T14235-BL" against PO "DEL T14235-BL".
		if invItem != "" && strings.HasSuffix(poItem, invItem) && len(invItem) >= 4 {
			score += 50
		}
		// Description keyword overlap (lowercased, both ways). Simple contains
		// is fine; invoice descriptions are usually shorter.
		if invDesc != "" && pl.Description != "" {
			poDesc := strings.ToLower(pl.Description)
			for _, tok := range strings.Fields(invDesc) {
				if len(tok) >= 4 && strings.Contains(poDesc, tok) {
					score += 5
				}
			}
		}
		// Exact qty match is a tiebreaker when item IDs are ambiguous.
		if math.Abs(inv.Qty-pl.QtyOrdered) <= QtyEpsilon {
			score += 10
		}
		if score > bestScore {
			bestScore = score
			bestIdx = i
		}
	}
	return bestIdx, bestScore
}

// FromVerifyResult converts the AI verification output into our existing
// Reconciliation shape. The AI has already paired each PO line with what it
// sees on the invoice, so we don't need the fuzzy item-ID matching that
// Compare() uses. This path is called when the worker has a resolved PO +
// PDF and uses VerifyAgainstPO instead of open-ended extraction.
func FromVerifyResult(poNo int64, v *aiclass.VerifyResult, poLines []p21.POLine) Reconciliation {
	r := Reconciliation{PONo: poNo, Lines: []LinePair{}}
	if v == nil {
		return r
	}
	r.InvoiceTotal = v.InvoiceTotalObserved
	for _, pl := range poLines {
		r.POTotal += pl.Extended
	}
	r.TotalDiff = r.InvoiceTotal - r.POTotal
	r.TotalMatch = math.Abs(r.TotalDiff) <= PriceEpsilon

	// Index PO lines by line_no for verify-result lookups.
	byLineNo := make(map[int]*p21.POLine, len(poLines))
	for i := range poLines {
		byLineNo[poLines[i].LineNo] = &poLines[i]
	}

	// Emit one LinePair per verify line. AI has already matched to the expected
	// line_no so no fuzzy pairing needed.
	for _, lr := range v.Lines {
		pl, ok := byLineNo[lr.LineNo]
		if !ok {
			continue // shouldn't happen; AI invented a line_no. Skip silently.
		}
		pair := LinePair{PO: pl}

		switch lr.Status {
		case "match":
			pair.Verdict = VerdictMatch
			// Fill in Invoice from PO so the UI shows both columns consistently.
			pair.Invoice = &cache.InvoiceLine{
				ItemID: pl.ItemID, Description: pl.Description,
				Qty: pl.QtyOrdered, UnitPrice: pl.UnitPrice, Extended: pl.Extended,
			}
			r.LineMatches++

		case "differs":
			pair.Invoice = &cache.InvoiceLine{
				ItemID: pl.ItemID, Description: pl.Description,
				Qty: lr.ObservedQty, UnitPrice: lr.ObservedUnitPrice, Extended: lr.ObservedExtended,
			}
			qtyOk := math.Abs(lr.ObservedQty-pl.QtyOrdered) <= QtyEpsilon
			priceOk := math.Abs(lr.ObservedUnitPrice-pl.UnitPrice) <= PriceEpsilon
			extOk := math.Abs(lr.ObservedExtended-pl.Extended) <= PriceEpsilon

			switch {
			case extOk && qtyOk && priceOk:
				// Model reported differs but numbers reconcile — trust the numbers.
				pair.Verdict = VerdictMatch
			case extOk:
				pair.Verdict = VerdictExtendedMatch
				pair.Note = "line total matches; qty/unit split differs (UoM variance)"
			case !qtyOk && !priceOk:
				pair.Verdict = VerdictBothMismatch
				pair.Note = "qty and price both differ"
			case !priceOk:
				pair.Verdict = VerdictPriceMismatch
				pair.Note = "unit price differs"
			case !qtyOk:
				pair.Verdict = VerdictQtyMismatch
				pair.Note = "qty differs"
			default:
				pair.Verdict = VerdictBothMismatch
			}
			if lr.Note != "" {
				if pair.Note != "" {
					pair.Note += " — " + lr.Note
				} else {
					pair.Note = lr.Note
				}
			}
			if pair.Verdict == VerdictMatch || pair.Verdict == VerdictExtendedMatch {
				r.LineMatches++
			} else {
				r.LineMismatches++
				r.AnyLineMismatch = true
			}

		case "not_found":
			pair.Verdict = VerdictMissingFromInv
			pair.Note = "AI could not find this line on the invoice"
			r.MissingFromInv++
		}
		r.Lines = append(r.Lines, pair)
	}

	// Append lines the AI saw that weren't on the PO (freight, tax, bonus).
	for _, u := range v.UnexpectedLines {
		inv := &cache.InvoiceLine{
			Description: u.Description,
			Qty:         u.Qty,
			UnitPrice:   u.UnitPrice,
			Extended:    u.Extended,
		}
		r.Lines = append(r.Lines, LinePair{
			Invoice: inv,
			Verdict: VerdictExtraOnInvoice,
			Note:    "not on PO (freight, tax, bonus, etc.)",
		})
		r.ExtraInvoice++
	}
	// Treat unexpected lines only as a discrepancy if they cause the total not
	// to match — otherwise freight/tax on a reconciling total is usually OK.
	if !r.TotalMatch {
		r.AnyLineMismatch = true
	}
	return r
}

// normalizeItemID lowercases and strips non-alphanumeric from an item ID
// so "DEL T14235-BL" and "T14235BL" can be compared meaningfully.
func normalizeItemID(s string) string {
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
