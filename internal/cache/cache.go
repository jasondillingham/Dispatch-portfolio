// Package cache is the Dispatch SQLite cache — small, local, throwaway.
// Holds computed data (invoice extractions, reconciliation results) that
// would be too costly to recompute on every page load. NOT source of truth
// for any workflow state: that still lives in Outlook Categories via Graph.
//
// Schema migrations are applied idempotently on Open(). Wipe the file to reset.
package cache

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Cache struct {
	db *sql.DB
}

// Open connects to a SQLite database at path. If path is empty, defaults to
// ~/.dispatch/cache.db. Parent directory is created if missing.
func Open(path string) (*Cache, error) {
	if path == "" {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, ".dispatch", "cache.db")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir cache dir: %w", err)
	}

	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	c := &Cache{db: db}
	if err := c.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return c, nil
}

func (c *Cache) Close() error { return c.db.Close() }

// migration is one numbered, named schema change. Migrations run in version
// order; each one is recorded in schema_migrations after a successful apply
// so subsequent runs skip it. ALTER TABLE ADD COLUMN errors with "duplicate
// column" are treated as idempotent success — common when migrating an
// already-deployed database to a baseline that includes its existing columns.
type migration struct {
	version     int
	description string
	stmts       []string
}

// migrations is the ordered list of schema changes. NEVER reorder, renumber,
// or delete entries — versions must be append-only so existing databases
// don't double-apply or skip ahead. To add a column or table, append a new
// entry with the next version number.
var migrations = []migration{
	{1, "baseline schema (extractions, messages, attachments, sync, worker_state, endpoint_activity, notes, rotations, cooldowns)", []string{
		`CREATE TABLE IF NOT EXISTS invoice_extractions (
			mailbox       TEXT NOT NULL,
			message_id    TEXT NOT NULL,
			extracted_at  TIMESTAMP NOT NULL,
			model         TEXT NOT NULL,
			po_no         INTEGER,
			invoice_data  TEXT,
			error_msg     TEXT,
			elapsed_ms    INTEGER,
			PRIMARY KEY (mailbox, message_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_invoice_extractions_po ON invoice_extractions(po_no)`,
		// Phase B: add po_lines snapshot and computed reconciliation. Both
		// as JSON blobs keyed alongside the extraction. Additive migration.
		`ALTER TABLE invoice_extractions ADD COLUMN po_lines_json TEXT`,
		`ALTER TABLE invoice_extractions ADD COLUMN reconcile_json TEXT`,
		// Rescan queue: set at store time when the extraction errored or the
		// verify verdict was too weak to be useful (no matches / all not_found).
		// Worker picks up flagged rows on subsequent passes.
		`ALTER TABLE invoice_extractions ADD COLUMN needs_rescan INTEGER DEFAULT 0`,
		`ALTER TABLE invoice_extractions ADD COLUMN rescan_attempts INTEGER DEFAULT 0`,
		`CREATE INDEX IF NOT EXISTS idx_extractions_rescan ON invoice_extractions(needs_rescan) WHERE needs_rescan = 1`,

		// Voucher tracking: populated by the P21 sync goroutine (read-only).
		// last_p21_sync_at is the cadence cursor. pay_status is one of:
		//   "" (unsynced yet), "unposted" (no apinv_hdr row found), "posted" (found, unpaid), "paid"
		`ALTER TABLE invoice_extractions ADD COLUMN voucher_no TEXT`,
		`ALTER TABLE invoice_extractions ADD COLUMN pay_status TEXT`,
		`ALTER TABLE invoice_extractions ADD COLUMN posted_at TIMESTAMP`,
		`ALTER TABLE invoice_extractions ADD COLUMN paid_at TIMESTAMP`,
		`ALTER TABLE invoice_extractions ADD COLUMN invoice_amount REAL`,
		`ALTER TABLE invoice_extractions ADD COLUMN check_no TEXT`,
		`ALTER TABLE invoice_extractions ADD COLUMN last_p21_sync_at TIMESTAMP`,
		`CREATE INDEX IF NOT EXISTS idx_extractions_pay_status ON invoice_extractions(pay_status)`,

		// Message cache: mailbox contents mirrored locally so the list view
		// can render without hitting Graph on every page load. Workflow state
		// (categories) still lives authoritatively in Outlook — this is pure cache.
		`CREATE TABLE IF NOT EXISTS messages (
			mailbox               TEXT NOT NULL,
			id                    TEXT NOT NULL,
			conversation_id       TEXT,
			internet_message_id   TEXT,
			subject               TEXT,
			sender_email          TEXT,
			sender_name           TEXT,
			received_at           TIMESTAMP,
			body_preview          TEXT,
			categories_json       TEXT,
			web_link              TEXT,
			has_attachments       INTEGER DEFAULT 0,
			last_synced_at        TIMESTAMP NOT NULL,
			PRIMARY KEY (mailbox, id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_received ON messages(mailbox, received_at DESC)`,
		// Phase 1 local cache: mirror full HTML/text bodies to local disk so
		// the detail view renders without hitting Graph, and Dispatch keeps
		// working during M365 outages. Filesystem-backed blobstore owns the
		// bytes; these columns are just pointers + freshness tracking.
		`ALTER TABLE messages ADD COLUMN body_html TEXT`,
		`ALTER TABLE messages ADD COLUMN body_text TEXT`,
		`ALTER TABLE messages ADD COLUMN last_full_body_fetch_at TIMESTAMP`,
		// attachments table: one row per (message, attachment). blob_sha is
		// the content hash — same bytes across many messages store once.
		// local_path is the canonical on-disk location (blobstore dedup path);
		// the by-message / by-vendor symlink trees are maintained alongside
		// but aren't tracked here (filesystem is the source of truth for them).
		`CREATE TABLE IF NOT EXISTS attachments (
			mailbox         TEXT NOT NULL,
			message_id      TEXT NOT NULL,
			attachment_id   TEXT NOT NULL,
			filename        TEXT NOT NULL,
			content_type    TEXT,
			size_bytes      INTEGER,
			blob_sha        TEXT,
			local_path      TEXT,
			stored_at       TIMESTAMP,
			last_error      TEXT,
			PRIMARY KEY (mailbox, message_id, attachment_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_attachments_sha ON attachments(blob_sha)`,
		`CREATE TABLE IF NOT EXISTS sync_status (
			mailbox          TEXT PRIMARY KEY,
			last_synced_at   TIMESTAMP NOT NULL,
			last_count       INTEGER NOT NULL,
			last_error       TEXT
		)`,
		// Phase 2 delta-sync state: the @odata.deltaLink returned by the
		// last successful /delta call. Stored per mailbox so we can resume
		// incremental sync across restarts. Empty = next sync will do a
		// full resync (which itself emits a delta link we save).
		`ALTER TABLE sync_status ADD COLUMN delta_link TEXT`,
		`ALTER TABLE sync_status ADD COLUMN delta_reset_at TIMESTAMP`,

		// Worker heartbeat. One row per goroutine slot — id is the slot index
		// (0..N-1). Worker updates its own slot on each message; clears
		// current_* when the slot finishes. Web reads all rows to render the
		// per-slot status card. run_started_at + processed_this_run live on
		// slot 0 only (run-level metadata, not per-slot).
		// Table is ephemeral — wiped on every StartRun().
		// pool differentiates sort/extract/fallback workers so the UI can
		// group them; (pool, slot) is the composite key, preventing sort
		// slot 0 from stomping extract slot 0's heartbeat.
		`CREATE TABLE IF NOT EXISTS worker_state (
			pool                       TEXT NOT NULL DEFAULT 'sort',
			slot                       INTEGER NOT NULL,
			mailbox                    TEXT,
			current_message_id         TEXT,
			current_step               TEXT,
			current_subject            TEXT,
			current_vendor             TEXT,
			current_started_at         TIMESTAMP,
			heartbeat_at               TIMESTAMP NOT NULL,
			run_started_at             TIMESTAMP,
			processed_this_run         INTEGER DEFAULT 0,
			last_completed_message_id  TEXT,
			last_completed_at          TIMESTAMP,
			PRIMARY KEY (pool, slot)
		)`,
		// Backward-compat migration: if the old singleton CHECK(id=1) table
		// exists, we can't DROP + recreate inside CREATE TABLE IF NOT EXISTS.
		// Detect via a probe INSERT; StartRun() handles the wipe anyway.

		// Endpoint activity. One row per Ollama endpoint URL. aiclass.Client
		// hooks populate current_* on request start and clear them on end;
		// last_* capture the most recent completion for idle-state display.
		// Totals let the UI show error rate and mean latency.
		`CREATE TABLE IF NOT EXISTS endpoint_activity (
			url                    TEXT PRIMARY KEY,
			current_message_id     TEXT,
			current_started_at     TIMESTAMP,
			last_completed_at      TIMESTAMP,
			last_duration_ms       INTEGER,
			last_error             TEXT,
			total_requests         INTEGER NOT NULL DEFAULT 0,
			total_errors           INTEGER NOT NULL DEFAULT 0,
			total_duration_ms      INTEGER NOT NULL DEFAULT 0
		)`,

		// Append-only notes per invoice. Clerks scribble context here that
		// doesn't fit the Outlook-category state machine — "called the vendor rep 4/27,
		// awaiting freight authorization." Read on detail render; small
		// indicator on the list row when count > 0. No edits or deletes:
		// keeps the audit trail honest, matches paper-AP's "you don't
		// erase the stamp" rule.
		`CREATE TABLE IF NOT EXISTS invoice_notes (
			note_uid       INTEGER PRIMARY KEY AUTOINCREMENT,
			mailbox        TEXT NOT NULL,
			message_id     TEXT NOT NULL,
			author         TEXT NOT NULL,
			body           TEXT NOT NULL,
			created_at     TIMESTAMP NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_invoice_notes_msg ON invoice_notes(mailbox, message_id, created_at)`,

		// Per-attachment rotation. When a clerk rotates a sideways-scanned
		// PDF in the detail viewer, save the angle so the next viewer
		// doesn't have to redo it. Keyed by (mailbox, message_id, attachment_id)
		// — granular enough that fixing one vendor's scan doesn't affect
		// other messages from the same vendor.
		`CREATE TABLE IF NOT EXISTS attachment_rotations (
			mailbox        TEXT NOT NULL,
			message_id     TEXT NOT NULL,
			attachment_id  TEXT NOT NULL,
			angle          INTEGER NOT NULL,
			set_at         TIMESTAMP NOT NULL,
			set_by         TEXT,
			PRIMARY KEY (mailbox, message_id, attachment_id)
		)`,

		// PDF cooldowns: after a tier runs (especially the 30-49 min CPU
		// fallback), stamp the content sha + tier with a next-allowed-at.
		// verify/fallback paths consult this before enqueueing so one
		// pathological PDF can't re-enter the pool and starve other work.
		// Keyed by content (sha256), not message, so the same vendor-resent
		// PDF across multiple mailboxes shares a cooldown.
		`CREATE TABLE IF NOT EXISTS pdf_cooldowns (
			pdf_sha256       TEXT NOT NULL,
			tier             INTEGER NOT NULL,
			next_allowed_at  TIMESTAMP NOT NULL,
			reason           TEXT,
			set_at           TIMESTAMP NOT NULL,
			PRIMARY KEY (pdf_sha256, tier)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_pdf_cooldowns_expiry ON pdf_cooldowns(next_allowed_at)`,
	}},
	{2, "follow-up timer for held messages", []string{
		// followup_at is set when a clerk Holds with a reason; the sweeper
		// reverts Status:Blocked → Status:New once the timestamp passes,
		// surfacing the message back into the Todo bucket. NULL = no timer.
		`ALTER TABLE messages ADD COLUMN followup_at TIMESTAMP`,
		`CREATE INDEX IF NOT EXISTS idx_messages_followup ON messages(mailbox, followup_at) WHERE followup_at IS NOT NULL`,
	}},
	// Phase 1 of the accuracy loop (see ACCURACY-LOOP.md). Append-only verdict
	// log per message. verdict ∈ {right, wrong, corrected}; corrected_data is
	// JSON-encoded clerk-supplied corrections when verdict='corrected', else
	// NULL. SQLite doesn't enforce the enum — the handler validates before
	// inserting. Same shape as invoice_notes (audit trail, no edits/deletes).
	{3, "clerk verdicts table for accuracy-loop Phase 1", []string{
		`CREATE TABLE IF NOT EXISTS clerk_verdicts (
			verdict_uid     INTEGER PRIMARY KEY AUTOINCREMENT,
			mailbox         TEXT NOT NULL,
			message_id      TEXT NOT NULL,
			user            TEXT NOT NULL,
			verdict         TEXT NOT NULL,
			corrected_data  TEXT,
			created_at      TIMESTAMP NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_verdicts_msg ON clerk_verdicts(mailbox, message_id, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_verdicts_recent ON clerk_verdicts(mailbox, created_at)`,
	}},
}

