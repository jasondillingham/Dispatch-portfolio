package cache

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

// openTempCache opens a brand-new SQLite cache in t.TempDir() and runs all
// migrations through schema_migrations bookkeeping. Returns a cache that
// already has the v3 clerk_verdicts table.
func openTempCache(t *testing.T) *Cache {
	t.Helper()
	c, err := Open(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestMigration_V3_Applied confirms the v3 row lands in schema_migrations
// after Open(). Guards against accidental reordering or skipping.
func TestMigration_V3_Applied(t *testing.T) {
	c := openTempCache(t)
	var v int
	if err := c.db.QueryRow(`SELECT version FROM schema_migrations WHERE version = 3`).Scan(&v); err != nil {
		t.Fatalf("v3 not recorded: %v", err)
	}
	if v != 3 {
		t.Fatalf("expected v=3, got %d", v)
	}
	// Table exists and is queryable.
	var n int
	if err := c.db.QueryRow(`SELECT COUNT(*) FROM clerk_verdicts`).Scan(&n); err != nil {
		t.Fatalf("clerk_verdicts not queryable: %v", err)
	}
}

// TestRecordVerdict_AppendsRow records one verdict and confirms it round-trips
// via ListVerdictsByMessage.
func TestRecordVerdict_AppendsRow(t *testing.T) {
	c := openTempCache(t)
	ctx := context.Background()
	mb, msg := "ap@example.com", "msg-1"

	if err := c.RecordVerdict(ctx, mb, msg, "ap-clerk", "right", ""); err != nil {
		t.Fatalf("RecordVerdict: %v", err)
	}
	got, err := c.ListVerdictsByMessage(ctx, mb, msg)
	if err != nil {
		t.Fatalf("ListVerdictsByMessage: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 verdict, got %d", len(got))
	}
	if got[0].Verdict != "right" || got[0].User != "ap-clerk" {
		t.Fatalf("unexpected verdict shape: %+v", got[0])
	}
	if got[0].CorrectedData != "" {
		t.Fatalf("expected empty corrected_data, got %q", got[0].CorrectedData)
	}
	if got[0].UID == 0 {
		t.Fatalf("expected nonzero UID")
	}
}

// TestRecordVerdict_AppendOnly records three verdicts on the same message
// and confirms List returns all three newest-first. No edit/replace path.
func TestRecordVerdict_AppendOnly(t *testing.T) {
	c := openTempCache(t)
	ctx := context.Background()
	mb, msg := "ap@example.com", "msg-2"

	for _, v := range []string{"right", "wrong", "corrected"} {
		cd := ""
		if v == "corrected" {
			cd = `{"po_number":"1234507"}`
		}
		if err := c.RecordVerdict(ctx, mb, msg, "ap-clerk", v, cd); err != nil {
			t.Fatalf("RecordVerdict %s: %v", v, err)
		}
		// Sleep a tick so created_at ordering is deterministic. SQLite stamps
		// at sub-second precision but the receiver uses time.Now().UTC() —
		// rapid inserts in the same nanosecond would otherwise tie on sort.
		time.Sleep(2 * time.Millisecond)
	}

	got, err := c.ListVerdictsByMessage(ctx, mb, msg)
	if err != nil {
		t.Fatalf("ListVerdictsByMessage: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 verdicts, got %d", len(got))
	}
	// Newest first: corrected, wrong, right.
	want := []string{"corrected", "wrong", "right"}
	for i, w := range want {
		if got[i].Verdict != w {
			t.Errorf("position %d: expected %s, got %s", i, w, got[i].Verdict)
		}
	}
	if got[0].CorrectedData == "" {
		t.Errorf("expected corrected_data populated on the corrected entry")
	}
}

// TestListVerdictsByMessage_EmptyResult: a message with no verdicts returns
// an empty slice (not nil) and no error. Guards templates that range over
// the result without nil-checking.
func TestListVerdictsByMessage_EmptyResult(t *testing.T) {
	c := openTempCache(t)
	got, err := c.ListVerdictsByMessage(context.Background(), "ap@example.com", "missing")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got == nil {
		t.Fatalf("expected non-nil empty slice")
	}
	if len(got) != 0 {
		t.Fatalf("expected len 0, got %d", len(got))
	}
}

// TestVerdictCountsByVendor_AggregatesAndOrders seeds messages with three
// vendors and a mix of verdicts; confirms aggregation + ordering by
// Wrong+Corrected DESC, ties broken by Total DESC, then Vendor ASC.
func TestVerdictCountsByVendor_AggregatesAndOrders(t *testing.T) {
	c := openTempCache(t)
	ctx := context.Background()
	mb := "ap@example.com"
	now := time.Now().UTC()

	// Seed messages.categories_json so VerdictCountsByVendor can JOIN.
	seedMsg := func(id, vendor string) {
		cats, _ := json.Marshal([]string{"Vendor: " + vendor, "Status: New"})
		if _, err := c.db.ExecContext(ctx, `
			INSERT INTO messages (mailbox, id, received_at, categories_json, last_synced_at)
			VALUES (?, ?, ?, ?, ?)
		`, mb, id, now, string(cats), now); err != nil {
			t.Fatalf("seed message %s: %v", id, err)
		}
	}
	seedMsg("m-globex-1", "Globex")
	seedMsg("m-globex-2", "Globex")
	seedMsg("m-vendor-b-1", "Vendor-B")
	seedMsg("m-vendor-b-2", "Vendor-B")
	seedMsg("m-vendor-b-3", "Vendor-B")
	seedMsg("m-vendor-c-1", "Vendor-C")

	// Globex: 2 wrong, 0 right (Wrong=2, Total=2)
	if err := c.RecordVerdict(ctx, mb, "m-globex-1", "u", "wrong", ""); err != nil {
		t.Fatal(err)
	}
	if err := c.RecordVerdict(ctx, mb, "m-globex-2", "u", "corrected", `{"po_number":"1"}`); err != nil {
		t.Fatal(err)
	}
	// Vendor-B: 2 wrong, 1 right (Wrong=2, Total=3) — tied with Globex on Wrong, more Total
	if err := c.RecordVerdict(ctx, mb, "m-vendor-b-1", "u", "wrong", ""); err != nil {
		t.Fatal(err)
	}
	if err := c.RecordVerdict(ctx, mb, "m-vendor-b-2", "u", "wrong", ""); err != nil {
		t.Fatal(err)
	}
	if err := c.RecordVerdict(ctx, mb, "m-vendor-b-3", "u", "right", ""); err != nil {
		t.Fatal(err)
	}
	// Vendor-C: 0 wrong, 1 right (Wrong=0)
	if err := c.RecordVerdict(ctx, mb, "m-vendor-c-1", "u", "right", ""); err != nil {
		t.Fatal(err)
	}

	got, err := c.VerdictCountsByVendor(ctx, mb, now.Add(-time.Hour), 0)
	if err != nil {
		t.Fatalf("VerdictCountsByVendor: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 vendors, got %d: %+v", len(got), got)
	}
	// Sort: Wrong DESC (Vendor-B=2, Globex=2 tie), then Total DESC (Vendor-B=3 > Globex=2),
	// then Vendor ASC. Fastenal last (Wrong=0).
	if got[0].Vendor != "Vendor-B" {
		t.Errorf("expected Vendor-B first (more total), got %s", got[0].Vendor)
	}
	if got[1].Vendor != "Globex" {
		t.Errorf("expected Globex second, got %s", got[1].Vendor)
	}
	if got[2].Vendor != "Vendor-C" {
		t.Errorf("expected Vendor-C third, got %s", got[2].Vendor)
	}
	// Disagree rates: Globex=1.0, Vendor-B=0.666..., Vendor-C=0.0
	if got[1].DisagreeRate != 1.0 {
		t.Errorf("Globex disagree rate: expected 1.0, got %v", got[1].DisagreeRate)
	}
	if got[2].DisagreeRate != 0.0 {
		t.Errorf("Vendor-C disagree rate: expected 0.0, got %v", got[2].DisagreeRate)
	}

	// Limit caps results.
	gotLim, err := c.VerdictCountsByVendor(ctx, mb, now.Add(-time.Hour), 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotLim) != 2 {
		t.Errorf("limit=2: expected 2 rows, got %d", len(gotLim))
	}
}

// TestVerdictCountsByVendor_RespectsSinceWindow confirms verdicts older than
// `since` are excluded from the aggregate.
func TestVerdictCountsByVendor_RespectsSinceWindow(t *testing.T) {
	c := openTempCache(t)
	ctx := context.Background()
	mb := "ap@example.com"
	now := time.Now().UTC()

	cats, _ := json.Marshal([]string{"Vendor: Acme"})
	if _, err := c.db.ExecContext(ctx, `
		INSERT INTO messages (mailbox, id, received_at, categories_json, last_synced_at)
		VALUES (?, ?, ?, ?, ?)
	`, mb, "m-1", now, string(cats), now); err != nil {
		t.Fatal(err)
	}

	// Insert a verdict, then back-date it 60 days.
	if err := c.RecordVerdict(ctx, mb, "m-1", "u", "wrong", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.ExecContext(ctx, `UPDATE clerk_verdicts SET created_at = ? WHERE message_id = ?`,
		now.Add(-60*24*time.Hour), "m-1"); err != nil {
		t.Fatal(err)
	}

	got, err := c.VerdictCountsByVendor(ctx, mb, now.Add(-30*24*time.Hour), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 results (verdict outside window), got %d: %+v", len(got), got)
	}
}
