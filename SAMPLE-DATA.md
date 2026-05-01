# Sample Data — Observed AP Mail Types

Synthetic representatives of the kinds of mail an AP department actually
receives — drawn from a real sample, names anonymized. Used to inform the
classifier design in `PROTOTYPE.md`. The point is the *shape* of the data:
which categories exist, how often each shows up, what subject/body patterns
distinguish them.

## Sample: 10 representative threads in a single inbox-day

| # | Sender (synthetic) | Type | Notes |
|---|---|---|---|
| 1 | shipping-vendor@example.com | **Payment Confirmation** | "Thank you for your AutoPay payment" — small dollar amount. We paid *them* — record-keeping only, not an inbound invoice |
| 2 | sales-rep@plumbing-supplier.example.com | **Vendor Marketing / FYI** | Price increase announcement. Not an invoice but AP may want awareness |
| 3 | notifications@freight-vendor.example.com | **Portal Notification** | "Your bills are available online" — action is "go fetch from their portal," not direct entry |
| 4 | edocs@vendor-c.example.com | **Statement / Dunning** | Overdue notice with PDF attached |
| 5 | promo@hvac-supplier.example.com | **Vendor Marketing** | Product promo — noise for AP |
| 6 | sales-rep@electrical-vendor.example.com | **Invoice** | PDF attached, straight vendor invoice |
| 7 | sales@vendor.example.com | **Invoice** | "Invoice from Vendor XYZ for SKU C1340 PO# 1235630" — PDF attached |
| 8 | sales@vendor.example.com | **Invoice** | Same vendor, different PO (1235613) |
| 9 | invoicing@hvac-vendor.example.com | **Invoice** | Portal link + attachment |
| 10 | another internal user + internal-rep@distributor.example.com | **Internal Collaboration** | "PO 1234776 SHORT A PALLET" — purchasing/AP issue thread |

**7 distinct classification types** emerge in a 10-message sample. Scaling
that up, the classifier needs to reliably distinguish:

1. **Invoice** — needs entering in the ERP (primary to-do)
2. **Statement / Dunning** — account status, may need entering or follow-up
3. **Payment Confirmation** — file only, no action required
4. **Portal Notification** — action is in the vendor's portal, not direct entry
5. **Vendor Marketing / FYI** — mostly noise; price increases matter enough to flag
6. **Internal Collaboration** — from a known-internal sender, already in progress
7. **Vendor Communication** — catch-all when nothing else matches

## Rough pattern rules (first draft)

Subject/body tokens that reliably indicate each type:

| Type | Tokens / patterns |
|---|---|
| Payment Confirmation | `thank you for your payment` / `autopay payment` / `payment received` / `payment confirmation` |
| Statement / Dunning | `dunning` / `past due` / `overdue` / `statement of account` / `outstanding balance` |
| Portal Notification | `bills are available online` / `new invoice in portal` / `login to view` / `access your account` |
| Vendor Marketing | `price increase` / `new product` / `introducing` / `announcement` / `promotion` |
| Internal Collaboration | sender domain matches the company's domain |
| Invoice | (has attachment) AND (`invoice` in subject OR `inv #` OR `invoice attached`) |
| Vendor Communication | fallback |

Each should pair with `Vendor: <name>` from the ERP vendor lookup when the
sender matches a known vendor.

## What this sample does and doesn't tell us

The sample is enough to inform an MVP classifier — the seven types cover the
pareto of what AP actually sees. What it doesn't give:

- **Distribution by volume.** A 10-message snapshot can't say whether
  invoices are 60% or 90% of inbox-day mail. The MVP works the same either
  way; later analytics need a bigger sample.
- **Borderline cases.** A "statement" that's actually a monthly summary with
  no dunning language. A "promo" that includes pricing data the AP team
  needs. The classifier will need a calibration corpus, not just type rules.
- **Vendor-specific invoice subject formats.** Carriers we deal with each
  have their own invoice-naming conventions. Worth cataloging once the
  vendor coverage is broad enough to matter.
