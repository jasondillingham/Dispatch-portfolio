// po-test is a throwaway CLI for validating the the ERP PO lookup path end-to-end.
// Takes one or more PO numbers on the command line, prints vendor/supplier/buyer.
//   go run ./cmd/po-test 1235759 1234904 1233546
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"

	"dispatch/internal/erp"
)

func main() {
	flag.Parse()
	args := flag.Args()
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: po-test <po_no> [<po_no> ...]")
		os.Exit(2)
	}
	c, err := erp.New("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "erp: %v\n", err)
		os.Exit(1)
	}
	defer c.Close()

	for _, a := range args {
		n, err := strconv.ParseInt(a, 10, 64)
		if err != nil {
			fmt.Printf("%s\tbad number\n", a)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		info, err := c.LookupPO(ctx, n)
		cancel()
		if err != nil {
			fmt.Printf("%d\tERROR: %v\n", n, err)
			continue
		}
		if info == nil {
			fmt.Printf("%d\tnot found\n", n)
			continue
		}
		fmt.Printf("PO %d:\n", n)
		fmt.Printf("  Vendor:   %s (id %s)\n", info.VendorName, info.VendorID)
		fmt.Printf("  Supplier: %s (id %s)\n", info.SupplierName, info.SupplierID)
		fmt.Printf("  Buyer:    %s\n", info.Buyer)
		fmt.Printf("  Date:     %s\n", info.OrderDate.Format("2006-01-02"))
		fmt.Printf("  Approved=%v  Canceled=%v\n\n", info.Approved, info.Canceled)
	}
}
