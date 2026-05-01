# Accuracy Loop: Verdict Capture → Diagnostics → Corpus → Prompt Evolution

> **Project paused 2026-05-01 — see [`PAUSED.md`](PAUSED.md).** Phase 1 is
> shipped; Phases 2–6 are scoped but not implemented. On resume, re-evaluate
> whether this loop is still the right strategy — if a 3rd-party tool ends
> up owning extraction, the corpus loop is moot.

This is the long-term plan for how Dispatch self-improves *without* becoming an
opaque autonomous artifact. Future-Claude in a fresh session: read this before
proposing changes to the extraction or classification pipeline.

The companion blog post is `~/Homelab/blog-posts/34-why-im-not-building-a-self-training-loop.md`
— it has the design rationale + the rejected alternative (closed-loop self-training)
and is the right read for "why these specific choices."

## Motivating problem

A 30-day vendor-by-vendor analysis (run 2026-04-30) showed:

```
Sample-HVAC - Units    48 invoices    2 with non-zero total    4% clean
Xtra Lite Lighting LLC           21 invoices    0                        0%
Sample-Faucet Brand                     12             0                        0%
ILSCO                            10             0                        0%
SecLock                           8             0                        0%
Zurn Water - Industries           7             0                        0%
Sample-Lighting Brand                      7             0                        0%
```

Extractions were succeeding (POs found, line items extracted, no error_msg) but
`invoice_amount` came back $0. The model wasn't looking for the total. Same-day
fix: rewrote the vision prompt to mirror the (already-better) text prompt's
total-finding rules + added a sum-from-lines fallback in `ExtractInvoiceData`
+ shipped a `vendorInvoicePrompts` registry so per-vendor overrides are a
5-line PR going forward.

**The actual problem the accuracy loop solves**: nothing in Dispatch told us
that Sample-HVAC's success rate had collapsed. I happened to run a
SQL query. Without it, those 48 invoices would have failed silently for as
long as the bug existed. The loop is what makes "extraction quality regressing
without anyone knowing" impossible.

## End-state architecture

A **two-tier inference** stack with continuous improvement of the fast tier:

- **Fast local model** (gemma3 / minicpm-v / similar) handles ~90% of mail.
  Per-vendor prompts in `vendorInvoicePrompts` get tuned over time. Cost: free,
  low-latency.
- **Strong-model teacher** (Claude API or local 27B) handles the failed 10%.
  Cost: real but bounded — only fires on flagged cases, not every message.

A **corpus + evaluation harness** lets humans propose prompt improvements,
score them against labeled examples, and promote winners via Git PR. The
system *suggests*; humans *promote*. Nothing magic happens behind anyone's
back. A future maintainer can `git log internal/aiclass/classifier.go` and see
exactly when each prompt was tightened, who reviewed it, what the
score-before-and-after was.

**This is not a closed-loop autonomous trainer.** That design reliably becomes
unmaintainable (no ground truth → drift, reward hacking, corruption,
debuggability collapse — see the blog post for the full failure-mode list).

## The 6 phases

Smallest first; each independently useful even if later phases never ship.

### Phase 1 — Verdict capture (1-2 days)

**Status: NOT STARTED.**

Three buttons on the detail + AP card pages:

- **✓ Looks right** — extraction matches the invoice
- **✗ Wrong** — extraction is off
- **✗ Wrong, here's what it should be** — Wrong + a small form for clerk's correction

New table (schema migration v3):

```sql
CREATE TABLE clerk_verdicts (
    verdict_uid     INTEGER PRIMARY KEY AUTOINCREMENT,
    mailbox         TEXT NOT NULL,
    message_id      TEXT NOT NULL,
    user            TEXT NOT NULL,
    verdict         TEXT NOT NULL,        -- 'right' | 'wrong' | 'corrected'
    corrected_data  TEXT,                  -- JSON; nullable
    created_at      TIMESTAMP NOT NULL
);
CREATE INDEX idx_verdicts_msg ON clerk_verdicts(mailbox, message_id);
CREATE INDEX idx_verdicts_recent ON clerk_verdicts(created_at);
```

Cache helpers (in `internal/cache/messages.go` next to the followup helpers):
- `RecordVerdict(ctx, mailbox, messageID, user, verdict, correctedJSON)`
- `ListVerdicts(ctx, mailbox, since)` — drives the overview-page leaderboard
- `CountVerdictsByVendor(ctx, mailbox, since)` — vendors with most wrong/right

