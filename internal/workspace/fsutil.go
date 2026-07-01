package workspace

import (
	"bytes"
	"hash/fnv"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/moby/patternmatcher"
	"github.com/moby/patternmatcher/ignorefile"
	gitignore "github.com/sabhiram/go-gitignore"
)

// Ignore filters paths out of sync (DESIGN.md §9): a built-in baseline (VCS, caches, build
// trees, binaries) plus the project's own .gitignore / .dockerignore / .avairyignore parsed
// with their real syntax.
type Ignore struct {
	Dirs     map[string]bool // exact path-segment names
	Prefixes []string        // path-segment prefixes (e.g. "build-")
	Suffixes []string        // file suffixes (e.g. ".o")

	git    *gitignore.GitIgnore           // .gitignore + .avairyignore (gitignore syntax)
	docker *patternmatcher.PatternMatcher // .dockerignore (dockerignore syntax)
}

// DefaultIgnore is the baseline exclude set — VCS, editor/agent state, build trees, caches,
// dependency dirs, and common binaries.
func DefaultIgnore() Ignore {
	dirs := []string{
		".git", ".svn", ".hg", ".avairy",
		// Agent-CLI per-project config/state: machine- and agent-local, must never sync — a conflict
		// marker written into one (e.g. .codex/config.toml) breaks the tool on startup.
		".claude", ".codex", ".grok", ".gemini", ".copilot", ".aider",
		".idea", ".vscode",
		"node_modules", "vendor", "target", "dist", "out", "bin", "obj",
		"__pycache__", ".venv", "venv", ".pytest_cache", ".mypy_cache",
		".cache", ".zig-cache", "zig-cache", "zig_global_cache",
		".cmake", ".next", ".nuxt", ".gradle", ".terraform",
	}
	m := make(map[string]bool, len(dirs))
	for _, d := range dirs {
		m[d] = true
	}
	return Ignore{
		Dirs:     m,
		Prefixes: []string{"build", "cmake-build"}, // build, build-wasm3, cmake-build-debug, …
		Suffixes: []string{".o", ".obj", ".a", ".lib", ".exe", ".dll", ".so", ".dylib", ".wasm", ".bin", ".test", ".class", ".pyc", ".DS_Store"},
	}
}

// IgnoreFor returns DefaultIgnore augmented with the dir's ignore files, parsed with their
// real syntax: .gitignore and .avairyignore (gitignore) and .dockerignore (dockerignore).
func IgnoreFor(dir string) Ignore {
	ig := DefaultIgnore()
	var lines []string
	lines = append(lines, readLines(filepath.Join(dir, ".gitignore"))...)
	lines = append(lines, readLines(filepath.Join(dir, ".avairyignore"))...)
	if len(lines) > 0 {
		ig.git = gitignore.CompileIgnoreLines(lines...)
	}
	if f, err := os.Open(filepath.Join(dir, ".dockerignore")); err == nil {
		if pats, rerr := ignorefile.ReadAll(f); rerr == nil && len(pats) > 0 {
			ig.docker, _ = patternmatcher.New(pats)
		}
		f.Close()
	}
	return ig
}

func readLines(path string) []string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return strings.Split(string(b), "\n")
}

// Match reports whether a slash-separated relative path should be excluded.
func (ig Ignore) Match(rel string) bool {
	for seg := range strings.SplitSeq(rel, "/") {
		if ig.Dirs[seg] {
			return true
		}
		for _, p := range ig.Prefixes {
			if strings.HasPrefix(seg, p) {
				return true
			}
		}
	}
	for _, suf := range ig.Suffixes {
		if strings.HasSuffix(rel, suf) {
			return true
		}
	}
	if ig.git != nil && ig.git.MatchesPath(rel) {
		return true
	}
	if ig.docker != nil {
		if m, _ := ig.docker.MatchesOrParentMatches(rel); m {
			return true
		}
	}
	return false
}

