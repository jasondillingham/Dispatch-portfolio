// Package sync mirrors Graph inbox contents into the local SQLite cache so
// the list view renders without hitting Graph on every page load.
//
// Deliberately naive strategy for the prototype: fetch the newest N messages
// via Graph and upsert them all. On a mailbox with 500-800 daily invoices,
// N=500 covers about a day's worth and a single sync takes 1-2 seconds.
// If we start feeling Graph throttling, swap in a delta query.
package sync

import (
	"context"
	"fmt"
	"time"

	"dispatch/internal/blobstore"
	"dispatch/internal/cache"
	"dispatch/internal/graph"
	"dispatch/internal/vendors"
)

type Syncer struct {
	gc       *graph.Client
	cache    *cache.Cache
	local    *LocalCache     // nil = local mirror disabled
	resolver *vendors.Resolver // used for vendor slugs on by-vendor symlinks
	mirrorSem chan struct{} // cap concurrent mirror goroutines
}

// maxConcurrentMirrors caps how many per-message MirrorMessage calls can
// run simultaneously. Higher = more parallel Graph fetches (faster cold
// sync) but also more Graph rate-limit pressure. 8 matches the sort-pool
// sizing and hasn't tripped 429s in practice.
const maxConcurrentMirrors = 8

func New(gc *graph.Client, c *cache.Cache) *Syncer {
	return &Syncer{
		gc:        gc,
		cache:     c,
		mirrorSem: make(chan struct{}, maxConcurrentMirrors),
	}
}

// WithLocalCache enables Phase 1 local mirror (bodies + attachments on disk).
// Pass a nil blobstore to disable — the Syncer then falls back to metadata-only
// sync (the pre-Phase-1 behavior). Resolver is used to compute vendor slugs for
// the by-vendor symlink tree; if nil, vendor routing goes to "_unknown".
func (s *Syncer) WithLocalCache(blob *blobstore.Store, resolver *vendors.Resolver) *Syncer {
	if blob != nil {
		s.local = &LocalCache{Blob: blob, Cache: s.cache}
	}
	s.resolver = resolver
	return s
}

// Stats is what a single sync pass produced.
type Stats struct {
	Fetched    int
	Upserted   int
	Elapsed    time.Duration
	Err        error
}

// SyncInbox pulls the newest `limit` messages from the mailbox's Inbox and
// upserts them. Prefers Graph /delta when a saved deltaLink is available —
// only fetches what's changed since last call, with a clean "all at once"
// fallback when the delta token has expired. Passing limit=0 with an active
// delta link means "give me everything that's changed, no cap."
// Idempotent: running twice with no new mail is a ~no-op (the delta call
// returns an empty Changed list + a fresh link).
func (s *Syncer) SyncInbox(ctx context.Context, mailbox string, limit int) Stats {
	start := time.Now()
	stats := Stats{}

	// Try delta first. Empty deltaLink → Graph returns all messages AND a
	// fresh link (the normal cold-start path), so the caller always gets
	// full coverage on first run and incremental on subsequent runs.
	deltaLink, _ := s.cache.GetDeltaLink(ctx, mailbox)
	delta, derr := s.gc.ListInboxDelta(mailbox, deltaLink)
	if derr == nil && delta != nil && !delta.Expired {
		stats = s.processDeltaResult(ctx, mailbox, delta)
		stats.Elapsed = time.Since(start)
		return stats
	}
	if delta != nil && delta.Expired {
		fmt.Printf("sync: delta link expired for %s, doing full resync\n", mailbox)
		_ = s.cache.ClearDeltaLink(ctx, mailbox)
	}
	// Fall through: full list (legacy behavior) — also seeds the first
	// deltaLink on success by calling /delta ourselves at the end.

	msgs, err := s.gc.ListInboxMessages(mailbox, limit)
	if err != nil {
		stats.Err = fmt.Errorf("list graph: %w", err)
		_ = s.cache.RecordSync(ctx, mailbox, 0, stats.Err.Error())
		stats.Elapsed = time.Since(start)
		return stats
	}
	stats.Fetched = len(msgs)

	for _, m := range msgs {
		if err := s.upsertAndMirror(ctx, mailbox, m); err != nil {
			stats.Err = err
			break
		}
		stats.Upserted++
	}

	// Seed a fresh delta link so subsequent syncs go incremental. Best-effort.
	if stats.Err == nil {
		if seed, err := s.gc.ListInboxDelta(mailbox, ""); err == nil && seed != nil && seed.DeltaLink != "" {
			_ = s.cache.SetDeltaLink(ctx, mailbox, seed.DeltaLink)
		}
	}

	stats.Elapsed = time.Since(start)
	errMsg := ""
	if stats.Err != nil {
		errMsg = stats.Err.Error()
	}
	_ = s.cache.RecordSync(ctx, mailbox, stats.Upserted, errMsg)
	return stats
}

