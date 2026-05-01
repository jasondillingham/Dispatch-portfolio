// ap.go — AP-mode handlers. Clerk-first surface with the four-bucket queue
// (Unassigned / Todo / Waiting / Done) and Pickup / Assign / Hold / Skip
// decision flow. Reuses detailData from main.go for body/recon/notes/etc.
// Routes: /ap (queue), /ap/pickup, /ap/assign, /ap/hold, /ap/skip, /ap/note.

package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"dispatch/internal/p21"
	"dispatch/internal/ui"

	"github.com/go-chi/chi/v5"
)


// apFilter is the narrowed set of tabs shown in AP mode. Buckets match the
// clerk's mental model: pickup pile (Unassigned), my pile (Todo), waiting
// on someone (Hold), posted (Done).
type apFilter string

const (
	apFilterUnassigned apFilter = "unassigned" // Owner empty, not Done, not Blocked — pickup pile
	apFilterTodo       apFilter = "todo"       // Owner == me, not Done, not Blocked — my work
	apFilterHold       apFilter = "hold"       // Status: Blocked — issue raised, awaiting reply
	apFilterDone       apFilter = "done"       // Status: Done AND received today — posted
)

func parseAPFilter(s string) apFilter {
	switch apFilter(strings.ToLower(strings.TrimSpace(s))) {
	case apFilterUnassigned:
		return apFilterUnassigned
	case apFilterHold:
		return apFilterHold
	case apFilterDone:
		return apFilterDone
	}
	return apFilterTodo
}

// applyAPFilter narrows a message list by the AP-mode tab. Mirrors what the
// existing ui.Apply does for admin filters; kept separate because the AP set
// is intentionally different (no Mine, no Marketing, no Unposted).
//
// user is the effective AP user (impersonation-aware) — used to split the
// pickup pile (Unassigned) from "my work" (Todo).
func applyAPFilter(msgs []ui.ViewMessage, f apFilter, user string) []ui.ViewMessage {
	out := make([]ui.ViewMessage, 0, len(msgs))
	startOfToday := time.Now().UTC().Truncate(24 * time.Hour)
	for _, m := range msgs {
		// Hide automation / marketing / etc — clerks shouldn't see these
		// in any AP-mode tab. The reclassify button lives in the More menu
		// for the rare false-positive recovery case.
		if m.HiddenByDefault() {
			continue
		}
		// Internal threads with no resolved vendor are noise to AP — they're
		// internal chatter, not actionable invoices.
		if m.Internal && m.Vendor == "" {
			continue
		}
		switch f {
		case apFilterUnassigned:
			// Hide rows the AI hasn't classified yet. Pre-classification mail
			// is noise: many will turn out to be Marketing/Internal that
			// shouldn't be in the queue at all. Once the worker classifies
			// them (~ms with the deterministic shortcut, ~1.5s with AI),
			// they appear here naturally if they're real invoices to pick up.
			if m.Kind == "" {
				continue
			}
			if m.Status != "Done" && m.Status != "Blocked" && m.Owner == "" {
				out = append(out, m)
			}
		case apFilterTodo:
			if m.Status != "Done" && m.Status != "Blocked" && m.MineIfOwner(user) {
				out = append(out, m)
			}
		case apFilterHold:
			if m.Status == "Blocked" {
				out = append(out, m)
			}
		case apFilterDone:
			if m.Status == "Done" && m.Received.After(startOfToday) {
				out = append(out, m)
			}
		}
	}
	return out
}

// apViewData drives the AP-mode template. Reuses the existing detailData
// (so we get body, recon, attachments, vendor history "for free") and adds
// the AP-specific nav + counts + plain-English summary fields.
type apViewData struct {
	detailData
	Filter        apFilter
	FilterLabel   string // "To do" / "Waiting" / "Done today"
	Index         int
	Total         int
	PrevURL       string // empty if at start
	NextURL       string // always non-empty (last → done screen)
	Counts        apCounts // for the tab bar
	UserName      string   // for greeting ("the AP pilot user" not "ap-clerk")
	UserID        string   // lowercase id used to filter self out of the Assign picker
	AssignTargets []p21.APUser // other AP clerks the current user can hand off to
	APUsers       []p21.APUser // full AP user list — drives the "view as" selector
	// View-as: when set, the Todo tab is filtered to messages owned by ViewAs
	// instead of the effective user. Read-only — Pickup/Assign/Hold buttons
	// are hidden because the clerk isn't acting as ViewAs, just looking.
	ViewAs     string // empty = own queue; otherwise lowercase AP-user ID
	ViewAsName string // display name for the banner
}

