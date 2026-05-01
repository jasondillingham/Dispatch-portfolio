// followup.go — auto-resurface for held messages. Background goroutine that
// polls cache.ListFollowupsDue every minute and reverts past-due rows from
// Status:Blocked to Status:New, clearing Blocker tags. Adds a clerk note
// explaining the auto-resurface so the next clerk has context.
//
// Held messages get followup_at stamped by handleAPHold (duration depends on
// the Hold reason — see the switch in ap.go). When the timer fires, the
// message reappears in the owner's Todo bucket on next render.

package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"dispatch/internal/cache"
	"dispatch/internal/graph"
)

// followupSweepInterval is how often the goroutine checks for due rows. Short
// enough that a 24-hour Hold resurfaces within a minute of being due (no
// surprise delay); long enough that 100 idle servers don't hammer the cache.
const followupSweepInterval = 60 * time.Second

// runFollowupSweep is the long-lived goroutine. Keeps running for the life
// of the process; doFollowupSweep is invoked once at startup (so a server
// restart doesn't lose any past-due rows that accumulated while down) and
// then on every tick.
func runFollowupSweep(cacheDB *cache.Cache, gc *graph.Client, mailbox string) {
	doFollowupSweep(cacheDB, gc, mailbox)
	t := time.NewTicker(followupSweepInterval)
	defer t.Stop()
	for range t.C {
		doFollowupSweep(cacheDB, gc, mailbox)
	}
}

// doFollowupSweep finds messages whose followup timer has passed and
// resurfaces them: clears Blocker:* + Status:Blocked, sets Status:New, adds
// a clerk note, and clears the followup_at column so we don't process the
// same row repeatedly.
func doFollowupSweep(cacheDB *cache.Cache, gc *graph.Client, mailbox string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ids, err := cacheDB.ListFollowupsDue(ctx, mailbox, time.Now())
	if err != nil {
		log.Printf("followup-sweep: list err: %v", err)
		return
	}
	if len(ids) == 0 {
		return
	}
	resurfaced, errs := 0, 0
	for _, id := range ids {
		if resurfaceFollowup(ctx, cacheDB, gc, mailbox, id) {
			resurfaced++
		} else {
			errs++
		}
	}
	log.Printf("followup-sweep: %d resurfaced, %d errs", resurfaced, errs)
}

// resurfaceFollowup performs the per-message work: read current categories,
// strip Blocker + Status:Blocked, set Status:New, push back to Graph, update
// cache, append a note, clear followup_at. Returns true on success.
//
// Best-effort on each step — partial failures (Graph 404 because message
// was deleted, cache write race) clear the followup_at anyway so we don't
// loop on a broken row. Real errors are logged.
func resurfaceFollowup(ctx context.Context, cacheDB *cache.Cache, gc *graph.Client, mailbox, msgID string) bool {
	m, err := gc.GetMessage(mailbox, msgID)
	if err != nil {
		// Message might have been deleted from Outlook between sync passes.
		// Clear the followup so we don't keep retrying forever.
		log.Printf("followup-sweep: get %s: %v", truncateID(msgID), err)
		_ = cacheDB.ClearFollowup(ctx, mailbox, msgID)
		return false
	}
	newCats := stripBlockerAndStatus(m.Categories)
	newCats = ensureStatusNew(newCats)
	if err := gc.SetCategories(mailbox, msgID, newCats); err != nil {
		log.Printf("followup-sweep: patch %s: %v", truncateID(msgID), err)
		return false
	}
	_ = cacheDB.UpdateCategories(ctx, mailbox, msgID, newCats)
	// Note that the resurface happened so the clerk knows why this row
	// reappeared in their Todo. Author "system" matches the existing
	// auto-action voice (Done auto-set on voucher post uses "system" too).
	_, _ = cacheDB.AddInvoiceNote(ctx, mailbox, msgID, "system",
		fmt.Sprintf("Follow-up timer fired at %s — resurfaced from Waiting.",
			time.Now().UTC().Format("Jan 2 3:04 PM")))
	_ = cacheDB.ClearFollowup(ctx, mailbox, msgID)
	return true
}

// stripBlockerAndStatus removes any "Blocker: *" entries and any
// "Status: *" entry from the categories list. Used by the followup
// sweeper to reset a Blocked row before stamping fresh Status:New.
func stripBlockerAndStatus(cats []string) []string {
	out := make([]string, 0, len(cats))
	for _, c := range cats {
		if strings.HasPrefix(c, "Blocker: ") || strings.HasPrefix(c, "Status: ") {
			continue
		}
		out = append(out, c)
	}
	return out
}

// ensureStatusNew adds "Status: New" if no Status entry already present.
// Idempotent — caller can pass a list that already has Status entries
// (stripBlockerAndStatus removes them first).
func ensureStatusNew(cats []string) []string {
	for _, c := range cats {
		if strings.HasPrefix(c, "Status: ") {
			return cats
		}
	}
	return append(cats, "Status: New")
}

// truncateID shortens a Graph message ID for log readability. Full IDs are
// 100+ chars; first 30 is enough to identify the row when grepping logs.
func truncateID(id string) string {
	if len(id) <= 30 {
		return id
	}
	return id[:30] + "…"
}