// processDeltaResult upserts the Changed slice (like the legacy path) and
// saves the new deltaLink for next time. RemovedIDs are logged — Phase 2
// keeps moved/archived messages in the local cache (never-prune per design),
// so removals from Graph's perspective are informational only for now.
func (s *Syncer) processDeltaResult(ctx context.Context, mailbox string, d *graph.DeltaResult) Stats {
	stats := Stats{Fetched: len(d.Changed)}
	for _, m := range d.Changed {
		if err := s.upsertAndMirror(ctx, mailbox, m); err != nil {
			stats.Err = err
			break
		}
		stats.Upserted++
	}
	if len(d.RemovedIDs) > 0 {
		fmt.Printf("sync: delta removed %d message(s) from %s inbox (kept in local cache per never-prune policy)\n",
			len(d.RemovedIDs), mailbox)
	}
	if d.DeltaLink != "" {
		_ = s.cache.SetDeltaLink(ctx, mailbox, d.DeltaLink)
	}
	errMsg := ""
	if stats.Err != nil {
		errMsg = stats.Err.Error()
	}
	_ = s.cache.RecordSync(ctx, mailbox, stats.Upserted, errMsg)
	return stats
}

// BackfillOnce queries for messages that need mirror catch-up (no full
// body fetched yet, or has_attachments=1 with no attachment rows) and
// fires MirrorMessage on each through the same bounded semaphore the
// sync loop uses. Returns how many messages it enqueued.
//
// Intended to run on a ticker. Fills the design gap where delta sync
// only upserts *changed* messages — stragglers from earlier failed syncs
// would otherwise never get re-mirrored because nothing is re-upserting
// them. Rate-limited via the semaphore so it can't starve fresh sync work.
func (s *Syncer) BackfillOnce(ctx context.Context, mailbox string, batchSize int) (int, error) {
	if s.local == nil {
		return 0, nil
	}
	stragglers, err := s.cache.ListUnmirroredMessages(ctx, mailbox, batchSize)
	if err != nil {
		return 0, fmt.Errorf("list stragglers: %w", err)
	}
	for _, cm := range stragglers {
		slug := ""
		if s.resolver != nil {
			if match := s.resolver.Resolve(cm.SenderEmail); match.Type != vendors.MatchUnknown {
				slug = blobstore.VendorSlug(match.Vendor.VendorName)
			}
		}
		select {
		case s.mirrorSem <- struct{}{}:
		case <-ctx.Done():
			return len(stragglers), ctx.Err()
		}
		go func(cm cache.CachedMessage, slug string) {
			defer func() { <-s.mirrorSem }()
			_ = s.local.MirrorMessage(context.Background(), s.gc, cm, slug)
		}(cm, slug)
	}
	return len(stragglers), nil
}

// upsertAndMirror is the shared per-message pipeline used by both the
// full-sync path and the delta path. Upserts the metadata row to the
// cache and fires a background mirror goroutine (body + attachments).
func (s *Syncer) upsertAndMirror(ctx context.Context, mailbox string, m graph.Message) error {
	cm := cache.CachedMessage{
		Mailbox:        mailbox,
		ID:             m.ID,
		ConversationID: m.ConversationID,
		Subject:        m.Subject,
		SenderEmail:    m.SenderAddress(),
		SenderName:     m.SenderName(),
		BodyPreview:    m.BodyPreview,
		Categories:     m.Categories,
		WebLink:        m.WebLink,
		HasAttachments: m.HasAttachments,
		LastSyncedAt:   time.Now().UTC(),
	}
	if t, err := time.Parse(time.RFC3339, m.ReceivedDateTime); err == nil {
		cm.ReceivedAt = t
	}
	if err := s.cache.UpsertMessage(ctx, cm); err != nil {
		return fmt.Errorf("upsert %s: %w", m.ID, err)
	}
	if s.local != nil {
		slug := ""
		if s.resolver != nil {
			if match := s.resolver.Resolve(cm.SenderEmail); match.Type != vendors.MatchUnknown {
				slug = blobstore.VendorSlug(match.Vendor.VendorName)
			}
		}
		s.mirrorSem <- struct{}{}
		go func(cm cache.CachedMessage, slug string) {
			defer func() { <-s.mirrorSem }()
			_ = s.local.MirrorMessage(context.Background(), s.gc, cm, slug)
		}(cm, slug)
	}
	return nil
}
