package control

import (
	"context"
	"errors"
	"testing"
)

// A node pulls the repo bundle over the channel; a missing repo on core 404s cleanly.
func TestPullBundleOverWire(t *testing.T) {
	core, srv := newCoreServer(t)
	n := NewNode(srv.URL, "linbot")
	if err := n.Enroll(core.CurrentToken(), "linux", nil); err != nil {
		t.Fatal(err)
	}

	// No Bundle provider → 404.
	if _, err := n.PullBundle(context.Background()); err == nil {
		t.Fatal("expected error when core has no repo")
	}

	core.Bundle = func(context.Context) ([]byte, error) { return []byte("BUNDLE-BYTES"), nil }
	got, err := n.PullBundle(context.Background())
	if err != nil || string(got) != "BUNDLE-BYTES" {
		t.Fatalf("pull bundle: got %q err=%v", got, err)
	}

	core.Bundle = func(context.Context) ([]byte, error) { return nil, errors.New("boom") }
	if _, err := n.PullBundle(context.Background()); err == nil {
		t.Fatal("bundle provider error should surface")
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
