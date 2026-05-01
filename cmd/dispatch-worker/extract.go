// extract.go — invoice extraction pipeline. extractInvoice (entry from sort
// pool when a PO is known) → tier-1/2/3 attempts → verifyInvoice (preferred
// path: confirm the PO's expected lines against the invoice image) →
// processFallbackJob (tier-4 escalation, drained by fallback pool). openExtract
// is the legacy "no ERP PO lines" fallback that produces an InvoiceData
// snapshot without recon. processExtractionResult writes the final verdict +
// auto-blocker categories.

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"dispatch/internal/aiclass"
	"dispatch/internal/blobstore"
	"dispatch/internal/cache"
	"dispatch/internal/graph"
	"dispatch/internal/erp"
	"dispatch/internal/pdftext"
	"dispatch/internal/recon"
	"dispatch/internal/vendors"
)


// extractInvoice reads the message's first PDF attachment, runs vision-based
// invoice reconciliation, and caches the result. The primary path is
// VerifyAgainstPO — given the PO's expected lines, the AI confirms each one
// on the invoice image (much higher accuracy than open-ended extraction).
// ExtractInvoiceData remains as the fallback when the ERP has no PO lines.
//
// Called only when we already have a resolved PO. Deliberately best-effort:
// the outer loop has already committed the vendor/buyer tags to Outlook.
// This just enriches the detail-view.
// minTier: skip extraction tiers below this. 0 = try everything from tier 1.
// Rescans pass minTier=lastTier+1 so each rescan attempt actually tries a
// different approach instead of re-running the tier that already produced
// a bad verdict.
func extractInvoice(cacheDB *cache.Cache, gc *graph.Client, vc, fallbackVC, paddleVC *aiclass.Client, fallbackJobs chan fallbackJob, erpc *erp.Client, blob *blobstore.Store, mailbox string, m graph.Message, poNo int64, currentCats []string, minTier int, slot int) {
	start := time.Now()
	storeErr := func(msg string) {
		storeExtractionErr(cacheDB, mailbox, m, vc, poNo, msg, start)
	}

	atts, err := gc.ListAttachments(mailbox, m.ID)
	if err != nil {
		storeErr("list attachments: " + err.Error())
		return
	}

	var pdfBytes []byte
	var pdfName string
	for _, a := range atts {
		if a.IsInline || !isPDF(a.ContentType, a.Name) {
			continue
		}
		if isNonInvoiceFormName(a.Name) {
			continue
		}
		b, _, err := fetchAttachmentBytes(mailbox, m.ID, a.ID, blob, gc, cacheDB)
		if err != nil || len(b) == 0 || len(b) > pdftext.MaxSize {
			continue
		}
		pdfBytes = b
		pdfName = a.Name
		break // first real PDF wins — same logic as PO extraction
	}
	if pdfBytes == nil {
		storeErr("no usable pdf attachment")
		return
	}

	// Tier 1: try the PDF's text layer first. For vendor-generated invoices
	// this returns the full invoice in ~50ms; we'd rather feed text to a
	// text-only LLM call than rasterize and run a vision model. Skipped when
	// rescan has told us tier 1 already ran and didn't produce a clean match.
	pdfText := ""
	textSource := ""
	if minTier <= 1 {
		if t, err := pdftext.Extract(bytes.NewReader(pdfBytes)); err == nil {
			pdfText = t
			textSource = "pdftotext"
		}
	}

	png, err := pdftext.ConvertFirstPagePNG(pdfBytes)
	if err != nil {
		storeErr("pdftoppm: " + err.Error())
		return
	}

	// Tier 2: Paddle OCR. Runs when tier 1 didn't produce text OR when the
	// rescan path explicitly wants this tier (minTier=2).
	if pdfText == "" && paddleVC != nil && minTier <= 2 {
		paddleCtx, paddleCancel := context.WithTimeout(context.Background(), 5*time.Minute)
		ocr, oerr := paddleVC.OCRTranscribe(paddleCtx, png)
		paddleCancel()
		if oerr != nil {
			fmt.Printf("           paddle-ocr err %q: %v\n", pdfName, oerr)
		} else if len(strings.TrimSpace(ocr)) >= 50 {
			pdfText = ocr
			textSource = "paddle-ocr"
			fmt.Printf("           paddle-ocr %q → %d chars transcribed\n", pdfName, len(pdfText))
		}
	}

	// Fetch the ERP PO lines up front. If we have them, take the verify path;
	// otherwise fall back to open-ended extraction.
	erpCtx, erpCancel := context.WithTimeout(context.Background(), 10*time.Second)
	poLines, _ := erpc.ListPOLines(erpCtx, poNo)
	erpCancel()

	// If rescan forced us to tier 3+, pdfText stays empty → verify/openExtract
	// will skip their text-first attempts and go straight to vision.
	if minTier >= 3 {
		pdfText = ""
	}

	// Content hash of the PDF bytes — used to key the per-PDF cooldown
	// table so one pathological invoice can't re-enter the fallback pool
	// on every rescan pass.
	shaBytes := sha256.Sum256(pdfBytes)
	pdfSha := hex.EncodeToString(shaBytes[:])

	if len(poLines) > 0 {
		verifyInvoice(cacheDB, gc, vc, fallbackVC, fallbackJobs, mailbox, m, poNo, pdfName, pdfSha, pdfText, textSource, png, poLines, start, currentCats, minTier, slot)
		return
	}
	// Fallback: no the ERP PO lines (PO not found or empty) → open-ended extraction.
	openExtract(cacheDB, vc, mailbox, m, poNo, pdfName, pdfText, textSource, png, start, vendorFromCategories(currentCats))
}

