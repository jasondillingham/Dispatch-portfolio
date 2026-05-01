// classify-untagged is a one-shot that runs the AI text classifier against
// every cached message that doesn't already have a Kind: tag, then PATCHes
// the resulting Kind (and Status: Done for non-actionable kinds) back to
// Outlook + the local cache.
//
// Why this exists: messages with a clear PO + attachment skip the AI sort
// pool entirely, so they never get a Kind tag. After 1400+ such messages
// piled up, the admin Doc column reads "—" for ~80% of rows. This fills
// the gap in one bulk pass without re-running the expensive vision
// extraction pipeline.
//
// Usage:
//
//	classify-untagged -mailbox ap@example.com [-dry-run] [-limit N] [-concurrency 4]
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"

	"dispatch/internal/aiclass"
	"dispatch/internal/cache"
	"dispatch/internal/graph"
)

const (
	statusCatPrefix = "Status: "
	kindCatPrefix   = "Kind: "
)

func main() {
	mailbox := flag.String("mailbox", "ap@example.com", "mailbox to classify")
	cachePath := flag.String("cache", "", "SQLite cache path (default: ~/.dispatch/cache.db)")
	aiURL := flag.String("ai-url", aiclass.DefaultURL, "Ollama URL for text classification")
	aiModel := flag.String("ai-model", aiclass.DefaultModel, "Ollama model")
	concurrency := flag.Int("concurrency", 2, "parallel classifier calls (keep low to share GPU with worker)")
	limit := flag.Int("limit", 0, "max messages to process (0 = all)")
	dryRun := flag.Bool("dry-run", true, "print actions without writing categories (default true — pass -dry-run=false to actually write)")
	flag.Parse()

	if *cachePath == "" {
		home, _ := os.UserHomeDir()
		*cachePath = filepath.Join(home, ".dispatch", "cache.db")
	}

	cdb, err := cache.Open(*cachePath)
	if err != nil {
		log.Fatalf("open cache: %v", err)
	}
	defer cdb.Close()

	// Open a second read-only handle to scan candidates. Cache doesn't
	// expose a raw DB, so we open our own sql.DB read-only just for the
	// SELECT — writes go through cdb.UpdateCategories.
	roDB, err := sql.Open("sqlite", *cachePath+"?mode=ro")
	if err != nil {
		log.Fatalf("open ro: %v", err)
	}
	defer roDB.Close()

	gc, err := graph.NewClient()
	if err != nil {
		log.Fatalf("graph: %v", err)
	}

	aiC := aiclass.NewClient(*aiURL, *aiModel)
	log.Printf("classifier: %s @ %s", *aiModel, *aiURL)
	log.Printf("dry-run: %v · concurrency: %d · limit: %d", *dryRun, *concurrency, *limit)

	candidates, err := loadCandidates(roDB, *mailbox, *limit)
	if err != nil {
		log.Fatalf("load: %v", err)
	}
	log.Printf("loaded %d untagged messages", len(candidates))

	type job struct {
		ID, Subject, Sender, Preview string
		ExistingCats                 []string
	}

	jobs := make(chan job, *concurrency*2)
	var wg sync.WaitGroup
	var classified, written, errs, skipped int64
	start := time.Now()

	for i := 0; i < *concurrency; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for j := range jobs {
				ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
				cls, err := aiC.Classify(ctx, j.Subject, j.Sender, j.Preview)
				cancel()
				if err != nil {
					atomic.AddInt64(&errs, 1)
					log.Printf("[w%d] err %s: %v", worker, truncate(j.Subject, 40), err)
					continue
				}
				atomic.AddInt64(&classified, 1)
				kind := strings.TrimSpace(cls.Kind)
				if kind == "" || kind == "Other" {
					atomic.AddInt64(&skipped, 1)
					continue
				}
				newCats := layerCategories(j.ExistingCats, kind, cls.Actionable)
				if *dryRun {
					log.Printf("[w%d dry] %s · From %s → Kind:%s actionable=%v", worker, truncate(j.Subject, 50), j.Sender, kind, cls.Actionable)
					continue
				}
				if err := gc.SetCategories(*mailbox, j.ID, newCats); err != nil {
					atomic.AddInt64(&errs, 1)
					log.Printf("[w%d] graph err %s: %v", worker, truncate(j.ID, 30), err)
					continue
				}
				ucCtx, ucCancel := context.WithTimeout(context.Background(), 5*time.Second)
				if err := cdb.UpdateCategories(ucCtx, *mailbox, j.ID, newCats); err != nil {
					log.Printf("[w%d] cache err %s: %v (graph PATCH already succeeded)", worker, truncate(j.ID, 30), err)
				}
				ucCancel()
				w := atomic.AddInt64(&written, 1)
				if w%25 == 0 {
					rate := float64(w) / time.Since(start).Seconds()
					remaining := float64(len(candidates))-(float64(w+skipped+errs))
					eta := time.Duration(remaining/rate) * time.Second
					log.Printf("progress: %d written, %d skipped, %d errs · %.1f/s · ETA %v", w, atomic.LoadInt64(&skipped), atomic.LoadInt64(&errs), rate, eta.Round(time.Second))
				}
			}
		}(i + 1)
	}

	for _, c := range candidates {
		jobs <- job{ID: c.ID, Subject: c.Subject, Sender: c.Sender, Preview: c.Preview, ExistingCats: c.Cats}
	}
	close(jobs)
	wg.Wait()

	log.Printf("done in %v", time.Since(start).Round(time.Second))
	log.Printf("classified=%d  written=%d  skipped(empty/Other)=%d  errs=%d", classified, written, skipped, errs)
}

type candidate struct {
	ID, Subject, Sender, Preview string
	Cats                         []string
}

func loadCandidates(db *sql.DB, mailbox string, limit int) ([]candidate, error) {
	q := `
		SELECT m.id, COALESCE(m.subject,''), COALESCE(m.sender_email,''), COALESCE(m.body_preview,''), COALESCE(m.categories_json,'[]')
		FROM messages m
		WHERE m.mailbox = ?
		  AND (m.categories_json IS NULL OR m.categories_json NOT LIKE '%Kind: %')
		ORDER BY m.received_at DESC
	`
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := db.Query(q, mailbox)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []candidate
	for rows.Next() {
		var c candidate
		var catsJSON string
		if err := rows.Scan(&c.ID, &c.Subject, &c.Sender, &c.Preview, &catsJSON); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(catsJSON), &c.Cats)
		out = append(out, c)
	}
	return out, rows.Err()
}

// layerCategories merges Kind:<kind> (and Status:Done if non-actionable) into
// the existing category list. Preserves all non-Kind/non-Status entries; if
// a Kind: entry already exists it gets replaced; same for Status: when the
// classification is non-actionable.
func layerCategories(existing []string, kind string, actionable bool) []string {
	out := make([]string, 0, len(existing)+2)
	hasKind := false
	hasStatus := false
	for _, c := range existing {
		switch {
		case strings.HasPrefix(c, kindCatPrefix):
			out = append(out, kindCatPrefix+kind)
			hasKind = true
		case !actionable && strings.HasPrefix(c, statusCatPrefix):
			out = append(out, statusCatPrefix+"Done")
			hasStatus = true
		default:
			out = append(out, c)
		}
	}
	if !hasKind {
		out = append(out, kindCatPrefix+kind)
	}
	if !actionable && !hasStatus {
		out = append(out, statusCatPrefix+"Done")
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
