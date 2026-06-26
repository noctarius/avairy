package git

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

// MirrorRefs returns the commit shas at the mirror's ref tips — what the node already has, sent
// to core so it can build an incremental bundle. A not-yet-created mirror returns nil (→ core
// sends a full bundle).
func MirrorRefs(ctx context.Context, mirrorDir string) ([]string, error) {
	if _, err := os.Stat(filepath.Join(mirrorDir, "HEAD")); err != nil {
		return nil, nil
	}
	out, err := runGit(ctx, mirrorDir, "show-ref", "--hash")
	if err != nil {
		return nil, err
	}
	var shas []string
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		if line != "" {
			shas = append(shas, line)
		}
	}
	return shas, nil
}

// UpdateMirror creates or refreshes a bare, read-only mirror of the canonical repo at mirrorDir
// from a bundle produced by Repo.Bundle (DESIGN.md §9). A node keeps such a mirror so its agent
// can check out and build/bisect past commits locally — on the node's own OS — without commit
// rights to core (history-writes still go through request_commit). First call clones the bundle
// as a mirror; later calls fetch the bundle's refs into it.
func UpdateMirror(ctx context.Context, mirrorDir string, bundle []byte) error {
	tmp, err := os.CreateTemp("", "avairy-bundle-*.bundle")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(bundle); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	if _, err := os.Stat(filepath.Join(mirrorDir, "HEAD")); err != nil {
		if err := os.MkdirAll(filepath.Dir(mirrorDir), 0o755); err != nil {
			return err
		}
		_, e := runGit(ctx, "", "clone", "--mirror", tmpPath, mirrorDir)
		return e
	}
	_, e := runGit(ctx, mirrorDir, "fetch", "--force", tmpPath, "+refs/*:refs/*")
	return e
}
