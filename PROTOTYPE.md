# Dispatch — Prototype Build Plan

## Problem Statement

The company's AP clerks get vendor emails scattered across 13+ shared inboxes (`ap@`, regional aliases, individual clerks). Each email is a to-do: match it to the ERP, resolve discrepancies, file it. Today there's no shared view of what's in flight, who's working on what, or what's blocked waiting on purchasing or a vendor callback.

**Front solves this commercially, but at ~$50/user/mo × 20+ users = $12K+/yr — and that number only grows when sales adopts it too. For a family-owned distributor with M365 already paid for, that license cost is the deal-breaker.**

## The Reframe — Dispatch is an Augmenter, Not a Replacement

Dispatch does NOT replace Outlook. Clerks keep reading and replying in Outlook. Dispatch is a **cross-inbox dashboard** that:

1. Polls the AP mailboxes via Graph API
2. Auto-resolves each message to a the ERP vendor
3. Exposes a unified view where clerks can see everything across mailboxes and set `Owner:`, `Status:`, and `Blocker:` values
4. Writes those values back as Outlook Categories — so the shared signal is visible to clerks working directly in Outlook too

**State lives in Outlook Categories.** There's no Dispatch database pretending to be authoritative. Categories are multi-user, visible in every Outlook client, and survive if Dispatch goes down.

What Dispatch gives AP that Outlook alone can't:

- Cross-mailbox unified list (one view of everything across the AP shared mailboxes)
- Visibility into who's on what
- Named blocker states (Purchasing, Vendor, Pricing, PO) that cross-cut mailboxes
- Deep the ERP integration (vendor resolution, PO lookup, pricing context) that no SaaS can build
- Future: document-storage filing automation

What Dispatch deliberately does NOT give (Outlook already does it well):

- A reader / reply UI — clerks click through to Outlook Web for that
- Rich conversation threading display — Graph's `conversationId` groups them; Outlook renders them
- Mobile — Outlook mobile is fine
- Rules engine depth — Dispatch auto-resolves vendors, but complex rules stay in Outlook or Power Automate

## MVP Scope

### In scope

- Poll **one prototype mailbox** (`catchall@example.com`) via Graph
- Auto-resolve sender → the ERP vendor, write `Vendor:` category
- Unified list UI (single mailbox for MVP; architected for N)
- Set `Owner:` / `Status:` / `Blocker:` via UI → writes categories back via Graph
- Category changes made from Outlook flow through to Dispatch on next poll (no UI needed to display them; that already works)
- Show conversation grouping (Graph `conversationId`)
- Read-only on attachments (prototype mailbox is phishing-adjacent — strict no-persistence rule)

### Out of scope for MVP

- Multi-mailbox ingest (add after MVP works on one)
- Reply UI (Outlook does replies)
- Internal comments (Outlook already threads replies; nice-to-have later)
- SnoozeUntil + auto-unsnooze worker
- Rules engine beyond vendor-resolution
- document-storage auto-filing (v2 — and production only, never prototype)
- Mobile-optimized UI (responsive is enough)

## Architecture

```
                  ┌─────────────────────────────────┐
                  │  Microsoft 365 Graph API        │
                  │  (mailbox + categories)         │
                  └────────────┬────────────────────┘
                               │ poll + PATCH
                               ▼
                     ┌──────────────────────┐
                     │  Dispatch Worker     │
                     │   - mail sync        │
                     │   - vendor resolver  │
                     │   - category writer  │
                     └───┬──────────┬───────┘
                         │          │
                         ▼          ▼
              ┌────────────────┐  ┌───────────────┐
              │ Dispatch       │  │ the ERP (vendor   │
              │ SQLite cache   │  │ master +      │
              │ (vendor_map,   │  │ contact       │
              │  last-synced)  │  │ email)        │
              └────────┬───────┘  └───────────────┘
                       │
                       ▼
              ┌─────────────────┐
              │ Dispatch Web UI │◄──── AP clerks (browser)
              │ (Go + HTMX)     │
              │ Cross-inbox     │
              │ unified list    │
              └─────────────────┘
```

Two processes, shared Graph client + the ERP connection:

