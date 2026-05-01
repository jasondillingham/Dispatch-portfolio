// Package blobstore is a content-addressed file store for attachment
// bytes and email bodies. The design goals:
//
//   - Dedup: same attachment sent to many messages stores only one copy.
//   - Self-sorting: no flat directory of 20k files. Blobs shard by the
//     first 2 hex chars of their sha256. By-message + by-vendor symlink
//     trees add human-browsable views.
//   - Offline-first: once a blob is on local disk, Dispatch works without
//     hitting Graph for attachment content.
//
// Layout (rooted at the path passed to New):
//
//	blobs/<ab>/<sha256>.bin        — canonical bytes, dedup source
//	by-message/YYYY/MM/DD/<msgID>/
//	    body.html body.txt metadata.json
//	    attachments/<filename>     — symlink into blobs/
//	by-vendor/<vendor-slug>/YYYY/MM/
//	    <filename>                 — symlink into blobs/
//	sync/delta-token.txt           — reserved for Phase 2 delta sync
//
// The blobstore never deletes on its own. Ref-counting lives in the
// SQLite cache (internal/cache); eviction is a future, separate concern.
package blobstore

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Store is a handle to a blobstore rooted at a local filesystem path.
// Safe for concurrent use; file ops are atomic (temp+rename) and
// directory creation is idempotent.
type Store struct {
	root string
}

// New opens (and creates if missing) a blobstore at root. The root
// should be a local mount — NFS, local disk, whatever — with enough
// free space for the mailbox volume. Creates the canonical subdirs
// up-front so callers don't need to.
func New(root string) (*Store, error) {
	if root == "" {
		return nil, errors.New("blobstore root is empty")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve root: %w", err)
	}
	for _, sub := range []string{"blobs", "by-message", "by-vendor", "sync"} {
		if err := os.MkdirAll(filepath.Join(abs, sub), 0o755); err != nil {
			return nil, fmt.Errorf("mkdir %s: %w", sub, err)
		}
	}
	return &Store{root: abs}, nil
}

// Root returns the absolute root path. Useful for logging or for the
// web layer to build static-file URLs rooted at the mount.
func (s *Store) Root() string { return s.root }

// Put writes bytes to the blobstore. Returns the sha256 hex digest and
// the on-disk path. Idempotent: if the blob already exists, no bytes
// are written (duplicate uploads are cheap). Atomic via temp+rename so
// partial writes never leave a truncated blob.
func (s *Store) Put(data []byte) (sha string, path string, err error) {
	sum := sha256.Sum256(data)
	sha = hex.EncodeToString(sum[:])
	dir := filepath.Join(s.root, "blobs", sha[:2])
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", fmt.Errorf("mkdir blob dir: %w", err)
	}
	path = filepath.Join(dir, sha+".bin")
	if _, statErr := os.Stat(path); statErr == nil {
		return sha, path, nil // already exists, dedup hit
	}
	tmp, err := os.CreateTemp(dir, "tmp-")
	if err != nil {
		return "", "", fmt.Errorf("tempfile: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", "", fmt.Errorf("write: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", "", fmt.Errorf("close: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return "", "", fmt.Errorf("rename: %w", err)
	}
	return sha, path, nil
}

// BlobPath returns the on-disk path for a given sha256. Path existence
// is not checked; callers should Stat before assuming.
//
// Validates the input is a 64-char hex string before constructing the path.
// filepath.Join already normalizes ".." segments, but the explicit check
// forecloses the entire class of path-traversal concerns regardless of how
// the path is later joined or compared. Returns "" on invalid input — every
// caller already handles "" gracefully (Stat fails, Open fails) so empty
// surfaces as "not found" rather than crashing.
func (s *Store) BlobPath(sha string) string {
	if !validSHA256(sha) {
		return ""
	}
	return filepath.Join(s.root, "blobs", sha[:2], sha+".bin")
}

// validSHA256Re matches a hex-encoded SHA-256: exactly 64 lowercase hex chars.
// blobstore exclusively writes these (see Write — sha is computed via
// hex.EncodeToString of sha256.Sum256), so anything not matching is either
// a caller bug or untrusted input we shouldn't service.
var validSHA256Re = regexp.MustCompile(`^[a-f0-9]{64}$`)

func validSHA256(s string) bool {
	return validSHA256Re.MatchString(s)
}

// Has reports whether a blob exists locally. Does not verify contents.
func (s *Store) Has(sha string) bool {
	_, err := os.Stat(s.BlobPath(sha))
	return err == nil
}

// Read returns an io.ReadCloser for the blob with the given sha, or an
// error if absent. Caller must Close.
func (s *Store) Read(sha string) (io.ReadCloser, error) {
	f, err := os.Open(s.BlobPath(sha))
	if err != nil {
		return nil, fmt.Errorf("blobstore read %s: %w", truncate(sha, 12), err)
	}
	return f, nil
}

// LinkByMessage creates the by-message symlink tree for one message +
// attachment. Safe to call repeatedly; existing symlinks are left alone.
// received: the email's received timestamp (drives the YYYY/MM/DD partition).
func (s *Store) LinkByMessage(received time.Time, messageID, filename, sha string) error {
	d := received.UTC()
	dir := filepath.Join(s.root, "by-message",
		fmt.Sprintf("%04d", d.Year()), fmt.Sprintf("%02d", d.Month()), fmt.Sprintf("%02d", d.Day()),
		sanitizeSegment(messageID), "attachments")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir by-message: %w", err)
	}
	return s.createSymlink(dir, filename, sha)
}

// LinkByVendor creates the by-vendor/YYYY/MM symlink for one attachment.
// vendorSlug should be lowercase-with-dashes (e.g. "sample-vendor-z-tools-inc").
// Empty slug is routed to "_unknown".
func (s *Store) LinkByVendor(received time.Time, vendorSlug, filename, sha string) error {
	if vendorSlug == "" {
		vendorSlug = "_unknown"
	}
	vendorSlug = sanitizeSegment(vendorSlug)
	d := received.UTC()
	dir := filepath.Join(s.root, "by-vendor", vendorSlug,
		fmt.Sprintf("%04d", d.Year()), fmt.Sprintf("%02d", d.Month()))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir by-vendor: %w", err)
	}
	return s.createSymlink(dir, filename, sha)
}

