// Package git wraps the git CLI over avairy's single canonical repository, which lives only on
// core (DESIGN.md §9). Remote nodes hold synced working trees with no .git, so all history
// reads are proxied here via MCP and all history writes (commits) run here, signed with the
// operator's key — which never leaves this machine. There is no go-git dependency; we shell
// out to the user's git so its config, signing, and hooks apply exactly as on the CLI.
package git

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// Repo is the canonical repository at Dir. Sign requires commits to be cryptographically
// signed (git -S); it is true in production (the design mandates signed history) and may be
// disabled in tests where no signing key is configured.
type Repo struct {
	Dir  string
	Sign bool
}

// Open returns a Repo for dir, verifying dir is inside a git work tree.
func Open(ctx context.Context, dir string, sign bool) (*Repo, error) {
	r := &Repo{Dir: dir, Sign: sign}
	if out, err := r.run(ctx, "rev-parse", "--is-inside-work-tree"); err != nil || strings.TrimSpace(out) != "true" {
		return nil, fmt.Errorf("git: %q is not a git work tree: %v", dir, err)
	}
	return r, nil
}

// HistoryMode selects a read-only history query.
type HistoryMode string

const (
	Log   HistoryMode = "log"
	Show  HistoryMode = "show"
	Diff  HistoryMode = "diff"
	Blame HistoryMode = "blame"
)

// History runs a read-only query against the repo (DESIGN.md §9: available to any node agent
// for root-cause analysis, independent of commit rights). ref/path are optional except where
// noted; both are validated so an agent can't smuggle in flags or escape the repo.
func (r *Repo) History(ctx context.Context, mode HistoryMode, ref, path string, limit int) (string, error) {
	if err := safeArg("ref", ref); err != nil {
		return "", err
	}
	if err := safeArg("path", path); err != nil {
		return "", err
	}
	if limit <= 0 || limit > 500 {
		limit = 50
	}

	var args []string
	switch mode {
	case Log:
		args = []string{"log", "--oneline", "--decorate", "-n", strconv.Itoa(limit)}
		if ref != "" {
			args = append(args, ref)
		}
	case Show:
		if ref == "" {
			ref = "HEAD"
		}
		args = []string{"show", "--stat", ref}
	case Diff:
		args = []string{"diff"}
		if ref != "" {
			args = append(args, ref)
		}
	case Blame:
		if path == "" {
			return "", fmt.Errorf("git: blame requires a path")
		}
		args = []string{"blame", "--date=short"}
		if ref != "" {
			args = append(args, ref)
		}
	default:
		return "", fmt.Errorf("git: unknown history mode %q (want log|show|diff|blame)", mode)
	}
	if path != "" {
		args = append(args, "--", path) // "--" stops path being read as a revision/flag
	}
	return r.run(ctx, args...)
}

// Commit stages paths (or everything, when paths is empty) and creates one commit, signed when
// Sign is set (DESIGN.md §9: core-only & signed). It returns the new short hash. A no-op commit
// (nothing staged) is reported as an error rather than creating an empty commit.
func (r *Repo) Commit(ctx context.Context, paths []string, message string) (string, error) {
	if strings.TrimSpace(message) == "" {
		return "", fmt.Errorf("git: commit message is required")
	}
	add := []string{"add", "--"}
	if len(paths) == 0 {
		add = []string{"add", "-A"}
	} else {
		for _, p := range paths {
			if err := safeArg("path", p); err != nil {
				return "", err
			}
		}
		add = append(add, paths...)
	}
	if _, err := r.run(ctx, add...); err != nil {
		return "", err
	}
	if out, err := r.run(ctx, "diff", "--cached", "--quiet"); err == nil && out == "" {
		return "", fmt.Errorf("git: nothing staged to commit")
	}
	commit := []string{"commit", "-m", message}
	if r.Sign {
		commit = append(commit, "-S")
	}
	if _, err := r.run(ctx, commit...); err != nil {
		return "", err
	}
	hash, err := r.run(ctx, "rev-parse", "--short", "HEAD")
	return strings.TrimSpace(hash), err
}

// run executes git in the repo dir and returns stdout (stderr is folded into the error).
func (r *Repo) run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = r.Dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", args[0], msg)
	}
	return stdout.String(), nil
}

// safeArg rejects empty-allowed values that would be read as flags (a leading '-'), so an agent
// can't pass e.g. ref="--upload-pack=...". Empty is allowed (caller decides if required).
func safeArg(name, v string) error {
	if strings.HasPrefix(v, "-") {
		return fmt.Errorf("git: %s %q must not start with '-'", name, v)
	}
	return nil
}
