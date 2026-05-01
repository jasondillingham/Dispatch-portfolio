# Dispatch

**A cross-inbox dashboard for an AP (accounts-payable) team that reads
shared mailboxes via Microsoft Graph, auto-tags every message with its ERP
vendor + PO, runs invoice reconciliation against the ERP, and lets clerks
triage who's working on what — without replacing Outlook as the reader.**

> ⏸ **Paused prototype.** Built as a proof-of-concept to evaluate whether
> a thin in-house augmenter could replace a $50/user/month commercial tool
> (Front). Phase 1 of an accuracy-improvement loop shipped before the
> project paused pending evaluation of a 3rd-party product covering the
> same ground. See [`PAUSED.md`](PAUSED.md) for the full state-at-pause.

## Why this was worth building

The motivating problem: an AP team gets vendor invoices scattered across
13+ shared mailboxes. Every message is a to-do — match it to a PO,
reconcile against expected pricing, file it. There's no shared view of
what's in flight, who owns what, or what's blocked.

Front solves this commercially at ~$50/user/month. For 20+ users that's
$12K+/yr — and the bill scales with headcount. The company already paid
for Microsoft 365. So the question was: *how thin can an in-house
augmenter be while still covering the cross-inbox visibility piece?*

That cost ceiling drove the most interesting design decision in the
project:

**State lives in Outlook Categories, not in a Dispatch database.** Every
workflow signal — `Vendor: <name>`, `Status: New|In Progress|Blocked|Done`,
`Owner: <user>`, `Blocker: Pricing|PO|Vendor|Purchasing` — is an Outlook
Category that any clerk can read or edit from any Outlook client. Dispatch
is a *participant* in that state, not the owner of it. A clerk who never
opens Dispatch can edit `Status: Done` directly in Outlook; on the next
poll, Dispatch picks up the change.

This eliminates the database-of-record drift problem that plagues most
tools-on-top-of-email. Dispatch's SQLite is a *cache*, designed to be
wipeable. If you delete it, the next sync rebuilds from Outlook + the
ERP, and no workflow state is lost.

## The accuracy loop

The other interesting design decision: how the AI extraction layer
self-improves *without* becoming a closed-loop autonomous training system.

Dispatch reads PDF invoices and extracts structured fields (PO number,
invoice number, total, line items) using a tiered pipeline — fast vision
model first, slower-but-stronger model on fallback. Some extractions are
wrong. The straightforward fix would be a closed-loop trainer: clerk says
"this is wrong," system collects examples, retrains, deploys. That has
four well-known failure modes documented in
[`ACCURACY-LOOP.md`](ACCURACY-LOOP.md) — and the doc explicitly rejects it
in favor of a 6-phase clerk-in-the-loop design where:

1. Clerks record verdicts (right / wrong / wrong + correction)
2. Wrong verdicts trigger a structured diagnostic dump
3. A strong model (Claude API) generates candidate prompt fixes
4. Fixes are evaluated against a corpus before any deployment
5. Promotion happens via Git PR — humans approve every change
6. Distillation back to the local model is a deliberate, opt-in step

Phase 1 (verdict capture) shipped. The other phases are scoped but not
implemented; see the doc for the rationale + the four failure modes that
killed the autonomous-loop alternative.

## What it does

- **Reads mail** via Graph API every 60s (delta sync), auto-classifies
  each sender (vendor / internal / automation / marketing) and resolves
  to an ERP vendor by email, domain, name token, or brand match
- **Discovers PO numbers** from subject, body, and attachment filenames
  via a heuristic scanner with column-layout awareness
- **Extracts invoice data from PDFs** through a tiered pipeline (text
  layer → OCR → vision model → strong-model fallback) and reconciles
  every line against the PO. Line-level verdicts: match, price/qty
  mismatch, missing, extra, fee-only (shipping/tax/handling)
- **Polls the ERP** every 10 min for voucher status; auto-flips
  `Status: Done` once the voucher posts. Clerks never click "Done" — the
  act of posting in the ERP *is* the close
- **Surfaces a queue UI** with filter tabs, keyboard-driven review mode,
  PO drill-down, vendor mini-history, and a "why blocked" summary
- **Operational admin page** with pipeline flow chart, soft-restart,
  model performance, endpoint health, queue-depth drill-downs

## Architecture

