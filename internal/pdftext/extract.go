// Package pdftext extracts text from PDF streams. Delegates to poppler's
// pdftotext via exec because the pure-Go libraries we tried (ledongthuc/pdf,
// rsc.io/pdf) choke on real-world invoice PDFs with common features like
// deflate-compressed streams or PDF form fields.
//
// Dependency: `pdftotext` (from poppler-utils) must be on PATH. Ubuntu:
// `apt install poppler-utils`. macOS: `brew install poppler`. Already present
// on ai-03 for the AI fallback path's PDF→image conversion step.
package pdftext

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// MaxSize caps PDFs we'll try to parse. Bigger usually means scanned images
// with little extractable text.
const MaxSize = 16 * 1024 * 1024 // 16 MB

// ExtractTimeout bounds pdftotext exec time. Well-formed invoices finish in
// <100ms; 5s is generous enough to handle multi-page docs without hanging
// the worker if a file is pathological.
const ExtractTimeout = 5 * time.Second

// ErrEmptyText signals a structurally valid PDF that yielded little/no text,
// i.e. an image PDF. The AI fallback path should take over when this is returned.
var ErrEmptyText = errors.New("pdf contained no extractable text (likely scanned image)")

// minTextChars is the threshold below which we call it an image PDF.
// Invoices with real text usually extract hundreds to thousands of chars.
const minTextChars = 50

// ConvertFirstPagePNG rasterizes page 1 of a PDF to PNG bytes via pdftoppm.
// Used as the handoff to vision models when text extraction returns ErrEmptyText.
// 150 dpi matches what we tested on ai-03 with gemma4:e4b — enough
// resolution for PO numbers without producing unnecessarily huge images.
//
// Implementation: pdftoppm doesn't reliably support stdin→stdout, so we
// write the PDF to a temp file, run pdftoppm with a temp prefix, then read
// back the generated PNG. Files are cleaned up regardless of outcome.
func ConvertFirstPagePNG(pdfBytes []byte) ([]byte, error) {
	if len(pdfBytes) == 0 {
		return nil, errors.New("empty pdf")
	}
	ctx, cancel := context.WithTimeout(context.Background(), ExtractTimeout)
	defer cancel()

	tmpPDF, err := os.CreateTemp("", "dispatch-pdf-*.pdf")
	if err != nil {
		return nil, fmt.Errorf("tempfile: %w", err)
	}
	defer os.Remove(tmpPDF.Name())
	if _, err := tmpPDF.Write(pdfBytes); err != nil {
		tmpPDF.Close()
		return nil, fmt.Errorf("write tempfile: %w", err)
	}
	tmpPDF.Close()

	// pdftoppm -singlefile writes "<prefix>.png" (no page suffix).
	prefix := tmpPDF.Name() + "-out"
	outPath := prefix + ".png"
	defer os.Remove(outPath)

	cmd := exec.CommandContext(ctx, "pdftoppm",
		"-r", "150", "-png", "-f", "1", "-l", "1", "-singlefile",
		tmpPDF.Name(), prefix)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("pdftoppm: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}
	png, err := os.ReadFile(outPath)
	if err != nil {
		return nil, fmt.Errorf("read pdftoppm output: %w", err)
	}
	if len(png) == 0 {
		return nil, errors.New("pdftoppm produced empty png")
	}
	return png, nil
}

// Extract reads a PDF from r and returns the concatenated text via pdftotext.
// The PDF bytes are piped to stdin, text comes back on stdout.
func Extract(r io.Reader) (string, error) {
	raw, err := io.ReadAll(io.LimitReader(r, MaxSize+1))
	if err != nil {
		return "", fmt.Errorf("read pdf: %w", err)
	}
	if len(raw) > MaxSize {
		return "", fmt.Errorf("pdf larger than %d bytes; skipping", MaxSize)
	}

	ctx, cancel := context.WithTimeout(context.Background(), ExtractTimeout)
	defer cancel()

	// -q: quiet (suppress errors to stderr; we still get them in Stderr buffer)
	// -enc UTF-8: consistent encoding
	// -layout: preserve physical layout (helps with tabular invoice data)
	// input/output "-" = stdin/stdout
	cmd := exec.CommandContext(ctx, "pdftotext", "-q", "-enc", "UTF-8", "-layout", "-", "-")
	cmd.Stdin = bytes.NewReader(raw)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	// pdftotext exits non-zero on many recoverable errors ("Syntax Error: Can't
	// get Fields array" etc.) while still producing usable output. Only treat
	// as failure if we got nothing on stdout.
	if stdout.Len() == 0 {
		if err != nil {
			return "", fmt.Errorf("pdftotext failed: %w (%s)", err, strings.TrimSpace(stderr.String()))
		}
		return "", ErrEmptyText
	}
	out := stdout.String()
	if len(strings.TrimSpace(out)) < minTextChars {
		return "", ErrEmptyText
	}
	return out, nil
}
