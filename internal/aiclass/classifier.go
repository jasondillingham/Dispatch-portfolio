// Package aiclass is a thin HTTP client for Ollama's /api/generate endpoint
// used to classify AP mail as actionable vs. non-actionable.
//
// We run against a local model (gemma4:e4b by default) on ai-03 — the
// prompt is short, the JSON output schema is fixed, and we keep the model
// warm by making sequential calls from the worker loop.
package aiclass

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	DefaultURL     = "http://<gpu-host-1>:11434"
	DefaultModel   = "gemma4:e4b"
	DefaultTimeout = 45 * time.Second
)

// Allowed values for Classification.Kind. Anything else we normalize to "Other".
// Kept narrow on purpose — downstream filter logic depends on these labels.
var KnownKinds = map[string]bool{
	"Invoice":           true,
	"Statement":         true,
	"Payment":           true,
	"OrderConfirmation": true, // vendor acks of OUR PO (no invoice yet)
	"Credit":            true,
	"Dispute":           true,
	"Marketing":         true,
	"Webinar":           true,
	"Newsletter":        true,
	"Other":             true,
}

// HideByDefault is the set of Kind values we filter out of the default
// AP queue. Users see them by opening the "Marketing" tab — nothing is deleted.
// OrderConfirmation is hidden because order acks require no AP action — the
// real invoice arrives separately and that's what gets posted.
var HideByDefault = map[string]bool{
	"Marketing":         true,
	"Webinar":           true,
	"Newsletter":        true,
	"OrderConfirmation": true,
}

type Classification struct {
	Actionable bool   `json:"actionable"`
	Kind       string `json:"kind"`
	Reason     string `json:"reason"`
}

// EndpointHook is the optional observability surface on Client. OnStart fires
// the moment we pick a URL for a request; OnEnd fires when the response is
// (fully) received or an error fires. Hooks run synchronously in the calling
// goroutine — keep them cheap (one SQLite write is fine; long I/O isn't).
type EndpointHook interface {
	OnRequestStart(url, messageID string)
	OnRequestEnd(url string, dur time.Duration, err error)
}

// Client talks to one or more Ollama endpoints. When multiple URLs are passed
// (comma-separated to NewClient), each request round-robins across them — lets
// us put two GPU boxes behind one logical model and double throughput without
// any caller changes.
type Client struct {
	urls    []string
	idx     atomic.Uint64
	model   string
	http    *http.Client
	timeout time.Duration
	hook    EndpointHook
}

// SetEndpointHook installs an observability hook. Nil is fine (no-op).
func (c *Client) SetEndpointHook(h EndpointHook) { c.hook = h }

// URLs returns a copy of the configured endpoint list — useful for the caller
// when it wants to render "all endpoints" info even before any request runs.
func (c *Client) URLs() []string {
	out := make([]string, len(c.urls))
	copy(out, c.urls)
	return out
}

// doWithHook wraps http.Do so observability hooks fire around every endpoint
// request. Non-2xx responses are surfaced to the hook as errors so the UI can
// count runner crashes (500s from Ollama when a model process dies).
func (c *Client) doWithHook(req *http.Request, url string) (*http.Response, error) {
	start := time.Now()
	if c.hook != nil {
		c.hook.OnRequestStart(url, "")
	}
	resp, err := c.http.Do(req)
	hookErr := err
	if err == nil && resp.StatusCode >= 400 {
		hookErr = fmt.Errorf("http %d", resp.StatusCode)
	}
	if c.hook != nil {
		c.hook.OnRequestEnd(url, time.Since(start), hookErr)
	}
	return resp, err
}

// NewClient accepts a single URL, a comma-separated list, or an empty string
// (uses DefaultURL). All listed URLs must host the same model.
func NewClient(url, model string) *Client {
	if url == "" {
		url = DefaultURL
	}
	if model == "" {
		model = DefaultModel
	}
	raw := strings.Split(url, ",")
	urls := make([]string, 0, len(raw))
	for _, u := range raw {
		u = strings.TrimSpace(u)
		if u != "" {
			urls = append(urls, strings.TrimRight(u, "/"))
		}
	}
	return &Client{
		urls:  urls,
		model: model,
		// No http.Client.Timeout: each call sets its own context deadline
		// (DefaultTimeout for text classify, VisionTimeout*2 for invoice
		// extraction on bigger models). A single http-client timeout would
		// cap longer operations unfairly.
		http:    &http.Client{},
		timeout: DefaultTimeout,
	}
}

// endpoint returns the next URL in round-robin order. Safe for concurrent use.
// With one URL it's a no-op; with N it atomically cycles.
func (c *Client) endpoint() string {
	if len(c.urls) == 1 {
		return c.urls[0]
	}
	n := c.idx.Add(1) - 1
	return c.urls[int(n%uint64(len(c.urls)))]
}

// Model returns the configured model name (for logging / cache provenance).
func (c *Client) Model() string { return c.model }