- **Worker**: polls Graph every few minutes, resolves vendors, writes `Vendor:` categories on new messages
- **Web**: shows the unified list; clerk actions PATCH categories directly to Graph; SQLite holds the vendor map and a thin message cache for list performance

SQLite is **cache, not source of truth**. If it's deleted, the next worker pass rebuilds it from Graph + the ERP.

## Data Model

### Outlook Categories (the durable state — lives in Exchange)

| Namespace | Values | Cardinality | Who writes | Notes |
|---|---|---|---|---|
| `Vendor` | the ERP vendor name (e.g. `HVAC-Vendor Co.`) or `Unknown` | single | Dispatch worker | Set on ingest, never overwritten |
| `Owner` | example.com username (e.g. `dsowell`) | single | Clerk via Dispatch or Outlook | Claim action |
| `Status` | `New` / `In Progress` / `Blocked` / `Done` | single | Clerk action | Defaults to `New` on ingest |
| `Blocker` | `Purchasing` / `Vendor` / `Pricing` / `PO` | multi | Clerk action | Only meaningful when `Status: Blocked` |

Four namespaces. `Vendor` is auto-set. `Owner`, `Status`, `Blocker` are clerk-editable. Outlook lets you edit categories on a shared mailbox from any client — so a clerk can do this in Dispatch OR in Outlook, and the state stays consistent.

### SQLite (cache only)

```sql
CREATE TABLE vendor_map (
    email TEXT PRIMARY KEY,
    domain TEXT NOT NULL,
    erp_vendor_id TEXT,
    vendor_name TEXT NOT NULL,
    source TEXT NOT NULL,              -- 'vendor_ud.ap_email' / 'contacts.email_address' / 'manual'
    refreshed_at TIMESTAMP
);

CREATE TABLE domain_map (
    domain TEXT PRIMARY KEY,
    erp_vendor_id TEXT,                -- only populated if unambiguous (1 domain → 1 vendor)
    vendor_name TEXT,
    vendor_count INTEGER NOT NULL,     -- how many vendors share this domain
    refreshed_at TIMESTAMP
);

CREATE TABLE message_cache (
    id TEXT PRIMARY KEY,               -- Graph message ID
    mailbox TEXT NOT NULL,
    conversation_id TEXT,
    sender_email TEXT,
    subject TEXT,
    received_at TIMESTAMP,
    categories_json TEXT,              -- snapshot from last poll, for list render
    last_synced_at TIMESTAMP
);
```

`vendor_map` and `domain_map` are already built from `data/vendor_emails.json` and `data/vendor_domains.json` — just need a nightly refresh job that re-runs the the ERP query.

`message_cache` exists only so the list view renders fast. Actions always go to Graph.

## The UI

Three panes, Front-inspired but thin:

```
┌─────────────┬─────────────────────────┬──────────────────────────┐
│ Mailboxes / │ Conversation list       │ Message detail           │
│ Filters     │ (vendor, subject, age,  │ (renders via Graph;      │
│             │  owner, status pill)    │  action buttons at top)  │
│ - All       │                         │                          │
│ - Mine      │                         │ [Claim] [Status▼] [Block▼]│
│ - Blocked   │                         │ [Open in Outlook]         │
│ - ap@       │                         │                          │
│ - ap-east@  │                         │ From: vendor@...          │
│ - ap-west@  │                         │ Subject: ...              │
│ ...         │                         │ Body...                   │
└─────────────┴─────────────────────────┴──────────────────────────┘
```

**Filters (left pane):**
- **All Open** (default triage view) — `Status ≠ Done`, across all mailboxes
- **Mine** — `Owner = <currentUser>`, all statuses except Done
- **Unclaimed** — no `Owner`, `Status ≠ Done`
- **Blocked** — `Status = Blocked`, any mailbox
- **Blocker: Purchasing** — purchasing team's default view
- **Per-mailbox** — `ap@`, the AP mailboxes, etc. (MVP: just `catchall@`)
- **Completed** — `Status = Done`, 30-day rolling window

**Detail pane (right):**
- Shows the message body via Graph (no persistence of attachments for prototype mailbox)
- Action buttons at top: **Claim** (sets Owner), **Status ▼** (New/In Progress/Blocked/Done), **Blocker ▼** (multi-select, only if Status=Blocked), **Open in Outlook** (deep link)

