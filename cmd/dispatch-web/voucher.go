// voucher.go — the ERP voucher status sync. Background goroutine that polls
// apinv_hdr for every extraction with a PO + invoice number, writes voucher_no
// + pay_status + check info back to cache, and patches Status:Done in Outlook
// when a voucher posts. Plus the Status:Blocked recheck and Status:Done-vs-
// unposted reconcile passes that fire alongside it.

package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"dispatch/internal/cache"
	"dispatch/internal/graph"
	"dispatch/internal/erp"
	"dispatch/internal/recon"
	"dispatch/internal/ui"

	"github.com/go-chi/chi/v5"
)


// runVoucherSync polls the ERP for apinv_hdr status on every extraction that has
// a PO and an invoice number. Writes voucher_no + pay_status + check info back
// to the cache. One tick = one batch of ≤ 50 candidates, so a full backlog
// drains over a few cycles without hammering the ERP.
//
// As of system-derived Done (2026-04-27): when a row's pay_status transitions
// to posted/paid, the sync also patches Outlook categories to add Status: Done.
// Clerks no longer set Done manually — the voucher posting in the ERP *is* the
// "I'm finished" signal. gc may be nil; if so, the cache write happens but the
// Outlook category isn't pushed back (acceptable: next /detail visit reflects
// pay_status anyway).
func runVoucherSync(erpc *erp.Client, cacheDB *cache.Cache, gc *graph.Client, mailbox string, interval time.Duration) {
	doVoucherSync(erpc, cacheDB, gc, mailbox, interval)
	t := time.NewTicker(interval)
	defer t.Stop()
	for range t.C {
		doVoucherSync(erpc, cacheDB, gc, mailbox, interval)
	}
}

func doVoucherSync(erpc *erp.Client, cacheDB *cache.Cache, gc *graph.Client, mailbox string, interval time.Duration) {
	// Reconcile Done+unposted contradictions before the main sync runs.
	// These are rows where the Outlook tag says Status:Done but the ERP reports
	// no posted voucher — a stale manual Done from before system-derived
	// Done shipped, or a reversed voucher. Clear Status:Done so the row
	// re-enters the queue. Cheap to run every tick.
	reconcileDoneUnposted(cacheDB, gc, mailbox)

	// Recheck Status:Blocked rows: if the buyer fixed the PO or the vendor
	// re-priced, the recon may now reconcile cleanly. Strip blockers and
	// flip back to Status:New so the row re-enters the queue. Costs one
	// the ERP PO query per blocked row each tick.
	recheckBlocked(erpc, cacheDB, gc, mailbox)

	const batchSize = 50
	staleBefore := time.Now().Add(-interval).UTC()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	cands, err := cacheDB.ListERPSyncCandidates(ctx, mailbox, staleBefore, batchSize)
	cancel()
	if err != nil {
		log.Printf("voucher-sync: list candidates: %v", err)
		return
	}
	if len(cands) == 0 {
		return
	}
	start := time.Now()
	var posted, paid, unposted, errs, autoDone int
	for _, cand := range cands {
		if didAutoDone := syncOneVoucher(erpc, cacheDB, gc, mailbox, cand.MessageID, cand.PONo, cand.InvoiceNumber, &posted, &paid, &unposted, &errs); didAutoDone {
			autoDone++
		}
	}
	log.Printf("voucher-sync: %d checked in %s  paid=%d posted=%d unposted=%d auto-done=%d errs=%d",
		len(cands), time.Since(start).Round(time.Millisecond), paid, posted, unposted, autoDone, errs)
}

