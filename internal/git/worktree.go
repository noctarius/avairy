package git

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// Disposable scratch worktrees (DESIGN.md §9): a node agent doing root-cause analysis can pin
// an old commit and build/reproduce against it without disturbing the live-synced canonical
// tree. Checkouts are detached (no branch), rooted OUTSIDE the repo (so the sync hub never sees
// them), tracked for cleanup, and pruned on shutdown — "disposable".

// Worktree is one scratch checkout.
type Worktree struct {
	ID   string `json:"id"`
	Ref  string `json:"ref"`
	Path string `json:"path"`
}

func (r *Repo) worktreeBase() string {
	if r.WorktreeBase != "" {
		return r.WorktreeBase
	}
	return filepath.Join(os.TempDir(), "avairy-worktrees")
}

// AddWorktree checks ref out (detached) into a fresh disposable worktree and returns it. An
// empty ref means HEAD. The checkout lives under WorktreeBase, isolated from the synced tree.
func (r *Repo) AddWorktree(ctx context.Context, ref string) (Worktree, error) {
	if ref == "" {
		ref = "HEAD"
	}
	if err := safeArg("ref", ref); err != nil {
		return Worktree{}, err
	}
	r.wmu.Lock()
	defer r.wmu.Unlock()
	if r.worktrees == nil {
		r.worktrees = make(map[string]Worktree)
	}
	r.wseq++
	id := fmt.Sprintf("wt%d", r.wseq)
	path := filepath.Join(r.worktreeBase(), id)
	if _, err := r.run(ctx, "worktree", "add", "--detach", path, ref); err != nil {
		return Worktree{}, err
	}
	wt := Worktree{ID: id, Ref: ref, Path: path}
	r.worktrees[id] = wt
	return wt, nil
}

// RemoveWorktree tears down a scratch worktree (force: discards any uncommitted scratch edits).
func (r *Repo) RemoveWorktree(ctx context.Context, id string) error {
	r.wmu.Lock()
	defer r.wmu.Unlock()
	wt, ok := r.worktrees[id]
	if !ok {
		return fmt.Errorf("git: unknown worktree %q", id)
	}
	if _, err := r.run(ctx, "worktree", "remove", "--force", wt.Path); err != nil {
		return err
	}
	delete(r.worktrees, id)
	return nil
}

// ListWorktrees returns the live scratch worktrees.
func (r *Repo) ListWorktrees() []Worktree {
	r.wmu.Lock()
	defer r.wmu.Unlock()
	out := make([]Worktree, 0, len(r.worktrees))
	for _, wt := range r.worktrees {
		out = append(out, wt)
	}
	return out
}

// PruneWorktrees removes every scratch worktree and clears stale metadata. Call on shutdown so
// disposable checkouts don't leak (they're throwaway by design).
func (r *Repo) PruneWorktrees(ctx context.Context) {
	r.wmu.Lock()
	defer r.wmu.Unlock()
	for id, wt := range r.worktrees {
		_, _ = r.run(ctx, "worktree", "remove", "--force", wt.Path)
		delete(r.worktrees, id)
	}
	_, _ = r.run(ctx, "worktree", "prune")
}