// Ping verifies each configured server is reachable. A pool client is only
// considered healthy when every endpoint answers — otherwise the first dead
// one will silently eat every Nth request.
func (c *Client) Ping(ctx context.Context) error {
	for _, u := range c.urls {
		req, _ := http.NewRequestWithContext(ctx, "GET", u+"/api/tags", nil)
		// Ping doesn't fire the endpoint hook — it's a health probe, not real
		// work, and we don't want it polluting request-count stats.
		resp, err := c.http.Do(req)
		if err != nil {
			return fmt.Errorf("ollama %s: %w", u, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			return fmt.Errorf("ollama %s: %d %s", u, resp.StatusCode, body)
		}
	}
	return nil
}

const promptTemplate = `You are an Accounts Payable mail triage classifier. Decide if a clerk must take action on this email.

ACTIONABLE examples: invoices, statements, credit memos, payment remittances (from us to a vendor), shipping notices tied to an invoice, vendor disputes needing response.
NON-ACTIONABLE examples: marketing, product announcements, webinar/training invites, newsletters, "lunch & learn", sales pitches, promotional reminders, generic "how we design unique solutions" emails, order confirmations / order acknowledgements / sales-order notices (the vendor confirming they received our PO — the real invoice arrives separately and is what AP posts).

Respond with ONLY a JSON object, no preamble:
{"actionable": true|false, "kind": "<label>", "reason": "<short phrase>"}

<label> must be one of: Invoice, Statement, Payment, OrderConfirmation, Credit, Dispute, Marketing, Webinar, Newsletter, Other

Use OrderConfirmation when the document is a vendor acknowledgement of OUR purchase order (mentions PO# / SO# but no invoice number, no invoice total, "we received your order", "order confirmation", "sales order").

From: %s
Subject: %s
Body preview: %s`

type ollamaReq struct {
	Model   string                 `json:"model"`
	Prompt  string                 `json:"prompt"`
	Stream  bool                   `json:"stream"`
	Think   bool                   `json:"think"`
	Options map[string]interface{} `json:"options,omitempty"`
}

type ollamaResp struct {
	Response string `json:"response"`
	Done     bool   `json:"done"`
	EvalCount int   `json:"eval_count"`
}

// Classify sends one message to the model and returns its verdict.
// Empty subject is fine; empty preview is fine; empty sender triggers an error
// because the model needs at least something to look at.
func (c *Client) Classify(ctx context.Context, subject, sender, preview string) (*Classification, error) {
	if strings.TrimSpace(subject) == "" && strings.TrimSpace(preview) == "" {
		return nil, errors.New("need subject or preview")
	}
	prompt := fmt.Sprintf(promptTemplate,
		truncate(sender, 200),
		truncate(subject, 500),
		truncate(preview, 1500),
	)
	body, _ := json.Marshal(ollamaReq{
		Model:  c.model,
		Prompt: prompt,
		Stream: false,
		Think:  false,
		Options: map[string]interface{}{
			"temperature":  0.0,
			"num_predict":  150,
			"top_p":        0.95,
		},
	})

	url := c.endpoint()
	req, err := http.NewRequestWithContext(ctx, "POST", url+"/api/generate", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.doWithHook(req, url)
	if err != nil {
		return nil, fmt.Errorf("ollama request: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("ollama %d: %s", resp.StatusCode, raw)
	}
	var or ollamaResp
	if err := json.Unmarshal(raw, &or); err != nil {
		return nil, fmt.Errorf("parse ollama response: %w", err)
	}
	return parseClassification(or.Response)
}

// jsonExtract locates a {...} JSON object in the response text. Models sometimes
// include a leading/trailing whitespace or code-fence; this pulls out the first
// balanced JSON object.
var jsonObjectRe = regexp.MustCompile(`\{[^{}]*\}`)

func parseClassification(s string) (*Classification, error) {
	s = strings.TrimSpace(s)
	// Try direct parse first.
	var c Classification
	if err := json.Unmarshal([]byte(s), &c); err == nil {
		return normalize(&c), nil
	}
	// Fall back: extract the first {...} substring.
	if m := jsonObjectRe.FindString(s); m != "" {
		if err := json.Unmarshal([]byte(m), &c); err == nil {
			return normalize(&c), nil
		}
	}
	return nil, fmt.Errorf("unparseable classifier response: %q", truncate(s, 200))
}

func normalize(c *Classification) *Classification {
	c.Kind = strings.TrimSpace(c.Kind)
	// Case-normalize to match KnownKinds keys
	for known := range KnownKinds {
		if strings.EqualFold(c.Kind, known) {
			c.Kind = known
			return c
		}
	}
	// Map common aliases we've seen in real outputs
	aliases := map[string]string{
		"invoice":              "Invoice",
		"invoices":             "Invoice",
		"po":                   "OrderConfirmation",
		"po confirmation":      "OrderConfirmation",
		"po order":             "OrderConfirmation",
		"order confirmation":   "OrderConfirmation",
		"order acknowledgment": "OrderConfirmation",
		"order acknowledgement": "OrderConfirmation",
		"order ack":            "OrderConfirmation",
		"sales order":          "OrderConfirmation",
		"so confirmation":      "OrderConfirmation",
		"purchase order":       "OrderConfirmation",
		"payment remittance":   "Payment",
		"payment":              "Payment",
		"payment scheduled":    "Payment",
		"remittance":           "Payment",
		"credit memo":          "Credit",
		"credit":               "Credit",
		"statement":            "Statement",
		"dispute":              "Dispute",
		"sales pitch":          "Marketing",
		"promotion":            "Marketing",
		"promotional":          "Marketing",
		"advertisement":        "Marketing",
		"ad":                   "Marketing",
		"training":             "Webinar",
		"lunch and learn":      "Webinar",
		"lunch & learn":        "Webinar",
	}
	lk := strings.ToLower(c.Kind)
	if canonical, ok := aliases[lk]; ok {
		c.Kind = canonical
		return c
	}
	c.Kind = "Other"
	return c
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// VisionTimeout is longer than text-classify because image token processing
// is heavier. On CPU an invoice page is ~12s; add headroom for slower machines.
const VisionTimeout = 90 * time.Second

const visionPrompt = `Look at this invoice/document image. Find any Purchase Order (PO) numbers.

A PO number at Acme Distribution is a 7-digit number (sometimes labeled "PO", "P.O.", "Purchase Order", "Customer PO", or "PO #"). Do NOT include invoice numbers, order numbers starting with letters, part numbers, dates, phone numbers, or any number that isn't clearly labeled as a PO.

Return ONLY a JSON object, no preamble:
{"po_numbers": ["<po>", ...], "notes": "<short observation>"}

If no PO numbers visible, return {"po_numbers": [], "notes": "<why>"}.`

// InvoiceLine matches the JSON shape returned by the vision extraction prompt.
// Duplicated from cache.InvoiceLine on purpose to keep aiclass free of a
// dependency on cache — the worker copies values between them.
type InvoiceLine struct {
	ItemID      string  `json:"item_id"`
	Description string  `json:"description"`
	Qty         float64 `json:"qty"`
	UnitPrice   float64 `json:"unit_price"`
	Extended    float64 `json:"extended"`
}

// InvoiceData is the full extraction target.
type InvoiceData struct {
	PONumber      string        `json:"po_number"`
	InvoiceNumber string        `json:"invoice_number"`
	InvoiceDate   string        `json:"invoice_date"`
	InvoiceTotal  float64       `json:"invoice_total"`
	Lines         []InvoiceLine `json:"lines"`
}

// genericInvoicePrompt is the default vision-extraction prompt. selectInvoicePrompt
// chooses this when no vendor-specific override matches.
//
// Mirrors the (proven-stronger) text-extract prompt's total-finding rules —
// previously the vision prompt was barebones ("<number or null>") and most
// vision-extracted invoices came back with invoice_total=0 because the model
// would find line items but never look for the summary. The diagnosis was
// concrete: Sample-HVAC had 48 invoices, 0% with non-zero
// invoice_amount, even though POs and line counts were extracted.
const genericInvoicePrompt = `You are extracting structured data from an invoice or order-confirmation document image.

Return ONLY this JSON object, no preamble or code fences:
{
  "po_number": "<the customer's 7-digit purchase order number this invoice references; null if not visible>",
  "invoice_number": "<vendor's invoice number; null if not visible>",
  "invoice_date": "<YYYY-MM-DD if clearly visible, else null>",
  "invoice_total": <number — see rules below, NEVER 0 unless invoice genuinely has zero total>,
  "lines": [
    {
      "item_id": "<vendor's part number or SKU for this line>",
      "description": "<brief description as printed>",
      "qty": <number>,
      "unit_price": <number — the per-unit price actually being charged, NOT list price>,
      "extended": <number — qty * unit_price>
    }
  ]
}

Rules:
- po_number must be a 7-digit number (Acme Distribution POs are 7 digits). Common labels: "PO", "P.O.", "Purchase Order", "Customer PO", "Cust. PO". Do not confuse with sales order number, invoice number, or tracking number.
- If the invoice has multiple price columns (list vs net, base vs discounted), use the final charged price.
- invoice_total is the FINAL amount the vendor wants paid. Look carefully for ANY of: "Invoice Total", "Grand Total", "Total Due", "Amount Due", "Balance Due", "Net Amount", "Net Amount Due", "Total", "TOTAL", "Net Invoice Amount", "Please Pay", "Pay This Amount", "Remit". Include tax and freight if applicable. Use the largest sensible dollar figure near the bottom of the invoice if no explicit total label exists. Never leave this as 0 when there are line items — a missing total is a failure worth trying harder for. If the invoice spans multiple pages, the total is on the last page.
- Omit header rows, total rows, tax rows, freight rows, and subtotal rows from lines[].
- If a field truly cannot be determined, use null (not guessed values). Do NOT use 0 as a placeholder.`

// vendorInvoicePrompts holds vendor-specific extraction prompts. Keys are
// lowercase substring matches against the resolved vendor name; the first
// matching key wins. Add an entry here when a vendor's layout is
// distinctive enough that the generic prompt mis-extracts repeatedly.
//
// Format convention: each entry should be a complete prompt (not a delta),
// because keeping prompts independent lets us tune one without disturbing
// others. Use genericInvoicePrompt as the starting template.
var vendorInvoicePrompts = map[string]string{
	// Sample-HVAC - Units (an HVAC distributor): 48
	// invoices in the cache, 0% with non-zero invoice_amount under the
	// generic prompt. Their PDFs are auto-generated "Invoice Details_…"
	// documents where the total often appears under "Net Amount Due"
	// at the bottom-right of the last page, separate from a "Subtotal"
	// row. The model was finding subtotals and stopping.
	"sample-hvac": genericInvoicePrompt + `

VENDOR-SPECIFIC NOTES (Sample-HVAC / Subsidiary / HVAC-Vendor):
- Total label is typically "Net Amount Due" or "Total Due" at the bottom-right of the last page. Do not confuse with "Subtotal" or "Net Sales" mid-page.
- Part numbers are alphanumeric strings like "910000000" or "C220-S00".
- Invoice numbers begin with "910" and are 9 digits.`,
}

// selectInvoicePrompt returns the vendor-specific prompt for the given
// vendor name, or the generic prompt when no vendor entry matches. Match
// is case-insensitive substring against the registry keys — so the registry
// key "sample-hvac" will match resolved vendor names like
// "Sample-HVAC - Units" or "Sample-HVAC Products."
func selectInvoicePrompt(vendor string) string {
	if vendor == "" {
		return genericInvoicePrompt
	}
	lower := strings.ToLower(vendor)
	for key, prompt := range vendorInvoicePrompts {
		if strings.Contains(lower, key) {
			return prompt
		}
	}
	return genericInvoicePrompt
}

// ExtractInvoiceData sends a PNG image to the vision model and extracts a
// full invoice record. Uses a single request; callers should allow ~VisionTimeout
// (or more for bigger models).
func (c *Client) ExtractInvoiceData(ctx context.Context, png []byte, vendor string) (*InvoiceData, error) {
	if len(png) == 0 {
		return nil, errors.New("empty image")
	}
	b64 := base64.StdEncoding.EncodeToString(png)

	body, _ := json.Marshal(map[string]interface{}{
		"model":  c.model,
		"prompt": selectInvoicePrompt(vendor),
		"images": []string{b64},
		"stream": false,
		"think":  false,
		// Grammar-constrained decoding so the model can't emit prose preambles
		// or malformed JSON. Matches what VerifyAgainstPO does.
		"format": "json",
		"options": map[string]interface{}{
			"temperature": 0.0,
			// Match VerifyAgainstPO's budget — 1500 was too tight for invoices
			// with many line items (Xtra Lite Lighting / similar truncated
			// mid-array). 4000 gives headroom for ~50 lines plus header.
			"num_predict": 4000,
		},
	})

	url := c.endpoint()
	req, err := http.NewRequestWithContext(ctx, "POST", url+"/api/generate", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.doWithHook(req, url)
	if err != nil {
		return nil, fmt.Errorf("ollama vision request: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("ollama vision %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var or struct {
		Response string `json:"response"`
	}
	if err := json.Unmarshal(raw, &or); err != nil {
		return nil, fmt.Errorf("parse ollama vision response: %w", err)
	}

	s := strings.TrimSpace(or.Response)
	// Strip code fences the model sometimes wraps around JSON (```json ... ```).
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```JSON")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)
	// Same coercion the Verify path uses: unquote "1" → 1, strip "$62.06" → 62.06.
	// Invoice extraction pushed it further: models also pack units into strings
	// ("1 EA", "25.00 U.S.D.") which normal currency-strip doesn't handle.
	s = coerceStringNums(s)
	s = normalizeFieldAliases(s)

	var d InvoiceData
	parsed := false
	if err := json.Unmarshal([]byte(s), &d); err == nil {
		parsed = true
	} else {
		// Truncated-output recovery: same fallback the Verify path uses. The
		// model ran out of tokens mid-array (often after several line items
		// on a long invoice). Close any open string + balance brackets and
		// re-parse — we keep the lines we DID get; recon will flag the
		// missing-line discrepancy downstream.
		if repaired, ok := repairTruncatedJSON(s); ok {
			var d2 InvoiceData
			if err2 := json.Unmarshal([]byte(repaired), &d2); err2 == nil {
				d = d2
				parsed = true
			}
		}
		if !parsed {
			return nil, fmt.Errorf("parse invoice json: %w (got %q)", err, truncate(s, 300))
		}
	}
	// Total-from-lines fallback: same heuristic the text path uses. When the
	// model finds line items but misses (or zeros out) the invoice total —
	// the dominant failure pattern observed for vendors like Sample-HVAC
	// Comfort under the old prompt — sum the extended amounts. Better to
	// surface a derived total that the recon step can reconcile against than
	// to flag the row as $0 and lose the signal.
	if d.InvoiceTotal == 0 && len(d.Lines) > 0 {
		var sum float64
		for _, l := range d.Lines {
			sum += l.Extended
		}
		if sum > 0 {
			d.InvoiceTotal = sum
		}
	}
	return &d, nil
}

// VerifyLineExpected is one line the caller expects to find on the invoice.
// Matches the shape of erp.POLine but kept separate so aiclass has no erp dep.
type VerifyLineExpected struct {
	LineNo      int     `json:"line_no"`
	ItemID      string  `json:"item_id"`
	Description string  `json:"description"`
	Qty         float64 `json:"qty"`
	UnitPrice   float64 `json:"unit_price"`
	Extended    float64 `json:"extended"`
}

// VerifyLineResult is the per-line verdict from the model.
type VerifyLineResult struct {
	LineNo            int          `json:"line_no"`
	Status            VerifyStatus `json:"status"` // "match" | "differs" | "not_found"
	ObservedQty       float64      `json:"observed_qty,omitempty"`
	ObservedUnitPrice float64      `json:"observed_unit_price,omitempty"`
	ObservedExtended  float64      `json:"observed_extended,omitempty"`
	Note              string       `json:"note,omitempty"`
}

// VerifyStatus is a string-typed alias that tolerates the loose JSON some
// models (gemma4 in particular) emit for the status field. Real values are
// "match" / "differs" / "not_found"; the model occasionally emits a bare
// number ("status": 1) — we coerce those to empty so the parse doesn't fail
// the whole verify result. Downstream recon will treat empty as ambiguous
// and flag the line, which is the right behavior.
type VerifyStatus string

func (s *VerifyStatus) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || string(data) == "null" {
		*s = ""
		return nil
	}
	if data[0] == '"' {
		var str string
		if err := json.Unmarshal(data, &str); err != nil {
			return err
		}
		*s = VerifyStatus(str)
		return nil
	}
	// Numbers, booleans, anything else: drop to empty rather than failing
	// the parent struct's unmarshal. The line stays in the result but with
	// no opinion on match/differs — recon will mark it ambiguous.
	*s = ""
	return nil
}

// VerifyUnexpectedLine is an invoice line the model saw that wasn't in the
// expected set (freight, tax, bonus items, etc).
type VerifyUnexpectedLine struct {
	Description string  `json:"description"`
	Qty         float64 `json:"qty"`
	UnitPrice   float64 `json:"unit_price"`
	Extended    float64 `json:"extended"`
}

// VerifyResult is the full verify-pass output.
type VerifyResult struct {
	InvoiceNumber        string                 `json:"invoice_number,omitempty"`
	InvoiceDate          string                 `json:"invoice_date,omitempty"`
	Lines                []VerifyLineResult     `json:"lines"`
	UnexpectedLines      []VerifyUnexpectedLine `json:"unexpected_lines,omitempty"`
	InvoiceTotalObserved float64                `json:"invoice_total_observed,omitempty"`
}

const verifyPromptHeader = `You are verifying a vendor invoice against the Purchase Order we issued. The PO and its expected lines are authoritative. Your job is to confirm the invoice has these lines with matching quantities and prices.

For each expected line:
- Scan the invoice image for the item (by part number, by description, or both)
- Compare qty, unit_price, and extended to the expected values
- Report one of:
    "match"     — qty + unit_price + extended all agree (exact penny)
    "differs"   — found the item but one or more numbers differ; include observed values
    "not_found" — can't find this line on the invoice

Prices must match to the penny ($0.005 tolerance). Quantities must match exactly unless the vendor is billing a bundle or reel equivalent to the same total — in that case report "match" with a brief note.

Also note:
- Any invoice lines that do NOT correspond to one of the expected PO lines (freight, tax, bonus items)
- The invoice's grand total as printed

Return ONLY this JSON, no preamble or code fences:
{
  "invoice_number": "<vendor's invoice number>",
  "invoice_date": "<YYYY-MM-DD if visible, else null>",
  "invoice_total_observed": N,
  "lines": [
    {"line_no": N, "status": "match"|"differs"|"not_found",
     "observed_qty": N, "observed_unit_price": N, "observed_extended": N,
     "note": "short phrase"}
  ],
  "unexpected_lines": [
    {"description": "...", "qty": N, "unit_price": N, "extended": N}
  ]
}

Expected PO lines:
`

// VerifyAgainstPO runs the verification prompt: given a rendered invoice page
// and the expected PO lines, the model checks each line instead of doing
// open-ended extraction. Substantially more reliable across vendor layouts
// than ExtractInvoiceData because the model has a specific target for each
// line.
func (c *Client) VerifyAgainstPO(ctx context.Context, png []byte, expected []VerifyLineExpected) (*VerifyResult, error) {
	if len(png) == 0 {
		return nil, errors.New("empty image")
	}
	if len(expected) == 0 {
		return nil, errors.New("no expected lines supplied")
	}

	// Build the expected-lines block. One per line, human-readable.
	var sb strings.Builder
	sb.WriteString(verifyPromptHeader)
	for _, e := range expected {
		fmt.Fprintf(&sb, "%d. %s (%s) — qty %g @ $%.4f each, extended $%.2f\n",
			e.LineNo, e.ItemID, e.Description, e.Qty, e.UnitPrice, e.Extended)
	}

	b64 := base64.StdEncoding.EncodeToString(png)
	body, _ := json.Marshal(map[string]interface{}{
		"model":  c.model,
		"prompt": sb.String(),
		"images": []string{b64},
		"stream": false,
		"think":  false,
		// format:json switches Ollama to grammar-constrained decoding, so
		// smaller vision models (llava:7b, minicpm-v) can't emit prose
		// preambles or unbalanced braces. Without this, any 7-8B model
		// tends to wrap output in "Based on the invoice..." narration.
		"format": "json",
		"options": map[string]interface{}{
			"temperature": 0.0,
			// 4000 not 2000: gemma4:26b on a 5+ line invoice was hitting the
			// ceiling mid-token, leaving truncated JSON like "...status":"matc…"
			// even with format:json. Format mode prevents *malformed* JSON but
			// can't conjure tokens after num_predict runs out.
			"num_predict": 4000,
		},
	})

	url := c.endpoint()
	req, err := http.NewRequestWithContext(ctx, "POST", url+"/api/generate", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.doWithHook(req, url)
	if err != nil {
		return nil, fmt.Errorf("ollama verify request: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("ollama verify %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var or struct {
		Response string `json:"response"`
	}
	if err := json.Unmarshal(raw, &or); err != nil {
		return nil, fmt.Errorf("parse ollama verify response: %w", err)
	}

	s := strings.TrimSpace(or.Response)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```JSON")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)
	// Coerce string-wrapped numerics that minicpm-v and similar models emit
	// (e.g., "line_no": "1"). Only for our known numeric fields — leaves
	// other string fields (status, note, invoice_number) untouched.
	s = coerceStringNums(s)
	// Normalize known field-name typos (gemma4 occasionally emits "lineno"
	// instead of "line_no" mid-output; same for a few others). Cheap text
	// substitution before the JSON parse.
	s = normalizeFieldAliases(s)

	var result VerifyResult
	if err := json.Unmarshal([]byte(s), &result); err == nil {
		return &result, nil
	} else {
		// Two recovery paths layered. Both are idempotent string transforms;
		// we try them in turn (and combined) so we don't have to predict
		// which broken pattern the model emitted.
		//
		// 1. repairTruncatedJSON: closes any open string + balances
		//    brackets when the model ran out of tokens mid-array.
		// 2. keepFirstTopLevelKeys: drops duplicate top-level keys when
		//    the model went off-rails and emitted "lines" three times in
		//    a row. Without this, json.Unmarshal would use the LAST one
		//    (per spec) — which is the broken/truncated one.
		repaired, didRepair := repairTruncatedJSON(s)
		if didRepair {
			var r2 VerifyResult
			if err2 := json.Unmarshal([]byte(repaired), &r2); err2 == nil {
				return &r2, nil
			}
		}
		if deduped, didDedupe := keepFirstTopLevelKeys(s); didDedupe {
			var r3 VerifyResult
			if err2 := json.Unmarshal([]byte(deduped), &r3); err2 == nil {
				return &r3, nil
			}
		}
		// Combined: dedupe THEN repair (covers cases where the truncated
		// duplicate also contains an unclosed bracket).
		if didRepair {
			if deduped, didDedupe := keepFirstTopLevelKeys(repaired); didDedupe {
				var r4 VerifyResult
				if err2 := json.Unmarshal([]byte(deduped), &r4); err2 == nil {
					return &r4, nil
				}
			}
		}
		return nil, fmt.Errorf("parse verify json: %w (got %q)", err, truncate(s, 400))
	}
}

// normalizeFieldAliases rewrites known field-name typos to their canonical
// form before the JSON parse. gemma4 occasionally emits these mid-output:
//
//	"lineno"      → "line_no"
//	"observed_qy" → "observed_qty"   (rare)
//
// Plain-text substitution is fine because these don't legitimately appear
// inside string values for any prompt we send.
var fieldAliasReplacer = strings.NewReplacer(
	`"lineno"`, `"line_no"`,
	`"observed_qy"`, `"observed_qty"`,
	`"observed_unitprice"`, `"observed_unit_price"`,
)

func normalizeFieldAliases(s string) string {
	return fieldAliasReplacer.Replace(s)
}

// keepFirstTopLevelKeys re-emits a JSON object with only the FIRST occurrence
// of each top-level key. Designed for the "model is repeating itself" failure
// mode where verify output looks like:
//
//	{"lines": [...valid...], "lines": [...also valid...], "lines": "[ truncated…"
//
// json.Unmarshal uses the LAST occurrence by spec, which here is the broken
// truncated string and fails to parse into []VerifyLineResult. We want the
// FIRST, which is the model's first-and-best attempt before it spiraled.
//
// Returns (rewritten, true) when at least one duplicate was dropped;
// (s, false) when the input had no duplicates or wasn't a parseable object.
func keepFirstTopLevelKeys(s string) (string, bool) {
	dec := json.NewDecoder(strings.NewReader(s))
	tok, err := dec.Token()
	if err != nil {
		return s, false
	}
	d, ok := tok.(json.Delim)
	if !ok || d != '{' {
		return s, false
	}
	type entry struct {
		key string
		val json.RawMessage
	}
	var ordered []entry
	seen := make(map[string]bool)
	dropped := false
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return s, false
		}
		key, ok := keyTok.(string)
		if !ok {
			return s, false
		}
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return s, false
		}
		if seen[key] {
			dropped = true
			continue
		}
		seen[key] = true
		ordered = append(ordered, entry{key, val})
	}
	if !dropped {
		return s, false
	}
	var b bytes.Buffer
	b.WriteByte('{')
	for i, e := range ordered {
		if i > 0 {
			b.WriteByte(',')
		}
		kj, _ := json.Marshal(e.key)
		b.Write(kj)
		b.WriteByte(':')
		b.Write(e.val)
	}
	b.WriteByte('}')
	return b.String(), true
}

// repairTruncatedJSON tries to close an object/array left mid-string by a
// model that ran out of tokens. Heuristic: close any unterminated string,
// drop any trailing comma, then balance `{` `[` with matching closers.
// Returns (repaired, true) on success; (s, false) if it couldn't help.
//
// Designed for the specific failure mode of grammar-constrained generation
// hitting num_predict — we still got mostly-valid JSON, just no closer.
func repairTruncatedJSON(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return s, false
	}
	// Walk the string tracking string-literal mode + bracket stack.
	stack := make([]byte, 0, 16)
	inStr := false
	escaped := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			stack = append(stack, '}')
		case '[':
			stack = append(stack, ']')
		case '}', ']':
			if len(stack) > 0 && stack[len(stack)-1] == c {
				stack = stack[:len(stack)-1]
			}
		}
	}
	if !inStr && len(stack) == 0 {
		return s, false // already balanced — caller's parse error was something else
	}
	var b strings.Builder
	b.WriteString(s)
	if inStr {
		b.WriteByte('"')
	}
	// Drop a trailing comma if any, before closing brackets. Walk back over
	// whitespace + at most one comma to find the real tail.
	repaired := b.String()
	rt := strings.TrimRight(repaired, " \t\n\r,")
	if rt != repaired {
		repaired = rt
	}
	// Append closers in reverse stack order.
	var closers strings.Builder
	for i := len(stack) - 1; i >= 0; i-- {
		closers.WriteByte(stack[i])
	}
	return repaired + closers.String(), true
}

// stringNumRe matches `"field": "12.34"` style where the value is purely
// numeric. Used to salvage output from models that quote their numbers.
//
// Two parallel field families share this map:
//   - InvoiceData (open-extract):    invoice_total, qty, unit_price, extended
//   - VerifyResult (verify-vs-PO):  observed_*, invoice_total_observed, line_no
//
// Add new entries when models start quoting a numeric field we hit. Cheap
// to keep both families here; the coerce only touches matched names.
var stringNumFields = map[string]bool{
	"line_no": true, "qty": true, "unit_price": true, "extended": true,
	"observed_qty": true, "observed_unit_price": true, "observed_extended": true,
	"invoice_total": true, "invoice_total_observed": true,
}

func coerceStringNums(s string) string {
	var out strings.Builder
	out.Grow(len(s))
	i := 0
	for i < len(s) {
		if s[i] != '"' {
			out.WriteByte(s[i])
			i++
			continue
		}
		// s[i] is the opening quote of a potential name.
		nameEndRel := strings.IndexByte(s[i+1:], '"')
		if nameEndRel < 0 {
			out.WriteByte(s[i])
			i++
			continue
		}
		nameClose := i + 1 + nameEndRel // index of name's closing "
		name := s[i+1 : nameClose]
		k := nameClose + 1
		for k < len(s) && (s[k] == ' ' || s[k] == '\t' || s[k] == '\n') {
			k++
		}
		if k >= len(s) || s[k] != ':' {
			out.WriteByte(s[i])
			i++
			continue
		}
		k++ // past colon
		for k < len(s) && (s[k] == ' ' || s[k] == '\t' || s[k] == '\n') {
			k++
		}
		if k >= len(s) || s[k] != '"' || !stringNumFields[name] {
			out.WriteByte(s[i])
			i++
			continue
		}
		// k is on opening quote of value.
		valStart := k + 1
		valEndRel := strings.IndexByte(s[valStart:], '"')
		if valEndRel < 0 {
			out.WriteByte(s[i])
			i++
			continue
		}
		raw := s[valStart : valStart+valEndRel]
		val := stripCurrency(raw)
		if !isNumericLiteral(val) {
			// Field is in our numeric set but the model emitted something
			// stripCurrency can't reduce to a number (e.g. "THA", a column
			// header the model misread as a price). Emit `null` so the
			// unmarshal succeeds with the field at zero — the line stays
			// in the result, recon flags it as discrepancy because zero
			// won't match the PO. Better than dropping the whole extraction.
			//
			// Trade-off: original bad string is lost. If you need to recover
			// it for diagnostics, log at this point. Today we accept the
			// loss because the goal is "extraction completes for the rest
			// of the invoice"; the line is still surfaced as a recon issue.
			out.WriteString(s[i:k]) // `"NAME"` + colon + ws, stopping at the opening quote
			out.WriteString("null")
			i = valStart + valEndRel + 1
			continue
		}
		// Emit `"NAME": <val>` — skip both quotes around the numeric value.
		out.WriteString(s[i:k]) // `"NAME"` + colon + any whitespace, stopping at the opening quote
		out.WriteString(val)
		i = valStart + valEndRel + 1 // one past value's closing quote
	}
	return out.String()
}

// stripCurrency drops display-only decoration models add around numbers so
// "$1,234.56", "1 EA", "25.00 U.S.D.", and "1940.00000 MFT" all collapse
// to their numeric cores. Used alongside coerceStringNums for models
// (minicpm-v, gemma) that can't resist annotating values with units.
//
// Suffix list covers the units we've actually seen vendors stamp into
// invoice line items internally: MFT/M for cable (per thousand feet), LF/FT
// for linear measure, LB/KG for weight, GAL/QT/OZ for liquid, plus the
// generic count/pack indicators. The list is iterated repeatedly because
// some values come compound ("1.50 USD ROLL" → strip ROLL, then USD).
func stripCurrency(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "$")
	s = strings.TrimPrefix(s, "€")
	s = strings.TrimPrefix(s, "£")
	s = strings.TrimPrefix(s, "USD ")
	s = strings.TrimSuffix(s, "%")
	s = strings.ReplaceAll(s, ",", "")
	// Iterate the suffix list multiple times — vendors emit compound units
	// like "1.50 USD ROLL" where stripping ROLL exposes USD as a new
	// stripable suffix on the next pass. 4 passes is plenty for any
	// real-world stack.
	for pass := 0; pass < 4; pass++ {
		before := s
		lower := strings.ToLower(strings.TrimSpace(s))
		for _, suf := range stripCurrencySuffixes {
			if strings.HasSuffix(lower, suf) {
				s = strings.TrimSpace(s[:len(s)-len(suf)])
				lower = strings.ToLower(s)
			}
		}
		if s == before {
			break // no change this pass — done
		}
	}
	return strings.TrimSpace(s)
}

