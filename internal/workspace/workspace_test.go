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

// An unchanged re-push must not bump the version (no perpetual re-sync).
func TestPushIdempotent(t *testing.T) {
	h := NewHub()
	r1 := h.Push("a", Change{Path: "f", Content: []byte("x"), Base: 0})
	r2 := h.Push("a", Change{Path: "f", Content: []byte("x"), Base: r1.Version}) // identical
	if r2.Version != r1.Version {
		t.Fatalf("unchanged re-push bumped version %d→%d", r1.Version, r2.Version)
	}
	r3 := h.Push("a", Change{Path: "f", Content: []byte("y"), Base: r1.Version}) // changed
	if r3.Version != r1.Version+1 {
		t.Fatalf("changed push should bump, got %d", r3.Version)
	}
}

// ScanChanges reads a file once, then skips it (stat-only) while unchanged.
func TestScanChangesSkipsUnchanged(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.go", "x", 0o644)
	stamps := make(Stamps)
	changed, stampOf, _, _ := ScanChanges(dir, DefaultIgnore(), stamps)
	if len(changed) != 1 {
		t.Fatalf("first scan should see the file, got %d", len(changed))
	}
	for p, s := range stampOf {
		stamps[p] = s // simulate accepted push
	}
	if changed, _, _, _ = ScanChanges(dir, DefaultIgnore(), stamps); len(changed) != 0 {
		t.Fatalf("unchanged file should be skipped, got %d", len(changed))
	}
}

// Repeated SyncUp on an unchanged tree doesn't churn hub versions.
func TestSyncUpStableWhenIdle(t *testing.T) {
	h := NewHub()
	dir := t.TempDir()
	write(t, dir, "a.go", "package a\n", 0o644)
	nv := NewNodeView("n")
	if _, err := nv.SyncUp(h, dir, DefaultIgnore()); err != nil {
		t.Fatal(err)
	}
	v1, _ := h.Get("a.go")
	for i := 0; i < 3; i++ {
		nv.SyncUp(h, dir, DefaultIgnore())
	}
	v2, _ := h.Get("a.go")
	if v1.Version != v2.Version {
		t.Fatalf("idle SyncUp churned version %d→%d", v1.Version, v2.Version)
	}
}
