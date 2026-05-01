// category-test validates that our Graph app registration has Mail.ReadWrite.Shared
// (or equivalent) on ap@example.com by:
//
//  1. Fetching the most recent message
//  2. PATCHing a marker category onto it (preserving any existing categories)
//  3. Re-reading the message to confirm the category is now present
//  4. PATCHing again to restore the original category list (no lingering test tags)
//
// Output: PASS means worker can write Vendor/Owner/Status/Blocker categories.
// FAIL with HTTP 403 means we need an Entra admin to grant Mail.ReadWrite.Shared.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"dispatch/internal/graph"
)

const testCategory = "DispatchTest-SafeToDelete"

func main() {
	mailbox := flag.String("mailbox", "ap@example.com", "mailbox to test against")
	flag.Parse()

	gc, err := graph.NewClient()
	if err != nil {
		log.Fatalf("graph client: %v", err)
	}

	fmt.Printf("1. Fetching newest message from %s ...\n", *mailbox)
	msgs, err := gc.ListInboxMessages(*mailbox, 1)
	if err != nil {
		fatalf("list failed: %v", err)
	}
	if len(msgs) == 0 {
		fatalf("mailbox is empty — nothing to test against")
	}
	m := msgs[0]
	original := append([]string{}, m.Categories...)
	fmt.Printf("   id=%s\n", shorten(m.ID, 40))
	fmt.Printf("   subject=%q\n", truncate(m.Subject, 60))
	fmt.Printf("   current categories=%v\n\n", original)

	target := append([]string{}, original...)
	target = append(target, testCategory)

	fmt.Printf("2. PATCHing categories=%v ...\n", target)
	if err := gc.SetCategories(*mailbox, m.ID, target); err != nil {
		fatalf("PATCH failed: %v", err)
	}
	fmt.Println("   PATCH succeeded.\n")

	fmt.Println("3. Re-fetching message to verify category is present ...")
	refreshed, err := gc.GetMessage(*mailbox, m.ID)
	if err != nil {
		fatalf("re-fetch failed: %v", err)
	}
	fmt.Printf("   categories=%v\n", refreshed.Categories)
	if !contains(refreshed.Categories, testCategory) {
		fatalf("expected %q in categories, not found", testCategory)
	}
	fmt.Println("   verified.\n")

	fmt.Printf("4. Restoring original categories=%v ...\n", original)
	if err := gc.SetCategories(*mailbox, m.ID, original); err != nil {
		fatalf("cleanup PATCH failed: %v", err)
	}
	fmt.Println("   cleanup succeeded.\n")

	fmt.Println("=== PASS ===")
	fmt.Println("Graph app has category write permission on", *mailbox)
	fmt.Println("Ready to build the worker.")
}

func fatalf(format string, a ...any) {
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "=== FAIL ===")
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "If this was a 403, the Entra app registration needs Mail.ReadWrite.Shared")
	fmt.Fprintln(os.Stderr, "(Application permission) and admin consent. the phishing filter's app registration")
	fmt.Fprintln(os.Stderr, "probably only has Mail.Read / Mail.ReadWrite — check Entra → App registrations")
	fmt.Fprintln(os.Stderr, "→ <your Graph app> → API permissions.")
	os.Exit(1)
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func shorten(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n/2] + "…" + s[len(s)-n/2:]
}
