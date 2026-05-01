# Dispatch — Paused

**Paused 2026-05-01.** Reason: management identified a 3rd-party product
that's ~90% ready for the same use case Dispatch was prototyping. Rather
than continue building in parallel, the project pauses pending evaluation
of the commercial option.

This is a *pause*, not abandonment. The prototype is in a clean shippable
state (Phase 1 of the accuracy loop just landed) and can be resumed if
the 3rd-party path doesn't work out.

## State at pause

- Last shipped feature: clerk verdict capture (Phase 1 of the accuracy
  loop) plus a `/extract-review` bulk verdict-capture surface for
  efficiently seeding the corpus
- Web service: still running for read-only browsing of the cached data
- Worker service: **stopped + disabled** so the AI inference pipeline
  isn't burning cycles for nothing
- Cache: 1453 messages, 726 invoice extractions, ~157 MB of body +
  attachment data on the local mirror

## What was next

The 6-phase accuracy loop is documented in
[`ACCURACY-LOOP.md`](ACCURACY-LOOP.md). Phase 1 is **Done**; Phases 2–6
are scoped but not started:

1. Phase 2 — diagnostic dump on thumbs-down verdicts
2. Phase 3 — strong-model teacher (Claude API) on flagged cases
3. Phase 4 — corpus + eval harness
4. Phase 5 — promotion workflow (auto-PR when eval beats threshold)
5. Phase 6 — distillation (parked; depends on corpus first)

[`PHASE1.md`](PHASE1.md) exists as a self-contained work-order template.
Phase 2 would mirror its format — schema, code locations, exact handler
shapes, tests, deploy commands, commit message — written so a fresh AI
session can pick up and ship it autonomously.

Outside the accuracy loop, a 7-item production-polish plan was also
paused mid-stream. The AP-mode UI was ~75% done; onboarding, audit trail,
monitoring, backups, accessibility, and a single-clerk pilot were the
remaining items.

## Decision to revisit on resume

The accuracy loop assumes Dispatch keeps owning the AI extraction. If the
3rd-party tool ends up handling extraction itself, Phases 2–6 are moot —
the corpus and self-improvement loop only matter while *we* own the
extractor. Three plausible futures on resume:

- **3rd-party covers extraction**: retire Dispatch, or repurpose to
  cross-mailbox visibility only (the Outlook-categories-as-state-machine
  layer has value independent of the AI extraction).
- **3rd-party falls short**: resume Phase 2, build the corpus, ride the
  loop to better extraction.
- **3rd-party covers a *different* slice**: keep Dispatch focused on the
  uncovered slice and rescope the roadmap.

Don't assume the answer on resume — re-evaluate before writing code.

## What this project is not

Worth naming, since the constraints shaped the design:

- **Not a Front clone.** No reply composer, no conversation editor, no
  rules engine depth. Outlook does those things well; Dispatch only adds
  what Outlook can't.
- **Not a closed-loop autonomous trainer.** Clerks record verdicts;
  humans review patterns; promotion happens via Git PR. Nothing trains
  itself behind anyone's back. See
  [`ACCURACY-LOOP.md`](ACCURACY-LOOP.md) for the four failure modes that
  killed the autonomous-loop alternative.
- **Not a replacement for the document-storage system.** Dispatch reads
  mail and reconciles invoices; it doesn't file the resulting paperwork.
  Filing stays with the existing tool.