// WriteMessageBody writes the full HTML/text bodies and a metadata.json
// for one message under by-message/YYYY/MM/DD/<id>/. Overwrites existing
// files — call whenever the body is refreshed. bodyHTML OR bodyText may
// be empty; both empty is legal (writes empty files).
func (s *Store) WriteMessageBody(received time.Time, messageID string, bodyHTML, bodyText, metadataJSON string) error {
	d := received.UTC()
	dir := filepath.Join(s.root, "by-message",
		fmt.Sprintf("%04d", d.Year()), fmt.Sprintf("%02d", d.Month()), fmt.Sprintf("%02d", d.Day()),
		sanitizeSegment(messageID))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir message body dir: %w", err)
	}
	if err := writeAtomic(filepath.Join(dir, "body.html"), []byte(bodyHTML)); err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(dir, "body.txt"), []byte(bodyText)); err != nil {
		return err
	}
	if metadataJSON != "" {
		if err := writeAtomic(filepath.Join(dir, "metadata.json"), []byte(metadataJSON)); err != nil {
			return err
		}
	}
	return nil
}

// createSymlink makes a symlink at dir/filename pointing to the blob for
// sha. Idempotent: if the link already points at the same target, no-op.
// If a different target exists, replaces it.
func (s *Store) createSymlink(dir, filename, sha string) error {
	target := s.BlobPath(sha)
	rel, err := filepath.Rel(dir, target)
	if err != nil {
		rel = target // fall back to absolute
	}
	linkPath := filepath.Join(dir, sanitizeSegment(filename))
	if existing, err := os.Readlink(linkPath); err == nil && existing == rel {
		return nil // already correct
	}
	_ = os.Remove(linkPath) // replace wrong target or stale link
	return os.Symlink(rel, linkPath)
}

// writeAtomic writes data to path via a temp file + rename so a partial
// write can't leave a half-corrupt file visible to readers.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-")
	if err != nil {
		return fmt.Errorf("tempfile %s: %w", path, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// sanitizeSegment makes a path component safe: replaces / \ : * ? " < >
// and control chars with "_". Does NOT lowercase or otherwise normalize —
// caller is responsible for slug generation upstream.
func sanitizeSegment(s string) string {
	if s == "" {
		return "_"
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r < 0x20 || r == '/' || r == '\\' || r == ':' || r == '*' || r == '?' || r == '"' || r == '<' || r == '>' || r == '|' {
			b.WriteByte('_')
			continue
		}
		b.WriteRune(r)
	}
	out := b.String()
	// Collapse leading/trailing dots (Windows/macOS compatibility) and space.
	out = strings.Trim(out, ". ")
	if out == "" {
		return "_"
	}
	return out
}

// VendorSlug converts a vendor display name to a filesystem-safe slug.
// "Sample-Vendor-Z Tools, Inc." → "sample-vendor-z-tools-inc".
func VendorSlug(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		case r == ' ' || r == '-' || r == '_' || r == '.' || r == ',':
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		default:
			// drop
		}
	}
	return strings.Trim(b.String(), "-")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