```
                                  ┌──────────────────────────┐
Microsoft Graph  ─────▶ web-sync ─┤ Local mirror             │
  /delta poll                     │ (bodies + attachments,   │
  every 60s                       │ dedup by sha256)         │
                                  └────────────┬─────────────┘
                                               │
                                               ▼
                       ┌─────────────┐  ┌──────────────┐  ┌──────────────────┐
                       │ SORT POOL   │─▶│ EXTRACT POOL │─▶│ FALLBACK POOL    │
                       │ N workers   │  │ N workers    │  │ N workers        │
                       │             │  │              │  │                  │
                       │ classify,   │  │ tier 1: text │  │ tier 4: strong   │
                       │ vendor      │  │ tier 2: OCR  │  │ vision model on  │
                       │ resolve, PO │  │ tier 3:      │  │ GPU host         │
                       │ discovery   │  │ vision model │  │                  │
                       │             │  │ on GPU host  │  │ recon · auto-    │
                       │             │  │              │  │ blocker · cooldown│
                       └─────────────┘  └──────────────┘  └──────────────────┘
                                                                 │
                                  ┌──────────────────┐           │
                                  │ Voucher sync     │◀──────────┘
                                  │ every 10 min     │   Done = voucher posted
                                  │ (ERP polling)    │
                                  └──────────────────┘
```

- **Worker** (`cmd/dispatch-worker`): the long-running pipeline. Three
  pools draining three channels with tier-based escalation, per-content
  cooldowns, and a rescan queue with attempt cap.
- **Web** (`cmd/dispatch-web`): the UI, voucher sync goroutine, admin
  tools, and the `/extract-review` bulk verdict-capture surface.
- **Cache** (`internal/cache`, SQLite): cached message metadata,
  extraction results, recon snapshots, voucher status, clerk notes,
  attachment rotations, cooldowns, clerk verdicts. Throwaway by design.
- **Blobstore** (filesystem-backed): full message bodies + attachment
  PDFs, dedup'd by sha256. Survives cache wipes.

## Tech stack

- **Go** (stdlib `net/http` + `chi` router + `html/template`)
- **HTMX + Alpine.js + Bootstrap 5** — CDN-loaded, no build step
- **Microsoft Graph API** via direct HTTP (client-credentials → bearer)
- **SQLite** for cache, **MSSQL** for ERP reads (read-only)
- **Ollama** for local vision inference (primary + fallback GPU hosts)
- **systemd** for service lifecycle

## What's worth reading

For a reviewer who wants to see how I think, in rough order of value:

| Doc | What's in it |
|---|---|
| [`ACCURACY-LOOP.md`](ACCURACY-LOOP.md) | The 6-phase plan for AI self-improvement, the rejected closed-loop alternative, and the four failure modes that killed it. The strategy doc; reads as design thinking. |
| [`PHASE1.md`](PHASE1.md) | A self-contained work order for Phase 1 of the accuracy loop, written so a fresh AI session can pick up and ship it autonomously. Shows how I scope work for autonomous execution — schema, code locations, exact handler shapes, tests, deploy commands, acceptance checks. |
| [`PROTOTYPE.md`](PROTOTYPE.md) | The original build plan. Problem statement, the cost-driven "augmenter not replacement" reframe, MVP scope (in/out), data model, UI sketch. |
| [`PAUSED.md`](PAUSED.md) | Current state at pause: what's running, what was next, what to revisit on resume. |
| [`MODEL-RESEARCH.md`](MODEL-RESEARCH.md) | Notes on model selection — A/B comparison of vision models for invoice extraction, GPU-fit constraints, fallback strategy. |
| [`SAMPLE-DATA.md`](SAMPLE-DATA.md) | Synthetic representatives of the kinds of mail an AP team actually receives. The classification problem in concrete form. |

## Layout

```
cmd/
  dispatch-worker/      # pipeline (sort → extract → fallback)
  dispatch-web/         # UI + voucher sync + admin
  strip-categories/     # one-shot: clear Dispatch-managed categories
  reality-check/        # one-shot diagnostics
  ...
internal/
  aiclass/              # Ollama client + prompts (classify, extract, verify)
  blobstore/            # filesystem-backed PDF + body cache
  cache/                # SQLite schema + queries
  graph/                # Graph API client (mail, attachments, categories)
  erp/                  # MSSQL client (PO lines, voucher lookup, users)
  pdftext/              # poppler-based text + first-page PNG extraction
  poscan/               # PO-number heuristics (subject, body, filenames)
  recon/                # invoice-vs-PO reconciliation engine
  sync/                 # Graph delta sync + local mirror
  ui/                   # ViewMessage shape, filters, category mutations
  vendors/              # sender → ERP vendor resolver
deploy/
  dispatch-web.service     # systemd unit
  dispatch-worker.service  # systemd unit
  dispatch-admin.sudoers   # NOPASSWD scope for soft-restart
```

## Build

```bash
make build
./bin/dispatch-web -addr :8085
./bin/dispatch-worker -mailbox ap@example.com \
  -concurrency 8 -extract-concurrency 4 -fallback-concurrency 3 \
  -loop-seconds 120
```

The repo doesn't include the runtime configs (Microsoft Graph credentials,
ERP connection strings, vendor email mappings) — those are environmental.
The interesting parts are the architecture, the design docs, and the code
itself.
