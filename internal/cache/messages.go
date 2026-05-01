// Package cache — message + attachment CRUD: mirroring Outlook messages and
// their attachments locally so the list view, detail render, and search
// queries don't hit Graph on every page load. Categories live in messages
// here, but Outlook is still authoritative — this is pure cache.
package cache

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"
)


// CachedMessage is a mailbox message as stored locally. Mirrors graph.Message's
// key fields. Categories are stored as a JSON array string for simplicity.
type CachedMessage struct {
	Mailbox           string
	ID                string
	ConversationID    string
	InternetMessageID string
	Subject           string
	SenderEmail       string
	SenderName        string
	ReceivedAt        time.Time
	BodyPreview       string
	Categories        []string
	WebLink           string
	HasAttachments    bool
	LastSyncedAt      time.Time
}

// UpsertMessage inserts or replaces a message row. Safe to call with the same
// ID repeatedly — LastSyncedAt updates every call so sweeps can find stale rows.
func (c *Cache) UpsertMessage(ctx context.Context, m CachedMessage) error {
	catsJSON, _ := json.Marshal(m.Categories)
	_, err := c.db.ExecContext(ctx, `
		INSERT INTO messages (
			mailbox, id, conversation_id, internet_message_id, subject,
			sender_email, sender_name, received_at, body_preview,
			categories_json, web_link, has_attachments, last_synced_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(mailbox, id) DO UPDATE SET
			conversation_id=excluded.conversation_id,
			internet_message_id=excluded.internet_message_id,
			subject=excluded.subject,
			sender_email=excluded.sender_email,
			sender_name=excluded.sender_name,
			received_at=excluded.received_at,
			body_preview=excluded.body_preview,
			categories_json=excluded.categories_json,
			web_link=excluded.web_link,
			has_attachments=excluded.has_attachments,
			last_synced_at=excluded.last_synced_at
	`, m.Mailbox, m.ID, m.ConversationID, m.InternetMessageID, m.Subject,
		m.SenderEmail, m.SenderName, m.ReceivedAt, m.BodyPreview,
		string(catsJSON), m.WebLink, boolToInt(m.HasAttachments), m.LastSyncedAt)
	return err
}

// UpdateMessageBody sets the cached body_html/body_text and stamps the
// fetch time. Call after pulling the full body from Graph.
func (c *Cache) UpdateMessageBody(ctx context.Context, mailbox, messageID, bodyHTML, bodyText string) error {
	_, err := c.db.ExecContext(ctx, `
		UPDATE messages SET body_html=?, body_text=?, last_full_body_fetch_at=?
		WHERE mailbox=? AND id=?
	`, bodyHTML, bodyText, time.Now().UTC(), mailbox, messageID)
	return err
}

// GetMessageBody returns the cached body (HTML + text) and when it was
// last fetched. All-empty return = body hasn't been fetched yet.
func (c *Cache) GetMessageBody(ctx context.Context, mailbox, messageID string) (bodyHTML, bodyText string, fetchedAt time.Time, err error) {
	var h, t sql.NullString
	var at sql.NullTime
	err = c.db.QueryRowContext(ctx, `
		SELECT body_html, body_text, last_full_body_fetch_at
		FROM messages WHERE mailbox=? AND id=?
	`, mailbox, messageID).Scan(&h, &t, &at)
	if err == sql.ErrNoRows {
		return "", "", time.Time{}, nil
	}
	if err != nil {
		return "", "", time.Time{}, err
	}
	if h.Valid {
		bodyHTML = h.String
	}
	if t.Valid {
		bodyText = t.String
	}
	if at.Valid {
		fetchedAt = at.Time
	}
	return
}

// CachedAttachment is one attachment row. local_path is empty when the
// blob hasn't been downloaded yet; blob_sha is set once the bytes are
// on disk and hashed.
type CachedAttachment struct {
	Mailbox       string
	MessageID     string
	AttachmentID  string
	Filename      string
	ContentType   string
	SizeBytes     int64
	BlobSHA       string
	LocalPath     string
	StoredAt      time.Time
	LastError     string
}

