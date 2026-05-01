package blobstore

import "testing"

func TestValidSHA256(t *testing.T) {
	cases := []struct {
		in   string
		want bool
		desc string
	}{
		{"0000000000000000000000000000000000000000000000000000000000000000", true, "all zeros"},
		{"abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789", true, "lowercase hex"},
		// Real SHA-256 from Dispatch's blobstore directory.
		{"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", true, "real sha"},

		// Reject non-hex / wrong length.
		{"", false, "empty"},
		{"abc", false, "too short"},
		{"abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789a", false, "too long"},
		{"abcdef0123456789abcdef0123456789abcdef0123456789abcdef012345678X", false, "uppercase hex (we only emit lowercase)"},
		{"ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789", false, "all uppercase"},

		// Path-traversal attempts.
		{"../../../etc/passwd", false, "path traversal"},
		{"..", false, "dotdot"},
		{"abcdef0123456789abcdef0123456789abcdef0123456789abcdef012345/678", false, "slash in middle"},
		{"abc def0123456789abcdef0123456789abcdef0123456789abcdef0123456789", false, "space"},
	}
	for _, c := range cases {
		got := validSHA256(c.in)
		if got != c.want {
			t.Errorf("%s: validSHA256(%q) = %v, want %v", c.desc, c.in, got, c.want)
		}
	}
}

func TestBlobPathRejectsInvalid(t *testing.T) {
	s := &Store{root: "/tmp/blobs"}
	for _, in := range []string{"", "../etc", "abc", "deadbeef"} {
		if got := s.BlobPath(in); got != "" {
			t.Errorf("BlobPath(%q) = %q, want empty (invalid sha)", in, got)
		}
	}
	// Valid 64-hex string should produce a non-empty path.
	good := "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	if got := s.BlobPath(good); got == "" {
		t.Errorf("BlobPath(valid) returned empty unexpectedly")
	}
}
