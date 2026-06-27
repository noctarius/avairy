package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"avairy/internal/workspace"
)

func read(t *testing.T, dir, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A node pulls the repo bundle over the channel; a missing repo on core 404s cleanly.
func TestPullBundleOverWire(t *testing.T) {
	core, srv := newCoreServer(t)
	n := NewNode(srv.URL, "linbot")
	if err := n.Enroll(core.CurrentToken(), "linux", nil); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()

	// No Bundle provider → 404.
	if _, err := n.PullBundle(ctx, nil); err == nil {
		t.Fatal("expected error when core has no repo")
	}

	// The node's "have" shas reach core (for incremental bundling), and bytes come back.
	var gotHave []string
	core.Bundle = func(_ context.Context, have []string) ([]byte, error) {
		gotHave = have
		return []byte("BUNDLE-BYTES"), nil
	}
	got, err := n.PullBundle(ctx, []string{"abc123"})
	if err != nil || string(got) != "BUNDLE-BYTES" {
		t.Fatalf("pull bundle: got %q err=%v", got, err)
	}
	if len(gotHave) != 1 || gotHave[0] != "abc123" {
		t.Fatalf("have not forwarded to core: %v", gotHave)
	}

	// Nothing new → 204 → (nil, nil), no error.
	core.Bundle = func(_ context.Context, _ []string) ([]byte, error) {
		return nil, nil
	}
	if b, err := n.PullBundle(ctx, nil); err != nil || b != nil {
		t.Fatalf("expected (nil,nil) on 204, got %q err=%v", b, err)
	}

	core.Bundle = func(_ context.Context, _ []string) ([]byte, error) {
		return nil, errors.New("boom")
	}
	if _, err := n.PullBundle(ctx, nil); err == nil {
		t.Fatal("bundle provider error should surface")
	}
}

// conflictedNodeB sets up a conflict: A publishes f.go@v1="A", B's divergent "B" is rejected and
// its file gets 3-way markers. Returns the wired core/server/nodes and B's dir.
func conflictedNodeB(t *testing.T) (*Core, *Node, *Node, string) {
	t.Helper()
	core, srv := newCoreServer(t)
	dirA, dirB := t.TempDir(), t.TempDir()
	writeFile(t, dirA, "f.go", "A")
	writeFile(t, dirB, "f.go", "B")

	nodeA := NewNode(srv.URL, "a")
	nodeA.Enroll(core.CurrentToken(), "linux", nil)
	nodeA.SyncUp(dirA) // f.go -> v1 ("A")

	nodeB := NewNode(srv.URL, "b")
	nodeB.Enroll(core.CurrentToken(), "linux", nil)
	if c, _ := nodeB.SyncUp(dirB); len(c) != 1 { // push "B" from base 0 → conflict @v1
		t.Fatalf("expected conflict, got %+v", c)
	}
	marked := read(t, dirB, "f.go")
	if !workspace.HasConflictMarkers([]byte(marked)) || !strings.Contains(marked, "A") || !strings.Contains(marked, "B") {
		t.Fatalf("expected 3-way markers with both sides, got:\n%s", marked)
	}
	// reuse dirA via a closure on nodeA — return what the tests need
	t.Cleanup(srv.Close)
	return core, nodeA, nodeB, dirB
}

// Editing a conflicted file marker-free resolves it: it unlocks, pushes from the adopted base,
// and the other node converges.
func TestNodeConflictResolveConverges(t *testing.T) {
	_, nodeA, nodeB, dirB := conflictedNodeB(t)
	dirA := t.TempDir()
	// nodeA needs a dir for SyncDown; reconstruct its v1 state then pull the resolution.
	writeFile(t, dirA, "f.go", "A")
	nodeA.SyncDown(dirA) // align A's local with v1 (no-op content-wise)

	writeFile(t, dirB, "f.go", "A+B merged") // agent removes markers, merges
	if c, _ := nodeB.SyncUp(dirB); len(c) != 0 {
		t.Fatalf("resolved push should not conflict, got %+v", c)
	}
	nodeA.SyncDown(dirA)
	if got := read(t, dirA, "f.go"); got != "A+B merged" {
		t.Fatalf("A did not converge on the merged content: %q", got)
	}
}

// A conflicted (locked) file is not clobbered by SyncDown even when the hub moves on.
func TestNodeConflictLockHoldsAgainstSyncDown(t *testing.T) {
	_, nodeA, nodeB, dirB := conflictedNodeB(t)
	marked := read(t, dirB, "f.go")

	dirA := t.TempDir()
	writeFile(t, dirA, "f.go", "A")
	nodeA.SyncDown(dirA)
	writeFile(t, dirA, "f.go", "A2") // hub moves to v2
	nodeA.SyncUp(dirA)

	nodeB.SyncDown(dirB) // must NOT overwrite B's in-progress markers
	if got := read(t, dirB, "f.go"); got != marked {
		t.Fatalf("locked conflicted file was clobbered:\n%s", got)
	}
}

// A fresh node whose workspace is already populated with the hub's files (a pre-existing checkout,
// or a restart that lost its in-memory base) must NOT report a conflict on every file: ResumeFromHub
// adopts the hub versions first so the initial SyncUp pushes idempotently instead of from base 0.
func TestResumeFromHubAvoidsSpuriousConflicts(t *testing.T) {
	core, srv := newCoreServer(t)
	t.Cleanup(srv.Close)

	// Core/seed publishes the project into the hub via node "seed".
	dirSeed := t.TempDir()
	writeFile(t, dirSeed, "main.go", "package main")
	writeFile(t, dirSeed, "go.mod", "module x")
	seed := NewNode(srv.URL, "seed")
	seed.Enroll(core.CurrentToken(), "linux", nil)
	seed.SyncUp(dirSeed)

	// A new node joins with the SAME files already on disk (e.g. an existing checkout).
	dirNew := t.TempDir()
	writeFile(t, dirNew, "main.go", "package main")
	writeFile(t, dirNew, "go.mod", "module x")
	fresh := NewNode(srv.URL, "fresh")
	fresh.Enroll(core.CurrentToken(), "linux", nil)

	if err := fresh.ResumeFromHub(dirNew); err != nil {
		t.Fatal(err)
	}
	if c, err := fresh.SyncUp(dirNew); err != nil || len(c) != 0 {
		t.Fatalf("initial sync after ResumeFromHub: conflicts=%+v err=%v (want none)", c, err)
	}

	// A genuine local edit after resume still flows (and doesn't conflict, hub hasn't moved).
	writeFile(t, dirNew, "main.go", "package main // edited")
	if c, _ := fresh.SyncUp(dirNew); len(c) != 0 {
		t.Fatalf("edit after resume should push cleanly, got %+v", c)
	}
	if f, ok := core.hub.Get("main.go"); !ok || string(f.Content) != "package main // edited" {
		t.Fatalf("edit did not land on the hub: %q ok=%v", f.Content, ok)
	}
}

// Resync reconciles a node's divergent/stale tree against the hub manifest: matching files stay,
// locally-diverged files are overwritten with canonical, hub-deleted files are removed, and missing
// canonical files are fetched — and only the delta is pulled.
func TestResyncReconcilesAgainstManifest(t *testing.T) {
	core, srv := newCoreServer(t)
	t.Cleanup(srv.Close)

	// Seed canonical: a.go, b.go, c.go.
	dirSeed := t.TempDir()
	writeFile(t, dirSeed, "a.go", "AAA")
	writeFile(t, dirSeed, "b.go", "BBB")
	writeFile(t, dirSeed, "c.go", "CCC")
	seed := NewNode(srv.URL, "seed")
	seed.Enroll(core.CurrentToken(), "linux", nil)
	seed.SyncUp(dirSeed)

	// A node with a divergent tree: a.go matches, b.go diverged, c.go missing, x.go is local-only.
	dir := t.TempDir()
	writeFile(t, dir, "a.go", "AAA")        // matches canonical → must not be re-pulled
	writeFile(t, dir, "b.go", "B-local")    // diverged → must be overwritten with canonical
	writeFile(t, dir, "x.go", "X-local")    // not in hub → must be deleted
	node := NewNode(srv.URL, "macos")
	node.Enroll(core.CurrentToken(), "darwin", nil)

	if err := node.Resync(dir); err != nil {
		t.Fatal(err)
	}
	if got := read(t, dir, "a.go"); got != "AAA" {
		t.Fatalf("a.go = %q, want AAA (unchanged)", got)
	}
	if got := read(t, dir, "b.go"); got != "BBB" {
		t.Fatalf("b.go = %q, want canonical BBB", got)
	}
	if got := read(t, dir, "c.go"); got != "CCC" {
		t.Fatalf("c.go = %q, want fetched CCC", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "x.go")); !os.IsNotExist(err) {
		t.Fatal("x.go (not in hub) should have been deleted")
	}
	// Base is in lockstep now: a no-op sync neither conflicts nor changes anything.
	if c, _ := node.SyncUp(dir); len(c) != 0 {
		t.Fatalf("post-resync sync should be clean, got %+v", c)
	}
}

// newConflict notifies once per (agent, path, hub version): a node re-pushing the same stale
// edit doesn't re-notify until the hub moves on.
func TestNewConflictDedup(t *testing.T) {
	c := NewCore(nil, nil)

	if !c.newConflict("alice", "f.go", 2) {
		t.Fatal("first conflict at v2 should notify")
	}
	if c.newConflict("alice", "f.go", 2) {
		t.Fatal("same (agent,path,version) should not re-notify")
	}
	if !c.newConflict("alice", "f.go", 3) {
		t.Fatal("hub moving to v3 should notify again")
	}
	if !c.newConflict("bob", "f.go", 2) {
		t.Fatal("a different agent should notify independently")
	}
	if !c.newConflict("alice", "other.go", 2) {
		t.Fatal("a different path should notify independently")
	}
}
