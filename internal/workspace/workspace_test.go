package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPushPullBetweenNodes(t *testing.T) {
	h := NewHub()
	if r := h.Push("alice", Change{Path: "main.go", Content: []byte("package main"), Base: 0}); !r.Applied || r.Version != 1 {
		t.Fatalf("alice push: %+v", r)
	}
	// bob, starting from empty base, pulls alice's file.
	got := h.Pull(map[string]uint64{})
	if len(got) != 1 || got[0].Path != "main.go" || string(got[0].Content) != "package main" {
		t.Fatalf("bob pull: %+v", got)
	}
}

func TestConcurrentEditConflict(t *testing.T) {
	h := NewHub()
	h.Push("alice", Change{Path: "f.go", Content: []byte("A"), Base: 0}) // v1

	// bob edited from base 0 (never saw v1) → conflict, hub unchanged.
	r := h.Push("bob", Change{Path: "f.go", Content: []byte("B"), Base: 0})
	if r.Applied || r.Conflict == nil {
		t.Fatalf("expected conflict, got %+v", r)
	}
	if r.Conflict.Hub.Version != 1 || string(r.Conflict.Hub.Content) != "A" {
		t.Fatalf("conflict should expose hub v1=A, got %+v", r.Conflict.Hub)
	}
	if cur, _ := h.Get("f.go"); string(cur.Content) != "A" {
		t.Fatal("hub must be unchanged on conflict")
	}

	// Agent reconciles → next version applied.
	if rr := h.Resolve("bob", "f.go", []byte("A+B")); !rr.Applied || rr.Version != 2 {
		t.Fatalf("resolve: %+v", rr)
	}
}

func TestLFNormalization(t *testing.T) {
	h := NewHub()
	h.Push("alice", Change{Path: "x.txt", Content: []byte("a\r\nb\r\n"), Base: 0})
	got, _ := h.Get("x.txt")
	if string(got.Content) != "a\nb\n" {
		t.Fatalf("CRLF not normalized: %q", got.Content)
	}
}

func TestIgnoreMatch(t *testing.T) {
	ig := DefaultIgnore()
	for _, p := range []string{".git/config", "node_modules/x/y.js", "a/build/out", "obj.o"} {
		if !ig.Match(p) {
			t.Errorf("%q should be ignored", p)
		}
	}
	for _, p := range []string{"src/main.go", "README.md"} {
		if ig.Match(p) {
			t.Errorf("%q should not be ignored", p)
		}
	}
}

// Round-trip a real directory through the hub: alice's tree syncs up, bob's tree syncs down,
// with .git excluded, LF-normalized, and the executable bit preserved.
func TestDirectoryRoundTrip(t *testing.T) {
	h := NewHub()
	dirA, dirB := t.TempDir(), t.TempDir()

	write(t, dirA, "src/main.go", "package main\r\n", 0o644)
	write(t, dirA, "run.sh", "#!/bin/sh\necho hi\n", 0o755)
	write(t, dirA, ".git/HEAD", "ref: refs/heads/main", 0o644) // must be ignored

	nvA := NewNodeView("alice")
	conflicts, err := nvA.SyncUp(h, dirA, DefaultIgnore())
	if err != nil || len(conflicts) != 0 {
		t.Fatalf("syncUp: err=%v conflicts=%v", err, conflicts)
	}
	if _, ok := h.Get(".git/HEAD"); ok {
		t.Fatal(".git must not be synced")
	}

	nvB := NewNodeView("bob")
	if err := nvB.SyncDown(h, dirB); err != nil {
		t.Fatalf("syncDown: %v", err)
	}
	if got := read(t, dirB, "src/main.go"); got != "package main\n" {
		t.Fatalf("main.go = %q (want LF-normalized)", got)
	}
	info, err := os.Stat(filepath.Join(dirB, "run.sh"))
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("run.sh mode = %v err=%v", info.Mode().Perm(), err)
	}
}

func write(t *testing.T, dir, rel, content string, mode os.FileMode) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(full, mode)
}

func read(t *testing.T, dir, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