// syncOneVoucher does the per-message lookup + write that doVoucherSync runs in
// a loop. Factored out so the per-message recheck endpoint (fired on
// navigation) can reuse the same path. Returns true when this call
// transitioned the message into a Status: Done state (for log accounting).
func syncOneVoucher(erpc *erp.Client, cacheDB *cache.Cache, gc *graph.Client, mailbox, messageID string, poNo int64, invoiceNumber string, posted, paid, unposted, errs *int) bool {
	lookupCtx, lookupCancel := context.WithTimeout(context.Background(), 5*time.Second)
	ap, err := erpc.LookupAPInvoice(lookupCtx, poNo, invoiceNumber)
	lookupCancel()
	if err != nil {
		if errs != nil {
			*errs++
		}
		return false
	}
	var info cache.VoucherInfo
	terminal := false // posted or paid → eligible for auto-Status:Done
	switch {
	case ap == nil:
		info.Status = "unposted"
		if unposted != nil {
			*unposted++
		}
	case ap.PaidInFull:
		info.Status = "paid"
		info.VoucherNo = ap.VoucherNo
		info.PostedAt = ap.InvoiceDate
		info.PaidAt = ap.CheckDate
		info.InvoiceAmount = ap.InvoiceAmount
		info.CheckNo = ap.CheckNo
		if paid != nil {
			*paid++
		}
		terminal = true
	default:
		info.Status = "posted"
		info.VoucherNo = ap.VoucherNo
		info.PostedAt = ap.InvoiceDate
		info.InvoiceAmount = ap.InvoiceAmount
		if posted != nil {
			*posted++
		}
		terminal = true
	}
	writeCtx, writeCancel := context.WithTimeout(context.Background(), 2*time.Second)
	if err := cacheDB.SetVoucherInfo(writeCtx, mailbox, messageID, info); err != nil {
		log.Printf("voucher-sync: write %s: %v", messageID[:min(20, len(messageID))], err)
		if errs != nil {
			*errs++
		}
		writeCancel()
		return false
	}
	writeCancel()

	if !terminal || gc == nil {
		return false
	}
	// Posted/paid → auto-flip Outlook category to Status: Done if not already.
	// Reads cached categories first to avoid a Graph fetch when the row is
	// already marked Done. Falls through silently on any error path; the next
	// sync tick will retry.
	catCtx, catCancel := context.WithTimeout(context.Background(), 2*time.Second)
	cats, err := cacheDB.GetCategories(catCtx, mailbox, messageID)
	catCancel()
	if err != nil {
		return false
	}
	if hasStatusDoneCategory(cats) {
		return false
	}
	newCats := ui.ReplaceStatus(cats, "Done")
	if err := gc.SetCategories(mailbox, messageID, newCats); err != nil {
		log.Printf("voucher-sync: set Status:Done %s: %v", messageID[:min(20, len(messageID))], err)
		return false
	}
	updCtx, updCancel := context.WithTimeout(context.Background(), 2*time.Second)
	_ = cacheDB.UpdateCategories(updCtx, mailbox, messageID, newCats)
	updCancel()
	return true
}