// verifyInvoice runs the preferred verify-against-PO path: the model confirms
// each expected line on the invoice image, producing a per-line verdict.
// Reconciliation is built directly from that output.
//
// Escalation is async. When the primary returns a poor verdict, we queue a
// fallbackJob and return — the main worker moves on immediately. A dedicated
// fallback pool drains the queue; its writes happen later, overwriting any
// cached primary verdict.
func verifyInvoice(cacheDB *cache.Cache, gc *graph.Client, vc, fallbackVC *aiclass.Client, fallbackJobs chan fallbackJob, mailbox string, m graph.Message, poNo int64, pdfName, pdfSha, pdfText, textSource string, png []byte, poLines []erp.POLine, start time.Time, currentCats []string, minTier int, slot int) {
	storeErr := func(msg string) {
		storeExtractionErr(cacheDB, mailbox, m, vc, poNo, msg, start)
	}

	// storeCooldownSkip finalizes a row that we're refusing to tier-4
	// rescan because the PDF is in cooldown. Clears needs_rescan so it
	// exits the rescan queue (otherwise the rescan pass would keep
	// re-enqueueing it every pass).
	storeCooldownSkip := func(until time.Time) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		msg := fmt.Sprintf("fallback cooldown until %s UTC", until.Format("2006-01-02 15:04"))
		_ = cacheDB.StoreInvoiceExtraction(ctx, mailbox, m.ID, "cooldown:tier4", poNo, nil, msg, time.Since(start), false)
	}

	expected := make([]aiclass.VerifyLineExpected, 0, len(poLines))
	for _, pl := range poLines {
		expected = append(expected, aiclass.VerifyLineExpected{
			LineNo: pl.LineNo, ItemID: pl.ItemID, Description: pl.Description,
			Qty: pl.QtyOrdered, UnitPrice: pl.UnitPrice, Extended: pl.Extended,
		})
	}

	// Text-first fast path. Text-only prompts are ~5-10× faster than vision
	// and work on any vendor-generated PDF. If the result looks reasonable
	// (primary didn't whiff on all lines), use it and skip the vision call.
	// On err, empty text, or whiff, fall through to the vision path below.
	if pdfText != "" {
		textCtx, textCancel := context.WithTimeout(context.Background(), aiclass.VisionTimeout)
		tv, terr := vc.VerifyAgainstPOFromText(textCtx, pdfText, expected)
		textCancel()
		if terr == nil && !shouldEscalateVerify(tv, nil, len(expected)) {
			tag := "text(" + textSource + "):" + vc.Model()
			fmt.Printf("           verify(%s): %q PO=%d → text-path hit in %s\n",
				textSource, pdfName, poNo, time.Since(start).Round(time.Second))
			processExtractionResult(cacheDB, gc, mailbox, m, poNo, pdfName, poLines, tv, currentCats, tag, start)
			return
		}
		// Text path whiffed; vision will get a shot. No storeErr here — we're
		// only finalizing on the vision verdict.
	}

	// If rescan told us tier 3 already ran and produced a bad verdict, skip
	// straight to tier 4 (fallback) by synthesizing an escalation request.
	if minTier >= 4 && fallbackVC != nil && fallbackJobs != nil {
		if pdfSha != "" {
			cdCtx, cdCancel := context.WithTimeout(context.Background(), 2*time.Second)
			active, until, reason, _ := cacheDB.CheckPDFCooldown(cdCtx, pdfSha, topExtractionTier)
			cdCancel()
			if active {
				fmt.Printf("           verify: %q PO=%d → tier-4 skip (cooldown until %s, %s)\n",
					pdfName, poNo, until.Format("15:04"), reason)
				storeCooldownSkip(until)
				return
			}
		}
		fmt.Printf("           verify: %q PO=%d → tier-4 rescan (skipping primary)\n", pdfName, poNo)
		select {
		case fallbackJobs <- fallbackJob{
			mailbox: mailbox, m: m, poNo: poNo, pdfName: pdfName, pdfSha: pdfSha, png: png,
			poLines: poLines, currentCats: currentCats,
			primaryResult: nil, primaryModel: vc.Model(),
			start: start, reason: "rescan escalation: minTier=4",
		}:
			return
		default:
			fmt.Printf("           fallback queue full; falling back to primary\n")
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), aiclass.VisionTimeout*3)
	defer cancel()
	v, err := vc.VerifyAgainstPO(ctx, png, expected)

	// Decide: escalate (queue fallback) or finalize inline?
	if shouldEscalateVerify(v, err, len(expected)) && fallbackVC != nil && fallbackJobs != nil {
		// Cooldown check: if tier-4 is in cooldown for this PDF, keep
		// whatever the primary produced (even if imperfect) rather than
		// burning another 30-49 min slot on a known-pathological invoice.
		if pdfSha != "" {
			cdCtx, cdCancel := context.WithTimeout(context.Background(), 2*time.Second)
			active, until, cdReason, _ := cacheDB.CheckPDFCooldown(cdCtx, pdfSha, topExtractionTier)
			cdCancel()
			if active {
				fmt.Printf("           verify: tier-4 cooldown active (until %s, %s) — keeping primary\n",
					until.Format("15:04"), cdReason)
				if err == nil && v != nil {
					processExtractionResult(cacheDB, gc, mailbox, m, poNo, pdfName, poLines, v, currentCats, vc.Model()+" (cooldown-kept)", start)
				} else {
					storeCooldownSkip(until)
				}
				return
			}
		}
		reason := "primary error"
		if err == nil {
			reason = fmt.Sprintf("primary found 0 of %d lines", len(expected))
		}
		fmt.Printf("           verify queued for fallback (%s → %s): %s\n",
			vc.Model(), fallbackVC.Model(), reason)
		select {
		case fallbackJobs <- fallbackJob{
			mailbox: mailbox, m: m, poNo: poNo, pdfName: pdfName, pdfSha: pdfSha, png: png,
			poLines: poLines, currentCats: currentCats,
			primaryResult: v, primaryModel: vc.Model(),
			start: start, reason: reason,
		}:
			return
		default:
			// Channel full — fall through to synchronous behavior so we don't
			// drop work. Not great but safer than losing the verdict.
			fmt.Printf("           fallback queue full; processing primary inline\n")
		}
	}

	if err != nil {
		storeErr("vision verify: " + err.Error())
		return
	}
	processExtractionResult(cacheDB, gc, mailbox, m, poNo, pdfName, poLines, v, currentCats, vc.Model(), start)
}

