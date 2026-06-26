package workspace

import (
	"os"
	"path/filepath"
	"testing"
	"time"
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

// IgnoreFor layers the project's .gitignore / .dockerignore / .avairyignore (parsed with
// their real syntax) on top of the built-in baseline.
func TestIgnoreForReadsProjectFiles(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, ".gitignore", "*.log\n/secret/\n!keep.log\n", 0o644)
	write(t, dir, ".dockerignore", "tmp\n**/*.tmpcache\n", 0o644)
	write(t, dir, ".avairyignore", "scratch/\n", 0o644)

	ig := IgnoreFor(dir)
	for _, p := range []string{"app.log", "secret/key", "tmp/x", "a/b/c.tmpcache", "scratch/notes.md", "obj.o"} {
		if !ig.Match(p) {
			t.Errorf("%q should be ignored", p)
		}
	}
	for _, p := range []string{"keep.log", "src/main.go", "README.md"} {
		if ig.Match(p) {
			t.Errorf("%q should not be ignored", p)
		}
	}
}

// Symlinks replicate as links (not as the dereferenced file), and a retargeted link re-syncs.
func TestSymlinkSync(t *testing.T) {
	h := NewHub()
	dirA, dirB := t.TempDir(), t.TempDir()
	write(t, dirA, "real.txt", "hi\n", 0o644)
	if err := os.Symlink("real.txt", filepath.Join(dirA, "link.txt")); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}
	nvA, nvB := NewNodeView("alice"), NewNodeView("bob")
	if _, err := nvA.SyncUp(h, dirA, DefaultIgnore()); err != nil {
		t.Fatal(err)
	}
	if err := nvB.SyncDown(h, dirB); err != nil {
		t.Fatal(err)
	}

	li, err := os.Lstat(filepath.Join(dirB, "link.txt"))
	if err != nil || li.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link.txt should be a symlink on bob: mode=%v err=%v", li.Mode(), err)
	}
	if tgt, _ := os.Readlink(filepath.Join(dirB, "link.txt")); tgt != "real.txt" {
		t.Fatalf("link target = %q, want real.txt", tgt)
	}

	// Retarget the link → re-syncs.
	if err := os.Remove(filepath.Join(dirA, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("other.txt", filepath.Join(dirA, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := nvA.SyncUp(h, dirA, DefaultIgnore()); err != nil {
		t.Fatal(err)
	}
	if err := nvB.SyncDown(h, dirB); err != nil {
		t.Fatal(err)
	}
	if tgt, _ := os.Readlink(filepath.Join(dirB, "link.txt")); tgt != "other.txt" {
		t.Fatalf("retargeted link = %q, want other.txt", tgt)
	}
}

// Deleting the last file in a directory prunes the now-empty directory on the receiving node.
func TestDeletePrunesEmptyDir(t *testing.T) {
	h := NewHub()
	dirA, dirB := t.TempDir(), t.TempDir()
	write(t, dirA, "pkg/sub/only.go", "package sub\n", 0o644)
	nvA, nvB := NewNodeView("alice"), NewNodeView("bob")
	nvA.SyncUp(h, dirA, DefaultIgnore())
	nvB.SyncDown(h, dirB)
	if _, err := os.Stat(filepath.Join(dirB, "pkg", "sub")); err != nil {
		t.Fatalf("dir should exist after first sync: %v", err)
	}

	// Remove the only file → its empty parents should be pruned on bob.
	if err := os.Remove(filepath.Join(dirA, "pkg", "sub", "only.go")); err != nil {
		t.Fatal(err)
	}
	nvA.SyncUp(h, dirA, DefaultIgnore())
	nvB.SyncDown(h, dirB)
	if _, err := os.Stat(filepath.Join(dirB, "pkg", "sub")); !os.IsNotExist(err) {
		t.Fatalf("empty 'pkg/sub' should be pruned on bob, stat err=%v", err)
	}
	if _, err := os.Stat(dirB); err != nil {
		t.Fatalf("the workspace root must NOT be pruned: %v", err)
	}
}

// A conflict resolved via Hub.Resolve converges both nodes (the reconciliation end state).
func TestConflictResolveConverges(t *testing.T) {
	h := NewHub()
	dirA, dirB := t.TempDir(), t.TempDir()
	write(t, dirA, "f.go", "A\n", 0o644)
	nvA, nvB := NewNodeView("alice"), NewNodeView("bob")

	// alice publishes v1; bob pulls it.
	if _, err := nvA.SyncUp(h, dirA, DefaultIgnore()); err != nil {
		t.Fatal(err)
	}
	if err := nvB.SyncDown(h, dirB); err != nil {
		t.Fatal(err)
	}

	// Both edit from v1 → bob lands v2, alice's push conflicts.
	write(t, dirA, "f.go", "A-alice\n", 0o644)
	write(t, dirB, "f.go", "A-bob\n", 0o644)
	if _, err := nvB.SyncUp(h, dirB, DefaultIgnore()); err != nil {
		t.Fatal(err)
	}
	conflicts, _ := nvA.SyncUp(h, dirA, DefaultIgnore())
	if len(conflicts) != 1 || conflicts[0].Path != "f.go" {
		t.Fatalf("expected alice to conflict, got %+v", conflicts)
	}

	// alice reconciles (merge of both) → next version, then both converge on SyncDown.
	h.Resolve("alice", "f.go", []byte("A-alice+A-bob\n"))
	if err := nvA.SyncDown(h, dirA); err != nil {
		t.Fatal(err)
	}
	if err := nvB.SyncDown(h, dirB); err != nil {
		t.Fatal(err)
	}
	if got := read(t, dirA, "f.go"); got != "A-alice+A-bob\n" {
		t.Fatalf("alice not converged: %q", got)
	}
	if got := read(t, dirB, "f.go"); got != "A-alice+A-bob\n" {
		t.Fatalf("bob not converged: %q", got)
	}
}

// The operator's seed view (item #19): when the seed loses a race to a node, MarkConflict writes
// git-style markers locally + locks the path; SyncDown won't clobber the held file; and once the
// markers are removed the next SyncUp pushes the resolution as the next version.
func TestSeedConflictMarkLockResolve(t *testing.T) {
	h := NewHub()
	seedDir, nodeDir := t.TempDir(), t.TempDir()
	write(t, seedDir, "f.go", "base\n", 0o644)
	seed, node := NewNodeView("core"), NewNodeView("node")

	// seed publishes v1; node pulls it.
	if _, err := seed.SyncUp(h, seedDir, DefaultIgnore()); err != nil {
		t.Fatal(err)
	}
	if err := node.SyncDown(h, nodeDir); err != nil {
		t.Fatal(err)
	}

	// Both edit from v1 → node lands v2, the operator's seed push conflicts.
	write(t, seedDir, "f.go", "operator-edit\n", 0o644)
	write(t, nodeDir, "f.go", "node-edit\n", 0o644)
	if _, err := node.SyncUp(h, nodeDir, DefaultIgnore()); err != nil {
		t.Fatal(err)
	}
	conflicts, err := seed.SyncUp(h, seedDir, DefaultIgnore())
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 1 || conflicts[0].Path != "f.go" {
		t.Fatalf("expected seed to conflict, got %+v", conflicts)
	}

	// Mark it: markers land in the operator's file, the path is locked.
	if err := seed.MarkConflict(seedDir, conflicts[0]); err != nil {
		t.Fatal(err)
	}
	if !seed.IsLocked("f.go") {
		t.Fatal("path should be locked after MarkConflict")
	}
	if got := read(t, seedDir, "f.go"); !HasConflictMarkers([]byte(got)) {
		t.Fatalf("file should hold conflict markers, got %q", got)
	}

	// A held file is NOT pushed (markers must not reach the hub) and NOT clobbered by SyncDown.
	if c, _ := seed.SyncUp(h, seedDir, DefaultIgnore()); len(c) != 0 {
		t.Fatalf("locked file should not re-conflict, got %+v", c)
	}
	if v, _ := h.Get("f.go"); v.Version != 2 || string(v.Content) != "node-edit\n" {
		t.Fatalf("hub should still be the node's v2, got v%d %q", v.Version, v.Content)
	}
	if err := seed.SyncDown(h, seedDir); err != nil {
		t.Fatal(err)
	}
	if got := read(t, seedDir, "f.go"); !HasConflictMarkers([]byte(got)) {
		t.Fatalf("SyncDown clobbered the held file: %q", got)
	}

	// Operator resolves in place (removes markers) → next SyncUp unlocks and pushes v3.
	write(t, seedDir, "f.go", "operator-edit+node-edit\n", 0o644)
	if c, _ := seed.SyncUp(h, seedDir, DefaultIgnore()); len(c) != 0 {
		t.Fatalf("resolved push should not conflict, got %+v", c)
	}
	if seed.IsLocked("f.go") {
		t.Fatal("path should be unlocked after marker-free push")
	}
	v, _ := h.Get("f.go")
	if v.Version != 3 || string(v.Content) != "operator-edit+node-edit\n" {
		t.Fatalf("hub should be the resolved v3, got v%d %q", v.Version, v.Content)
	}
	if err := node.SyncDown(h, nodeDir); err != nil {
		t.Fatal(err)
	}
	if got := read(t, nodeDir, "f.go"); got != "operator-edit+node-edit\n" {
		t.Fatalf("node did not converge: %q", got)
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

// A rewrite with identical content (new mtime) must NOT be reported as changed — this is what
// stops an fsnotify-triggered sync over our own write from ping-ponging.
func TestScanChangesIgnoresIdenticalRewrite(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.go", "package a\n", 0o644)
	stamps := make(Stamps)
	_, stampOf, _, _ := ScanChanges(dir, DefaultIgnore(), stamps)
	for p, s := range stampOf {
		stamps[p] = s
	}

	// Rewrite the same bytes with a bumped mtime (as an atomic-rename / touch would).
	full := filepath.Join(dir, "a.go")
	if err := os.WriteFile(full, []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(full, future, future); err != nil {
		t.Fatal(err)
	}

	changed, stampOf2, _, _ := ScanChanges(dir, DefaultIgnore(), stamps)
	if len(changed) != 0 {
		t.Fatalf("identical-content rewrite reported %d changes (want 0)", len(changed))
	}
	if _, ok := stampOf2["a.go"]; !ok {
		t.Fatal("touched file's refreshed stamp should be returned so it isn't re-read forever")
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
