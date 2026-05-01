// dispatch-worker polls a mailbox, resolves vendor identity on external vendor
// mail, and writes Outlook Categories back via Graph PATCH.
//
// Tag policy (MVP):
//   - Sender class Vendor, match succeeded → set "Vendor: <Name>" + "Status: New"
//   - Sender class Vendor, match failed   → set "Vendor: Unknown" + "Status: New"
//   - Sender class Internal/Relay/Logistics/Bank → do nothing (leave untagged)
//
// Idempotency: a message is considered already processed if its categories list
// contains any entry starting with "Status:". On re-runs we skip those so
// clerk-set statuses (In Progress, Blocked, Done) are never overwritten.
//
// Usage:
//   dispatch-worker [-mailbox EMAIL] [-limit N] [-dry-run]
//
// --dry-run prints intended PATCHes without sending them. Safe to run repeatedly.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"dispatch/internal/aiclass"
	"dispatch/internal/blobstore"
	"dispatch/internal/cache"
	"dispatch/internal/graph"
	"dispatch/internal/p21"
	"dispatch/internal/pdftext"
	"dispatch/internal/poscan"
	"dispatch/internal/vendors"
)

// aiVisionModel overrides the model used specifically for invoice extraction.
// Benchmarks showed gemma4:26b meaningfully more accurate than e4b on PO
// numbers while only 2× slower on CPU. the author approved the tradeoff.
// aiVisionModel is the primary (fast) vision model. Must fit on ai-02's
// 8 GB VRAM pure-GPU for the speed win to hold. If extraction fails or comes
// back empty, the worker escalates to aiFallbackVisionURL + aiFallbackVisionModel
// (currently the slower-but-more-reliable gemma4:26b on ai-03's CPU).
const (
	aiVisionModel             = "minicpm-v:latest"
	// Fallback pool (10 concurrent CPU slots for gemma4:26b):
	//   ai-03: 4 containers × 10 cores / 40 GB
	//   mjolnir: 2 containers × 6 cores / 50 GB
	//   ai-04: 4 containers × 4 cores / 40 GB (shrunk from 8 — running 8
	//     gemma4 + Paddle oversubscribed the 32-core box by 4 cores and
	//     caused 7.5m timeouts. Our fallback-concurrency=3 only needs ~3
	//     simultaneous, so 4 endpoints on ai-04 is plenty.)
	// Worker's N-goroutine pool round-robins across all ten so any
	// escalation bottleneck comes from GPU primary capacity, not CPU fallback.
	// ai-03 (<gpu-host-2>) is now Tesla P40 GPU as of 2026-04-27 — single
	// native Ollama instance with OLLAMA_NUM_PARALLEL=4 handling its own
	// concurrency. Drops the previous CPU-endpoint round-robin (mjolnir +
	// ai-04, 6 total): a GPU-fast endpoint diluted by CPU-slow ones makes
	// the average case worse, not better. Mjolnir/ai-04 still reachable
	// if a deeper fallback is wanted later — re-add as comma-separated.
	aiFallbackVisionURL = "http://<gpu-host-2>:11434"
	aiFallbackVisionModel     = "gemma4:26b"
)

// attachmentBlockedMailboxes is a deny-list for mailboxes where we must
// never fetch attachment bytes. Empty today (the legacy catchall mailbox
// is no longer in play). If Dispatch is ever pointed at a new mailbox
// with untrusted content, add it here BEFORE pointing the worker at it.
var attachmentBlockedMailboxes = map[string]bool{}

// maxAttachmentsPerMessage caps how many PDFs we scan on a single message.
// Invoices rarely have more than 1-2 PDFs; a large batch would be wasteful.
const maxAttachmentsPerMessage = 3

const (
	statusNewCategory     = "Status: New"
	statusBlockedCategory = "Status: Blocked"
	vendorCatPrefix       = "Vendor: "
	statusCatPrefix       = "Status: "
	buyerCatPrefix        = "Buyer: "
	kindCatPrefix         = "Kind: "
	blockerCatPrefix      = "Blocker: "
	unknownVendorName     = "Unknown"
)