func (c *Cache) migrate() error {
	// Schema-version table. Records each applied migration so future runs
	// skip already-applied versions. Persistent across restarts; safe to
	// inspect via `SELECT * FROM schema_migrations` for diagnosis.
	if _, err := c.db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version     INTEGER PRIMARY KEY,
		description TEXT,
		applied_at  TIMESTAMP NOT NULL
	)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	var current int
	_ = c.db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current)

	for _, m := range migrations {
		if m.version <= current {
			continue
		}
		for _, s := range m.stmts {
			if _, err := c.db.Exec(s); err != nil {
				// ALTER TABLE ADD COLUMN errors if column already exists.
				// Treat "duplicate column" as idempotent success — common
				// on the FIRST run against a pre-existing database that
				// already had the column set added by the old non-versioned
				// migrate path.
				if strings.Contains(err.Error(), "duplicate column") {
					continue
				}
				return fmt.Errorf("migration v%d (%s): %w", m.version, m.description, err)
			}
		}
		if _, err := c.db.Exec(
			`INSERT INTO schema_migrations (version, description, applied_at) VALUES (?, ?, ?)`,
			m.version, m.description, time.Now().UTC()); err != nil {
			return fmt.Errorf("record migration v%d: %w", m.version, err)
		}
	}

	return c.rebuildLegacyWorkerStateIfNeeded()
}

