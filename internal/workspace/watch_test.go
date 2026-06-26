package workspace

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWatchSignalsOnChange(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.go", "package a\n", 0o644)

	ch, err := Watch(t.Context(), dir, DefaultIgnore())
	if err != nil {
		t.Fatalf("watch: %v", err)
	}

	// An edit to a watched file signals.
	write(t, dir, "a.go", "package a // edit\n", 0o644)
	if !waitSignal(ch, 3*time.Second) {
		t.Fatal("expected a signal after editing a watched file")
	}

	// A new subdirectory is auto-watched: a file created in it signals too.
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	drain(ch) // the mkdir itself may have signaled; clear it
	write(t, dir, "sub/b.go", "package b\n", 0o644)
	if !waitSignal(ch, 3*time.Second) {
		t.Fatal("expected a signal after creating a file in a new subdir")
	}
}

func TestWatchIgnoresExcludedPaths(t *testing.T) {
	dir := t.TempDir()
	ch, err := Watch(t.Context(), dir, DefaultIgnore())
	if err != nil {
		t.Fatalf("watch: %v", err)
	}

	// Writing under an ignored dir (.git) must not trigger a sync.
	write(t, dir, ".git/HEAD", "ref: x\n", 0o644)
	if waitSignal(ch, 600*time.Millisecond) {
		t.Fatal("ignored path should not signal")
	}
}

func waitSignal(ch <-chan struct{}, d time.Duration) bool {
	select {
	case <-ch:
		return true
	case <-time.After(d):
		return false
	}
}

func drain(ch <-chan struct{}) {
	for {
		select {
		case <-ch:
		case <-time.After(300 * time.Millisecond):
			return
		}
	}
}
