package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSessionPersistRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".avairy", "session")

	if got := readSession(path); got != "" {
		t.Fatalf("missing session file should read empty, got %q", got)
	}
	writeSession(path, "sess-123") // also creates the .avairy dir
	if got := readSession(path); got != "sess-123" {
		t.Fatalf("round-trip = %q, want sess-123", got)
	}
	// Owner-only perms (it can be a credential-ish identifier).
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("session file mode = %v err=%v", fi.Mode().Perm(), err)
	}
	// Trailing whitespace is trimmed.
	_ = os.WriteFile(path, []byte("  sess-456\n"), 0o600)
	if got := readSession(path); got != "sess-456" {
		t.Fatalf("trim = %q, want sess-456", got)
	}
}