// entryPayload returns the syncable content and mode for a directory entry: a regular file's
// bytes (mode = perm bits), or a symlink's target path (mode carries fs.ModeSymlink, so the
// other end recreates a link rather than a file). ok is false for entries avairy doesn't sync
// (directories, sockets, devices). Symlinks are NOT followed — the link itself is replicated.
func entryPayload(p string, d fs.DirEntry) (content []byte, mode fs.FileMode, ok bool, err error) {
	if d.Type()&fs.ModeSymlink != 0 {
		target, e := os.Readlink(p)
		if e != nil {
			return nil, 0, false, e
		}
		return []byte(target), fs.ModeSymlink | 0o777, true, nil
	}
	if d.Type().IsRegular() {
		b, e := os.ReadFile(p)
		if e != nil {
			return nil, 0, false, e
		}
		info, e := d.Info()
		if e != nil {
			return nil, 0, false, e
		}
		return b, info.Mode().Perm(), true, nil
	}
	return nil, 0, false, nil // dir / socket / device — not synced
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
		content, mode, ok, rerr := entryPayload(p, d)
		if rerr != nil {
			return rerr
		}
		if !ok {
			return nil
		}
		out = append(out, Change{Path: rel, Content: content, Mode: mode})
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
		pruneEmptyParents(dir, filepath.Dir(full)) // don't leave empty dirs behind after a delete/move
		return nil
	}
	parent := filepath.Dir(full)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	// A symlink: recreate the link (target = content), replacing whatever is there. Not atomic
	// (no temp+rename for links), which is acceptable for the rare symlink case.
	if f.Mode&fs.ModeSymlink != 0 {
		if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
			return err
		}
		return os.Symlink(string(f.Content), full)
	}
	mode := f.Mode.Perm()
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

// pruneEmptyParents removes now-empty directories from start upward toward root (exclusive), so
// deleting the last file in a directory (or moving a subtree away) doesn't leave empty husks on
// other nodes. It stops at the first directory that isn't empty (os.Remove fails) or at root.
func pruneEmptyParents(root, start string) {
	root = filepath.Clean(root)
	for cur := filepath.Clean(start); cur != root && len(cur) > len(root); cur = filepath.Dir(cur) {
		if os.Remove(cur) != nil {
			return // non-empty, or already gone — stop climbing
		}
	}
}

// FileStamp identifies a file revision. Size+ModNano is the cheap stat-only gate (skip
// unchanged files without reading them — the difference between idle and pegging a CPU). Hash
// is the authoritative content identity, checked only when the cheap gate trips: it stops a
// metadata-only change (atomic rename, touch, git checkout — or our own SyncDown/reconcile
// writes seen by fsnotify) from being mistaken for a content change and re-pushed. Without it,
// fsnotify-triggered syncs over content-identical writes can ping-pong (write → resync →
// write …).
type FileStamp struct {
	Size    int64
	ModNano int64
	Hash    uint64 // FNV-1a of content; 0 until the file is read
}

// HashContent is a fast non-cryptographic content fingerprint for change detection.
func HashContent(b []byte) uint64 {
	h := fnv.New64a()
	_, _ = h.Write(b)
	return h.Sum64()
}

// Stamps maps a path to its last-synced stamp.
type Stamps map[string]FileStamp

// ScanChanges walks dir and returns Changes for files whose *content* differs from prev, plus
// the set of paths seen. Detection is two-stage: an unchanged size+mtime is a stat-only skip
// (no read); when that gate trips the file is read and content-hashed, and only a real hash
// difference counts as changed. stampOf carries the fresh stamp for every file that was read —
// both genuinely-changed files and "touched but identical" ones — so the caller can refresh
// the latter immediately and stop re-reading them (and never re-push identical content). prev
// is not mutated.
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
		typ := d.Type()
		if d.IsDir() || (!typ.IsRegular() && typ&fs.ModeSymlink == 0) {
			return nil // dirs and non-regular non-symlink entries (sockets/devices) aren't synced
		}
		info, ierr := d.Info() // lstat (WalkDir doesn't follow links) — the link's own size/mtime
		if ierr != nil {
			return ierr
		}
		seen[rel] = true
		prevSt, had := prev[rel]
		st := FileStamp{Size: info.Size(), ModNano: info.ModTime().UnixNano(), Hash: prevSt.Hash}
		if had && prevSt.Size == st.Size && prevSt.ModNano == st.ModNano {
			return nil // cheap gate: size+mtime unchanged → skip the read
		}
		// Gate tripped — read content (file bytes, or symlink target) and hash to find out
		// whether it actually changed.
		content, mode, ok, rerr := entryPayload(p, d)
		if rerr != nil {
			return rerr
		}
		if !ok {
			return nil
		}
		st.Hash = HashContent(content)
		stampOf[rel] = st
		if had && prevSt.Hash == st.Hash {
			return nil // metadata moved but content identical → refresh stamp, don't re-push
		}
		changed = append(changed, Change{Path: rel, Content: content, Mode: mode})
		return nil
	})
	return changed, stampOf, seen, err
}

// NodeView tracks a node's per-path hub version and content stamp, and syncs a directory
// to/from the hub. Unchanged files are skipped via their stamp (no re-read, no re-push).
type NodeView struct {
	ID        string
	base      map[string]uint64
	stamps    Stamps
	conflicts map[string]bool // paths holding unresolved conflict markers (locked from sync)
}

// NewNodeView returns a node view for the given node/agent id.
func NewNodeView(id string) *NodeView {
	return &NodeView{ID: id, base: make(map[string]uint64), stamps: make(Stamps), conflicts: make(map[string]bool)}
}

