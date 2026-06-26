package workspace

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Watch returns a channel that emits a (coalesced) signal whenever a non-ignored file under
// dir changes, so syncs can fire on edits instead of only on a timer (DESIGN.md §9). It's the
// trigger, not the truth: callers still scan on the signal and should keep a coarse fallback
// poll, because fsnotify can't watch subtrees natively (we add dirs as they appear, with an
// inherent create race), drops events under load, and is unreliable on network filesystems.
//
// fsnotify is single-directory per watch, so we walk dir and add every directory, then add new
// directories as they're created. Bursts (an editor's write+rename+chmod, a multi-file save)
// are debounced into one signal. The channel closes when ctx is cancelled or setup fails; a
// nil/closed channel simply means "no events" and the fallback poll carries the load.
func Watch(ctx context.Context, dir string, ig Ignore) (<-chan struct{}, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	addTree(w, dir, ig) // best-effort: a dir that can't be watched still gets picked up by the poll

	out := make(chan struct{}, 1)
	go func() {
		defer w.Close()
		defer close(out)
		var debounce <-chan time.Time
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-w.Events:
				if !ok {
					return
				}
				rel, rerr := filepath.Rel(dir, ev.Name)
				if rerr != nil {
					continue
				}
				if rel != "." && ig.Match(filepath.ToSlash(rel)) {
					continue // edits under ignored paths (.git, build/, …) don't trigger syncs
				}
				// A newly-created directory needs its own watch (and may already contain files).
				if ev.Op&fsnotify.Create != 0 {
					if info, e := os.Stat(ev.Name); e == nil && info.IsDir() {
						addTree(w, ev.Name, ig)
					}
				}
				if debounce == nil {
					debounce = time.After(200 * time.Millisecond)
				}
			case <-debounce:
				debounce = nil
				select {
				case out <- struct{}{}: // signal; a pending one already covers this round
				default:
				}
			case <-w.Errors:
				// Watcher errors (e.g. fd exhaustion) are non-fatal: the fallback poll still syncs.
			}
		}
	}()
	return out, nil
}

// addTree adds dir and all its non-ignored subdirectories to the watcher (best effort).
func addTree(w *fsnotify.Watcher, dir string, ig Ignore) {
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil //nolint:nilerr // skip unreadable entries; the poll backs us up
		}
		rel, rerr := filepath.Rel(dir, p)
		if rerr != nil {
			return nil
		}
		if rel != "." && ig.Match(filepath.ToSlash(rel)) {
			return filepath.SkipDir
		}
		_ = w.Add(p)
		return nil
	})
}
