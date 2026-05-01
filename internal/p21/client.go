// Package p21 is a thin MSSQL client for Dispatch's P21 lookups (PO number
// → vendor, item ID → supplier, etc). Read-only. Queries are narrow and
// targeted — we are NOT a general P21 data layer, just enough to answer
// "who is this invoice for" from an email's contents.
package p21

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	_ "github.com/microsoft/go-mssqldb"
)

type Config struct {
	Database struct {
		Server   string `json:"server"`
		User     string `json:"user"`
		Password string `json:"password"`
		Database string `json:"database"`
		Options  struct {
			Encrypt                bool `json:"encrypt"`
			TrustServerCertificate bool `json:"trustServerCertificate"`
		} `json:"options"`
	} `json:"database"`
}

type Client struct {
	db *sql.DB

	// AP-user list cache. ListAPUsers hits this before going to SQL.
	apUsersMu     sync.Mutex
	apUsersCache  []APUser
	apUsersCached time.Time
}

// APUser is a P21 user with one of the AP role tags. Returned by ListAPUsers
// for the impersonation dropdown.
type APUser struct {
	ID    string // canonical P21 id (e.g., "DSOWELL"); we lowercase for filter compares
	Name  string // display name (e.g., "an AP clerk Sowell")
	Email string
	Role  string // "AP Clerk" or "AP Leader"
}

// FirstName returns the leading word of Name. Convenience for the dropdown
// label which is space-tight; falls back to ID when Name is empty.
func (u APUser) FirstName() string {
	if u.Name == "" {
		return u.ID
	}
	if i := strings.IndexByte(u.Name, ' '); i > 0 {
		return u.Name[:i]
	}
	return u.Name
}

