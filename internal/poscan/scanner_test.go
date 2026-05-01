package poscan

import (
	"reflect"
	"testing"
)

func TestExtractPOs(t *testing.T) {
	cases := []struct {
		name string
		text string
		want []int64
	}{
		{"subject with PO#", "Generic-Brand PO# 1235759 / SO# A000743898", []int64{1235759}},
		{"internal reply re prefix", "Re: 1233546 Sample-Vendor-Z", nil}, // bare number, no keyword
		{"lowercase po", "Payment for po 1234904 received", []int64{1234904}},
		{"P.O. with dots and space", "re: P.O. 1234904 status", []int64{1234904}},
		{"P.O.# tight", "P.O.#1221741 Sample-HVAC follow up", []int64{1221741}},
		{"purchase order phrase", "Your purchase order 1233323 shipped", []int64{1233323}},
		{"multiple POs, dedup", "Re: PO 1234904 - also PO# 1234904 and PO 1235759", []int64{1234904, 1235759}},
		{"too short", "PO 12345 reminder", nil}, // 5 digits, below threshold
		{"too long", "PO 123456789 reminder", nil},
		{"credit card fragment", "your card ending 1234567", nil},
		{"order keyword", "Order #1232675 sample-vendor parts", []int64{1232675}},
		// Regression: Sample Industries-style column layout. Customer P.O header
		// on one row, value on the next row — many non-digit chars between.
		// Requires [^\d]{0,400} window in poRe. The other numbers (842924
		// "PO Box", 588281 "Sales Order") show up here because they match
		// PO-ish keywords too; P21 validation downstream filters the false
		// positives. This test just asserts we CAPTURE the real PO as a
		// candidate.
		{"columnar invoice layout", `Remit To: Sample Industries, LLC
PO Box 842924                                      Sales Order #SO588281
Anytown, ST 99999-9999

         Customer P.O                   Ship Via                       Tracking Num                              Terms
          1234507                SampleShipper Ground            380328555706                     2% 10th Net 25th 2nd Month`,
			[]int64{842924, 588281, 1234507}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractPOs(5, tc.text)
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ExtractPOs(%q) = %v, want %v", tc.text, got, tc.want)
			}
		})
	}
}