**What's not here:** reply composer. Clicking "Open in Outlook" goes straight to Outlook Web with the message open — they reply there.

## User Flows

### Flow A: New invoice arrives
1. Vendor emails `ap@example.com`
2. Dispatch worker picks it up within 5 min
3. Worker resolves sender → the ERP vendor, writes `Vendor: HVAC-Vendor Co.` + `Status: New`
4. Appears in Unclaimed list at the top

### Flow B: Clerk claims + enters in the ERP
1. an AP clerk opens Dispatch, sees HVAC-Vendor invoice in Unclaimed
2. Clicks **Claim** → writes `Owner: dsowell`
3. Clicks **Status ▼ → In Progress**
4. Opens the ERP in another tab, enters the invoice
5. Back in Dispatch, **Status ▼ → Done**
6. Disappears from open views; remains in Completed for 30 days

### Flow C: Pricing needs purchasing input
1. an AP clerk opens a HVAC-Vendor invoice, sees unit price doesn't match PO
2. **Status ▼ → Blocked**, **Blocker ▼ → Pricing, Purchasing**
3. She leaves a note in the message subject line or replies internally in Outlook
4. another internal user has "Blocker: Purchasing" as her default filter; sees it
5. another internal user replies in Outlook with the correct price
6. an AP clerk sees the reply in Outlook, updates the ERP, sets `Status: Done`, removes Blocker

### Flow D: Tool-resistant clerk
1. Kandi doesn't want to use Dispatch
2. She keeps working in Outlook
3. She sees `Vendor: HVAC-Vendor Co.` category on each email (set by Dispatch)
4. When done, right-click → Categorize → `Status: Done`
5. Next Dispatch poll picks up the change; item moves to Completed
6. She never opened Dispatch. Computer still knows she finished.

**Flow D is the whole point of categories-as-state.** No forced adoption.

### Flow E: Cross-inbox visibility (this is the reason Dispatch exists)
1. Accounting manager opens Dispatch → "All Open"
2. Sees 47 items across `ap@`, the various AP shared mailboxes — grouped, each tagged with owner and status
3. Notices 12 marked `Blocked: Purchasing` for over a week
4. Messages the purchasing team; they filter to "Blocker: Purchasing" and work through them

This is not possible in Outlook alone.

## Components to Build — Ordered

1. **`vendor-sync`** — the ERP query → SQLite (`vendor_map`, `domain_map`). **Already done as static JSON; wrap in a Go binary + nightly cron.**
2. **Graph mail client** — extend the upstream phishing-filter's auth + fetch pattern with PATCH support for categories + master category helpers
3. **Vendor resolver** — sender email/domain → vendor name using `vendor_map` then `domain_map` (only unambiguous domains; ambiguous domains → `Unknown`)
4. **Worker binary** — `dispatch worker`: poll one mailbox, resolve + write Vendor/Status categories, sleep
5. **Web UI list view** — three-pane layout, single-mailbox first, HTMX for live refresh
6. **Action buttons** — Claim / Status / Blocker → PATCH to Graph
7. **Multi-mailbox ingest** — extend worker to loop over configured mailboxes
8. **Cross-mailbox filters** — All Open, Mine, Blocked, per-blocker

Steps 1-5 = minimum viable prototype (see classification + unified list).
Steps 6-8 = the actual collaborative workflow.

## Category Naming Contract

The only design artifact a parallel project's author needs to agree on. If the separate mail-reading UI reads the same namespaces, both UIs stay coherent.

```
Vendor:   <Name>                                   (single, auto)
Owner:    <user>                                   (single, optional)
Status:   New | In Progress | Blocked | Done      (single, default New)
Blocker:  Purchasing | Vendor | Pricing | PO      (multi, only if Blocked)
```

Four values for `Status`. If we need more, we add `Blocker` entries instead — don't multiply statuses.

## Prototype Mailboxes

### `ap@example.com` — primary prototype data source (real AP mail, safe to write)

