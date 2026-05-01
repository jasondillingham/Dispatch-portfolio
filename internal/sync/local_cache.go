// local_cache.go — Phase 1 local mirror: pulls full message bodies and
// attachment bytes into the SQLite cache + filesystem blobstore.
//
// Runs after UpsertMessage during SyncInbox. A message with a stale or
// empty body gets GetMessageDetail'd from Graph and its body columns
// filled. A message with attachments gets each one downloaded via
// FetchAttachmentContent, deduplicated via sha256 in the blobstore,
// and symlinked into the by-message + by-vendor trees.
//
// Both body and attachment persistence are best-effort: failures log
// and move on. The local cache is a speed/resilience win, not a
// correctness dependency — Dispatch still works if the blobstore is
// offline, just with the old Graph-every-time behavior.

package sync

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"dispatch/internal/blobstore"
	"dispatch/internal/cache"
	"dispatch/internal/graph"
)

// LocalCache bundles the filesystem blobstore with the SQLite cache so
// a Syncer can mirror messages without two extra constructor args per
// call. Nil-safe: a nil LocalCache disables all local mirroring.
type LocalCache struct {
	Blob  *blobstore.Store
	Cache *cache.Cache
}

// bodyRefreshInterval is how often we re-pull the full body. Bodies
// rarely change post-delivery (it's already-received mail), so a
// long interval is fine; we mostly just need the first fetch.
const bodyRefreshInterval = 30 * 24 * time.Hour // 30 days

// perMessageBudget caps how long we'll spend on a single message's mirror
// work before moving on. Keeps one pathologically slow message (e.g. a
// 10 MB attachment on a bad link) from blowing the whole sync's budget.
const perMessageBudget = 60 * time.Second

// MirrorMessage pulls the full body + attachments for one message into
// local storage. Safe to call every sync — inner operations short-
// circuit when cached content is fresh. vendorSlug drives the by-vendor
// symlink tree; empty string routes to "_unknown".
//
// Uses a detached per-message context with perMessageBudget so the
// parent sync's (possibly tight) deadline doesn't make every mirror
// call race the same clock — each message gets a fair shot, and one
// message's timeout doesn't cancel the next.
func (lc *LocalCache) MirrorMessage(parent context.Context, gc *graph.Client, m cache.CachedMessage, vendorSlug string) error {
	if lc == nil || lc.Blob == nil || lc.Cache == nil {
		return nil
	}
	// Detach from parent so sync-level timeouts don't immediately propagate,
	// but still honor an explicit parent cancel (e.g. process shutdown).
	ctx, cancel := context.WithTimeout(context.Background(), perMessageBudget)
	defer cancel()
	go func() {
		select {
		case <-parent.Done():
			if parent.Err() == context.Canceled {
				cancel()
			}
		case <-ctx.Done():
		}
	}()

	// Body: skip if recently fetched.
	_, _, fetchedAt, err := lc.Cache.GetMessageBody(ctx, m.Mailbox, m.ID)
	if err == nil && !fetchedAt.IsZero() && time.Since(fetchedAt) < bodyRefreshInterval {
		// already fresh — skip body fetch
	} else {
		if err := lc.fetchAndStoreBody(ctx, gc, m); err != nil {
			// log-and-continue; attachments can still try
			fmt.Printf("local-cache body %s: %v\n", truncate(m.ID, 30), err)
		}
	}
	// Attachments: only attempt if Graph says the message has any.
	if m.HasAttachments {
		if err := lc.fetchAndStoreAttachments(ctx, gc, m, vendorSlug); err != nil {
			fmt.Printf("local-cache atts %s: %v\n", truncate(m.ID, 30), err)
		}
	}
	return nil
}

func (lc *LocalCache) fetchAndStoreBody(ctx context.Context, gc *graph.Client, m cache.CachedMessage) error {
	detail, err := gc.GetMessageDetail(m.Mailbox, m.ID)
	if err != nil {
		// 404 means the message has been moved/archived/deleted in Graph
		// but is still in our local cache (never-prune policy). Stamp the
		// fetch timestamp with empty bodies so backfill doesn't keep
		// retrying forever — the message is Graph-gone.
		if strings.Contains(err.Error(), "404") || strings.Contains(err.Error(), "ErrorItemNotFound") {
			_ = lc.Cache.UpdateMessageBody(ctx, m.Mailbox, m.ID, "", "")
			return fmt.Errorf("graph detail: message no longer in mailbox (404) — marked to skip backfill")
		}
		return fmt.Errorf("graph detail: %w", err)
	}
	var bodyHTML, bodyText string
	if detail.Body != nil {
		switch detail.Body.ContentType {
		case "html", "HTML", "Html":
			bodyHTML = detail.Body.Content
		case "text", "Text":
			bodyText = detail.Body.Content
		default:
			// Unknown content type — stash as text so we at least have something.
			bodyText = detail.Body.Content
		}
	}
	if err := lc.Cache.UpdateMessageBody(ctx, m.Mailbox, m.ID, bodyHTML, bodyText); err != nil {
		return fmt.Errorf("cache update body: %w", err)
	}
	// Also write to filesystem for human browsing + offline access.
	metadataJSON := fmt.Sprintf(`{"id":%q,"mailbox":%q,"subject":%q,"sender":%q,"received":%q}`,
		m.ID, m.Mailbox, escapeJSON(m.Subject), escapeJSON(m.SenderEmail), m.ReceivedAt.Format(time.RFC3339))
	return lc.Blob.WriteMessageBody(m.ReceivedAt, m.ID, bodyHTML, bodyText, metadataJSON)
}

