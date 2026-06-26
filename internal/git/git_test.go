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