// apCounts feeds the four-tab bar at the top of /ap.
type apCounts struct {
	Unassigned int
	Todo       int
	Hold       int
	Done       int
}

// handleAP renders the AP-mode focused view. Bounds-clamped index; redirects
// to a "queue cleared" screen at end. Auto-claims the current message on
// open (sets Owner to the effective user) so the clerk doesn't have to
// click a separate Claim button.
func (s *server) handleAP(w http.ResponseWriter, r *http.Request) {
	filter := parseAPFilter(r.URL.Query().Get("filter"))
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
	user := s.effectiveUser(r)

	// "View as" — read-only peek at another clerk's Todo. Validated against
	// the live P21 AP-user list so a typo can't divert the filter to a
	// random string. If view-as is set + filter is Todo, the Todo bucket
	// shows that clerk's owned messages instead of the current user's.
	// Other tabs (Unassigned, Waiting, Done) ignore view-as — they're
	// shared queues anyway.
	viewAs := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("view")))
	viewAsName := ""
	apUsers := []p21.APUser{}
	if s.p21 != nil {
		luCtx, luCancel := context.WithTimeout(r.Context(), 1*time.Second)
		if list, err := s.p21.ListAPUsers(luCtx); err == nil {
			apUsers = list
			if viewAs != "" {
				ok := false
				for _, u := range list {
					if strings.EqualFold(u.ID, viewAs) {
						viewAs = strings.ToLower(u.ID)
						viewAsName = u.Name
						ok = true
						break
					}
				}
				if !ok {
					// Unknown user — drop silently rather than 400. Browser
					// state could carry a stale ID; safer to fall back to
					// own queue than to blow up.
					viewAs = ""
				}
			}
		}
		luCancel()
	}

	// Effective filter user: when viewing as someone else AND on the Todo
	// tab, filter Todo to that user's owned messages. Otherwise use the
	// real effective user.
	filterUser := user
	if viewAs != "" && filter == apFilterTodo {
		filterUser = viewAs
	}

	msgs := applyAPFilter(all, filter, filterUser)
	counts := apCounts{
		Unassigned: len(applyAPFilter(all, apFilterUnassigned, user)),
		Todo:       len(applyAPFilter(all, apFilterTodo, filterUser)),
		Hold:       len(applyAPFilter(all, apFilterHold, user)),
		Done:       len(applyAPFilter(all, apFilterDone, user)),
	}

	if len(msgs) == 0 || idx >= len(msgs) {
		// Empty queue — render a friendly "queue cleared" page with the
		// other tabs in case there's work elsewhere.
		data := map[string]any{
			"Filter":      filter,
			"FilterLabel": apFilterLabel(filter),
			"Counts":      counts,
			"UserName":    apUserName(s, r, user),
		}
		if err := s.tmpl.ExecuteTemplate(w, "ap-empty.html", data); err != nil {
			log.Printf("render ap-empty: %v", err)
		}
		return
	}

	current := msgs[idx]
	detail, err := s.buildDetailData(r.Context(), current.ID, user)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	detail.IsReview = true // reuses the review-mode close-button suppression

	// No auto-claim — the clerk explicitly hits Pickup when they decide
	// to work an invoice. Paper analogue: you can shuffle through the
	// stack without committing; "picking up" is a deliberate gesture.

	// Preserve view-as across prev/next so keyboard nav doesn't bounce out
	// of the read-only mode mid-scan.
	viewQS := ""
	if viewAs != "" {
		viewQS = "&view=" + viewAs
	}
	prevURL := ""
	if idx > 0 {
		prevURL = fmt.Sprintf("/ap?filter=%s&index=%d%s", filter, idx-1, viewQS)
	}
	nextURL := fmt.Sprintf("/ap?filter=%s&index=%d%s", filter, idx+1, viewQS)

	// Assign picker: list of OTHER AP clerks (excluding self). Reuses the
	// apUsers slice already fetched above for the view-as selector — same
	// underlying P21 query, no extra round-trip.
	var assignTargets []p21.APUser
	for _, u := range apUsers {
		if !strings.EqualFold(u.ID, user) {
			assignTargets = append(assignTargets, u)
		}
	}

	data := apViewData{
		detailData:    detail,
		Filter:        filter,
		FilterLabel:   apFilterLabel(filter),
		Index:         idx,
		Total:         len(msgs),
		PrevURL:       prevURL,
		NextURL:       nextURL,
		Counts:        counts,
		UserName:      apUserName(s, r, user),
		UserID:        user,
		AssignTargets: assignTargets,
		APUsers:       apUsers,
		ViewAs:        viewAs,
		ViewAsName:    viewAsName,
	}
	if err := s.tmpl.ExecuteTemplate(w, "ap.html", data); err != nil {
		log.Printf("render ap: %v", err)
	}
}