func (lc *LocalCache) fetchAndStoreAttachments(ctx context.Context, gc *graph.Client, m cache.CachedMessage, vendorSlug string) error {
	atts, err := gc.ListAttachments(m.Mailbox, m.ID)
	if err != nil {
		return fmt.Errorf("list attachments: %w", err)
	}
	// Check what's already stored so we don't re-fetch unchanged attachments.
	existing, _ := lc.Cache.ListMessageAttachments(ctx, m.Mailbox, m.ID)
	haveSHA := map[string]string{} // attachment-id → blob_sha
	for _, a := range existing {
		if a.BlobSHA != "" {
			haveSHA[a.AttachmentID] = a.BlobSHA
		}
	}
	for _, a := range atts {
		if a.IsInline {
			continue
		}
		// Metadata upsert first (in case of download failure, we keep the
		// record so ListMessageAttachments returns it).
		meta := cache.CachedAttachment{
			Mailbox:      m.Mailbox,
			MessageID:    m.ID,
			AttachmentID: a.ID,
			Filename:     a.Name,
			ContentType:  a.ContentType,
			SizeBytes:    int64(a.Size),
		}
		if existingSHA, ok := haveSHA[a.ID]; ok && lc.Blob.Has(existingSHA) {
			// Already downloaded + still on disk — refresh symlinks (cheap)
			// and move on.
			meta.BlobSHA = existingSHA
			meta.LocalPath = lc.Blob.BlobPath(existingSHA)
			_ = lc.Cache.UpsertAttachment(ctx, meta)
			_ = lc.Blob.LinkByMessage(m.ReceivedAt, m.ID, a.Name, existingSHA)
			_ = lc.Blob.LinkByVendor(m.ReceivedAt, vendorSlug, a.Name, existingSHA)
			continue
		}
		// Fresh download.
		rdr, _, _, err := gc.FetchAttachmentContent(m.Mailbox, m.ID, a.ID)
		if err != nil {
			meta.LastError = fmt.Sprintf("fetch: %v", err)
			_ = lc.Cache.UpsertAttachment(ctx, meta)
			continue
		}
		data, err := io.ReadAll(rdr)
		rdr.Close()
		if err != nil {
			meta.LastError = fmt.Sprintf("read: %v", err)
			_ = lc.Cache.UpsertAttachment(ctx, meta)
			continue
		}
		sha, path, err := lc.Blob.Put(data)
		if err != nil {
			meta.LastError = fmt.Sprintf("blob put: %v", err)
			_ = lc.Cache.UpsertAttachment(ctx, meta)
			continue
		}
		meta.BlobSHA = sha
		meta.LocalPath = path
		meta.StoredAt = time.Now().UTC()
		meta.LastError = ""
		if err := lc.Cache.UpsertAttachment(ctx, meta); err != nil {
			return fmt.Errorf("upsert attachment: %w", err)
		}
		_ = lc.Blob.LinkByMessage(m.ReceivedAt, m.ID, a.Name, sha)
		_ = lc.Blob.LinkByVendor(m.ReceivedAt, vendorSlug, a.Name, sha)
	}
	return nil
}

// escapeJSON does minimal escaping for a string embedded in a JSON
// literal. Used for the metadata.json alongside bodies; we don't need
// a full JSON marshaler for four string fields.
func escapeJSON(s string) string {
	// Replace backslash first, then quote and control chars inline.
	var out []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\\', '"':
			out = append(out, '\\', c)
		case '\n':
			out = append(out, '\\', 'n')
		case '\r':
			out = append(out, '\\', 'r')
		case '\t':
			out = append(out, '\\', 't')
		default:
			if c < 0x20 {
				continue
			}
			out = append(out, c)
		}
	}
	return string(out)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
