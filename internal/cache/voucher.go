// Package cache — voucher subsystem: P21 voucher status sync per extraction
// row, plus the Done-vs-unposted reconciliation helper that surfaces clerk-set
// "Done" categories where P21 disagrees.

package cache

import (
	"context"
	"database/sql"
	"time"
)


// VoucherInfo is what the P21 sync writes back to cache per extraction.
// Zero values are valid: Status="unposted" means P21 has no matching apinv_hdr
// row yet; Status="posted" means a voucher exists but isn't paid in full.
type VoucherInfo struct {
	VoucherNo     string
	Status        string // "unposted" | "posted" | "paid"
	PostedAt      time.Time
	PaidAt        time.Time
	InvoiceAmount float64
	CheckNo       string
}

// P21SyncCandidate is one row the sync loop needs to look up in P21.
type P21SyncCandidate struct {
	MessageID     string
	PONo          int64
	InvoiceNumber string
}

// ListP21SyncCandidates returns extractions that have both po_no and an
// invoice_number and haven't been synced with P21 recently (or ever). Limited
// to reasonably recent messages — old vouchers rarely change state.
func (c *Cache) ListP21SyncCandidates(ctx context.Context, mailbox string, staleBefore time.Time, limit int) ([]P21SyncCandidate, error) {
	rows, err := c.db.QueryContext(ctx, `
		SELECT e.message_id, e.po_no, json_extract(e.invoice_data, '$.invoice_number')
		FROM invoice_extractions e
		WHERE e.mailbox = ?
		  AND e.po_no IS NOT NULL AND e.po_no > 0
		  AND e.invoice_data IS NOT NULL
		  AND json_extract(e.invoice_data, '$.invoice_number') IS NOT NULL
		  AND json_extract(e.invoice_data, '$.invoice_number') != ''
		  AND (e.last_p21_sync_at IS NULL OR e.last_p21_sync_at < ?)
		  AND e.pay_status IS NOT 'paid'
		ORDER BY e.last_p21_sync_at IS NOT NULL, e.last_p21_sync_at ASC
		LIMIT ?
	`, mailbox, staleBefore.UTC(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []P21SyncCandidate
	for rows.Next() {
		var (
			c    P21SyncCandidate
			poNo sql.NullInt64
			inv  sql.NullString
		)
		if err := rows.Scan(&c.MessageID, &poNo, &inv); err != nil {
			return nil, err
		}
		if poNo.Valid {
			c.PONo = poNo.Int64
		}
		if inv.Valid {
			c.InvoiceNumber = inv.String
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetVoucherInfo writes a P21 sync result back onto an extraction row. Called
// once per candidate after the P21 query. For not-found vouchers, pass
// Status="unposted" with zero-value fields.
func (c *Cache) SetVoucherInfo(ctx context.Context, mailbox, messageID string, info VoucherInfo) error {
	now := time.Now().UTC()
	var voucherNo, checkNo, status sql.NullString
	var postedAt, paidAt sql.NullTime
	var invAmt sql.NullFloat64
	if info.VoucherNo != "" {
		voucherNo = sql.NullString{String: info.VoucherNo, Valid: true}
	}
	if info.CheckNo != "" {
		checkNo = sql.NullString{String: info.CheckNo, Valid: true}
	}
	if info.Status != "" {
		status = sql.NullString{String: info.Status, Valid: true}
	}
	if !info.PostedAt.IsZero() {
		postedAt = sql.NullTime{Time: info.PostedAt, Valid: true}
	}
	if !info.PaidAt.IsZero() {
		paidAt = sql.NullTime{Time: info.PaidAt, Valid: true}
	}
	if info.InvoiceAmount != 0 {
		invAmt = sql.NullFloat64{Float64: info.InvoiceAmount, Valid: true}
	}
	_, err := c.db.ExecContext(ctx, `
		UPDATE invoice_extractions
		SET voucher_no = ?, pay_status = ?, posted_at = ?, paid_at = ?,
		    invoice_amount = ?, check_no = ?, last_p21_sync_at = ?
		WHERE mailbox = ? AND message_id = ?
	`, voucherNo, status, postedAt, paidAt, invAmt, checkNo, now, mailbox, messageID)
	return err
}

// GetVoucherInfo returns a message's stored voucher info + whether it was ever
// synced. Empty Status + zero LastSyncAt == never synced.
func (c *Cache) GetVoucherInfo(ctx context.Context, mailbox, messageID string) (VoucherInfo, time.Time, error) {
	var (
		info    VoucherInfo
		voucher sql.NullString
		status  sql.NullString
		posted  sql.NullTime
		paid    sql.NullTime
		invAmt  sql.NullFloat64
		check   sql.NullString
		lastAt  sql.NullTime
	)
	err := c.db.QueryRowContext(ctx, `
		SELECT voucher_no, pay_status, posted_at, paid_at, invoice_amount, check_no, last_p21_sync_at
		FROM invoice_extractions
		WHERE mailbox = ? AND message_id = ?
	`, mailbox, messageID).Scan(&voucher, &status, &posted, &paid, &invAmt, &check, &lastAt)
	if err == sql.ErrNoRows {
		return info, time.Time{}, nil
	}
	if err != nil {
		return info, time.Time{}, err
	}
	if voucher.Valid {
		info.VoucherNo = voucher.String
	}
	if status.Valid {
		info.Status = status.String
	}
	if posted.Valid {
		info.PostedAt = posted.Time
	}
	if paid.Valid {
		info.PaidAt = paid.Time
	}
	if invAmt.Valid {
		info.InvoiceAmount = invAmt.Float64
	}
	if check.Valid {
		info.CheckNo = check.String
	}
	var syncedAt time.Time
	if lastAt.Valid {
		syncedAt = lastAt.Time
	}
	return info, syncedAt, nil
}

// ListDoneUnpostedConflicts returns message IDs where the Outlook category
// includes "Status: Done" but the P21 voucher status came back "unposted."
// Two ways this can happen:
//   - Legacy: a clerk hit Done before posting was wired (or before the
//     system-derived-Done switch).
//   - Drift: the voucher was deleted/reversed in P21 after Done was set.
// In both cases the contradiction is explicit and we can clear Status:Done
// safely. (Empty pay_status is *not* included — that means we never tried
// to look it up, which is a different signal.)
func (c *Cache) ListDoneUnpostedConflicts(ctx context.Context, mailbox string) ([]string, error) {
	rows, err := c.db.QueryContext(ctx, `
		SELECT m.id
		FROM messages m
		JOIN invoice_extractions e ON e.mailbox = m.mailbox AND e.message_id = m.id
		WHERE m.mailbox = ?
		  AND m.categories_json LIKE '%"Status: Done"%'
		  AND e.pay_status = 'unposted'
	`, mailbox)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
