package main

import (
	"fmt"
	"os"
	"regexp"

	"dispatch/internal/pdftext"
)

func main() {
	f, _ := os.Open(os.Args[1])
	defer f.Close()
	text, _ := pdftext.Extract(f)

	// Same regex as poscan
	re := regexp.MustCompile(`(?i)\b(?:P\.?\s*O\.?|purchase\s+order|customer\s+po|cust\s*\.?\s*po|order)[^\d]{0,40}(\d{6,8})\b`)
	matches := re.FindAllStringSubmatch(text, -1)
	fmt.Printf("Total matches: %d\n", len(matches))
	for _, m := range matches {
		fmt.Printf("  %q  → %q\n", m[0], m[1])
	}
}