// rebuildLegacyWorkerStateIfNeeded handles the one-off worker_state schema
// drift from before the sort/extract pool split. Conditional on table state
// rather than schema version — fires when the live table doesn't have a
// `pool` column or has the legacy CHECK(id=1) singleton constraint. Content
// is ephemeral (StartRun wipes it on every worker start) so dropping is
// safe. No-op on fresh databases (CREATE TABLE in v1 already produces the
// new shape).
func (c *Cache) rebuildLegacyWorkerStateIfNeeded() error {
	var existingSQL string
	_ = c.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='worker_state'`).Scan(&existingSQL)
	needsRebuild := strings.Contains(existingSQL, "CHECK (id = 1)") ||
		(existingSQL != "" && !strings.Contains(existingSQL, "pool"))
	if !needsRebuild {
		return nil
	}
	if _, err := c.db.Exec(`DROP TABLE worker_state`); err != nil {
		return fmt.Errorf("drop legacy worker_state: %w", err)
	}
	if _, err := c.db.Exec(`CREATE TABLE IF NOT EXISTS worker_state (
		pool                       TEXT NOT NULL DEFAULT 'sort',
		slot                       INTEGER NOT NULL,
		mailbox                    TEXT,
		current_message_id         TEXT,
		current_step               TEXT,
		current_subject            TEXT,
		current_vendor             TEXT,
		current_started_at         TIMESTAMP,
		heartbeat_at               TIMESTAMP NOT NULL,
		run_started_at             TIMESTAMP,
		processed_this_run         INTEGER DEFAULT 0,
		last_completed_message_id  TEXT,
		last_completed_at          TIMESTAMP,
		PRIMARY KEY (pool, slot)
	)`); err != nil {
		return fmt.Errorf("recreate worker_state: %w", err)
	}
	return nil
}

// InvoiceLine is a single extracted line item. Matches what the AI returns
// after prompt shaping; see aiclass for the source schema.
type InvoiceLine struct {
	ItemID      string  `json:"item_id"`
	Description string  `json:"description"`
	Qty         float64 `json:"qty"`
	UnitPrice   float64 `json:"unit_price"`
	Extended    float64 `json:"extended"`
}

// InvoiceData is the cached extraction. JSON-marshaled into invoice_data.
type InvoiceData struct {
	PONumber      string        `json:"po_number"`
	InvoiceNumber string        `json:"invoice_number"`
	InvoiceDate   string        `json:"invoice_date"`
	InvoiceTotal  float64       `json:"invoice_total"`
	Lines         []InvoiceLine `json:"lines"`
}

// InvoiceExtraction is a full row, for read back.
type InvoiceExtraction struct {
	Mailbox     string
	MessageID   string
	ExtractedAt time.Time
	Model       string
	PONo        int64
	Data        *InvoiceData // nil if error during extraction
	ErrorMsg    string
	ElapsedMs   int

	// Phase B additions. Cached snapshot of P21 po_line at extraction time
	// (so recon doesn't drift silently if P21 data changes later), plus the
	// computed per-line verdict. Opaque JSON at this layer — the recon
	// package owns the schema.
	POLinesJSON   string
	ReconcileJSON string

	// Voucher-tracking fields populated by the P21 sync. VoucherStatus is
	// empty string until first sync. Readers should null-check InvoiceAmount
	// (zero value is valid for credit memos etc) via the status field.
	VoucherNo            string
	VoucherStatus        string    // "" | "unposted" | "posted" | "paid"
	VoucherPostedAt      time.Time
	VoucherPaidAt        time.Time
	VoucherInvoiceAmount float64
	VoucherCheckNo       string
}

// StoreInvoiceExtraction upserts the extraction for a message. If data is nil,
// errorMsg must explain why (e.g., model returned garbage).
// needsRescan signals the rescan queue; callers compute it based on whether
// the extraction produced actionable output.
func (c *Cache) StoreInvoiceExtraction(ctx context.Context, mailbox, messageID, model string, poNo int64, data *InvoiceData, errorMsg string, elapsed time.Duration, needsRescan bool) error {
	var dataJSON sql.NullString
	if data != nil {
		b, err := json.Marshal(data)
		if err != nil {
			return fmt.Errorf("marshal invoice data: %w", err)
		}
		dataJSON = sql.NullString{String: string(b), Valid: true}
	}
	var poNoNull sql.NullInt64
	if poNo > 0 {
		poNoNull = sql.NullInt64{Int64: poNo, Valid: true}
	}
	rescanInt := 0
	if needsRescan {
		rescanInt = 1
	}
	// On UPDATE, increment rescan_attempts whenever we re-store a row (i.e.,
	// this IS a rescan). Keeps needs_rescan current to the latest verdict.
	_, err := c.db.ExecContext(ctx, `
		INSERT INTO invoice_extractions
			(mailbox, message_id, extracted_at, model, po_no, invoice_data, error_msg, elapsed_ms, needs_rescan, rescan_attempts)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0)
		ON CONFLICT(mailbox, message_id) DO UPDATE SET
			extracted_at=excluded.extracted_at,
			model=excluded.model,
			po_no=excluded.po_no,
			invoice_data=excluded.invoice_data,
			error_msg=excluded.error_msg,
			elapsed_ms=excluded.elapsed_ms,
			needs_rescan=excluded.needs_rescan,
			rescan_attempts=COALESCE(invoice_extractions.rescan_attempts,0) + 1
	`, mailbox, messageID, time.Now().UTC(), model, poNoNull, dataJSON, errorMsg, elapsed.Milliseconds(), rescanInt)
	return err
}

// ListRescanQueue returns message IDs flagged for rescan, capped. Used by the
// worker at the end of a normal pass to retry bad-match messages.
// Messages with too many attempts are skipped so we don't loop forever.
func (c *Cache) ListRescanQueue(ctx context.Context, mailbox string, maxAttempts, limit int) ([]string, error) {
	rows, err := c.db.QueryContext(ctx, `
		SELECT message_id FROM invoice_extractions
		WHERE mailbox = ? AND needs_rescan = 1 AND rescan_attempts < ?
		ORDER BY extracted_at ASC
		LIMIT ?
	`, mailbox, maxAttempts, limit)
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

// GetInvoiceExtraction returns the cached row for a message, or nil if absent.
func (c *Cache) GetInvoiceExtraction(ctx context.Context, mailbox, messageID string) (*InvoiceExtraction, error) {
	var (
		r           InvoiceExtraction
		dataJSON    sql.NullString
		errMsg      sql.NullString
		poNoNull    sql.NullInt64
		elapsed     sql.NullInt64
		poLinesJSON sql.NullString
		reconJSON   sql.NullString
		voucher     sql.NullString
		payStatus   sql.NullString
		postedAt    sql.NullTime
		paidAt      sql.NullTime
		invAmt      sql.NullFloat64
		checkNo     sql.NullString
	)
	err := c.db.QueryRowContext(ctx, `
		SELECT mailbox, message_id, extracted_at, model, po_no, invoice_data, error_msg, elapsed_ms,
		       po_lines_json, reconcile_json,
		       voucher_no, pay_status, posted_at, paid_at, invoice_amount, check_no
		FROM invoice_extractions WHERE mailbox = ? AND message_id = ?
	`, mailbox, messageID).Scan(&r.Mailbox, &r.MessageID, &r.ExtractedAt, &r.Model, &poNoNull, &dataJSON, &errMsg, &elapsed,
		&poLinesJSON, &reconJSON, &voucher, &payStatus, &postedAt, &paidAt, &invAmt, &checkNo)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if poNoNull.Valid {
		r.PONo = poNoNull.Int64
	}
	if errMsg.Valid {
		r.ErrorMsg = errMsg.String
	}
	if elapsed.Valid {
		r.ElapsedMs = int(elapsed.Int64)
	}
	if dataJSON.Valid {
		var d InvoiceData
		if err := json.Unmarshal([]byte(dataJSON.String), &d); err == nil {
			r.Data = &d
		}
	}
	if poLinesJSON.Valid {
		r.POLinesJSON = poLinesJSON.String
	}
	if reconJSON.Valid {
		r.ReconcileJSON = reconJSON.String
	}
	if voucher.Valid {
		r.VoucherNo = voucher.String
	}
	if payStatus.Valid {
		r.VoucherStatus = payStatus.String
	}
	if postedAt.Valid {
		r.VoucherPostedAt = postedAt.Time
	}
	if paidAt.Valid {
		r.VoucherPaidAt = paidAt.Time
	}
	if invAmt.Valid {
		r.VoucherInvoiceAmount = invAmt.Float64
	}
	if checkNo.Valid {
		r.VoucherCheckNo = checkNo.String
	}
	return &r, nil
}

// StoreReconciliation updates just the po_lines_json and reconcile_json
// columns on an existing invoice_extractions row. Call after StoreInvoiceExtraction.
func (c *Cache) StoreReconciliation(ctx context.Context, mailbox, messageID string, poLinesJSON, reconcileJSON string) error {
	_, err := c.db.ExecContext(ctx, `
		UPDATE invoice_extractions
		SET po_lines_json = ?, reconcile_json = ?
		WHERE mailbox = ? AND message_id = ?
	`, poLinesJSON, reconcileJSON, mailbox, messageID)
	return err
}

type Completion struct {
	MessageID    string
	Subject      string
	SenderEmail  string
	ExtractedAt  time.Time
	PONo         int64
	HasError     bool
	NeedsRescan  bool
	HasMatch     bool   // reconcile TotalMatch
	Model        string // e.g., "minicpm-v:latest" or "gemma4:26b (fallback)"
	ElapsedMs    int
}

// ListRecentCompletions returns the N most recently extracted messages,
// joined against messages for display fields.
func (c *Cache) ListRecentCompletions(ctx context.Context, mailbox string, limit int) ([]Completion, error) {
	rows, err := c.db.QueryContext(ctx, `
		SELECT e.message_id, COALESCE(m.subject,''), COALESCE(m.sender_email,''),
		       e.extracted_at, COALESCE(e.po_no, 0),
		       CASE WHEN e.error_msg IS NOT NULL AND e.error_msg != ''
		                 AND (e.model IS NULL OR e.model NOT LIKE 'cooldown:%')
		            THEN 1 ELSE 0 END,
		       COALESCE(e.needs_rescan, 0),
		       e.reconcile_json,
		       COALESCE(e.model,''),
		       COALESCE(e.elapsed_ms, 0)
		FROM invoice_extractions e
		LEFT JOIN messages m ON m.mailbox = e.mailbox AND m.id = e.message_id
		WHERE e.mailbox = ?
		ORDER BY e.extracted_at DESC
		LIMIT ?
	`, mailbox, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Completion{}
	for rows.Next() {
		var (
			co       Completion
			poNo     int64
			errFlag  int
			rescan   int
			reconRaw sql.NullString
			elapsed  int
		)
		if err := rows.Scan(&co.MessageID, &co.Subject, &co.SenderEmail,
			&co.ExtractedAt, &poNo, &errFlag, &rescan, &reconRaw, &co.Model, &elapsed); err != nil {
			return nil, err
		}
		co.PONo = poNo
		co.HasError = errFlag == 1
		co.NeedsRescan = rescan == 1
		co.ElapsedMs = elapsed
		if reconRaw.Valid && len(reconRaw.String) > 2 {
			var peek struct {
				TotalMatch bool `json:"total_match"`
			}
			if err := json.Unmarshal([]byte(reconRaw.String), &peek); err == nil {
				co.HasMatch = peek.TotalMatch
			}
		}
		out = append(out, co)
	}
	return out, rows.Err()
}


// RelatedMessage is a compact neighbor for the detail page's "Related" card —
// another message in the mailbox that touches the same PO.
type RelatedMessage struct {
	MessageID   string
	Subject     string
	SenderEmail string
	ReceivedAt  time.Time
	PONo        int64
	Internal    bool
}

// ListRelatedByPO returns messages (other than the one identified by
// excludeID) that have an invoice_extractions row for the given po_no.
// Ordered newest-first. Used by the detail page to surface the vendor invoice
// when viewing an internal reply, and vice-versa.
func (c *Cache) ListRelatedByPO(ctx context.Context, mailbox string, poNo int64, excludeID string, limit int) ([]RelatedMessage, error) {
	if poNo <= 0 {
		return nil, nil
	}
	rows, err := c.db.QueryContext(ctx, `
		SELECT e.message_id, COALESCE(m.subject,''), COALESCE(m.sender_email,''),
		       m.received_at, e.po_no
		FROM invoice_extractions e
		LEFT JOIN messages m ON m.mailbox = e.mailbox AND m.id = e.message_id
		WHERE e.mailbox = ? AND e.po_no = ? AND e.message_id != ?
		ORDER BY m.received_at DESC
		LIMIT ?
	`, mailbox, poNo, excludeID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RelatedMessage{}
	for rows.Next() {
		var r RelatedMessage
		var received sql.NullTime
		if err := rows.Scan(&r.MessageID, &r.Subject, &r.SenderEmail, &received, &r.PONo); err != nil {
			return nil, err
		}
		if received.Valid {
			r.ReceivedAt = received.Time
		}
		r.Internal = strings.HasSuffix(strings.ToLower(r.SenderEmail), "@example.com")
		out = append(out, r)
	}
	return out, rows.Err()
}

// ModelMix returns per-model counts over a time window. Used by the admin
// page to show "what tier is doing the work lately." Time-windowed (not
// last-N) so a slow week with few completions still shows a representative
// slice. Two weeks is the default — enough to smooth daily variation.
func (c *Cache) ModelMix(ctx context.Context, mailbox string, since time.Duration) (map[string]int, error) {
	cutoff := time.Now().Add(-since).UTC()
	rows, err := c.db.QueryContext(ctx, `
		SELECT model, COUNT(*)
		FROM invoice_extractions
		WHERE mailbox = ?
		  AND extracted_at >= ?
		  AND model IS NOT NULL AND model != ''
		  AND model NOT LIKE 'skip:%'
		  AND model NOT LIKE 'cooldown:%'
		GROUP BY model
	`, mailbox, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var m string
		var n int
		if err := rows.Scan(&m, &n); err != nil {
			return nil, err
		}
		out[m] = n
	}
	return out, rows.Err()
}

// ModelStatsRow is one per-model summary for the queue page's Model
// Performance card. Fields mirror cmd/dispatch-web/modelStats; we keep
// the shape here so the SQL-to-struct mapping is co-located.
type ModelStatsRow struct {
	Model     string
	Total     int
	Clean     int // needs_rescan=0 AND error_msg=''
	Rescanned int // needs_rescan=1
	Errored   int // error_msg != ''
	TotalMs   int // sum elapsed_ms (caller computes mean)
}

// ModelStatsBreakdown returns per-model extraction outcome stats for a
// mailbox. Honest end-to-end view: a model's "clean" count is what made
// it past reconciliation without getting flagged for another rescan.
// Sorted by total descending so the heaviest-used tier is first.
func (c *Cache) ModelStatsBreakdown(ctx context.Context, mailbox string) ([]ModelStatsRow, error) {
	// Exclude pseudo-rows that aren't actual AI inference: stubs (sort
	// pool overflow, transient), skip:* (Automation/Internal — clerk
	// shouldn't even see these), cooldown:* (PDF cooldown shortcuts).
	// They distort avg-latency and error rates in a "model performance"
	// view because they're either zero-elapsed or transient.
	rows, err := c.db.QueryContext(ctx, `
		SELECT model,
		       COUNT(*),
		       SUM(CASE WHEN needs_rescan = 0 AND (error_msg IS NULL OR error_msg = '') THEN 1 ELSE 0 END),
		       SUM(CASE WHEN needs_rescan = 1 THEN 1 ELSE 0 END),
		       SUM(CASE WHEN error_msg IS NOT NULL AND error_msg != '' THEN 1 ELSE 0 END),
		       COALESCE(SUM(elapsed_ms), 0)
		FROM invoice_extractions
		WHERE mailbox = ?
		  AND model IS NOT NULL AND model != ''
		  AND model NOT LIKE 'skip:%'
		  AND model NOT LIKE 'cooldown:%'
		GROUP BY model
		ORDER BY COUNT(*) DESC
	`, mailbox)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ModelStatsRow
	for rows.Next() {
		var r ModelStatsRow
		if err := rows.Scan(&r.Model, &r.Total, &r.Clean, &r.Rescanned, &r.Errored, &r.TotalMs); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PendingInboxCount returns how many messages the worker has not yet seen at
// all — i.e., no Status: category tag. This is the "real" pending number.
// Non-invoice mail (marketing, statements, internal) gets Status: tagged
// during the worker's first pass and drops out of this count even though it
// never produces an extraction row. Use UnextractedCount for the broader
// "no cache row" view.
func (c *Cache) PendingInboxCount(ctx context.Context, mailbox string) (int, error) {
	var n int
	err := c.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM messages
		WHERE mailbox = ?
		  AND (categories_json IS NULL OR categories_json NOT LIKE '%Status:%')
	`, mailbox).Scan(&n)
	return n, err
}