// New connects to P21 using the config file at path (or the default search
// locations if path is ""). Default search order:
//  1. $P21_MSSQL_CONFIG
//  2. ../configs/mssql_config.json  (workspace convention)
//  3. ../../configs/mssql_config.json
func New(path string) (*Client, error) {
	if path == "" {
		path = os.Getenv("P21_MSSQL_CONFIG")
	}
	if path == "" {
		candidates := []string{
			"../configs/mssql_config.json",
			"../../configs/mssql_config.json",
		}
		for _, c := range candidates {
			if _, err := os.Stat(c); err == nil {
				path = c
				break
			}
		}
	}
	if path == "" {
		return nil, fmt.Errorf("mssql_config.json not found (set P21_MSSQL_CONFIG)")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	q := url.Values{}
	q.Set("database", cfg.Database.Database)
	q.Set("encrypt", boolStr(cfg.Database.Options.Encrypt))
	// Internal P21 uses a self-signed cert (CN: SSL_Self_Signed_Fallback).
	// Force trust — we're only connecting to the internal corporate network and we
	// still want encryption on the wire; just don't validate the cert chain.
	q.Set("trustservercertificate", "true")
	u := &url.URL{
		Scheme:   "sqlserver",
		User:     url.UserPassword(cfg.Database.User, cfg.Database.Password),
		Host:     cfg.Database.Server,
		RawQuery: q.Encode(),
	}
	db, err := sql.Open("sqlserver", u.String())
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	// Pool sized for the worker's parallel paths: 8 sort + 4 extract +
	// 3 fallback workers can all hit P21 concurrently (PO lookups, voucher
	// status checks, AP-user list refreshes). At 4 max-open the pool would
	// queue under load and block on connection acquisition; 12 gives room
	// for full worker concurrency plus the web's voucher-sync goroutine.
	db.SetMaxOpenConns(12)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(30 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("ping %s: %w", cfg.Database.Server, err)
	}
	return &Client{db: db}, nil
}

func (c *Client) Close() error { return c.db.Close() }

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// POInfo is the subset of po_hdr (+ joined names) that Dispatch needs for
// auto-tagging an invoice. All fields populated on a successful lookup.
type POInfo struct {
	PONo         int64
	VendorID     string
	VendorName   string
	SupplierID   string
	SupplierName string
	Buyer        string // po_hdr.created_by, uppercased by convention
	OrderDate    time.Time
	Approved     bool
	Canceled     bool
}

// POLine is a snapshot of po_line joined with inv_mast, capturing just
// the fields Dispatch needs for reconciliation against an AI-read invoice.
type POLine struct {
	LineNo      int     `json:"line_no"`
	ItemID      string  `json:"item_id"`      // canonical SKU (e.g., "DEL T14235-BL")
	Description string  `json:"description"`  // po_line.item_description
	QtyOrdered  float64 `json:"qty_ordered"`
	UnitPrice   float64 `json:"unit_price"`
	Extended    float64 `json:"extended"` // qty * unit_price, computed here
}

// ListPOLines returns all lines on a PO, ordered by line_no.
// Returns empty slice (not nil) if the PO has no lines (or doesn't exist).
func (c *Client) ListPOLines(ctx context.Context, poNo int64) ([]POLine, error) {
	const q = `
SELECT pl.line_no, im.item_id, pl.item_description, pl.qty_ordered, pl.unit_price
FROM po_line pl WITH (NOLOCK)
LEFT JOIN inv_mast im WITH (NOLOCK) ON im.inv_mast_uid = pl.inv_mast_uid
WHERE pl.po_no = @p1
ORDER BY pl.line_no
`
	rows, err := c.db.QueryContext(ctx, q, poNo)
	if err != nil {
		return nil, fmt.Errorf("list po lines %d: %w", poNo, err)
	}
	defer rows.Close()

	out := []POLine{}
	for rows.Next() {
		var (
			lineNo     int
			itemID     sql.NullString
			itemDesc   sql.NullString
			qtyOrdered float64
			unitPrice  float64
		)
		if err := rows.Scan(&lineNo, &itemID, &itemDesc, &qtyOrdered, &unitPrice); err != nil {
			return nil, err
		}
		out = append(out, POLine{
			LineNo:      lineNo,
			ItemID:      itemID.String,
			Description: itemDesc.String,
			QtyOrdered:  qtyOrdered,
			UnitPrice:   unitPrice,
			Extended:    qtyOrdered * unitPrice,
		})
	}
	return out, rows.Err()
}

// LookupPO returns info for a PO number, or (nil, nil) if not found.
// Canceled or not-approved POs are still returned — the caller decides how
// strict to be; for invoice classification we accept any matching PO because
// mismatches (invoice for a canceled PO) are exactly the case AP needs to see.
func (c *Client) LookupPO(ctx context.Context, poNo int64) (*POInfo, error) {
	const q = `
SELECT TOP 1
  ph.po_no, ph.vendor_id, v.vendor_name,
  ph.supplier_id, s.supplier_name,
  ph.created_by, ph.order_date, ph.approved, ph.cancel_flag
FROM po_hdr ph WITH (NOLOCK)
LEFT JOIN vendor v WITH (NOLOCK) ON v.vendor_id = ph.vendor_id
LEFT JOIN supplier s WITH (NOLOCK) ON s.supplier_id = ph.supplier_id
WHERE ph.po_no = @p1
`
	var (
		info              POInfo
		supplierID        sql.NullString
		supplierName      sql.NullString
		vendorName        sql.NullString
		buyer             sql.NullString
		orderDate         sql.NullTime
		approved, cancel  sql.NullString
	)
	row := c.db.QueryRowContext(ctx, q, poNo)
	if err := row.Scan(
		&info.PONo, &info.VendorID, &vendorName,
		&supplierID, &supplierName,
		&buyer, &orderDate, &approved, &cancel,
	); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("lookup po %d: %w", poNo, err)
	}
	info.VendorName = vendorName.String
	info.SupplierID = supplierID.String
	info.SupplierName = supplierName.String
	info.Buyer = buyer.String
	info.OrderDate = orderDate.Time
	info.Approved = approved.String == "Y"
	info.Canceled = cancel.String == "Y"
	return &info, nil
}

// APInvoice is a row out of apinv_hdr for the voucher-tracking sync. Returned
// by LookupAPInvoice. paid_in_full='Y' → Status="paid"; else "posted".
type APInvoice struct {
	VoucherNo     string
	InvoiceNo     string    // vendor's invoice number (our supplier_invoice_no)
	PONo          string    // apinv_hdr.po_no is varchar(50), not always numeric
	InvoiceDate   time.Time
	InvoiceAmount float64
	AmountPaid    float64
	PaidInFull    bool
	CheckNo       string
	CheckDate     time.Time
	Approved      bool
}

