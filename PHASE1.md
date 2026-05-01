# Phase 1 Work Order: Clerk Verdict Capture

This is a self-contained implementation prompt for a fresh Claude Code session.
Read this file end-to-end before doing anything. Every decision the new session
needs to make has been pre-decided and is documented inline. Do the work, ship
it, log the result. Do not ask clarifying questions — if something seems
ambiguous, the answer is in this doc; re-read the relevant section.

## Mission

Build clerk verdict capture for Dispatch. AP clerks need a way to flag when
the AI got the extraction wrong (or right). This is Phase 1 of the
6-phase accuracy loop documented in `ACCURACY-LOOP.md`. Read that file before
starting; it explains *why* this matters and what builds on top of it.

**Success criteria** (all must be true before the work is done):

1. Three buttons on the message detail page (`/message/{id}`) and on the AP
   side-by-side view (`/ap`): **✓ Looks right**, **✗ Wrong**, **✗ Wrong, here's what it should be** (the third opens a small inline form).
2. New `clerk_verdicts` SQLite table created via schema migration v3.
3. Three new cache methods exposed: `RecordVerdict`, `ListVerdictsByMessage`,
   `VerdictCountsByVendor`.
4. New `POST /message/{rowID}/verdict` route handles all three button cases.
5. Detail + AP pages show the user's most recent verdict if any (so they
   don't double-click).
6. Overview page (`/admin/overview`) gains a new leaderboard: **"Vendors clerks disagree with most"** — top 10 by `verdict='wrong'` or `'corrected'` count over the last 30 days.
7. New tests in `internal/cache/` for the three cache methods and for the
   migration apply.
8. Build clean (`go build ./...` passes), all tests pass (`go test ./...`),
   deployed to staging, committed, pushed.
9. The Phase 1 status row in `ACCURACY-LOOP.md` updated from **"Not started"**
   to **"Done"** with the commit SHA recorded.
10. Append a one-line entry to `todo.md` under "Recently shipped" noting the
    Phase 1 completion + commit SHA.

If any of #1–#10 isn't true, the work is not done.

## Pre-granted permissions

You do NOT need to ask for permission for any of the following. All of these
are explicitly authorized for this work order:

- Run `go build`, `go test`, `go fmt`, `go vet`
- Read any file in `~/projects/dispatch/`
- Edit any file under `cmd/dispatch-web/`, `cmd/dispatch-worker/`,
  `internal/cache/`, `cmd/dispatch-web/templates/`
- Edit `ACCURACY-LOOP.md`, `todo.md`, `CLAUDE.md`
- Run `make staging-deploy` (cross-compiles + rsyncs to staging — safe; reverses by re-deploy)
- Run `ssh dispatch@<staging-host> 'sudo systemctl restart dispatch-web'` (graceful restart)
- Run read-only SQLite queries against the staging DB:
  `ssh dispatch@<staging-host> 'sqlite3 -readonly /home/dispatch/.dispatch/cache.db "..."'`
- Run `git add` (specific files), `git commit`, `git push origin main`
- Use the existing CSRF middleware (it already covers all POST routes)

