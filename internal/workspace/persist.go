package workspace

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Hub persistence (DESIGN.md §9): the canonical workspace is held in memory, but a core
// restart must not lose it (and leave every node re-syncing against an empty hub). We
// snapshot the full file set — content, mode, version, writer, deletion tombstones — to a
// single JSON file, written atomically and reloaded on startup. (A future git-backed store
// would replace this; the snapshot is the simple, correct MVP.)

type hubSnapshot struct {
	Files []FileState `json:"files"`
}

// LoadHub restores a hub from a snapshot at path. A missing file yields an empty hub (first
// run); a corrupt one is an error so the operator notices rather than silently losing state.
func LoadHub(path string) (*Hub, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return NewHub(), nil
	}
	if err != nil {
		return nil, err
	}
	var snap hubSnapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		return nil, err
	}
	h := NewHub()
	for i := range snap.Files {
		f := snap.Files[i]
		h.files[f.Path] = &f
	}
	return h, nil // loaded straight into the map → not marked dirty (already on disk)
}

// SaveIfDirty writes the hub to path only if it changed since the last save, reporting whether
// it wrote. Use it on a timer so an idle hub costs nothing.
func (h *Hub) SaveIfDirty(path string) (bool, error) {
	h.mu.Lock()
	dirty := h.dirty
	h.mu.Unlock()
	if !dirty {
		return false, nil
	}
	return true, h.Save(path)
}

// Save atomically writes the hub's current state to path (temp + rename), creating the parent
// dir. The dirty flag is cleared atomically with the snapshot (so a write landing mid-Save
// re-marks dirty for the next tick rather than being lost); on failure it's forced back on.
func (h *Hub) Save(path string) error {
	h.mu.Lock()
	snap := hubSnapshot{Files: make([]FileState, 0, len(h.files))}
	for _, f := range h.files {
		snap.Files = append(snap.Files, f.clone())
	}
	h.dirty = false
	h.mu.Unlock()

	if err := h.writeSnapshot(path, snap); err != nil {
		h.mu.Lock()
		h.dirty = true // retry next tick
		h.mu.Unlock()
		return err
	}
	return nil
}

func (h *Hub) writeSnapshot(path string, snap hubSnapshot) error {
	b, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".avairy-hub-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}