// processExtractionResult is the shared post-verify pipeline: synthesize
// InvoiceData, run recon, persist, auto-blocker. Called from both the main
// worker (when no escalation) and the fallback pool (when escalation
// completed). Idempotent on the extraction row — second call just overwrites.
func processExtractionResult(cacheDB *cache.Cache, gc *graph.Client, mailbox string, m graph.Message, poNo int64, pdfName string, poLines []erp.POLine, v *aiclass.VerifyResult, currentCats []string, usedModel string, start time.Time) {
	cached := &cache.InvoiceData{
		PONumber:      fmt.Sprintf("%d", poNo),
		InvoiceNumber: v.InvoiceNumber,
		InvoiceDate:   v.InvoiceDate,
		InvoiceTotal:  v.InvoiceTotalObserved,
	}
	byLineNo := map[int]*erp.POLine{}
	for i := range poLines {
		byLineNo[poLines[i].LineNo] = &poLines[i]
	}
	for _, lr := range v.Lines {
		pl, ok := byLineNo[lr.LineNo]
		if !ok {
			continue
		}
		inv := cache.InvoiceLine{ItemID: pl.ItemID, Description: pl.Description}
		if lr.Status == "differs" {
			inv.Qty = lr.ObservedQty
			inv.UnitPrice = lr.ObservedUnitPrice
			inv.Extended = lr.ObservedExtended
		} else if lr.Status == "match" {
			inv.Qty = pl.QtyOrdered
			inv.UnitPrice = pl.UnitPrice
			inv.Extended = pl.Extended
		}
		cached.Lines = append(cached.Lines, inv)
	}
	for _, u := range v.UnexpectedLines {
		cached.Lines = append(cached.Lines, cache.InvoiceLine{
			Description: u.Description, Qty: u.Qty, UnitPrice: u.UnitPrice, Extended: u.Extended,
		})
	}

	r := recon.FromVerifyResult(poNo, v, poLines)
	cleanMatch := r.TotalMatch && !r.AnyLineMismatch
	needsRescan := !cleanMatch

	storeCtx, storeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer storeCancel()
	if err := cacheDB.StoreInvoiceExtraction(storeCtx, mailbox, m.ID, usedModel, poNo, cached, "", time.Since(start), needsRescan); err != nil {
		fmt.Printf("           verify-cache store failed: %v\n", err)
		return
	}
	poLinesJSON, _ := json.Marshal(poLines)
	reconJSON, _ := json.Marshal(r)
	if err := cacheDB.StoreReconciliation(storeCtx, mailbox, m.ID, string(poLinesJSON), string(reconJSON)); err != nil {
		fmt.Printf("           verify-recon cache store failed: %v\n", err)
		return
	}

	verdict := "match"
	if !cleanMatch {
		verdict = "DISCREPANCY (flagged for rescan queue)"
	}
	fmt.Printf("           verify: %q PO=%d inv=%.2f po=%.2f diff=%.2f lines=%d✓/%d≠/%d? + %d extra %s in %s [%s]\n",
		pdfName, r.PONo, r.InvoiceTotal, r.POTotal, r.TotalDiff,
		r.LineMatches, r.LineMismatches, r.MissingFromInv, r.ExtraInvoice, verdict,
		time.Since(start).Round(time.Second), usedModel)

	if !cleanMatch && len(currentCats) > 0 {
		blockers := autoBlockersFromRecon(r)
		if len(blockers) > 0 {
			newCats := applyAutoBlockers(currentCats, blockers)
			if !sameCats(currentCats, newCats) {
				err := gc.SetCategories(mailbox, m.ID, newCats)
				if err != nil {
					fmt.Printf("           auto-blocker patch err: %v\n", err)
				} else {
					fmt.Printf("           auto-blocker: %v (status→Blocked)\n", blockers)
				}
			}
		}
	}
}