func main() {
	mailbox := flag.String("mailbox", "ap@example.com", "mailbox to process")
	limit := flag.Int("limit", 500, "max messages to scan per run")
	emailsPath := flag.String("emails", "data/vendor_emails.json", "path to vendor_emails.json")
	domainsPath := flag.String("domains", "data/vendor_domains.json", "path to vendor_domains.json")
	mssqlConfig := flag.String("mssql", "", "path to mssql_config.json (default: search standard locations)")
	aiURL := flag.String("ai-url", aiclass.DefaultURL, "Ollama URL for text classification (single host; model must be present)")
	aiModel := flag.String("ai-model", aiclass.DefaultModel, "Ollama model for text classification")
	visionURL := flag.String("ai-vision-url", "", "primary vision Ollama URL(s); comma-sep for round-robin pool; defaults to -ai-url if empty")
	visionModel := flag.String("ai-vision-model", aiVisionModel, "primary vision model (must be pulled on every -ai-vision-url host)")
	fallbackVisionURL := flag.String("ai-fallback-vision-url", aiFallbackVisionURL, "fallback Ollama URL for verify escalation (empty disables fallback)")
	fallbackVisionModel := flag.String("ai-fallback-vision-model", aiFallbackVisionModel, "fallback vision model used when primary errors or finds nothing")
	paddleURL := flag.String("ai-paddle-url", "", "PaddleOCR-VL URL (empty disables — current default). Paddle on CPU proved too slow (>7 min per call on shared-host ai-04); re-enable when we have dedicated GPU or a lightly-loaded CPU box.")
	paddleModel := flag.String("ai-paddle-model", "PaddlePaddle/PaddleOCR-VL", "Paddle model name (must match what the container advertises)")
	blobDir := flag.String("blob-dir", "/mnt/ap-synology/dispatch", "local blobstore for attachment reads. Empty = always fetch from Graph. When set, worker reads PDFs from disk instead of re-pulling from Graph on every extraction pass.")
	cachePath := flag.String("cache", "", "SQLite cache path (default: ~/.dispatch/cache.db)")
	dryRun := flag.Bool("dry-run", false, "preview PATCHes without sending them")
	concurrency := flag.Int("concurrency", 2, "SORT pool size — goroutines that classify, resolve vendor, discover POs, and tag Outlook categories. I/O-bound; fast. Never runs invoice extraction (that's the extract pool). Bump higher to tag fresh mail faster.")
	extractConcurrency := flag.Int("extract-concurrency", 4, "EXTRACT pool size — goroutines that drain queued extraction jobs (run verify-against-PO or open-ended extraction). CPU/GPU bound. When the sort pool enqueues faster than this can drain, the extra jobs get a stub row that the rescan pass will pick up later.")
	fallbackConcurrency := flag.Int("fallback-concurrency", 3, "goroutines draining the async fallback queue. Each holds one inference in flight against the single ai-03 GPU endpoint. 3 leaves headroom under OLLAMA_NUM_PARALLEL=4 so a burst doesn't queue at the model.")
	loopSeconds := flag.Int("loop-seconds", 0, "if >0, re-run the main+rescan pass every N seconds until the queue is empty, then exit. Fallback pool stays alive across iterations (closed only on final exit). 0 = one-shot.")
	flag.Parse()

	resolver, err := vendors.Load(*emailsPath, *domainsPath)
	if err != nil {
		log.Fatalf("load resolver: %v", err)
	}
	gc, err := graph.NewClient()
	if err != nil {
		log.Fatalf("graph client: %v", err)
	}
	p21c, err := p21.New(*mssqlConfig)
	if err != nil {
		log.Fatalf("p21 client: %v", err)
	}
	defer p21c.Close()

	var aiClient *aiclass.Client
	var visionClient *aiclass.Client
	if *aiURL != "" {
		aiClient = aiclass.NewClient(*aiURL, *aiModel)
		pingCtx, pingCancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := aiClient.Ping(pingCtx); err != nil {
			log.Printf("ai classifier disabled: %v", err)
			aiClient = nil
		} else {
			// Separate client for vision: defaults to -ai-url, but can be a
			// comma-separated list so multiple GPU boxes round-robin behind
			// the same model.
			vu := *visionURL
			if vu == "" {
				vu = *aiURL
			}
			visionClient = aiclass.NewClient(vu, *visionModel)
			log.Printf("primary vision: %s @ %s", *visionModel, vu)
		}
		pingCancel()
	}

	// Fallback vision client: hit when the fast primary errors or comes back
	// empty. Runs on the slower-but-more-reliable CPU server. Ping at startup
	// so we don't hold the worker if it's unreachable.
	var fallbackVisionClient *aiclass.Client
	if *fallbackVisionURL != "" && visionClient != nil {
		tryFallback := aiclass.NewClient(*fallbackVisionURL, *fallbackVisionModel)
		pingCtx, pingCancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := tryFallback.Ping(pingCtx); err != nil {
			log.Printf("fallback vision disabled: %v", err)
		} else {
			fallbackVisionClient = tryFallback
			log.Printf("fallback vision: %s @ %s", *fallbackVisionModel, *fallbackVisionURL)
		}
		pingCancel()
	}

	// Local blobstore: Phase 1 mirror. Read attachment bytes from disk
	// when available; falls back to Graph (and caches on the way back)
	// when a blob hasn't been downloaded yet. Optional — nil if the
	// share isn't mounted or -blob-dir is empty.
	var blobStore *blobstore.Store
	if *blobDir != "" {
		bs, berr := blobstore.New(*blobDir)
		if berr != nil {
			log.Printf("blobstore disabled: %v", berr)
		} else {
			blobStore = bs
			log.Printf("blobstore enabled at %s", bs.Root())
		}
	}

	// Paddle OCR client: tier-2 of the extraction cascade. Used when the
	// PDF has no text layer (scanned image). Paddle transcribes the image
	// to text, then the worker feeds that text back into the same
	// text-based extraction methods that pdftotext output uses. Optional —
	// if ping fails we fall straight from pdftotext to vision as before.
	var paddleClient *aiclass.Client
	if *paddleURL != "" {
		tryPaddle := aiclass.NewClient(*paddleURL, *paddleModel)
		// Longer ping timeout than other endpoints because Paddle runs on a
		// shared CPU host — when ai-04 is momentarily saturated a 5s ping
		// fails and we lose Paddle for the whole run. 30s tolerates transient
		// load; real per-request timeouts (5 min) still bound actual work.
		pingCtx, pingCancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := tryPaddle.Ping(pingCtx); err != nil {
			log.Printf("paddle OCR disabled: %v", err)
		} else {
			paddleClient = tryPaddle
			log.Printf("paddle OCR: %s @ %s", *paddleModel, *paddleURL)
		}
		pingCancel()
	}

	cacheDB, err := cache.Open(*cachePath)
	if err != nil {
		log.Fatalf("cache: %v", err)
	}
	defer cacheDB.Close()

	// Error logs split by subsystem so we can tail one or the other and
	// see only what we care about. Both also go to stdout (the main log)
	// so nothing's lost. Best-effort: if the file open fails, we just
	// don't tee.
	workerErrFile, _ := os.OpenFile("/tmp/dispatch-worker-errors.log",
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	endpointErrFile, _ := os.OpenFile("/tmp/dispatch-endpoint-errors.log",
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	extractionErrFile, _ := os.OpenFile("/tmp/dispatch-extraction-errors.log",
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	errLog = &errorLoggers{
		worker:     workerErrFile,
		endpoint:   endpointErrFile,
		extraction: extractionErrFile,
	}
	defer errLog.close()

	// Wire endpoint-activity tracking. Hook fires on every vision request so
	// the web UI can show current-request elapsed + historical totals per URL.
	// errLog tees per-request errors to /tmp/dispatch-endpoint-errors.log.
	eh := &endpointHook{cache: cacheDB, errLog: errLog}
	if visionClient != nil {
		visionClient.SetEndpointHook(eh)
	}
	if fallbackVisionClient != nil {
		fallbackVisionClient.SetEndpointHook(eh)
	}
	if aiClient != nil {
		aiClient.SetEndpointHook(eh)
	}
	if paddleClient != nil {
		paddleClient.SetEndpointHook(eh)
	}

	mode := "LIVE"
	if *dryRun {
		mode = "DRY RUN"
	}
	loopMode := *loopSeconds > 0
	if loopMode {
		fmt.Printf("dispatch-worker [%s loop=%ds]  mailbox=%s  limit=%d\n\n", mode, *loopSeconds, *mailbox, *limit)
	} else {
		fmt.Printf("dispatch-worker [%s]  mailbox=%s  limit=%d\n\n", mode, *mailbox, *limit)
	}

	if err := cacheDB.StartRun(context.Background(), *mailbox, []cache.PoolSpec{
		{Pool: "sort", Size: *concurrency},
		{Pool: "extract", Size: *extractConcurrency},
		{Pool: "fallback", Size: *fallbackConcurrency},
	}); err != nil {
		log.Printf("worker heartbeat: start run: %v", err)
	}

	var counters runCounters

	// Fallback pool: N goroutines draining fallbackJobs concurrently with the
	// extract pool. Buffer sized generously so extract never blocks queuing.
	var fallbackJobsCh chan fallbackJob
	var fallbackWG sync.WaitGroup
	if fallbackVisionClient != nil && *fallbackConcurrency > 0 {
		fallbackJobsCh = make(chan fallbackJob, *fallbackConcurrency*8)
		fmt.Printf("Async fallback pool: %d workers draining into %d endpoints.\n",
			*fallbackConcurrency, len(strings.Split(*fallbackVisionURL, ",")))
		for i := 0; i < *fallbackConcurrency; i++ {
			fallbackWG.Add(1)
			go func(idx int) {
				defer fallbackWG.Done()
				for job := range fallbackJobsCh {
					processFallbackJob(cacheDB, gc, fallbackVisionClient, job)
				}
			}(i)
		}
	}

	// Extract pool: M goroutines draining extractJobs. Receives jobs from
	// (a) the sort pool when a message has a resolved PO, and (b) the rescan
	// pass. This pool owns the slow path (verify-against-PO, text/paddle/
	// vision cascade, optional fallback escalation). Buffer sized so the
	// sort pool can queue ahead without blocking; when buffer fills, the
	// sort worker writes a stub-rescan row instead of blocking.
	var extractJobsCh chan extractJob
	var extractWG sync.WaitGroup
	deps := &processorDeps{} // fwd-declared so extract workers can close over it
	if *extractConcurrency > 0 {
		// Buffer = workers × 64 (was ×8). At ×8 a full-mailbox reprocess
		// (1000+ messages with PO+attachment) overflows in seconds and the
		// sort pool starts dropping stub-rescan rows. Worse, the rescan
		// loop pushes to the SAME channel, so when the buffer is full,
		// rescan also can't drain — stubs pile up. ×64 gives roughly 250
		// slots, enough headroom for the entire mailbox burst on a fresh
		// reprocess. Memory cost: extractJob is small (a *graph.Message
		// pointer + a few ints); 250 of them ≈ <1MB.
		extractJobsCh = make(chan extractJob, *extractConcurrency*64)
		fmt.Printf("Extract pool: %d workers (buffer=%d).\n",
			*extractConcurrency, cap(extractJobsCh))
		for i := 0; i < *extractConcurrency; i++ {
			extractWG.Add(1)
			go func(slot int) {
				defer extractWG.Done()
				for job := range extractJobsCh {
					_ = cacheDB.SetSlotCurrent(context.Background(), "extract", slot, *mailbox, job.m.ID, "extract", job.m.Subject, "")
					extractInvoice(cacheDB, gc, visionClient, fallbackVisionClient, paddleClient,
						fallbackJobsCh, p21c, blobStore, *mailbox, job.m, job.poNo, job.currentCats, job.minTier, slot)
					_ = cacheDB.MarkSlotCompleted(context.Background(), "extract", slot, job.m.ID)
				}
				_ = cacheDB.MarkSlotIdle(context.Background(), "extract", slot)
			}(i)
		}
	}

	*deps = processorDeps{
		mailbox:              *mailbox,
		dryRun:               *dryRun,
		resolver:             resolver,
		gc:                   gc,
		p21c:                 p21c,
		aiClient:             aiClient,
		visionClient:         visionClient,
		fallbackVisionClient: fallbackVisionClient,
		paddleClient:         paddleClient,
		cacheDB:              cacheDB,
		fallbackJobs:         fallbackJobsCh,
		extractJobs:          extractJobsCh,
		blobStore:            blobStore,
	}

	// Rescan constants referenced by both the main loop and the loop
	// terminator (to read queue depth for "empty → exit" check).
	const rescanBatchSize = 10000
	const rescanMaxAttempts = 4 // 4 tiers, 4 allowed attempts (one per tier)

	// Orphan sweep: ListRescanQueue filters rescan_attempts >= cap but
	// MarkRescanExhausted only fires when a row is actually picked up,
	// so rows that raced past the cap (channel-duplicate bug — same row
	// enqueued many times before first store lands) sit at needs_rescan=1
	// forever. One-shot cleanup on start; the cooldown mechanism keeps
	// new ones from being created.
	sweepCtx, sweepCancel := context.WithTimeout(context.Background(), 5*time.Second)
	if n, err := cacheDB.SweepOrphanedRescans(sweepCtx, *mailbox, rescanMaxAttempts); err != nil {
		log.Printf("orphan-rescan sweep: %v", err)
	} else if n > 0 {
		log.Printf("orphan-rescan sweep: cleared needs_rescan on %d past-cap rows", n)
	}
	sweepCancel()

	// Pass loop: in one-shot mode this runs exactly once. In loop mode it
	// re-polls Graph for new messages + drains any remaining rescans every
	// *loopSeconds until the queue is empty. Fallback pool stays alive
	// across iterations — it's closed at the very end below.
	for iter := 0; ; iter++ {
		if loopMode && iter > 0 {
			fmt.Printf("\n=== Pass %d starting ===\n", iter+1)
		}

		msgs, err := gc.ListInboxMessages(*mailbox, *limit)
		if err != nil {
			log.Printf("list messages: %v (ending loop)", err)
			break
		}
		fmt.Printf("Fetched %d messages.\n\n", len(msgs))

		// Concurrent worker pool: N goroutines pull messages off a channel and run
		// the full per-message pipeline in parallel. Fallback is async, so main
		// workers never block on the slow CPU path.
		n := *concurrency
		if n < 1 {
			n = 1
		}
		if n > len(msgs) {
			n = len(msgs)
		}
		if n == 0 {
			n = 1 // avoid panic on empty msgs
		}
		if iter == 0 {
			fmt.Printf("Processing with %d parallel workers.\n\n", n)
		}

		if len(msgs) > 0 {
			jobs := make(chan graph.Message, len(msgs))
			var wg sync.WaitGroup
			for slot := 0; slot < n; slot++ {
				wg.Add(1)
				go func(slot int) {
					defer wg.Done()
					lg := &slotLogger{slot: slot, prefix: fmt.Sprintf("[w%d] ", slot)}
					for m := range jobs {
						processMessage(lg, m, deps, &counters)
					}
					// Clear this slot's heartbeat once its jobs channel drains.
					_ = cacheDB.MarkSlotIdle(context.Background(), "sort", slot)
				}(slot)
			}
			for _, m := range msgs {
				jobs <- m
			}
			close(jobs)
			wg.Wait()
		}

		// Rescan pass happens inline so each loop iteration processes:
		// new messages from Graph + any stragglers still flagged for rescan.
		runRescanPass(cacheDB, gc, deps, visionClient, rescanBatchSize, rescanMaxAttempts, *mailbox, *dryRun)

		// Loop terminator: exit when there's genuinely nothing to do. The
		// real backlog is "messages the sort pool hasn't tagged yet"
		// (PendingInboxCount) + "messages flagged for rescan still under
		// the cap" (RescanQueueDepth). UnextractedCount is a much looser
		// signal (counts non-invoice mail too) — printed for observability
		// but doesn't gate the loop.
		if !loopMode {
			break
		}
		depthCtx, depthCancel := context.WithTimeout(context.Background(), 5*time.Second)
		rescanDepth, _ := cacheDB.RescanQueueDepth(depthCtx, *mailbox, rescanMaxAttempts)
		pending, _ := cacheDB.PendingInboxCount(depthCtx, *mailbox)
		unextracted, _ := cacheDB.UnextractedCount(depthCtx, *mailbox)
		depthCancel()
		if rescanDepth == 0 && pending == 0 {
			fmt.Printf("\nloop: queue empty (0 needs-first-pass, 0 rescans), exiting\n")
			break
		}
		fmt.Printf("\nloop: backlog=%d needs-first-pass + %d rescans (total unextracted=%d), sleeping %ds...\n",
			pending, rescanDepth, unextracted, *loopSeconds)
		time.Sleep(time.Duration(*loopSeconds) * time.Second)
	}

	// Shims: the rescan pass and final summary still reference plain ints.
	// Pull current counter values into locals; rescan-pass mutations continue
	// to update these (sequential, so no race).
	tagged := int(counters.tagged.Load())
	taggedUnknown := int(counters.taggedUnknown.Load())
	poOverrides := int(counters.poOverrides.Load())
	taggedInternal := int(counters.taggedInternal.Load())
	skippedTagged := int(counters.skippedTagged.Load())
	skippedNonVendor := int(counters.skippedNonVendor.Load())
	skippedInternalNoPO := int(counters.skippedInternalNoPO.Load())
	skippedEmpty := int(counters.skippedEmpty.Load())
	errs := int(counters.errs.Load())
	_, _, _, _, _, _, _, _ = taggedUnknown, poOverrides, taggedInternal, skippedTagged, skippedNonVendor, skippedInternalNoPO, skippedEmpty, errs
	_ = tagged // referenced by summary

	// Rescan pass runs inline each loop iteration — see runRescanPass.
	// Drain the extract pool first: sort + rescan producers are already
	// done (loop exited), so closing extractJobsCh lets remaining jobs
	// finish then the pool exits. After extract drains, we can drain the
	// fallback pool (which may have escalations queued by the extract pool).
	if extractJobsCh != nil {
		// Sample the queue depth BEFORE close — once closed, the workers
		// drain the buffer concurrently and len() races with their reads.
		pending := len(extractJobsCh)
		close(extractJobsCh)
		if pending > 0 {
			fmt.Printf("\n=== Extract drain: waiting on %d queued jobs ===\n", pending)
		}
		extractWG.Wait()
	}
	// Drain the fallback pool: close the channel and wait for workers to
	// finish whatever they were running. Without this we'd exit with vouchers
	// still computing — results would be lost.
	if fallbackJobsCh != nil {
		// Sample queue depth BEFORE close (same race fix as the extract pool
		// drain above) — once closed, workers drain the buffer concurrently
		// and len() races with their reads.
		pending := len(fallbackJobsCh)
		close(fallbackJobsCh)
		if pending > 0 {
			fmt.Printf("\n=== Fallback drain: waiting on %d queued jobs ===\n", pending)
		}
		fallbackWG.Wait()
	}

	// Final: clear all slot rows so the UI flips to idle. Covers all three
	// pools; no harm if a pool had zero workers (update matches nothing).
	for slot := 0; slot < *concurrency; slot++ {
		_ = cacheDB.MarkSlotIdle(context.Background(), "sort", slot)
	}
	for slot := 0; slot < *extractConcurrency; slot++ {
		_ = cacheDB.MarkSlotIdle(context.Background(), "extract", slot)
	}
	for slot := 0; slot < *fallbackConcurrency; slot++ {
		_ = cacheDB.MarkSlotIdle(context.Background(), "fallback", slot)
	}

	fmt.Printf("\n=== Summary (%s) ===\n", mode)
	fmt.Printf("  Tagged with known vendor:     %d  (of which internal w/ PO: %d)\n", tagged, taggedInternal)
	fmt.Printf("  Tagged as Vendor: Unknown:    %d\n", taggedUnknown)
	fmt.Printf("    of which via PO-number override: %d\n", poOverrides)
	fmt.Printf("  Skipped (already has Status): %d\n", skippedTagged)
	fmt.Printf("  Skipped (relay/logistics/bank): %d\n", skippedNonVendor)
	fmt.Printf("  Skipped (internal, no PO found): %d\n", skippedInternalNoPO)
	fmt.Printf("  Skipped (empty sender):       %d\n", skippedEmpty)
	fmt.Printf("  Errors:                       %d\n", errs)
}

// runRescanPass enqueues rescan items into the extract pool. The extract
// pool does the heavy work; rescanPass just determines which tier to
// start from (based on the cached model) and either marks top-tier items
// exhausted or pushes a job with the appropriate minTier. Because extract
// pool runs concurrently, this function completes quickly — it's just a
// producer.
func runRescanPass(cacheDB *cache.Cache, gc *graph.Client, deps *processorDeps, visionClient *aiclass.Client, batchSize, maxAttempts int, mailbox string, dryRun bool) {
	if dryRun || visionClient == nil || deps.extractJobs == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	rescanIDs, err := cacheDB.ListRescanQueue(ctx, mailbox, maxAttempts, batchSize)
	cancel()
	if err != nil {
		fmt.Printf("rescan queue read failed: %v\n", err)
		return
	}
	if len(rescanIDs) == 0 {
		return
	}
	fmt.Printf("\n=== Rescan pass: enqueueing %d flagged messages into extract pool ===\n", len(rescanIDs))

	queued := 0
	exhausted := 0
	dropped := 0
	for _, id := range rescanIDs {
		exCtx, exCancel := context.WithTimeout(context.Background(), 3*time.Second)
		ext, _ := cacheDB.GetInvoiceExtraction(exCtx, mailbox, id)
		exCancel()
		if ext == nil || ext.PONo == 0 {
			continue
		}
		lastTier := extractionTier(ext.Model)
		nextMinTier := lastTier + 1
		if nextMinTier > topExtractionTier {
			clrCtx, clrCancel := context.WithTimeout(context.Background(), 3*time.Second)
			_ = cacheDB.MarkRescanExhausted(clrCtx, mailbox, id)
			clrCancel()
			exhausted++
			continue
		}
		msg, err := gc.GetMessage(mailbox, id)
		if err != nil {
			fmt.Printf("  rescan fetch %s: %v\n", truncate(id, 30), err)
			continue
		}
		// Block (with a short timeout) instead of `default: dropped`. Sort
		// and rescan share the same channel; in a fresh-reprocess burst,
		// `default` means rescan stubs accumulate forever because sort
		// keeps the buffer full. A 5-second blocking send punches through
		// most bursts (an extract worker frees a slot within a second or
		// two of an AI call returning); pathological cases still fall
		// through to "dropped" so the rescan pass doesn't hang.
		jobCtx, jobCancel := context.WithTimeout(context.Background(), 5*time.Second)
		select {
		case deps.extractJobs <- extractJob{m: *msg, poNo: ext.PONo, currentCats: msg.Categories, minTier: nextMinTier}:
			queued++
		case <-jobCtx.Done():
			dropped++
		}
		jobCancel()
	}
	fmt.Printf("=== Rescan enqueue done: %d queued, %d exhausted, %d dropped (channel full) ===\n", queued, exhausted, dropped)
}

// endpointHook implements aiclass.EndpointHook by writing to the SQLite cache.
// Called synchronously per request — SQLite WAL mode handles concurrent writes
// from multiple goroutines without blocking the HTTP call.
type endpointHook struct {
	cache  *cache.Cache
	errLog *errorLoggers
}

func (h *endpointHook) OnRequestStart(url, messageID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = h.cache.EndpointRequestStart(ctx, url, messageID)
}

func (h *endpointHook) OnRequestEnd(url string, dur time.Duration, reqErr error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = h.cache.EndpointRequestEnd(ctx, url, dur, reqErr)
	if reqErr != nil && h.errLog != nil {
		h.errLog.logEndpointErr(url, "after %s: %v", dur.Round(time.Millisecond), reqErr)
	}
}

// runCounters holds the per-run totals. Each field is atomic so concurrent
// workers can increment without a mutex. The summary prints are sequential
// (main finishes the pool join first), so no atomic read-then-print issue.
type runCounters struct {
	tagged              atomic.Int64
	taggedUnknown       atomic.Int64
	poOverrides         atomic.Int64
	taggedInternal      atomic.Int64
	skippedTagged       atomic.Int64
	skippedNonVendor    atomic.Int64
	skippedInternalNoPO atomic.Int64
	skippedEmpty        atomic.Int64
	errs                atomic.Int64
}

// processorDeps bundles everything a worker goroutine needs to process a
// message. Keeps processMessage's signature sane.
type processorDeps struct {
	mailbox              string
	dryRun               bool
	resolver             *vendors.Resolver
	gc                   *graph.Client
	p21c                 *p21.Client
	aiClient             *aiclass.Client
	visionClient         *aiclass.Client
	fallbackVisionClient *aiclass.Client
	paddleClient         *aiclass.Client
	cacheDB              *cache.Cache
	// blobStore is the local filesystem cache of attachment bytes. Nil =
	// disabled, always fetch from Graph. When set, the worker checks the
	// cache.attachments table for a known blob_sha + reads from disk; only
	// missing attachments fall back to Graph (and get cached on the way).
	blobStore *blobstore.Store
	// Async escalation: when primary verify needs the slow CPU fallback, we
	// push a job onto this channel instead of blocking the main goroutine.
	// Nil if fallback is disabled.
	fallbackJobs chan fallbackJob
	// Sort-to-extract handoff: sort pool pushes jobs here; extract pool
	// drains. Nil if extract pool disabled (extractConcurrency=0) — in
	// that case sort workers run extractInvoice inline (legacy behavior).
	extractJobs chan extractJob
}

// extractJob is the payload pushed from sort-pool to extract-pool. Carries
// the bare minimum the extract worker needs — it'll fetch attachments
// fresh from Graph, same as the old inline path did. minTier encodes the
// tier-escalation for rescans (0 = fresh, >0 = skip tiers at or below
// lastTier).
type extractJob struct {
	m           graph.Message
	poNo        int64
	currentCats []string
	minTier     int
}

// fallbackJob is the payload for an async fallback verify. Carries every
// piece of context the fallback worker needs (we don't want it re-fetching
// the attachment or re-rendering the PDF). primaryResult is kept so that if
// the fallback itself fails, we can still cache the (bad but parseable)
// primary verdict so the clerk sees *something*. pdfSha keys the cooldown
// table so we can stamp the (content, tier-4) pair after a fallback run.
type fallbackJob struct {
	mailbox       string
	m             graph.Message
	poNo          int64
	pdfName       string
	pdfSha        string
	png           []byte
	poLines       []p21.POLine
	currentCats   []string
	primaryResult *aiclass.VerifyResult
	primaryModel  string
	start         time.Time
	reason        string
}

// slotLogger wraps fmt.Printf with a per-goroutine prefix so interleaved
// output in concurrent runs stays readable. Not synchronized — fmt is
// line-atomic in practice for the short lines we emit. Slot is the numeric
// index used for heartbeat writes.
type slotLogger struct {
	slot   int
	prefix string
}

func (l *slotLogger) printf(format string, a ...any) {
	fmt.Print(l.prefix)
	fmt.Printf(format, a...)
}

// processMessage runs the full per-message pipeline: classification, PO
// resolution, category tagging, and invoice extraction. Extracted from the
// old inline for-range loop so a pool of goroutines can drive it in parallel.
func processMessage(lg *slotLogger, m graph.Message, deps *processorDeps, counters *runCounters) {
	sender := m.SenderAddress()
	if sender == "" {
		counters.skippedEmpty.Add(1)
		return
	}
	if hasStatus(m.Categories) {
		counters.skippedTagged.Add(1)
		return
	}
	class := vendors.Classify(sender)
	if class == vendors.ClassRelay || class == vendors.ClassLogistics || class == vendors.ClassBank {
		counters.skippedNonVendor.Add(1)
		// Tag so these don't accumulate in "needs first pass" forever.
		// These are automation senders (carrier shipping, billing platforms, ERP services
		// notifications, bank alerts) — no action required from AP, but
		// the clerk's filter should see them as processed, not pending.
		markSkipDone(lg, deps, m, "Automation")
		return
	}

	_ = deps.cacheDB.SetSlotCurrent(context.Background(), "sort", lg.slot, deps.mailbox, m.ID, "examining", m.Subject, "")

	var (
		vendorName = unknownVendorName
		matchLabel string
		match      vendors.Match
		buyer      string
		isInternal = class == vendors.ClassInternal
	)

	if !isInternal {
		match = deps.resolver.Resolve(sender)
		matchLabel = string(match.Type)
		if match.Type != vendors.MatchUnknown {
			vendorName = match.Vendor.VendorName
		}
	}

	// Conversation inheritance: if this message is a reply in a thread we've
	// already tagged, inherit the Vendor + Kind + last-resolved PoNo from
	// the prior sibling. Saves an AI classifier call for internal-reply or
	// generic-sender messages where the resolver returned Unknown, and
	// seeds the PO-lookup path with a known-good PO if the prior had one.
	var prior cache.ConversationPrior
	if m.ConversationID != "" {
		pCtx, pCancel := context.WithTimeout(context.Background(), 2*time.Second)
		prior, _ = deps.cacheDB.GetConversationPrior(pCtx, deps.mailbox, m.ConversationID, m.ID)
		pCancel()
		if vendorName == unknownVendorName && prior.Vendor != "" {
			vendorName = prior.Vendor
			matchLabel = "conversation"
			lg.printf("           conv-inherit: vendor=%s from prior in thread\n", prior.Vendor)
		}
	}

	poNos := poscan.ExtractPOs(3, m.Subject, m.BodyPreview)
	if len(poNos) == 0 {
		poNos = poscan.ExtractBareSubjectNumbers(m.Subject)
	}
	// Inherit the prior PO number if we didn't find one in subject/body.
	// Thread-scoped so it's only applied when the conversation clearly
	// already had a resolved PO. Validation still runs downstream.
	if len(poNos) == 0 && prior.PoNo > 0 {
		poNos = []int64{prior.PoNo}
		matchLabel = "conv-po"
		lg.printf("           conv-inherit: po=%d from prior in thread\n", prior.PoNo)
	}

	if len(poNos) == 0 && m.HasAttachments && !attachmentBlockedMailboxes[strings.ToLower(deps.mailbox)] {
		_ = deps.cacheDB.SetSlotCurrent(context.Background(), "sort", lg.slot, deps.mailbox, m.ID, "reading-PDFs", m.Subject, vendorName)
		atts, err := deps.gc.ListAttachments(deps.mailbox, m.ID)
		if err == nil {
			// Filename-regex pass: try to extract a PO from each attachment's
			// filename BEFORE fetching its bytes. Patterns like
			// "PO1235840.pdf" or "Invoice_1235840.pdf" give us the PO for
			// free. Only P21-validated numbers count — random 7-digit runs
			// are noisy so we filter downstream.
			var filenameCandidates []int64
			for _, a := range atts {
				if a.IsInline || !isPDF(a.ContentType, a.Name) {
					continue
				}
				if isNonInvoiceFormName(a.Name) {
					continue
				}
				filenameCandidates = append(filenameCandidates, poscan.ExtractPOsFromFilename(a.Name)...)
			}
			if validated := filterPOsValidInP21(deps.p21c, filenameCandidates); len(validated) > 0 {
				poNos = validated
				matchLabel = "filename"
				lg.printf("           filename-po: found %v without opening PDF\n", validated)
			}
			// Fall through to PDF-reading only if filename didn't resolve.
			scanned := 0
			for _, a := range atts {
				if len(poNos) > 0 {
					break
				}
				if scanned >= maxAttachmentsPerMessage {
					break
				}
				if a.IsInline || !isPDF(a.ContentType, a.Name) {
					continue
				}
				if isNonInvoiceFormName(a.Name) {
					lg.printf("           pdf %q: skipped (non-invoice form — W-9/setup/tax)\n", a.Name)
					continue
				}
				scanned++
				pdfBytes, _, err := fetchAttachmentBytes(deps.mailbox, m.ID, a.ID, deps.blobStore, deps.gc, deps.cacheDB)
				if err != nil || len(pdfBytes) == 0 || len(pdfBytes) > pdftext.MaxSize {
					continue
				}

				textReturnedBytes := false
				text, err := pdftext.Extract(bytes.NewReader(pdfBytes))
				if err == nil {
					textReturnedBytes = true
					candidates := poscan.ExtractPOs(5, text)
					if validated := filterPOsValidInP21(deps.p21c, candidates); len(validated) > 0 {
						poNos = validated
						matchLabel = fmt.Sprintf("pdf:%s", a.Name)
						break
					}
				} else if !errors.Is(err, pdftext.ErrEmptyText) {
					lg.printf("           pdf-text unexpected err %q: %v\n", a.Name, err)
					continue
				}

				if deps.visionClient == nil {
					lg.printf("           pdf %q: no valid PO found in text; AI disabled\n", a.Name)
					continue
				}
				reason := "scanned image"
				if textReturnedBytes {
					reason = "text found but no P21-valid PO"
				}
				lg.printf("           pdf %q: %s, trying AI vision...\n", a.Name, reason)
				_ = deps.cacheDB.SetSlotCurrent(context.Background(), "sort", lg.slot, deps.mailbox, m.ID, "ai-vision", m.Subject, vendorName)
				png, perr := pdftext.ConvertFirstPagePNG(pdfBytes)
				if perr != nil {
					lg.printf("           pdftoppm err %q: %v\n", a.Name, perr)
					continue
				}
				visionCtx, visionCancel := context.WithTimeout(context.Background(), aiclass.VisionTimeout)
				visionPOs, notes, verr := deps.visionClient.ExtractPOsFromImage(visionCtx, png)
				visionCancel()
				if verr != nil {
					lg.printf("           ai vision err %q: %v\n", a.Name, verr)
					continue
				}
				lg.printf("           ai vision %q: POs=%v notes=%q\n", a.Name, visionPOs, notes)
				if validated := filterPOsValidInP21(deps.p21c, visionPOs); len(validated) > 0 {
					poNos = validated
					matchLabel = fmt.Sprintf("ai-pdf:%s", a.Name)
					break
				}
			}
		}
	}

	fromPDF := strings.HasPrefix(matchLabel, "pdf:")
	pdfLabel := matchLabel
	for _, po := range poNos {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		info, err := deps.p21c.LookupPO(ctx, po)
		cancel()
		if err != nil {
			lg.printf("           po %d lookup error: %v\n", po, err)
			continue
		}
		if info == nil {
			continue
		}
		if vendorName != info.VendorName && vendorName != unknownVendorName {
			lg.printf("           PO override: sender said %q, PO %d says %q — using PO\n",
				vendorName, po, info.VendorName)
		}
		vendorName = info.VendorName
		buyer = info.Buyer
		if fromPDF {
			matchLabel = fmt.Sprintf("%s po:%d", pdfLabel, po)
		} else {
			matchLabel = fmt.Sprintf("po:%d", po)
		}
		counters.poOverrides.Add(1)
		break
	}

	if isInternal && vendorName == unknownVendorName {
		counters.skippedInternalNoPO.Add(1)
		// Internal mail with no resolved PO — typically context-only threads
		// between buyers. Tag Done + Kind: Internal so they drop out of the
		// "needs first pass" counter. Clerks can filter by Kind if they want
		// to review internal chatter.
		markSkipDone(lg, deps, m, "Internal")
		return
	}

	// Hybrid classify: deterministic rules first (free, ~ms, exact for ~60%
	// of mail), AI as fallback for the ambiguous cases. Saves a GPU call on
	// every "Invoice #12345", "Statement", "Order Confirmation" subject the
	// rules can confidently tag. AI still runs when rules return empty so
	// nothing slips through unclassified.
	kind := ""
	if !isInternal {
		if k := aiclass.DeterministicKind(m.Subject, sender, m.BodyPreview); k != "" {
			kind = k
			lg.printf("           classify (rule): kind=%s\n", k)
		} else if deps.aiClient != nil {
			_ = deps.cacheDB.SetSlotCurrent(context.Background(), "sort", lg.slot, deps.mailbox, m.ID, "ai-classify", m.Subject, vendorName)
			aiCtx, aiCancel := context.WithTimeout(context.Background(), aiclass.DefaultTimeout)
			cls, err := deps.aiClient.Classify(aiCtx, m.Subject, sender, m.BodyPreview)
			aiCancel()
			if err != nil {
				lg.printf("           ai classify error: %v\n", err)
			} else {
				kind = cls.Kind
				lg.printf("           ai: kind=%s actionable=%v reason=%q\n",
					cls.Kind, cls.Actionable, cls.Reason)
			}
		}
	}

	newCats := mergeCategories(m.Categories, vendorName, buyer, kind)
	action := fmt.Sprintf("PATCH  %-30s → %s  [%s]",
		truncate(sender, 30), vendorName, matchLabel)
	badge := matchBadgeFor(matchLabel, match.Type)
	if isInternal {
		badge = "[INT-PO]"
	}
	lg.printf("  %-8s %s\n", badge, action)
	lg.printf("           subject: %q\n", truncate(m.Subject, 70))
	if buyer != "" {
		lg.printf("           buyer: %s\n", buyer)
	}
	lg.printf("           categories: %v → %v\n", m.Categories, newCats)

	if !deps.dryRun {
		_ = deps.cacheDB.SetSlotCurrent(context.Background(), "sort", lg.slot, deps.mailbox, m.ID, "tagging", m.Subject, vendorName)
		if err := deps.gc.SetCategories(deps.mailbox, m.ID, newCats); err != nil {
			lg.printf("           ERROR: %v\n", err)
			counters.errs.Add(1)
			_ = deps.cacheDB.MarkSlotCompleted(context.Background(), "sort", lg.slot, m.ID)
			return
		}
	}
	if vendorName == unknownVendorName {
		counters.taggedUnknown.Add(1)
	} else if isInternal {
		counters.taggedInternal.Add(1)
		counters.tagged.Add(1)
	} else {
		counters.tagged.Add(1)
	}

	if !deps.dryRun && deps.visionClient != nil && len(poNos) > 0 &&
		m.HasAttachments && !attachmentBlockedMailboxes[strings.ToLower(deps.mailbox)] {
		cacheCtx, cacheCancel := context.WithTimeout(context.Background(), 2*time.Second)
		has, _ := deps.cacheDB.HasInvoiceExtraction(cacheCtx, deps.mailbox, m.ID)
		cacheCancel()
		if !has {
			_ = deps.cacheDB.SetSlotCurrent(context.Background(), "sort", lg.slot, deps.mailbox, m.ID, "extracting-invoice", m.Subject, vendorName)
			// Hand off to the extract pool. Non-blocking: if the pool's
			// channel is full, drop a stub row so the rescan pass will
			// pick the message up later. Sort workers never block on
			// extract — they just keep sorting new mail.
			if deps.extractJobs != nil {
				select {
				case deps.extractJobs <- extractJob{m: m, poNo: poNos[0], currentCats: newCats, minTier: 0}:
					// queued; extract pool will handle it
				default:
					_ = deps.cacheDB.StubExtractionForRescan(context.Background(), deps.mailbox, m.ID, poNos[0])
					lg.printf("           extract queue full, stubbed for rescan\n")
				}
			} else {
				// Legacy inline path (extractConcurrency=0). Same as pre-split.
				extractInvoice(deps.cacheDB, deps.gc, deps.visionClient, deps.fallbackVisionClient, deps.paddleClient, deps.fallbackJobs, deps.p21c, deps.blobStore, deps.mailbox, m, poNos[0], newCats, 0, lg.slot)
			}
		}
	}

	_ = deps.cacheDB.MarkSlotCompleted(context.Background(), "sort", lg.slot, m.ID)
}

// hasStatus returns true if any category is a Status: marker, meaning the
// message has already been through the worker (or a clerk has set a status).
func hasStatus(cats []string) bool {
	for _, c := range cats {
		if strings.HasPrefix(c, statusCatPrefix) {
			return true
		}
	}
	return false
}

// storeExtractionErr writes a needs_rescan=true error row for a message that
// failed somewhere in the extraction pipeline, and tees the line to the
// extraction-errors log file. Replaces three near-identical closures that
// used to live inside extractInvoice / verifyInvoice / openExtract.
//
// nil-safe on vc: if the client is nil (rare, only on init failure paths
// where the worker entered fallback mode without a primary), the model
// field is recorded as "unknown" rather than crashing on vc.Model().
func storeExtractionErr(cacheDB *cache.Cache, mailbox string, m graph.Message, vc *aiclass.Client, poNo int64, msg string, start time.Time) {
	model := "unknown"
	if vc != nil {
		model = vc.Model()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = cacheDB.StoreInvoiceExtraction(ctx, mailbox, m.ID, model, poNo, nil, msg, time.Since(start), true)
	errLog.logExtractionErr(m.ID, m.Subject, model, msg)
}

// markSkipDone tags a message the worker deliberately skipped with
// Status: Done + Kind: <kind>. Prevents bare-category mail from piling
// up in the "needs first pass" counter when it's actually already been
// examined and found non-actionable (automation senders, internal
// chatter without a PO, etc). Best-effort — Graph failure logs but
// doesn't stop worker flow.
func markSkipDone(lg *slotLogger, deps *processorDeps, m graph.Message, kind string) {
	if deps.dryRun {
		return
	}
	// Preserve any non-Dispatch categories the user/another system may
	// have set; layer our Status + Kind on top.
	cats := []string{}
	for _, c := range m.Categories {
		if strings.HasPrefix(c, statusCatPrefix) ||
			strings.HasPrefix(c, vendorCatPrefix) ||
			strings.HasPrefix(c, kindCatPrefix) ||
			strings.HasPrefix(c, buyerCatPrefix) ||
			strings.HasPrefix(c, blockerCatPrefix) ||
			strings.HasPrefix(c, "Owner: ") {
			continue
		}
		cats = append(cats, c)
	}
	cats = append(cats, "Status: Done", "Kind: "+kind)
	if err := deps.gc.SetCategories(deps.mailbox, m.ID, cats); err != nil {
		lg.printf("           skip-tag err %s: %v\n", truncate(m.ID, 30), err)
	}
	// Mark the message as "looked at, not an invoice" so UnextractedCount
	// (total messages without an extraction row) stops counting it. Without
	// this, every automation/internal skip lingers in the unextracted count
	// forever — the log reads like a growing backlog when in fact it's
	// drained-as-much-as-it's-going-to-drain.
	skipCtx, skipCancel := context.WithTimeout(context.Background(), 3*time.Second)
	if err := deps.cacheDB.MarkSkipExtraction(skipCtx, deps.mailbox, m.ID, kind); err != nil {
		lg.printf("           skip-cache err %s: %v\n", truncate(m.ID, 30), err)
	}
	skipCancel()
}

// mergeCategories returns the new category list: preserve anything not
// in our managed namespaces, then add Vendor:X, Status: New, and optionally
// Buyer:X and Kind:X. Empty buyer/kind skips the corresponding entry.
// vendorFromCategories pulls the resolved vendor name out of a categories
// list — used by the extraction path to thread vendor down to the AI client
// for per-vendor prompt selection. Returns empty string when no Vendor: tag
// is present (caller's selectInvoicePrompt will fall back to the generic
// prompt).
func vendorFromCategories(cats []string) string {
	for _, c := range cats {
		if strings.HasPrefix(c, vendorCatPrefix) {
			return strings.TrimPrefix(c, vendorCatPrefix)
		}
	}
	return ""
}

func mergeCategories(existing []string, vendor, buyer, kind string) []string {
	out := make([]string, 0, len(existing)+4)
	for _, c := range existing {
		if strings.HasPrefix(c, vendorCatPrefix) ||
			strings.HasPrefix(c, statusCatPrefix) ||
			strings.HasPrefix(c, buyerCatPrefix) ||
			strings.HasPrefix(c, kindCatPrefix) {
			continue // overwrite our managed namespaces; preserve everything else
		}
		out = append(out, c)
	}
	out = append(out, vendorCatPrefix+sanitizeCategoryValue(vendor))
	out = append(out, statusNewCategory)
	if buyer = strings.TrimSpace(buyer); buyer != "" {
		out = append(out, buyerCatPrefix+sanitizeCategoryValue(strings.ToLower(buyer)))
	}
	if kind = strings.TrimSpace(kind); kind != "" {
		out = append(out, kindCatPrefix+sanitizeCategoryValue(kind))
	}
	return out
}

// sanitizeCategoryValue drops characters Outlook's Master Category List rejects.
// Commas are the known offender (Graph returns ErrorPropertyUpdate 400 on PATCH).
// P21 vendor names like "HyLite LED LLC, Arva" or "JMP Equipment Company, LLC"
// would otherwise fail every run. Collapses the resulting double-spaces.
func sanitizeCategoryValue(s string) string {
	s = strings.ReplaceAll(s, ",", "")
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	return strings.TrimSpace(s)
}

func matchBadge(t vendors.MatchType) string {
	switch t {
	case vendors.MatchExact:
		return "[EMAIL]"
	case vendors.MatchDomain:
		return "[DOMAIN]"
	case vendors.MatchStem:
		return "[STEM]"
	case vendors.MatchName:
		return "[NAME]"
	default:
		return "[UNK]"
	}
}