// handleRecheckVoucher fires a single-message voucher sync. Called from the
// browser when a clerk navigates away from a message (review-mode →, split-pane
// ↓/↑) — gives them an "AP person enters voucher in the ERP, hits next, prior
// message disappears from the queue" workflow without waiting for the next
// 10-min bulk sync. Returns 204 on success/no-op; the actual UI update
// happens on the next list refresh (10s) or via SSE later.
//
// Idempotent: looking up an already-posted message just refreshes the cache
// timestamp. Cheap: one indexed query against apinv_hdr.
func (s *server) handleRecheckVoucher(w http.ResponseWriter, r *http.Request) {
	if s.erp == nil {
		// Voucher sync isn't configured. Return 204 so callers don't error.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	msgID, err := decodeRowID(chi.URLParam(r, "rowID"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	// Need PO + invoice number for the the ERP lookup. Read from the cached
	// extraction row; bail silently if either is missing (nothing to recheck).
	extCtx, extCancel := context.WithTimeout(r.Context(), 2*time.Second)
	ext, err := s.cache.GetInvoiceExtraction(extCtx, s.mailbox, msgID)
	extCancel()
	if err != nil || ext == nil || ext.PONo == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if ext.Data == nil || ext.Data.InvoiceNumber == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// Run the same single-message path the bulk sync uses. Counters are nil
	// because there's nothing to log here.
	syncOneVoucher(s.erp, s.cache, s.gc, s.mailbox, msgID, ext.PONo, ext.Data.InvoiceNumber, nil, nil, nil, nil)
	w.WriteHeader(http.StatusNoContent)
}

// recheckBlocked re-runs reconciliation for every Status:Blocked message
// against fresh the ERP PO lines. If the recon now reports clean (every line
// matches, total reconciles, no fee-only discrepancy), strip the blockers
// and downgrade Status to New — the row re-enters the active queue and the
// next worker pass will re-recon to confirm.
//
// Why we run this: a buyer can fix a PO line price after we blocked, or a
// vendor can send a corrected invoice (which on its own creates a new
// message; this catches the case where the corrected PO retroactively
// makes the original invoice clean). Without auto-recheck, blockers sit
// forever even after the underlying issue is resolved.
//
// Conservative: requires TotalMatch AND AnyLineMismatch=false AND no
// fee-only discrepancy. A clerk who explicitly added a Blocker (e.g.,
// "Won't Pay") via the menu won't get auto-cleared because that path
// doesn't run recon — those have non-recon-derived blockers and the
// recheck only clears recon-derived ones if the recon is now clean.
func recheckBlocked(erpc *erp.Client, cacheDB *cache.Cache, gc *graph.Client, mailbox string) {
	if erpc == nil || gc == nil {
		return
	}
	listCtx, listCancel := context.WithTimeout(context.Background(), 3*time.Second)
	rows, err := cacheDB.ListBlockedMessages(listCtx, mailbox)
	listCancel()
	if err != nil {
		log.Printf("blocked-recheck: list: %v", err)
		return
	}
	if len(rows) == 0 {
		return
	}
	cleared := 0
	for _, br := range rows {
		// Re-fetch PO lines from the ERP. If the lookup fails or returns nothing,
		// don't clear — preserve the existing Blocked state.
		poCtx, poCancel := context.WithTimeout(context.Background(), 5*time.Second)
		poLines, err := erpc.ListPOLines(poCtx, br.PONo)
		poCancel()
		if err != nil || len(poLines) == 0 {
			continue
		}
		r := recon.Compare(br.PONo, br.InvoiceData, poLines)
		if !r.TotalMatch || r.AnyLineMismatch {
			continue
		}
		// Now-clean. Strip blockers + downgrade Status.
		catCtx, catCancel := context.WithTimeout(context.Background(), 2*time.Second)
		cats, err := cacheDB.GetCategories(catCtx, mailbox, br.MessageID)
		catCancel()
		if err != nil {
			continue
		}
		newCats := ui.StripAllBlockers(cats)
		if len(newCats) == len(cats) {
			continue // nothing to clear (no Blocker: entries — racy)
		}
		if err := gc.SetCategories(mailbox, br.MessageID, newCats); err != nil {
			log.Printf("blocked-recheck: set %s: %v", br.MessageID[:min(20, len(br.MessageID))], err)
			continue
		}
		updCtx, updCancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = cacheDB.UpdateCategories(updCtx, mailbox, br.MessageID, newCats)
		updCancel()
		// Refresh the cached recon snapshot so the UI reflects the new
		// state without waiting for the next worker pass.
		reconJSON, _ := json.Marshal(r)
		poLinesJSON, _ := json.Marshal(poLines)
		recCtx, recCancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = cacheDB.StoreReconciliation(recCtx, mailbox, br.MessageID, string(poLinesJSON), string(reconJSON))
		recCancel()
		cleared++
	}
	if cleared > 0 {
		log.Printf("blocked-recheck: re-recon'd %d row(s), unblocked %d (PO updates resolved discrepancy)", len(rows), cleared)
	}
}

// reconcileDoneUnposted strips Status:Done from any cached message where the
// the ERP lookup came back unposted. Per-row work: GetCategories → drop Done →
// add New → SetCategories on Graph → UpdateCategories in cache. Bounded
// (cache query is filtered + indexed); typically nothing to do once the
// initial backlog is cleared.
func reconcileDoneUnposted(cacheDB *cache.Cache, gc *graph.Client, mailbox string) {
	if gc == nil {
		return
	}
	listCtx, listCancel := context.WithTimeout(context.Background(), 3*time.Second)
	ids, err := cacheDB.ListDoneUnpostedConflicts(listCtx, mailbox)
	listCancel()
	if err != nil {
		log.Printf("done-unposted reconcile: list: %v", err)
		return
	}
	if len(ids) == 0 {
		return
	}
	fixed := 0
	for _, id := range ids {
		catCtx, catCancel := context.WithTimeout(context.Background(), 2*time.Second)
		cats, err := cacheDB.GetCategories(catCtx, mailbox, id)
		catCancel()
		if err != nil {
			continue
		}
		if !hasStatusDoneCategory(cats) {
			continue // raced with another reconciler — no longer Done
		}
		newCats := ui.ReplaceStatus(cats, "New")
		if err := gc.SetCategories(mailbox, id, newCats); err != nil {
			log.Printf("done-unposted reconcile: set %s: %v", id[:min(20, len(id))], err)
			continue
		}
		updCtx, updCancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = cacheDB.UpdateCategories(updCtx, mailbox, id, newCats)
		updCancel()
		fixed++
	}
	if fixed > 0 {
		log.Printf("done-unposted reconcile: cleared Status:Done on %d row(s) (the ERP said unposted)", fixed)
	}
}
