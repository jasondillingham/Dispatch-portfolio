// Package cache — operational telemetry: worker heartbeats and Ollama endpoint
// activity. Both feed the admin dashboard; neither is on the message-processing
// hot path.
package cache

import (
	"context"
	"database/sql"
	"time"
)


// WorkerState is the heartbeat snapshot for one slot used by the web UI.
// Current* fields are empty when the slot is idle. Slot is the goroutine
// index 0..N-1 within its Pool ("sort", "extract", "fallback").
type WorkerState struct {
	Pool                    string
	Slot                    int
	Mailbox                 string
	CurrentMessageID        string
	CurrentStep             string
	CurrentSubject          string
	CurrentVendor           string
	CurrentStartedAt        time.Time
	HeartbeatAt             time.Time
	RunStartedAt            time.Time
	ProcessedThisRun        int
	LastCompletedMessageID  string
	LastCompletedAt         time.Time
}

// IsIdle reports whether the slot has nothing in flight (cleared by MarkSlotIdle).
func (w *WorkerState) IsIdle() bool { return w.CurrentMessageID == "" }

// PoolSpec describes one pool's seed: a pool name and how many slots it owns.
type PoolSpec struct {
	Pool string
	Size int
}

// StartRun wipes worker_state and seeds one row per slot per pool.
// run-level metadata (run_started_at, processed_this_run) lives on
// ("sort", 0). Call once at worker startup before spawning goroutines.
func (c *Cache) StartRun(ctx context.Context, mailbox string, pools []PoolSpec) error {
	now := time.Now().UTC()
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM worker_state`); err != nil {
		return err
	}
	for _, p := range pools {
		size := p.Size
		if size < 1 {
			continue
		}
		for slot := 0; slot < size; slot++ {
			// Only (sort, 0) owns the run-level fields; others get NULL there.
			var runStart any = nil
			if p.Pool == "sort" && slot == 0 {
				runStart = now
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO worker_state (pool, slot, mailbox, heartbeat_at, run_started_at, processed_this_run)
				VALUES (?, ?, ?, ?, ?, 0)
			`, p.Pool, slot, mailbox, now, runStart); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// SetSlotCurrent updates one slot's heartbeat with what it's actively working on.
// Call at the top of each message iteration and at step boundaries inside it.
func (c *Cache) SetSlotCurrent(ctx context.Context, pool string, slot int, mailbox, messageID, step, subject, vendor string) error {
	now := time.Now().UTC()
	_, err := c.db.ExecContext(ctx, `
		INSERT INTO worker_state
			(pool, slot, mailbox, current_message_id, current_step, current_subject, current_vendor, current_started_at, heartbeat_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(pool, slot) DO UPDATE SET
			mailbox=excluded.mailbox,
			current_message_id=excluded.current_message_id,
			current_step=excluded.current_step,
			current_subject=COALESCE(excluded.current_subject, worker_state.current_subject),
			current_vendor=COALESCE(excluded.current_vendor, worker_state.current_vendor),
			current_started_at=CASE
				WHEN worker_state.current_message_id = excluded.current_message_id THEN worker_state.current_started_at
				ELSE excluded.current_started_at
			END,
			heartbeat_at=excluded.heartbeat_at
	`, pool, slot, mailbox, messageID, step, nullStr(subject), nullStr(vendor), now, now)
	return err
}