// LookupAPInvoice finds an apinv_hdr row by (po_no, invoice_no). Uses both
// since invoice numbers aren't globally unique — same vendor can reuse an
// invoice_no against different POs. Returns (nil, nil) when no match — that's
// the "unposted" signal for Dispatch.
func (c *Client) LookupAPInvoice(ctx context.Context, poNo int64, invoiceNo string) (*APInvoice, error) {
	invoiceNo = strings.TrimSpace(invoiceNo)
	if poNo <= 0 || invoiceNo == "" {
		return nil, nil
	}
	const q = `
SELECT TOP 1
  voucher_no, invoice_no, po_no, invoice_date, invoice_amount, amount_paid,
  paid_in_full, check_no, check_date, approved
FROM apinv_hdr WITH (NOLOCK)
WHERE po_no = @p1 AND invoice_no = @p2
ORDER BY date_created DESC
`
	var (
		ap       APInvoice
		poStr    sql.NullString
		invDate  sql.NullTime
		paidFull sql.NullString
		checkNo  sql.NullString
		checkDt  sql.NullTime
		approved sql.NullString
	)
	row := c.db.QueryRowContext(ctx, q, fmt.Sprintf("%d", poNo), invoiceNo)
	if err := row.Scan(
		&ap.VoucherNo, &ap.InvoiceNo, &poStr, &invDate, &ap.InvoiceAmount, &ap.AmountPaid,
		&paidFull, &checkNo, &checkDt, &approved,
	); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("lookup ap invoice po=%d inv=%q: %w", poNo, invoiceNo, err)
	}
	ap.PONo = strings.TrimSpace(poStr.String)
	ap.VoucherNo = strings.TrimSpace(ap.VoucherNo)
	if invDate.Valid {
		ap.InvoiceDate = invDate.Time
	}
	ap.PaidInFull = paidFull.String == "Y"
	ap.CheckNo = strings.TrimSpace(checkNo.String)
	if checkDt.Valid {
		ap.CheckDate = checkDt.Time
	}
	ap.Approved = approved.String == "Y"
	return &ap, nil
}

// LookupUserEmail returns the email_address from the P21 users table for a
// given login id. Used by the "Ask buyer" preview to populate the To: line.
// Returns ("", nil) when the user isn't found rather than an error — the UI
// just shows "no email on file" in that case.
func (c *Client) LookupUserEmail(ctx context.Context, userID string) (string, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return "", nil
	}
	var email sql.NullString
	err := c.db.QueryRowContext(ctx, `
		SELECT email_address FROM users WITH (NOLOCK)
		WHERE delete_flag = 'N' AND LOWER(id) = LOWER(@p1)
	`, userID).Scan(&email)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("lookup user email %q: %w", userID, err)
	}
	return strings.TrimSpace(email.String), nil
}

// apUserCacheTTL is how long ListAPUsers reuses a result before re-hitting
// P21. New AP hires show up within this window.
const apUserCacheTTL = 5 * time.Minute

// ListAPUsers returns active AP Clerk + AP Leader users from P21. Joins
// users → roles via role_uid. Filters delete_flag='N'; doesn't filter
// active='N' because at this site the active flag isn't reliably maintained
// (all current AP clerks have active='N' but are working). 5-min in-memory
// cache keeps page loads off the SQL hot path; new hires appear within TTL.
func (c *Client) ListAPUsers(ctx context.Context) ([]APUser, error) {
	c.apUsersMu.Lock()
	if c.apUsersCache != nil && time.Since(c.apUsersCached) < apUserCacheTTL {
		out := append([]APUser(nil), c.apUsersCache...)
		c.apUsersMu.Unlock()
		return out, nil
	}
	c.apUsersMu.Unlock()

	const q = `
SELECT u.id, u.name, COALESCE(u.email_address, ''), r.role
FROM users u WITH (NOLOCK)
JOIN roles r WITH (NOLOCK) ON r.role_uid = u.role_uid
WHERE u.delete_flag = 'N'
  AND r.delete_flag = 'N'
  AND r.role IN ('AP Clerk','AP Leader')
ORDER BY r.role, u.name
`
	rows, err := c.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list AP users: %w", err)
	}
	defer rows.Close()
	out := []APUser{}
	for rows.Next() {
		var u APUser
		if err := rows.Scan(&u.ID, &u.Name, &u.Email, &u.Role); err != nil {
			return nil, err
		}
		u.ID = strings.TrimSpace(u.ID)
		u.Name = strings.TrimSpace(u.Name)
		u.Email = strings.TrimSpace(u.Email)
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	c.apUsersMu.Lock()
	c.apUsersCache = append([]APUser(nil), out...)
	c.apUsersCached = time.Now()
	c.apUsersMu.Unlock()
	return out, nil
}