// processFallbackJob runs the fallback verify for a queued job. If fallback
// errors and the job had a primary result, we finalize with the primary (so
// the clerk still sees *something*). If both failed, store an error row.
//
// Regardless of outcome, stamps a tier-4 cooldown on the PDF content hash so
// the rescan loop can't re-enter this slot on the next pass. Duration varies
// by outcome: longer for error/timeout (transient infra problem may need
// time), shorter for discrepancy (verdict is stable, just not clean).
func processFallbackJob(cacheDB *cache.Cache, gc *graph.Client, fallbackVC *aiclass.Client, job fallbackJob) {
	expected := make([]aiclass.VerifyLineExpected, 0, len(job.poLines))
	for _, pl := range job.poLines {
		expected = append(expected, aiclass.VerifyLineExpected{
			LineNo: pl.LineNo, ItemID: pl.ItemID, Description: pl.Description,
			Qty: pl.QtyOrdered, UnitPrice: pl.UnitPrice, Extended: pl.Extended,
		})
	}
	stampCooldown := func(dur time.Duration, reason string) {
		if job.pdfSha == "" {
			return
		}
		cdCtx, cdCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cdCancel()
		_ = cacheDB.SetPDFCooldown(cdCtx, job.pdfSha, topExtractionTier, time.Now().UTC().Add(dur), reason)
	}
	// Fallback timeout budget: gemma4:26b on ai-03's Tesla P40 (since
	// 2026-04-27) lands at 2-4 min for typical invoices. Burst load with 3
	// fallback workers and OLLAMA_NUM_PARALLEL=4 can push individual calls
	// past 6 min when ai-03 is queueing internally. 9 min gives headroom
	// for those bursts without holding a worker forever on a broken endpoint.
	ctx, cancel := context.WithTimeout(context.Background(), aiclass.VisionTimeout*6)
	defer cancel()
	v, err := fallbackVC.VerifyAgainstPO(ctx, job.png, expected)
	model := fallbackVC.Model() + " (fallback)"
	if err != nil {
		// 6h cooldown on error/timeout: long enough to ride out most infra
		// wobbles, short enough that a clerk retry-tomorrow workflow still
		// works.
		stampCooldown(6*time.Hour, "fallback error: "+truncate(err.Error(), 80))
		if job.primaryResult != nil {
			fmt.Printf("           fallback error, keeping primary: %v\n", err)
			processExtractionResult(cacheDB, gc, job.mailbox, job.m, job.poNo, job.pdfName,
				job.poLines, job.primaryResult, job.currentCats, job.primaryModel+" (primary-kept)", job.start)
			return
		}
		storeCtx, storeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		errMsg := "fallback: " + err.Error()
		_ = cacheDB.StoreInvoiceExtraction(storeCtx, job.mailbox, job.m.ID, model, job.poNo, nil, errMsg, time.Since(job.start), true)
		storeCancel()
		errLog.logExtractionErr(job.m.ID, job.m.Subject, model, errMsg)
		fmt.Printf("           fallback verify error: %v (no primary to keep)\n", err)
		return
	}
	// 24h cooldown on a completed tier-4 run: whether clean match or
	// discrepancy, we've done the work; no reason to repeat on CPU today.
	// A human verdict or new-PDF-upload changes the sha and bypasses.
	stampCooldown(24*time.Hour, "fallback completed at tier 4")
	processExtractionResult(cacheDB, gc, job.mailbox, job.m, job.poNo, job.pdfName,
		job.poLines, v, job.currentCats, model, job.start)
}

