// Package cache — small auxiliary tables: clerk notes, attachment rotations,
// tier-PDF cooldowns, and clerk verdicts. Each is independent; grouped here
// because none warrants its own file.
package cache

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)


// InvoiceNote is one append-only note row. Author is the effective user
// (impersonated identity wins over auth user, matching how Owner works).
type InvoiceNote struct {
	NoteUID   int64
	Author    string
	Body      string
	CreatedAt time.Time
}

// AddInvoiceNote appends a note. No edit / delete path — notes are an audit
// trail, not a draft surface. Returns the new note's UID for the caller's
// optimistic render.
func (c *Cache) AddInvoiceNote(ctx context.Context, mailbox, messageID, author, body string) (int64, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return 0, fmt.Errorf("empty note body")
	}
	res, err := c.db.ExecContext(ctx, `
		INSERT INTO invoice_notes (mailbox, message_id, author, body, created_at)
		VALUES (?, ?, ?, ?, ?)
	`, mailbox, messageID, author, body, time.Now().UTC())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListInvoiceNotes returns notes for a message, oldest first (chronological
// log order matches paper "scribbled annotations" mental model).
func (c *Cache) ListInvoiceNotes(ctx context.Context, mailbox, messageID string) ([]InvoiceNote, error) {
	rows, err := c.db.QueryContext(ctx, `
		SELECT note_uid, author, body, created_at
		FROM invoice_notes
		WHERE mailbox = ? AND message_id = ?
		ORDER BY created_at ASC, note_uid ASC
	`, mailbox, messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []InvoiceNote{}
	for rows.Next() {
		var n InvoiceNote
		if err := rows.Scan(&n.NoteUID, &n.Author, &n.Body, &n.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// CountInvoiceNotesByMessage returns a {messageID: count} map for an entire
// mailbox. Used by the list render to show a 💬 badge on rows with notes
// without a per-row roundtrip.
func (c *Cache) CountInvoiceNotesByMessage(ctx context.Context, mailbox string) (map[string]int, error) {
	rows, err := c.db.QueryContext(ctx, `
		SELECT message_id, COUNT(*) FROM invoice_notes WHERE mailbox = ?
		GROUP BY message_id
	`, mailbox)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// GetAttachmentRotation returns the saved rotation angle (0/90/180/270) for
// an attachment in a message. Returns 0 if no row exists. Cheap point lookup
// keyed on the PRIMARY KEY.
func (c *Cache) GetAttachmentRotation(ctx context.Context, mailbox, messageID, attachmentID string) (int, error) {
	var angle int
	err := c.db.QueryRowContext(ctx, `
		SELECT angle FROM attachment_rotations
		WHERE mailbox = ? AND message_id = ? AND attachment_id = ?
	`, mailbox, messageID, attachmentID).Scan(&angle)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return angle, nil
}

// SetAttachmentRotation saves a rotation angle. Angle should be in
// {0, 90, 180, 270}; values outside that range are normalized into it.
// Setting 0 deletes the row (no rotation = absent row).
func (c *Cache) SetAttachmentRotation(ctx context.Context, mailbox, messageID, attachmentID string, angle int, user string) error {
	// Normalize into [0, 359] then snap to the four legal values.
	angle = ((angle % 360) + 360) % 360
	switch {
	case angle < 45 || angle >= 315:
		angle = 0
	case angle < 135:
		angle = 90
	case angle < 225:
		angle = 180
	default:
		angle = 270
	}
	if angle == 0 {
		_, err := c.db.ExecContext(ctx, `
			DELETE FROM attachment_rotations
			WHERE mailbox = ? AND message_id = ? AND attachment_id = ?
		`, mailbox, messageID, attachmentID)
		return err
	}
	_, err := c.db.ExecContext(ctx, `
		INSERT INTO attachment_rotations (mailbox, message_id, attachment_id, angle, set_at, set_by)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(mailbox, message_id, attachment_id) DO UPDATE SET
			angle  = excluded.angle,
			set_at = excluded.set_at,
			set_by = excluded.set_by
	`, mailbox, messageID, attachmentID, angle, time.Now().UTC(), user)
	return err
}

// CheckPDFCooldown reports whether a (pdf_sha, tier) pair is within its
// cooldown window. Returns (active, untilUTC, reason). On no matching row,
// returns (false, zero-time, "").
func (c *Cache) CheckPDFCooldown(ctx context.Context, pdfSha string, tier int) (bool, time.Time, string, error) {
	var until time.Time
	var reason sql.NullString
	err := c.db.QueryRowContext(ctx, `
		SELECT next_allowed_at, reason
		FROM pdf_cooldowns
		WHERE pdf_sha256 = ? AND tier = ?
	`, pdfSha, tier).Scan(&until, &reason)
	if err == sql.ErrNoRows {
		return false, time.Time{}, "", nil
	}
	if err != nil {
		return false, time.Time{}, "", err
	}
	if time.Now().UTC().Before(until) {
		return true, until, reason.String, nil
	}
	return false, until, reason.String, nil
}

// SetPDFCooldown stamps (or refreshes) a cooldown for a (pdf_sha, tier) pair.
// Overwrites any existing row. Caller picks the duration based on the outcome
// (success with discrepancy, timeout, error, etc.).
func (c *Cache) SetPDFCooldown(ctx context.Context, pdfSha string, tier int, until time.Time, reason string) error {
	_, err := c.db.ExecContext(ctx, `
		INSERT INTO pdf_cooldowns (pdf_sha256, tier, next_allowed_at, reason, set_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(pdf_sha256, tier) DO UPDATE SET
			next_allowed_at = excluded.next_allowed_at,
			reason          = excluded.reason,
			set_at          = excluded.set_at
	`, pdfSha, tier, until.UTC(), reason, time.Now().UTC())
	return err
}

// Verdict is one append-only clerk verdict on an extraction. Drives the
// accuracy-loop corpus (see ACCURACY-LOOP.md) — every wrong/corrected verdict
// is a candidate diagnostic case to feed to the strong-model teacher in
// Phase 2-3.
type Verdict struct {
	UID           int64
	User          string
	Verdict       string // "right" | "wrong" | "corrected"
	CorrectedData string // JSON string, empty when not 'corrected'
	CreatedAt     time.Time
}

// RecordVerdict appends a verdict row. No edit path — clerks can re-record
// (e.g. flipped opinion after re-reading the PDF) and we keep all of them
// chronologically. Caller is responsible for normalizing verdict to one of
// the canonical strings before calling.
func (c *Cache) RecordVerdict(ctx context.Context, mailbox, messageID, user, verdict, correctedData string) error {
	var cd any
	if correctedData != "" {
		cd = correctedData
	}
	_, err := c.db.ExecContext(ctx, `
		INSERT INTO clerk_verdicts (mailbox, message_id, user, verdict, corrected_data, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, mailbox, messageID, user, verdict, cd, time.Now().UTC())
	return err
}

// ListVerdictsByMessage returns all verdicts on one message, newest first.
// Used by the detail/AP UI to show the clerk's most recent verdict (so they
// can see "you marked this Wrong 2h ago" instead of double-clicking).
func (c *Cache) ListVerdictsByMessage(ctx context.Context, mailbox, messageID string) ([]Verdict, error) {
	rows, err := c.db.QueryContext(ctx, `
		SELECT verdict_uid, user, verdict, COALESCE(corrected_data, ''), created_at
		FROM clerk_verdicts
		WHERE mailbox = ? AND message_id = ?
		ORDER BY created_at DESC, verdict_uid DESC
	`, mailbox, messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Verdict{}
	for rows.Next() {
		var v Verdict
		if err := rows.Scan(&v.UID, &v.User, &v.Verdict, &v.CorrectedData, &v.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// VendorVerdictCount is one row in the "vendors clerks disagree with most"
// leaderboard. Wrong includes both 'wrong' and 'corrected' — both signal
// that the extraction missed.
type VendorVerdictCount struct {
	Vendor       string
	Right        int
	Wrong        int
	Total        int
	DisagreeRate float64 // Wrong/Total, 0.0-1.0
}

// UserVerdictedMessageIDs returns the set of message IDs that `user` has
// recorded ANY verdict on, mailbox-scoped. Used by /extract-review to skip
// messages the clerk has already classified so the queue shrinks naturally.
// Returns an empty (non-nil) map when there are no rows.
func (c *Cache) UserVerdictedMessageIDs(ctx context.Context, mailbox, user string) (map[string]bool, error) {
	rows, err := c.db.QueryContext(ctx, `
		SELECT DISTINCT message_id FROM clerk_verdicts
		WHERE mailbox = ? AND user = ?
	`, mailbox, user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// VerdictCountsByVendor returns per-vendor verdict tallies over the recent
// window. Joins messages.categories_json to extract the Vendor: tag, then
// aggregates in Go (the JSON walk is cheaper in code than in SQL JSON1, and
// matches the RecentMessageMeta pattern used elsewhere). Mailbox-scoped.
// Limit caps the result count (top N by Wrong+Corrected DESC, then Total
// DESC, then Vendor ASC for stable ordering).
func (c *Cache) VerdictCountsByVendor(ctx context.Context, mailbox string, since time.Time, limit int) ([]VendorVerdictCount, error) {
	rows, err := c.db.QueryContext(ctx, `
		SELECT v.verdict, COALESCE(m.categories_json, '[]')
		FROM clerk_verdicts v
		JOIN messages m ON m.mailbox = v.mailbox AND m.id = v.message_id
		WHERE v.mailbox = ? AND v.created_at >= ?
	`, mailbox, since.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	agg := map[string]*VendorVerdictCount{}
	for rows.Next() {
		var verdict, catsJSON string
		if err := rows.Scan(&verdict, &catsJSON); err != nil {
			return nil, err
		}
		vendor := ""
		var cats []string
		if err := json.Unmarshal([]byte(catsJSON), &cats); err == nil {
			for _, cat := range cats {
				if strings.HasPrefix(cat, "Vendor: ") {
					vendor = strings.TrimPrefix(cat, "Vendor: ")
					break
				}
			}
		}
		if vendor == "" {
			continue
		}
		row, ok := agg[vendor]
		if !ok {
			row = &VendorVerdictCount{Vendor: vendor}
			agg[vendor] = row
		}
		row.Total++
		switch verdict {
		case "right":
			row.Right++
		case "wrong", "corrected":
			row.Wrong++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]VendorVerdictCount, 0, len(agg))
	for _, r := range agg {
		if r.Total > 0 {
			r.DisagreeRate = float64(r.Wrong) / float64(r.Total)
		}
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Wrong != out[j].Wrong {
			return out[i].Wrong > out[j].Wrong
		}
		if out[i].Total != out[j].Total {
			return out[i].Total > out[j].Total
		}
		return out[i].Vendor < out[j].Vendor
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