// stripCurrencySuffixes is the lowercase-with-leading-space suffix set
// stripCurrency removes. Order matters only insofar as longer-more-specific
// suffixes go first ("u.s.d." before "usd" so "U.S.D." doesn't leave a
// stray dot). Adding a new unit here is a cheap way to recover invoices
// that previously failed extraction with type-mismatch errors.
var stripCurrencySuffixes = []string{
	// Currency
	" u.s.d.", " usd", " cad", " eur", " gbp",
	// Counts / packaging
	" each", " ea", " pcs", " pc", " pk", " pack", " packs",
	" box", " boxes", " bag", " bags", " bdl", " bundle", " bundles",
	" ct", " count", " roll", " rolls", " set", " sets",
	" kit", " kits", " case", " cases", " carton", " cartons",
	" doz", " dozen", " gross", " jar", " jars", " can", " cans",
	// Length
	" mft", " m", " c", // M = per thousand, C = per hundred (legacy pricing)
	" lf", " linft", " lin ft", " linear ft", " ft", " in", " inch",
	" mm", " cm", " yd",
	// Weight
	" lb", " lbs", " kg", " g", " oz",
	// Volume
	" gal", " gallon", " gallons", " qt", " pt", " fl oz", " fluid oz",
	// Time / labor
	" hr", " hour", " hours", " day", " days", " mo", " month", " months",
	" yr", " year", " years",
}

