// pdf-test extracts text from a local PDF file and runs our PO regex against it.
//   go run ./cmd/pdf-test /tmp/test-invoice.pdf
package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"dispatch/internal/pdftext"
	"dispatch/internal/poscan"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: pdf-test <file.pdf>")
		os.Exit(2)
	}
	f, err := os.Open(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer f.Close()

	start := time.Now()
	text, err := pdftext.Extract(f)
	dur := time.Since(start)
	if err != nil {
		fmt.Printf("error: %v  (elapsed: %s)\n", err, dur)
		os.Exit(1)
	}
	fmt.Printf("Extracted %d chars in %s\n", len(text), dur)
	if len(text) > 300 {
		fmt.Printf("Preview: %q...\n", text[:300])
	} else {
		fmt.Printf("Text: %q\n", text)
	}
	pos := poscan.ExtractPOs(10, text)
	fmt.Printf("PO numbers found: %v\n", pos)
	fmt.Println("(lines containing 'PO' or 'order' for inspection):")
	for _, line := range strings.Split(text, "\n") {
		l := strings.ToLower(line)
		if strings.Contains(l, "po") || strings.Contains(l, "order") {
			fmt.Printf("  | %s\n", strings.TrimSpace(line))
		}
	}
}
