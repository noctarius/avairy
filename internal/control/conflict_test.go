package control

import (
	"testing"
)

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