// shouldEscalateVerify returns true when a verify call's output is
// non-clean enough that we want a higher-tier model to take another pass.
// Triggers: any error, OR any non-match line status (differs/not_found/
// missing) — because a bad OCR can produce confident-looking verdicts that
// are actually garbage. The caller is responsible for respecting the tier
// ceiling: once we've reached the top tier (gemma4 fallback), additional
// escalation is pointless and will never clear.
func shouldEscalateVerify(v *aiclass.VerifyResult, err error, expectedCount int) bool {
	if err != nil {
		return true
	}
	if v == nil || expectedCount == 0 {
		return false
	}
	matches := 0
	for _, lr := range v.Lines {
		if lr.Status == "match" {
			matches++
		}
	}
	// Clean match = every expected line accounted for with "match" status.
	// Anything less → escalate so a higher-tier model can sanity-check.
	return matches < expectedCount
}

// extractionTier returns an ordinal 1..4 for a model column value, so the
// rescan path can pick the next tier up rather than re-running the tier
// that already produced a bad verdict. Tier 0 is returned when the string
// can't be classified (legacy rows, unknown models) — treated as "below
// tier 1" so any tier can run next.
//
//	1 text(pdftotext):*        fast, flimsy — bad OCR of text layers
//	2 text(paddle-ocr):*       specialized OCR on an image
//	3 <bare model name>        primary vision (minicpm-v etc.)
//	4 * (fallback)             big-iron fallback (gemma4:26b)
func extractionTier(modelTag string) int {
	switch {
	case strings.HasPrefix(modelTag, "text(pdftotext):"):
		return 1
	case strings.HasPrefix(modelTag, "text(paddle-ocr):"):
		return 2
	case strings.HasSuffix(modelTag, "(fallback)"):
		return 4
	case strings.HasPrefix(modelTag, "skip:"):
		// Skip rows should never rescan. Return the top tier so the
		// rescan-exhaustion branch cleans needs_rescan if anything ever
		// does flag one. (Normal path: skip rows have needs_rescan=0,
		// so ListRescanQueue never picks them up.)
		return topExtractionTier
	case modelTag == "":
		return 0
	default:
		return 3
	}
}

