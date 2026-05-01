// verify-test benchmarks the new VerifyAgainstPO vision prompt against the
// existing open-ended ExtractInvoiceData on real cached invoices.
//
// Usage:
//   verify-test -po 1235306
//   verify-test -message <graph-id>
//
// Prints: expected PO lines (P21 truth), AI extraction result, AI verification
// result, and a brief accuracy summary.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"dispatch/internal/aiclass"
	"dispatch/internal/cache"
	"dispatch/internal/graph"
	"dispatch/internal/p21"
	"dispatch/internal/pdftext"
)

func main() {
	poFlag := flag.Int64("po", 0, "PO number (looks up matching message in cache)")
	msgFlag := flag.String("message", "", "specific Graph message ID (bypasses cache lookup)")
	modelFlag := flag.String("model", "gemma4:e4b", "Ollama model (try e4b vs 26b to compare)")
	urlFlag := flag.String("url", "", "Ollama server URL (default: aiclass.DefaultURL)")
	mailboxFlag := flag.String("mailbox", "ap@example.com", "mailbox")
	flag.Parse()

	if *poFlag == 0 && *msgFlag == "" {
		fmt.Fprintln(os.Stderr, "provide -po or -message")
		os.Exit(2)
	}

	c, err := cache.Open("")
	must(err)
	defer c.Close()
	gc, err := graph.NewClient()
	must(err)
	p21c, err := p21.New("")
	must(err)
	defer p21c.Close()

	ai := aiclass.NewClient(*urlFlag, *modelFlag)

	// Find the right message.
	msgID := *msgFlag
	var poNo int64 = *poFlag
	ctx := context.Background()
	if msgID == "" {
		row, err := findCachedMessageByPO(c, *mailboxFlag, poNo)
		must(err)
		if row == "" {
			log.Fatalf("no cached message for PO %d", poNo)
		}
		msgID = row
	}
	if poNo == 0 {
		if ext, err := c.GetInvoiceExtraction(ctx, *mailboxFlag, msgID); err == nil && ext != nil {
			poNo = ext.PONo
		}
	}
	if poNo == 0 {
		log.Fatalf("could not determine PO for message %s", msgID)
	}

	// Ground truth: P21 PO lines.
	ctxP21, cancelP21 := context.WithTimeout(ctx, 10*time.Second)
	poLines, err := p21c.ListPOLines(ctxP21, poNo)
	cancelP21()
	must(err)

	fmt.Printf("=== PO %d ground truth (%d lines from P21) ===\n", poNo, len(poLines))
	for _, l := range poLines {
		fmt.Printf("  %2d  %-25s  qty %5g  $%8.4f  ext $%10.2f  %s\n",
			l.LineNo, l.ItemID, l.QtyOrdered, l.UnitPrice, l.Extended, l.Description)
	}
	fmt.Println()

	// Fetch PDF attachment.
	atts, err := gc.ListAttachments(*mailboxFlag, msgID)
	must(err)
	var pdfBytes []byte
	for _, a := range atts {
		lowerName := strings.ToLower(a.Name)
		lowerCT := strings.ToLower(a.ContentType)
		isPDF := strings.Contains(lowerCT, "pdf") || strings.HasSuffix(lowerName, ".pdf")
		if a.IsInline || !isPDF {
			continue
		}
		rdr, _, _, err := gc.FetchAttachmentContent(*mailboxFlag, msgID, a.ID)
		must(err)
		b, _ := io.ReadAll(io.LimitReader(rdr, pdftext.MaxSize+1))
		rdr.Close()
		if len(b) > 0 {
			pdfBytes = b
			fmt.Printf("Using attachment: %s (%d bytes)\n\n", a.Name, len(b))
			break
		}
	}
	if pdfBytes == nil {
		log.Fatal("no PDF attachment found")
	}

	png, err := pdftext.ConvertFirstPagePNG(pdfBytes)
	must(err)

	// Approach B: verify against PO.
	expected := make([]aiclass.VerifyLineExpected, 0, len(poLines))
	for _, l := range poLines {
		expected = append(expected, aiclass.VerifyLineExpected{
			LineNo: l.LineNo, ItemID: l.ItemID, Description: l.Description,
			Qty: l.QtyOrdered, UnitPrice: l.UnitPrice, Extended: l.Extended,
		})
	}

	fmt.Printf("=== Approach B: VerifyAgainstPO (model=%s) ===\n", *modelFlag)
	vCtx, vCancel := context.WithTimeout(ctx, 15*time.Minute)
	start := time.Now()
	result, err := ai.VerifyAgainstPO(vCtx, png, expected)
	vCancel()
	bDur := time.Since(start)
	if err != nil {
		fmt.Printf("  error: %v\n", err)
	} else {
		fmt.Printf("  completed in %s\n\n", bDur.Round(time.Second))
		matches, diffs, notFound := 0, 0, 0
		for _, lr := range result.Lines {
			glyph := "?"
			switch lr.Status {
			case "match":
				glyph = "✓"
				matches++
			case "differs":
				glyph = "≠"
				diffs++
			case "not_found":
				glyph = "—"
				notFound++
			}
			fmt.Printf("  %s line %d: %s", glyph, lr.LineNo, lr.Status)
			if lr.Status == "differs" {
				fmt.Printf("  (observed qty=%g unit=$%.4f ext=$%.2f)",
					lr.ObservedQty, lr.ObservedUnitPrice, lr.ObservedExtended)
			}
			if lr.Note != "" {
				fmt.Printf("  — %s", lr.Note)
			}
			fmt.Println()
		}
		fmt.Printf("\n  Summary: %d match / %d differs / %d not found\n", matches, diffs, notFound)
		if len(result.UnexpectedLines) > 0 {
			fmt.Printf("  Unexpected lines on invoice (freight/tax/other): %d\n", len(result.UnexpectedLines))
			for _, u := range result.UnexpectedLines {
				fmt.Printf("    %s — qty %g @ $%.2f = $%.2f\n", u.Description, u.Qty, u.UnitPrice, u.Extended)
			}
		}
		fmt.Printf("  Invoice total (AI-read): $%.2f\n", result.InvoiceTotalObserved)
	}
}

func findCachedMessageByPO(c *cache.Cache, mailbox string, poNo int64) (string, error) {
	// Quick read via the existing extraction table — not the cleanest API but
	// sufficient for the test script.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	msgs, err := c.ListMessages(ctx, mailbox, 500)
	if err != nil {
		return "", err
	}
	for _, m := range msgs {
		ext, _ := c.GetInvoiceExtraction(ctx, mailbox, m.ID)
		if ext != nil && ext.PONo == poNo {
			return m.ID, nil
		}
	}
	return "", nil
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