Web routes:
- `POST /message/{rowID}/verdict` — body has `verdict=right|wrong|corrected` and optional `corrected_data` JSON
- Updates the row + redirects (or returns 200 for HTMX swap on the verdict-button block)

Overview page (`/admin/overview`): add "Vendors clerks disagree with most" leaderboard alongside the existing dispute / blocked leaderboards.

**Promotion gate to Phase 2**: do clerks actually click the buttons? If after
a week of pilot the table is empty, stop here — it means clerks would rather
work around bad extractions than report them, and the rest of the pipeline
has nothing to consume.

### Phase 2 — Diagnostic dump on thumbs-down (~half day)

**Status: NOT STARTED. Depends on Phase 1.**

When a clerk records `verdict=wrong` (or `corrected`), backend writes:

```
/var/dispatch/diagnostic/<message_id>/
    case.json             # metadata: vendor, sender, timestamp, clerk, verdict
    email.eml             # original email
    attachment.pdf        # source PDF
    page1.png             # rendered first page
    text-pdftotext.txt    # extracted text via poppler
    extraction-prod.json  # what the small model said
    recon.json            # recon result (if any)
    correction.json       # clerk's correction (if provided)
```

These are filesystem artifacts, NOT checked into git (sensitive vendor data,
PII). Stored on the Synology mount (`/mnt/ap-synology/dispatch/diagnostic/`)
with sha256-deduped attachment storage.

Manual review for now: I read these weekly, look for patterns, write better
vendor prompts. Specifically — does a vendor's failures cluster around one
issue (missed total) or are they all different? Cluster → per-vendor prompt
fix. Spread → root-cause investigation.

**Promotion gate to Phase 3**: have I hand-reviewed 20+ flagged cases and
identified at least one pattern that a per-vendor prompt could fix?

### Phase 3 — Strong-model teacher (2-3 days)

**Status: NOT STARTED. Depends on Phase 2.**

Async-call a strong model on diagnostic dumps with a prompt like:

> Here's the rendered PDF + email + what our small model said. What's the
> correct extraction? Use this exact JSON shape: {...InvoiceData schema...}.

Save as `teacher.json` next to the other diagnostic artifacts.

Two implementation paths to consider:
- **Claude API** — fast, accurate, costs ~$0.05 per call. At ~10 flagged
  cases/day, ~$15/month. Recommended path; matches the project's bias toward
  reliable infrastructure over self-hosted.
- **Local gemma3:27b** — already on staging hardware (ai-04). Free but slow
  (5-10s on P40 GPU; minutes on CPU) and weaker. Reasonable backup.

Configuration: `-teacher-url` (Anthropic endpoint) or `-teacher-local-url`
(Ollama endpoint), `-teacher-model` (claude-sonnet-4-6 or gemma3:27b), and a
budget cap (`-teacher-monthly-cap-cents`) that disables the teacher above
threshold.

The teacher's output is **proposed ground truth, not final**. Phase 4
human-review pulls these into the corpus only after manual approval.

### Phase 4 — Corpus + offline evaluation (1 week)

**Status: NOT STARTED. Depends on Phase 3.**

Corpus location: `testdata/corpus/<vendor_slug>/<case_id>/`

```
testdata/corpus/sample-hvac/2026-05-01-msg-AQM.../
    case.json            # vendor, source diagnostic, reviewer, timestamp
    page1.png            # input
    text.txt             # input
    expected.json        # the labeled correct extraction
```

Corpus IS checked into git — but only after human review. Diagnostic dumps
(Phase 2) live outside git and only the labeled subset gets promoted in.

New `cmd/eval-prompts` tool:

```bash
eval-prompts --vendor "sample-hvac" --prompt-file alt-prompt.txt
```

For each case in the vendor's corpus directory, runs both the production
prompt + the alternate, scores against `expected.json` on:
- po_number exact match
- invoice_number exact match
- invoice_total within $0.01
- per-line: item_id exact, qty/unit_price/extended within tolerance

Output: markdown table with side-by-side scores.

**Promotion gate to Phase 5**: at least one alternate prompt has shown a
durable >10-point lead over production on a held-out subset of the corpus.

### Phase 5 — Promotion workflow (few days)

**Status: NOT STARTED. Depends on Phase 4.**

When `cmd/eval-prompts` shows a proposed prompt outperforming production
beyond a threshold, the system opens a GitHub PR:

