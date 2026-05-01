// Package ui contains view-model types and helpers for the Dispatch web UI.
// Keeps category parsing and filter logic out of the HTTP handlers.
package ui

import (
	"sort"
	"strings"
	"time"

	"dispatch/internal/graph"
	"dispatch/internal/vendors"
)

type ViewMessage struct {
	ID             string
	ConversationID string // Graph's conversationId — used to collapse replies
	Subject        string
	Sender         string
	SenderName     string
	Received       time.Time
	Vendor         string
	Owner          string
	Buyer          string // P21 PO created_by, lowercased
	Status         string // "", "New", "In Progress", "Blocked", "Done"
	Kind           string // AI-assigned: Invoice, Payment, Marketing, Webinar, etc.
	Blockers       []string
	Internal       bool // sender is @example.com — show subdued, no Vendor expected
	HasAttachments bool // Graph has_attachments flag — surface a paperclip in the list
	WebLink        string
	RawCats        []string
	// ConversationSize is how many messages in the cache share this
	// ConversationID. Populated by the web layer's dedupe pass — if >1,
	// the row is the representative (newest) and shows a "+N" badge.
	ConversationSize int

	// AI pipeline status, set by the web handler from cache. Zero values mean
	// "not processed yet." Used by the row template for a small indicator.
	AIProcessed       bool
	AIHasExtraction   bool
	AIHasReconcile    bool
	AITotalMatch      bool
	AIAnyLineMismatch bool
	AIErrorMsg        string
	AINeedsRescan     bool // flagged in cache as "not a clean match" — surfaced in Rescan filter

	// Voucher tracking from the P21 sync. Empty until the sync ticks.
	PayStatus string // "" | "unposted" | "posted" | "paid"
	VoucherNo string

	// PONo is the resolved P21 PO number for this message's invoice. 0 when no
	// PO matched. Populated from invoice_extractions.po_no during hydration.
	// Used by the row template's clickable PO badge and the ?po= filter.
	PONo int64

	// NoteCount is the number of clerk notes attached to this message.
	// Drives a small 💬 indicator on the list row when > 0.
	NoteCount int

	// InvoiceAmount is the dollar value extracted from the invoice (when
	// available). 0 when no extraction yet. Used for queue-total
	// computation and per-row dollar display.
	InvoiceAmount float64
}

// AIStatus returns a short tag for the row indicator template. "Processed"
// by any automation — deterministic OR AI — so every worker-touched row
// surfaces the mark. Priority: error > issue > clean > pending > classified > tagged.
//
//	""           - worker hasn't touched this message yet
//	"tagged"     - worker ran (Vendor/Status set) via deterministic path (regex, P21)
//	"classified" - Kind: set by LLM classifier (vendor mail with no PO)
//	"pending"    - AI extraction done, reconciliation hasn't run yet
//	"clean"      - AI extraction + reconciliation says all matches
//	"issue"      - AI extraction + reconciliation found discrepancy
//	"error"      - AI extraction failed
func (v ViewMessage) AIStatus() string {
	if v.AIErrorMsg != "" {
		return "error"
	}
	if v.AIProcessed && v.AIHasReconcile {
		if v.AITotalMatch && !v.AIAnyLineMismatch {
			return "clean"
		}
		return "issue"
	}
	if v.AIProcessed {
		return "pending"
	}
	if v.Kind != "" {
		return "classified"
	}
	// Any Status at all means the worker tagged it via a deterministic path
	// (sender domain, PO regex, P21 lookup). That's still "processed."
	if v.Status != "" {
		return "tagged"
	}
	return ""
}

// Pipeline returns a short tag describing which PROCESSING STAGE a message
// is currently in (distinct from AIStatus, which is the business verdict).
// Derived from existing DB fields — no new schema needed. Drives the
// pipeline-badge column in /list + the header badge in /detail.
//
//	""          — sort pending (no extraction row yet)
//	"sorted"    — sort done (Vendor/Status tagged) but extraction hasn't run
//	"extracting"— extraction in progress (queued stub or started-but-unfinished)
//	"rescan"    — has extraction but flagged for rescan at next tier
//	"done"      — clean match, no further action needed
//	"exhausted" — reached top tier, never got a clean match — clerk's call
//	"error"     — extraction errored terminally
func (v ViewMessage) Pipeline() string {
	if v.AIErrorMsg != "" {
		return "error"
	}
	if !v.AIHasExtraction {
		if v.Vendor != "" || v.Status != "" {
			return "sorted"
		}
		return ""
	}
	// Has an extraction row.
	if v.AINeedsRescan {
		return "rescan"
	}
	if v.AIHasReconcile && v.AITotalMatch && !v.AIAnyLineMismatch && len(v.Blockers) == 0 {
		return "done"
	}
	// Extraction row exists, needs_rescan=0, but something's off — that's
	// the tier-4 exhaustion case the worker marks when it gives up climbing.
	return "exhausted"
}