// UnextractedCount returns messages lacking an invoice_extractions row. Mostly
// non-invoice mail; exposed for observability/debug rather than queue sizing.
func (c *Cache) UnextractedCount(ctx context.Context, mailbox string) (int, error) {
	var n int
	err := c.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM messages m
		WHERE m.mailbox = ?
		  AND NOT EXISTS (
		      SELECT 1 FROM invoice_extractions e
		      WHERE e.mailbox = m.mailbox AND e.message_id = m.id
		  )
	`, mailbox).Scan(&n)
	return n, err
}

// RescanQueueDepth returns how many messages are currently flagged for rescan
// and still under the attempt cap.
func (c *Cache) RescanQueueDepth(ctx context.Context, mailbox string, maxAttempts int) (int, error) {
	var n int
	err := c.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM invoice_extractions
		WHERE mailbox = ? AND needs_rescan = 1 AND rescan_attempts < ?
	`, mailbox, maxAttempts).Scan(&n)
	return n, err
}

// MarkRescanExhausted clears needs_rescan for an extraction we can't
// improve further — e.g., a tier-4 result we've already reached the top
// tier on. Without this, the rescan queue keeps re-picking the item,
// skipping it, and never draining.
func (c *Cache) MarkRescanExhausted(ctx context.Context, mailbox, messageID string) error {
	_, err := c.db.ExecContext(ctx, `
		UPDATE invoice_extractions SET needs_rescan = 0
		WHERE mailbox = ? AND message_id = ?
	`, mailbox, messageID)
	return err
}

