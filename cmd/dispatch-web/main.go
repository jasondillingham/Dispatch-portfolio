// dispatch-web is the Dispatch list UI.
//
// First slices:
//  1. read-only list with filter tabs
//  2. inline Claim/Status/Blocker actions that PATCH categories to Graph
//
// No SQLite cache yet — messages fetched fresh from Graph per request.
// Port: 8085.
package main

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log"
	"os"
	"net/http"
	"net/url"
	"os/exec"
	"sort"
	"strconv"
	"sync"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"dispatch/internal/blobstore"
	"dispatch/internal/cache"
	"dispatch/internal/graph"
	"dispatch/internal/erp"
	"dispatch/internal/recon"
	dispatchsync "dispatch/internal/sync"
	"dispatch/internal/ui"
	"dispatch/internal/vendors"
)

// Mailboxes where attachment-content streaming is blocked. catchall
// is phishing-adjacent and could contain malware payloads; even inline-rendered
// attachments (PDFs, images) can embed tracker pixels or exploit the browser's
// native viewer on the rare day there's an unpatched CVE. Keep the streaming
// path closed for that mailbox.
var attachmentBlockedMailboxes = map[string]bool{
	"catchall@example.com": true,
}

// Content-Types safe to render inline. Others force download (Content-Disposition: attachment)
// so the browser doesn't try to render them as HTML.
var inlineContentTypes = map[string]bool{
	"application/pdf": true,
	"image/png":       true,
	"image/jpeg":      true,
	"image/jpg":       true,
	"image/gif":       true,
	"image/webp":      true,
	"image/svg+xml":   false, // SVG can contain scripts — force download
	"text/plain":      true,
}

type detailData struct {
	M             ui.ViewMessage
	User          string
	BodyHTML      template.HTML // sandboxed-iframe-bound body when contentType=html (legacy single-message view)
	BodyText      string        // plain text fallback
	To            string
	Cc            string
	Attachments   []graph.Attachment
	FirstPDF      *graph.Attachment // if present, show inline preview
	FirstPDFAngle int               // saved rotation (0/90/180/270) for the FirstPDF; used by detail.html to apply CSS transform on first paint
	Thread        []ThreadCard      // full conversation for card-style display
	Extraction    *cache.InvoiceExtraction
	Recon         *recon.Reconciliation
	Related       []cache.RelatedMessage // other messages sharing this PO
	RelatedPO     int64                  // the PO number used for the lookup
	IsReview      bool                   // true when rendered inside Review Mode — swaps the close button for an Esc hint
	Notes         []cache.InvoiceNote    // append-only clerk annotations for this message
	VendorHistory []cache.VendorHistoryRow      // last N invoices from the same vendor
	VendorStats   cache.VendorHistorySummary    // aggregate of clean/issue/posted across full vendor history
	RecentVerdict *cache.Verdict                // most recent verdict by current user, nil if none — drives the "you marked this X" hint
}

// ThreadCard is one message rendered as a card in the Gmail-style thread view.
// Body is already quote-collapsed and wrapped for iframe display.
type ThreadCard struct {
	ID         string
	FromName   string
	FromEmail  string
	ReceivedAt time.Time
	Subject    string
	Preview    string
	BodyHTML   template.HTML
	BodyText   string
	IsCurrent  bool // the card the user clicked to open this detail
	IsInternal bool // @example.com sender — subtle styling
}

//go:embed templates/*.html
var tmplFS embed.FS

type server struct {
	gc                *graph.Client
	cache             *cache.Cache
	syncer            *dispatchsync.Syncer
	erp               *erp.Client // nil when voucher sync is disabled
	tmpl              *template.Template
	mailbox           string
	user              string
	limit             int
	primaryURLs       []string // for /queue endpoint panel
	fallbackURLs      []string // for /queue endpoint panel
	paddleURLs        []string // for /queue endpoint panel
	primaryModel      string
	fallbackModel     string
	paddleModel       string
}

type filterOpt struct {
	Key   ui.Filter
	Label string
	Count int
}

type pageData struct {
	Mailbox          string
	User             string // effective user — equals impersonated ID when active, else auth user
	AuthUser         string // always the authenticated admin (for the "viewing as ___" banner)
	APUsers          []erp.APUser // populates the impersonate dropdown
	Impersonating    bool
	ImpersonatedName string // display name of impersonated user (e.g., "the AP pilot user")
	Active           ui.Filter
	Filters          []filterOpt
	Messages         []ui.ViewMessage
	Query            string // populated for /search renders
	PONo             int64  // when set, list is filtered to messages on this PO; banner above the list
	QueueDrill       string // when set, list is filtered by an admin-page drill-down (?queue=needs-first-pass|rescan|errored)
	QueueDrillLabel  string // human label for the drill-down banner ("needs first pass", "errored", etc)
	QueueTotal       float64 // sum of InvoiceAmount across .Messages — drives the "$X to process" chip
	QueueTotalRows   int     // count of rows that contributed to QueueTotal (some have no extraction)
}

// queueTotalFor sums extracted invoice amounts across a filtered list.
// Returns (sum, rowsCounted). Rows without an extraction contribute 0.
func queueTotalFor(msgs []ui.ViewMessage) (float64, int) {
	var sum float64
	var n int
	for _, m := range msgs {
		if m.InvoiceAmount > 0 {
			sum += m.InvoiceAmount
			n++
		}
	}
	return sum, n
}

