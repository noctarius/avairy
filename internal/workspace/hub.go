// Package workspace implements avairy's file-sync hub (DESIGN.md §9): core holds the
// canonical workspace and per-file versions; nodes push their changes and pull others'.
// Concurrent divergent edits are detected and surfaced for agent reconciliation (never
// silently merged). avairy owns propagation and conflicts — there is no git in the loop
// (git lives only on core; node trees are synced, with .git excluded).
//
// Versioning: each file carries a monotonically increasing hub version. Because the hub is
// the single linearization point (hub topology, not peer mesh), this per-file counter is
// the collapsed form of the design's version vectors — sufficient to detect when two nodes
// edited the same file from the same base.
package workspace

import (
	"bytes"
	"io/fs"
	"sync"
	"time"
)

// FileState is the canonical state of one path at the hub. The json tags pin the on-disk
// snapshot format (see persist.go) so it stays stable across Go field renames.
type FileState struct {
	Path     string      `json:"path"`
	Content  []byte      `json:"content,omitempty"`
	Mode     fs.FileMode `json:"mode"`
	Version  uint64      `json:"version"` // hub version; bumps on every accepted write
	Writer   string      `json:"writer,omitempty"`
	Deleted  bool        `json:"deleted,omitempty"`
	Modified time.Time   `json:"modified,omitempty"` // when this version was stored (for "age" in a resync overview)
}

// Change is a node's proposed write to one path, edited from base version Base
// (0 = the node believes the file is new).
type Change struct {
	Path    string
	Content []byte
	Mode    fs.FileMode
	Deleted bool
	Base    uint64
}

// Conflict reports a divergent concurrent edit: the node edited Path from Base, but the hub
// has since moved to Hub.Version. Both sides are returned for agent reconciliation.
type Conflict struct {
	Path     string
	Base     uint64
	Hub      FileState
	Incoming Change
}

// PushResult is the outcome of a Push.
type PushResult struct {
	Applied  bool
	Version  uint64 // new hub version, when Applied
	Conflict *Conflict
}

// Hub is the canonical, in-memory workspace store. It can be snapshotted to / restored from
// disk (see persist.go) so a core restart doesn't lose canonical state.
type Hub struct {
	mu    sync.Mutex
	files map[string]*FileState
	dirty bool // a write occurred since the last successful Save (skip idle persistence)
}

// NewHub returns an empty hub.
func NewHub() *Hub { return &Hub{files: make(map[string]*FileState)} }

// Push applies a node's change if it was edited from the current hub version; otherwise it
// returns a Conflict and leaves the hub unchanged. Text content is LF-normalized in transit.
func (h *Hub) Push(node string, c Change) PushResult {
	c.Content = normalizeForTransit(c.Content)

	h.mu.Lock()
	defer h.mu.Unlock()

	cur, exists := h.files[c.Path]
	if !exists {
		// New file. Accept unless the node thinks it's editing an existing version.
		if c.Base != 0 {
			return PushResult{Conflict: &Conflict{Path: c.Path, Base: c.Base, Incoming: c}}
		}
		return PushResult{Applied: true, Version: h.store(node, c, 1)}
	}
	if c.Base != cur.Version {
		// Hub moved since the node's base → concurrent edit.
		return PushResult{Conflict: &Conflict{Path: c.Path, Base: c.Base, Hub: cur.clone(), Incoming: c}}
	}
	// Idempotent: an unchanged re-push must NOT bump the version (else versions inflate every
	// sync tick and every node perpetually re-pulls the whole tree).
	if (c.Deleted && cur.Deleted) || (!c.Deleted && !cur.Deleted && bytes.Equal(cur.Content, c.Content)) {
		return PushResult{Applied: true, Version: cur.Version}
	}
	return PushResult{Applied: true, Version: h.store(node, c, cur.Version+1)}
}

// Resolve force-applies agent/human-reconciled content as the next version, clearing a
// conflict. Use after Push returned a Conflict.
func (h *Hub) Resolve(node, path string, merged []byte) PushResult {
	h.mu.Lock()
	defer h.mu.Unlock()
	next := uint64(1)
	if cur, ok := h.files[path]; ok {
		next = cur.Version + 1
	}
	return PushResult{Applied: true, Version: h.store(node, Change{Path: path, Content: normalizeForTransit(merged), Mode: 0o644}, next)}
}

// store writes a change at version v (caller holds the lock).
func (h *Hub) store(node string, c Change, v uint64) uint64 {
	mode := c.Mode
	if mode == 0 {
		mode = 0o644
	}
	h.files[c.Path] = &FileState{
		Path:     c.Path,
		Content:  c.Content,
		Mode:     mode,
		Version:  v,
		Writer:   node,
		Deleted:  c.Deleted,
		Modified: time.Now(),
	}
	h.dirty = true
	return v
}

// ManifestEntry is one path's canonical fingerprint, for a node to reconcile its tree against the
// hub without trusting its tracked versions (item #21): the content checksum drives the delta, the
// version re-syncs the node's base, and Modified gives the operator's overview a real "age".
type ManifestEntry struct {
	Path     string    `json:"path"`
	Checksum uint64    `json:"checksum"`
	Version  uint64    `json:"version"`
	Modified time.Time `json:"modified"`
}

// Manifest returns a fingerprint of every live (non-deleted) path. A node diffs this against its
// local files and pulls only what differs — far cheaper than re-downloading the whole tree.
func (h *Hub) Manifest() []ManifestEntry {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]ManifestEntry, 0, len(h.files))
	for _, f := range h.files {
		if f.Deleted {
			continue
		}
		out = append(out, ManifestEntry{Path: f.Path, Checksum: HashContent(f.Content), Version: f.Version, Modified: f.Modified})
	}
	return out
}

// Pull returns the files whose hub version differs from the node's known base (i.e. updates
// the node hasn't seen yet), including deletions.
func (h *Hub) Pull(base map[string]uint64) []FileState {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []FileState
	for path, f := range h.files {
		if base[path] != f.Version {
			out = append(out, f.clone())
		}
	}
	return out
}

// Get returns the current state of a path.
func (h *Hub) Get(path string) (FileState, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if f, ok := h.files[path]; ok {
		return f.clone(), true
	}
	return FileState{}, false
}

// List returns all current file states.
func (h *Hub) List() []FileState {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]FileState, 0, len(h.files))
	for _, f := range h.files {
		out = append(out, f.clone())
	}
	return out
}

func (f *FileState) clone() FileState {
	c := *f
	c.Content = append([]byte(nil), f.Content...)
	return c
}
