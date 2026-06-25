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

// FileStamp cheaply identifies a file revision (size + mtime) so unchanged files can be
// skipped with a stat instead of a full read — the difference between idle and pegging a CPU.
type FileStamp struct {
	Size    int64
	ModNano int64
}

// Stamps maps a path to its last-synced stamp.
type Stamps map[string]FileStamp

// ScanChanges walks dir and returns Changes only for files new or modified since prev (whose
// stamp it also reports), plus the set of paths seen. Unchanged files are stat-only, not read.
// prev is not mutated — the caller updates its stamps once a push is accepted.
func ScanChanges(dir string, ig Ignore, prev Stamps) (changed []Change, stampOf map[string]FileStamp, seen map[string]bool, err error) {
	seen = make(map[string]bool)
	stampOf = make(map[string]FileStamp)
	if _, e := os.Stat(dir); os.IsNotExist(e) {
		return nil, stampOf, seen, nil
	}
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
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
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		seen[rel] = true
		st := FileStamp{Size: info.Size(), ModNano: info.ModTime().UnixNano()}
		if prev[rel] == st {
			return nil // unchanged since last sync — skip the read
		}
		content, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		stampOf[rel] = st
		changed = append(changed, Change{Path: rel, Content: content, Mode: info.Mode().Perm()})
		return nil
	})
	return changed, stampOf, seen, err
}

// NodeView tracks a node's per-path hub version and content stamp, and syncs a directory
// to/from the hub. Unchanged files are skipped via their stamp (no re-read, no re-push).
type NodeView struct {
	ID     string
	base   map[string]uint64
	stamps Stamps
}

// NewNodeView returns a node view for the given node/agent id.
func NewNodeView(id string) *NodeView {
	return &NodeView{ID: id, base: make(map[string]uint64), stamps: make(Stamps)}
}

// SyncUp pushes changed files (and detected deletions) to the hub, returning any conflicts.
func (nv *NodeView) SyncUp(h *Hub, dir string, ig Ignore) ([]Conflict, error) {
	changed, stampOf, seen, err := ScanChanges(dir, ig, nv.stamps)
	if err != nil {
		return nil, err
	}
	var conflicts []Conflict
	for _, c := range changed {
		c.Base = nv.base[c.Path]
		res := h.Push(nv.ID, c)
		switch {
		case res.Applied:
			nv.base[c.Path] = res.Version
			nv.stamps[c.Path] = stampOf[c.Path]
		case res.Conflict != nil:
			conflicts = append(conflicts, *res.Conflict)
		}
	}
	for path := range nv.base {
		if seen[path] {
			continue
		}
		res := h.Push(nv.ID, Change{Path: path, Deleted: true, Base: nv.base[path]})
		if res.Applied {
			nv.base[path] = res.Version
			delete(nv.stamps, path)
		} else if res.Conflict != nil {
			conflicts = append(conflicts, *res.Conflict)
		}
	}
	return conflicts, nil
}

// SyncDown pulls updates the node hasn't seen and applies them to dir, advancing base and
// recording the written file's stamp (so the next SyncUp won't re-read it).
func (nv *NodeView) SyncDown(h *Hub, dir string) error {
	for _, f := range h.Pull(nv.base) {
		if err := ApplyFile(dir, f); err != nil {
			return err
		}
		nv.base[f.Path] = f.Version
		if f.Deleted {
			delete(nv.stamps, f.Path)
		} else if info, e := os.Stat(filepath.Join(dir, filepath.FromSlash(f.Path))); e == nil {
			nv.stamps[f.Path] = FileStamp{Size: info.Size(), ModNano: info.ModTime().UnixNano()}
		}
	}
	return nil
}

// Base returns the node's known version for a path (for conflict reconciliation flows).
func (nv *NodeView) Base(path string) uint64 { return nv.base[path] }