const topExtractionTier = 4

// autoBlockersFromRecon maps recon verdicts to allowed Blocker: values.
//
// Three business cases produce blockers:
//   - Pricing: invoice unit price differs, or invoice has a line not on PO
//     (tax, freight, bonus items) — clerk decides whether to pay the delta
//   - Purchasing: qty or item differs in a way that suggests vendor-side
//     error — needs the buyer to reconcile
//   - Partial: invoice totals less than PO with no bad lines, just some
//     PO lines missing → vendor shipped partial and will invoice the rest
//     separately. NOT a problem per se, but clerks still want it flagged so
//     they don't post payment thinking the PO is complete
func autoBlockersFromRecon(r recon.Reconciliation) []string {
	var pricing, purchasing bool
	for _, lp := range r.Lines {
		switch lp.Verdict {
		case recon.VerdictPriceMismatch, recon.VerdictExtraOnInvoice:
			pricing = true
		case recon.VerdictQtyMismatch:
			purchasing = true
		case recon.VerdictBothMismatch:
			pricing = true
			purchasing = true
		}
	}

	// Partial-PO detection: total mismatch, every present line matched,
	// and some PO lines have no invoice counterpart. Classic "shipment 1
	// of 2" scenario. We flag it as Partial (not Pricing) so the clerk
	// sees the real reason and doesn't waste time chasing a pricing
	// discrepancy that doesn't exist.
	if !r.TotalMatch && !pricing && !purchasing && r.MissingFromInv > 0 {
		return []string{"Partial"}
	}

	// Total-only discrepancy with no line-level signal AND no missing PO
	// lines to explain it → something in the total we can't attribute.
	// Usually freight/tax that didn't map to a PO line. Flag Pricing so
	// a clerk can investigate.
	if !r.TotalMatch && !pricing && !purchasing {
		pricing = true
	}
	out := []string{}
	if pricing {
		out = append(out, "Pricing")
	}
	if purchasing {
		out = append(out, "Purchasing")
	}
	return out
}

// applyAutoBlockers returns a new category list with the given Blocker: tags
// added (duplicates skipped) and Status: New replaced with Status: Blocked.
// All other categories are preserved as-is.
func applyAutoBlockers(current []string, blockers []string) []string {
	out := make([]string, 0, len(current)+len(blockers))
	seenBlocker := map[string]bool{}
	for _, c := range current {
		if c == statusNewCategory {
			out = append(out, statusBlockedCategory)
			continue
		}
		if strings.HasPrefix(c, blockerCatPrefix) {
			seenBlocker[strings.TrimPrefix(c, blockerCatPrefix)] = true
		}
		out = append(out, c)
	}
	for _, b := range blockers {
		if seenBlocker[b] {
			continue
		}
		out = append(out, blockerCatPrefix+b)
	}
	return out
}