// apFilterLabel returns the human-facing tab label. Plain English by design.
func apFilterLabel(f apFilter) string {
	switch f {
	case apFilterUnassigned:
		return "Unassigned"
	case apFilterHold:
		return "Waiting"
	case apFilterDone:
		return "Done today"
	}
	return "To do"
}

// apUserName resolves the display-friendly first name for the greeting.
// Falls back to the lowercase user ID if P21 lookup fails.
func apUserName(s *server, r *http.Request, user string) string {
	if s.p21 == nil {
		return capitalize(user)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 1*time.Second)
	users, err := s.p21.ListAPUsers(ctx)
	cancel()
	if err != nil {
		return capitalize(user)
	}
	for _, u := range users {
		if strings.EqualFold(u.ID, user) {
			return u.FirstName()
		}
	}
	return capitalize(user)
}

// handleAPPickup is the "I'll work this one" gesture. Sets Owner to the
// effective user and Status: In Progress, then advances to the next message.
// Naming intentionally avoids "Approve" — the clerk isn't approving the
// invoice content (that happens when they post the voucher in P21); they're
// just picking it up off the stack to work it.
func (s *server) handleAPPickup(w http.ResponseWriter, r *http.Request) {
	msgID, err := decodeRowID(chi.URLParam(r, "rowID"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	user := s.effectiveUser(r)
	m, err := s.gc.GetMessage(s.mailbox, msgID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	cats := ui.ReplaceOwner(m.Categories, user)
	cats = ui.ReplaceStatus(cats, "In Progress")
	if err := s.gc.SetCategories(s.mailbox, msgID, cats); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	_ = s.cache.UpdateCategories(r.Context(), s.mailbox, msgID, cats)
	// Clear any pending followup timer — the clerk is actively working it
	// now, so the auto-resurface no longer applies.
	_ = s.cache.ClearFollowup(r.Context(), s.mailbox, msgID)
	apRedirectNext(w, r)
}

// handleAPAssign hands the message to a different AP clerk. Form field "to"
// is the lowercase target user ID; validated against the live P21 AP-user
// list so a typo can't drop arbitrary cookie values into Owner. Sets
// Status: In Progress so the assignee sees it as active work, not new.
// Auto-advances the current clerk to the next message.
func (s *server) handleAPAssign(w http.ResponseWriter, r *http.Request) {
	msgID, err := decodeRowID(chi.URLParam(r, "rowID"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	target := strings.ToLower(strings.TrimSpace(r.FormValue("to")))
	if target == "" {
		http.Error(w, "no assignee", http.StatusBadRequest)
		return
	}
	if s.p21 == nil {
		http.Error(w, "P21 not configured — can't validate assignee", http.StatusServiceUnavailable)
		return
	}
	listCtx, listCancel := context.WithTimeout(r.Context(), 2*time.Second)
	users, err := s.p21.ListAPUsers(listCtx)
	listCancel()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	valid := false
	for _, u := range users {
		if strings.EqualFold(u.ID, target) {
			valid = true
			target = strings.ToLower(u.ID)
			break
		}
	}
	if !valid {
		http.Error(w, "unknown assignee", http.StatusBadRequest)
		return
	}
	m, err := s.gc.GetMessage(s.mailbox, msgID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	cats := ui.ReplaceOwner(m.Categories, target)
	cats = ui.ReplaceStatus(cats, "In Progress")
	if err := s.gc.SetCategories(s.mailbox, msgID, cats); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	_ = s.cache.UpdateCategories(r.Context(), s.mailbox, msgID, cats)
	apRedirectNext(w, r)
}

// handleAPHold parks the message with a Blocker and Status: Blocked. Reasons
// are mapped from plain-English picker values to the existing Blocker enum.
func (s *server) handleAPHold(w http.ResponseWriter, r *http.Request) {
	msgID, err := decodeRowID(chi.URLParam(r, "rowID"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	reason := strings.TrimSpace(r.FormValue("reason"))
	blocker := ""
	// Followup duration per reason: how long the message stays Blocked
	// before the sweeper auto-resurfaces it back into Todo. External
	// dependencies (vendor, customer) get longer waits since they have
	// to respond out-of-band; internal ones (pricing, PO purchasing) are
	// faster because they're a Slack ping away. "Won't Pay" never
	// resurfaces — clerk explicitly closed it.
	followupDuration := time.Duration(0)
	switch reason {
	case "ask-buyer":
		blocker = "Purchasing"
		followupDuration = 24 * time.Hour
	case "ask-vendor":
		blocker = "Vendor"
		followupDuration = 72 * time.Hour
	case "pricing":
		blocker = "Pricing"
		followupDuration = 24 * time.Hour
	case "po":
		blocker = "PO"
		followupDuration = 48 * time.Hour
	case "wont-pay":
		blocker = "Won't Pay"
		followupDuration = 0 // never auto-resurface
	default:
		http.Error(w, "unknown hold reason", http.StatusBadRequest)
		return
	}
	user := s.effectiveUser(r)
	m, err := s.gc.GetMessage(s.mailbox, msgID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	// Set owner (auto-claim) + add the blocker (which auto-sets Status: Blocked).
	cats := ui.ReplaceOwner(m.Categories, user)
	cats = ui.ToggleBlocker(cats, blocker)
	if err := s.gc.SetCategories(s.mailbox, msgID, cats); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	_ = s.cache.UpdateCategories(r.Context(), s.mailbox, msgID, cats)
	// Stamp the followup deadline (or clear it for "Won't Pay"). The sweeper
	// in followup.go scans this column and auto-resurfaces past-due rows.
	if followupDuration > 0 {
		_ = s.cache.SetFollowup(r.Context(), s.mailbox, msgID, time.Now().Add(followupDuration))
	} else {
		_ = s.cache.ClearFollowup(r.Context(), s.mailbox, msgID)
	}
	apRedirectNext(w, r)
}

// handleAPSkip just advances to the next message in the filter without
// changing any state. Useful when the clerk wants to come back to a tricky
// invoice but isn't ready to Hold it yet.
func (s *server) handleAPSkip(w http.ResponseWriter, r *http.Request) {
	apRedirectNext(w, r)
}

// handleAPAddNote appends a clerk note from the AP-mode side-by-side panel,
// then 303-redirects back to the same /ap position (filter + index). Separate
// from handleAddNote because that one renders detail.html for the admin view;
// AP mode wants a redirect that stays on /ap so the new note appears on
// reload alongside the rest of the side-by-side surface.
func (s *server) handleAPAddNote(w http.ResponseWriter, r *http.Request) {
	msgID, err := decodeRowID(chi.URLParam(r, "rowID"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	body := strings.TrimSpace(r.FormValue("body"))
	if body == "" {
		// Empty submit — just bounce back without an error so an accidental
		// click doesn't dump a 400 page on the clerk.
		apRedirectCurrent(w, r)
		return
	}
	user := s.effectiveUser(r)
	addCtx, addCancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer addCancel()
	if _, err := s.cache.AddInvoiceNote(addCtx, s.mailbox, msgID, user, body); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	apRedirectCurrent(w, r)
}

// apRedirectCurrent sends the clerk back to the same /ap index they were on.
// Used after notes / actions that shouldn't auto-advance the queue.
func apRedirectCurrent(w http.ResponseWriter, r *http.Request) {
	filter := parseAPFilter(r.FormValue("filter"))
	idx := r.FormValue("index")
	if idx == "" {
		idx = "0"
	}
	target := fmt.Sprintf("/ap?filter=%s&index=%s", filter, idx)
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// apRedirectNext computes the next URL based on the form's index field and
// 303-redirects there. Form sets index = current+1 from the AP template so
// the server doesn't need a "current index" param.
func apRedirectNext(w http.ResponseWriter, r *http.Request) {
	filter := parseAPFilter(r.FormValue("filter"))
	nextIdx := r.FormValue("next_index")
	if nextIdx == "" {
		nextIdx = "0"
	}
	target := fmt.Sprintf("/ap?filter=%s&index=%s", filter, nextIdx)
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// filtersFor returns the tab list for this request. Narrowed tab set when an
