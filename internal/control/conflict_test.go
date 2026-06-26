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