// sameCats is a set-equality check (order-insensitive). Used to skip a
// no-op PATCH when auto-blocker merges don't change anything.
func sameCats(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[string]int{}
	for _, x := range a {
		m[x]++
	}
	for _, x := range b {
		m[x]--
	}
	for _, n := range m {
		if n != 0 {
			return false
		}
	}
	return true
}

// openExtract is the legacy open-ended extraction fallback, used when the ERP
// has no po_line data for the claimed PO (e.g., PO was never entered or
// invoice references a non-existent PO). Less reliable than verify but better
// than nothing — produces an InvoiceData snapshot for the UI with no recon.
func openExtract(cacheDB *cache.Cache, vc *aiclass.Client, mailbox string, m graph.Message, poNo int64, pdfName, pdfText, textSource string, png []byte, start time.Time, vendor string) {
	storeErr := func(msg string) {
		storeExtractionErr(cacheDB, mailbox, m, vc, poNo, msg, start)
	}

	// Text-first: if the PDF had a text layer, try that before vision. We
	// require at least one line item to count the text path as "success" —
	// a stub result means the model couldn't parse it and vision should
	// have a shot.
	var data *aiclass.InvoiceData
	usedModel := vc.Model()
	if pdfText != "" {
		textCtx, textCancel := context.WithTimeout(context.Background(), aiclass.VisionTimeout)
		td, terr := vc.ExtractInvoiceDataFromText(textCtx, pdfText)
		textCancel()
		if terr == nil && td != nil && len(td.Lines) > 0 {
			data = td
			usedModel = "text(" + textSource + "):" + vc.Model()
		}
	}

	if data == nil {
		ctx, cancel := context.WithTimeout(context.Background(), aiclass.VisionTimeout*2)
		defer cancel()
		d, err := vc.ExtractInvoiceData(ctx, png, vendor)
		if err != nil {
			storeErr("vision extract: " + err.Error())
			return
		}
		data = d
	}

	storeCtx, storeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer storeCancel()
	cached := &cache.InvoiceData{
		PONumber:      data.PONumber,
		InvoiceNumber: data.InvoiceNumber,
		InvoiceDate:   data.InvoiceDate,
		InvoiceTotal:  data.InvoiceTotal,
	}
	for _, l := range data.Lines {
		cached.Lines = append(cached.Lines, cache.InvoiceLine{
			ItemID: l.ItemID, Description: l.Description,
			Qty: l.Qty, UnitPrice: l.UnitPrice, Extended: l.Extended,
		})
	}
	// openExtract couldn't reconcile (no PO lines in the ERP), so always flag for rescan —
	// either the ERP gets the PO entered later and a rescan succeeds, or a clerk reviews.
	if err := cacheDB.StoreInvoiceExtraction(storeCtx, mailbox, m.ID, usedModel, poNo, cached, "", time.Since(start), true); err != nil {
		fmt.Printf("           extract-cache store failed: %v\n", err)
		return
	}
	fmt.Printf("           extract (no PO in the ERP, flagged for rescan): %q → %d lines, total=%.2f in %s [%s]\n",
		pdfName, len(cached.Lines), cached.InvoiceTotal, time.Since(start).Round(time.Second), usedModel)
}

// matchBadgeFor returns the display badge, preferring the PO label if the
// worker promoted via PO lookup, otherwise the resolver's match type.
func matchBadgeFor(label string, fallback vendors.MatchType) string {
	if strings.Contains(label, "ai-pdf:") {
		return "[AI-PDF]"
	}
	if strings.Contains(label, "pdf:") {
		return "[PDF-PO]"
	}
	if strings.HasPrefix(label, "po:") {
		return "[PO]"
	}
	return matchBadge(fallback)
}

