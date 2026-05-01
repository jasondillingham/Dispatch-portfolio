// text_extract.go — text-layer-first extraction path.
//
// When a PDF has an embedded text layer (vendor-generated invoices — the vast
// majority), we'd rather parse that than rasterize + run a vision model. Same
// extraction target, same prompt structure, just text input instead of an
// image. These methods mirror ExtractInvoiceData / VerifyAgainstPO so the
// worker can try them first and fall through to the vision variants only
// when the PDF is a scanned image.

package aiclass

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// maxTextBytes caps how much extracted PDF text we send to the model. Ollama
// contexts are limited and a long multi-page invoice could blow past them;
// 32k chars ~ 8k tokens covers all real-world invoices we've seen.
const maxTextBytes = 32 * 1024

const invoiceTextPrompt = `You are extracting structured data from invoice text that was pulled from a PDF. The text preserves physical layout with multiple spaces between columns.

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
- invoice_total is the FINAL amount the vendor wants paid. Look carefully for ANY of: "Invoice Total", "Grand Total", "Total Due", "Amount Due", "Balance Due", "Net Amount", "Total", "TOTAL", "Net Invoice Amount", "Please Pay", "Remit". Include tax and freight if applicable. Use the largest sensible dollar figure near the bottom of the invoice if no explicit total label exists. Never leave this as 0 when there are line items — a missing total is a failure worth trying harder for.
- Omit header rows, total rows, tax rows, freight rows, and subtotal rows from lines[].
- If a field truly cannot be determined, use null (not guessed values). Do NOT use 0 as a placeholder.

Invoice text:
`

// ExtractInvoiceDataFromText mirrors ExtractInvoiceData but reads from the
// PDF's text layer instead of a rasterized image. Returns ErrNoTextPath when
// caller should fall back to the vision path (empty/too-short text).
func (c *Client) ExtractInvoiceDataFromText(ctx context.Context, text string) (*InvoiceData, error) {
	text = strings.TrimSpace(text)
	if len(text) < 50 {
		return nil, ErrNoTextPath
	}
	if len(text) > maxTextBytes {
		text = text[:maxTextBytes]
	}

	prompt := invoiceTextPrompt + text
	body, _ := json.Marshal(map[string]interface{}{
		"model":  c.model,
		"prompt": prompt,
		"stream": false,
		"think":  false,
		"format": "json",
		"options": map[string]interface{}{
			"temperature": 0.0,
			"num_predict": 1500,
		},
	})

	s, err := c.postAndReadJSON(ctx, body, "invoice-text")
	if err != nil {
		return nil, err
	}
	var d InvoiceData
	if err := json.Unmarshal([]byte(s), &d); err != nil {
		return nil, fmt.Errorf("parse invoice-text json: %w (got %q)", err, truncate(s, 300))
	}
	// Fallback: if the model left invoice_total at 0 but we have line items
	// with extended amounts, sum them. Common failure mode with small text
	// models that find line items but miss the summary. Clerk will see a
	// matching total on the UI instead of a flagged-as-zero row.
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

// VerifyAgainstPOFromText mirrors VerifyAgainstPO using PDF text instead of
// a rendered image. Same prompt, same output shape.
func (c *Client) VerifyAgainstPOFromText(ctx context.Context, text string, expected []VerifyLineExpected) (*VerifyResult, error) {
	text = strings.TrimSpace(text)
	if len(text) < 50 {
		return nil, ErrNoTextPath
	}
	if len(expected) == 0 {
		return nil, errors.New("no expected lines supplied")
	}
	if len(text) > maxTextBytes {
		text = text[:maxTextBytes]
	}

	var sb strings.Builder
	sb.WriteString(verifyPromptHeader)
	for _, e := range expected {
		fmt.Fprintf(&sb, "%d. %s (%s) — qty %g @ $%.4f each, extended $%.2f\n",
			e.LineNo, e.ItemID, e.Description, e.Qty, e.UnitPrice, e.Extended)
	}
	sb.WriteString("\nInvoice text (layout-preserved from PDF):\n")
	sb.WriteString(text)

	body, _ := json.Marshal(map[string]interface{}{
		"model":  c.model,
		"prompt": sb.String(),
		"stream": false,
		"think":  false,
		"format": "json",
		"options": map[string]interface{}{
			"temperature": 0.0,
			"num_predict": 2000,
		},
	})

	s, err := c.postAndReadJSON(ctx, body, "verify-text")
	if err != nil {
		return nil, err
	}
	var result VerifyResult
	if err := json.Unmarshal([]byte(s), &result); err != nil {
		return nil, fmt.Errorf("parse verify-text json: %w (got %q)", err, truncate(s, 400))
	}
	return &result, nil
}

// ErrNoTextPath signals that a text-based extraction attempt was skipped
// because there wasn't enough text to work with. Callers should fall through
// to the vision path.
var ErrNoTextPath = errors.New("insufficient text for text-path extraction")

// postAndReadJSON is a small helper consolidating the POST+fence-strip+coerce
// dance shared by both text methods (and already used in the image methods).
// Returned string is the cleaned JSON body ready to unmarshal.
func (c *Client) postAndReadJSON(ctx context.Context, body []byte, kind string) (string, error) {
	url := c.endpoint()
	req, err := http.NewRequestWithContext(ctx, "POST", url+"/api/generate", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.doWithHook(req, url)
	if err != nil {
		return "", fmt.Errorf("ollama %s request: %w", kind, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("ollama %s %d: %s", kind, resp.StatusCode, truncate(string(raw), 200))
	}
	var or struct {
		Response string `json:"response"`
	}
	if err := json.Unmarshal(raw, &or); err != nil {
		return "", fmt.Errorf("parse ollama %s response: %w", kind, err)
	}
	s := strings.TrimSpace(or.Response)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```JSON")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)
	s = coerceStringNums(s)
	return s, nil
}
