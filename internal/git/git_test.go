package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func initRepo(t *testing.T) (string, context.Context) {
	t.Helper()
	dir := t.TempDir()
	ctx := context.Background()
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "Test"},
		{"config", "commit.gpgsign", "false"},
	} {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir, ctx
}

func TestOpenRejectsNonRepo(t *testing.T) {
	if _, err := Open(context.Background(), t.TempDir(), false); err == nil {
		t.Fatal("expected non-repo dir to fail Open")
	}
}

func TestCommitAndHistory(t *testing.T) {
	dir, ctx := initRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := Open(ctx, dir, false) // no signing in test
	if err != nil {
		t.Fatal(err)
	}

	hash, err := r.Commit(ctx, nil, "first commit")
	if err != nil || hash == "" {
		t.Fatalf("commit: hash=%q err=%v", hash, err)
	}

	out, err := r.History(ctx, Log, "", "", 10)
	if err != nil || !strings.Contains(out, "first commit") {
		t.Fatalf("log missing commit: out=%q err=%v", out, err)
	}

	// Nothing staged → an error, not an empty commit.
	if _, err := r.Commit(ctx, nil, "noop"); err == nil {
		t.Fatal("expected 'nothing staged' error")
	}
	// Empty message → rejected before touching git.
	if _, err := r.Commit(ctx, nil, "   "); err == nil {
		t.Fatal("expected empty-message rejection")
	}
}

func TestScratchWorktree(t *testing.T) {
	dir, ctx := initRepo(t)
	write := func(s string) {
		if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r, _ := Open(ctx, dir, false)
	r.WorktreeBase = filepath.Join(t.TempDir(), "wt") // isolated, off the canonical tree

	write("v1\n")
	if _, err := r.Commit(ctx, nil, "v1"); err != nil {
		t.Fatal(err)
	}
	write("v2\n")
	if _, err := r.Commit(ctx, nil, "v2"); err != nil {
		t.Fatal(err)
	}

	// Check out the previous commit in a disposable worktree, isolated from the live tree.
	wt, err := r.AddWorktree(ctx, "HEAD~1")
	if err != nil {
		t.Fatalf("add worktree: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(wt.Path, "a.txt")); string(got) != "v1\n" {
		t.Fatalf("scratch checkout has %q, want old version v1", got)
	}
	// The canonical tree is untouched.
	if got, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(got) != "v2\n" {
		t.Fatalf("canonical tree was disturbed: %q", got)
	}
	if len(r.ListWorktrees()) != 1 {
		t.Fatalf("expected 1 live worktree, got %d", len(r.ListWorktrees()))
	}

	// Disposable: prune removes it from disk and tracking.
	r.PruneWorktrees(ctx)
	if _, err := os.Stat(wt.Path); !os.IsNotExist(err) {
		t.Fatal("pruned worktree dir should be gone")
	}
	if len(r.ListWorktrees()) != 0 {
		t.Fatal("prune should clear tracking")
	}
}

// The cross-OS RCA path: core bundles its repo, a node builds a read-only mirror from the
// bundle, and an agent checks out a past commit locally from that mirror.
func TestBundleMirrorWorktree(t *testing.T) {
	dir, ctx := initRepo(t)
	write := func(s string) {
		if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r, _ := Open(ctx, dir, false)
	write("v1\n")
	if _, err := r.Commit(ctx, nil, "v1"); err != nil {
		t.Fatal(err)
	}
	write("v2\n")
	if _, err := r.Commit(ctx, nil, "v2"); err != nil {
		t.Fatal(err)
	}

	// Core bundles the repo; node builds a mirror from the bundle bytes.
	bundle, err := r.Bundle(ctx)
	if err != nil || len(bundle) == 0 {
		t.Fatalf("bundle: len=%d err=%v", len(bundle), err)
	}
	mirror := filepath.Join(t.TempDir(), "mirror.git")
	if err := UpdateMirror(ctx, mirror, bundle); err != nil {
		t.Fatalf("build mirror: %v", err)
	}

	// From the mirror, an agent checks out a PAST commit locally (what it'd build/bisect).
	scratch := filepath.Join(t.TempDir(), "scratch")
	if _, err := runGit(ctx, "", "--git-dir="+mirror, "worktree", "add", "--detach", scratch, "HEAD~1"); err != nil {
		t.Fatalf("worktree from mirror: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(scratch, "a.txt")); string(got) != "v1\n" {
		t.Fatalf("mirror worktree has %q, want old version v1", got)
	}

	// Refresh is idempotent (re-applying the same bundle succeeds).
	if err := UpdateMirror(ctx, mirror, bundle); err != nil {
		t.Fatalf("refresh mirror: %v", err)
	}
}

func TestHistoryRejectsFlagInjection(t *testing.T) {
	dir, ctx := initRepo(t)
	r, _ := Open(ctx, dir, false)
	if _, err := r.History(ctx, Log, "--output=/tmp/x", "", 10); err == nil {
		t.Fatal("ref starting with '-' must be rejected")
	}
	if _, err := r.History(ctx, Blame, "", "", 10); err == nil {
		t.Fatal("blame without a path must be rejected")
	}
	if _, err := r.History(ctx, "bogus", "", "", 10); err == nil {
		t.Fatal("unknown mode must be rejected")
	}
}