// filterPOsValidInERP returns only the PO numbers that actually exist in the ERP's
// po_hdr table. Called after PDF/AI extraction produces candidates; prevents
// the worker from tagging a message with a vendor's internal reference number
// that happens to be 7 digits. 2-second per-PO timeout.
func filterPOsValidInERP(erpc *erp.Client, candidates []int64) []int64 {
	out := make([]int64, 0, len(candidates))
	for _, po := range candidates {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		info, err := erpc.LookupPO(ctx, po)
		cancel()
		if err == nil && info != nil {
			out = append(out, po)
		}
	}
	return out
}

// isPDF returns true for attachments that should be scanned for text extraction.
// Checks content-type primarily, with a filename extension fallback because
// Graph sometimes returns generic types.
// fetchAttachmentBytes reads an attachment's bytes, preferring the local
// blobstore (populated by the web-server's sync pass) and falling back to
// Graph only when the blob is missing. The Graph path is the old behavior;
// the blobstore path saves a round-trip per extraction pass and keeps the
// worker running through Graph outages on previously-seen attachments.
// Returns the bytes + a label ("local" or "graph") for logging.
func fetchAttachmentBytes(mailbox, messageID, attachmentID string, blob *blobstore.Store, gc *graph.Client, cacheDB *cache.Cache) ([]byte, string, error) {
	// Local first.
	if blob != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		atts, _ := cacheDB.ListMessageAttachments(ctx, mailbox, messageID)
		cancel()
		for _, a := range atts {
			if a.AttachmentID != attachmentID || a.BlobSHA == "" {
				continue
			}
			if !blob.Has(a.BlobSHA) {
				break // row says we have it but disk says otherwise — fall through to Graph
			}
			rc, err := blob.Read(a.BlobSHA)
			if err != nil {
				break
			}
			defer rc.Close()
			data, err := io.ReadAll(io.LimitReader(rc, pdftext.MaxSize+1))
			if err == nil && len(data) > 0 {
				return data, "local", nil
			}
			break
		}
	}
	// Graph fallback — same code path that's always existed.
	rdr, _, _, err := gc.FetchAttachmentContent(mailbox, messageID, attachmentID)
	if err != nil {
		return nil, "graph", err
	}
	defer rdr.Close()
	data, err := io.ReadAll(io.LimitReader(rdr, pdftext.MaxSize+1))
	if err != nil {
		return nil, "graph", err
	}
	return data, "graph", nil
}

func isPDF(contentType, name string) bool {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	if strings.HasPrefix(ct, "application/pdf") || ct == "application/x-pdf" {
		return true
	}
	return strings.HasSuffix(strings.ToLower(name), ".pdf")
}

// nonInvoiceFormPatterns matches filenames that we know in advance will
// never contain a Acme Distribution PO: W-9s, vendor/customer setup forms, state
// tax-exempt certificates. Skipping these at the PDF-scan stage avoids
// burning 30-60s per file on vision calls that always come back empty.
//
// Keep conservative — false negatives (missed non-invoices) cost a wasted
// vision call. False positives (a real invoice mistakenly skipped) cost
// an untagged message. We'd rather over-process than under-tag.
var nonInvoiceFormPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bw[-_ ]?9\b`),                            // W-9, W9, W 9
	regexp.MustCompile(`(?i)\b(new[\s_-]+)?(vendor|customer)([\s_-]+(setup|information))?[\s_-]+form\b`),
	regexp.MustCompile(`(?i)\btax[\s_-]+exempt\b|\bresale[\s_-]+certificate\b`),
	regexp.MustCompile(`(?i)\b(virginia|state|form)[\s_-]+(form[\s_-]+)?st[-_]\d+\b`), // state sales-tax cert forms
	regexp.MustCompile(`(?i)\bCustomer[\s_-]+Setup[\s_-]+Form\b`),
}

// isNonInvoiceFormName returns true if the filename matches any known
// non-invoice pattern. Only checks filename — we don't read the file.
func isNonInvoiceFormName(name string) bool {
	for _, re := range nonInvoiceFormPatterns {
		if re.MatchString(name) {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
