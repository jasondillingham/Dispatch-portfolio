// reality-check fetches recent messages from a mailbox, classifies each sender
// (internal/relay/logistics/bank/vendor), then runs vendor-class senders through
// the P21 resolver. Reports coverage in two layers so internal chatter and
// automation relays don't dilute the vendor-match number.
//
// Usage:
//   reality-check [-mailbox EMAIL] [-limit N]
//
// Defaults: ap@example.com, 500 messages.
package main

import (
	"encoding/csv"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"

	"dispatch/internal/graph"
	"dispatch/internal/vendors"
)

func main() {
	mailbox := flag.String("mailbox", "ap@example.com", "mailbox to inspect")
	limit := flag.Int("limit", 500, "max messages to fetch (newest first)")
	emailsPath := flag.String("emails", "data/vendor_emails.json", "path to vendor_emails.json")
	domainsPath := flag.String("domains", "data/vendor_domains.json", "path to vendor_domains.json")
	unmatchedOut := flag.String("unmatched-out", "unmatched.csv", "CSV path for unmatched vendor-class senders")
	flag.Parse()

	resolver, err := vendors.Load(*emailsPath, *domainsPath)
	if err != nil {
		log.Fatalf("load resolver: %v", err)
	}
	nEmails, nDomains, nAmbig := resolver.Stats()
	fmt.Printf("Resolver loaded: %d emails, %d unambiguous domains, %d ambiguous domains, %d name tokens\n\n",
		nEmails, nDomains, nAmbig, resolver.NameTokenCount())

	gc, err := graph.NewClient()
	if err != nil {
		log.Fatalf("graph client: %v", err)
	}
	fmt.Printf("Fetching up to %d messages from %s ...\n", *limit, *mailbox)
	msgs, err := gc.ListInboxMessages(*mailbox, *limit)
	if err != nil {
		log.Fatalf("list messages: %v", err)
	}
	fmt.Printf("Got %d messages.\n\n", len(msgs))

	// First pass: classify every sender.
	classCounts := map[vendors.SenderClass]int{}
	vendorMsgs := []graph.Message{}
	internalSenders := map[string]int{}
	relaySenders := map[string]int{}
	logisticsSenders := map[string]int{}
	bankSenders := map[string]int{}

	for _, m := range msgs {
		sender := m.SenderAddress()
		if sender == "" {
			classCounts["empty"]++
			continue
		}
		class := vendors.Classify(sender)
		classCounts[class]++
		switch class {
		case vendors.ClassVendor:
			vendorMsgs = append(vendorMsgs, m)
		case vendors.ClassInternal:
			internalSenders[strings.ToLower(sender)]++
		case vendors.ClassRelay:
			relaySenders[strings.ToLower(sender)]++
		case vendors.ClassLogistics:
			logisticsSenders[strings.ToLower(sender)]++
		case vendors.ClassBank:
			bankSenders[strings.ToLower(sender)]++
		}
	}

	fmt.Println("=== Sender class breakdown ===")
	fmt.Printf("  Total messages:   %d\n", len(msgs))
	fmt.Printf("  Internal:         %d  (AP clerks, internal forwards)\n", classCounts[vendors.ClassInternal])
	fmt.Printf("  Relay:            %d  (Intuit, DocuSign, PandaDoc — vendor hidden in body)\n", classCounts[vendors.ClassRelay])
	fmt.Printf("  Logistics:        %d  (Carrier-A, Carrier-B, freight carriers)\n", classCounts[vendors.ClassLogistics])
	fmt.Printf("  Bank:             %d  (payment-confirmation mail)\n", classCounts[vendors.ClassBank])
	fmt.Printf("  Vendor (candidate): %d\n", classCounts[vendors.ClassVendor])
	fmt.Printf("  Empty sender:     %d\n\n", classCounts["empty"])

	// Second pass: vendor coverage, vendor-class only.
	var exact, domain, stem, name, unknown, ambigFallthrough int
	unmatchedCounts := map[string]int{}
	ambigCounts := map[string]int{}
	stemExamples := map[string]string{}
	nameExamples := map[string]string{}

	for _, m := range vendorMsgs {
		sender := m.SenderAddress()
		match := resolver.Resolve(sender)
		switch match.Type {
		case vendors.MatchExact:
			exact++
		case vendors.MatchDomain:
			domain++
		case vendors.MatchStem:
			stem++
			stemExamples[strings.ToLower(sender)] = match.Vendor.VendorName
		case vendors.MatchName:
			name++
			nameExamples[strings.ToLower(sender)] = match.Vendor.VendorName
		default:
			unknown++
			if isAmbig, _ := resolver.IsAmbiguousDomain(sender); isAmbig {
				ambigFallthrough++
				at := strings.LastIndex(sender, "@")
				if at >= 0 {
					ambigCounts[strings.ToLower(sender[at+1:])]++
				}
			}
			unmatchedCounts[strings.ToLower(sender)]++
		}
	}

	vTotal := len(vendorMsgs)
	fmt.Println("=== Vendor coverage (vendor-class only) ===")
	fmt.Printf("  Vendor-class messages:  %d\n", vTotal)
	if vTotal > 0 {
		pct := func(n int) float64 { return 100 * float64(n) / float64(vTotal) }
		fmt.Printf("  Exact email match:     %4d  (%.1f%%)\n", exact, pct(exact))
		fmt.Printf("  Domain match:          %4d  (%.1f%%)\n", domain, pct(domain))
		fmt.Printf("  Stem match (stripped subdomain): %4d  (%.1f%%)\n", stem, pct(stem))
		fmt.Printf("  Name match (domain label → vendor name): %4d  (%.1f%%)\n", name, pct(name))
		fmt.Printf("  Unknown:               %4d  (%.1f%%)\n", unknown, pct(unknown))
		fmt.Printf("    of which domain is known-but-ambiguous: %d  (%.1f%%)\n",
			ambigFallthrough, pct(ambigFallthrough))
	}
	fmt.Println()

	if len(stemExamples) > 0 {
		fmt.Println("=== Stem-match hits ===")
		for s, v := range stemExamples {
			fmt.Printf("  %-40s → %s\n", s, v)
		}
		fmt.Println()
	}
	if len(nameExamples) > 0 {
		fmt.Println("=== Name-match hits ===")
		for s, v := range nameExamples {
			fmt.Printf("  %-40s → %s\n", s, v)
		}
		fmt.Println()
	}

	fmt.Println("=== Top unmatched vendor-class senders ===")
	printTop(unmatchedCounts, 15)
	fmt.Println()

	if len(ambigCounts) > 0 {
		fmt.Println("=== Ambiguous-domain hits in vendor-class mail ===")
		fmt.Println("(domain maps to multiple vendors — resolver deliberately returns Unknown)")
		printTop(ambigCounts, 10)
		fmt.Println()
	}

	if len(relaySenders) > 0 {
		fmt.Println("=== Relay senders seen (extract vendor from body instead) ===")
		printTop(relaySenders, 8)
		fmt.Println()
	}

	if err := writeUnmatchedCSV(*unmatchedOut, unmatchedCounts); err != nil {
		log.Fatalf("write csv: %v", err)
	}
	fmt.Printf("Wrote %s with %d unique unmatched senders (vendor-class only).\n", *unmatchedOut, len(unmatchedCounts))
}

func printTop(counts map[string]int, n int) {
	type kv struct {
		K string
		V int
	}
	entries := make([]kv, 0, len(counts))
	for k, v := range counts {
		entries = append(entries, kv{k, v})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].V != entries[j].V {
			return entries[i].V > entries[j].V
		}
		return entries[i].K < entries[j].K
	})
	if len(entries) < n {
		n = len(entries)
	}
	for i := 0; i < n; i++ {
		fmt.Printf("  %4d  %s\n", entries[i].V, entries[i].K)
	}
}

func writeUnmatchedCSV(path string, counts map[string]int) error {
	type row struct {
		Sender string
		Count  int
	}
	rows := make([]row, 0, len(counts))
	for k, v := range counts {
		rows = append(rows, row{k, v})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Count != rows[j].Count {
			return rows[i].Count > rows[j].Count
		}
		return rows[i].Sender < rows[j].Sender
	})
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	defer w.Flush()
	if err := w.Write([]string{"sender", "count"}); err != nil {
		return err
	}
	for _, r := range rows {
		if err := w.Write([]string{r.Sender, fmt.Sprintf("%d", r.Count)}); err != nil {
			return err
		}
	}
	return nil
}