// HiddenByDefault reports whether the message's Kind is one we filter out of
// the main queue (marketing, webinar invites, newsletters).
func (v ViewMessage) HiddenByDefault() bool {
	return HideByDefaultKinds[v.Kind]
}

// StatusOptions lists the user-selectable Status values shown in the dropdown
// menus. "Done" is intentionally excluded — it's now system-derived: voucher
// sync flips Status: Done once the AP voucher is posted in P21. A clerk who
// finishes work without posting (write-off, vendor-cancelled) should use
// Blocker: Won't Pay instead. See "system-derived Done" in todo.md.
var StatusOptions = []string{"New", "In Progress", "Blocked"}

// allValidStatuses is the full set the system accepts, including the
// system-only "Done" (which never appears in StatusOptions). Used by
// ValidStatus to gate handleStatus / system writes.
var allValidStatuses = []string{"New", "In Progress", "Blocked", "Done"}

// BlockerOptions lists the allowed Blocker values. Multi-valued per message.
// Partial is auto-set when the invoice totals less than the PO only because
// some PO lines are missing — clerks still want this visible, just not as a
// "something's wrong" Pricing flag.
//
// "Won't Pay" is the explicit "we're closing this without posting" exit:
// duplicates, write-offs, vendor-cancelled invoices. Without it, system-
// derived Done (which depends on a posted voucher) would leave these stuck.
var BlockerOptions = []string{"Purchasing", "Vendor", "Pricing", "PO", "Partial", "Won't Pay"}

// HasBlocker reports whether the given blocker name is currently set.
func (v ViewMessage) HasBlocker(name string) bool {
	for _, b := range v.Blockers {
		if strings.EqualFold(b, name) {
			return true
		}
	}
	return false
}

// MineIfOwner reports whether the current user owns this message.
func (v ViewMessage) MineIfOwner(user string) bool {
	return v.Owner != "" && strings.EqualFold(v.Owner, user)
}

const (
	vendorPrefix  = "Vendor: "
	ownerPrefix   = "Owner: "
	statusPrefix  = "Status: "
	blockerPrefix = "Blocker: "
	buyerPrefix   = "Buyer: "
	kindPrefix    = "Kind: "
)

// HideByDefaultKinds is the set of AI-assigned Kinds filtered out of the
// default AP queue. Users review them via their dedicated filter tabs and
// can rescue false positives by clearing the Kind category.
var HideByDefaultKinds = map[string]bool{
	"Marketing":  true,
	"Webinar":    true,
	"Newsletter": true,
	"Payment":    true, // vendor "payment scheduled" acks — informational, no action required
}

// marketingKinds are noise-y vendor communication types — shown under the
// "Marketing" filter tab only.
var marketingKinds = map[string]bool{
	"Marketing":  true,
	"Webinar":    true,
	"Newsletter": true,
}

// paymentKinds are AP payment-event notifications (vendor ack that our payment
// is scheduled/processed). Shown under the "Payments" filter tab.
var paymentKinds = map[string]bool{
	"Payment": true,
}

func FromGraph(m graph.Message) ViewMessage {
	v := ViewMessage{
		ID:             m.ID,
		ConversationID: m.ConversationID,
		Subject:        m.Subject,
		Sender:         m.SenderAddress(),
		SenderName:     m.SenderName(),
		WebLink:        m.WebLink,
		HasAttachments: m.HasAttachments,
		RawCats:        append([]string{}, m.Categories...),
	}
	if t, err := time.Parse(time.RFC3339, m.ReceivedDateTime); err == nil {
		v.Received = t
	}
	if vendors.Classify(v.Sender) == vendors.ClassInternal {
		v.Internal = true
	}
	for _, c := range m.Categories {
		switch {
		case strings.HasPrefix(c, vendorPrefix):
			v.Vendor = strings.TrimPrefix(c, vendorPrefix)
		case strings.HasPrefix(c, ownerPrefix):
			v.Owner = strings.TrimPrefix(c, ownerPrefix)
		case strings.HasPrefix(c, statusPrefix):
			v.Status = strings.TrimPrefix(c, statusPrefix)
		case strings.HasPrefix(c, blockerPrefix):
			v.Blockers = append(v.Blockers, strings.TrimPrefix(c, blockerPrefix))
		case strings.HasPrefix(c, buyerPrefix):
			v.Buyer = strings.TrimPrefix(c, buyerPrefix)
		case strings.HasPrefix(c, kindPrefix):
			v.Kind = strings.TrimPrefix(c, kindPrefix)
		}
	}
	return v
}