// SweepOrphanedRescans clears needs_rescan on any row whose rescan_attempts
// has passed the cap. These are rows ListRescanQueue already filters out but
// MarkRescanExhausted never got to clear (because it only fires when a row
// still passes the cap at pickup time). Without this sweep, the UI's
// "pending rescan" count stays artificially high after the channel-duplicate
// bug enqueued the same row many times. Returns rows affected.
func (c *Cache) SweepOrphanedRescans(ctx context.Context, mailbox string, maxAttempts int) (int64, error) {
	res, err := c.db.ExecContext(ctx, `
		UPDATE invoice_extractions SET needs_rescan = 0
		WHERE mailbox = ? AND needs_rescan = 1 AND rescan_attempts >= ?
	`, mailbox, maxAttempts)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// StubExtractionForRescan writes a placeholder extraction row marking
// (mailbox, message_id) as needing extraction. Used by the sort pool
// when the extract pool's channel is full — the sort worker refuses to
// block, so it drops a stub that the rescan pass will pick up.
// No-op if a real extraction already exists (ON CONFLICT skips update).
func (c *Cache) StubExtractionForRescan(ctx context.Context, mailbox, messageID string, poNo int64) error {
	_, err := c.db.ExecContext(ctx, `
		INSERT INTO invoice_extractions
			(mailbox, message_id, extracted_at, model, po_no, invoice_data, error_msg, elapsed_ms, needs_rescan, rescan_attempts)
		VALUES (?, ?, ?, '', ?, NULL, 'queued: extract pool full', 0, 1, 0)
		ON CONFLICT(mailbox, message_id) DO NOTHING
	`, mailbox, messageID, time.Now().UTC(), sql.NullInt64{Int64: poNo, Valid: poNo > 0})
	return err
}

// MarkSkipExtraction writes a zero-cost extraction row marking a message as
// deliberately-not-invoice (automation senders, internal chatter, etc.). Keeps
// UnextractedCount accurate: "we looked, decided it's not extractable work"
// is different from "we haven't looked yet." Reason is baked into model as
// "skip:<Reason>" so the queue views and /detail can display it.
//
// DO NOTHING on conflict — once a message is skipped, it stays skipped; any
// later re-examination that produced a real extraction would have taken a
// different code path (fresh-sort HasInvoiceExtraction guard).
func (c *Cache) MarkSkipExtraction(ctx context.Context, mailbox, messageID, reason string) error {
	model := "skip:" + reason
	_, err := c.db.ExecContext(ctx, `
		INSERT INTO invoice_extractions
			(mailbox, message_id, extracted_at, model, po_no, invoice_data, error_msg, elapsed_ms, needs_rescan, rescan_attempts)
		VALUES (?, ?, ?, ?, NULL, NULL, '', 0, 0, 0)
		ON CONFLICT(mailbox, message_id) DO NOTHING
	`, mailbox, messageID, time.Now().UTC(), model)
	return err
}

// VendorHistoryRow is one entry in the per-vendor mini-history shown on the
// detail page. Compact projection of message + extraction + recon + voucher
// state — enough for a clerk to skim "is this vendor normally smooth?"
type VendorHistoryRow struct {
	MessageID    string
	Subject      string
	Received     time.Time
	PONo         int64
	InvoiceNo    string
	InvoiceTotal float64
	PayStatus    string // "" / unposted / posted / paid
	VoucherNo    string
	HasRecon     bool
	TotalMatch   bool
	HadIssue     bool // recon ran and found a mismatch
}

// VendorHistorySummary is the aggregate stats line above the per-row list.
// Computed in the same SQL as the rows so the caller can show "5 invoices,
// 4 clean, 1 disputed" at a glance.
type VendorHistorySummary struct {
	TotalCount   int
	CleanCount   int // recon ran AND TotalMatch AND no AnyLineMismatch
	IssueCount   int // recon found a mismatch
	UnpostedCount int
	PostedCount   int
	PaidCount     int
}

// GetVendorHistory returns the most recent N invoices from the same vendor
// (excluding the current message), plus a summary of how that vendor's
// invoices have historically reconciled. Vendor match is by exact category
// value ("Vendor: Sample-Distributor LLC" etc) — the same way filters identify it.
//
// Returns (nil, nil, nil) when vendor is empty / "Unknown" — those would
// span hundreds of unrelated senders and aren't useful as history.
func (c *Cache) GetVendorHistory(ctx context.Context, mailbox, vendor, currentMessageID string, limit int) ([]VendorHistoryRow, VendorHistorySummary, error) {
	vendor = strings.TrimSpace(vendor)
	if vendor == "" || strings.EqualFold(vendor, "Unknown") {
		return nil, VendorHistorySummary{}, nil
	}
	pattern := "%\"Vendor: " + vendor + "\"%"

	// One query for the rows, one for the aggregates. Both filter on the
	// same Vendor: tag — counts on the full history, rows capped at limit.
	rowsQ := `
		SELECT m.id, m.subject, m.received_at,
		       COALESCE(e.po_no, 0),
		       COALESCE(e.invoice_data, ''),
		       COALESCE(e.reconcile_json, ''),
		       COALESCE(e.pay_status, ''),
		       COALESCE(e.voucher_no, '')
		FROM messages m
		LEFT JOIN invoice_extractions e ON e.mailbox = m.mailbox AND e.message_id = m.id
		WHERE m.mailbox = ? AND m.id != ?
		  AND m.categories_json LIKE ?
		ORDER BY m.received_at DESC
		LIMIT ?
	`
	rowResult, err := c.db.QueryContext(ctx, rowsQ, mailbox, currentMessageID, pattern, limit)
	if err != nil {
		return nil, VendorHistorySummary{}, err
	}
	defer rowResult.Close()

	out := []VendorHistoryRow{}
	for rowResult.Next() {
		var (
			id        string
			subject   string
			received  time.Time
			poNo      int64
			invData   string
			reconJSON string
			payStatus string
			voucherNo string
		)
		if err := rowResult.Scan(&id, &subject, &received, &poNo, &invData, &reconJSON, &payStatus, &voucherNo); err != nil {
			return nil, VendorHistorySummary{}, err
		}
		row := VendorHistoryRow{
			MessageID: id, Subject: subject, Received: received,
			PONo: poNo, PayStatus: payStatus, VoucherNo: voucherNo,
		}
		if invData != "" {
			var d InvoiceData
			if err := json.Unmarshal([]byte(invData), &d); err == nil {
				row.InvoiceNo = d.InvoiceNumber
				row.InvoiceTotal = d.InvoiceTotal
			}
		}
		if reconJSON != "" {
			row.HasRecon = true
			// Cheap field-presence check rather than a full unmarshal — we
			// only need TotalMatch + AnyLineMismatch which are top-level.
			row.TotalMatch = strings.Contains(reconJSON, `"total_match":true`)
			row.HadIssue = strings.Contains(reconJSON, `"any_line_mismatch":true`)
		}
		out = append(out, row)
	}
	if err := rowResult.Err(); err != nil {
		return nil, VendorHistorySummary{}, err
	}

	// Aggregate counts across the FULL history (not just the limited rows).
	aggQ := `
		SELECT
		  COUNT(*) AS total,
		  SUM(CASE WHEN e.reconcile_json LIKE '%"total_match":true%'
		            AND e.reconcile_json NOT LIKE '%"any_line_mismatch":true%'
		           THEN 1 ELSE 0 END) AS clean,
		  SUM(CASE WHEN e.reconcile_json LIKE '%"any_line_mismatch":true%'
		           THEN 1 ELSE 0 END) AS issue,
		  SUM(CASE WHEN e.pay_status = 'unposted' THEN 1 ELSE 0 END) AS unposted,
		  SUM(CASE WHEN e.pay_status = 'posted'   THEN 1 ELSE 0 END) AS posted,
		  SUM(CASE WHEN e.pay_status = 'paid'     THEN 1 ELSE 0 END) AS paid
		FROM messages m
		LEFT JOIN invoice_extractions e ON e.mailbox = m.mailbox AND e.message_id = m.id
		WHERE m.mailbox = ? AND m.id != ?
		  AND m.categories_json LIKE ?
	`
	var sum VendorHistorySummary
	row := c.db.QueryRowContext(ctx, aggQ, mailbox, currentMessageID, pattern)
	var total, clean, issue, unposted, posted, paid sql.NullInt64
	if err := row.Scan(&total, &clean, &issue, &unposted, &posted, &paid); err != nil && err != sql.ErrNoRows {
		return nil, VendorHistorySummary{}, err
	}
	if total.Valid {
		sum.TotalCount = int(total.Int64)
	}
	if clean.Valid {
		sum.CleanCount = int(clean.Int64)
	}
	if issue.Valid {
		sum.IssueCount = int(issue.Int64)
	}
	if unposted.Valid {
		sum.UnpostedCount = int(unposted.Int64)
	}
	if posted.Valid {
		sum.PostedCount = int(posted.Int64)
	}
	if paid.Valid {
		sum.PaidCount = int(paid.Int64)
	}
	return out, sum, nil
}


// AISummary is a compact per-message status used by the list view.
// Populated only when a cache row exists.
type AISummary struct {
	Processed       bool // any cache row exists (success or failure)
	HasExtraction   bool // invoice_data is non-null (the AI extracted something)
	HasReconcile    bool // reconcile_json is non-null
	TotalMatch      bool // recon.TotalMatch
	AnyLineMismatch bool // recon.AnyLineMismatch
	ErrorMsg        string
	NeedsRescan     bool // flagged for the rescan queue (anything not a clean match)
	PONo            int64 // resolved P21 PO number; 0 when no PO matched
	InvoiceAmount   float64 // extracted invoice total (preferred) or P21 voucher amount; 0 when neither known
	// Voucher fields (populated by P21 sync — empty until first sync tick)
	PayStatus       string // "" | "unposted" | "posted" | "paid"
	VoucherNo       string
}

// ListAISummaryForMailbox returns one summary per message that has a cache row.
// Designed for the list render: one SQL query covers all messages, caller
// looks up by message_id. O(rows) but all-local; SQLite is fast.
func (c *Cache) ListAISummaryForMailbox(ctx context.Context, mailbox string) (map[string]AISummary, error) {
	rows, err := c.db.QueryContext(ctx, `
		SELECT message_id, invoice_data, reconcile_json, error_msg, needs_rescan,
		       COALESCE(pay_status,''), COALESCE(voucher_no,''), COALESCE(po_no, 0),
		       COALESCE(invoice_amount, 0)
		FROM invoice_extractions WHERE mailbox = ?
	`, mailbox)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]AISummary)
	for rows.Next() {
		var (
			msgID         string
			dataJSON      sql.NullString
			reconJSON     sql.NullString
			errMsg        sql.NullString
			needsRescan   sql.NullInt64
			payStatus     string
			voucherNo     string
			poNo          int64
			invoiceAmount float64
		)
		if err := rows.Scan(&msgID, &dataJSON, &reconJSON, &errMsg, &needsRescan, &payStatus, &voucherNo, &poNo, &invoiceAmount); err != nil {
			return nil, err
		}
		s := AISummary{
			Processed:     true,
			NeedsRescan:   needsRescan.Valid && needsRescan.Int64 > 0,
			PayStatus:     payStatus,
			VoucherNo:     voucherNo,
			PONo:          poNo,
			InvoiceAmount: invoiceAmount,
		}
		if dataJSON.Valid && len(dataJSON.String) > 2 {
			s.HasExtraction = true
			// Pull invoice_total out of the extracted JSON when the P21
			// voucher amount isn't yet known. Cheap parse — every row.
			if s.InvoiceAmount == 0 {
				var d InvoiceData
				if err := json.Unmarshal([]byte(dataJSON.String), &d); err == nil {
					s.InvoiceAmount = d.InvoiceTotal
				}
			}
		}
		if errMsg.Valid {
			s.ErrorMsg = errMsg.String
		}
		if reconJSON.Valid && len(reconJSON.String) > 2 {
			s.HasReconcile = true
			// Shallow peek at the two flags we care about without unmarshaling
			// the whole struct. Works because Go json.Marshal doesn't pretty-print.
			var peek struct {
				TotalMatch      bool `json:"total_match"`
				AnyLineMismatch bool `json:"any_line_mismatch"`
			}
			if err := json.Unmarshal([]byte(reconJSON.String), &peek); err == nil {
				s.TotalMatch = peek.TotalMatch
				s.AnyLineMismatch = peek.AnyLineMismatch
			}
		}
		out[msgID] = s
	}
	return out, rows.Err()
}