> ## Vendor prompt proposal: Sample-HVAC
>
> Proposed prompt scores **91% (43/47 cases)** vs production's **67% (32/47 cases)**.
> Held-out subset: **89% vs 64%**.
>
> ### Diff
> ```diff
> ...prompt diff...
> ```
>
> ### Sample wins (3 random)
> [list of cases that proposed-prompt got right and production got wrong]
>
> ### Sample preserved (3 random)
> [cases where both got it right — confirms no regression on the easy stuff]
>
> Approve to promote.

Human merges. New prompt deploys via the existing `vendorInvoicePrompts`
registry (`internal/aiclass/classifier.go`). No autonomous promotion;
no merge without a human in the loop.

### Phase 6 — Distillation (much later)

**Status: PARKED. Don't start until corpus is 1000+ cases.**

Fine-tune a small model (LoRA on a 4B base — e.g., gemma3:4b, qwen3:4b) on
the strong-model teacher outputs. Once the small model gets close to the
teacher's accuracy on the held-out set, swap it in as the production model
and retire the prompt-tuning machinery for that vendor class.

This is task #54 in the prior todo list. Listed for completeness; not on the
near-term roadmap.

## Status snapshot (2026-04-30)

| Phase | Status | Code refs |
|---|---|---|
| Foundations: per-vendor prompt registry | **Done** | `internal/aiclass/classifier.go` — `vendorInvoicePrompts`, `selectInvoicePrompt` |
| Foundations: vendor threaded through extraction | **Done** | `cmd/dispatch-worker/main.go` — `vendorFromCategories`; `cmd/dispatch-worker/extract.go` — passed to `ExtractInvoiceData` |
| Foundations: vendor-by-vendor analytics | **Partial** | `/admin/overview` shows leaderboards by Kind/Status; need to add "vendors with most thumbs-down" once Phase 1 lands |
| Phase 1 — Verdict capture | **Done** (commit 63ac0c3) | `clerk_verdicts` table (schema v3), `RecordVerdict`/`ListVerdictsByMessage`/`VerdictCountsByVendor` in `internal/cache/aux_tables.go`, `POST /message/{rowID}/verdict` in `cmd/dispatch-web/main.go`, `verdict-buttons.html` template included from detail + AP, "Vendors clerks disagree with most" card in `/admin/overview` |
| Phase 2 — Diagnostic dump | **Not started** | (planned) writes to `/mnt/ap-synology/dispatch/diagnostic/<msg_id>/` |
| Phase 3 — Strong-model teacher | **Not started** | (planned) Claude API client behind `-teacher-url` flag |
| Phase 4 — Corpus + eval | **Not started** | (planned) `testdata/corpus/`, `cmd/eval-prompts/` |
| Phase 5 — Promotion workflow | **Not started** | (planned) GitHub PR auto-creation when eval beats threshold |
| Phase 6 — Distillation | **Parked** | needs corpus first |

## What signals to watch for

The phased plan only works if data validates each phase before the next is
built. Concretely:

- **Phase 1**: count of verdicts/week. If <5/week after pilot, the architecture
  stops here — clerks won't generate the data the rest of the loop consumes.
- **Phase 2**: among 20 hand-reviewed flagged cases, do clear failure
  patterns emerge per vendor? If yes, per-vendor prompts have leverage. If
  no, root-cause is something other than prompt quality (PDF rendering, model
  capability, layout drift).
- **Phase 3**: does the strong model (whichever — Claude or local 27B) actually
  produce more accurate output than the small model on flagged cases? If the
  teacher is wrong half the time too, the bottleneck isn't model size.
- **Phase 4**: first time a proposed prompt beats production by >10 points on
  a held-out test set. If this never happens, prompts aren't the lever — the
  small model has reached its ceiling and Phase 6 (distillation/upgrade)
  jumps ahead.

## What this design explicitly is NOT

- It is not an autonomous training loop. Humans are in every promotion gate.
- It is not closed-loop. Every change is a Git PR with a human reviewer.
- It is not a replacement for clerk attention; it's an amplifier. The system
  catches more, prompts iterate, but the floor is still "what a clerk would
  catch and report."
- It is not optimization on a metric the model controls. Every promotion is
  measured against ground truth labels reviewed by a human.

## Why it survives operator turnover

A future maintainer (could be the AP pilot user, could be the next IT hire, could be
me-after-2-years-away) opens this file and sees:

1. The current state of every phase
2. What's done and where the code lives
3. What's planned and the data signals to validate before building
4. The rejected alternative (closed-loop trainer) and *why* it was rejected
5. Links to the design rationale post for deeper context

Every prompt change is a Git commit; every promotion is a PR with the eval
report attached. Nothing depends on tribal knowledge or on me being around to
re-explain why something was built a particular way. That is the entire point
of the design.
