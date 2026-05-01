// strip-categories removes Dispatch-managed categories from every message in
// a mailbox, leaving any non-Dispatch categories intact. Used before a clean
// re-run so the worker sees all messages as unprocessed.
//
// Usage:
//
//	strip-categories -mailbox ap@example.com [-dry-run] [-limit 1000]
package main

import (
	"flag"
	"fmt"
	"log"
	"strings"

	"dispatch/internal/graph"
)

// prefixes we own — anything starting with these gets stripped.
var managed = []string{
	"Vendor: ", "Status: ", "Buyer: ", "Kind: ",
	"Blocker: ", "Owner: ",
}

func isManaged(cat string) bool {
	for _, p := range managed {
		if strings.HasPrefix(cat, p) {
			return true
		}
	}
	return false
}

func main() {
	mailbox := flag.String("mailbox", "ap@example.com", "mailbox to strip")
	limit := flag.Int("limit", 1000, "max messages to process")
	dryRun := flag.Bool("dry-run", false, "print changes without sending PATCHes")
	flag.Parse()

	gc, err := graph.NewClient()
	if err != nil {
		log.Fatalf("graph: %v", err)
	}
	msgs, err := gc.ListInboxMessages(*mailbox, *limit)
	if err != nil {
		log.Fatalf("list: %v", err)
	}
	fmt.Printf("Fetched %d messages.\n", len(msgs))

	var stripped, skipped, errs int
	for _, m := range msgs {
		var kept []string
		var removed []string
		for _, c := range m.Categories {
			if isManaged(c) {
				removed = append(removed, c)
			} else {
				kept = append(kept, c)
			}
		}
		if len(removed) == 0 {
			skipped++
			continue
		}
		if kept == nil {
			kept = []string{}
		}
		fmt.Printf("  %s  %-40s  -%v  →  %v\n",
			truncate(m.ID, 20), truncate(m.Subject, 40), removed, kept)
		if *dryRun {
			stripped++
			continue
		}
		if err := gc.SetCategories(*mailbox, m.ID, kept); err != nil {
			fmt.Printf("    ERROR: %v\n", err)
			errs++
			continue
		}
		stripped++
	}
	fmt.Printf("\nstripped=%d  skipped-clean=%d  errors=%d\n", stripped, skipped, errs)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