// UpsertAttachment writes an attachment metadata row. Safe to call before
// the bytes are fetched (BlobSHA + LocalPath + StoredAt empty) and again
// after fetch to fill those in.
func (c *Cache) UpsertAttachment(ctx context.Context, a CachedAttachment) error {
	var storedAt any
	if !a.StoredAt.IsZero() {
		storedAt = a.StoredAt
	}
	_, err := c.db.ExecContext(ctx, `
		INSERT INTO attachments
			(mailbox, message_id, attachment_id, filename, content_type, size_bytes, blob_sha, local_path, stored_at, last_error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(mailbox, message_id, attachment_id) DO UPDATE SET
			filename=excluded.filename,
			content_type=excluded.content_type,
			size_bytes=excluded.size_bytes,
			blob_sha=COALESCE(NULLIF(excluded.blob_sha,''), attachments.blob_sha),
			local_path=COALESCE(NULLIF(excluded.local_path,''), attachments.local_path),
			stored_at=COALESCE(excluded.stored_at, attachments.stored_at),
			last_error=excluded.last_error
	`, a.Mailbox, a.MessageID, a.AttachmentID, a.Filename, a.ContentType, a.SizeBytes, a.BlobSHA, a.LocalPath, storedAt, a.LastError)
	return err
}

// ListMessageAttachments returns all attachment rows for a message.
// Empty slice if no rows (including "message has no attachments" cases).
func (c *Cache) ListMessageAttachments(ctx context.Context, mailbox, messageID string) ([]CachedAttachment, error) {
	rows, err := c.db.QueryContext(ctx, `
		SELECT mailbox, message_id, attachment_id, filename,
		       COALESCE(content_type,''), COALESCE(size_bytes,0),
		       COALESCE(blob_sha,''), COALESCE(local_path,''),
		       stored_at, COALESCE(last_error,'')
		FROM attachments
		WHERE mailbox=? AND message_id=?
		ORDER BY filename
	`, mailbox, messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CachedAttachment
	for rows.Next() {
		var a CachedAttachment
		var storedAt sql.NullTime
		if err := rows.Scan(&a.Mailbox, &a.MessageID, &a.AttachmentID, &a.Filename,
			&a.ContentType, &a.SizeBytes, &a.BlobSHA, &a.LocalPath,
			&storedAt, &a.LastError); err != nil {
			return nil, err
		}
		if storedAt.Valid {
			a.StoredAt = storedAt.Time
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListMessages returns cached messages for a mailbox, newest first. If limit > 0,
// only the newest limit rows are returned.
func (c *Cache) ListMessages(ctx context.Context, mailbox string, limit int) ([]CachedMessage, error) {
	q := `SELECT mailbox, id, conversation_id, internet_message_id, subject,
	             sender_email, sender_name, received_at, body_preview,
	             categories_json, web_link, has_attachments, last_synced_at
	      FROM messages WHERE mailbox = ? ORDER BY received_at DESC`
	args := []interface{}{mailbox}
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := c.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]CachedMessage, 0, 256)
	for rows.Next() {
		var (
			m          CachedMessage
			catsJSON   sql.NullString
			hasAtt     int
			convID     sql.NullString
			imsgID     sql.NullString
			subject    sql.NullString
			sEmail     sql.NullString
			sName      sql.NullString
			bodyPrev   sql.NullString
			webLink    sql.NullString
		)
		if err := rows.Scan(&m.Mailbox, &m.ID, &convID, &imsgID, &subject,
			&sEmail, &sName, &m.ReceivedAt, &bodyPrev,
			&catsJSON, &webLink, &hasAtt, &m.LastSyncedAt); err != nil {
			return nil, err
		}
		m.ConversationID = convID.String
		m.InternetMessageID = imsgID.String
		m.Subject = subject.String
		m.SenderEmail = sEmail.String
		m.SenderName = sName.String
		m.BodyPreview = bodyPrev.String
		m.WebLink = webLink.String
		m.HasAttachments = hasAtt != 0
		if catsJSON.Valid && catsJSON.String != "" {
			_ = json.Unmarshal([]byte(catsJSON.String), &m.Categories)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SearchMessages returns messages whose subject, sender, body preview, or
// categories match the query, plus any message whose cached extraction
// references the query as a PO/invoice number.
//
// Simple SQL LIKE for now — fine at prototype volumes. If we outgrow that,
// SQLite FTS5 is a single migration away.
func (c *Cache) SearchMessages(ctx context.Context, mailbox, query string, limit int) ([]CachedMessage, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, nil
	}
	like := "%" + q + "%"

	// Detect all-numeric query (likely a PO or invoice number) — bias toward
	// category and extraction matches for those.
	numeric := true
	for _, r := range q {
		if r < '0' || r > '9' {
			numeric = false
			break
		}
	}

	// Base query: substring match on the main text fields.
	sqlStr := `
		SELECT m.mailbox, m.id, m.conversation_id, m.internet_message_id, m.subject,
		       m.sender_email, m.sender_name, m.received_at, m.body_preview,
		       m.categories_json, m.web_link, m.has_attachments, m.last_synced_at
		FROM messages m
		LEFT JOIN invoice_extractions x ON x.mailbox = m.mailbox AND x.message_id = m.id
		WHERE m.mailbox = ?
		  AND (
		    m.subject LIKE ? COLLATE NOCASE
		    OR m.sender_email LIKE ? COLLATE NOCASE
		    OR m.sender_name LIKE ? COLLATE NOCASE
		    OR m.body_preview LIKE ? COLLATE NOCASE
		    OR m.categories_json LIKE ? COLLATE NOCASE
		    OR x.invoice_data LIKE ? COLLATE NOCASE
		    OR (x.po_no IS NOT NULL AND CAST(x.po_no AS TEXT) LIKE ?)
		  )
		GROUP BY m.mailbox, m.id
		ORDER BY m.received_at DESC`
	args := []interface{}{mailbox, like, like, like, like, like, like, like}
	_ = numeric
	if limit > 0 {
		sqlStr += " LIMIT ?"
		args = append(args, limit)
	}

	rows, err := c.db.QueryContext(ctx, sqlStr, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]CachedMessage, 0, 64)
	for rows.Next() {
		var (
			m        CachedMessage
			catsJSON sql.NullString
			hasAtt   int
			convID   sql.NullString
			imsgID   sql.NullString
			subject  sql.NullString
			sEmail   sql.NullString
			sName    sql.NullString
			bodyPrev sql.NullString
			webLink  sql.NullString
		)
		if err := rows.Scan(&m.Mailbox, &m.ID, &convID, &imsgID, &subject,
			&sEmail, &sName, &m.ReceivedAt, &bodyPrev,
			&catsJSON, &webLink, &hasAtt, &m.LastSyncedAt); err != nil {
			return nil, err
		}
		m.ConversationID = convID.String
		m.InternetMessageID = imsgID.String
		m.Subject = subject.String
		m.SenderEmail = sEmail.String
		m.SenderName = sName.String
		m.BodyPreview = bodyPrev.String
		m.WebLink = webLink.String
		m.HasAttachments = hasAtt != 0
		if catsJSON.Valid && catsJSON.String != "" {
			_ = json.Unmarshal([]byte(catsJSON.String), &m.Categories)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}


// SetFollowup stamps a future timestamp on a message so the followup sweeper
// will resurface it (Status:Blocked → New) when the timestamp passes. Pass
// zero time to clear (use ClearFollowup for clarity).
func (c *Cache) SetFollowup(ctx context.Context, mailbox, messageID string, dueAt time.Time) error {
	var t any
	if !dueAt.IsZero() {
		t = dueAt.UTC()
	}
	_, err := c.db.ExecContext(ctx,
		`UPDATE messages SET followup_at = ? WHERE mailbox = ? AND id = ?`,
		t, mailbox, messageID)
	return err
}

// ClearFollowup removes a pending followup timer. Called when the sweeper
// auto-resurfaces, when a clerk picks up the message manually, or when a
// new Hold replaces the old one with a fresh duration.
func (c *Cache) ClearFollowup(ctx context.Context, mailbox, messageID string) error {
	_, err := c.db.ExecContext(ctx,
		`UPDATE messages SET followup_at = NULL WHERE mailbox = ? AND id = ?`,
		mailbox, messageID)
	return err
}

// ListFollowupsDue returns IDs of messages whose followup timestamp has
// passed. Mailbox-scoped. Used by the followup sweeper to find rows that
// should resurface from Waiting back into Todo.
func (c *Cache) ListFollowupsDue(ctx context.Context, mailbox string, now time.Time) ([]string, error) {
	rows, err := c.db.QueryContext(ctx, `
		SELECT id FROM messages
		WHERE mailbox = ?
		  AND followup_at IS NOT NULL
		  AND followup_at <= ?
	`, mailbox, now.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
