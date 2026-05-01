package aiclass

import "testing"

func TestDeterministicKind(t *testing.T) {
	cases := []struct {
		name    string
		subject string
		sender  string
		body    string
		want    string
	}{
		// Statement
		{"bare statement", "Statement", "ar@vendor.example.com", "", "Statement"},
		{"account statement", "Account Statement", "ar@vendor.example.com", "", "Statement"},
		{"monthly statement", "Monthly Statement: April", "ar@vendor.example.com", "", "Statement"},
		{"statement of account", "Statement of Account - Acme Distribution", "ar@vendor.example.com", "", "Statement"},
		{"plural statements", "Statements - April", "ar@vendor.example.com", "", "Statement"},
		{"NOT bare 'statement' inside other words", "Statement of Work proposal", "x@example.com", "", ""},

		// OrderConfirmation
		{"order confirmation", "Order Confirmation - PO 12345", "noreply@vendor.example.com", "", "OrderConfirmation"},
		{"order acknowledgement (UK)", "Your order acknowledgement #SO123", "x@example.com", "", "OrderConfirmation"},
		{"order acknowledgment (US)", "Order Acknowledgment - 8076527", "x@example.com", "", "OrderConfirmation"},
		{"Generic-Brand PO+SO pattern", "Generic-Brand PO# 1235759 / SO# A000743898", "donotreply@vendor-b.example.com", "", "OrderConfirmation"},
		{"NSI order received", "Your Sample Industries order has been received!", "contact@vendor.example.com", "", "OrderConfirmation"},
		{"order tracking", "Order Tracking", "contact@vendor.example.com", "", "OrderConfirmation"},
		{"thank you for your order", "Thanks for your order", "x@example.com", "", "OrderConfirmation"},

		// Credit
		{"credit memo", "Credit Memo 12345", "x@example.com", "", "Credit"},
		{"credit note", "Credit Note - return", "x@example.com", "", "Credit"},

		// Payment
		{"remittance advice", "Remittance Advice from Acme Distribution", "ap@example.com", "", "Payment"},
		{"payment scheduled", "Your payment is scheduled", "x@example.com", "", "Payment"},
		{"payment notification", "Payment notification - check 12345", "x@example.com", "", "Payment"},

		// Webinar
		{"webinar invite", "Webinar: Q2 product roadmap", "events@vendor.example.com", "", "Webinar"},
		{"lunch & learn", "Lunch & Learn this Thursday", "x@example.com", "", "Webinar"},
		{"lunch and learn", "Lunch and Learn invite", "x@example.com", "", "Webinar"},
		{"register now", "Register now for our class", "x@example.com", "", "Webinar"},

		// Marketing
		{"marketing sender", "April catalog", "marketing@vendor.example.com", "", "Marketing"},
		{"promo sender", "this month's deals", "promotions@vendor.example.com", "", "Marketing"},
		{"news sender", "Industry update", "news@vendor.example.com", "", "Newsletter"}, // Newsletter wins on subject
		{"limited time offer", "Limited Time: Save 20%", "x@example.com", "", "Marketing"},
		{"exclusive offer subject", "Exclusive offer for our partners", "x@example.com", "", "Marketing"},

		// Newsletter
		{"newsletter subject", "April Newsletter from Acme", "x@example.com", "", "Newsletter"},
		{"monthly digest", "Monthly Digest - April", "x@example.com", "", "Newsletter"},

		// Invoice
		{"invoice with #", "Invoice #12345 from Acme", "x@example.com", "", "Invoice"},
		{"invoice no number prefix", "Invoice No. 5490000", "x@example.com", "", "Invoice"},
		{"invoice attached subject", "Invoice attached - PO 6355", "x@example.com", "", "Invoice"},
		{"please find your invoice in body", "Account update", "x@example.com", "Hi, please find your invoice attached.", "Invoice"},
		{"new invoice", "New Invoice from Vendor", "x@example.com", "", ""},  // "new invoice" alone without number doesn't fire; that's fine

		// Should NOT match (returns empty for AI fallback)
		{"vague 'Re: invoice'", "Re: invoice question", "x@example.com", "", ""},
		{"random vendor mail", "Following up", "ap@vendor.example.com", "", ""},
		{"bare PO subject", "PO 1235759", "x@example.com", "", ""},
		{"order without keyword", "your shipment is here", "x@example.com", "", ""},
		{"empty subject", "", "x@example.com", "", ""},

		// Conflict resolution: OrderConfirmation BEFORE Invoice when subject has both
		{"order conf with invoice number", "Order Confirmation - Invoice #12345", "x@example.com", "", "OrderConfirmation"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := DeterministicKind(c.subject, c.sender, c.body)
			if got != c.want {
				t.Errorf("subject=%q sender=%q body=%q\n  want: %q\n  got:  %q", c.subject, c.sender, c.body, c.want, got)
			}
		})
	}
}
