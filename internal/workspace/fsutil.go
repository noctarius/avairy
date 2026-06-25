package workspace

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Ignore filters paths out of sync (gitignore-style, DESIGN.md §9): never sync .git, build
// output, dependency dirs, or binaries.
type Ignore struct {
	Dirs     map[string]bool
	Suffixes []string
}

// DefaultIgnore is the baseline exclude set.
func DefaultIgnore() Ignore {
	return Ignore{
		Dirs:     map[string]bool{".git": true, "node_modules": true, "build": true, "dist": true, "target": true, ".avairy": true},
		Suffixes: []string{".o", ".exe", ".dll", ".so", ".dylib", ".test", ".class", ".pyc"},
	}
}

// Match reports whether a slash-separated relative path should be excluded.
func (ig Ignore) Match(rel string) bool {
	for seg := range strings.SplitSeq(rel, "/") {
		if ig.Dirs[seg] {
			return true
		}
	}
	for _, suf := range ig.Suffixes {
		if strings.HasSuffix(rel, suf) {
			return true
		}
	}
	return false
}

// normalizeForTransit LF-normalizes text content; binary content (contains NUL) is left as-is.
func normalizeForTransit(b []byte) []byte {
	if bytes.IndexByte(b, 0) >= 0 {
		return b
	}
	return bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
}

// Scan walks dir and returns a Change per regular, non-ignored file (Base unset). A
// non-existent dir scans as empty (the workspace may not be created yet).
func Scan(dir string, ig Ignore) ([]Change, error) {
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return nil, nil
	}
	var out []Change
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(dir, p)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if ig.Match(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		content, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		info, rerr := d.Info()
		if rerr != nil {
			return rerr
		}
		out = append(out, Change{Path: rel, Content: content, Mode: info.Mode().Perm()})
		return nil
	})
	return out, err
}

// ApplyFile writes (or deletes) one hub file into dir, atomically (temp + rename), creating
// parent directories and preserving the mode bit.
func ApplyFile(dir string, f FileState) error {
	full := filepath.Join(dir, filepath.FromSlash(f.Path))
	if f.Deleted {
		if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	parent := filepath.Dir(full)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	mode := f.Mode
	if mode == 0 {
		mode = 0o644
	}
	tmp, err := os.CreateTemp(parent, ".avairy-tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(f.Content); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, full)
}

// NodeView tracks a node's per-path known hub version and syncs a real directory to/from
// the hub. The fs-watch loop that calls SyncUp on change lands with the node daemon (§4).
type NodeView struct {
	ID   string
	base map[string]uint64
}

// NewNodeView returns a node view for the given node/agent id.
func NewNodeView(id string) *NodeView {
	return &NodeView{ID: id, base: make(map[string]uint64)}
}

// SyncUp scans dir and pushes every change (and detected deletion) to the hub, returning any
// conflicts. The node's base advances for each accepted change; conflicted paths are left
// for reconciliation.
func (nv *NodeView) SyncUp(h *Hub, dir string, ig Ignore) ([]Conflict, error) {
	changes, err := Scan(dir, ig)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(changes))
	var conflicts []Conflict
	push := func(c Change) {
		c.Base = nv.base[c.Path]
		res := h.Push(nv.ID, c)
		switch {
		case res.Applied:
			nv.base[c.Path] = res.Version
		case res.Conflict != nil:
			conflicts = append(conflicts, *res.Conflict)
		}
	}
	for _, c := range changes {
		seen[c.Path] = true
		push(c)
	}
	for path := range nv.base {
		if !seen[path] {
			push(Change{Path: path, Deleted: true})
		}
	}
	return conflicts, nil
}

// SyncDown pulls updates the node hasn't seen and applies them to dir, advancing base.
func (nv *NodeView) SyncDown(h *Hub, dir string) error {
	for _, f := range h.Pull(nv.base) {
		if err := ApplyFile(dir, f); err != nil {
			return err
		}
		nv.base[f.Path] = f.Version
	}
	return nil
}

// Base returns the node's known version for a path (for conflict reconciliation flows).
func (nv *NodeView) Base(path string) uint64 { return nv.base[path] }
