package workspace

import (
	"bytes"
	"fmt"

	udiff "github.com/aymanbagabas/go-udiff"
)

// Git-style 3-way conflict markers (DESIGN.md §9): when a node's edit is rejected as divergent,
// the node writes both sides into the file so the agent resolves it in place — the same workflow
// agents already know from git. The local edit is the "ours" side, so nothing is lost; the file
// is held (not pushed, not overwritten) until the markers are gone.

const (
	markerStart = "<<<<<<< local (your edit)"
	markerMid   = "======="
	markerEnd   = ">>>>>>> hub"
)

// HasConflictMarkers reports whether content still contains unresolved conflict markers.
func HasConflictMarkers(b []byte) bool {
	return bytes.Contains(b, []byte(markerStart)) && bytes.Contains(b, []byte("\n"+markerMid+"\n"))
}

// MergeMarkers produces the local edit reconciled against the canonical hub content with git-style
// markers around ONLY the regions that actually differ — a deterministic line-level diff (Myers, via
// go-udiff), not a whole-file wrap. Lines both sides agree on stay clean; each changed span becomes
// one <<<<<<< local / ======= / >>>>>>> hub vN hunk, so the agent reconciles just the real changes.
func MergeMarkers(local, hub []byte, hubVersion uint64) []byte {
	edits := udiff.Lines(string(local), string(hub))
	if len(edits) == 0 {
		return local // identical (shouldn't happen on a real conflict) → nothing to mark
	}
	udiff.SortEdits(edits)
	var b bytes.Buffer
	pos := 0
	for _, e := range edits {
		b.Write(local[pos:e.Start]) // unchanged region preceding this hunk
		b.WriteString(markerStart + "\n")
		writeSide(&b, local[e.Start:e.End]) // "ours": the local lines this hunk replaces
		b.WriteString(markerMid + "\n")
		writeSide(&b, []byte(e.New)) // "theirs": the canonical hub lines
		fmt.Fprintf(&b, "%s v%d\n", markerEnd, hubVersion)
		pos = e.End
	}
	b.Write(local[pos:]) // trailing unchanged region
	return b.Bytes()
}

// writeSide writes one side of a hunk, terminated by a newline so the next marker sits on its own
// line. An empty side (a pure insertion/deletion) writes nothing — no spurious blank line.
func writeSide(b *bytes.Buffer, content []byte) {
	if len(content) == 0 {
		return
	}
	b.Write(content)
	if content[len(content)-1] != '\n' {
		b.WriteByte('\n')
	}
}
