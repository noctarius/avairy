package control

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// Decision strings an operator gives a routed conflict (DESIGN.md §9, item #19).
const (
	// ConflictMine: the operator resolves it themselves (in their editor — the file already holds
	// git-style markers); the notification clears.
	ConflictMine = "mine"
	// ConflictDelegate: hand it to an agent, which edits the markers out and calls resolve_conflict.
	ConflictDelegate = "delegate"
	// ConflictResync / ConflictResolve: the operator's verdict on a node's held startup conflict
	// (#21). Resync = checksum-manifest reconcile (discard local divergence); Resolve = write markers
	// and reconcile as usual. Routed back to the node as a heartbeat directive.
	ConflictResync  = "resync"
	ConflictResolve = "resolve"
)

// OperatorConflict is a file conflict with no owning agent — the operator's own seed workspace
// diverging from a node's edit, or (later) a git rebase/merge conflict on core's repo. Unlike an
// agent push conflict (routed to the agent whose push lost), there's no agent to hand it to, so it
// surfaces in the TUI Conflicts view where the human resolves it or delegates it (DESIGN.md §9).
type OperatorConflict struct {
	ID         string    `json:"id"`
	Path       string    `json:"path"`
	HubVersion uint64    `json:"hubVersion"`
	Source     string    `json:"source"` // "seed" | "git"
	Detail     string    `json:"detail,omitempty"`
	At         time.Time `json:"-"`
}

// Conflicts is the operator-facing conflict registry. It's a notification board, not a blocking
// broker (Approvals blocks a gated action; a conflict is already on disk with markers) — Raise
// records it, the TUI shows it, and the operator Resolves or it's cleared once the markers go away.
type Conflicts struct {
	// OnRaise/OnResolve observe the lifecycle (used to journal, which wakes the TUI to refresh).
	OnRaise   func(OperatorConflict)
	OnResolve func(OperatorConflict, string)

	mu      sync.Mutex
	seq     int
	pending map[string]*OperatorConflict
	byPath  map[string]string // path -> id, so re-raising a path dedups and ClearPath can find it
}

// NewConflicts returns an empty conflict registry.
func NewConflicts() *Conflicts {
	return &Conflicts{pending: make(map[string]*OperatorConflict), byPath: make(map[string]string)}
}

// Raise records a conflict for the operator and returns its id. Re-raising a path that's already
// pending updates it in place (same id) rather than stacking duplicates — a seed conflict re-fires
// every sync tick until resolved, and the operator should see one entry, not a growing list.
func (c *Conflicts) Raise(oc OperatorConflict) string {
	c.mu.Lock()
	if id, ok := c.byPath[oc.Path]; ok {
		existing := c.pending[id]
		oc.ID, oc.At = existing.ID, existing.At
		*existing = oc
		c.mu.Unlock()
		return id
	}
	c.seq++
	oc.ID = fmt.Sprintf("cf%d", c.seq)
	oc.At = time.Now()
	c.pending[oc.ID] = &oc
	c.byPath[oc.Path] = oc.ID
	c.mu.Unlock()
	if c.OnRaise != nil {
		c.OnRaise(oc)
	}
	return oc.ID
}

// Pending returns unresolved conflicts, oldest first (for the TUI).
func (c *Conflicts) Pending() []OperatorConflict {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]OperatorConflict, 0, len(c.pending))
	for _, oc := range c.pending {
		out = append(out, *oc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

// Resolve records the operator's decision and clears the conflict, returning it (and whether it was
// still pending). The caller acts on the decision (delegate → publish to an agent; mine → nothing,
// the operator edits the marked file and the next sync picks it up).
func (c *Conflicts) Resolve(id, decision string) (OperatorConflict, bool) {
	c.mu.Lock()
	oc, ok := c.pending[id]
	if !ok {
		c.mu.Unlock()
		return OperatorConflict{}, false
	}
	resolved := *oc
	delete(c.pending, id)
	delete(c.byPath, oc.Path)
	c.mu.Unlock()
	if c.OnResolve != nil {
		c.OnResolve(resolved, decision)
	}
	return resolved, true
}

// ClearPath removes a pending conflict for path — used when the markers were removed and the file
// synced cleanly, so the conflict resolved itself without an explicit operator verdict. Reports
// whether one was cleared.
func (c *Conflicts) ClearPath(path string) bool {
	c.mu.Lock()
	id, ok := c.byPath[path]
	if !ok {
		c.mu.Unlock()
		return false
	}
	oc := *c.pending[id]
	delete(c.pending, id)
	delete(c.byPath, path)
	c.mu.Unlock()
	if c.OnResolve != nil {
		c.OnResolve(oc, "cleared")
	}
	return true
}
