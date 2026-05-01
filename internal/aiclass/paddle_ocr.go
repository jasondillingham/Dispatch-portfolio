// paddle_ocr.go — OCR-specialized transcription via PaddleOCR-VL.
//
// PaddleOCR-VL (0.9B) is purpose-built for document OCR. Unlike general VL
// models (minicpm-v, gemma4), we don't ask it to produce structured JSON —
// we just get back a clean text transcription of the image. The worker
// then hands that transcription to ExtractInvoiceDataFromText or
// VerifyAgainstPOFromText, reusing the same text-path extraction prompts
// that work on pdftotext output. Paddle slots between tier 1 (pdftotext)
// and tier 3 (general vision models) in the extraction cascade.

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
	"strings"
)

// ocrPrompt instructs Paddle to transcribe the image. Phrasing matters:
// "transcribe" prompts Paddle's OCR mode; asking reasoning-style questions
// ("extract the invoice total") pushes it into VL mode which it's NOT
// optimized for at 0.9B.
const ocrPrompt = "Transcribe all visible text from this document. Preserve the layout, tables, and line breaks as printed."

// OCRTranscribe sends a PNG to Paddle and returns the extracted text.
// This is a low-level primitive — callers feed the resulting text into
// ExtractInvoiceDataFromText / VerifyAgainstPOFromText.
func (c *Client) OCRTranscribe(ctx context.Context, png []byte) (string, error) {
	if len(png) == 0 {
		return "", errors.New("empty image")
	}
	b64 := base64.StdEncoding.EncodeToString(png)

	body, _ := json.Marshal(map[string]interface{}{
		"model":  c.model,
		"prompt": ocrPrompt,
		"images": []string{b64},
		"stream": false,
		"think":  false,
		// No format:json — we want plain text, not JSON wrapping.
		"options": map[string]interface{}{
			"temperature": 0.0,
			"num_predict": 4000, // full invoice transcriptions can be long
		},
	})

	url := c.endpoint()
	req, err := http.NewRequestWithContext(ctx, "POST", url+"/api/generate", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.doWithHook(req, url)
	if err != nil {
		return "", fmt.Errorf("paddle-ocr request: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("paddle-ocr %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var or struct {
		Response string `json:"response"`
	}
	if err := json.Unmarshal(raw, &or); err != nil {
		return "", fmt.Errorf("parse paddle-ocr response: %w", err)
	}
	return strings.TrimSpace(or.Response), nil
}