// MarkSlotCompleted clears current_* on the given slot and bumps the shared
// processed_this_run counter (held on sort slot 0). Safe with concurrent
// writers — SQLite serializes WAL commits.
func (c *Cache) MarkSlotCompleted(ctx context.Context, pool string, slot int, messageID string) error {
	now := time.Now().UTC()
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		UPDATE worker_state
		SET current_message_id=NULL,
		    current_step=NULL,
		    current_subject=NULL,
		    current_vendor=NULL,
		    current_started_at=NULL,
		    last_completed_message_id=?,
		    last_completed_at=?,
		    heartbeat_at=?
		WHERE pool=? AND slot=?
	`, messageID, now, now, pool, slot); err != nil {
		return err
	}
	// Bump the run-level processed counter on (sort, 0).
	if _, err := tx.ExecContext(ctx, `
		UPDATE worker_state SET processed_this_run=COALESCE(processed_this_run,0)+1
		WHERE pool='sort' AND slot=0
	`); err != nil {
		return err
	}
	return tx.Commit()
}

// MarkSlotIdle clears current_* on the given slot without touching counters.
// Call from the goroutine's deferred cleanup at batch end.
func (c *Cache) MarkSlotIdle(ctx context.Context, pool string, slot int) error {
	now := time.Now().UTC()
	_, err := c.db.ExecContext(ctx, `
		UPDATE worker_state
		SET current_message_id=NULL,
		    current_step=NULL,
		    current_subject=NULL,
		    current_vendor=NULL,
		    current_started_at=NULL,
		    heartbeat_at=?
		WHERE pool=? AND slot=?
	`, now, pool, slot)
	return err
}

// GetAllWorkerStates returns every slot row, grouped by pool (sort, extract,
// fallback) then by slot index.
func (c *Cache) GetAllWorkerStates(ctx context.Context) ([]WorkerState, error) {
	rows, err := c.db.QueryContext(ctx, `
		SELECT pool, slot, mailbox, current_message_id, current_step, current_subject, current_vendor,
		       current_started_at, heartbeat_at, run_started_at,
		       COALESCE(processed_this_run,0), last_completed_message_id, last_completed_at
		FROM worker_state
		ORDER BY
			CASE pool WHEN 'sort' THEN 1 WHEN 'extract' THEN 2 WHEN 'fallback' THEN 3 ELSE 4 END,
			slot
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WorkerState
	for rows.Next() {
		var (
			w          WorkerState
			mailbox    sql.NullString
			curID      sql.NullString
			curStep    sql.NullString
			curSubject sql.NullString
			curVendor  sql.NullString
			curStart   sql.NullTime
			runStart   sql.NullTime
			lastID     sql.NullString
			lastAt     sql.NullTime
		)
		if err := rows.Scan(&w.Pool, &w.Slot, &mailbox, &curID, &curStep, &curSubject, &curVendor,
			&curStart, &w.HeartbeatAt, &runStart,
			&w.ProcessedThisRun, &lastID, &lastAt); err != nil {
			return nil, err
		}
		if mailbox.Valid {
			w.Mailbox = mailbox.String
		}
		if curID.Valid {
			w.CurrentMessageID = curID.String
		}
		if curStep.Valid {
			w.CurrentStep = curStep.String
		}
		if curSubject.Valid {
			w.CurrentSubject = curSubject.String
		}
		if curVendor.Valid {
			w.CurrentVendor = curVendor.String
		}
		if curStart.Valid {
			w.CurrentStartedAt = curStart.Time
		}
		if runStart.Valid {
			w.RunStartedAt = runStart.Time
		}
		if lastID.Valid {
			w.LastCompletedMessageID = lastID.String
		}
		if lastAt.Valid {
			w.LastCompletedAt = lastAt.Time
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// Completion is a compact per-message record for the recent-activity list.

// EndpointActivity is the live + historical snapshot for one Ollama URL.
type EndpointActivity struct {
	URL               string
	CurrentMessageID  string    // empty when idle
	CurrentStartedAt  time.Time // zero when idle
	LastCompletedAt   time.Time
	LastDurationMs    int
	LastError         string
	TotalRequests     int
	TotalErrors       int
	TotalDurationMs   int
}

// EndpointRequestStart records that a request to `url` began. Called by
// aiclass.Client hooks. messageID is optional context.
func (c *Cache) EndpointRequestStart(ctx context.Context, url, messageID string) error {
	now := time.Now().UTC()
	_, err := c.db.ExecContext(ctx, `
		INSERT INTO endpoint_activity (url, current_message_id, current_started_at)
		VALUES (?, ?, ?)
		ON CONFLICT(url) DO UPDATE SET
			current_message_id=excluded.current_message_id,
			current_started_at=excluded.current_started_at
	`, url, nullStr(messageID), now)
	return err
}

// EndpointRequestEnd records completion. Clears current_*, updates last_*
// and bumps totals.
func (c *Cache) EndpointRequestEnd(ctx context.Context, url string, dur time.Duration, reqErr error) error {
	now := time.Now().UTC()
	errStr := ""
	errInc := 0
	if reqErr != nil {
		errStr = reqErr.Error()
		if len(errStr) > 200 {
			errStr = errStr[:200]
		}
		errInc = 1
	}
	ms := int(dur.Milliseconds())
	_, err := c.db.ExecContext(ctx, `
		INSERT INTO endpoint_activity
			(url, current_message_id, current_started_at,
			 last_completed_at, last_duration_ms, last_error,
			 total_requests, total_errors, total_duration_ms)
		VALUES (?, NULL, NULL, ?, ?, ?, 1, ?, ?)
		ON CONFLICT(url) DO UPDATE SET
			current_message_id=NULL,
			current_started_at=NULL,
			last_completed_at=excluded.last_completed_at,
			last_duration_ms=excluded.last_duration_ms,
			last_error=excluded.last_error,
			total_requests=endpoint_activity.total_requests+1,
			total_errors=endpoint_activity.total_errors+?,
			total_duration_ms=endpoint_activity.total_duration_ms+?
	`, url, now, ms, nullStr(errStr), errInc, ms, errInc, ms)
	return err
}

// GetEndpointActivity returns the live activity row for every known endpoint,
// keyed by URL. Callers typically merge this with their configured URL list
// so unseen endpoints still render as "never used."
func (c *Cache) GetEndpointActivity(ctx context.Context) (map[string]EndpointActivity, error) {
	rows, err := c.db.QueryContext(ctx, `
		SELECT url,
		       COALESCE(current_message_id,''),
		       current_started_at,
		       last_completed_at, COALESCE(last_duration_ms,0), COALESCE(last_error,''),
		       COALESCE(total_requests,0), COALESCE(total_errors,0), COALESCE(total_duration_ms,0)
		FROM endpoint_activity
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]EndpointActivity{}
	for rows.Next() {
		var (
			a          EndpointActivity
			curStart   sql.NullTime
			lastDone   sql.NullTime
		)
		if err := rows.Scan(&a.URL, &a.CurrentMessageID, &curStart,
			&lastDone, &a.LastDurationMs, &a.LastError,
			&a.TotalRequests, &a.TotalErrors, &a.TotalDurationMs); err != nil {
			return nil, err
		}
		if curStart.Valid {
			a.CurrentStartedAt = curStart.Time
		}
		if lastDone.Valid {
			a.LastCompletedAt = lastDone.Time
		}
		out[a.URL] = a
	}
	return out, rows.Err()
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