A dedicated test mailbox. An Exchange transport rule BCCs every inbound message addressed to the production AP mailboxes to this test mailbox. Real volume, real sender mix, real threading — but since the test mailbox is a BCC sink, none of our category writes or classifier mistakes ever touch the live AP mailboxes.

Rule properties that make it safe:
- BCC is additive — original delivery to the real AP inboxes is unchanged
- `StopRuleProcessing = $false` — existing AP whitelist/spam-bypass rules still fire first
- `RuleErrorAction = Ignore` — if the test mailbox is unavailable, original mail still delivers
- One PowerShell command to disable the transport rule

On the test mailbox we can:
- Write Outlook Categories freely (Vendor, Owner, Status, Blocker)
- Stream attachments for UI display via ephemeral Graph fetches (legitimate vendor mail, not a malware risk)
- Delete/reprocess freely — the real mailboxes are the source of truth

What we still DON'T do, even on the test mailbox:
- No writes to document-storage, shared drives, or any production store
- No writes back to the original AP mailboxes
- No outbound mail from the test mailbox (don't confuse vendors by replying from a testing inbox)

### `catchall@example.com` — secondary, danger-zone corpus

Used for Phase 1 plumbing validation only. Contains phishing, DMARC reports, spam, and false-positive filter catches — NOT representative of AP mail, hence the 6.5% vendor-resolution rate we saw. Keep for spot-testing classifier edge cases.

**Strict rule: DO NOT save attachments from this mailbox.**
- No writes of attachment bytes to disk, or any persistent store
- May stream attachment content through ephemeral Graph fetches for classification only
- May record attachment metadata (filename, content-type, size, hash) but not content
- If code touches attachments, the handling must be gated by `mailbox != "catchall@..."` or disabled entirely in prototype builds

## Risks / Open Questions

1. **Graph API throttling** — an upstream mail-processing project hit `MailboxConcurrency` with 8 concurrent fetches. Single-threaded ingest + 200ms gap, or `$batch` endpoint.

2. **Master category colors** — Outlook renders category colors from per-mailbox master category definitions. Worker must create `Vendor: *`, `Status: New/In Progress/Blocked/Done`, `Blocker: *` with distinct colors at startup, or they render uncolored (looks broken).

3. **Per-mailbox master categories** — each shared mailbox has its own master list. Multi-mailbox expansion means syncing the category taxonomy into each.

4. **Writing categories on shared mailboxes** — Graph PATCH against a shared mailbox requires appropriate delegated/application permissions. Confirm `Mail.ReadWrite.Shared` is in our app registration.

5. **Ambiguous domains** — `freemail.example.com` maps to many vendors. Vendor resolver must fall back to `Unknown` rather than guess — a `Vendor: Unknown` is still a filter/triage signal.

6. **Sales-rep agency domains** — `agency.example.com` (32 vendors), `other-agency.example.com` (23) — one agency reps many vendor lines. If auto-resolution picks the wrong vendor, it's confusingly wrong. Consider: for ambiguous domains, leave Vendor empty and require manual set, or set `Vendor: Multiple (agency.example.com)` to flag.

7. **Adoption by resistant users** — Flow D covers it. Framing matters: "the computer is filing for you" beats "you're being tracked."

8. **Coordination with a parallel mail-reading UI in flight.** A 30-min sync before committing to a category contract. If the other project adds similar tracking, Dispatch becomes the prototype. If not, Dispatch ships and the other project may fold it in later — either way, the category contract is the migration bridge.

## Key Decisions (Locked)

- **Name: Dispatch.**
- **State lives in Outlook Categories.** Dispatch is an augmenter, not a replacement. This is the cost-driven decision — $0 per-user licensing vs. Front's $50.
- **No reply UI.** Clerks reply in Outlook.
- **Prototype mailbox: `catchall@`** with strict no-attachment-persistence rule.
- **Go + HTMX + Bootstrap + SQLite**, matching workspace conventions.
- **Status taxonomy: `New` / `In Progress` / `Blocked` / `Done`** — four values, don't add more. Use `Blocker` namespace to encode specifics.
- **Cross-inbox unified view is the core value-add** — the one thing Outlook alone can't do.
- **the ERP integration is the moat** — no SaaS can build this for us; it's why we're not just buying Front.