func isNumericLiteral(s string) bool {
	if s == "" {
		return false
	}
	dot := false
	for i, ch := range s {
		switch {
		case ch == '-' && i == 0:
		case ch == '.' && !dot:
			dot = true
		case ch >= '0' && ch <= '9':
		default:
			return false
		}
	}
	return true
}

// ExtractPOsFromImage sends a PNG image to the vision model and returns any
// PO numbers it extracts, validated as 6-8 digit integers.
// `notes` is included for debug logging — returned even on zero matches.
func (c *Client) ExtractPOsFromImage(ctx context.Context, png []byte) (pos []int64, notes string, err error) {
	if len(png) == 0 {
		return nil, "", errors.New("empty image")
	}
	b64 := base64.StdEncoding.EncodeToString(png)

	body, _ := json.Marshal(map[string]interface{}{
		"model":  c.model,
		"prompt": visionPrompt,
		"images": []string{b64},
		"stream": false,
		"think":  false,
		"options": map[string]interface{}{
			"temperature": 0.0,
			"num_predict": 200,
		},
	})

	url := c.endpoint()
	req, err := http.NewRequestWithContext(ctx, "POST", url+"/api/generate", bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.doWithHook(req, url)
	if err != nil {
		return nil, "", fmt.Errorf("ollama vision request: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, "", fmt.Errorf("ollama vision %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var or struct {
		Response string `json:"response"`
	}
	if err := json.Unmarshal(raw, &or); err != nil {
		return nil, "", fmt.Errorf("parse ollama vision response: %w", err)
	}

	type visionOut struct {
		PONumbers []string `json:"po_numbers"`
		Notes     string   `json:"notes"`
	}
	var out visionOut
	s := strings.TrimSpace(or.Response)
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		// Model sometimes adds prefix/suffix; try to extract the first JSON object.
		if m := jsonObjectRe.FindString(s); m != "" {
			_ = json.Unmarshal([]byte(m), &out)
		}
	}

	for _, p := range out.PONumbers {
		// Strip common garnishes ("#", "PO", spaces)
		clean := strings.TrimFunc(p, func(r rune) bool { return r == '#' || r == ' ' || r == '.' })
		if n, err := strconv.ParseInt(strings.TrimSpace(clean), 10, 64); err == nil && n >= 100000 && n <= 99999999 {
			pos = append(pos, n)
		}
	}
	return pos, out.Notes, nil
}