// SyncUp pushes changed files (and detected deletions) to the hub, returning any conflicts.
func (nv *NodeView) SyncUp(h *Hub, dir string, ig Ignore) ([]Conflict, error) {
	changed, stampOf, seen, err := ScanChanges(dir, ig, nv.stamps)
	if err != nil {
		return nil, err
	}
	changedSet := make(map[string]bool, len(changed))
	for _, c := range changed {
		changedSet[c.Path] = true
	}
	// Files read but unchanged in content (metadata moved only): refresh their stamp now so we
	// don't re-read them — and never push identical content (no fsnotify ping-pong).
	for path, st := range stampOf {
		if !changedSet[path] {
			nv.stamps[path] = st
		}
	}
	var conflicts []Conflict
	for _, c := range changed {
		// A path holding unresolved conflict markers is LOCKED: don't push it (that would land the
		// markers in the hub). When it's edited marker-free, it's resolved → unlock and push from
		// the adopted base so it lands as the next version. Mirrors control.Node.SyncUp.
		if HasConflictMarkers(c.Content) {
			nv.conflicts[c.Path] = true
			nv.stamps[c.Path] = stampOf[c.Path]
			continue
		}
		delete(nv.conflicts, c.Path)
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
		if seen[path] || nv.conflicts[path] { // a conflicted (held) file isn't a deletion
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
		if nv.conflicts[f.Path] {
			continue // LOCKED: the operator/agent is resolving conflict markers here — don't clobber it
		}
		if err := ApplyFile(dir, f); err != nil {
			return err
		}
		nv.base[f.Path] = f.Version
		if f.Deleted {
			delete(nv.stamps, f.Path)
		} else if info, e := os.Stat(filepath.Join(dir, filepath.FromSlash(f.Path))); e == nil {
			// Record size+mtime AND content hash: a later touch trips the cheap gate but the
			// hash match then proves the content is unchanged, so we don't re-push our own write.
			nv.stamps[f.Path] = FileStamp{Size: info.Size(), ModNano: info.ModTime().UnixNano(), Hash: HashContent(f.Content)}
		}
	}
	return nil
}

// Base returns the node's known version for a path (for conflict reconciliation flows).
func (nv *NodeView) Base(path string) uint64 { return nv.base[path] }

// MarkConflict writes git-style markers into a rejected file under dir (the operator's local edit
// is the "ours" side, so nothing is lost), adopts the hub version as base, and locks the path from
// further sync until the markers are removed. Use after SyncUp returned a Conflict for an
// owner-less (seed) edit, so the human — or a local agent sharing this workspace — can resolve it
// in place and the next SyncUp lands the result as HubVersion+1. Mirrors control.Node.markConflict.
func (nv *NodeView) MarkConflict(dir string, c Conflict) error {
	full := filepath.Join(dir, filepath.FromSlash(c.Path))
	local, err := os.ReadFile(full)
	if err != nil {
		return err // file vanished; nothing to mark
	}
	if !HasConflictMarkers(local) {
		marked := MergeMarkers(local, c.Hub.Content, c.Hub.Version)
		if err := os.WriteFile(full, marked, 0o644); err != nil {
			return err
		}
		if info, e := os.Stat(full); e == nil {
			nv.stamps[c.Path] = FileStamp{Size: info.Size(), ModNano: info.ModTime().UnixNano(), Hash: HashContent(marked)}
		}
	}
	nv.conflicts[c.Path] = true
	nv.base[c.Path] = c.Hub.Version // resolved edit will push from here → Hub.Version+1
	return nil
}

// LockedPaths returns the paths currently held with unresolved conflict markers.
func (nv *NodeView) LockedPaths() []string {
	out := make([]string, 0, len(nv.conflicts))
	for p := range nv.conflicts {
		out = append(out, p)
	}
	return out
}

// IsLocked reports whether path is held with unresolved conflict markers.
func (nv *NodeView) IsLocked(path string) bool { return nv.conflicts[path] }

// ResumeFromHub primes a freshly-created view against a restored hub so the next SyncUp behaves
// like a resume, not a first sync. For each hub file that still exists under dir it adopts the
// hub's version as the view's base: an unchanged local file is then skipped (idempotent), a
// locally-edited one bumps the version (operator wins), and — crucially — hub files the
// operator doesn't have locally (e.g. contributed by other nodes) are left unclaimed, so they
// are NOT seen as local deletions and wiped. Call once at startup before the first SyncUp.
func (nv *NodeView) ResumeFromHub(h *Hub, dir string) {
	for _, f := range h.List() {
		full := filepath.Join(dir, filepath.FromSlash(f.Path))
		if _, err := os.Stat(full); err == nil {
			nv.base[f.Path] = f.Version
		}
	}
}