// BlockedMessage is the per-row payload returned by ListBlockedMessages.
// Carries enough context to re-run recon without joining tables in the caller.
type BlockedMessage struct {
	MessageID   string
	PONo        int64
	InvoiceData *InvoiceData // nil if no successful extraction yet
}

// ListPendingFirstPass returns IDs of messages with no Status: tag at all —
// the worker hasn't touched them. Capped to a recent time window because
// truly old untouched messages are usually permanent edge cases (catchall
// addresses, weird sender format) and clog the drill-down.
func (c *Cache) ListPendingFirstPass(ctx context.Context, mailbox string, since time.Duration, limit int) ([]string, error) {
	cutoff := time.Now().Add(-since).UTC()
	rows, err := c.db.QueryContext(ctx, `
		SELECT id FROM messages
		WHERE mailbox = ?
		  AND received_at >= ?
		  AND (categories_json IS NULL OR categories_json NOT LIKE '%Status:%')
		ORDER BY received_at DESC
		LIMIT ?
	`, mailbox, cutoff, limit)
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

// ListErroredExtractions returns IDs of messages whose extraction has a
// non-empty error_msg in the recent window. Drives the admin "errored"
// drill-down. Excludes blank error_msg (we sweep stale errors elsewhere),
// transient stubs (`queued:` prefix) which the rescan pass will drain
// without admin intervention, and cooldown stubs (model `cooldown:%`)
// which are state markers — not failures — that the worker writes when
// escalation is suppressed by an active SHA-keyed cooldown.
func (c *Cache) ListErroredExtractions(ctx context.Context, mailbox string, since time.Duration, limit int) ([]string, error) {
	cutoff := time.Now().Add(-since).UTC()
	rows, err := c.db.QueryContext(ctx, `
		SELECT message_id FROM invoice_extractions
		WHERE mailbox = ?
		  AND extracted_at >= ?
		  AND error_msg IS NOT NULL AND error_msg != ''
		  AND error_msg NOT LIKE 'queued:%'
		  AND (model IS NULL OR model NOT LIKE 'cooldown:%')
		ORDER BY extracted_at DESC
		LIMIT ?
	`, mailbox, cutoff, limit)
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

// ListRescanQueueRecent is the rescan-queue counterpart to the drill-downs.
// Bounds to a time window AND the existing attempts cap so the page isn't
// poisoned by stale rows the cooldown sweeper would already filter.
func (c *Cache) ListRescanQueueRecent(ctx context.Context, mailbox string, since time.Duration, maxAttempts, limit int) ([]string, error) {
	cutoff := time.Now().Add(-since).UTC()
	rows, err := c.db.QueryContext(ctx, `
		SELECT e.message_id
		FROM invoice_extractions e
		JOIN messages m ON m.mailbox = e.mailbox AND m.id = e.message_id
		WHERE e.mailbox = ?
		  AND m.received_at >= ?
		  AND e.needs_rescan = 1
		  AND e.rescan_attempts < ?
		ORDER BY e.extracted_at DESC
		LIMIT ?
	`, mailbox, cutoff, maxAttempts, limit)
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

// ListBlockedMessages returns Status:Blocked rows that have a usable cached
// extraction (PO + invoice data). Used by the auto-recheck pass that re-runs
// recon against fresh P21 PO lines to see whether buyer/vendor actions have
// resolved the discrepancy. Skips rows without an invoice extraction —
// those have no recon to recheck.
func (c *Cache) ListBlockedMessages(ctx context.Context, mailbox string) ([]BlockedMessage, error) {
	rows, err := c.db.QueryContext(ctx, `
		SELECT m.id, e.po_no, e.invoice_data
		FROM messages m
		JOIN invoice_extractions e ON e.mailbox = m.mailbox AND e.message_id = m.id
		WHERE m.mailbox = ?
		  AND m.categories_json LIKE '%"Status: Blocked"%'
		  AND e.po_no IS NOT NULL AND e.po_no > 0
		  AND e.invoice_data IS NOT NULL AND e.invoice_data != ''
		  AND (e.error_msg IS NULL OR e.error_msg = '')
	`, mailbox)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BlockedMessage{}
	for rows.Next() {
		var (
			id       string
			poNo     sql.NullInt64
			dataJSON sql.NullString
		)
		if err := rows.Scan(&id, &poNo, &dataJSON); err != nil {
			return nil, err
		}
		bm := BlockedMessage{MessageID: id}
		if poNo.Valid {
			bm.PONo = poNo.Int64
		}
		if dataJSON.Valid && dataJSON.String != "" {
			var d InvoiceData
			if err := json.Unmarshal([]byte(dataJSON.String), &d); err == nil {
				bm.InvoiceData = &d
			}
		}
		if bm.InvoiceData == nil {
			continue
		}
		out = append(out, bm)
	}
	return out, rows.Err()
}


// GetCategories returns the cached categories for a message. Used by voucher
// sync to decide whether a Status: Done patch is needed without spending a
// Graph round-trip per row. Returns ([], nil) if the row isn't cached.
func (c *Cache) GetCategories(ctx context.Context, mailbox, messageID string) ([]string, error) {
	var catsJSON sql.NullString
	err := c.db.QueryRowContext(ctx,
		`SELECT categories_json FROM messages WHERE mailbox = ? AND id = ?`,
		mailbox, messageID).Scan(&catsJSON)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !catsJSON.Valid || catsJSON.String == "" {
		return []string{}, nil
	}
	var cats []string
	if err := json.Unmarshal([]byte(catsJSON.String), &cats); err != nil {
		return nil, err
	}
	return cats, nil
}

// MessageMeta is a parsed view of a message's category-derived metadata —
// vendor / buyer / kind / status / blockers. Drives the admin overview
// leaderboards. Categories live as a JSON array of "Prefix: Value" strings;
// this struct flattens them so the analytics handler doesn't have to re-parse.
type MessageMeta struct {
	ID         string
	Vendor     string
	Buyer      string
	Kind       string
	Status     string
	Blockers   []string
	ReceivedAt time.Time
}

// RecentMessageMeta returns parsed message metadata for messages received
// since `since`. Mailbox-scoped. Used by the admin overview to compute
// vendor/buyer leaderboards by Kind/Status/Blocker.
func (c *Cache) RecentMessageMeta(ctx context.Context, mailbox string, since time.Time) ([]MessageMeta, error) {
	rows, err := c.db.QueryContext(ctx, `
		SELECT id, COALESCE(categories_json, '[]'), received_at
		FROM messages
		WHERE mailbox = ? AND received_at >= ?
		ORDER BY received_at DESC
	`, mailbox, since.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MessageMeta{}
	for rows.Next() {
		var m MessageMeta
		var catsJSON string
		if err := rows.Scan(&m.ID, &catsJSON, &m.ReceivedAt); err != nil {
			return nil, err
		}
		var cats []string
		if err := json.Unmarshal([]byte(catsJSON), &cats); err != nil {
			cats = nil
		}
		for _, c := range cats {
			switch {
			case strings.HasPrefix(c, "Vendor: "):
				m.Vendor = strings.TrimPrefix(c, "Vendor: ")
			case strings.HasPrefix(c, "Buyer: "):
				m.Buyer = strings.TrimPrefix(c, "Buyer: ")
			case strings.HasPrefix(c, "Kind: "):
				m.Kind = strings.TrimPrefix(c, "Kind: ")
			case strings.HasPrefix(c, "Status: "):
				m.Status = strings.TrimPrefix(c, "Status: ")
			case strings.HasPrefix(c, "Blocker: "):
				m.Blockers = append(m.Blockers, strings.TrimPrefix(c, "Blocker: "))
			}
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// UpdateCategories is the hot-path call used after a successful PATCH to Graph:
// it refreshes just the categories on a cached message so the UI sees the
// change immediately rather than waiting for the next sync cycle.
func (c *Cache) UpdateCategories(ctx context.Context, mailbox, messageID string, categories []string) error {
	catsJSON, _ := json.Marshal(categories)
	_, err := c.db.ExecContext(ctx,
		`UPDATE messages SET categories_json = ?, last_synced_at = ?
		 WHERE mailbox = ? AND id = ?`,
		string(catsJSON), time.Now().UTC(), mailbox, messageID)
	return err
}

// RecordSync updates the sync_status row for a mailbox. Empty errMsg means success.
func (c *Cache) RecordSync(ctx context.Context, mailbox string, count int, errMsg string) error {
	_, err := c.db.ExecContext(ctx, `
		INSERT INTO sync_status (mailbox, last_synced_at, last_count, last_error)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(mailbox) DO UPDATE SET
			last_synced_at = excluded.last_synced_at,
			last_count     = excluded.last_count,
			last_error     = excluded.last_error
	`, mailbox, time.Now().UTC(), count, errMsg)
	return err
}

// LastSync returns the most recent sync timestamp + stats for a mailbox,
// or zero value if never synced.
func (c *Cache) LastSync(ctx context.Context, mailbox string) (at time.Time, count int, errMsg string, err error) {
	var e sql.NullString
	err = c.db.QueryRowContext(ctx,
		`SELECT last_synced_at, last_count, last_error FROM sync_status WHERE mailbox = ?`,
		mailbox).Scan(&at, &count, &e)
	if err == sql.ErrNoRows {
		return time.Time{}, 0, "", nil
	}
	if e.Valid {
		errMsg = e.String
	}
	return
}

// ConversationPrior captures what we already know about a conversation
// from a previously-processed message. Used by the worker to skip
// classify/PO-discovery work when a reply shows up in a thread we've
// already tagged. Fields are zero-valued when no prior exists.
type ConversationPrior struct {
	Vendor string
	Kind   string
	PoNo   int64  // last resolved PO in the thread (0 if never resolved)
	Buyer  string // P21 buyer from the last PO lookup
}

// GetConversationPrior returns the best-available prior for a
// conversationId. We look at the most recent previously-tagged sibling
// in the same conversation (excluding the current message). If nothing
// found, all fields are empty / zero.
//
// "Tagged" = has any category. A message with Vendor: and Status: tags
// came from a worker pass; one without hasn't been touched yet and
// carries no prior.
func (c *Cache) GetConversationPrior(ctx context.Context, mailbox, conversationID, excludeMessageID string) (ConversationPrior, error) {
	if conversationID == "" {
		return ConversationPrior{}, nil
	}
	var p ConversationPrior
	var catsJSON sql.NullString
	err := c.db.QueryRowContext(ctx, `
		SELECT COALESCE(categories_json,'')
		FROM messages
		WHERE mailbox = ?
		  AND conversation_id = ?
		  AND id != ?
		  AND categories_json IS NOT NULL
		  AND categories_json LIKE '%Vendor:%'
		ORDER BY received_at DESC
		LIMIT 1
	`, mailbox, conversationID, excludeMessageID).Scan(&catsJSON)
	if err == sql.ErrNoRows {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	// Parse Vendor / Kind / Buyer out of the JSON array of category strings.
	var cats []string
	_ = json.Unmarshal([]byte(catsJSON.String), &cats)
	for _, cat := range cats {
		switch {
		case strings.HasPrefix(cat, "Vendor: "):
			p.Vendor = strings.TrimPrefix(cat, "Vendor: ")
		case strings.HasPrefix(cat, "Kind: "):
			p.Kind = strings.TrimPrefix(cat, "Kind: ")
		case strings.HasPrefix(cat, "Buyer: "):
			p.Buyer = strings.TrimPrefix(cat, "Buyer: ")
		}
	}
	// Also look at the sibling's invoice_extractions row to inherit PoNo.
	var poNo sql.NullInt64
	_ = c.db.QueryRowContext(ctx, `
		SELECT e.po_no
		FROM messages m
		JOIN invoice_extractions e ON e.mailbox = m.mailbox AND e.message_id = m.id
		WHERE m.mailbox = ?
		  AND m.conversation_id = ?
		  AND m.id != ?
		  AND e.po_no IS NOT NULL
		ORDER BY m.received_at DESC
		LIMIT 1
	`, mailbox, conversationID, excludeMessageID).Scan(&poNo)
	if poNo.Valid {
		p.PoNo = poNo.Int64
	}
	return p, nil
}

// GetDeltaLink returns the last-saved Graph @odata.deltaLink for a mailbox,
// or empty string if never saved. Callers treat empty as "do a full sync".
func (c *Cache) GetDeltaLink(ctx context.Context, mailbox string) (string, error) {
	var link sql.NullString
	err := c.db.QueryRowContext(ctx,
		`SELECT delta_link FROM sync_status WHERE mailbox = ?`, mailbox).Scan(&link)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if !link.Valid {
		return "", nil
	}
	return link.String, nil
}

// SetDeltaLink saves the next delta link returned by Graph. Upserts the
// sync_status row so first-time callers work too.
func (c *Cache) SetDeltaLink(ctx context.Context, mailbox, link string) error {
	_, err := c.db.ExecContext(ctx, `
		INSERT INTO sync_status (mailbox, last_synced_at, last_count, last_error, delta_link)
		VALUES (?, ?, 0, '', ?)
		ON CONFLICT(mailbox) DO UPDATE SET delta_link = excluded.delta_link
	`, mailbox, time.Now().UTC(), link)
	return err
}

// ListUnmirroredMessages returns cached messages that need mirror backfill:
// either the full body was never fetched, OR Graph flagged attachments on
// the message but we don't have any attachment rows for it. Ordered newest-
// first so backfill focuses on the work the clerk's most likely to look
// at in the UI. limit caps the batch size so a huge backlog doesn't
// monopolize the mirror pool — call repeatedly on a ticker.
func (c *Cache) ListUnmirroredMessages(ctx context.Context, mailbox string, limit int) ([]CachedMessage, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := c.db.QueryContext(ctx, `
		SELECT m.mailbox, m.id, m.conversation_id, COALESCE(m.internet_message_id,''),
		       COALESCE(m.subject,''), COALESCE(m.sender_email,''), COALESCE(m.sender_name,''),
		       m.received_at, COALESCE(m.body_preview,''), COALESCE(m.categories_json,'[]'),
		       COALESCE(m.web_link,''), m.has_attachments, m.last_synced_at
		FROM messages m
		WHERE m.mailbox = ?
		  AND (
		      m.last_full_body_fetch_at IS NULL
		      OR (m.has_attachments = 1
		          AND NOT EXISTS (
		              SELECT 1 FROM attachments a
		              WHERE a.mailbox = m.mailbox AND a.message_id = m.id AND a.blob_sha != ''
		          )
		      )
		  )
		ORDER BY m.received_at DESC
		LIMIT ?
	`, mailbox, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CachedMessage
	for rows.Next() {
		var (
			m         CachedMessage
			catsJSON  string
			hasAtt    int
		)
		if err := rows.Scan(&m.Mailbox, &m.ID, &m.ConversationID, &m.InternetMessageID,
			&m.Subject, &m.SenderEmail, &m.SenderName, &m.ReceivedAt, &m.BodyPreview,
			&catsJSON, &m.WebLink, &hasAtt, &m.LastSyncedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(catsJSON), &m.Categories)
		m.HasAttachments = hasAtt != 0
		out = append(out, m)
	}
	return out, rows.Err()
}

// ClearDeltaLink wipes the saved delta link (e.g. when Graph rejects it
// with 410 Gone). Records the reset time so we can alert on frequent
// resyncs. Next sync will do a full fetch and save a fresh link.
func (c *Cache) ClearDeltaLink(ctx context.Context, mailbox string) error {
	_, err := c.db.ExecContext(ctx, `
		UPDATE sync_status SET delta_link = '', delta_reset_at = ?
		WHERE mailbox = ?
	`, time.Now().UTC(), mailbox)
	return err
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// HasInvoiceExtraction reports whether we already have a cached row (any outcome).
func (c *Cache) HasInvoiceExtraction(ctx context.Context, mailbox, messageID string) (bool, error) {
	var one int
	err := c.db.QueryRowContext(ctx,
		`SELECT 1 FROM invoice_extractions WHERE mailbox = ? AND message_id = ? LIMIT 1`,
		mailbox, messageID).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}
