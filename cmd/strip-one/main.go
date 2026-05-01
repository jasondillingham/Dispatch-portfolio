// strip-one removes Dispatch-managed categories from a single message by ID.
// Used to force-rerun one specific message through the worker after a
// regex/classifier fix without touching the rest of the mailbox. Deletes on
// demand once this surgical reprocess is done.
package main

import (
	"flag"
	"fmt"
	"log"
	"strings"

	"dispatch/internal/graph"
)

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
	mailbox := flag.String("mailbox", "ap@example.com", "mailbox")
	msgID := flag.String("id", "", "exact message ID to strip")
	flag.Parse()
	if *msgID == "" {
		log.Fatalf("provide -id <messageID>")
	}
	gc, err := graph.NewClient()
	if err != nil {
		log.Fatalf("graph: %v", err)
	}
	m, err := gc.GetMessage(*mailbox, *msgID)
	if err != nil {
		log.Fatalf("get message: %v", err)
	}
	fmt.Printf("subject: %s\n", m.Subject)
	fmt.Printf("current categories: %v\n", m.Categories)
	kept := make([]string, 0, len(m.Categories))
	for _, c := range m.Categories {
		if !isManaged(c) {
			kept = append(kept, c)
		}
	}
	fmt.Printf("writing: %v\n", kept)
	if err := gc.SetCategories(*mailbox, *msgID, kept); err != nil {
		log.Fatalf("set categories: %v", err)
	}
	fmt.Println("OK")
}
