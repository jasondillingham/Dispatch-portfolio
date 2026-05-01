// extract_review is the bulk verdict-capture surface. PDF on the left, the
// AI's extracted fields prefilled in editable inputs on the right, three
// buttons that record a verdict and auto-advance. Built on top of the same
// detailData + cache.RecordVerdict primitives that Phase 1 introduced — the
// goal is to let one clerk plow through a sitting backlog of extractions in
// minutes and leave the verdict corpus warm for Phase 2/3.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"dispatch/internal/ui"
)

// extractReviewData drives extract-review.html.
type extractReviewData struct {
	detailData
	// Queue position
	Index int
	Total int
	// URLs preserve the unverifiedOnly flag across nav
	PrevURL  string
	NextURL  string
	SelfURL  string // current page URL (for forms that don't auto-advance)
	// Filter toggle
	UnverifiedOnly bool
	UnverifiedURL  string
	AllURL         string
	// Counts shown in header
	TotalExtractions int
	UnverifiedCount  int
	// Prefill values pulled out of detailData.Extraction.Data so the template
	// doesn't have to nil-check on every row.
	FillPONumber      string
	FillInvoiceNumber string
	FillInvoiceDate   string
	FillInvoiceTotal  string
}

// handleExtractReview renders /extract-review — a focused, keyboard-friendly
// PDF + prefilled-fields surface for plowing through the extraction backlog.
// Default: only messages with successful extractions that the current user
// hasn't verdicted yet. Use ?all=1 to also show already-verdicted ones (e.g.
// to revisit after spotting a pattern).
func (s *server) handleExtractReview(w http.ResponseWriter, r *http.Request) {
	user := s.effectiveUser(r)
	unverifiedOnly := r.URL.Query().Get("all") != "1"

	idx := 0
	if v := r.URL.Query().Get("index"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			idx = n
		}
	}

	all, err := s.fetchMessages()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	// First filter: only messages with a successful AI extraction. AINeedsRescan
	// is fine — those still have data, the clerk can verdict them. Errored
	// extractions are skipped (no fields to prefill).
	withExtraction := make([]ui.ViewMessage, 0, len(all))
	for _, m := range all {
		if m.AIHasExtraction && m.AIErrorMsg == "" {
			withExtraction = append(withExtraction, m)
		}
	}
	totalExtractions := len(withExtraction)

	// Second filter: subtract messages the user already verdicted, unless ?all=1.
	verdictSet, _ := s.cache.UserVerdictedMessageIDs(r.Context(), s.mailbox, user)
	unverifiedCount := 0
	for _, m := range withExtraction {
		if !verdictSet[m.ID] {
			unverifiedCount++
		}
	}
	queue := withExtraction
	if unverifiedOnly {
		queue = make([]ui.ViewMessage, 0, unverifiedCount)
		for _, m := range withExtraction {
			if !verdictSet[m.ID] {
				queue = append(queue, m)
			}
		}
	}

	// Empty / past-end → render the "all done" screen with the toggle so the
	// clerk can flip to ?all=1 if they want a second pass.
	if len(queue) == 0 || idx >= len(queue) {
		empty := map[string]any{
			"UnverifiedOnly":   unverifiedOnly,
			"TotalExtractions": totalExtractions,
			"UnverifiedCount":  unverifiedCount,
			"User":             user,
		}
		if err := s.tmpl.ExecuteTemplate(w, "extract-review-empty.html", empty); err != nil {
			log.Printf("render extract-review-empty: %v", err)
		}
		return
	}

	current := queue[idx]
	detail, err := s.buildDetailData(r.Context(), current.ID, user)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	detail.IsReview = true // suppresses the close button on shared partials

	// Build URLs — the unverified flag has to ride along with every nav link
	// or the queue jumps around when the page reloads after a verdict.
	queryFor := func(idx int) string {
		q := fmt.Sprintf("index=%d", idx)
		if !unverifiedOnly {
			q += "&all=1"
		}
		return q
	}
	prevURL := ""
	if idx > 0 {
		prevURL = "/extract-review?" + queryFor(idx-1)
	}
	nextURL := "/extract-review?" + queryFor(idx+1)
	selfURL := "/extract-review?" + queryFor(idx)

	data := extractReviewData{
		detailData:       detail,
		Index:            idx,
		Total:            len(queue),
		PrevURL:          prevURL,
		NextURL:          nextURL,
		SelfURL:          selfURL,
		UnverifiedOnly:   unverifiedOnly,
		UnverifiedURL:    "/extract-review",
		AllURL:           "/extract-review?all=1",
		TotalExtractions: totalExtractions,
		UnverifiedCount:  unverifiedCount,
	}

	if detail.Extraction != nil && detail.Extraction.Data != nil {
		d := detail.Extraction.Data
		data.FillPONumber = d.PONumber
		data.FillInvoiceNumber = d.InvoiceNumber
		data.FillInvoiceDate = d.InvoiceDate
		if d.InvoiceTotal != 0 {
			data.FillInvoiceTotal = strconv.FormatFloat(d.InvoiceTotal, 'f', 2, 64)
		}
	}

	if err := s.tmpl.ExecuteTemplate(w, "extract-review.html", data); err != nil {
		log.Printf("render extract-review: %v", err)
	}
}

// handleExtractReviewVerdict records the clerk verdict from /extract-review
// and 303s to the next index. Reuses cache.RecordVerdict (Phase 1) — this
// handler is just the queue-advancing wrapper. Form fields:
//
//	verdict        — "right" | "wrong" | "corrected"
//	po_number, invoice_number, invoice_date, invoice_total, notes — prefilled
//	                 from the extraction; only persisted when verdict=corrected
//	next_index     — the next queue position to land on (template computes it
//	                 based on unverifiedOnly mode)
//	all            — "1" if the page is in include-already-verdicted mode
//
// Skip is a plain link to NextURL — doesn't pass through this handler.
func (s *server) handleExtractReviewVerdict(w http.ResponseWriter, r *http.Request) {
	msgID, err := decodeRowID(chi.URLParam(r, "rowID"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	verdict := strings.TrimSpace(r.FormValue("verdict"))
	switch verdict {
	case "right", "wrong", "corrected":
	default:
		http.Error(w, "bad verdict", http.StatusBadRequest)
		return
	}

	correctedJSON := ""
	if verdict == "corrected" {
		payload := map[string]string{
			"po_number":      strings.TrimSpace(r.FormValue("po_number")),
			"invoice_number": strings.TrimSpace(r.FormValue("invoice_number")),
			"invoice_date":   strings.TrimSpace(r.FormValue("invoice_date")),
			"invoice_total":  strings.TrimSpace(r.FormValue("invoice_total")),
			"notes":          strings.TrimSpace(r.FormValue("notes")),
		}
		if b, err := json.Marshal(payload); err == nil {
			correctedJSON = string(b)
		}
	}
	user := s.effectiveUser(r)
	if err := s.cache.RecordVerdict(r.Context(), s.mailbox, msgID, user, verdict, correctedJSON); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Advance. Default: same index — when unverifiedOnly is on, the row we
	// just verdicted falls out of the queue and "current index" naturally
	// becomes the next message. When ?all=1 is on, we increment because the
	// row stays in the queue. Template tells us via next_index.
	nextIdx := r.FormValue("next_index")
	if nextIdx == "" {
		nextIdx = "0"
	}
	target := "/extract-review?index=" + nextIdx
	if r.FormValue("all") == "1" {
		target += "&all=1"
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}