Do NOT do any of these without asking:
- Run any destructive SQL (DELETE, DROP, etc.) against the staging DB
- Run `git push --force` or anything that rewrites history
- Skip git hooks (`--no-verify`)
- Modify `data/` or `configs/` files (they're real production data)
- Modify the existing schema migrations (v1, v2) — append v3, never edit prior versions
- Add new external dependencies to `go.mod` (this work needs zero new deps)

If you genuinely hit a blocker that requires a decision not in this doc, log
it in `todo.md` under a new "## Phase 1 questions" section and proceed with
whatever you've completed. Do not stall.

## Context (minimum)

- Repo at `~/projects/dispatch/`
- Module: `dispatch` (see `go.mod`)
- Go 1.21+, stdlib only for new code (chi router for HTTP, modernc.org/sqlite for DB)
- Templates in `cmd/dispatch-web/templates/` use Go html/template + HTMX +
  Bootstrap 5 (CDN-loaded, no build step)
- All AP-mode handlers live in `cmd/dispatch-web/ap.go`
- Cache schema migrations live in `internal/cache/cache.go` in the
  `migrations` slice (append-only, never reorder)
- Cache helper methods are split across `internal/cache/cache.go`,
  `voucher.go`, `workers.go`, `messages.go`, `aux_tables.go`. **New verdict
  helpers go in `aux_tables.go`** — same pattern as InvoiceNote (the existing
  append-only clerk-action table).
- Staging is at `dispatch@<staging-host>`, binaries at `/opt/staging/dispatch/bin/`
- Cache DB at `/home/dispatch/.dispatch/cache.db` on staging
- HTTP Basic auth: user=`ap`, password in `/opt/staging/dispatch/env`
  (env var `DISPATCH_PASSWORD`)
- Commit message convention: imperative present tense, bullet body, end with
  `Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>`
- Use HEREDOCs for multi-line commit messages (see prior commits for examples)

## Implementation plan

Do these in order. Each step has a clear "definition of done."

### Step 1 — Schema migration v3

**File**: `internal/cache/cache.go`, append to the `migrations` slice
right after the v2 entry.

```go
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
```

Constraints (not enforced by SQLite but documented in the comment):
- `verdict` must be one of: `right`, `wrong`, `corrected`
- `corrected_data` is a JSON string when verdict='corrected', NULL otherwise
- Append-only — no edit/delete (matches `invoice_notes` pattern)

**Done when**: `migrations` slice has v3 entry; on next worker startup,
`schema_migrations` table contains row `(3, ..., <timestamp>)`. Verify with:
```bash
ssh dispatch@<staging-host> 'sqlite3 -readonly /home/dispatch/.dispatch/cache.db "SELECT * FROM schema_migrations;"'
```

### Step 2 — Cache helpers

**File**: `internal/cache/aux_tables.go` (append to end of file, alongside the
`InvoiceNote` helpers).

Implement three methods + one type. Match the InvoiceNote conventions exactly
(receivers, context as first arg, sql.NullString for optional fields).

```go
// Verdict is one append-only clerk verdict on an extraction. Drives the
// accuracy-loop corpus (see ACCURACY-LOOP.md) — every wrong/corrected verdict
// is a candidate diagnostic case to feed to the strong-model teacher in
// Phase 2-3.
type Verdict struct {
    UID            int64
    User           string
    Verdict        string    // "right" | "wrong" | "corrected"
    CorrectedData  string    // JSON string, empty when not 'corrected'
    CreatedAt      time.Time
}

// RecordVerdict appends a verdict row. No edit path — clerks can re-record
// (e.g. flipped opinion after re-reading the PDF) and we keep all of them
// chronologically. Caller is responsible for normalizing verdict to one of
// the canonical strings before calling.
func (c *Cache) RecordVerdict(ctx context.Context, mailbox, messageID, user, verdict, correctedData string) error

// ListVerdictsByMessage returns all verdicts on one message, newest first.
// Used by the detail/AP UI to show the clerk's most recent verdict (so they
// can see "you marked this Wrong 2h ago" instead of double-clicking).
func (c *Cache) ListVerdictsByMessage(ctx context.Context, mailbox, messageID string) ([]Verdict, error)

// VendorVerdictCount is one row in the "vendors clerks disagree with most"
// leaderboard.
type VendorVerdictCount struct {
    Vendor      string
    Right       int
    Wrong       int  // includes 'wrong' and 'corrected'
    Total       int
    DisagreeRate float64  // (Wrong + Corrected) / Total, 0.0-1.0
}

// VerdictCountsByVendor returns per-vendor verdict tallies over the recent
// window. Joins messages.categories_json on Vendor: prefix to group. Mailbox-
// scoped. Limit caps the result count (top N by Wrong+Corrected).
func (c *Cache) VerdictCountsByVendor(ctx context.Context, mailbox string, since time.Time, limit int) ([]VendorVerdictCount, error)
```

Implementation notes:
- For `VerdictCountsByVendor`, the SQL needs to extract the vendor from
  `messages.categories_json`. Reuse the pattern from `RecentMessageMeta` in
  `internal/cache/cache.go` — that function shows how to walk the JSON-array
  categories. **Easiest**: load all verdicts in the window, JOIN messages,
  parse categories_json in Go, aggregate in a map. The `MessageMeta` parse
  loop is the proven pattern.
- Order results by `Wrong+Corrected DESC, Total DESC` so highest-disagreement
  vendors land first.
- `DisagreeRate` is `0.0` when `Total == 0`; don't divide by zero.

**Done when**: code compiles, methods callable from main package, tests pass
(see Step 8).

### Step 3 — POST /message/{rowID}/verdict route

**File**: `cmd/dispatch-web/main.go` (route registration with the other
message-mutation routes around line 643-650) + a new handler function.

Route: `r.Post("/message/{rowID}/verdict", s.handleVerdict)` — register it
right after the existing `/message/{rowID}/status` registration.

Handler signature:
```go
func (s *server) handleVerdict(w http.ResponseWriter, r *http.Request)
```

Form fields the handler accepts (POST):
- `verdict` (required) — one of `right`, `wrong`, `corrected`
- `corrected_data` (optional) — JSON string, only meaningful for verdict='corrected'

Handler logic:
1. `decodeRowID` to get `messageID`. 400 on bad ID.
2. `r.ParseForm()`. 400 on bad form.
3. Validate `verdict` is one of the three canonical values. 400 otherwise.
4. Get `user := s.effectiveUser(r)`.
5. Call `s.cache.RecordVerdict(ctx, s.mailbox, messageID, user, verdict, correctedData)`.
   On error: 500 with the error string.
6. Return: HTMX partial render of the verdict-buttons fragment showing the
   new state ("✓ You marked this Right 2s ago"), OR a 303 redirect back to
   the referer if HX-Request header isn't present.

**Done when**: route registered, handler compiles, manual curl test against
staging returns 200 and inserts a row (see Step 9).

### Step 4 — Verdict-buttons fragment template

**File**: NEW — `cmd/dispatch-web/templates/verdict-buttons.html`

Define a reusable `verdict-buttons` template that renders the three buttons
+ a status line if there's a recent verdict. Include from both the detail
template and the AP template.

```html
{{define "verdict-buttons"}}
<div class="verdict-buttons" id="verdict-{{.M.ID | rowID}}"
     style="display:flex; flex-direction:column; gap:.5rem; padding:.85rem 1rem; background:#f9fafb; border-top:1px solid #e5e7eb;">
  {{if .RecentVerdict}}
    <div class="muted" style="font-size:.85rem;">
      You marked this <strong>{{.RecentVerdict.Verdict}}</strong> {{relTime .RecentVerdict.CreatedAt}}.
      Click again to update.
    </div>
  {{end}}
  <div style="display:flex; gap:.5rem; flex-wrap:wrap;">
    <button class="action-btn verdict-btn verdict-right"
            hx-post="/message/{{.M.ID | rowID}}/verdict"
            hx-vals='{"verdict":"right"}'
            hx-target="#verdict-{{.M.ID | rowID}}"
            hx-swap="outerHTML">
      ✓ Looks right
    </button>
    <button class="action-btn verdict-btn verdict-wrong"
            hx-post="/message/{{.M.ID | rowID}}/verdict"
            hx-vals='{"verdict":"wrong"}'
            hx-target="#verdict-{{.M.ID | rowID}}"
            hx-swap="outerHTML">
      ✗ Wrong
    </button>
    <button type="button" class="action-btn verdict-btn verdict-correct"
            onclick="document.getElementById('verdict-correction-{{.M.ID | rowID}}').classList.toggle('hidden');">
      ✗ Wrong, here's what it should be
    </button>
  </div>
  <form id="verdict-correction-{{.M.ID | rowID}}" class="hidden"
        hx-post="/message/{{.M.ID | rowID}}/verdict"
        hx-target="#verdict-{{.M.ID | rowID}}"
        hx-swap="outerHTML"
        style="display:flex; flex-direction:column; gap:.5rem; padding:.5rem; background:#fff; border:1px solid #d1d5db; border-radius:.35rem;">
    <input type="hidden" name="verdict" value="corrected">
    <label style="font-size:.8rem;">Correct PO #: <input type="text" name="po_number" style="width:100%; padding:.25rem;"></label>
    <label style="font-size:.8rem;">Correct Invoice #: <input type="text" name="invoice_number" style="width:100%; padding:.25rem;"></label>
    <label style="font-size:.8rem;">Correct total: <input type="text" name="invoice_total" placeholder="e.g. 253.62" style="width:100%; padding:.25rem;"></label>
    <label style="font-size:.8rem;">Notes (optional): <textarea name="notes" rows="2" style="width:100%; padding:.25rem;"></textarea></label>
    <button type="submit" class="action-btn" style="background:#1e40af;color:#fff;align-self:flex-end;">Save correction</button>
  </form>
</div>

<style>
  .verdict-btn { font-size:.85rem; padding:.4rem .85rem; border-radius:.35rem; cursor:pointer; border:1px solid; }
  .verdict-right { background:#ecfdf5; color:#065f46; border-color:#a7f3d0; }
  .verdict-right:hover { background:#a7f3d0; }
  .verdict-wrong { background:#fef2f2; color:#991b1b; border-color:#fecaca; }
  .verdict-wrong:hover { background:#fecaca; }
  .verdict-correct { background:#fffbeb; color:#78350f; border-color:#fde68a; }
  .verdict-correct:hover { background:#fde68a; }
  .verdict-buttons .hidden { display:none; }
</style>
{{end}}
```

The form's `corrected_data` field gets posted as multiple form fields
(po_number, invoice_number, invoice_total, notes). The handler must build
JSON from those fields server-side. Default decision: build the JSON in the
handler like:

```go
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
```

(That gives downstream phases a structured payload to work with. Empty string
strings are fine — keeps the JSON shape stable.)

**Done when**: template defined; can be invoked from other templates via
`{{template "verdict-buttons" .}}`.

### Step 5 — Wire into detail template

**File**: `cmd/dispatch-web/templates/detail.html`

Find the closing `</div>` for the main detail container (probably near the
bottom, after the manual-entry section). Insert just before it:

```html
{{template "verdict-buttons" .}}
```

The detail template already has access to `.M` (the message) via `detailData`.
You'll need to add `RecentVerdict` to the data passed in.

### Step 6 — Pass RecentVerdict + updates to detailData

**File**: `cmd/dispatch-web/main.go`

Find the `detailData` struct definition (around line 66-90). Add:
```go
RecentVerdict *cache.Verdict // most recent verdict by current user, nil if none
```

Find `buildDetailData` (around line 1850+). After loading notes (the
`s.cache.ListInvoiceNotes` call), add:
```go
if list, err := s.cache.ListVerdictsByMessage(ctx, s.mailbox, msgID); err == nil {
    for _, v := range list {
        if strings.EqualFold(v.User, user) {
            data.RecentVerdict = &v
            break  // list is newest-first, take the first match
        }
    }
}
```

### Step 7 — Wire into AP template

**File**: `cmd/dispatch-web/templates/ap.html`

The verdict buttons should appear in the right-hand info pane of the
side-by-side, BELOW the existing Notes section and ABOVE the "Reach out"
(Email buyer/vendor) section. Find the existing `{{template "..." }}` calls
in that area; add:

```html
{{/* Verdict capture — clerk feedback on extraction quality. Drives the
     accuracy-loop corpus (see ACCURACY-LOOP.md). */}}
{{template "verdict-buttons" .}}
```

The apViewData struct embeds detailData, so `.M` and `.RecentVerdict` are
already accessible inside the template.

### Step 8 — Tests

**File**: NEW — `internal/cache/verdicts_test.go`

Use the test pattern from existing files (e.g., `internal/aiclass/deterministic_test.go`).
For DB-backed tests: open a temp SQLite (use the existing `Open(":memory:")`
pattern that other tests use, OR use `t.TempDir()` for a real file).

Tests required:
- `TestRecordVerdict_AppendsRow` — record one, list returns it
- `TestRecordVerdict_AppendOnly` — record three on the same message, list
  returns all three newest-first
- `TestRecordVerdict_NormalizesVerdict` — invalid verdict string is rejected
  by the handler (test the handler-level validation, not the cache call —
  the cache stores whatever the caller gives it)
- `TestVerdictCountsByVendor_AggregatesAndOrders` — seed messages with
  different vendors + verdicts, confirm aggregation + ordering
- `TestListVerdictsByMessage_EmptyResult` — message with no verdicts
  returns empty slice (NOT nil), no error

Use `aux_tables.go`'s test conventions if any exist; otherwise mirror
`internal/vendors/resolver_test.go` style.

### Step 9 — Wire into overview page

**File**: `cmd/dispatch-web/main.go` (handleAdminOverview function around
line 2370+) AND `cmd/dispatch-web/templates/overview.html`.

In the handler:
1. After the existing leaderboard computation, call:
   `since := time.Now().Add(-30 * 24 * time.Hour).UTC()`
   `verdictsByVendor, _ := s.cache.VerdictCountsByVendor(ctx, s.mailbox, since, 10)`
2. Add a new field on `overviewData`:
   `TopVendorsDisagreed []VendorVerdictCount` (or similar — match the
   existing leaderRow style if cleaner)
3. Pass it through to the template

In `overview.html`, add a new `.ov-card` block matching the existing
"Vendors with most disputes" pattern. Display columns: Vendor, Wrong+Corrected,
Right, Total, Disagree % (formatted as integer percent).

### Step 10 — Build, test, deploy

```bash
cd ~/projects/dispatch
go build ./... 2>&1 | head -10
go test ./internal/... 2>&1 | tail -10
make staging-deploy 2>&1 | tail -3
ssh dispatch@<staging-host> 'sudo systemctl restart dispatch-web'
```

If `go build` fails, fix and retry. If tests fail, fix and retry. Don't
deploy a broken build.

After deploy, verify:
```bash
# Confirm v3 migration applied
ssh dispatch@<staging-host> 'sqlite3 -readonly /home/dispatch/.dispatch/cache.db "SELECT version, description FROM schema_migrations ORDER BY version;"'
# Should show v1, v2, v3

# Test the route — POST a 'right' verdict against any extracted message
# Find a recent message ID:
ssh dispatch@<staging-host> 'sqlite3 -readonly /home/dispatch/.dispatch/cache.db "SELECT id FROM messages WHERE mailbox=\"ap@example.com\" ORDER BY received_at DESC LIMIT 1;"'
# Then encode it and POST (use the rowID encoding pattern from existing curl in todo.md)
```

### Step 11 — Commit + push

Stage explicit files (no `git add -A`):
```bash
git add internal/cache/cache.go internal/cache/aux_tables.go internal/cache/verdicts_test.go \
        cmd/dispatch-web/main.go cmd/dispatch-web/templates/verdict-buttons.html \
        cmd/dispatch-web/templates/detail.html cmd/dispatch-web/templates/ap.html \
        cmd/dispatch-web/templates/overview.html \
        ACCURACY-LOOP.md todo.md
```

Commit message:
```
feat: clerk verdict capture (Phase 1 of accuracy loop)

Phase 1 of the 6-phase accuracy loop documented in ACCURACY-LOOP.md.
Three buttons on the message detail + AP side-by-side: ✓ Looks right,
✗ Wrong, ✗ Wrong + correction. New clerk_verdicts table (schema v3),
append-only history per message. Overview page gains a "Vendors clerks
disagree with most" leaderboard.

Unblocks Phase 2 (diagnostic dump on thumbs-down) and Phase 3 (strong-
model teacher on flagged cases). Without this data layer, every later
phase is impossible — verdict capture is the foundation that lets the
system learn what it's getting wrong.

What this is NOT: a closed-loop autonomous trainer (see ACCURACY-LOOP.md
for the four failure modes that kill those). The system records clerk
feedback; humans review patterns; promotion happens via Git PR. Nothing
magic happens behind anyone's back.

Status: Phase 1 marked Done in ACCURACY-LOOP.md. Phase 2 next session.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
```

Push to origin/main.

### Step 12 — Update tracking docs

In `ACCURACY-LOOP.md`, find the status table:
```
| Phase 1 — Verdict capture | **Not started** | (planned) ... |
```
Change to:
```
| Phase 1 — Verdict capture | **Done** (commit <SHA>) | `clerk_verdicts` table, `RecordVerdict`/`ListVerdictsByMessage`/`VerdictCountsByVendor` in `internal/cache/aux_tables.go`, `POST /message/{id}/verdict`, three buttons on detail + AP, overview leaderboard |
```
where `<SHA>` is the short SHA from your commit (`git log -1 --format="%h"`).

In `todo.md`, find or create a "## Recently shipped" section and prepend
a one-liner:
```
- 2026-MM-DD — Phase 1 verdict capture shipped (commit <SHA>). Three buttons + table + overview leaderboard. Phase 2 (diagnostic dump) is next; depends on having ~20 hand-reviewed Wrong verdicts to identify patterns.
```

Commit those two doc updates as a separate small commit:
```
docs: mark Phase 1 done in ACCURACY-LOOP.md, log in todo.md

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
```

Push.

## Acceptance check (run before declaring done)

All of the following must be true. If any are false, fix before claiming done:

- [ ] `go build ./...` exits 0
- [ ] `go test ./internal/...` exits 0
- [ ] `schema_migrations` on staging includes row `(3, ...)`
- [ ] `clerk_verdicts` table exists and is queryable on staging
- [ ] Browsing to `https://<staging-host>:8085/message/<some-rowID>` shows the three verdict buttons at the bottom
- [ ] Browsing to `/ap?filter=todo&index=0` shows the three verdict buttons in the right pane (when there's a current message)
- [ ] POST to `/message/<rowID>/verdict` with `verdict=right` returns 200 and inserts a row visible via SQL
- [ ] `/admin/overview` renders without error AND shows the "Vendors clerks disagree with most" card (likely empty for now — that's correct, no clerk has clicked anything yet)
- [ ] Two commits pushed to `origin/main`: the feature commit + the docs-update commit
- [ ] `ACCURACY-LOOP.md` Phase 1 row says **Done** with a commit SHA
- [ ] `todo.md` has a "Recently shipped" entry for Phase 1

## Known issues + fallbacks

**If the migration fails on staging**: Don't manually edit the migrations
slice or run ALTER statements. Instead, log the error in `todo.md` under a
new "## Phase 1 blocker" section and stop. The error will be in
`sudo journalctl -u dispatch-web --since "5 minutes ago"`.

**If a test fails for a reason unrelated to your changes**: Check
`go test ./...` against `main` BEFORE your changes (use `git stash`).
If the failure pre-exists, log it in `todo.md` and proceed — it's not your
problem to fix.

**If `make staging-deploy` succeeds but the service won't start**: Roll back
with `git revert HEAD && make staging-deploy && ssh ... systemctl restart`.
Log the original failure in `todo.md` with the journalctl output.

**If you can't decide between two approaches**: Pick the simpler one.
Document the decision inline in code comments. The next session can revisit
if it was wrong.

**If you discover the work is bigger than expected**: Ship Step 1-3
(migration + cache helpers + route handler) as the minimum viable Phase 1.
The buttons + UI integration can be a follow-up commit. The backend is the
critical path; the UI can stub.

## Final notes

- Match existing code style. The codebase has consistent conventions; if you
  find yourself inventing a new pattern, look for an existing similar pattern
  first (notes for append-only tables, recon for leaderboard rendering, etc.).
- Don't add features not in this spec. No verdict-deletion, no
  verdict-editing, no "snooze" verdict. The spec is the spec.
- Don't refactor unrelated code while you're in there. Phase 1 ships clean.
- When in doubt about ANYTHING, re-read this file before asking. The answer
  is here.

When complete, the system has learned a new sense: clerk feedback. Phase 2
will use it to build the diagnostic corpus. The architecture compounds from
here — but only if Phase 1 lands solid.