// Filter is the set of supported filter names. Kept small; add more later.
type Filter string

const (
	FilterOpen         Filter = "open"
	FilterUnclaimed    Filter = "unclaimed"
	FilterMine         Filter = "mine"
	FilterMyBuyer      Filter = "mybuyer"   // messages whose Buyer matches currentUser
	FilterBlocked      Filter = "blocked"
	FilterDone         Filter = "done"
	FilterMarketing    Filter = "marketing" // AI-flagged marketing/webinar/newsletter
	FilterPayments     Filter = "payments"  // AI-flagged payment-scheduled notifications
	FilterUnposted     Filter = "unposted"  // extraction succeeded but no P21 voucher yet — actionable for AP
	FilterRescan       Filter = "rescan"    // AI processed but not a clean match — needs review / retry
	FilterMatch        Filter = "match"     // invoice reconciled cleanly, safe to post
	FilterDiscrepancy  Filter = "discrepancy" // extraction found a discrepancy (any blocker)
	FilterAll          Filter = "all"
)

func (f Filter) Label() string {
	switch f {
	case FilterUnclaimed:
		return "Unclaimed"
	case FilterMine:
		return "Mine"
	case FilterMyBuyer:
		return "My POs"
	case FilterBlocked:
		return "Blocked"
	case FilterDone:
		return "Completed"
	case FilterMarketing:
		return "Marketing"
	case FilterPayments:
		return "Payments"
	case FilterUnposted:
		return "Unposted"
	case FilterRescan:
		return "Rescan"
	case FilterMatch:
		return "Matched"
	case FilterDiscrepancy:
		return "Discrepancy"
	case FilterAll:
		return "All"
	default:
		return "Open"
	}
}

// Apply filters and sorts msgs. Newest first. Internal cross-talk is kept visible
// under all filters (it's workflow signal, not noise) unless the filter itself
// excludes it (e.g., Mine/Unclaimed filter on Vendor-class only).
func Apply(msgs []ViewMessage, f Filter, currentUser string) []ViewMessage {
	user := strings.ToLower(strings.TrimSpace(currentUser))
	cutoff := time.Now().AddDate(0, 0, -30)

	out := make([]ViewMessage, 0, len(msgs))
	for _, m := range msgs {
		// Default work filters hide AI-flagged marketing/webinars — Option 2
		// in the design: don't hide if message has no AI verdict yet (safer).
		hiddenByAI := m.HiddenByDefault()
		switch f {
		case FilterOpen:
			if m.Status != "Done" && !hiddenByAI {
				out = append(out, m)
			}
		case FilterUnclaimed:
			if m.Status != "Done" && m.Owner == "" && !m.Internal && !hiddenByAI {
				out = append(out, m)
			}
		case FilterMine:
			if strings.EqualFold(m.Owner, user) && m.Status != "Done" && !hiddenByAI {
				out = append(out, m)
			}
		case FilterMyBuyer:
			if strings.EqualFold(m.Buyer, user) && m.Status != "Done" && !hiddenByAI {
				out = append(out, m)
			}
		case FilterBlocked:
			if m.Status == "Blocked" && !hiddenByAI {
				out = append(out, m)
			}
		case FilterDone:
			if m.Status == "Done" && m.Received.After(cutoff) {
				out = append(out, m)
			}
		case FilterMarketing:
			if marketingKinds[m.Kind] {
				out = append(out, m)
			}
		case FilterPayments:
			if paymentKinds[m.Kind] {
				out = append(out, m)
			}
		case FilterUnposted:
			if m.AIHasExtraction && m.PayStatus == "unposted" {
				out = append(out, m)
			}
		case FilterRescan:
			if m.AINeedsRescan {
				out = append(out, m)
			}
		case FilterMatch:
			// Invoice was extracted, reconciled cleanly, no blockers,
			// no rescan pending. Safe to post.
			if m.AIHasExtraction && !m.AINeedsRescan && len(m.Blockers) == 0 {
				out = append(out, m)
			}
		case FilterDiscrepancy:
			// Invoice was extracted but has a discrepancy. Either needs
			// rescan (model flagged it) or has an auto-blocker set.
			if m.AIHasExtraction && (m.AINeedsRescan || len(m.Blockers) > 0) {
				out = append(out, m)
			}
		case FilterAll:
			out = append(out, m)
		default:
			if m.Status != "Done" && !hiddenByAI {
				out = append(out, m)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Received.After(out[j].Received)
	})
	return out
}
