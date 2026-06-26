package mcp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"avairy/internal/gating"
	"avairy/internal/git"
)

func gitRepoForTest(t *testing.T) *git.Repo {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{{"init"}, {"config", "user.email", "t@e.com"}, {"config", "user.name", "T"}, {"config", "commit.gpgsign", "false"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := git.Open(context.Background(), dir, false)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// request_commit is gated: an allowing operator commits and history shows it; a denying one
// blocks it. git_history reads without gating.
func TestGitToolsGatedCommit(t *testing.T) {
	s, _ := newTestServer(t)
	s.RegisterAgent("alice", nil, nil)
	allow := true
	s.EnableGit(gitRepoForTest(t), func(_ context.Context, _ gating.Request) (gating.Decision, error) {
		if allow {
			return gating.Allow, nil
		}
		return gating.Deny, nil
	})

	// Approved commit.
	commit, err := s.handleRequestCommit(asAgent("alice"), call(map[string]any{"message": "init"}))
	if err != nil {
		t.Fatal(err)
	}
	if got := mustText(t, commit); !strings.Contains(got, "committed") {
		t.Fatalf("commit result = %q", got)
	}
	// History (no gating) shows it.
	hist, err := s.handleGitHistory(asAgent("alice"), call(map[string]any{"mode": "log"}))
	if err != nil {
		t.Fatal(err)
	}
	if log := mustText(t, hist); !strings.Contains(log, "init") {
		t.Fatalf("log missing commit: %q", log)
	}

	// Denied commit returns an error and doesn't commit.
	allow = false
	if err := os.WriteFile(filepath.Join(s.gitRepo.Dir, "b.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res2, err := s.handleRequestCommit(asAgent("alice"), call(map[string]any{"message": "second"}))
	if err != nil {
		t.Fatal(err)
	}
	if !res2.IsError || !strings.Contains(resultText(res2), "denied") {
		t.Fatalf("denied commit should error: %+v", res2)
	}
}