// humanMoney formats a float as comma-separated dollars without the $ sign.
// Used by the queueTotal chip; keeps the template clean.
func humanMoney(v float64) string {
	whole := int64(v)
	cents := int64((v - float64(whole)) * 100)
	if cents < 0 {
		cents = -cents
	}
	// Insert thousands separators.
	wholeStr := fmt.Sprintf("%d", whole)
	if whole < 0 {
		// keep the minus on the front
		neg := wholeStr[0] == '-'
		if neg {
			wholeStr = wholeStr[1:]
		}
	}
	var b strings.Builder
	for i, c := range wholeStr {
		if i > 0 && (len(wholeStr)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return fmt.Sprintf("%s.%02d", b.String(), cents)
}

// filterByIDSet narrows a list to messages whose ID is in the given set.
// Used by the admin queue drill-downs (?queue=needs-first-pass etc) where
// the cache returns a tight, time-windowed ID list and we want the existing
// list view to render only those.
func filterByIDSet(msgs []ui.ViewMessage, ids map[string]bool) []ui.ViewMessage {
	if len(ids) == 0 {
		return nil
	}
	out := make([]ui.ViewMessage, 0, len(ids))
	for _, m := range msgs {
		if ids[m.ID] {
			out = append(out, m)
		}
	}
	return out
}

// resolveQueueDrilldown reads ?queue=X and returns the matching ID set +
// a human-readable label for the banner. Empty kind returns nil → no filter.
// Time window is fixed at 7 days for now; old admin requests get cut off so
// the drill-down stays useful instead of dragging the whole history.
func (s *server) resolveQueueDrilldown(ctx context.Context, kind string) (map[string]bool, string, error) {
	const window = 7 * 24 * time.Hour
	const limit = 500
	switch kind {
	case "needs-first-pass":
		ids, err := s.cache.ListPendingFirstPass(ctx, s.mailbox, window, limit)
		return setOf(ids), "needs first pass", err
	case "rescan":
		ids, err := s.cache.ListRescanQueueRecent(ctx, s.mailbox, window, 4, limit)
		return setOf(ids), "awaiting rescan", err
	case "errored":
		ids, err := s.cache.ListErroredExtractions(ctx, s.mailbox, window, limit)
		return setOf(ids), "errored", err
	default:
		return nil, "", nil
	}
}

func setOf(ids []string) map[string]bool {
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out
}

// filterByPO narrows a message list to those matching the given PO. Returns
// the input untouched when poNo == 0.
func filterByPO(msgs []ui.ViewMessage, poNo int64) []ui.ViewMessage {
	if poNo <= 0 {
		return msgs
	}
	out := make([]ui.ViewMessage, 0, 8)
	for _, m := range msgs {
		if m.PONo == poNo {
			out = append(out, m)
		}
	}
	return out
}

// parsePO reads the ?po= query param. Returns 0 when missing or invalid.
func parsePO(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// reviewData drives the focused full-screen review view. Embeds the standard
// detailData (so review.html can reuse detail.html partials) and adds the
// position info + filter context the keyboard nav needs.
type reviewData struct {
	detailData            // current message — body, recon, attachments, etc.
	Filter     ui.Filter  // filter the clerk is reviewing under
	FilterLabel string
	Index      int        // zero-based position
	Total      int        // total messages in filter at render time
	PrevIndex  int        // -1 if at start
	NextIndex  int        // -1 if at end
	StartedAt  time.Time  // when the review session began (for elapsed display)
}

// reviewDoneData is shown when a clerk reviews past the last message in their
// filter. Inbox-zero screen with a small reward — paper has no progress bar.
type reviewDoneData struct {
	Mailbox     string
	User        string
	Filter      ui.Filter
	FilterLabel string
	Total       int
	Elapsed     time.Duration
}

var filterOrder = []ui.Filter{
	ui.FilterOpen, ui.FilterUnclaimed, ui.FilterMine, ui.FilterMyBuyer,
	ui.FilterMatch, ui.FilterDiscrepancy,
	ui.FilterBlocked, ui.FilterUnposted, ui.FilterRescan,
	ui.FilterDone, ui.FilterMarketing, ui.FilterPayments, ui.FilterAll,
}

// filterOrderImpersonating is the narrowed tab set shown when admins
// "view as" an AP user. Mirrors what the AP role actually cares about
// day-to-day; the full admin tab bar would be too noisy.
var filterOrderImpersonating = []ui.Filter{
	ui.FilterUnclaimed, ui.FilterMine, ui.FilterDiscrepancy,
	ui.FilterDone, ui.FilterAll,
}

func main() {
	mailbox := flag.String("mailbox", "ap@example.com", "mailbox to display")
	user := flag.String("user", "dispatchadmin", "current user (for Mine filter + Claim)")
	limit := flag.Int("limit", 2000, "max messages to keep in cache (newest). Bumped from 500 so Graph polls cover everything in recent inbox history instead of dropping older messages.")
	blobDir := flag.String("blob-dir", "/mnt/ap-synology/dispatch", "local mirror root — bodies + attachments stored here as a content-addressed blobstore. Empty disables local mirror (metadata-only sync).")
	emailsPath := flag.String("emails", "data/vendor_emails.json", "vendor emails JSON (for by-vendor symlink routing)")
	domainsPath := flag.String("domains", "data/vendor_domains.json", "vendor domains JSON")
	syncInterval := flag.Duration("sync", 60*time.Second, "background sync interval")
	backfillInterval := flag.Duration("backfill", 30*time.Second, "mirror-backfill interval — catches messages whose bodies/attachments didn't finish mirroring during sync. 0 disables.")
	backfillBatch := flag.Int("backfill-batch", 50, "max messages per backfill pass. Rate-limits the mirror catch-up so it doesn't starve fresh sync work.")
	cachePath := flag.String("cache", "", "SQLite cache path (default: ~/.dispatch/cache.db)")
	addr := flag.String("addr", ":8085", "listen address")
	mssqlConfig := flag.String("mssql", "", "path to mssql_config.json (default: search standard locations); empty disables voucher sync")
	voucherSyncInterval := flag.Duration("voucher-sync", 10*time.Minute, "how often to poll the ERP for voucher status")
	primaryURLs := flag.String("primary-urls",
		"http://<gpu-host-1>:11434",
		"comma-sep primary vision endpoints (for queue page health display). "+
			"ai-02 (<gpu-host-1>) is the live primary; <gpu-host-3> was dropped "+
			"2026-04-27 because it serves olmocr2, not the vision model the worker calls.")
	fallbackURLs := flag.String("fallback-urls",
		"http://<gpu-host-2>:11434",
		"comma-sep fallback vision endpoints. As of 2026-04-27 there's a single "+
			"native-Ollama endpoint on ai-03's Tesla P40 (24GB, gemma4:26b, "+
			"OLLAMA_NUM_PARALLEL=4). Old multi-endpoint CPU pool (mjolnir + ai-04 "+
			"+ ai-03 docker copies) retired with the GPU install.")
	paddleURLs := flag.String("paddle-urls", "",
		"comma-sep Paddle OCR endpoints for the queue page. Empty (default) hides the Paddle row; set when Paddle is back online.")
	primaryModel := flag.String("primary-model", "minicpm-v:latest", "primary vision model name (display only)")
	fallbackModel := flag.String("fallback-model", "gemma4:26b", "fallback vision model name (display only)")
	paddleModel := flag.String("paddle-model", "PaddlePaddle/PaddleOCR-VL", "Paddle model name (display only)")
	flag.Parse()

	gc, err := graph.NewClient()
	if err != nil {
		log.Fatalf("graph client: %v", err)
	}
	c, err := cache.Open(*cachePath)
	if err != nil {
		log.Fatalf("cache: %v", err)
	}
	defer c.Close()

	funcs := template.FuncMap{
		"relTime":        relTime,
		"statusSlug":     statusSlug,
		"rowID":          rowID,
		"hasPrefix":      strings.HasPrefix,
		"hasSuffix":      strings.HasSuffix,
		"divMs":          func(ms int) float64 { return float64(ms) / 1000.0 },
		"minus1":         func(n int) int { return n - 1 },
		"ageDays":        func(t time.Time) int { return int(time.Since(t).Hours() / 24) },
		"thousands":      humanMoney,
		"reconSummary":   summarizeReconForAsk,
		"docTypeLabel": func(kind string) string {
			// Plain-English document-type label for the AP-mode meta row.
			// AI's Kind: tag is the source; we humanize it.
			switch strings.ToLower(strings.TrimSpace(kind)) {
			case "":
				return "Document type unclear"
			case "invoice":
				return "Invoice"
			case "credit", "credit memo", "credit-memo":
				return "Credit memo"
			case "statement":
				return "Statement"
			case "payment":
				return "Payment notice"
			case "dispute":
				return "Dispute"
			case "po":
				return "Order Confirmation" // legacy Kind: PO migrated label
			case "orderconfirmation":
				return "Order Confirmation"
			case "marketing":
				return "Marketing"
			case "newsletter":
				return "Newsletter"
			case "webinar":
				return "Training/Webinar"
			default:
				return kind
			}
		},
		"docTypeSlug": func(kind string) string {
			// CSS-class slug. Lowercased + spaces→dashes. Used as
			// .ap-doc-pill.kind-X to color the pill per document type.
			s := strings.ToLower(strings.TrimSpace(kind))
			if s == "" {
				return "unknown"
			}
			return strings.ReplaceAll(s, " ", "-")
		},
		"apHoldOptions": func() map[string][]string {
			// Plain-English picker shown when AP clerk hits Hold. First
			// element is the button label (what they read); second is a
			// short subhead (what it does behind the scenes). Server maps
			// these reason keys to the existing Blocker enum.
			return map[string][]string{
				"ask-buyer":  {"Ask the buyer", "Sets a Purchasing hold"},
				"ask-vendor": {"Ask the vendor", "Sets a Vendor hold"},
				"pricing":    {"Wrong amount", "Pricing hold — invoice $ doesn't match PO"},
				"po":         {"Wrong PO number", "PO hold — wrong or missing PO"},
				"wont-pay":   {"Won't pay this", "Won't Pay hold — duplicate / cancelled / write-off"},
			}
		},
		"add":            func(a, b int) int { return a + b },
		"progressPct": func(idx, total int) int {
			if total <= 0 {
				return 0
			}
			pct := ((idx + 1) * 100) / total
			if pct > 100 {
				pct = 100
			}
			return pct
		},
		"humanDuration": func(d time.Duration) string {
			if d < time.Minute {
				return fmt.Sprintf("%ds", int(d.Seconds()))
			}
			if d < time.Hour {
				m := int(d.Minutes())
				s := int(d.Seconds()) % 60
				return fmt.Sprintf("%dm %ds", m, s)
			}
			h := int(d.Hours())
			m := int(d.Minutes()) % 60
			return fmt.Sprintf("%dh %dm", h, m)
		},
		"avgPerItem": func(d time.Duration, n int) string {
			if n <= 0 {
				return "—"
			}
			per := d / time.Duration(n)
			if per < time.Minute {
				return fmt.Sprintf("%.1fs", per.Seconds())
			}
			return fmt.Sprintf("%dm %ds", int(per.Minutes()), int(per.Seconds())%60)
		},
		"mineIfOwner":    func(m ui.ViewMessage, u string) bool { return m.MineIfOwner(u) },
		"statusOptions":  func() []string { return ui.StatusOptions },
		"blockerOptions": func() []string { return ui.BlockerOptions },
		"dict":           dict,
		"humanSize":      humanSize,
		"parseAndRel":    parseAndRel,
		"money": func(v float64) string { return fmt.Sprintf("%.2f", v) },
		"pct":   func(v float64) int { return int(v*100 + 0.5) }, // 0.0–1.0 → integer percent, rounded
		"inc1":  func(n int) int { return n + 1 },                // 0-indexed → 1-indexed for "row N of M" labels
		"aiGlyph": func(status string) string {
			switch status {
			case "clean":
				return "✓"
			case "issue":
				return "⚠"
			case "error":
				return "✕"
			case "pending":
				return "○"
			case "classified":
				return "AI"
			case "tagged":
				return "✔"
			default:
				return ""
			}
		},
		"aiTitle": func(status string) string {
			switch status {
			case "clean":
				return "AI reviewed · invoice matches PO"
			case "issue":
				return "AI reviewed · discrepancy vs PO (click to review)"
			case "error":
				return "AI extraction error"
			case "pending":
				return "AI extracted · reconciliation pending"
			case "classified":
				return "AI classified this message"
			case "tagged":
				return "Processed · vendor / buyer resolved deterministically (no AI needed)"
			default:
				return ""
			}
		},
		"verdictIcon": func(v recon.Verdict) string {
			switch v {
			case recon.VerdictMatch:
				return "✓"
			case recon.VerdictExtendedMatch:
				return "≈"
			case recon.VerdictMissingFromInv:
				return "…"
			case recon.VerdictExtraOnInvoice, recon.VerdictBothMismatch:
				return "✕"
			case recon.VerdictShippingFee:
				return "🚚"
			case recon.VerdictTaxFee:
				return "%"
			case recon.VerdictHandlingFee:
				return "⊕"
			default:
				return "⚠"
			}
		},
		"isFeeVerdict": recon.IsFeeVerdict,
		"humanMs": func(ms int) string {
			if ms < 1000 {
				return fmt.Sprintf("%dms", ms)
			}
			return fmt.Sprintf("%.1fs", float64(ms)/1000)
		},
	}
	tmpl, err := template.New("").Funcs(funcs).ParseFS(tmplFS, "templates/*.html")
	if err != nil {
		log.Fatalf("parse templates: %v", err)
	}

	syncer := dispatchsync.New(gc, c)
	// Local mirror (Phase 1): point the syncer at the Synology blobstore
	// so each sync pass also pulls full bodies + attachment bytes down.
	// Failures are best-effort; if the share is unreachable the syncer
	// falls back to metadata-only behavior.
	if *blobDir != "" {
		blobStore, err := blobstore.New(*blobDir)
		if err != nil {
			log.Printf("blobstore disabled: %v", err)
		} else {
			resolver, rerr := vendors.Load(*emailsPath, *domainsPath)
			if rerr != nil {
				log.Printf("blobstore: vendor resolver load failed (by-vendor symlinks will route to _unknown): %v", rerr)
				resolver = nil
			}
			syncer = syncer.WithLocalCache(blobStore, resolver)
			log.Printf("local blobstore enabled at %s", blobStore.Root())
		}
	}
	s := &server{
		gc: gc, cache: c, syncer: syncer, tmpl: tmpl,
		mailbox: *mailbox, user: *user, limit: *limit,
		primaryURLs:   splitCSV(*primaryURLs),
		fallbackURLs:  splitCSV(*fallbackURLs),
		paddleURLs:    splitCSV(*paddleURLs),
		primaryModel:  *primaryModel,
		paddleModel:   *paddleModel,
		fallbackModel: *fallbackModel,
	}

	// Seed cache synchronously on startup so the first page load is fast.
	// Timeout is generous because the local-mirror work fetches full body
	// + attachment bytes for every message — a cold run of 2000 messages
	// takes minutes even on a good network. After first run the short-
	// circuit paths kick in and it's mostly instant.
	initCtx, initCancel := context.WithTimeout(context.Background(), 15*time.Minute)
	initStats := syncer.SyncInbox(initCtx, *mailbox, *limit)
	initCancel()
	log.Printf("initial sync: fetched=%d upserted=%d in %s err=%v",
		initStats.Fetched, initStats.Upserted, initStats.Elapsed.Round(time.Millisecond), initStats.Err)

	// Periodic background sync.
	go func() {
		t := time.NewTicker(*syncInterval)
		defer t.Stop()
		for range t.C {
			// 5 min periodic budget: steady-state is cheap (already-mirrored
			// messages short-circuit), but a burst of new mail with big
			// attachments can easily take >30s.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			st := syncer.SyncInbox(ctx, *mailbox, *limit)
			cancel()
			if st.Err != nil {
				log.Printf("sync err: %v (fetched=%d)", st.Err, st.Fetched)
			} else if st.Fetched > 0 || st.Upserted > 0 {
				// Only log when there's actual work — quiet when nothing changed,
				// visible when new mail arrives.
				log.Printf("sync: fetched=%d upserted=%d in %s (delta)",
					st.Fetched, st.Upserted, st.Elapsed.Round(time.Millisecond))
			}
		}
	}()

	// Mirror backfill ticker: catches messages whose bodies or attachments
	// didn't finish mirroring during an earlier sync (goroutine died, Graph
	// 429'd, process restarted mid-flight, etc). Without this, delta sync's
	// upsert-only behavior leaves those messages stuck forever.
	if *backfillInterval > 0 {
		go func() {
			t := time.NewTicker(*backfillInterval)
			defer t.Stop()
			for range t.C {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
				n, err := syncer.BackfillOnce(ctx, *mailbox, *backfillBatch)
				cancel()
				if err != nil {
					log.Printf("backfill err: %v", err)
				} else if n > 0 {
					log.Printf("backfill: queued %d stragglers for mirror", n)
				}
			}
		}()
	}

	// Optional: the ERP voucher sync. Polls apinv_hdr for extractions with both a
	// PO and an invoice_number. Read-only — never writes to the ERP. Updates cache
	// with voucher_no + pay_status + check info so the UI can surface "still
	// needs vouchering" vs "paid and closed".
	if *mssqlConfig != "" || envHasMSSQL() {
		erpClient, err := erp.New(*mssqlConfig)
		if err != nil {
			log.Printf("voucher sync disabled (erp open failed): %v", err)
		} else {
			log.Printf("voucher sync enabled, interval=%s", *voucherSyncInterval)
			s.erp = erpClient
			go runVoucherSync(erpClient, c, gc, *mailbox, *voucherSyncInterval)
		}
	} else {
		log.Printf("voucher sync disabled (no -mssql config)")
	}

	// Follow-up sweeper: revives messages that have been Hold'd past their
	// per-reason follow-up window (see handleAPHold). Independent of the ERP —
	// always runs as long as we have cache + Graph access.
	log.Printf("followup sweep enabled, interval=%s", followupSweepInterval)
	go runFollowupSweep(c, gc, *mailbox)

	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	// Password gate. Opt-in: set DISPATCH_PASSWORD in the env to enable.
	// Leaving it unset is fine for local dev but logs a warning. /healthz is
	// always open (the middleware short-circuits that path) so load balancers
	// and systemd can still probe liveness.
	// chi requires middleware be registered BEFORE routes — keep this above
	// any r.Get / r.Post calls.
	if pw := os.Getenv("DISPATCH_PASSWORD"); pw != "" {
		log.Printf("password gate: enabled (user=ap)")
		r.Use(requirePassword("ap", pw))
	} else {
		log.Printf("password gate: DISABLED — set DISPATCH_PASSWORD to require auth")
	}
	// CSRF protection: reject state-changing requests (POST/PATCH/PUT/DELETE)
	// from a different origin. Required because we use HTTP Basic auth, which
	// the browser auto-attaches even to cross-origin requests.
	r.Use(requireSameOriginPost)
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	r.Get("/", s.handleIndex)
	r.Get("/list", s.handleList)
	r.Get("/search", s.handleSearch)
	r.Get("/detail-empty", func(w http.ResponseWriter, r *http.Request) {
		_ = s.tmpl.ExecuteTemplate(w, "detail-empty", nil)
	})
	r.Get("/message/{rowID}", s.handleDetail)
	r.Get("/message/{rowID}/body", s.handleMessageBody)
	r.Get("/message/{rowID}/attachment/{attID}", s.handleAttachment)
	r.Post("/message/{rowID}/claim", s.handleClaim)
	r.Post("/message/{rowID}/status", s.handleStatus)
	r.Post("/message/{rowID}/verdict", s.handleVerdict)
	r.Post("/message/{rowID}/blocker/{name}", s.handleBlocker)
	r.Get("/worker/status", s.handleWorkerStatus)
	// /queue → /admin: the page is operational tooling, not for AP clerks.
	// Keep /queue as a 301 so old bookmarks still land somewhere.
	r.Get("/queue", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin", http.StatusMovedPermanently)
	})
	r.Get("/admin", s.handleAdmin)
	r.Get("/admin/overview", s.handleAdminOverview)
	r.Post("/admin/restart-workers", s.handleAdminRestartWorkers)
	r.Get("/review", s.handleReview)
	r.Get("/extract-review", s.handleExtractReview)
	r.Post("/extract-review/{rowID}/verdict", s.handleExtractReviewVerdict)

	// AP-mode surface — clerk-first, three-button decision flow. Lives
	// alongside /list and /review (admin-friendly views) during pilot;
	// once the AP team is on it we'll auto-redirect AP users from /
	// based on the userMode helper. the AP pilot user is the pilot (2026-04-29).
	r.Get("/ap", s.handleAP)
	r.Post("/ap/pickup/{rowID}", s.handleAPPickup)
	r.Post("/ap/assign/{rowID}", s.handleAPAssign)
	r.Post("/ap/hold/{rowID}", s.handleAPHold)
	r.Post("/ap/skip/{rowID}", s.handleAPSkip)
	r.Post("/ap/note/{rowID}", s.handleAPAddNote)
	r.Post("/message/{rowID}/recheck-voucher", s.handleRecheckVoucher)
	r.Post("/message/{rowID}/clear-kind", s.handleClearKind)
	r.Post("/message/{rowID}/manual-entry", s.handleManualEntry)
	r.Post("/message/{rowID}/attachment/{attID}/rotation", s.handleSetRotation)
	r.Post("/impersonate", s.handleImpersonateSet)
	r.Post("/impersonate/exit", s.handleImpersonateExit)
	r.Post("/message/{rowID}/notes", s.handleAddNote)
	r.Get("/message/{rowID}/ask/{audience}", s.handleAskPreview)

	log.Printf("dispatch-web listening on %s (mailbox=%s user=%s)", *addr, *mailbox, *user)
	if err := http.ListenAndServe(*addr, r); err != nil {
		log.Fatal(err)
	}
}

func (s *server) fetchMessages() ([]ui.ViewMessage, error) {
	// Read from local cache — maintained by the background sync goroutine.
	// Never hits Graph on the request path.
	listCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cms, err := s.cache.ListMessages(listCtx, s.mailbox, s.limit)
	if err != nil {
		return nil, err
	}

	summaryCtx, scancel := context.WithTimeout(context.Background(), 2*time.Second)
	summaries, _ := s.cache.ListAISummaryForMailbox(summaryCtx, s.mailbox)
	scancel()

	noteCtx, ncancel := context.WithTimeout(context.Background(), 2*time.Second)
	noteCounts, _ := s.cache.CountInvoiceNotesByMessage(noteCtx, s.mailbox)
	ncancel()

	out := make([]ui.ViewMessage, 0, len(cms))
	for _, cm := range cms {
		vm := cachedToView(cm)
		if sum, ok := summaries[cm.ID]; ok {
			vm.AIProcessed = sum.Processed
			vm.AIHasExtraction = sum.HasExtraction
			vm.AIHasReconcile = sum.HasReconcile
			vm.AITotalMatch = sum.TotalMatch
			vm.AIAnyLineMismatch = sum.AnyLineMismatch
			vm.AIErrorMsg = sum.ErrorMsg
			vm.AINeedsRescan = sum.NeedsRescan
			vm.PayStatus = sum.PayStatus
			vm.VoucherNo = sum.VoucherNo
			vm.PONo = sum.PONo
			vm.InvoiceAmount = sum.InvoiceAmount
		}
		if n, ok := noteCounts[cm.ID]; ok {
			vm.NoteCount = n
		}
		out = append(out, vm)
	}
	return dedupeByConversation(out), nil
}

// dedupeByConversation collapses messages sharing a ConversationID into a
// single representative (the newest). ConversationSize is set on each
// representative so the list template can render a "+N" badge. Messages with
// empty ConversationID are treated as their own group (always rendered).
// Filter evaluation happens downstream — it sees the deduped list, so a
// conversation is only "Open"/"Blocked"/etc based on the newest message's
// state (which matches clerk intuition: they update the latest reply).
func dedupeByConversation(msgs []ui.ViewMessage) []ui.ViewMessage {
	// Map convID → (representative, count). Messages with empty convID get
	// a synthetic unique key so they pass through untouched.
	type slot struct {
		rep   ui.ViewMessage
		count int
	}
	byConv := make(map[string]*slot, len(msgs))
	order := make([]string, 0, len(msgs))
	for i, m := range msgs {
		key := m.ConversationID
		if key == "" {
			key = "\x00msg:" + m.ID // unique per-message key so it isn't merged
		}
		s, ok := byConv[key]
		if !ok {
			byConv[key] = &slot{rep: msgs[i], count: 1}
			order = append(order, key)
			continue
		}
		s.count++
		// Keep the newest as representative.
		if m.Received.After(s.rep.Received) {
			s.rep = msgs[i]
		}
	}
	out := make([]ui.ViewMessage, 0, len(byConv))
	for _, key := range order {
		s := byConv[key]
		s.rep.ConversationSize = s.count
		out = append(out, s.rep)
	}
	return out
}

// cachedToView converts a local cached message to the UI view-model. Parallel
// to ui.FromGraph but works off the SQLite row.
func cachedToView(cm cache.CachedMessage) ui.ViewMessage {
	g := graph.Message{
		ID:               cm.ID,
		ConversationID:   cm.ConversationID,
		Subject:          cm.Subject,
		ReceivedDateTime: cm.ReceivedAt.Format(time.RFC3339),
		Categories:       cm.Categories,
		WebLink:          cm.WebLink,
		HasAttachments:   cm.HasAttachments,
		BodyPreview:      cm.BodyPreview,
	}
	if cm.SenderEmail != "" {
		g.From = &graph.EmailAddr{}
		g.From.EmailAddress.Address = cm.SenderEmail
		g.From.EmailAddress.Name = cm.SenderName
	}
	return ui.FromGraph(g)
}

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	active := parseFilter(r.URL.Query().Get("filter"))
	poNo := parsePO(r.URL.Query().Get("po"))
	queueDrill := strings.TrimSpace(r.URL.Query().Get("queue"))
	all, err := s.fetchMessages()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	// PO filter overrides the standard filters: when a clerk drills into a
	// specific PO they want every related message regardless of status,
	// owner, or hidden Kind. So apply PO filtering before the normal filter
	// short-circuits, and reset Active to All so the visual filter chip
	// doesn't lie about scope.
	if poNo > 0 {
		all = filterByPO(all, poNo)
		active = ui.FilterAll
	}

	// Admin drill-down (?queue=needs-first-pass|rescan|errored) overrides
	// all other filters and shows whatever the cache returned, capped to
	// last 7 days. Like the PO filter, resets Active to All so the chip
	// reflects scope honestly.
	var drillLabel string
	if queueDrill != "" {
		dctx, dcancel := context.WithTimeout(r.Context(), 3*time.Second)
		ids, label, derr := s.resolveQueueDrilldown(dctx, queueDrill)
		dcancel()
		if derr == nil && ids != nil {
			all = filterByIDSet(all, ids)
			active = ui.FilterAll
			drillLabel = label
		} else {
			queueDrill = "" // unknown kind → ignore
		}
	}

	user := s.effectiveUser(r)
	filterList := s.filtersFor(r)
	filters := make([]filterOpt, 0, len(filterList))
	for _, f := range filterList {
		filters = append(filters, filterOpt{
			Key:   f,
			Label: f.Label(),
			Count: len(ui.Apply(all, f, user)),
		})
	}
	msgs := ui.Apply(all, active, user)
	total, totalRows := queueTotalFor(msgs)
	data := pageData{
		Mailbox:         s.mailbox,
		User:            user,
		Active:          active,
		Filters:         filters,
		Messages:        msgs,
		PONo:            poNo,
		QueueDrill:      queueDrill,
		QueueDrillLabel: drillLabel,
		QueueTotal:      total,
		QueueTotalRows:  totalRows,
	}
	s.hydrateChrome(r, &data)
	if err := s.tmpl.ExecuteTemplate(w, "layout.html", data); err != nil {
		log.Printf("render index: %v", err)
	}
}

func (s *server) handleList(w http.ResponseWriter, r *http.Request) {
	active := parseFilter(r.URL.Query().Get("filter"))
	poNo := parsePO(r.URL.Query().Get("po"))
	queueDrill := strings.TrimSpace(r.URL.Query().Get("queue"))
	all, err := s.fetchMessages()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if poNo > 0 {
		all = filterByPO(all, poNo)
		active = ui.FilterAll
	}
	var drillLabel string
	if queueDrill != "" {
		dctx, dcancel := context.WithTimeout(r.Context(), 3*time.Second)
		ids, label, derr := s.resolveQueueDrilldown(dctx, queueDrill)
		dcancel()
		if derr == nil && ids != nil {
			all = filterByIDSet(all, ids)
			active = ui.FilterAll
			drillLabel = label
		} else {
			queueDrill = ""
		}
	}
	user := s.effectiveUser(r)
	msgs := ui.Apply(all, active, user)
	total, totalRows := queueTotalFor(msgs)
	data := pageData{
		Mailbox:         s.mailbox,
		User:            user,
		Active:          active,
		Messages:        msgs,
		PONo:            poNo,
		QueueDrill:      queueDrill,
		QueueDrillLabel: drillLabel,
		QueueTotal:      total,
		QueueTotalRows:  totalRows,
	}
	if err := s.tmpl.ExecuteTemplate(w, "list.html", data); err != nil {
		log.Printf("render list: %v", err)
	}
}

// handleSearch renders the main layout with search results substituted for
// the normal filtered list. Hits SQLite cache only — never reaches Graph.
func (s *server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	data := pageData{Mailbox: s.mailbox, User: s.effectiveUser(r), Active: ui.FilterAll, Query: q}
	s.hydrateChrome(r, &data)

	if q == "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	hits, err := s.cache.SearchMessages(ctx, s.mailbox, q, 200)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	summaries, _ := s.cache.ListAISummaryForMailbox(ctx, s.mailbox)
	out := make([]ui.ViewMessage, 0, len(hits))
	for _, cm := range hits {
		vm := cachedToView(cm)
		if sum, ok := summaries[cm.ID]; ok {
			vm.AIProcessed = sum.Processed
			vm.AIHasExtraction = sum.HasExtraction
			vm.AIHasReconcile = sum.HasReconcile
			vm.AITotalMatch = sum.TotalMatch
			vm.AIAnyLineMismatch = sum.AnyLineMismatch
			vm.AIErrorMsg = sum.ErrorMsg
			vm.AINeedsRescan = sum.NeedsRescan
		}
		out = append(out, vm)
	}
	data.Messages = out
	if err := s.tmpl.ExecuteTemplate(w, "layout.html", data); err != nil {
		log.Printf("render search: %v", err)
	}
}

// --- actions ---

// handleClaim toggles Owner: between currentUser and unset.
func (s *server) handleClaim(w http.ResponseWriter, r *http.Request) {
	msgID, err := decodeRowID(chi.URLParam(r, "rowID"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	m, err := s.gc.GetMessage(s.mailbox, msgID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	user := s.effectiveUser(r)
	current := ownerFrom(m.Categories)
	var next string
	if strings.EqualFold(current, user) {
		next = "" // release
	} else {
		next = user // claim (or steal)
	}
	newCats := ui.ReplaceOwner(m.Categories, next)
	if err := s.gc.SetCategories(s.mailbox, msgID, newCats); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	// Keep the local cache in sync so the row we just re-render reflects the
	// new categories without waiting for the next sync tick.
	_ = s.cache.UpdateCategories(r.Context(), s.mailbox, msgID, newCats)
	s.renderRow(w, msgID, s.effectiveUser(r))
}

// handleStatus sets Status: to form field "status".
func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	msgID, err := decodeRowID(chi.URLParam(r, "rowID"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	status := r.FormValue("status")
	if !ui.ValidStatus(status) {
		http.Error(w, "bad status", http.StatusBadRequest)
		return
	}
	m, err := s.gc.GetMessage(s.mailbox, msgID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	newCats := ui.ReplaceStatus(m.Categories, status)
	if err := s.gc.SetCategories(s.mailbox, msgID, newCats); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	_ = s.cache.UpdateCategories(r.Context(), s.mailbox, msgID, newCats)
	s.renderRow(w, msgID, s.effectiveUser(r))
}

// handleVerdict records a clerk verdict on the AI extraction. Phase 1 of the
// accuracy loop (see ACCURACY-LOOP.md) — these append-only rows are what
// every later phase (diagnostic dump, teacher, eval, promotion) consumes.
// Three canonical verdicts: "right" (extraction looks correct), "wrong"
// (something's off, no detail), "corrected" (extraction wrong + clerk supplied
// the right values). For 'corrected', the form posts po_number /
// invoice_number / invoice_total / notes which we marshal into a JSON blob;
// downstream phases get a stable shape to read.
func (s *server) handleVerdict(w http.ResponseWriter, r *http.Request) {
	msgID, err := decodeRowID(chi.URLParam(r, "rowID"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	verdict := strings.TrimSpace(r.FormValue("verdict"))
	switch verdict {
	case "right", "wrong", "corrected":
	default:
		http.Error(w, "bad verdict", http.StatusBadRequest)
		return
	}

	correctedJSON := ""
	if verdict == "corrected" {
		payload := map[string]string{
			"po_number":      strings.TrimSpace(r.FormValue("po_number")),
			"invoice_number": strings.TrimSpace(r.FormValue("invoice_number")),
			"invoice_total":  strings.TrimSpace(r.FormValue("invoice_total")),
			"notes":          strings.TrimSpace(r.FormValue("notes")),
		}
		if b, err := json.Marshal(payload); err == nil {
			correctedJSON = string(b)
		}
	}

	user := s.effectiveUser(r)
	if err := s.cache.RecordVerdict(r.Context(), s.mailbox, msgID, user, verdict, correctedJSON); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Render just the verdict-buttons fragment for HTMX outerHTML swap so the
	// surrounding page (detail panel or AP side-by-side) doesn't reload.
	// Non-HTMX callers (curl, bookmarks) get a 303 back to the referer.
	if r.Header.Get("HX-Request") == "" {
		ref := r.Header.Get("Referer")
		if ref == "" {
			ref = "/"
		}
		http.Redirect(w, r, ref, http.StatusSeeOther)
		return
	}

	m, err := s.gc.GetMessage(s.mailbox, msgID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	vm := ui.FromGraph(*m)
	frag := struct {
		M             ui.ViewMessage
		RecentVerdict *cache.Verdict
	}{M: vm}
	if list, err := s.cache.ListVerdictsByMessage(r.Context(), s.mailbox, msgID); err == nil {
		for i := range list {
			if strings.EqualFold(list[i].User, user) {
				frag.RecentVerdict = &list[i]
				break
			}
		}
	}
	if err := s.tmpl.ExecuteTemplate(w, "verdict-buttons", frag); err != nil {
		log.Printf("render verdict-buttons: %v", err)
	}
}

// handleBlocker toggles Blocker: {name} on/off, and auto-manages Status:Blocked.
func (s *server) handleBlocker(w http.ResponseWriter, r *http.Request) {
	msgID, err := decodeRowID(chi.URLParam(r, "rowID"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	name := chi.URLParam(r, "name")
	if !ui.ValidBlocker(name) {
		http.Error(w, "bad blocker", http.StatusBadRequest)
		return
	}
	m, err := s.gc.GetMessage(s.mailbox, msgID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	newCats := ui.ToggleBlocker(m.Categories, name)
	if err := s.gc.SetCategories(s.mailbox, msgID, newCats); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	_ = s.cache.UpdateCategories(r.Context(), s.mailbox, msgID, newCats)
	s.renderRow(w, msgID, s.effectiveUser(r))
}

// handleMessageBody serves the message's HTML (or text) body as an independent
// page so the detail-pane iframe can load it via src= instead of srcdoc=.
// Go's html/template aggressively sanitizes srcdoc attribute contents (strips
// <table>, inline styles, etc.), so long vendor-email HTML gets reduced to a
// flat text run. Serving the body as its own page bypasses that entirely.
func htmlEscape(s string) string {
	return template.HTMLEscapeString(s)
}

func (s *server) handleMessageBody(w http.ResponseWriter, r *http.Request) {
	msgID, err := decodeRowID(chi.URLParam(r, "rowID"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	m, err := s.gc.GetMessageDetail(s.mailbox, msgID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// CSP hardens what the email can do inside the iframe sandbox:
	// - no scripts, no frames, no forms
	// - images allowed (inline vendor logos are useful context)
	// - inline styles allowed (vendor emails rely on them)
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; img-src * data:; style-src 'unsafe-inline' *; font-src *; media-src *; base-uri 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	if m.Body == nil {
		fmt.Fprint(w, "<html><body style='padding:1rem;color:#6b7280;font-family:sans-serif;'>(empty body)</body></html>")
		return
	}
	if strings.EqualFold(m.Body.ContentType, "html") {
		collapsed, _ := ui.CollapseQuotedHTML(m.Body.Content)
		fmt.Fprint(w, collapsed)
		return
	}
	// Plain text: wrap in a readable HTML shell, preserve whitespace, linkify bare URLs.
	fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><style>
body { font-family: -apple-system, Segoe UI, Roboto, sans-serif; color: #111827; line-height: 1.5; padding: 1rem; }
pre { white-space: pre-wrap; word-wrap: break-word; font-family: inherit; margin: 0; }
a { color: #1e40af; }
</style></head><body><pre>%s</pre></body></html>`, htmlEscape(m.Body.Content))
}

// handleAttachment streams an attachment from Graph straight to the browser.
// - Blocked for catchall (malware risk)
// - PDFs and safe image types render inline; everything else forces download
// - No bytes touch disk; stream is copied directly to the response
func (s *server) handleAttachment(w http.ResponseWriter, r *http.Request) {
	if attachmentBlockedMailboxes[strings.ToLower(s.mailbox)] {
		http.Error(w, "attachment streaming disabled for this mailbox (phishing-adjacent)", http.StatusForbidden)
		return
	}
	msgID, err := decodeRowID(chi.URLParam(r, "rowID"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	attID := chi.URLParam(r, "attID")

	// Look up filename from metadata so Content-Disposition is nice.
	atts, err := s.gc.ListAttachments(s.mailbox, msgID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	var filename string
	var declaredType string
	for _, a := range atts {
		if a.ID == attID {
			filename = a.Name
			declaredType = a.ContentType
			break
		}
	}
	if filename == "" {
		http.Error(w, "attachment not found", http.StatusNotFound)
		return
	}

	body, contentType, contentLen, err := s.gc.FetchAttachmentContent(s.mailbox, msgID, attID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer body.Close()

	// Use the strongest-evidence content type available.
	if contentType == "" {
		contentType = declaredType
	}
	// Normalize (strip charset params)
	baseType := strings.TrimSpace(strings.Split(contentType, ";")[0])

	disposition := "attachment"
	if inlineContentTypes[strings.ToLower(baseType)] {
		disposition = "inline"
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`%s; filename=%q`, disposition, filename))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "private, max-age=60")
	if contentLen > 0 {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", contentLen))
	}
	if _, err := io.Copy(w, body); err != nil {
		log.Printf("stream attachment: %v", err)
	}
}

// handleDetail renders the detail pane for one message: full body, recipients,
// attachment metadata, and the other messages in its conversation.
func (s *server) handleDetail(w http.ResponseWriter, r *http.Request) {
	msgID, err := decodeRowID(chi.URLParam(r, "rowID"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	data, err := s.buildDetailData(r.Context(), msgID, s.effectiveUser(r))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if err := s.tmpl.ExecuteTemplate(w, "detail.html", data); err != nil {
		log.Printf("render detail: %v", err)
	}
}

// handleReview renders the focused full-screen review view: one message at a
// time, position counter, ←/→ keyboard nav. Bounds-clamped — out-of-range
// index redirects to the inbox-zero screen. Filter context is preserved across
// nav clicks so "next" means "next in the same filter."
func (s *server) handleReview(w http.ResponseWriter, r *http.Request) {
	filter := parseFilter(r.URL.Query().Get("filter"))
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
	msgs := ui.Apply(all, filter, user)

	// session-start cookie tracks the elapsed time shown on the
	// inbox-zero screen. Set on first entry, cleared on the done screen.
	startedAt := getOrSetReviewStart(w, r, filter)

	if len(msgs) == 0 || idx >= len(msgs) {
		// Inbox-zero — clerk reviewed past the end. Render the done screen
		// with elapsed time. Cookie cleared so a re-entry starts a new timer.
		clearReviewStart(w, filter)
		done := reviewDoneData{
			Mailbox:     s.mailbox,
			User:        user,
			Filter:      filter,
			FilterLabel: filter.Label(),
			Total:       len(msgs),
			Elapsed:     time.Since(startedAt).Round(time.Second),
		}
		if err := s.tmpl.ExecuteTemplate(w, "review-done.html", done); err != nil {
			log.Printf("render review-done: %v", err)
		}
		return
	}

	current := msgs[idx]
	detail, err := s.buildDetailData(r.Context(), current.ID, user)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	detail.IsReview = true

	prevIdx := idx - 1
	nextIdx := idx + 1
	if nextIdx >= len(msgs) {
		// Don't return -1; the keyboard nav clamps to len so the next '→'
		// lands on the inbox-zero screen rather than getting wedged.
		nextIdx = len(msgs)
	}

	data := reviewData{
		detailData:  detail,
		Filter:      filter,
		FilterLabel: filter.Label(),
		Index:       idx,
		Total:       len(msgs),
		PrevIndex:   prevIdx,
		NextIndex:   nextIdx,
		StartedAt:   startedAt,
	}
	if err := s.tmpl.ExecuteTemplate(w, "review.html", data); err != nil {
		log.Printf("render review: %v", err)
	}
}

// =====================================================================
// AP-MODE SURFACE
//
// Clerk-first view. Sits alongside the admin /review and /list during pilot.
// Three filter tabs (To do / Waiting / Done today), three decision buttons
// (Approve / Hold / Skip), plain-English everywhere, full-screen one-at-a-time.
// Power tools (rotation, manual entry, reclassify) live behind a "More" menu
// that the AP-mode template renders collapsed by default.
//
// Mode detection (future): if effectiveUser is in the the ERP AP-Clerk role, the
// admin / handler will redirect to /ap. For now everyone can hit /ap directly
// for testing.
// =====================================================================

// admin is impersonating an AP user; full set otherwise. The narrowed set
// matches what AP people actually care about: Unclaimed → Mine → Discrepancy
// → Done → All.
func (s *server) filtersFor(r *http.Request) []ui.Filter {
	if s.isImpersonating(r) {
		return filterOrderImpersonating
	}
	return filterOrder
}

// hydrateChrome populates the impersonation banner + AP-user dropdown on
// pageData. Called by full-page renders (handleIndex, handleSearch); HTMX
// partials skip it because they don't render the chrome.
func (s *server) hydrateChrome(r *http.Request, data *pageData) {
	data.AuthUser = s.user
	if s.isImpersonating(r) {
		data.Impersonating = true
		// Best-effort: look up the display name from the AP user list.
		// Cached, so this is cheap.
		if s.erp != nil {
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			users, err := s.erp.ListAPUsers(ctx)
			cancel()
			if err == nil {
				cur := s.impersonatedID(r)
				for _, u := range users {
					if strings.EqualFold(u.ID, cur) {
						data.ImpersonatedName = u.Name
						break
					}
				}
			}
		}
	}
	// Always populate the AP user list — the dropdown shows even when not
	// impersonating, so the admin can pick someone.
	if s.erp != nil && !data.Impersonating {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		if users, err := s.erp.ListAPUsers(ctx); err == nil {
			data.APUsers = users
		}
		cancel()
	}
}

// handleImpersonateSet sets the impersonation cookie to a the ERP AP-user ID and
// redirects home. Validates against the live AP user list so a typo or
// non-AP id silently bounces back. 8h cookie life — survives a workday but
// expires overnight so the next day starts as the real user.
func (s *server) handleImpersonateSet(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	target := strings.ToLower(strings.TrimSpace(r.FormValue("user")))
	if target == "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	// Verify the target is a current AP user — silent no-op on mismatch so
	// nobody can drop arbitrary cookie values via crafted form posts.
	if s.erp == nil {
		http.Error(w, "erp not configured", http.StatusServiceUnavailable)
		return
	}
	listCtx, listCancel := context.WithTimeout(r.Context(), 3*time.Second)
	users, err := s.erp.ListAPUsers(listCtx)
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
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     impersonateCookie,
		Value:    target,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   8 * 3600,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleImpersonateExit clears the impersonation cookie and redirects home.
func (s *server) handleImpersonateExit(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     impersonateCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// impersonateCookie names the cookie that overrides s.user for a session.
// Lowercase user ID; empty/missing means no impersonation. 8h MaxAge so
// admins don't accidentally leave it on overnight.
const impersonateCookie = "dispatch_impersonate"

// effectiveUser returns the user ID for filter logic + mutation Owner writes.
// When the impersonation cookie is set, that wins; otherwise the auth user.
// Always lowercased so EqualFold compares against Buyer / Owner are stable.
func (s *server) effectiveUser(r *http.Request) string {
	if c, err := r.Cookie(impersonateCookie); err == nil && c.Value != "" {
		return strings.ToLower(strings.TrimSpace(c.Value))
	}
	return strings.ToLower(s.user)
}

// isImpersonating returns true when the request has the impersonation cookie.
// Used by handlers + the layout banner.
func (s *server) isImpersonating(r *http.Request) bool {
	c, err := r.Cookie(impersonateCookie)
	return err == nil && c.Value != ""
}

// impersonatedID returns the cookie value (empty string when not impersonating).
func (s *server) impersonatedID(r *http.Request) string {
	c, err := r.Cookie(impersonateCookie)
	if err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(c.Value))
}

// reviewStartCookie names the per-filter session-start cookie. Per-filter so a
// clerk who flips Review from Open → Discrepancy starts a fresh elapsed timer
// instead of seeing combined time.
func reviewStartCookie(filter ui.Filter) string {
	return "dispatch_review_start_" + string(filter)
}

func getOrSetReviewStart(w http.ResponseWriter, r *http.Request, filter ui.Filter) time.Time {
	name := reviewStartCookie(filter)
	if c, err := r.Cookie(name); err == nil {
		if ts, perr := time.Parse(time.RFC3339, c.Value); perr == nil {
			return ts
		}
	}
	now := time.Now().UTC()
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    now.Format(time.RFC3339),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   8 * 3600, // 8h — survives lunch break, expires overnight
	})
	return now
}

func clearReviewStart(w http.ResponseWriter, filter ui.Filter) {
	http.SetCookie(w, &http.Cookie{
		Name:     reviewStartCookie(filter),
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
	})
}

// buildDetailData assembles the per-message detail view: body, thread,
// attachments, extraction, recon, related messages. Pure data — no HTTP. Shared
// by handleDetail (HTMX panel swap) and handleReview (full-screen mode).
// `user` is the effective user (impersonated or auth) — used for Owner-aware
// display in the detail template.
func (s *server) buildDetailData(ctx context.Context, msgID, user string) (detailData, error) {
	// Try local cache first (Phase 1). Falls through to Graph on miss
	// so first-view-of-a-message still works, just slower.
	bodyCtx, bodyCancel := context.WithTimeout(ctx, 2*time.Second)
	cachedHTML, cachedText, _, _ := s.cache.GetMessageBody(bodyCtx, s.mailbox, msgID)
	bodyCancel()

	m, err := s.gc.GetMessageDetail(s.mailbox, msgID)
	if err != nil {
		return detailData{}, err
	}
	vm := ui.FromGraph(*m)

	data := detailData{M: vm, User: user}
	// Prefer local cached body if we have it — resilient to Graph outages
	// after first fetch, and avoids re-downloading the same body repeatedly.
	if cachedHTML != "" {
		collapsed, _ := ui.CollapseQuotedHTML(cachedHTML)
		data.BodyHTML = template.HTML(collapsed)
	} else if cachedText != "" {
		data.BodyText = cachedText
	} else if m.Body != nil {
		if strings.EqualFold(m.Body.ContentType, "html") {
			collapsed, _ := ui.CollapseQuotedHTML(m.Body.Content)
			data.BodyHTML = template.HTML(collapsed)
		} else {
			data.BodyText = m.Body.Content
		}
	}
	data.To = joinAddrs(m.ToRecipients)
	data.Cc = joinAddrs(m.CcRecipients)

	if m.HasAttachments {
		atts, err := s.gc.ListAttachments(s.mailbox, msgID)
		if err == nil {
			// Filter inline (embedded images etc) — show only real attachments
			real := make([]graph.Attachment, 0, len(atts))
			for _, a := range atts {
				if !a.IsInline {
					real = append(real, a)
				}
			}
			data.Attachments = real
			// Promote the first PDF to inline preview position
			if !attachmentBlockedMailboxes[strings.ToLower(s.mailbox)] {
				for i := range real {
					if strings.EqualFold(real[i].ContentType, "application/pdf") {
						data.FirstPDF = &real[i]
						// Pull saved rotation if any. Cache miss → 0 (no rotation).
						rotCtx, rotCancel := context.WithTimeout(ctx, 1*time.Second)
						a, _ := s.cache.GetAttachmentRotation(rotCtx, s.mailbox, msgID, real[i].ID)
						rotCancel()
						data.FirstPDFAngle = a
						break
					}
				}
			}
		}
	}

	// Build Gmail-style thread cards. If the conversation has multiple messages,
	// render them all with full bodies; otherwise just this one.
	var threadMsgs []graph.Message
	if m.ConversationID != "" {
		if tt, err := s.gc.ListConversationMessages(s.mailbox, m.ConversationID); err == nil && len(tt) > 0 {
			threadMsgs = tt
		}
	}
	if len(threadMsgs) == 0 {
		threadMsgs = []graph.Message{*m}
	}
	data.Thread = buildThreadCards(threadMsgs, m.ID)

	// Phase A/B: attach cached invoice extraction + reconciliation.
	cacheCtx, cacheCancel := context.WithTimeout(ctx, 2*time.Second)
	var poForRelated int64
	if ext, err := s.cache.GetInvoiceExtraction(cacheCtx, s.mailbox, msgID); err == nil && ext != nil {
		data.Extraction = ext
		poForRelated = ext.PONo
		if ext.ReconcileJSON != "" {
			var rec recon.Reconciliation
			if jerr := json.Unmarshal([]byte(ext.ReconcileJSON), &rec); jerr == nil {
				data.Recon = &rec
			}
		}
	}
	cacheCancel()

	// Related-messages lookup: when this message is an internal reply about
	// a PO OR a vendor invoice on a PO, surface other cached messages with
	// the same PO number. Gives clerks one-click context across the thread.
	if poForRelated > 0 {
		relCtx, relCancel := context.WithTimeout(ctx, 2*time.Second)
		if rel, err := s.cache.ListRelatedByPO(relCtx, s.mailbox, poForRelated, msgID, 10); err == nil {
			data.Related = rel
			data.RelatedPO = poForRelated
		}
		relCancel()
	}

	// Clerk notes: append-only log shown above the thread. Cheap query —
	// indexed on (mailbox, message_id, created_at).
	noteCtx, noteCancel := context.WithTimeout(ctx, 2*time.Second)
	if notes, err := s.cache.ListInvoiceNotes(noteCtx, s.mailbox, msgID); err == nil {
		data.Notes = notes
	}
	noteCancel()

	// Phase 1 of accuracy loop: surface the current user's most recent
	// verdict so the buttons can show "You marked this Wrong 2h ago" instead
	// of looking blank after a click. List is newest-first; first match wins.
	verdictCtx, verdictCancel := context.WithTimeout(ctx, 2*time.Second)
	if list, err := s.cache.ListVerdictsByMessage(verdictCtx, s.mailbox, msgID); err == nil {
		for i := range list {
			if strings.EqualFold(list[i].User, user) {
				data.RecentVerdict = &list[i]
				break
			}
		}
	}
	verdictCancel()

	// Vendor mini-history: last 5 invoices from the same vendor + aggregate
	// stats. Lets the clerk see "is this vendor normally smooth?" without
	// hitting search. Skipped silently for Unknown / unresolved vendor.
	if data.M.Vendor != "" {
		vhCtx, vhCancel := context.WithTimeout(ctx, 2*time.Second)
		hist, sum, err := s.cache.GetVendorHistory(vhCtx, s.mailbox, data.M.Vendor, msgID, 5)
		vhCancel()
		if err == nil {
			data.VendorHistory = hist
			data.VendorStats = sum
		}
	}

	return data, nil
}

// renderRow re-fetches the message and renders just its row (for HTMX outerHTML swap).
// `user` is the effective user — equals impersonated ID when active, else auth user.
func (s *server) renderRow(w http.ResponseWriter, msgID, user string) {
	m, err := s.gc.GetMessage(s.mailbox, msgID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	vm := ui.FromGraph(*m)
	data := map[string]any{"M": vm, "User": user}
	if err := s.tmpl.ExecuteTemplate(w, "row", data); err != nil {
		log.Printf("render row: %v", err)
	}
}

// --- helpers ---

// Graph message IDs contain characters that don't round-trip through URL paths
// cleanly. rowID/decodeRowID wrap them in urlsafe base64.
func rowID(id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(id))
}

func decodeRowID(rid string) (string, error) {
	b, err := base64.RawURLEncoding.DecodeString(rid)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// workerViewData is the shape consumed by both the status-bar fragment and the
// full queue page. Pre-formatted strings keep the template simple.
type workerViewData struct {
	Mailbox           string
	User              string
	Running           bool   // any slot has a fresh heartbeat AND current_message_id set
	Stale             bool   // has-current but heartbeat older than staleAfter
	Idle              bool   // a run happened but nothing in flight right now
	StatusText        string // one-line phrase for the bar: "4 workers · ai-vision Distributor-Vendor" / "idle"
	StepLabel         string // most-recent step across active slots, for the bar
	CurrentSubject    string // busiest slot's subject (for bar)
	CurrentVendor     string
	CurrentElapsed    string
	BusySlotCount     int
	TotalSlotCount    int
	Slots             []slotView // per-slot rendering for the queue page's Now section
	ProcessedThisRun  int
	RunElapsed        string
	LastSubject       string
	LastAgo           string
	PendingCount      int // messages without Status: tag (untouched by worker)
	UnextractedCount  int // messages without an invoice_extractions row (mostly non-invoice)
	RescanCount       int
	ErroredCount      int // recent (last 7d) extractions with non-empty error_msg — drives the admin drill-down counter
	RecentDone        []cache.Completion
	PrimaryEndpoints  []endpointInfo
	FallbackEndpoints []endpointInfo
	PaddleEndpoints   []endpointInfo
	ModelMix          []modelCount
	ModelStats        []modelStats
}

// slotView is one worker slot rendered in the Now card. Covers all three UI
// states: busy (has a current message), stale (heartbeat gone quiet), idle.
// Pool is "sort", "extract", or "fallback" so the UI can group them.
type slotView struct {
	Pool    string
	Slot    int
	State   string // "busy" | "stale" | "idle"
	Step    string
	Subject string
	Vendor  string
	Elapsed string
}

// endpointInfo is a live snapshot of one Ollama host for the queue page's
// Endpoints panel. LoadedModel is empty when the server is idle or unreachable.
type endpointInfo struct {
	Role           string // "primary" or "fallback"
	URL            string
	Reachable      bool
	Busy           bool // model loaded in VRAM
	Model          string
	VRAMGB         float64
	LatencyMs      int
	Err            string
	CurrentElapsed string // live "how long has this request been running" — empty when not in flight
	LastAgo        string // "34s ago" when idle
	LastDurMs      int    // last completed request duration
	MeanDurMs      int    // rolling mean from totals
	Requests       int
	Errors         int
}

// modelStats summarizes extraction outcomes for one model tag. Used by the
// queue page's Model Performance card so the operator can see which
// tier/model pulls its weight — clean match rate is the most honest metric
// because it's the full end-to-end test (extraction + the ERP reconciliation).
type modelStats struct {
	Model         string  // raw model column value (includes text(pdftotext):... prefixes)
	Total         int     // extractions produced by this model
	Clean         int     // needs_rescan=0 AND no error
	Rescanned    int      // needs_rescan=1 — queued for a higher tier
	Errored       int     // error_msg set
	CleanPct      int     // Clean / Total * 100, rounded
	AvgMs         int     // mean elapsed_ms across extractions
}

// modelCount is one bar in the model-mix chart (recent completions split by
// which tier actually produced the verdict).
type modelCount struct {
	Model string
	Count int
	Pct   int
}

// workerStaleAfter is the time without a heartbeat before the UI shows "stalled".
// Needs to be longer than the worst-case in-call duration without an update. The
// fallback gemma4:26b on CPU takes up to 2 min on a heavy invoice, so 3 min is
// the safe floor. Shorter values flash "stalled" on legitimate fallback work.
const workerStaleAfter = 3 * time.Minute
const rescanMaxAttempts = 3

func (s *server) buildWorkerView(ctx context.Context) (*workerViewData, error) {
	slots, err := s.cache.GetAllWorkerStates(ctx)
	if err != nil {
		return nil, err
	}
	pending, _ := s.cache.PendingInboxCount(ctx, s.mailbox)
	unextracted, _ := s.cache.UnextractedCount(ctx, s.mailbox)
	rescan, _ := s.cache.RescanQueueDepth(ctx, s.mailbox, rescanMaxAttempts)
	recent, _ := s.cache.ListRecentCompletions(ctx, s.mailbox, 15)

	vd := &workerViewData{
		Mailbox:          s.mailbox,
		User:             s.user,
		PendingCount:     pending,
		UnextractedCount: unextracted,
		RescanCount:      rescan,
		RecentDone:       recent,
		TotalSlotCount:   len(slots),
	}
	if len(slots) == 0 {
		vd.StatusText = "worker has not run yet"
		return vd, nil
	}

	// Aggregate run-level fields from slot 0 (where they're stored).
	head := slots[0]
	vd.ProcessedThisRun = head.ProcessedThisRun
	if !head.RunStartedAt.IsZero() {
		vd.RunElapsed = fmtDur(time.Since(head.RunStartedAt))
	}

	// Build per-slot views + pick the most-informative one for the status bar.
	var busiestBusy *cache.WorkerState
	var busiestStale *cache.WorkerState
	latestCompletedAt := time.Time{}
	var latestCompletedID string
	for i := range slots {
		st := slots[i]
		age := time.Since(st.HeartbeatAt)
		sv := slotView{Pool: st.Pool, Slot: st.Slot}
		switch {
		case st.CurrentMessageID != "":
			if age > workerStaleAfter {
				sv.State = "stale"
				if busiestStale == nil {
					busiestStale = &slots[i]
				}
			} else {
				sv.State = "busy"
				// Pick whichever busy slot has been running longest — more informative.
				if busiestBusy == nil || (!st.CurrentStartedAt.IsZero() &&
					(busiestBusy.CurrentStartedAt.IsZero() || st.CurrentStartedAt.Before(busiestBusy.CurrentStartedAt))) {
					busiestBusy = &slots[i]
				}
				vd.BusySlotCount++
			}
			sv.Step = st.CurrentStep
			sv.Subject = st.CurrentSubject
			sv.Vendor = st.CurrentVendor
			if !st.CurrentStartedAt.IsZero() {
				sv.Elapsed = fmtDur(time.Since(st.CurrentStartedAt))
			}
		default:
			sv.State = "idle"
		}
		if st.LastCompletedAt.After(latestCompletedAt) {
			latestCompletedAt = st.LastCompletedAt
			latestCompletedID = st.LastCompletedMessageID
		}
		vd.Slots = append(vd.Slots, sv)
	}

	// Map aggregate state to legacy Running/Stale/Idle + StatusText.
	switch {
	case busiestBusy != nil:
		vd.Running = true
		s := busiestBusy
		vd.StepLabel = s.CurrentStep
		vd.CurrentSubject = s.CurrentSubject
		vd.CurrentVendor = s.CurrentVendor
		if !s.CurrentStartedAt.IsZero() {
			vd.CurrentElapsed = fmtDur(time.Since(s.CurrentStartedAt))
		}
		if vd.BusySlotCount > 1 {
			vd.StatusText = fmt.Sprintf("%d workers · %s %s",
				vd.BusySlotCount, s.CurrentStep, truncate(s.CurrentSubject, 50))
		} else {
			vd.StatusText = fmt.Sprintf("%s · %s", s.CurrentStep, truncate(s.CurrentSubject, 60))
		}
	case busiestStale != nil:
		vd.Stale = true
		s := busiestStale
		vd.StepLabel = s.CurrentStep
		vd.CurrentSubject = s.CurrentSubject
		vd.StatusText = fmt.Sprintf("stalled on %s · no heartbeat for %s",
			truncate(s.CurrentSubject, 60), fmtDur(time.Since(s.HeartbeatAt)))
	default:
		// No busy slots — idle. Use the newest heartbeat across slots as "last seen".
		newest := time.Time{}
		for _, st := range slots {
			if st.HeartbeatAt.After(newest) {
				newest = st.HeartbeatAt
			}
		}
		if newest.IsZero() {
			vd.StatusText = "worker has not run yet"
		} else {
			vd.Idle = true
			vd.StatusText = fmt.Sprintf("worker idle · last activity %s ago", fmtDur(time.Since(newest)))
		}
	}

	if latestCompletedID != "" {
		if co := findCompletion(recent, latestCompletedID); co != nil {
			vd.LastSubject = co.Subject
		}
		if !latestCompletedAt.IsZero() {
			vd.LastAgo = fmtDur(time.Since(latestCompletedAt))
		}
	}
	return vd, nil
}

func findCompletion(list []cache.Completion, id string) *cache.Completion {
	for i := range list {
		if list[i].MessageID == id {
			return &list[i]
		}
	}
	return nil
}

func fmtDur(d time.Duration) string {
	switch {
	case d < time.Second:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func (s *server) handleWorkerStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	vd, err := s.buildWorkerView(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.tmpl.ExecuteTemplate(w, "worker-status", vd); err != nil {
		log.Printf("render worker-status: %v", err)
	}
}

// handleAdmin renders the operational dashboard: worker pools, endpoint health,
// model stats, restart controls. Replaces the old /queue page (which now 301s
// here). The page mixes monitoring (read-only) with controls (restart workers,
// future: auto-assign rules), which is why it's "/admin" — clerks shouldn't
// see this; ops people do.
func (s *server) handleAdmin(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
	defer cancel()
	vd, err := s.buildWorkerView(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Richer signals only for /queue (not the status-bar fragment which
	// refreshes every 5s and shouldn't be hammering every endpoint).
	activity, _ := s.cache.GetEndpointActivity(ctx)
	vd.PrimaryEndpoints = pollEndpoints(ctx, "primary", s.primaryURLs, s.primaryModel, activity)
	vd.FallbackEndpoints = pollEndpoints(ctx, "fallback", s.fallbackURLs, s.fallbackModel, activity)
	vd.PaddleEndpoints = pollEndpoints(ctx, "paddle", s.paddleURLs, s.paddleModel, activity)
	// Per-model stats for the performance card — map cache rows to view struct.
	if rows, err := s.cache.ModelStatsBreakdown(ctx, s.mailbox); err == nil {
		vd.ModelStats = make([]modelStats, 0, len(rows))
		for _, r := range rows {
			ms := modelStats{
				Model:     r.Model,
				Total:     r.Total,
				Clean:     r.Clean,
				Rescanned: r.Rescanned,
				Errored:   r.Errored,
			}
			if r.Total > 0 {
				ms.CleanPct = (r.Clean * 100) / r.Total
				ms.AvgMs = r.TotalMs / r.Total
			}
			vd.ModelStats = append(vd.ModelStats, ms)
		}
	}
	// Recent errored count for the drill-down counter — same time window as
	// the drill-down lookup so the count and list agree.
	if ids, err := s.cache.ListErroredExtractions(ctx, s.mailbox, 7*24*time.Hour, 1000); err == nil {
		vd.ErroredCount = len(ids)
	}

	if mix, err := s.cache.ModelMix(ctx, s.mailbox, 14*24*time.Hour); err == nil {
		vd.ModelMix = rankModelMix(mix)
	}
	if err := s.tmpl.ExecuteTemplate(w, "queue.html", vd); err != nil {
		log.Printf("render admin: %v", err)
	}
}

// handleAdminRestartWorkers triggers a soft restart of dispatch-worker.service
// via systemctl. The cms user has a NOPASSWD sudoers entry for that one
// command (see deploy/dispatch-admin.sudoers). systemd sends SIGTERM, which
// the worker's request contexts respect — in-flight extractions get
// TimeoutStopSec=60 to land before SIGKILL.
//
// Returns plain text describing the result so the admin page can show it
// inline without a full reload.
func (s *server) handleAdminRestartWorkers(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sudo", "-n", "/bin/systemctl", "restart", "dispatch-worker.service")
	out, err := cmd.CombinedOutput()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err != nil {
		// Common cause: sudoers entry not installed yet on this host. Print
		// the captured output so the admin can copy/paste it for diagnosis.
		log.Printf("admin restart-workers: %v: %s", err, string(out))
		fmt.Fprintf(w, `<div class="admin-restart-result admin-restart-fail">restart failed: %s<br><pre>%s</pre><p class="muted">Likely cause: <code>cms</code> user lacks sudo to <code>systemctl restart dispatch-worker.service</code>. Install <code>deploy/dispatch-admin.sudoers</code> on this host.</p></div>`,
			template.HTMLEscapeString(err.Error()), template.HTMLEscapeString(string(out)))
		return
	}
	fmt.Fprintf(w, `<div class="admin-restart-result admin-restart-ok">✓ workers restarted · graceful TERM sent · in-flight calls have up to 60s to land</div>`)
}

// leaderRow is one row in any of the overview leaderboards: a label (vendor
// name, buyer ID, kind, etc.) plus a count.
type leaderRow struct {
	Label string
	Count int
}

// overviewData drives the /admin/overview template — leaderboards over the
// last N days of message metadata.
type overviewData struct {
	WindowDays         int
	Total              int
	WithKind           int
	ClassifiedPct      int         // WithKind / Total * 100, rounded
	KindBreakdown      []leaderRow // kind → count, sorted desc
	TopVendorsDispute  []leaderRow
	TopBuyersDispute   []leaderRow
	TopVendorsBlocked  []leaderRow
	TopBuyersBlocked   []leaderRow
	TopVendorsByVolume []leaderRow
	// Phase 1 of accuracy loop: vendors whose extractions clerks marked Wrong
	// or Corrected most often over the window. Empty until clerks start
	// recording verdicts.
	TopVendorsDisagreed []cache.VendorVerdictCount
}

// handleAdminOverview renders /admin/overview — vendor + buyer leaderboards
// derived from cached message metadata over the last 30 days. Pure analytics
// page; no controls. Runs in a single sweep over RecentMessageMeta and
// aggregates in memory (a few thousand messages, well under any threshold
// where we'd need a SQL group-by).
func (s *server) handleAdminOverview(w http.ResponseWriter, r *http.Request) {
	const windowDays = 30
	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
	defer cancel()

	since := time.Now().Add(-time.Duration(windowDays) * 24 * time.Hour).UTC()
	metas, err := s.cache.RecentMessageMeta(ctx, s.mailbox, since)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Counters keyed by (kind|status) → label → count. Cheap; one pass.
	kindCount := map[string]int{}
	vendorByKind := map[string]map[string]int{}
	buyerByKind := map[string]map[string]int{}
	vendorByStatus := map[string]map[string]int{}
	buyerByStatus := map[string]map[string]int{}
	vendorVolume := map[string]int{}
	withKind := 0
	for _, m := range metas {
		if m.Kind != "" {
			withKind++
			kindCount[m.Kind]++
			if m.Vendor != "" {
				if vendorByKind[m.Kind] == nil {
					vendorByKind[m.Kind] = map[string]int{}
				}
				vendorByKind[m.Kind][m.Vendor]++
			}
			if m.Buyer != "" {
				if buyerByKind[m.Kind] == nil {
					buyerByKind[m.Kind] = map[string]int{}
				}
				buyerByKind[m.Kind][m.Buyer]++
			}
		}
		if m.Status != "" {
			if m.Vendor != "" {
				if vendorByStatus[m.Status] == nil {
					vendorByStatus[m.Status] = map[string]int{}
				}
				vendorByStatus[m.Status][m.Vendor]++
			}
			if m.Buyer != "" {
				if buyerByStatus[m.Status] == nil {
					buyerByStatus[m.Status] = map[string]int{}
				}
				buyerByStatus[m.Status][m.Buyer]++
			}
		}
		if m.Vendor != "" {
			vendorVolume[m.Vendor]++
		}
	}

	classifiedPct := 0
	if len(metas) > 0 {
		classifiedPct = (withKind * 100) / len(metas)
	}
	disagreed, _ := s.cache.VerdictCountsByVendor(ctx, s.mailbox, since, 10)

	data := overviewData{
		WindowDays:          windowDays,
		Total:               len(metas),
		WithKind:            withKind,
		ClassifiedPct:       classifiedPct,
		KindBreakdown:       topN(kindCount, 20),
		TopVendorsDispute:   topN(vendorByKind["Dispute"], 10),
		TopBuyersDispute:    topN(buyerByKind["Dispute"], 10),
		TopVendorsBlocked:   topN(vendorByStatus["Blocked"], 10),
		TopBuyersBlocked:    topN(buyerByStatus["Blocked"], 10),
		TopVendorsByVolume:  topN(vendorVolume, 10),
		TopVendorsDisagreed: disagreed,
	}

	if err := s.tmpl.ExecuteTemplate(w, "overview.html", data); err != nil {
		log.Printf("render overview: %v", err)
	}
}

// topN returns the top n {label, count} entries from m, sorted by count desc
// with stable tiebreaker by label asc. Returns empty slice (not nil) so
// templates can safely range/len.
func topN(m map[string]int, n int) []leaderRow {
	rows := make([]leaderRow, 0, len(m))
	for label, c := range m {
		rows = append(rows, leaderRow{Label: label, Count: c})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Count != rows[j].Count {
			return rows[i].Count > rows[j].Count
		}
		return rows[i].Label < rows[j].Label
	})
	if len(rows) > n {
		rows = rows[:n]
	}
	return rows
}

// pollEndpoints queries each URL's /api/ps concurrently and merges in the
// cached endpoint_activity row (current elapsed, last duration, totals).
// Quick timeout per request so a dead endpoint doesn't stall the page render.
func pollEndpoints(parentCtx context.Context, role string, urls []string, expectedModel string, activity map[string]cache.EndpointActivity) []endpointInfo {
	out := make([]endpointInfo, len(urls))
	var wg sync.WaitGroup
	client := &http.Client{Timeout: 3 * time.Second}
	for i, u := range urls {
		wg.Add(1)
		go func(i int, u string) {
			defer wg.Done()
			info := endpointInfo{Role: role, URL: u}
			start := time.Now()
			req, _ := http.NewRequestWithContext(parentCtx, "GET", u+"/api/ps", nil)
			resp, err := client.Do(req)
			info.LatencyMs = int(time.Since(start).Milliseconds())
			if err != nil {
				info.Err = err.Error()
				out[i] = info
				return
			}
			defer resp.Body.Close()
			var body struct {
				Models []struct {
					Name     string `json:"name"`
					SizeVRAM int64  `json:"size_vram"`
				} `json:"models"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				info.Err = err.Error()
				out[i] = info
				return
			}
			info.Reachable = true
			for _, m := range body.Models {
				if strings.Contains(m.Name, expectedModel) || expectedModel == "" {
					info.Busy = true
					info.Model = m.Name
					info.VRAMGB = float64(m.SizeVRAM) / 1e9
					break
				}
			}
			if !info.Busy && len(body.Models) > 0 {
				// Something else loaded — surface it so it's visible.
				info.Model = body.Models[0].Name
				info.VRAMGB = float64(body.Models[0].SizeVRAM) / 1e9
			}
			if act, ok := activity[u]; ok {
				if !act.CurrentStartedAt.IsZero() {
					info.CurrentElapsed = fmtDur(time.Since(act.CurrentStartedAt))
				}
				if !act.LastCompletedAt.IsZero() {
					info.LastAgo = fmtDur(time.Since(act.LastCompletedAt))
				}
				info.LastDurMs = act.LastDurationMs
				info.Requests = act.TotalRequests
				info.Errors = act.TotalErrors
				if act.TotalRequests > 0 {
					info.MeanDurMs = act.TotalDurationMs / act.TotalRequests
				}
			}
			out[i] = info
		}(i, u)
	}
	wg.Wait()
	return out
}

// rankModelMix converts a {model → count} map into a sorted slice with percents
// computed off the total. Useful for a stacked-bar visual on the queue page.
func rankModelMix(m map[string]int) []modelCount {
	total := 0
	for _, n := range m {
		total += n
	}
	out := make([]modelCount, 0, len(m))
	for name, n := range m {
		label := name
		if label == "" {
			label = "(unknown)"
		}
		pct := 0
		if total > 0 {
			pct = n * 100 / total
		}
		out = append(out, modelCount{Model: label, Count: n, Pct: pct})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	return out
}

// splitCSV trims and returns non-empty comma-separated tokens.
func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func ownerFrom(cats []string) string {
	const prefix = "Owner: "
	for _, c := range cats {
		if strings.HasPrefix(c, prefix) {
			return strings.TrimPrefix(c, prefix)
		}
	}
	return ""
}

func parseFilter(s string) ui.Filter {
	switch ui.Filter(s) {
	case ui.FilterUnclaimed, ui.FilterMine, ui.FilterMyBuyer, ui.FilterBlocked, ui.FilterDone, ui.FilterMarketing, ui.FilterPayments, ui.FilterUnposted, ui.FilterRescan, ui.FilterMatch, ui.FilterDiscrepancy, ui.FilterAll:
		return ui.Filter(s)
	}
	return ui.FilterOpen
}

func relTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return t.Format("Jan 2")
	}
}

func statusSlug(s string) string {
	return strings.ToLower(strings.ReplaceAll(s, " ", ""))
}

// dict builds a map for passing multiple values into a template (Go's html/template
// doesn't have a built-in for this; one-liner keeps it simple).
func dict(kvs ...any) map[string]any {
	m := make(map[string]any, len(kvs)/2)
	for i := 0; i+1 < len(kvs); i += 2 {
		k, ok := kvs[i].(string)
		if !ok {
			continue
		}
		m[k] = kvs[i+1]
	}
	return m
}

// buildThreadCards converts raw Graph messages into ThreadCards, sorted newest
// first, with each body's quoted history collapsed so the thread doesn't double
// up on content. Internal-sender cards get a subtle flag for styling.
func buildThreadCards(msgs []graph.Message, currentID string) []ThreadCard {
	cards := make([]ThreadCard, 0, len(msgs))
	for _, m := range msgs {
		c := ThreadCard{
			ID:        m.ID,
			FromEmail: m.SenderAddress(),
			FromName:  m.SenderName(),
			Subject:   m.Subject,
			Preview:   m.BodyPreview,
			IsCurrent: m.ID == currentID,
		}
		if t, err := time.Parse(time.RFC3339, m.ReceivedDateTime); err == nil {
			c.ReceivedAt = t
		}
		if strings.HasSuffix(strings.ToLower(c.FromEmail), "@example.com") {
			c.IsInternal = true
		}
		if m.Body != nil {
			if strings.EqualFold(m.Body.ContentType, "html") {
				collapsed, _ := ui.CollapseQuotedHTML(m.Body.Content)
				c.BodyHTML = template.HTML(collapsed)
			} else {
				c.BodyText = m.Body.Content
			}
		}
		cards = append(cards, c)
	}
	// Newest first (Gmail convention for AP workflows — see the latest reply first).
	sort.Slice(cards, func(i, j int) bool { return cards[i].ReceivedAt.After(cards[j].ReceivedAt) })
	return cards
}

func joinAddrs(as []graph.EmailAddr) string {
	parts := make([]string, 0, len(as))
	for _, a := range as {
		if a.EmailAddress.Name != "" {
			parts = append(parts, fmt.Sprintf("%s <%s>", a.EmailAddress.Name, a.EmailAddress.Address))
		} else {
			parts = append(parts, a.EmailAddress.Address)
		}
	}
	return strings.Join(parts, ", ")
}

func humanSize(n int) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	case n < 1024*1024*1024:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	default:
		return fmt.Sprintf("%.1f GB", float64(n)/(1024*1024*1024))
	}
}

func parseAndRel(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return relTime(t)
}

// requirePassword gates every non-/healthz request behind HTTP Basic Auth with
// a single shared password. Intended to keep random browsers and bots out of
// the internal-only Dispatch UI; not a real auth system. Uses constant-time
// compare so response timing can't be used to probe the password.
func requirePassword(user, pw string) func(http.Handler) http.Handler {
	userBytes := []byte(user)
	pwBytes := []byte(pw)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/healthz" {
				next.ServeHTTP(w, r)
				return
			}
			gotUser, gotPW, ok := r.BasicAuth()
			if !ok ||
				subtle.ConstantTimeCompare([]byte(gotUser), userBytes) != 1 ||
				subtle.ConstantTimeCompare([]byte(gotPW), pwBytes) != 1 {
				w.Header().Set("WWW-Authenticate", `Basic realm="Dispatch"`)
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// requireSameOriginPost rejects state-changing requests (POST/PATCH/PUT/DELETE)
// whose Origin or Referer header doesn't match the request Host. Standard CSRF
// defense for cookie/Basic-auth flows: a malicious page can trigger the
// browser to make a cross-origin POST with credentials attached, but it cannot
// forge the Origin header.
//
// HTTP Basic auth makes us specifically vulnerable here — the browser sends
// credentials on every same-domain request automatically, so without this
// check, an attacker page could cause an authenticated admin to soft-restart
// workers, change message status, or post notes just by visiting the page.
//
// GETs are not protected: they're idempotent and our handlers don't change
// state on GET (verified by inspection: only the listed POST routes mutate).
func requireSameOriginPost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		if !sameOriginRequest(r) {
			http.Error(w, "cross-origin request rejected (CSRF protection)", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// sameOriginRequest verifies the request originated from the same host the
// server is serving. Prefers Origin (set by browsers on all POSTs in modern
// versions), falls back to Referer (sometimes sent when Origin isn't), and
// rejects when neither is present (a legitimate browser POST always includes
// at least one).
func sameOriginRequest(r *http.Request) bool {
	host := r.Host
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || u.Host == "" {
			return false
		}
		return u.Host == host
	}
	if ref := r.Header.Get("Referer"); ref != "" {
		u, err := url.Parse(ref)
		if err != nil || u.Host == "" {
			return false
		}
		return u.Host == host
	}
	return false
}

// envHasMSSQL reports whether the mssql config is discoverable via environment
// or the standard search paths erp.New checks. Used to auto-enable voucher
// sync when the caller didn't pass -mssql explicitly but a config exists.
func envHasMSSQL() bool {
	if os.Getenv("MSSQL_CONFIG_PATH") != "" {
		return true
	}
	for _, p := range []string{"../configs/mssql_config.json", "../../configs/mssql_config.json", "/etc/dispatch/mssql_config.json"} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}


// handleManualEntry accepts clerk-entered invoice data for messages where AI
// extraction failed or wasn't possible (handwritten invoices, scanned-image
// PDFs the OCR can't read, vendor PDFs with no PO). Saves an extraction row
// with model="manual:<user>" so the rest of the pipeline (recon, voucher
// sync, system-derived Done) treats it like any other extraction.
//
// Optional voucher_no field: if the clerk just posted in the ERP and knows the
// voucher number, we skip the 10-min lookup wait and flip Status:Done
// immediately. Without voucher_no, the row enters the queue as Status:New
// and the next voucher sync will find it.
func (s *server) handleManualEntry(w http.ResponseWriter, r *http.Request) {
	msgID, err := decodeRowID(chi.URLParam(r, "rowID"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	poStr := strings.TrimSpace(r.FormValue("po"))
	invoiceNo := strings.TrimSpace(r.FormValue("invoice"))
	totalStr := strings.TrimSpace(r.FormValue("total"))
	voucherNo := strings.TrimSpace(r.FormValue("voucher_no"))
	dateStr := strings.TrimSpace(r.FormValue("invoice_date"))

	if invoiceNo == "" && voucherNo == "" {
		http.Error(w, "either invoice number or voucher number is required", http.StatusBadRequest)
		return
	}
	var poNo int64
	if poStr != "" {
		n, perr := strconv.ParseInt(poStr, 10, 64)
		if perr != nil || n <= 0 {
			http.Error(w, "po must be a positive integer (or blank)", http.StatusBadRequest)
			return
		}
		poNo = n
	}
	var total float64
	if totalStr != "" {
		// Strip $ and commas before parsing; clerks often paste verbatim.
		clean := strings.NewReplacer("$", "", ",", "", " ", "").Replace(totalStr)
		t, terr := strconv.ParseFloat(clean, 64)
		if terr != nil || t < 0 {
			http.Error(w, "total must be a positive number (or blank)", http.StatusBadRequest)
			return
		}
		total = t
	}

	data := &cache.InvoiceData{
		PONumber:      poStr,
		InvoiceNumber: invoiceNo,
		InvoiceDate:   dateStr,
		InvoiceTotal:  total,
	}
	model := "manual:" + s.effectiveUser(r)
	storeCtx, storeCancel := context.WithTimeout(r.Context(), 5*time.Second)
	if err := s.cache.StoreInvoiceExtraction(storeCtx, s.mailbox, msgID, model, poNo, data, "", 0, false); err != nil {
		storeCancel()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	storeCancel()

	// If voucher_no provided, the clerk has already posted in the ERP — flip
	// pay_status + Status:Done so the row exits the queue immediately.
	if voucherNo != "" {
		info := cache.VoucherInfo{
			Status:        "posted",
			VoucherNo:     voucherNo,
			InvoiceAmount: total,
			PostedAt:      time.Now().UTC(),
		}
		vCtx, vCancel := context.WithTimeout(r.Context(), 3*time.Second)
		_ = s.cache.SetVoucherInfo(vCtx, s.mailbox, msgID, info)
		vCancel()
		// Push Status:Done back to Outlook.
		m, err := s.gc.GetMessage(s.mailbox, msgID)
		if err == nil {
			newCats := ui.ReplaceStatus(m.Categories, "Done")
			if err := s.gc.SetCategories(s.mailbox, msgID, newCats); err == nil {
				cCtx, cCancel := context.WithTimeout(r.Context(), 2*time.Second)
				_ = s.cache.UpdateCategories(cCtx, s.mailbox, msgID, newCats)
				cCancel()
			}
		}
	}

	// Re-render the detail pane so the clerk sees their entry land. HTMX
	// hx-target=#detail on the form swaps innerHTML.
	data2, err := s.buildDetailData(r.Context(), msgID, s.effectiveUser(r))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if err := s.tmpl.ExecuteTemplate(w, "detail.html", data2); err != nil {
		log.Printf("render detail (manual): %v", err)
	}
}

// askPreviewData drives the Ask Buyer / Ask Vendor preview panel. Pure
// preview — no email is sent. Clerk can copy the rendered text to clipboard
// and paste into Outlook themselves. (Once we wire actual sending we'll
// add a real "Send" button alongside the existing "Copy" button.)
type askPreviewData struct {
	Audience string // "buyer" or "vendor"
	To       string // resolved recipient email; empty when no email on file
	ToName   string // display name (e.g., "Sample Sender" or "Sample-Distributor LLC")
	Subject  string
	Body     string
	NoEmail  string // when To couldn't be resolved, this explains why
}

// handleAskPreview renders the templated "Ask <buyer|vendor>" message for a
// given message. PROTOTYPE — does not send. UI shows the rendered to/subject/
// body with a copy-to-clipboard button so the clerk can paste into Outlook.
func (s *server) handleAskPreview(w http.ResponseWriter, r *http.Request) {
	msgID, err := decodeRowID(chi.URLParam(r, "rowID"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	audience := chi.URLParam(r, "audience")
	if audience != "buyer" && audience != "vendor" {
		http.Error(w, "audience must be buyer or vendor", http.StatusBadRequest)
		return
	}

	// Pull what we need: vendor name, PO, invoice + recon for the
	// discrepancy summary, original sender for vendor email.
	m, err := s.gc.GetMessage(s.mailbox, msgID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	vm := ui.FromGraph(*m)
	extCtx, extCancel := context.WithTimeout(r.Context(), 2*time.Second)
	ext, _ := s.cache.GetInvoiceExtraction(extCtx, s.mailbox, msgID)
	extCancel()

	var rec *recon.Reconciliation
	if ext != nil && ext.ReconcileJSON != "" {
		var rr recon.Reconciliation
		if err := json.Unmarshal([]byte(ext.ReconcileJSON), &rr); err == nil {
			rec = &rr
		}
	}

	data := askPreviewData{Audience: audience}
	switch audience {
	case "buyer":
		if vm.Buyer == "" {
			data.NoEmail = "no buyer assigned to this PO"
		} else if s.erp == nil {
			data.NoEmail = "the ERP not configured — can't look up buyer email"
		} else {
			lookCtx, lookCancel := context.WithTimeout(r.Context(), 2*time.Second)
			email, _ := s.erp.LookupUserEmail(lookCtx, vm.Buyer)
			lookCancel()
			if email == "" {
				data.NoEmail = "no email on file for " + vm.Buyer + " in the ERP"
			} else {
				data.To = email
				data.ToName = vm.Buyer
			}
		}
	case "vendor":
		// Reply to the original sender. SenderName falls back to the email
		// address when display name is missing.
		if vm.Sender == "" {
			data.NoEmail = "no sender on this message"
		} else {
			data.To = vm.Sender
			if vm.SenderName != "" {
				data.ToName = vm.SenderName
			} else {
				data.ToName = vm.Vendor
			}
		}
	}

	data.Subject = buildAskSubject(audience, vm, ext)
	data.Body = buildAskBody(audience, vm, ext, rec, s.effectiveUser(r))

	if err := s.tmpl.ExecuteTemplate(w, "ask-preview.html", data); err != nil {
		log.Printf("render ask-preview: %v", err)
	}
}

// buildAskSubject crafts the Subject: line for the templated message.
// Differs slightly between buyer and vendor: buyers prefer PO# in subject,
// vendors prefer invoice#.
func buildAskSubject(audience string, vm ui.ViewMessage, ext *cache.InvoiceExtraction) string {
	po := ""
	if ext != nil && ext.PONo > 0 {
		po = fmt.Sprintf("%d", ext.PONo)
	}
	inv := ""
	if ext != nil && ext.Data != nil {
		inv = ext.Data.InvoiceNumber
	}
	switch audience {
	case "buyer":
		if po != "" {
			return "Question on PO " + po + " — " + vm.Vendor
		}
		return "Question on invoice from " + vm.Vendor
	case "vendor":
		if inv != "" {
			return "Question about invoice " + inv
		}
		if po != "" {
			return "Question about PO " + po
		}
		return "Question about your invoice"
	}
	return "Question"
}

// buildAskBody crafts the message body. Uses the reconciliation summary when
// available so the clerk doesn't have to re-explain the discrepancy.
func buildAskBody(audience string, vm ui.ViewMessage, ext *cache.InvoiceExtraction, rec *recon.Reconciliation, signer string) string {
	var b strings.Builder
	switch audience {
	case "buyer":
		b.WriteString("Hi " + capitalize(vm.Buyer) + ",\n\n")
		if ext != nil && ext.PONo > 0 {
			fmt.Fprintf(&b, "We received an invoice from %s on PO %d", vm.Vendor, ext.PONo)
			if ext.Data != nil && ext.Data.InvoiceNumber != "" {
				fmt.Fprintf(&b, " (invoice %s)", ext.Data.InvoiceNumber)
			}
			b.WriteString(" and need your guidance before posting.\n\n")
		} else {
			fmt.Fprintf(&b, "We received an invoice from %s and need your guidance before posting.\n\n", vm.Vendor)
		}
		if rec != nil {
			b.WriteString(summarizeReconForAsk(rec))
			b.WriteString("\n\n")
		}
		b.WriteString("Can you confirm how to proceed?\n\nThanks,\n")
	case "vendor":
		vendorName := vm.Vendor
		if vendorName == "" {
			vendorName = "team"
		}
		b.WriteString("Hi " + vendorName + " AP team,\n\n")
		if ext != nil && ext.Data != nil && ext.Data.InvoiceNumber != "" {
			fmt.Fprintf(&b, "Following up on invoice %s", ext.Data.InvoiceNumber)
			if ext.PONo > 0 {
				fmt.Fprintf(&b, " (our PO %d)", ext.PONo)
			}
			b.WriteString(":\n\n")
		} else if ext != nil && ext.PONo > 0 {
			fmt.Fprintf(&b, "Following up on the invoice for PO %d:\n\n", ext.PONo)
		} else {
			b.WriteString("Following up on a recent invoice:\n\n")
		}
		if rec != nil {
			b.WriteString(summarizeReconForAsk(rec))
			b.WriteString("\n\n")
		}
		b.WriteString("Could you review and confirm? Happy to send a copy of the PO if helpful.\n\nThanks,\n")
	}
	if signer != "" {
		b.WriteString(capitalize(signer))
		b.WriteString("\nAcme Distribution AP\n")
	}
	return b.String()
}

// summarizeReconForAsk produces the natural-language discrepancy summary that
// goes inside both an Ask-Buyer body AND the Why-Blocked banner (item #5).
// One canonical synth function, two consumers.
func summarizeReconForAsk(r *recon.Reconciliation) string {
	if r == nil {
		return ""
	}
	switch {
	case r.FeeOnlyDiscrepancy:
		return fmt.Sprintf("Every PO line matches; the invoice has %d fee line%s not on the PO (likely shipping/tax). Total invoice $%.2f vs PO $%.2f (diff $%.2f).",
			r.FeeLines, plural(r.FeeLines), r.InvoiceTotal, r.POTotal, r.TotalDiff)
	case r.LineMismatches > 0 && r.ExtraInvoice > 0:
		return fmt.Sprintf("%d line%s mismatch the PO and %d invoice line%s have no PO counterpart. Total invoice $%.2f vs PO $%.2f (diff $%.2f).",
			r.LineMismatches, plural(r.LineMismatches), r.ExtraInvoice, plural(r.ExtraInvoice),
			r.InvoiceTotal, r.POTotal, r.TotalDiff)
	case r.LineMismatches > 0:
		return fmt.Sprintf("%d line%s on the invoice don't match the PO. Total invoice $%.2f vs PO $%.2f (diff $%.2f).",
			r.LineMismatches, plural(r.LineMismatches), r.InvoiceTotal, r.POTotal, r.TotalDiff)
	case r.MissingFromInv > 0 && r.ExtraInvoice == 0 && r.LineMismatches == 0:
		return fmt.Sprintf("%d PO line%s aren't billed on this invoice (looks like a partial shipment). Invoice $%.2f vs PO $%.2f.",
			r.MissingFromInv, plural(r.MissingFromInv), r.InvoiceTotal, r.POTotal)
	case !r.TotalMatch:
		return fmt.Sprintf("Lines reconcile but totals differ: invoice $%.2f vs PO $%.2f (diff $%.2f).",
			r.InvoiceTotal, r.POTotal, r.TotalDiff)
	}
	return fmt.Sprintf("Invoice and PO reconcile: $%.2f.", r.InvoiceTotal)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func capitalize(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// handleAddNote appends a clerk note to a message. Append-only — no edit or
// delete path, by design. Renders the updated detail pane on success so the
// new note shows up in the log without a full page refresh.
func (s *server) handleAddNote(w http.ResponseWriter, r *http.Request) {
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
		http.Error(w, "note body is required", http.StatusBadRequest)
		return
	}
	user := s.effectiveUser(r)
	addCtx, addCancel := context.WithTimeout(r.Context(), 3*time.Second)
	if _, err := s.cache.AddInvoiceNote(addCtx, s.mailbox, msgID, user, body); err != nil {
		addCancel()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	addCancel()
	// Re-render the detail pane so the new note appears in the log.
	data, err := s.buildDetailData(r.Context(), msgID, user)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if err := s.tmpl.ExecuteTemplate(w, "detail.html", data); err != nil {
		log.Printf("render detail (note add): %v", err)
	}
}

// handleSetRotation persists a rotation angle for the inline PDF preview.
// Called by JS when a clerk hits the rotate button. Returns 204 — the JS
// has already applied the rotation client-side; this is just persistence so
// the next viewer sees the corrected orientation. Angle is normalized into
// {0, 90, 180, 270} server-side.
func (s *server) handleSetRotation(w http.ResponseWriter, r *http.Request) {
	msgID, err := decodeRowID(chi.URLParam(r, "rowID"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	attID := chi.URLParam(r, "attID")
	if attID == "" {
		http.Error(w, "missing attachment id", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	angle, _ := strconv.Atoi(r.FormValue("angle"))
	saveCtx, saveCancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer saveCancel()
	if err := s.cache.SetAttachmentRotation(saveCtx, s.mailbox, msgID, attID, angle, s.user); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleClearKind strips the Kind: category from a message. Used when a clerk
// corrects an AI misclassification — e.g. "actually this Sample-Electric
// invoice isn't Marketing." Removing the Kind returns the row to the main
// queue (HiddenByDefault no longer fires). Same swap pattern as the other
// row-mutation handlers.
func (s *server) handleClearKind(w http.ResponseWriter, r *http.Request) {
	msgID, err := decodeRowID(chi.URLParam(r, "rowID"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	m, err := s.gc.GetMessage(s.mailbox, msgID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	newCats := ui.StripKind(m.Categories)
	if err := s.gc.SetCategories(s.mailbox, msgID, newCats); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	_ = s.cache.UpdateCategories(r.Context(), s.mailbox, msgID, newCats)
	s.renderRow(w, msgID, s.effectiveUser(r))
}

func hasStatusDoneCategory(cats []string) bool {
	for _, c := range cats {
		if c == "Status: Done" {
			return true
		}
	}
	return false
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
